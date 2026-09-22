// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

// Smoke tests for the notation-neutral compiler, driven through the IR
// with no front-end present. The heavy verification lives downstream, in
// the front-ends' suites, and in tests/oracle_test.rs against the
// canonical compiler's own output. Mirrors go/bnf_test.go and
// ts/test/bnf.test.js.

mod common;

use std::sync::Arc;

use common::{group, opt, parse_with, plus, prod, reference, rx, sens_term, star, term, tok};
use serde_json::{json, Map, Value};
use tabnas_bnf::{
    attach_actions, builtin_token, compile_spec, eliminate_left_recursion, emit_grammar_spec,
    escape_regexp, mark_listing, to_jsonic, to_pure_spec, to_recognition_spec, ActionFn, AltSpec,
    CompileOptions, ConvertOptions, Element, Grammar, GrammarSpec, JsonicOptions, Kind, Production,
    RuleSpec, SrcSpan,
};

fn emit(grammar: Grammar, opts: ConvertOptions) -> GrammarSpec {
    match emit_grammar_spec(&grammar, &opts) {
        Ok(spec) => spec,
        Err(e) => panic!("emit failed: {e}"),
    }
}

fn demo() -> ConvertOptions {
    ConvertOptions::tag("demo")
}

/// Every alt of every rule: the counter machinery is spread across a
/// dispatcher and its `$altN` chain rules.
fn alts_of(spec: &GrammarSpec) -> Vec<AltSpec> {
    let mut out = Vec::new();
    for rs in spec.rule.values().flatten() {
        out.extend(rs.open.iter().cloned());
        if let Some(close) = &rs.close {
            out.extend(close.iter().cloned());
        }
    }
    out
}

fn n_of(alt: &AltSpec) -> Option<&Map<String, Value>> {
    alt.get("n").and_then(Value::as_object)
}

fn strict(spec: &GrammarSpec) -> String {
    to_jsonic(
        &to_pure_spec(spec).expect("pure"),
        JsonicOptions {
            strict: true,
            indent: None,
        },
    )
}

#[test]
fn compiles_a_minimal_ir_grammar() {
    let spec = emit(
        Grammar::new(vec![
            prod("val", vec![vec![reference("add")]]),
            prod("add", vec![vec![tok("#NR")]]),
        ]),
        demo(),
    );
    assert!(spec.rule.contains_key("val"));
    assert!(spec.rule.contains_key("add"));
}

#[test]
fn stamps_the_caller_tag_not_a_notation() {
    // The tag must be the caller's, never a notation this package
    // assumes. Nothing here may know what syntax a grammar came from.
    let spec = emit(
        Grammar::new(vec![prod("top", vec![vec![term("x")]])]),
        demo(),
    );
    let text = spec.to_value().to_string();
    assert!(
        text.contains("demo"),
        "expected the caller's tag on an emitted alt"
    );
    assert!(!text.contains("\"abnf\""), "must not assume a notation");
}

#[test]
fn defaults_the_tag_to_bnf() {
    let spec = emit(
        Grammar::new(vec![prod("top", vec![vec![term("x")]])]),
        ConvertOptions::default(),
    );
    let tags: Vec<String> = alts_of(&spec)
        .iter()
        .map(|a| a.g().unwrap_or("").split(',').next().unwrap().to_string())
        .collect();
    assert!(
        !tags.is_empty(),
        "no group tags emitted, so this test proves nothing"
    );
    for tag in tags {
        assert_eq!(
            tag, "bnf",
            "the default is this package's own name, never a notation"
        );
    }
}

#[test]
fn does_not_reuse_an_action_ref_across_attach_actions_calls() {
    // Two calls must not collide: resetting the counter meant the second
    // call reused the first ref, overwrote its function, and one action
    // ran twice while the other never ran.
    let mut spec = emit(
        Grammar::new(vec![prod("op", vec![vec![term("inc")], vec![term("dec")]])]),
        demo().marks(true),
    );
    let noop: ActionFn = Arc::new(|_r, _c| Ok(()));
    for key in ["@op:o:INC", "@op:o:DEC"] {
        attach_actions(&mut spec, vec![(key.to_string(), vec![noop.clone()])])
            .unwrap_or_else(|e| panic!("attach {key} failed: {e}"));
    }
    let mut refs: Vec<&String> = spec.refs.keys().filter(|k| k.contains("_user")).collect();
    refs.sort();
    assert_eq!(
        refs.len(),
        2,
        "expected 2 user-action refs after 2 calls, got {refs:?}"
    );
    // The alts must point at DIFFERENT refs.
    let mut used = std::collections::HashSet::new();
    for alt in &spec.rule["op"].as_ref().unwrap().open {
        for name in alt.actions() {
            if name.contains("_user") {
                assert!(
                    used.insert(name.clone()),
                    "two alts share the action ref {name}"
                );
            }
        }
    }
    assert_eq!(used.len(), 2);
}

#[test]
fn names_generated_action_refs_like_typescript() {
    // TypeScript names them `@bnf_a<n>`; the Go port still says
    // `@abnf_a<n>` (an open divergence pinned on both of those sides).
    // This port follows the canonical name.
    let spec = emit(
        Grammar::new(vec![prod("top", vec![vec![term("x")]])]),
        demo(),
    );
    assert!(
        !spec.refs.is_empty(),
        "no action refs emitted, so this test proves nothing"
    );
    for name in spec.refs.keys() {
        assert!(
            name.starts_with("@bnf_a"),
            "ref {name} should be named @bnf_a<n>"
        );
    }
}

#[test]
fn lifts_a_single_literal_production_into_a_named_token() {
    let spec = emit(
        Grammar::new(vec![
            prod("top", vec![vec![reference("PL")]]),
            prod("PL", vec![vec![term("+")]]),
        ]),
        demo(),
    );
    assert_eq!(spec.options["fixed"]["token"]["#PL"], json!("+"));
}

#[test]
fn eliminates_left_recursion_into_iterative_form() {
    let out = eliminate_left_recursion(&Grammar::new(vec![
        prod(
            "expr",
            vec![
                vec![reference("expr"), term("+"), reference("num")],
                vec![reference("num")],
            ],
        ),
        prod("num", vec![vec![tok("#NR")]]),
    ]))
    .expect("eliminate");
    let expr = out
        .productions
        .iter()
        .find(|p| p.name == "expr")
        .expect("expr");
    for alt in &expr.alts {
        assert!(
            !alt.first().is_some_and(|el| el.is_ref_to("expr")),
            "expr must no longer start with itself"
        );
    }
}

