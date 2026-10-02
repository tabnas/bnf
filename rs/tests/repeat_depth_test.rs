// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

//! Every repetition compiles to a same-depth replace loop (AGENTS.md,
//! "Repetition is replacement, never a push chain"). Twin of
//! `ts/test/repeat-depth.test.js`.
//!
//! A star, a plus and an unbounded rep are SEQUENCE, so the loop they
//! desugar to runs every iteration in one frame: the item may be pushed,
//! but the loop is re-entered by replacement (`r:`) rather than by
//! pushing a fresh copy. Rule depth is then bounded by the grammar's
//! nesting, never by the input's length. The helper used to be emitted as
//! the right recursion it is written as in the IR (`H = item H / ε`, each
//! item pushing a new `H`), so a flat file of a few thousand records cost
//! a frame per record and tripped the hosts' depth guards.
//!
//! The proof is observed, not inferred from the spec: the engine reports
//! the depth `d` of every rule it runs (`subscribe_rule_done`), and the
//! deepest one over ten thousand items must be exactly the deepest one
//! over a single iteration. Time is the second observable: ten times the
//! items must cost about ten times as long.

mod common;

use std::collections::{BTreeMap, BTreeSet};
use std::rc::Rc;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::{Duration, Instant};

use common::{group, opt, plus, prod, reference, rx, sens_term, star};
use serde_json::{json, Value};
use tabnas_bnf::{
    emit_grammar_spec, to_recognition_spec, ConvertOptions, Element, Grammar, GrammarSpec,
    Production, ValueAnnotation,
};

const N: usize = 10_000;

/// The tests of one binary run in parallel threads, and a timing taken
/// while the others parse ten thousand items measures them as much as the
/// parse it times. Every test here that parses a long input or reads the
/// clock holds this for its whole run, so each timing has the process to
/// itself. A test that fails while holding it poisons it; the others
/// still run.
fn exclusive() -> MutexGuard<'static, ()> {
    static ONE_AT_A_TIME: Mutex<()> = Mutex::new(());
    ONE_AT_A_TIME
        .lock()
        .unwrap_or_else(|poisoned| poisoned.into_inner())
}

/// A parser for the grammar, and a way to read the deepest rule it ran.
struct Probe {
    spec: GrammarSpec,
    parser: tabnas::Tabnas,
    deepest: Arc<AtomicUsize>,
}

struct Parsed {
    out: Value,
    deepest: usize,
}

impl Probe {
    fn new(productions: Vec<Production>, builtins: bool, recognition: bool) -> Self {
        let start = productions[0].name.clone();
        let spec = match emit_grammar_spec(
            &Grammar::new(productions),
            &ConvertOptions::tag("depth").start(start).builtins(builtins),
        ) {
            Ok(spec) => spec,
            Err(e) => panic!("emit: {e}"),
        };
        let mut parser = tabnas::Tabnas::new();
        if recognition {
            let rec = to_recognition_spec(&spec).expect("recognition spec");
            let engine = tabnas::GrammarSpec::from_value(rec).expect("recognition grammar");
            parser.grammar(&engine).expect("install recognition");
        } else {
            spec.install(&mut parser).expect("install");
        }
        let deepest = Arc::new(AtomicUsize::new(0));
        let seen = deepest.clone();
        parser.subscribe_rule_done(move |rule, _context, _done| {
            seen.fetch_max(rule.d, Ordering::Relaxed);
        });
        Self {
            spec,
            parser,
            deepest,
        }
    }

    fn parse(&self, src: &str) -> Parsed {
        self.deepest.store(0, Ordering::Relaxed);
        let out = match self.parser.parse(src) {
            Ok(v) => v.to_json(),
            Err(e) => panic!("parse of {} bytes: {e}", src.len()),
        };
        Parsed {
            out,
            deepest: self.deepest.load(Ordering::Relaxed),
        }
    }
}

/// Each timing sample repeats the parse until it has run for at least
/// this long, and reports the cost per parse, so a clock that ticks
/// coarsely cannot read a short parse as nothing, or as one whole tick.
/// (TypeScript's twin met exactly that on Windows, where process CPU time
/// moves in 15.6 ms steps.)
const SAMPLE: Duration = Duration::from_millis(150);