#[test]
fn collects_rule_references_from_a_sequence() {
    let mut refs = indexmap::IndexSet::new();
    tabnas_bnf::refs_in(
        &[
            reference("a"),
            star(reference("b")),
            group(vec![vec![reference("c")]]),
        ],
        &mut refs,
    );
    let got: Vec<&String> = refs.iter().collect();
    assert_eq!(got, vec!["a", "b", "c"]);
}

#[test]
fn diagnostics_name_the_notation() {
    // A front-end's users should see their own notation's name on an
    // error about their own syntax, never "bnf:".
    let err = emit_grammar_spec(
        &Grammar::new(vec![prod("A", vec![vec![reference("A"), term("x")]])]),
        &ConvertOptions::tag("gbnf"),
    )
    .expect_err("a purely left-recursive rule is refused");
    assert!(
        err.message.starts_with("gbnf: "),
        "expected the caller's tag to prefix the diagnostic, got {:?}",
        err.message
    );
}

#[test]
fn escapes_regex_metacharacters_in_literals() {
    assert!(
        escape_regexp("a.b").contains("\\."),
        "expected the dot escaped"
    );
    assert_eq!(escape_regexp("a.b*c"), "a\\.b\\*c");
}

#[test]
fn maps_bareword_names_to_engine_builtin_tokens() {
    for (name, want) in [("NR", "#NR"), ("TX", "#TX"), ("ST", "#ST"), ("VL", "#VL")] {
        assert_eq!(builtin_token(name), Some(want));
    }
    assert_eq!(builtin_token("XX"), None);
}

#[test]
fn serialises_a_spec_as_jsonic_text() {
    let spec = emit(
        Grammar::new(vec![prod("top", vec![vec![term("x")]])]),
        demo().builtins(true),
    );
    let text = to_jsonic(&to_pure_spec(&spec).unwrap(), JsonicOptions::default());
    assert!(!text.is_empty());
    assert!(
        text.contains("top: {"),
        "relaxed jsonic uses bare keys: {text}"
    );
}

// A repetition helper terminates on an empty alternative, which names
// no token. The engine only offers a matcher where the active rule names
// it, so without a guard the token that follows the repetition is never
// lexed and the parse dies at the loop exit.
#[test]
fn guards_a_repetition_helper_with_its_follow_set() {
    let spec = emit(
        Grammar::new(vec![
            prod("root", vec![vec![star(reference("W")), reference("D")]]),
            prod("W", vec![vec![rx("[ ]", "")]]),
            prod("D", vec![vec![rx("[0-9]", "")]]),
        ]),
        demo(),
    );
    // The helper itself, not its `$alt0`/`$step1` chain rules.
    let (_, rs) = spec
        .rule
        .iter()
        .find(|(name, _)| {
            name.starts_with("_gen") && name.contains("_star_") && !name.contains('$')
        })
        .expect("a generated star helper");
    let rs = rs.as_ref().unwrap();
    let d_token = spec.rule["D"].as_ref().unwrap().open[0]
        .s()
        .unwrap()
        .to_string();
    let guards: Vec<&AltSpec> = rs
        .open
        .iter()
        .filter(|a| a.s() == Some(d_token.as_str()))
        .collect();
    assert_eq!(
        guards.len(),
        1,
        "the star helper must name D on its terminating alternative"
    );
    assert_eq!(
        guards[0].b(),
        Some(1),
        "the FOLLOW guard must push its peeked token back"
    );
    assert!(
        guards[0].p().is_none() && guards[0].r().is_none(),
        "the FOLLOW guard must not push or replace a rule"
    );
    // The unguarded empty alternative stays last as the fallback.
    assert!(
        rs.open.last().unwrap().s().is_none(),
        "expected a bare fallback alternative"
    );
}

// `root = star X Y` where X is nullable: what follows the star is
// FIRST(X) AND FIRST(Y), because X can vanish.
#[test]
fn carries_follow_through_a_nullable_suffix() {
    let spec = emit(
        Grammar::new(vec![
            prod(
                "root",
                vec![vec![star(reference("W")), reference("X"), reference("Y")]],
            ),
            prod("W", vec![vec![rx("[ ]", "")]]),
            prod("X", vec![vec![tok("#NR")], vec![]]),
            prod("Y", vec![vec![tok("#TX")]]),
        ]),
        demo(),
    );
    let (_, rs) = spec
        .rule
        .iter()
        .find(|(name, _)| {
            name.starts_with("_gen") && name.contains("_star_") && !name.contains('$')
        })
        .expect("a generated star helper");
    let guarded: Vec<&str> = rs
        .as_ref()
        .unwrap()
        .open
        .iter()
        .filter_map(|a| a.s())
        .collect();
    assert!(guarded.contains(&"#NR"), "expected FIRST(X) in the guard");
    assert!(
        guarded.contains(&"#TX"),
        "expected FIRST(Y) in the guard, since X is nullable"
    );
}

// `^` binds tighter than `|`, so `^a|bc` anchors only the first branch.
#[test]
fn groups_a_regex_before_anchoring_it() {
    let spec = emit(
        Grammar::new(vec![prod("top", vec![vec![rx("a|bc", "")]])]),
        demo(),
    );
    let tokens = spec.options["match"]["token"].as_object().unwrap();
    let source = tokens.values().next().unwrap().as_str().unwrap();
    assert_eq!(source, "@~/^(?:a|bc)/");
    let re = regex::Regex::new("^(?:a|bc)").unwrap();
    assert!(
        !re.is_match("xbc"),
        "the second branch must not match at a non-zero offset"
    );
    assert!(re.is_match("bc"));
}

// `emit_grammar_spec` works on a copy and reads these fields off it, so
// a copy that carried only `productions` dropped them silently.
#[test]
fn keeps_grammar_level_remove_and_clear_all_across_the_internal_copy() {
    let grammar = Grammar {
        remove: vec!["gone".into()],
        clear_all: true,
        ..Grammar::new(vec![
            prod("top", vec![vec![tok("#NR")]]),
            prod("gone", vec![vec![tok("#TX")]]),
        ])
    };
    let before = grammar.clone();
    let spec = emit(grammar.clone(), demo().start("top"));
    assert_eq!(
        spec.rule.get("gone"),
        Some(&None),
        "expected `gone` to be removed"
    );
    assert!(spec.clear, "expected clear_all to set spec.clear");
    assert_eq!(spec.options["fixed"]["token"]["#gone"], Value::Null);
    assert_eq!(grammar, before, "the caller's grammar is untouched");
    assert!(spec.to_value()["clear"] == json!(true));
}

// The IR reserves no names, so a grammar may legitimately contain a
// production called `__start__`.
#[test]
fn does_not_overwrite_a_user_rule_named_start() {
    let spec = emit(
        Grammar::new(vec![prod("__start__", vec![vec![tok("#NR")]])]),
        demo(),
    );
    let start = spec.options["rule"]["start"].as_str().unwrap().to_string();
    assert_ne!(
        start, "__start__",
        "the wrapper must not take the user rule's name"
    );
    assert_eq!(
        spec.rule["__start__"].as_ref().unwrap().open[0].s(),
        Some("#NR")
    );
    assert_eq!(
        spec.rule[&start].as_ref().unwrap().open[0].p(),
        Some("__start__")
    );
}

// Strict mode promises valid JSON; a raw tab or CR inside a string makes
// a JSON reader reject it.
#[test]
fn escapes_every_control_character_in_strict_jsonic_output() {
    let text = to_jsonic(
        &json!({"s": "a\tb\rcd\u{1}"}),
        JsonicOptions {
            strict: true,
            indent: None,
        },
    );
    let back: Value = serde_json::from_str(&text).expect("valid JSON");
    assert_eq!(back["s"], "a\tb\rcd\u{1}");
}

// ---- suffix debt (issue #6) ----------------------------------------
//
// Left-recursion elimination turns `A = ["x"] A "y" / "z"` into
// `A = ( "x" A "y" | "z" ) "y"*`: a correct CFG and a broken parser.
// The tail loop is greedy, so the inner A eats the `"y"` the enclosing
// alternative still owes, and no lookahead settles it.

fn hidden_left_rec(alts: Vec<Vec<Element>>) -> Grammar {
    Grammar::new(vec![prod("A", alts)])
}

fn star_helpers(spec: &GrammarSpec) -> Vec<(&String, &RuleSpec)> {
    spec.rule
        .iter()
        .filter(|(name, _)| name.contains("_star_") && !name.contains('$'))
        .filter_map(|(name, rs)| rs.as_ref().map(|rs| (name, rs)))
        .collect()
}

#[test]
fn suffix_debt_guards_the_contested_loop() {
    let spec = emit(
        hidden_left_rec(vec![
            vec![opt(sens_term("x")), reference("A"), sens_term("y")],
            vec![sens_term("z")],
        ]),
        demo(),
    );

    // The alternative that pushes the inner A increments the counter...
    let mut counter = String::new();
    let mut pushes = 0;
    for a in alts_of(&spec) {
        let Some(n) = n_of(&a) else { continue };
        if n.is_empty() {
            continue;
        }
        pushes += 1;
        for (name, delta) in n {
            counter = name.clone();
            assert_eq!(delta, &json!(1), "expected the push to add one debt");
        }
        assert_eq!(a.p(), Some("A"), "the debt must ride on the push of A");
    }
    assert_eq!(pushes, 1, "expected exactly one debt-carrying push");

    // ...and the tail loop's continue alternative refuses to run while
    // any debt is outstanding.
    let mut guards = 0;
    for a in alts_of(&spec) {
        let Some(c) = a.get("c").and_then(Value::as_object) else {
            continue;
        };
        guards += 1;
        assert_eq!(
            c.get(&format!("n.{counter}")),
            Some(&json!(0)),
            "expected the guard to test {counter} == 0, got {c:?}"
        );
    }
    assert_eq!(guards, 1, "expected exactly one guarded alternative");

    // The loop's exits stay unguarded, so it yields rather than fails.
    for (name, rs) in star_helpers(&spec) {
        for (i, a) in rs.open.iter().enumerate() {
            if i > 0 {
                assert!(
                    a.get("c").is_none(),
                    "{name}: exit alternative {i} must stay unconditional"
                );
            }
        }
    }
}

#[test]
fn suffix_debt_resets_across_a_reanchoring_alternative() {
    // `"(" A ")"` owes a `")"`, not a `"y"`, so the A it pushes must
    // start from a clean slate or `x(zy)y` cannot parse.
    let spec = emit(
        hidden_left_rec(vec![
            vec![opt(sens_term("x")), reference("A"), sens_term("y")],
            vec![sens_term("("), reference("A"), sens_term(")")],
            vec![sens_term("z")],
        ]),
        demo(),
    );
    let mut deltas: Vec<i64> = alts_of(&spec)
        .iter()
        .filter_map(n_of)
        .flat_map(|n| n.values().filter_map(Value::as_i64).collect::<Vec<_>>())
        .collect();
    deltas.sort();
    assert_eq!(
        deltas,
        vec![0, 1],
        "expected one increment and one barrier reset"
    );
}

#[test]
fn suffix_debt_leaves_uncontested_grammars_alone() {
    let cases = vec![
        // The loop repeats `"w"`; the only suffix after a self-reference
        // is `")"`, which never competes with it.
        (
            "disjoint suffix",
            hidden_left_rec(vec![
                vec![reference("A"), sens_term("w")],
                vec![sens_term("("), reference("A"), sens_term(")")],
                vec![sens_term("z")],
            ]),
        ),
        // A nullable suffix commits the enclosing frame to nothing.
        (
            "nullable suffix",
            hidden_left_rec(vec![
                vec![opt(sens_term("x")), reference("A"), opt(sens_term("y"))],
                vec![sens_term("z")],
            ]),
        ),
        (
            "plain direct left recursion",
            hidden_left_rec(vec![
                vec![reference("A"), sens_term("y")],
                vec![sens_term("z")],
            ]),
        ),
    ];
    for (name, g) in cases {
        for a in alts_of(&emit(g, demo())) {
            assert!(
                n_of(&a).is_none_or(Map::is_empty) && a.get("c").is_none(),
                "{name}: must compile exactly as before, got n={:?} c={:?}",
                a.get("n"),
                a.get("c")
            );
        }
    }
}

#[test]
fn suffix_debt_allocates_a_counter_per_contested_rule() {
    let spec = emit(
        Grammar::new(vec![
            prod(
                "B",
                vec![
                    vec![opt(sens_term("p")), reference("B"), sens_term("q")],
                    vec![reference("A")],
                ],
            ),
            prod(
                "A",
                vec![
                    vec![opt(sens_term("x")), reference("A"), sens_term("y")],
                    vec![sens_term("z")],
                ],
            ),
        ]),
        demo(),
    );
    let mut counters = std::collections::HashSet::new();
    for a in alts_of(&spec) {
        if let Some(c) = a.get("c").and_then(Value::as_object) {
            for path in c.keys() {
                counters.insert(path.clone());
            }
        }
    }
    assert_eq!(
        counters.len(),
        2,
        "expected one counter per contested rule, got {counters:?}"
    );
}