/// The cheapest of a few samples, in milliseconds per parse, so one slow
/// sample (a page fault, a neighbouring process) does not decide the
/// verdict. The clock is the monotonic one: the standard library has no
/// process CPU clock, and `exclusive` keeps this binary's other tests off
/// the processor while it runs.
fn fastest(mut f: impl FnMut(), runs: usize) -> f64 {
    let mut best = f64::INFINITY;
    for _ in 0..runs {
        let t0 = Instant::now();
        let mut n = 0u32;
        let mut spent;
        loop {
            f();
            n += 1;
            spent = t0.elapsed();
            if spent >= SAMPLE || n >= 10_000 {
                break;
            }
        }
        best = best.min(spent.as_secs_f64() * 1e3 / f64::from(n));
    }
    best
}

/// Linear time: ten times the items cost ten times as long, within a
/// small factor. The factor is not 1, and not stable enough to pin near
/// 1: the larger input pays more per item for memory while the work per
/// item is the same. A parse that is quadratic in the item count costs a
/// hundred times as long; the bound is TypeScript's, 50 times. Measured on
/// one machine, the loop costs 10 to 11 times in a debug build, in every
/// case below; the push chain it replaced cost 90 to 95 times for `*item`
/// with builtins and 154 to 158 times with closures in a release build,
/// and in a debug build its first case had not finished timing after 25
/// minutes. A measurement that lands above the bound is taken again, up
/// to three times, before it counts: a quadratic parse is over it every
/// time.
fn assert_linear(p: &Probe, make: fn(usize) -> String, label: &str) {
    let small = make(N / 10);
    let large = make(N);
    // Nothing here compiles at run time, so one parse warms what there is
    // to warm. The first large sample pays for growing the process to the
    // large input's size; the fastest sample is the one that counts.
    p.parse(&small);
    let mut seen = String::new();
    for _ in 0..3 {
        let t_small = fastest(|| drop(p.parse(&small)), 3);
        let t_large = fastest(|| drop(p.parse(&large)), 2);
        if t_large < 50.0 * t_small {
            return;
        }
        seen.push_str(&format!(
            " {:.1}x ({t_large:.2} ms against {t_small:.2} ms per parse);",
            t_large / t_small
        ));
    }
    panic!(
        "{label}: {N} items against {} took{seen} not linear",
        N / 10
    );
}

fn times(n: usize, s: &str, sep: &str) -> String {
    vec![s; n].join(sep)
}

fn lit(s: &str) -> Element {
    sens_term(s)
}

fn item() -> Production {
    prod("item", vec![vec![rx("[a-z]+", "")]])
}

fn kids(v: &Value) -> &Vec<Value> {
    v["kids"].as_array().expect("a node with kids")
}

/// `doc = *pair`, `pair = key "=" val ";" ; @object`, `key = *[a-z]`,
/// `val = *[0-9]`: stars inside an item that builds a value.
fn pairs() -> Vec<Production> {
    vec![
        prod("doc", vec![vec![star(reference("pair"))]]),
        Production {
            value: Some(ValueAnnotation::object(["key", "val"])),
            ..prod(
                "pair",
                vec![vec![reference("key"), lit("="), reference("val"), lit(";")]],
            )
        },
        prod("key", vec![vec![star(rx("[a-z]", ""))]]),
        prod("val", vec![vec![star(rx("[0-9]", ""))]]),
    ]
}

/// One case: the grammar, the input for n items, the smallest n that runs
/// one full iteration of every repetition in it, and what the value must
/// hold for n items.
struct Case {
    name: &'static str,
    grammar: fn() -> Vec<Production>,
    make: fn(usize) -> String,
    one: usize,
    check: fn(&Value, usize),
}