// Hidden left recursion has to become direct left recursion before the
// tail loop exists at all.
#[test]
fn expands_nullable_left_prefixes() {
    let out = eliminate_left_recursion(&hidden_left_rec(vec![
        vec![opt(sens_term("x")), reference("A"), sens_term("y")],
        vec![sens_term("z")],
    ]))
    .expect("eliminate");
    let a = out
        .productions
        .iter()
        .find(|p| p.name == "A")
        .expect("rule A survives");
    for alt in &a.alts {
        assert!(
            !alt.first().is_some_and(|el| el.is_ref_to("A")),
            "A still re-enters itself immediately"
        );
        if alt.len() > 1 {
            assert!(
                !(matches!(alt[0].kind, Kind::Opt { .. }) && alt[1].is_ref_to("A")),
                "A still re-enters itself behind an opt"
            );
        }
    }
}

#[test]
fn suffix_debt_guards_only_the_contested_branches() {
    // The loop repeats `"y"` and `"w"`; the suffix owes a `"y"`.
    // Blocking the `"w"` branch too would reject `xzwy`.
    let spec = emit(
        hidden_left_rec(vec![
            vec![reference("A"), sens_term("y")],
            vec![reference("A"), sens_term("w")],
            vec![sens_term("x"), reference("A"), sens_term("y")],
            vec![sens_term("z")],
        ]),
        demo(),
    );
    let token_of = |lit: &str| -> String {
        spec.options["fixed"]["token"]
            .as_object()
            .unwrap()
            .iter()
            .find(|(_, v)| v.as_str() == Some(lit))
            .map(|(k, _)| k.clone())
            .unwrap_or_else(|| panic!("no token for {lit:?}"))
    };
    let mut guarded = std::collections::BTreeSet::new();
    let mut open = std::collections::BTreeSet::new();
    for (_, rs) in star_helpers(&spec) {
        for a in &rs.open {
            let (Some(s), Some(_)) = (a.s(), a.p()) else {
                continue;
            };
            let head = s.split(' ').next().unwrap().to_string();
            if a.get("c").is_some() {
                guarded.insert(head);
            } else {
                open.insert(head);
            }
        }
    }
    assert_eq!(guarded.into_iter().collect::<Vec<_>>(), vec![token_of("y")]);
    assert_eq!(open.into_iter().collect::<Vec<_>>(), vec![token_of("w")]);
}

#[test]
fn suffix_debt_sees_a_self_reference_inside_a_group() {
    // The detector runs before desugar, where the recursive call is
    // still inside an IR group.
    let spec = emit(
        hidden_left_rec(vec![
            vec![reference("A"), sens_term("y")],
            vec![group(vec![
                vec![sens_term("x"), reference("A"), sens_term("y")],
                vec![sens_term("z")],
            ])],
        ]),
        demo(),
    );
    let guards = alts_of(&spec)
        .iter()
        .filter(|a| a.get("c").is_some())
        .count();
    assert_eq!(guards, 1, "expected the grouped seed to allocate a guard");
}

#[test]
fn suffix_debt_counter_name_matches_typescript() {
    // TypeScript sanitises with a Unicode-aware regular expression, so an
    // astral rule name reduces to one underscore per code point.
    for (name, want) in [
        ("a-b", "debt_a_b"),
        ("\u{1F600}", "debt__"),
        ("x\u{1F600}y", "debt_x_y"),
    ] {
        let spec = emit(
            Grammar::new(vec![prod(
                name,
                vec![
                    vec![opt(sens_term("x")), reference(name), sens_term("y")],
                    vec![sens_term("z")],
                ],
            )]),
            demo(),
        );
        let got = alts_of(&spec)
            .iter()
            .filter_map(n_of)
            .flat_map(|n| n.keys().cloned().collect::<Vec<_>>())
            .next()
            .unwrap_or_default();
        assert_eq!(got, want, "rule {name:?}");
    }
}

// Compilation mode drops every closure; a guard that needed one would
// make these grammars uncompilable rather than merely untree-built.
#[test]
fn keeps_the_debt_guard_expressible_as_pure_data() {
    let spec = emit(
        hidden_left_rec(vec![
            vec![opt(sens_term("x")), reference("A"), sens_term("y")],
            vec![sens_term("z")],
        ]),
        demo().builtins(true),
    );
    let text = compile_spec(
        &spec,
        CompileOptions {
            strict: true,
            ..Default::default()
        },
    )
    .expect("compile");
    let _: Value = serde_json::from_str(&text).expect("valid JSON");
    assert!(text.contains("\"n.debt_A\""), "{text}");
}

// `NR = "NR"` wanted `#NR`, which the lexer's number matcher owns. The
// engine rejects a fixed.token entry under a matcher-owned name.
#[test]
fn never_allocates_a_lifted_literal_an_engine_owned_token_name() {
    let lift = |lit: &str| -> Map<String, Value> {
        let spec = emit(
            Grammar::new(vec![
                prod("top", vec![vec![reference(lit)]]),
                prod(lit, vec![vec![sens_term(lit)]]),
            ]),
            demo(),
        );
        spec.options["fixed"]["token"].as_object().unwrap().clone()
    };
    for owned in ["NR", "TX", "ST", "VL", "ZZ", "SP", "LN", "CM"] {
        let fixed = lift(owned);
        assert!(
            !fixed.contains_key(&format!("#{owned}")),
            "#{owned} is engine-owned and must not be allocated to a literal"
        );
        // The literal still gets a token, just under a free name.
        let values: Vec<&Value> = fixed.values().collect();
        assert_eq!(values, vec![&json!(owned)]);
    }
    // A name the engine does not own is still used as-is.
    let pl = lift("PL");
    assert_eq!(pl.len(), 1);
    assert_eq!(pl["#PL"], json!("PL"));
}

// Left factoring rewrites a user rule's alternatives, so it must fire
// only where the dispatcher genuinely cannot separate them.
mod left_factoring_is_bounded_by_dispatch_lookahead {
    use super::*;

    fn factored(grammar: Grammar, name: &str) -> bool {
        emit(grammar, demo())
            .rule
            .keys()
            .any(|rn| rn.starts_with(&format!("{name}$fact")))
    }

    fn two_alts(prefix: Vec<Element>) -> Grammar {
        let mut a = prefix.clone();
        a.push(reference("P"));
        let mut b = prefix;
        b.push(reference("Q"));
        Grammar::new(vec![
            prod("g", vec![a, b]),
            prod("P", vec![vec![term("p")]]),
            prod("Q", vec![vec![term("q")]]),
        ])
    }

    #[test]
    fn leaves_a_short_prefix_of_plain_terminals_to_the_dispatcher() {
        assert!(!factored(two_alts(vec![term("a"), term("x")]), "g"));
    }

    #[test]
    fn leaves_a_short_prefix_wrapped_in_a_group() {
        assert!(!factored(
            two_alts(vec![group(vec![vec![term("a")]]), term("x")]),
            "g"
        ));
    }

    #[test]
    fn leaves_a_short_prefix_containing_an_optional() {
        assert!(!factored(two_alts(vec![opt(term("-")), term("1")]), "g"));
    }

    #[test]
    fn leaves_a_bounded_repetition_that_fits_the_lookahead() {
        assert!(!factored(
            two_alts(vec![Element::rep(2, Some(2), term("a"))]),
            "g"
        ));
    }

    #[test]
    fn factors_a_prefix_longer_than_the_lookahead() {
        let long: Vec<Element> = ["a", "b", "c", "d", "e"].iter().map(|s| term(s)).collect();
        assert!(factored(two_alts(long), "g"));
    }

    #[test]
    fn factors_an_unbounded_repetition() {
        assert!(factored(two_alts(vec![plus(term("a"))]), "g"));
    }

    #[test]
    fn keeps_a_mark_per_branch_when_it_does_not_factor() {
        let listing = mark_listing(&emit(
            two_alts(vec![group(vec![vec![term("a")]]), term("x")]),
            demo().marks(true),
        ));
        let marks: Vec<&str> = listing.lines().filter(|l| l.contains("o:")).collect();
        assert_eq!(marks.len(), 2, "{listing}");
        assert_ne!(marks[0], marks[1], "{listing}");
    }
}

// Coverage feeding the contest checks must agree with what a matcher
// actually matches: a bare literal is case-insensitive by default, so
// `"G"` also matches `g` and contests a lowercase class.
#[test]
fn counts_both_cases_of_a_case_insensitive_literal_as_covered() {
    let spec = emit(
        Grammar::new(vec![
            prod(
                "g",
                vec![vec![term("G"), reference("L")], vec![reference("L")]],
            ),
            prod("L", vec![vec![rx("[a-z]", "")]]),
        ]),
        demo(),
    );
    let first = &spec.rule["g"].as_ref().unwrap().open[0];
    assert!(
        first.s().is_some_and(|s| s.split(' ').count() > 1),
        "expected a multi-token keyword guard first, got {:?}",
        spec.rule["g"].as_ref().unwrap().to_value()
    );
}

// A class must be able to fire at ANY lookahead slot, so every class
// token is eager, which is how it serialises (`@~/…/`) on every side.
#[test]
fn marks_a_character_class_token_eager() {
    let spec = emit(
        Grammar::new(vec![
            prod(
                "x",
                vec![vec![
                    term("0"),
                    opt(group(vec![vec![star(reference("d")), reference("t")]])),
                ]],
            ),
            prod("d", vec![vec![rx("[\\u0031-\\u0039]", "")]]),
            prod("t", vec![vec![rx("[\\u0061-\\u007a]", "")]]),
        ]),
        demo(),
    );
    let tokens = spec.options["match"]["token"].as_object().unwrap();
    assert_eq!(tokens.len(), 2);
    for (name, re) in tokens {
        assert!(
            re.as_str().unwrap().starts_with("@~/"),
            "class token {name} should be eager: {re}"
        );
    }
}

// The alt `m` mark is compiler-internal: an emitted `m` makes the
// grammar FAIL the engine's schema validation.
mod marks_do_not_reach_the_wire {
    use super::*;

    fn marked() -> GrammarSpec {
        emit(
            Grammar::new(vec![prod("op", vec![vec![term("inc")], vec![term("dec")]])]),
            demo().marks(true).builtins(true),
        )
    }

    #[test]
    fn mark_listing_still_reads_marks_off_the_in_memory_spec() {
        assert!(mark_listing(&marked()).contains("op  o:"));
    }

    #[test]
    fn pure_and_recognition_both_drop_m() {
        let spec = marked();
        for (name, out) in [
            ("to_pure_spec", to_pure_spec(&spec).unwrap()),
            ("to_recognition_spec", to_recognition_spec(&spec).unwrap()),
        ] {
            let text = to_jsonic(
                &out,
                JsonicOptions {
                    strict: true,
                    indent: None,
                },
            );
            assert!(!text.contains("\"m\":"), "{name} emitted a mark");
        }
        assert!(
            !spec.to_value().to_string().contains("\"m\":"),
            "the engine document carries no mark"
        );
    }

    #[test]
    fn shaping_does_not_mutate_the_callers_spec() {
        let spec = marked();
        to_pure_spec(&spec).unwrap();
        assert!(mark_listing(&spec).contains("op  o:"));
    }
}

// Production names are untrusted keys: a grammar is text, and nothing
// about a name may make a rule disappear. Mirrors
// ts/test/prototype-keys.test.js, which guards the JavaScript hazard;
// here the map has no prototype to collide with, and this pins it.
#[test]
fn prototype_shaped_production_names_survive() {
    for name in [
        "__proto__",
        "constructor",
        "toString",
        "hasOwnProperty",
        "valueOf",
    ] {
        let spec = emit(
            Grammar::new(vec![
                prod(name, vec![vec![term("x"), term("y")]]),
                prod("other", vec![vec![term("x"), term("y")]]),
            ]),
            ConvertOptions::default(),
        );
        assert!(
            spec.rule.contains_key(name),
            "{name} was dropped; rule keys were {:?}",
            spec.rule.keys().collect::<Vec<_>>()
        );
    }
}

// ---- tail-repeat separator ----------------------------------------

// `list = DIGIT [ "," list ]`, with the comma used NOWHERE else, is the
// isolating case: the separator is stashed off `alts`, so an allocator
// that only walked `alts` gave it no token and the repeat never matched.
fn list_grammar() -> Grammar {
    Grammar::new(vec![
        prod("doc", vec![vec![reference("list")]]),
        prod(
            "list",
            vec![vec![
                rx("[0-9]", ""),
                opt(group(vec![vec![term(","), reference("list")]])),
            ]],
        ),
    ])
}

#[test]
fn tail_repeat_separator_gets_a_token() {
    let spec = emit(list_grammar(), demo().start("doc"));
    let close = spec.rule["list"]
        .as_ref()
        .unwrap()
        .close
        .clone()
        .expect("close alternates");
    let sep = close[0].s().unwrap_or("");
    assert!(!sep.is_empty(), "separator alternate names no token");
    assert!(
        sep.starts_with('#'),
        "separator alternate s = {sep:?}, want a #token name"
    );
    // And it works: a list of three parses as three iterations.
    let out = parse_with(&spec, "1,2,3").expect("parse");
    assert_eq!(out["src"], "1,2,3");
}