fn cases() -> Vec<Case> {
    vec![
        Case {
            name: "*item",
            grammar: || vec![prod("doc", vec![vec![star(reference("item"))]]), item()],
            make: |n| times(n, "ab", " "),
            one: 1,
            check: |out, n| assert_eq!(kids(out).len(), n),
        },
        Case {
            name: "1*item",
            grammar: || vec![prod("doc", vec![vec![plus(reference("item"))]]), item()],
            make: |n| times(n, "ab", " "),
            one: 2,
            check: |out, n| assert_eq!(kids(out).len(), n),
        },
        Case {
            name: "3*item",
            grammar: || {
                vec![
                    prod("doc", vec![vec![Element::rep(3, None, reference("item"))]]),
                    item(),
                ]
            },
            make: |n| times(n, "ab", " "),
            one: 4,
            check: |out, n| assert_eq!(kids(out).len(), n),
        },
        Case {
            name: "*\"x\" (a terminal item)",
            grammar: || vec![prod("doc", vec![vec![star(lit("x")), lit(";")]])],
            make: |n| times(n, "x", "") + ";",
            one: 1,
            check: |out, n| assert_eq!(out["src"].as_str().unwrap().len(), n + 1),
        },
        Case {
            name: "item *(\",\" item)",
            grammar: || {
                vec![
                    prod(
                        "doc",
                        vec![vec![
                            reference("item"),
                            star(group(vec![vec![lit(","), reference("item")]])),
                        ]],
                    ),
                    item(),
                ]
            },
            make: |n| times(n, "ab", ","),
            one: 2,
            check: |out, n| assert_eq!(kids(out).len(), n - 1),
        },
        Case {
            name: "*( item \";\" / \"!\" ) (a star of a group)",
            grammar: || {
                vec![
                    prod(
                        "doc",
                        vec![vec![star(group(vec![
                            vec![reference("item"), lit(";")],
                            vec![lit("!")],
                        ]))]],
                    ),
                    item(),
                ]
            },
            make: |n| times(n, "ab;", ""),
            one: 1,
            check: |out, n| assert_eq!(kids(out).len(), n),
        },
        Case {
            name: "*row, row = \"[\" *cell \"]\" (a star inside the item)",
            grammar: || {
                vec![
                    prod("doc", vec![vec![star(reference("row"))]]),
                    prod(
                        "row",
                        vec![vec![lit("["), star(reference("cell")), lit("]")]],
                    ),
                    prod("cell", vec![vec![rx("[a-z]", "")]]),
                ]
            },
            make: |n| times(n, "[abc]", ""),
            one: 1,
            check: |out, n| {
                assert_eq!(kids(out).len(), n);
                assert_eq!(kids(&kids(out)[n - 1]).len(), 3);
            },
        },
        Case {
            name: "row = \"[\" *cell \"]\" (ten thousand cells in one row)",
            grammar: || {
                vec![
                    prod(
                        "row",
                        vec![vec![lit("["), star(reference("cell")), lit("]")]],
                    ),
                    prod("cell", vec![vec![rx("[a-z]", "")]]),
                ]
            },
            make: |n| format!("[{}]", times(n, "a", "")),
            one: 1,
            check: |out, n| assert_eq!(kids(out).len(), n),
        },
        Case {
            name: "doc = *a *b (two sibling stars)",
            grammar: || {
                vec![
                    prod(
                        "doc",
                        vec![vec![star(reference("a")), star(reference("b"))]],
                    ),
                    prod("a", vec![vec![lit("a")], vec![lit("A")]]),
                    prod("b", vec![vec![lit("b")], vec![lit("B")]]),
                ]
            },
            make: |n| times(n, "a", "") + &times(n, "b", ""),
            one: 1,
            check: |out, n| {
                assert_eq!(kids(out).len(), 2 * n);
                assert_eq!(kids(out)[n]["src"], json!("b"));
            },
        },
        Case {
            name: "*( \"(\" *item \")\" ) (a star directly inside a star's group)",
            grammar: || {
                vec![
                    prod(
                        "doc",
                        vec![vec![star(group(vec![vec![
                            lit("("),
                            star(reference("item")),
                            lit(")"),
                        ]]))]],
                    ),
                    item(),
                ]
            },
            make: |n| times(n, "(ab cd)", ""),
            one: 1,
            check: |out, n| assert_eq!(kids(out).len(), 2 * n),
        },
        Case {
            // The item builds a value, and each of its members is a star:
            // the loops over `key`'s letters and `val`'s digits run inside
            // an item of the loop over `pair`, side by side. Each
            // allocates the node it accumulates into on its own way in,
            // which is what the member reads back; a loop that took its
            // pusher's instead would write its letters into the object
            // (see `the_loop_counter` below).
            name:
                "*pair, pair = key \"=\" val \";\" ; @object (stars in an item that builds a value)",
            grammar: pairs,
            make: |n| times(n, "ab=12;", ""),
            one: 1,
            check: |out, n| {
                assert_eq!(kids(out).len(), n);
                assert_eq!(kids(out)[n - 1], json!({"key": "ab", "val": "12"}));
            },
        },
        Case {
            // Left recursion is rewritten to `seed tail*`: the star it
            // synthesises is a loop like any other, and the tree stays
            // flat.
            name: "sum = sum \"+\" item / item (left recursion)",
            grammar: || {
                vec![
                    prod(
                        "sum",
                        vec![
                            vec![reference("sum"), lit("+"), reference("item")],
                            vec![reference("item")],
                        ],
                    ),
                    item(),
                ]
            },
            make: |n| times(n, "ab", "+"),
            one: 2,
            check: |out, n| {
                assert_eq!(out["rule"], json!("sum"));
                assert_eq!(kids(out).len(), n - 1);
            },
        },
    ]
}

fn depth_is_one_items(builtins: bool) {
    let _one = exclusive();
    let mode = if builtins { "builtins" } else { "closures" };
    for c in cases() {
        let p = Probe::new((c.grammar)(), builtins, false);
        let one = p.parse(&(c.make)(c.one));
        let many = p.parse(&(c.make)(N));
        (c.check)(&many.out, N);
        assert_eq!(
            many.deepest, one.deepest,
            "{} ({mode}): {N} items reached depth {}; one iteration needs {}",
            c.name, many.deepest, one.deepest
        );
    }
}

#[test]
fn depth_over_ten_thousand_items_is_one_items_closures() {
    depth_is_one_items(false);
}

#[test]
fn depth_over_ten_thousand_items_is_one_items_builtins() {
    depth_is_one_items(true);
}

fn time_is_linear(builtins: bool) {
    let _one = exclusive();
    let mode = if builtins { "builtins" } else { "closures" };
    for c in cases() {
        assert_linear(
            &Probe::new((c.grammar)(), builtins, false),
            c.make,
            &format!("{} ({mode})", c.name),
        );
    }
}

#[test]
fn time_is_linear_in_the_item_count_closures() {
    time_is_linear(false);
}

#[test]
fn time_is_linear_in_the_item_count_builtins() {
    time_is_linear(true);
}

#[test]
fn holds_in_a_recognition_only_grammar() {
    let _one = exclusive();
    // The loop's back-edge and its `rep` counter are structural, so a
    // spec stripped of every tree builder still loops in one frame.
    let g = vec![
        prod(
            "doc",
            vec![vec![
                reference("item"),
                star(group(vec![vec![lit(","), reference("item")]])),
            ]],
        ),
        item(),
    ];
    let p = Probe::new(g, true, true);
    let one = p.parse(&times(2, "ab", ","));
    let many = p.parse(&times(N, "ab", ","));
    assert_eq!(many.deepest, one.deepest);
}

#[test]
fn collects_one_element_per_item_into_an_array_at_one_depth() {
    let _one = exclusive();
    // `list = "(" *( item ";" ) ")"  ; @array`: the array-collection
    // planner hands the loop the array it inherits, and each iteration
    // pushes one element into it.
    let list = || Production {
        value: Some(ValueAnnotation::array()),
        ..prod(
            "list",
            vec![vec![
                lit("("),
                star(group(vec![vec![reference("item"), lit(";")]])),
                lit(")"),
            ]],
        )
    };
    for builtins in [false, true] {
        let p = Probe::new(vec![list(), item()], builtins, false);
        let one = p.parse("(ab;)");
        assert_eq!(one.out, json!(["ab"]));
        let many = p.parse(&format!("({})", times(N, "ab;", "")));
        let out = many.out.as_array().expect("an array");
        assert_eq!(out.len(), N);
        assert!(out.iter().all(|e| e == "ab"));
        assert_eq!(many.deepest, one.deepest);
    }
}

#[test]
fn takes_a_repetition_member_as_its_text_at_one_depth() {
    let _one = exclusive();
    // An object member that is a star is the run's source text: the loop
    // accumulates into the node its entry allocated, which is what the
    // member reads back, not the object it sits in.
    let g = || {
        vec![
            Production {
                value: Some(ValueAnnotation::object(["ds", "e"])),
                ..prod(
                    "obj",
                    vec![vec![
                        lit("<"),
                        star(reference("d")),
                        lit(">"),
                        reference("e"),
                    ]],
                )
            },
            prod("d", vec![vec![rx("[0-9]", "")]]),
            prod("e", vec![vec![lit("!"), lit("?")]]),
        ]
    };
    for builtins in [false, true] {
        let p = Probe::new(g(), builtins, false);
        assert_eq!(p.parse("<>!?").out, json!({"ds": "", "e": "!?"}));
        let one = p.parse("<7>!?");
        assert_eq!(one.out, json!({"ds": "7", "e": "!?"}));
        let many = p.parse(&format!("<{}>!?", times(N, "7", "")));
        assert_eq!(many.out["ds"].as_str().unwrap().len(), N);
        assert_eq!(many.deepest, one.deepest);
    }
}