// ---- provenance ----------------------------------------------------

// `doc` repeats `item`. The star helper is generated FOR doc, and the
// only rule name embedded in its own generated name is `item`, the rule
// being repeated.
fn repeat_grammar() -> Grammar {
    Grammar::new(vec![
        prod(
            "doc",
            vec![vec![reference("item"), star(reference("item"))]],
        ),
        prod("item", vec![vec![term("a")], vec![term("b")]]),
    ])
}

fn provenance_of(spec: &GrammarSpec) -> Map<String, Value> {
    spec.meta
        .as_ref()
        .and_then(|m| m.get("provenance"))
        .and_then(Value::as_object)
        .cloned()
        .expect("meta.provenance")
}

#[test]
fn provenance_attributes_a_helper_to_its_enclosing_rule() {
    let spec = emit(repeat_grammar(), demo());
    let prov = provenance_of(&spec);
    let star = spec
        .rule
        .keys()
        .find(|n| n.starts_with("_gen") && n.contains("star_item") && !n.contains('$'))
        .expect("a generated star helper named after item");
    assert_eq!(
        prov[star], "doc",
        "{star} belongs to doc, which repeats item"
    );
}

#[test]
fn provenance_attributes_every_generated_rule_and_only_to_authored_ones() {
    let grammar = repeat_grammar();
    let authored: Vec<String> = grammar.productions.iter().map(|p| p.name.clone()).collect();
    let spec = emit(grammar, demo());
    let prov = provenance_of(&spec);
    for name in spec.rule.keys() {
        if authored.contains(name) {
            assert!(
                !prov.contains_key(name),
                "{name} is author-written and must not be listed"
            );
            continue;
        }
        let origin = prov
            .get(name)
            .unwrap_or_else(|| panic!("generated rule {name} has no provenance"));
        assert!(
            authored.contains(&origin.as_str().unwrap().to_string()),
            "{name} resolves to {origin}, which the author never wrote"
        );
    }
    // No phantoms: an entry naming a rule that was never emitted.
    for name in prov.keys() {
        assert!(
            spec.rule.contains_key(name),
            "{name} has provenance but was not emitted"
        );
    }
}

#[test]
fn provenance_names_the_start_wrapper_after_the_start_rule() {
    let spec = emit(repeat_grammar(), demo());
    assert_eq!(provenance_of(&spec)["__start__"], "doc");
}

#[test]
fn provenance_survives_compilation() {
    let spec = emit(repeat_grammar(), demo().start("doc").builtins(true));
    for (name, out) in [
        ("to_pure_spec", to_pure_spec(&spec).unwrap()),
        ("to_recognition_spec", to_recognition_spec(&spec).unwrap()),
    ] {
        assert_eq!(
            out["meta"]["provenance"]["__start__"], "doc",
            "{name} dropped meta.provenance"
        );
    }
    // ...and through serialisation, which is how it reaches a tool.
    let round: Value = serde_json::from_str(&strict(&spec)).expect("valid JSON");
    assert_eq!(round["meta"]["provenance"]["__start__"], "doc");
}

#[test]
fn provenance_can_be_turned_off() {
    let spec = emit(repeat_grammar(), demo().start("doc").provenance(false));
    assert!(spec.meta.is_none(), "expected no meta with provenance off");
}

// ---- recovery sync tags --------------------------------------------

#[test]
fn sync_tags_on_the_only_two_token_naming_close_alts() {
    let spec = emit(list_grammar(), demo().start("doc"));
    let close_alt = |rule: &str, idx: usize| -> AltSpec {
        spec.rule[rule].as_ref().unwrap().close.as_ref().unwrap()[idx].clone()
    };
    let end = close_alt("__start__", 0);
    assert_eq!(end.s(), Some("#ZZ"));
    assert_eq!(end.g(), Some("demo,end"), "the tag with the end group");

    let sep = close_alt("list", 0);
    assert!(sep.s().is_some_and(|s| s.starts_with('#')));
    assert_eq!(sep.g(), Some("demo,comma"), "the tag with the comma group");

    // Every OTHER close alternate names no token, and the tag it carries
    // must stay the caller's own.
    for (name, rs) in &spec.rule {
        let Some(rs) = rs else { continue };
        for (i, a) in rs.close.iter().flatten().enumerate() {
            if name == "__start__" || (name == "list" && i == 0) {
                continue;
            }
            assert!(
                a.s().is_none(),
                "{name} close alt {i} names token {:?}",
                a.s()
            );
            assert_eq!(
                a.g(),
                Some("demo"),
                "{name} close alt {i} wants the bare tag"
            );
        }
    }
}

// The sync tags above have to EARN their place: strip them back off and
// recovery must get measurably worse, or they are decoration and the
// assertions on them prove nothing. Mirrors
// ts/test/bnf.test.js, "keeps the rest of a list when embedded in a
// tagged host grammar".
//
// The host rule stands in for the grammar this one gets embedded in. Its
// single tag is what disables the structural fallback the generated
// rules would otherwise have relied on, so the emitted sync tags are the
// only resynchronisation left.
const SYNC_GROUPS: [&str; 3] = ["close", "comma", "end"];

/// The list grammar as pure data, which is the shape a host embeds.
/// `builtins` is what makes it function-free; marks are off, so there is
/// nothing for `to_pure_spec` to strip on top of that.
fn list_spec() -> GrammarSpec {
    emit(list_grammar(), demo().start("doc").builtins(true))
}

/// Wrap a spec in a host rule carrying its own tag, and start there.
fn under_host(spec: &GrammarSpec) -> GrammarSpec {
    let mut spec = spec.clone();
    let mut open = AltSpec::new();
    open.set("p", "doc").set("g", "host");
    let mut close = AltSpec::new();
    close
        .set("s", "#ZZ")
        .set("a", "@bubble$")
        .set("g", "host,end");
    spec.rule.insert(
        "host".to_string(),
        Some(RuleSpec {
            open: vec![open],
            close: Some(vec![close]),
        }),
    );
    let rule = spec
        .options
        .entry("rule")
        .or_insert_with(|| json!({}))
        .as_object_mut()
        .expect("options.rule is an object");
    rule.insert("start".into(), json!("host"));
    spec
}

/// The same spec with every sync tag taken back off.
fn without_sync_tags(spec: &GrammarSpec) -> GrammarSpec {
    let mut spec = spec.clone();
    for rule in spec.rule.values_mut().flatten() {
        for alt in rule.close.iter_mut().flatten() {
            let Some(tags) = alt.g() else { continue };
            let kept: Vec<&str> = tags
                .split(',')
                .filter(|t| !SYNC_GROUPS.contains(t))
                .collect();
            alt.set("g", kept.join(","));
        }
    }
    spec
}