#[test]
fn refuses_a_star_of_something_that_can_match_nothing() {
    // `*[x]` has an iteration that can match without progress. Refuse it
    // before the engine has to detect the non-advancing loop.
    let grammar = Grammar::new(vec![prod("doc", vec![vec![star(opt(lit("x"))), lit(";")]])]);
    let err = emit_grammar_spec(&grammar, &ConvertOptions::tag("depth"))
        .expect_err("nullable star compiled");
    assert!(
        err.message.contains("unbounded repetition") && err.message.contains("empty string"),
        "{err}"
    );
}

/// The generated loop for a star over `hint`: `_gen<digits>_star_<hint>`.
fn is_star_of(name: &str, hint: &str) -> bool {
    name.strip_prefix("_gen")
        .and_then(|rest| rest.strip_suffix(&format!("_star_{hint}")))
        .is_some_and(|digits| !digits.is_empty() && digits.bytes().all(|b| b.is_ascii_digit()))
}

fn rule<'a>(spec: &'a GrammarSpec, name: &str) -> &'a tabnas_bnf::RuleSpec {
    spec.rule
        .get(name)
        .and_then(Option::as_ref)
        .unwrap_or_else(|| panic!("no rule {name}"))
}

#[test]
fn emits_the_loop_as_a_replace_never_a_push_of_itself() {
    // The shape, for `doc = *item`. The loop's first alternative is its
    // entry: on the way in (counter `rep` still 0) it allocates the node
    // and re-enters the loop, counted. Every other alternative decides
    // continue or exit as the right-recursive helper always did, but
    // continuing hands over to the iteration by replacement: `$alt0`
    // pushes the item (clearing the counter for whatever the item holds)
    // and replaces itself, counted again, with `$step1`, which replaces
    // itself with the loop. The rule names are the ones the helper always
    // compiled to.
    let p = Probe::new(
        vec![prod("doc", vec![vec![star(reference("item"))]]), item()],
        true,
        false,
    );
    let spec = &p.spec;
    let lp = spec
        .rule
        .keys()
        .find(|n| is_star_of(n, "item"))
        .unwrap_or_else(|| panic!("no loop among {:?}", spec.rule.keys().collect::<Vec<_>>()))
        .clone();
    let iter = format!("{lp}$alt0");
    let step = format!("{iter}$step1");
    let mut names: Vec<&str> = spec.rule.keys().map(String::as_str).collect();
    names.sort_unstable();
    let mut want = vec![
        "__start__",
        lp.as_str(),
        iter.as_str(),
        step.as_str(),
        "doc",
        "item",
    ];
    want.sort_unstable();
    assert_eq!(names, want);

    let pushes: Vec<Option<&str>> = rule(spec, "doc").open.iter().map(|a| a.p()).collect();
    assert_eq!(pushes, vec![Some(lp.as_str())]);

    let open = &rule(spec, &lp).open;
    let (entry, open) = open.split_first().expect("the loop has alternatives");
    assert_eq!(entry.get("c"), Some(&json!({"n.rep": 0})));
    assert_eq!(entry.get("n"), Some(&json!({"rep": 1})));
    assert_eq!(entry.r(), Some(lp.as_str()));
    assert_eq!(entry.get("a"), Some(&json!("@node$")));
    assert_eq!(entry.k().expect("k")["node$"]["init"], json!(true));
    let takes: Vec<_> = open.iter().filter(|a| a.r().is_some()).collect();
    assert!(!takes.is_empty());
    for a in takes {
        assert_eq!(
            a.r(),
            Some(iter.as_str()),
            "continuing hands over to the iteration"
        );
        assert_eq!(a.p(), None);
        assert_eq!(a.get("c"), None);
        assert_eq!(a.get("a"), None, "an iteration allocates nothing");
    }
    for a in open.iter().filter(|a| a.r().is_none()) {
        assert_eq!(a.p(), None, "an exit takes no item");
        assert_eq!(a.get("a"), None, "an exit allocates nothing");
    }
    assert_eq!(rule(spec, &lp).close, None);

    let takes: Vec<_> = rule(spec, &iter)
        .open
        .iter()
        .map(|a| (a.p(), a.get("n").cloned()))
        .collect();
    assert_eq!(takes, vec![(Some("item"), Some(json!({"rep": 0})))]);
    let backs: Vec<_> = rule(spec, &iter)
        .close
        .as_ref()
        .expect("the iteration captures its item")
        .iter()
        .map(|a| (a.r(), a.get("n").cloned(), a.get("a").cloned()))
        .collect();
    assert_eq!(
        backs,
        vec![(
            Some(step.as_str()),
            Some(json!({"rep": 1})),
            Some(json!("@capture$"))
        )]
    );
    let again: Vec<_> = rule(spec, &step).open.iter().map(|a| a.r()).collect();
    assert_eq!(again, vec![Some(lp.as_str())]);
    assert_eq!(rule(spec, &step).close, None);

    // Nothing but the enclosing rule pushes the loop, and nothing pushes
    // the iteration or its step.
    for (name, rs) in &spec.rule {
        let Some(rs) = rs else { continue };
        for a in rs.open.iter().chain(rs.close.iter().flatten()) {
            if name != "doc" {
                assert_ne!(a.p(), Some(lp.as_str()), "{name} pushes the loop");
            }
            assert_ne!(a.p(), Some(iter.as_str()), "{name} pushes the iteration");
            assert_ne!(a.p(), Some(step.as_str()), "{name} pushes the step");
        }
    }
}

#[test]
fn matches_a_terminal_item_and_re_enters_the_loop_in_one_alternative() {
    // `*"x"` needs no iteration rule: the loop is the one rule the helper
    // always was, and its continue alternative matches the item and
    // replaces the loop with itself.
    let p = Probe::new(
        vec![prod("doc", vec![vec![star(lit("x")), lit(";")]])],
        true,
        false,
    );
    let spec = &p.spec;
    let generated: Vec<&String> = spec.rule.keys().filter(|n| n.starts_with("_gen")).collect();
    assert_eq!(generated.len(), 1, "{generated:?}");
    let lp = generated[0].as_str();
    assert!(lp.contains("_star_"), "{lp}");
    let (entry, open) = rule(spec, lp).open.split_first().expect("alternatives");
    assert_eq!(entry.get("c"), Some(&json!({"n.rep": 0})));
    let takes: Vec<_> = open.iter().filter(|a| a.r().is_some()).collect();
    assert_eq!(takes.len(), 1);
    assert_eq!(takes[0].r(), Some(lp));
    let node = &takes[0].k().expect("k")["node$"];
    assert_eq!(node["init"], json!(false));
    assert_eq!(node["nterms"], json!(1));
    assert_eq!(rule(spec, lp).close, None);
}

#[test]
fn enters_a_loop_afresh_inside_an_item_of_the_same_loop() {
    // `v = "[" *v "]" / "x"`: the loop over `v` is reached again from
    // inside one of its own items. The counter the outer loop holds at 1
    // is cleared by the push of the item, so the inner loop allocates a
    // node of its own and the trees nest as the brackets do.
    let g = vec![prod(
        "v",
        vec![
            vec![lit("["), star(reference("v")), lit("]")],
            vec![lit("x")],
        ],
    )];
    let p = Probe::new(g, false, false);
    let out = p.parse("[x[xx]x]").out;
    assert_eq!(out["src"], json!("[x[xx]x]"));
    let srcs: Vec<&Value> = kids(&out).iter().map(|k| &k["src"]).collect();
    assert_eq!(srcs, vec![&json!("x"), &json!("[xx]"), &json!("x")]);
    let inner: Vec<&Value> = kids(&kids(&out)[1]).iter().map(|k| &k["src"]).collect();
    assert_eq!(inner, vec![&json!("x"), &json!("x")]);
}

// Every loop allocates the node its iterations accumulate into once, on
// the way in, and it is a node of its own rather than the node of the
// rule that pushed it. The `rep` counter is what tells the way in from
// the way back, and counters are INHERITED: a pushed rule and a replacing
// rule both start with a copy of their predecessor's. So a loop reached
// inside an item of another loop, beside another loop, or inside a
// loop's group could see the 1 its neighbour set, skip its entry and
// write into whatever node it was pushed with. The push of an item clears
// the counter, which is what prevents that; these watch the engine to
// show it does, from every place a loop can be reached. Twin of the
// "the loop counter" block in `ts/test/repeat-depth.test.js`.