/// Parse with recovery on, returning the error count and the recovered
/// source text.
fn recover(spec: &GrammarSpec, src: &str) -> (usize, String) {
    let mut spec = spec.clone();
    spec.options
        .insert("parse".into(), json!({ "recover": { "enabled": true } }));
    let mut parser = tabnas::Tabnas::new();
    spec.install(&mut parser).expect("install");
    let out = parser.parse_recover(src);
    let text = out
        .value
        .as_ref()
        .map(|v| src_of(&v.to_json()))
        .unwrap_or_default();
    (out.errors.len(), text)
}

/// The `src` of the first node that carries one, walking kids.
fn src_of(node: &Value) -> String {
    match node {
        Value::Object(map) => match map.get("src") {
            Some(Value::String(s)) => s.clone(),
            _ => map.get("kids").map(src_of).unwrap_or_default(),
        },
        Value::Array(items) => items.iter().map(src_of).collect(),
        Value::String(s) => s.clone(),
        _ => String::new(),
    }
}

#[test]
fn sync_tags_keep_the_rest_of_a_list_under_a_tagged_host() {
    let spec = list_spec();
    let (tagged_errors, tagged_src) = recover(&under_host(&spec), "1,!,3");
    let (bare_errors, bare_src) = recover(&under_host(&without_sync_tags(&spec)), "1,!,3");

    assert_eq!(tagged_errors, 1, "tagged: one bad token, one error");
    assert_eq!(bare_errors, 1, "untagged: one bad token, one error");
    assert!(
        tagged_src.contains('3'),
        "tagged: the item after the error should survive, got {tagged_src:?}"
    );
    assert!(
        !bare_src.contains('3'),
        "untagged: without the separator sync point the tail is lost. \
         A pass here means the tags are no longer doing anything, got \
         {bare_src:?}"
    );
}

// A grammar is free to contain a production actually called
// `__start__`; the wrapper then takes a numbered name.
#[test]
fn start_wrapper_avoids_an_authored_name() {
    let spec = emit(
        Grammar::new(vec![
            prod("doc", vec![vec![reference("__start__")]]),
            prod("__start__", vec![vec![tok("#NR")]]),
        ]),
        demo().start("doc"),
    );
    assert!(
        spec.rule.contains_key("__start2__"),
        "rules: {:?}",
        spec.rule.keys().collect::<Vec<_>>()
    );
    let prov = provenance_of(&spec);
    assert!(
        !prov.contains_key("__start__"),
        "the author's __start__ rule is listed as generated"
    );
    assert_eq!(prov["__start2__"], "doc");
}

// A caller may hold arbitrary JSON in `meta`; it travels whole.
#[test]
fn carried_meta_survives_serialisation() {
    let mut spec = emit(
        Grammar::new(vec![prod("v", vec![vec![tok("#NR")]])]),
        demo().builtins(true),
    );
    spec.meta = Some(json!({"pairs": {"k": "v"}, "tags": ["a", "b"]}));
    let text = strict(&spec);
    assert!(!text.contains("null"), "meta serialised as null:\n{text}");
    for want in ["\"pairs\"", "\"tags\"", "\"a\"", "\"b\""] {
        assert!(text.contains(want), "serialised meta lost {want}:\n{text}");
    }
}

// ---- source spans --------------------------------------------------

fn at(s: usize, e: usize, r: usize, c: usize) -> SrcSpan {
    SrcSpan::at(s, e, r, c)
}

#[test]
fn unknown_rule_ref_carries_the_element_span() {
    let sp = at(10, 15, 2, 7);
    let err = emit_grammar_spec(
        &Grammar::new(vec![prod(
            "doc",
            vec![vec![reference("nope").with_span(sp)]],
        )]),
        &demo(),
    )
    .expect_err("an unknown-rule failure");
    assert!(err.message.contains("references unknown rule 'nope'"));
    assert_eq!(err.rule.as_deref(), Some("doc"));
    assert_eq!(err.sp, Some(sp));
}

#[test]
fn unknown_rule_ref_without_spans_still_fails_identically() {
    let err = emit_grammar_spec(
        &Grammar::new(vec![prod("doc", vec![vec![reference("nope")]])]),
        &demo(),
    )
    .expect_err("an unknown-rule failure");
    assert!(err.message.contains("references unknown rule 'nope'"));
    assert_eq!(err.sp, None, "no span recorded, so none reported");
}

#[test]
fn purely_left_recursive_carries_the_production_span() {
    let sp = at(0, 12, 1, 1);
    let err = emit_grammar_spec(
        &Grammar::new(vec![Production {
            sp: Some(sp),
            ..prod(
                "loop",
                vec![
                    vec![reference("loop"), term("x")],
                    vec![reference("loop"), term("y")],
                ],
            )
        }]),
        &demo(),
    )
    .expect_err("a left-recursion failure");
    assert!(err.message.contains("purely left-recursive"));
    assert_eq!(err.rule.as_deref(), Some("loop"));
    assert_eq!(err.sp, Some(sp));
}

// Elements travel by value through the rewrite passes, so a span
// recorded at parse time survives to the emitter.
#[test]
fn element_spans_survive_to_the_emitter() {
    let sp = at(20, 24, 3, 1);
    let el = reference("gone").with_span(sp);
    let g = Grammar::new(vec![
        prod("doc", vec![vec![reference("mid")]]),
        prod("mid", vec![vec![el.clone(), term("x")]]),
    ]);
    let err = emit_grammar_spec(&g, &demo().start("doc")).expect_err("an unknown-rule failure");
    assert_eq!(
        err.sp,
        Some(sp),
        "the span did not survive the rewrite passes"
    );
    assert_eq!(el.sp, Some(sp), "the caller's element is untouched");
}

// Every converted failure message is byte-identical to the canonical
// compiler's: callers have historically matched on the text.
#[test]
fn converted_failure_messages_are_byte_identical() {
    let cases: Vec<(&str, Grammar, &str)> =
        vec![
        (
            "unknown rule reference",
            Grammar::new(vec![prod("doc", vec![vec![reference("nope")]])]),
            "demo: rule 'doc' references unknown rule 'nope'",
        ),
        (
            "purely left-recursive",
            Grammar::new(vec![prod(
                "loop",
                vec![
                    vec![reference("loop"), term("x")],
                    vec![reference("loop"), term("y")],
                ],
            )]),
            "demo: rule 'loop' is purely left-recursive (no seed alternative); cannot eliminate",
        ),
        (
            "prose inside an expression",
            Grammar::new(vec![prod("doc", vec![vec![term("a"), Element::prose("stuff")]])]),
            "demo: rule 'doc' uses prose ('<stuff>') inside an expression; prose may only \
             stand alone as the whole definition of a built-in lexer token.",
        ),
        (
            "prose as a whole definition",
            Grammar::new(vec![prod("doc", vec![vec![Element::prose("stuff")]])]),
            "demo: rule 'doc' is defined only by prose ('<stuff>'), which describes a \
             terminal but does not define one. Prose is allowed only for built-in lexer \
             tokens (TX, NR, ST, VL).",
        ),
    ];
    for (name, grammar, message) in cases {
        let err = emit_grammar_spec(&grammar, &demo()).expect_err(name);
        assert_eq!(err.message, message, "{name}: message drifted");
    }
}