struct EntryCase {
    name: &'static str,
    grammar: fn() -> Vec<Production>,
    src: &'static str,
    value: Option<fn() -> Value>,
}

fn cell() -> Production {
    prod("cell", vec![vec![rx("[a-z]", "")]])
}

fn entry_cases() -> Vec<EntryCase> {
    vec![
        EntryCase {
            name: "a star inside the item of a star",
            grammar: || {
                vec![
                    prod("doc", vec![vec![star(reference("row"))]]),
                    prod(
                        "row",
                        vec![vec![lit("["), star(reference("cell")), lit("]")]],
                    ),
                    cell(),
                ]
            },
            src: "[ab][][c]",
            value: None,
        },
        EntryCase {
            name: "two sibling stars",
            grammar: || {
                vec![
                    prod(
                        "doc",
                        vec![vec![
                            star(reference("cell")),
                            lit(";"),
                            star(reference("cell")),
                        ]],
                    ),
                    cell(),
                ]
            },
            src: "ab;cd",
            value: None,
        },
        EntryCase {
            name: "two sibling stars inside the item of a star",
            grammar: || {
                vec![
                    prod("doc", vec![vec![star(reference("row"))]]),
                    prod(
                        "row",
                        vec![vec![
                            lit("["),
                            star(reference("cell")),
                            lit(";"),
                            star(reference("cell")),
                            lit("]"),
                        ]],
                    ),
                    cell(),
                ]
            },
            src: "[ab;cd][;][a;]",
            value: None,
        },
        EntryCase {
            name: "a star directly inside a star's group",
            grammar: || {
                vec![
                    prod(
                        "doc",
                        vec![vec![star(group(vec![vec![
                            lit("("),
                            star(reference("cell")),
                            lit(")"),
                        ]]))]],
                    ),
                    cell(),
                ]
            },
            src: "(ab)()(c)",
            value: None,
        },
        EntryCase {
            name: "three stars, each in the group of the one outside it",
            grammar: || {
                vec![
                    prod(
                        "doc",
                        vec![vec![star(group(vec![vec![
                            lit("["),
                            star(group(vec![vec![
                                lit("("),
                                star(reference("cell")),
                                lit(")"),
                            ]])),
                            lit("]"),
                        ]]))]],
                    ),
                    cell(),
                ]
            },
            src: "[(ab)()][][(c)]",
            value: None,
        },
        EntryCase {
            name: "a plus and an m* inside a star's group",
            grammar: || {
                vec![
                    prod(
                        "doc",
                        vec![vec![star(group(vec![vec![
                            lit("<"),
                            plus(reference("cell")),
                            lit(";"),
                            Element::rep(2, None, reference("cell")),
                            lit(">"),
                        ]]))]],
                    ),
                    cell(),
                ]
            },
            src: "<a;bc><abc;def>",
            value: None,
        },
        EntryCase {
            name: "a star of a terminal inside a star's group",
            grammar: || {
                vec![prod(
                    "doc",
                    vec![vec![star(group(vec![vec![
                        lit("("),
                        star(lit("x")),
                        lit(")"),
                    ]]))]],
                )]
            },
            src: "(xx)()(x)",
            value: None,
        },
        EntryCase {
            name: "a loop reached again inside one of its own items",
            grammar: || {
                vec![prod(
                    "v",
                    vec![
                        vec![lit("["), star(reference("v")), lit("]")],
                        vec![lit("x")],
                    ],
                )]
            },
            src: "[x[xx[]]x]",
            value: None,
        },
        EntryCase {
            name: "stars in an item that builds a value",
            grammar: pairs,
            src: "ab=12;=;c=3;",
            value: Some(|| {
                json!({
                    "rule": "doc",
                    "src": "",
                    "kids": [
                        {"key": "ab", "val": "12"},
                        {"key": "", "val": ""},
                        {"key": "c", "val": "3"},
                    ],
                })
            }),
        },
    ]
}

/// The loops of a spec that its start rule can reach: the rules whose
/// first alternative is a loop's entry. (A production whose only use was
/// inlined, as a leading member is, keeps its own loop, which nothing
/// runs.)
fn loops_of(spec: &GrammarSpec) -> BTreeSet<String> {
    let start = spec.options["rule"]["start"]
        .as_str()
        .expect("a start rule")
        .to_string();
    let mut reached = vec![start.clone()];
    let mut seen: BTreeSet<String> = BTreeSet::from([start]);
    while let Some(name) = reached.pop() {
        let Some(Some(rs)) = spec.rule.get(&name) else {
            continue;
        };
        for a in rs.open.iter().chain(rs.close.iter().flatten()) {
            for to in [a.p(), a.r()].into_iter().flatten() {
                if seen.insert(to.to_string()) {
                    reached.push(to.to_string());
                }
            }
        }
    }
    seen.into_iter()
        .filter(|name| {
            spec.rule
                .get(name)
                .and_then(Option::as_ref)
                .and_then(|rs| rs.open.first())
                .and_then(|a| a.get("c"))
                .and_then(|c| c.get("n.rep"))
                == Some(&json!(0))
        })
        .collect()
}

/// What the engine saw of the loops' rules as each one finished: the
/// node cells each loop ran in, by the rule that pushed it, and every
/// loop rule whose node was its pusher's.
#[derive(Default)]
struct Ran {
    // (loop, the pusher's rule id) -> the addresses of its node cells
    nodes: BTreeMap<(String, usize), BTreeSet<usize>>,
    borrowed: Vec<String>,
}

/// One case, in one mode: what went wrong, if anything.
fn enters_afresh_case(c: &EntryCase, builtins: bool) -> Result<(), String> {
    let productions = (c.grammar)();
    let start = productions[0].name.clone();
    let spec = emit_grammar_spec(
        &Grammar::new(productions),
        &ConvertOptions::tag("depth").start(start).builtins(builtins),
    )
    .map_err(|e| format!("emit: {e}"))?;
    let loops = loops_of(&spec);
    if loops.is_empty() {
        return Err("no loop is reachable".into());
    }
    let mut parser = tabnas::Tabnas::new();
    spec.install(&mut parser)
        .map_err(|e| format!("install: {e}"))?;
    let ran = Arc::new(Mutex::new(Ran::default()));
    let seen = ran.clone();
    let watched = loops.clone();
    parser.subscribe_rule_done(move |rule, _context, _done| {
        let name = rule.name.as_str();
        if !watched.contains(name) {
            return;
        }
        let mut ran = seen.lock().unwrap_or_else(|p| p.into_inner());
        if rule
            .parent_node
            .as_ref()
            .is_some_and(|pusher| Rc::ptr_eq(pusher, &rule.node))
        {
            ran.borrowed.push(name.to_string());
        }
        let pusher = rule.parent_rule.as_ref().map_or(usize::MAX, |p| p.i);
        ran.nodes
            .entry((name.to_string(), pusher))
            .or_default()
            .insert(Rc::as_ptr(&rule.node) as usize);
    });
    let out = parser
        .parse(c.src)
        .map_err(|e| format!("parse of {:?}: {e}", c.src))?
        .to_json();
    if let Some(value) = c.value {
        if out != value() {
            return Err(format!("value {out}, want {}", value()));
        }
    }

    let ran = ran.lock().unwrap_or_else(|p| p.into_inner());
    if !ran.borrowed.is_empty() {
        return Err(format!(
            "a loop ran in the node of its pusher: {:?}",
            ran.borrowed
        ));
    }
    let mut entered = BTreeSet::new();
    for ((name, _), nodes) in &ran.nodes {
        entered.insert(name.clone());
        if nodes.len() != 1 {
            return Err(format!(
                "{name} allocated {} nodes on one entry",
                nodes.len()
            ));
        }
    }
    if entered != loops {
        return Err(format!(
            "the input must reach every loop: ran {entered:?} of {loops:?}"
        ));
    }
    Ok(())
}

fn enters_afresh(builtins: bool) {
    let mode = if builtins { "builtins" } else { "closures" };
    let failures: Vec<String> = entry_cases()
        .iter()
        .filter_map(|c| {
            enters_afresh_case(c, builtins)
                .err()
                .map(|e| format!("{} ({mode}): {e}", c.name))
        })
        .collect();
    assert!(failures.is_empty(), "{}", failures.join("\n"));
}

#[test]
fn the_loop_counter_enters_every_loop_afresh_closures() {
    enters_afresh(false);
}

#[test]
fn the_loop_counter_enters_every_loop_afresh_builtins() {
    enters_afresh(true);
}