// Spans are metadata for diagnostics and must be invisible in the output.
#[test]
fn spans_do_not_change_the_emitted_grammar() {
    let with_spans = Grammar::new(vec![Production {
        sp: Some(at(0, 9, 1, 1)),
        ..prod("doc", vec![vec![term("a").with_span(at(0, 3, 1, 1))]])
    }]);
    let without = Grammar::new(vec![prod("doc", vec![vec![term("a")]])]);
    let a = strict(&emit(with_spans, demo().builtins(true)));
    let b = strict(&emit(without, demo().builtins(true)));
    assert_eq!(a, b, "spans must be invisible in the emitted grammar");
}

// A grammar the compiler cannot compile is INVALID USER INPUT, and
// invalid user input is an error return.
#[test]
fn emit_failure_is_an_error_not_a_panic() {
    let err = emit_grammar_spec(
        &Grammar::new(vec![prod("A", vec![vec![reference("A"), term("y")]])]),
        &ConvertOptions::tag("bnf"),
    )
    .expect_err("an error for a purely left-recursive rule");
    assert!(err.message.contains("purely left-recursive"));
}

// A removal-only grammar has nothing to start from: an error, not a
// panic.
#[test]
fn emit_removal_only_grammar_errors() {
    let err = emit_grammar_spec(
        &Grammar {
            remove: vec!["gone".into()],
            ..Grammar::new(vec![])
        },
        &ConvertOptions::default(),
    )
    .expect_err("a controlled error");
    assert!(err.message.contains("no productions"), "{err}");
}

// `spec.rule["gone"] = None` is how a spec says "gone is removed", and
// a removed rule has no alternates to mark.
#[test]
fn mark_listing_skips_a_removed_rule() {
    let spec = emit(
        Grammar {
            remove: vec!["gone".into()],
            ..Grammar::new(vec![
                prod("top", vec![vec![term("x")], vec![term("y")]]),
                prod("gone", vec![vec![term("z")]]),
            ])
        },
        demo().marks(true),
    );
    assert_eq!(
        spec.rule.get("gone"),
        Some(&None),
        "the removal survived as the null marker"
    );
    let listing = mark_listing(&spec);
    assert!(
        !listing.contains("gone"),
        "a removed rule must not be listed:\n{listing}"
    );
    assert!(
        listing.contains("top"),
        "the surviving rule's marks are missing:\n{listing}"
    );
}

// A null rule entry is a REMOVAL, so it has to survive serialisation as
// one, through every entry point.
#[test]
fn removal_survives_serialisation() {
    let mut spec = GrammarSpec::default();
    spec.rule.insert("gone".into(), None);
    spec.rule.insert("kept".into(), Some(RuleSpec::default()));
    for (name, out) in [
        ("to_value", spec.to_value()),
        ("to_recognition_spec", to_recognition_spec(&spec).unwrap()),
        ("to_pure_spec", to_pure_spec(&spec).unwrap()),
    ] {
        let rules = out["rule"]
            .as_object()
            .unwrap_or_else(|| panic!("{name}: no rule block"));
        assert_eq!(
            rules.get("gone"),
            Some(&Value::Null),
            "{name}: removal dropped"
        );
        assert!(
            rules.contains_key("kept"),
            "{name}: surviving rule was dropped"
        );
    }
}

// The shared default builders are thread-safe: a spec built on one
// thread installs and parses on another.
#[test]
fn a_spec_installs_on_another_thread() {
    let spec = emit(
        Grammar::new(vec![prod("val", vec![vec![tok("#NR")]])]),
        demo().start("val"),
    );
    let handle = std::thread::spawn(move || parse_with(&spec, "7").expect("parse"));
    assert_eq!(handle.join().unwrap()["src"], "7");
}

// ---- untrusted IR shapes ------------------------------------------

// A grammar is untrusted input, and the passes over an element are
// recursive. Nesting past the budget is an ERROR RETURN: a Rust stack
// that runs out aborts the process, which is not a failure mode a
// compiler may offer a grammar author. TypeScript raises a catchable
// RangeError several hundred levels later instead; DIVERGENCE.md
// records that difference.
#[test]
fn nesting_past_the_budget_is_refused_not_a_stack_overflow() {
    let mut el = term("a");
    for _ in 0..200 {
        el = opt(el);
    }
    let err = emit_grammar_spec(&Grammar::new(vec![prod("doc", vec![vec![el]])]), &demo())
        .expect_err("deeply nested elements are refused");
    assert!(
        err.message.contains("nests elements more than 128 deep"),
        "message was: {}",
        err.message
    );
    assert_eq!(err.rule.as_deref(), Some("doc"));

    // The same for a group, which nests through its alternatives.
    let mut el = term("a");
    for _ in 0..200 {
        el = group(vec![vec![el]]);
    }
    assert!(emit_grammar_spec(&Grammar::new(vec![prod("doc", vec![vec![el]])]), &demo()).is_err());

    // Just inside the budget still compiles.
    let mut el = term("a");
    for _ in 0..100 {
        el = opt(el);
    }
    assert!(emit_grammar_spec(&Grammar::new(vec![prod("doc", vec![vec![el]])]), &demo()).is_ok());
}

// The reference graph is as deep as the grammar author made it, so the
// walks over it (Tarjan's components, Paull's ordering) carry their own
// stack. A chain of a few thousand rules overflowed a recursive walk.
#[test]
fn a_long_reference_chain_compiles() {
    const N: usize = 3000;
    let mut prods: Vec<Production> = (0..N)
        .map(|i| {
            prod(
                &format!("r{i}"),
                vec![vec![reference(&format!("r{}", i + 1))]],
            )
        })
        .collect();
    prods.push(prod(&format!("r{N}"), vec![vec![term("a")]]));
    let spec = emit_grammar_spec(&Grammar::new(prods), &demo()).expect("a long chain compiles");
    assert!(spec.rule.contains_key("r0"));
}
