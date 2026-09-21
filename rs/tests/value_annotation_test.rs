// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

// Value annotations, driven through the IR rather than a notation.
// Mirrors ts/test/value-annotation.test.js and go/value_annotation_test.go.
//
// A front-end says WHAT a rule builds; nothing here knows how the
// notation spelled it. ABNF carries it in a trailing comment, but this
// compiler must not know that, so these drive `Production.value`
// directly, which is also the only way to reach the cases no front-end
// has syntax for yet.

mod common;

use common::{group, opt, plus, prod, reference, rx, star, term, tok};
use serde_json::{json, Value};
use tabnas_bnf::{
    attach_action_slots, emit_grammar_spec, to_recognition_spec, AltSpec, ConvertOptions, Element,
    Grammar, GrammarSpec, Production, ValueAnnotation,
};

// `1*DIGIT`, not a bare `[0-9]+` terminal. The difference decides
// whether a LEADING member survives: left-recursion elimination folds a
// leading reference into this rule, and a rule whose whole body is one
// terminal becomes literals here, which stops it being a member at all.
fn digits() -> Element {
    plus(rx("[0-9]", ""))
}

fn letters() -> Element {
    plus(rx("[a-z]", ""))
}

fn object(members: &[&str]) -> Option<ValueAnnotation> {
    Some(ValueAnnotation::object(members.iter().copied()))
}

fn array() -> Option<ValueAnnotation> {
    Some(ValueAnnotation::array())
}

fn annotated(name: &str, value: Option<ValueAnnotation>, alts: Vec<Vec<Element>>) -> Production {
    Production {
        value,
        ..prod(name, alts)
    }
}

fn emit_value(prods: Vec<Production>, start: &str) -> GrammarSpec {
    match emit_grammar_spec(
        &Grammar::new(prods),
        &ConvertOptions::tag("tst").start(start).builtins(true),
    ) {
        Ok(spec) => spec,
        Err(e) => panic!("emit: {e}"),
    }
}

fn emit_err(prods: Vec<Production>, start: &str) -> tabnas_bnf::EmitError {
    match emit_grammar_spec(
        &Grammar::new(prods),
        &ConvertOptions::tag("tst").start(start).builtins(true),
    ) {
        Ok(_) => panic!("expected a refusal"),
        Err(e) => e,
    }
}

fn build_value(prods: Vec<Production>, start: &str, src: &str) -> Value {
    let spec = emit_value(prods, start);
    let mut parser = tabnas::Tabnas::new();
    spec.install(&mut parser).expect("install");
    match parser.parse(src) {
        Ok(v) => v.to_json(),
        Err(e) => panic!("parse {src:?}: {e}"),
    }
}

/// A rule's emitted alts rendered as JSON, so a test can assert which
/// ACTIONS survived without depending on the alt shape.
fn alt_actions_of(spec: &GrammarSpec, rule: &str) -> String {
    let rs = spec.rule[rule].as_ref().expect("a rule");
    rs.to_value().to_string()
}

// `top = a "." b "." c` with the parts named. The FIRST member is the one
// that matters: a production's leading reference is inlined by the
// left-recursion pass, so `top` pushes a generated helper rather than
// `a`. The annotation naming its own parts is what survives that.
fn triple_prods() -> Vec<Production> {
    vec![
        annotated(
            "top",
            object(&["a", "b", "c"]),
            vec![vec![
                reference("a"),
                term("."),
                reference("b"),
                term("."),
                reference("c"),
            ]],
        ),
        prod("a", vec![vec![digits()]]),
        prod("b", vec![vec![digits()]]),
        prod("c", vec![vec![digits()]]),
    ]
}

#[test]
fn builds_an_object() {
    assert_eq!(
        build_value(triple_prods(), "top", "1.2.30"),
        json!({"a": "1", "b": "2", "c": "30"})
    );
}

// Not a restatement of the test above: it asserts the CAUSE. `top`'s
// first segment pushes a generated helper, not `a`, so the key can only
// have come from the annotation, never from the chain.
#[test]
fn names_the_inlined_first_member() {
    let spec = emit_value(triple_prods(), "top");
    let open = &spec.rule["top"].as_ref().unwrap().open;
    assert!(!open.is_empty());
    assert_ne!(
        open[0].p(),
        Some("a"),
        "this test is pointless if the leading ref survives"
    );
    assert_eq!(
        open[0]
            .k()
            .and_then(|k| k.get("key$"))
            .and_then(|k| k.get("lit")),
        Some(&json!("a")),
        "first member key: got {:?}",
        open[0].k()
    );
}

// `inner` is annotated, so it is assigned WHOLE: omitting `src` is what
// makes nesting work. `name` is not, so it still resolves to its text.
#[test]
fn nests_an_annotated_member() {
    let prods = || {
        vec![
            annotated(
                "top",
                object(&["name", "inner"]),
                vec![vec![reference("name"), term("="), reference("inner")]],
            ),
            prod("name", vec![vec![letters()]]),
            annotated(
                "inner",
                object(&["maj", "min"]),
                vec![vec![reference("maj"), term("."), reference("min")]],
            ),
            prod("maj", vec![vec![digits()]]),
            prod("min", vec![vec![digits()]]),
        ]
    };
    assert_eq!(
        build_value(prods(), "top", "ab=1.2"),
        json!({"name": "ab", "inner": {"maj": "1", "min": "2"}})
    );
    // The nested member's close carries NO src; a scalar one does.
    let spec = emit_value(prods(), "top");
    let closes = spec.rule["top$step1"]
        .as_ref()
        .unwrap()
        .close
        .clone()
        .expect("a close on top$step1");
    assert!(
        closes[0].k().is_none_or(|k| !k.contains_key("setval$")),
        "a nested member must be assigned whole, not flattened to text"
    );
}

#[test]
fn builds_an_array() {
    let prods = vec![
        annotated(
            "top",
            array(),
            vec![vec![reference("a"), term(","), reference("b")]],
        ),
        prod("a", vec![vec![digits()]]),
        prod("b", vec![vec![digits()]]),
    ];
    assert_eq!(build_value(prods, "top", "1,2"), json!(["1", "2"]));
}

// The tree builders and the value builders cannot both run: both own
// the rule's node.
#[test]
fn drops_the_tree_builders() {
    let spec = emit_value(triple_prods(), "top");
    let top = alt_actions_of(&spec, "top");
    for bad in ["@node$", "@capture$"] {
        assert!(
            !top.contains(bad),
            "{bad} must not survive on a value rule: {top}"
        );
    }
    // ...but the MEMBERS keep theirs: src reads the node.src they build.
    let member = alt_actions_of(&spec, "b");
    assert!(
        member.contains("@node$"),
        "a member still needs its tree builders to accumulate src: {member}"
    );
}

#[test]
fn refuses_the_wrong_member_count() {
    let prods = vec![
        annotated(
            "top",
            object(&["a", "b"]),
            vec![vec![reference("a"), term("x")]],
        ),
        prod("a", vec![vec![digits()]]),
    ];
    let err = emit_err(prods, "top");
    assert!(
        err.message
            .contains("names 2 members but has 1 part that produces a value"),
        "{err}"
    );
}

// `a = x ":"` is folded into `top` by left-recursion elimination, so the
// member would capture `x` and silently drop the `":"` that belonged to
// `a`.
#[test]
fn refuses_a_lost_member_shape() {
    let prods = vec![
        annotated(
            "top",
            object(&["a", "b"]),
            vec![vec![reference("a"), term(","), reference("b")]],
        ),
        prod("a", vec![vec![reference("x"), term(":")]]),
        prod("x", vec![vec![digits()]]),
        prod("b", vec![vec![digits()]]),
    ];
    let err = emit_err(prods, "top");
    assert!(err.message.contains("folded into this rule"), "{err}");
}

// Kind is an unrestricted string in the IR, so anything can reach here;
// an unknown kind must not fall through as an object.
#[test]
fn refuses_an_unknown_kind() {
    let prods = vec![
        annotated(
            "top",
            Some(ValueAnnotation {
                kind: "arry".into(),
                members: vec![],
            }),
            vec![vec![reference("a")]],
        ),
        prod("a", vec![vec![digits()]]),
    ];
    let err = emit_err(prods, "top");
    assert!(err.message.contains("unknown kind 'arry'"), "{err}");
}

// `top = child` takes the all-simple shortcut, which used to return
// before the annotation was ever consulted.
#[test]
fn builds_on_a_single_reference() {
    let prods = vec![
        annotated("top", object(&["child"]), vec![vec![reference("child")]]),
        prod("child", vec![vec![digits()]]),
    ];
    assert_eq!(build_value(prods, "top", "7"), json!({"child": "7"}));
}

// Nesting is decided by POSITION, not by a member name: an array names
// nothing, so a name-based rule could never nest one.
#[test]
fn nests_an_array_element() {
    let prods = vec![
        annotated(
            "top",
            array(),
            vec![vec![reference("plain"), term(","), reference("one")]],
        ),
        prod("plain", vec![vec![digits()]]),
        annotated(
            "one",
            object(&["p", "q"]),
            vec![vec![reference("p"), term(":"), reference("q")]],
        ),
        prod("p", vec![vec![digits()]]),
        prod("q", vec![vec![digits()]]),
    ];
    assert_eq!(
        build_value(prods, "top", "9,1:2"),
        json!(["9", {"p": "1", "q": "2"}])
    );
}

// Recognition mode drops the output-building actions. The value builders
// belong in that set for the same reason the tree builders do.
#[test]
fn value_builders_are_stripped_from_the_recognition_spec() {
    let spec = emit_value(triple_prods(), "top");
    let rec = to_recognition_spec(&spec).expect("recognition").to_string();
    for act in ["@object$", "@key$", "@setval$", "@push$", "@array$"] {
        assert!(
            !rec.contains(act),
            "{act} must not survive recognition mode"
        );
    }
}

#[test]
fn refuses_alternatives() {
    // Distinct leading refs, so left factoring cannot merge these into
    // one alternative behind the guard's back.
    let prods = vec![
        annotated(
            "top",
            object(&["a"]),
            vec![
                vec![reference("a"), term("x")],
                vec![reference("b"), term("y")],
            ],
        ),
        prod("a", vec![vec![digits()]]),
        prod("b", vec![vec![letters()]]),
    ];
    let err = emit_err(prods, "top");
    assert!(err.message.contains("alternatives"), "{err}");
}

// ---- The plan and the emitter must count the SAME parts -------------

// The plan counted only `ref` elements, so a leading group was not a
// member to it, and `inner`'s nesting flag landed on the GROUP.
#[test]
fn nests_by_position_past_a_group() {
    let prods = vec![
        annotated(
            "top",
            array(),
            vec![vec![
                group(vec![vec![letters()]]),
                term(","),
                reference("inner"),
            ]],
        ),
        annotated("inner", object(&["y"]), vec![vec![reference("y")]]),
        prod("y", vec![vec![digits()]]),
    ];
    assert_eq!(build_value(prods, "top", "ab,3"), json!(["ab", {"y": "3"}]));
}

#[test]
fn counts_a_group_as_a_member() {
    let prods = vec![
        annotated(
            "top",
            object(&["inner"]),
            vec![vec![
                group(vec![vec![letters()]]),
                term(","),
                reference("inner"),
            ]],
        ),
        prod("inner", vec![vec![digits()]]),
    ];
    let err = emit_err(prods, "top");
    assert!(
        err.message
            .contains("names 1 member but has 2 parts that produce a value"),
        "{err}"
    );
}

// Lifting an ANNOTATED single-literal rule to a token discarded its
// builders with no diagnostic, and shifted its caller's flags.
#[test]
fn survives_the_literal_lift() {
    let prods = vec![
        annotated(
            "top",
            array(),
            vec![vec![reference("n"), reference("s"), reference("m")]],
        ),
        prod("n", vec![vec![digits()]]),
        annotated("s", object(&[]), vec![vec![term("+")]]),
        prod("m", vec![vec![digits()]]),
    ];
    assert_eq!(build_value(prods, "top", "12+34"), json!(["12", {}, "34"]));
}

// A member whose own rule is one literal becomes a lexer token, so it
// stops pushing, and every later flag then sits one place too early.
#[test]
fn refuses_a_changed_part_count() {
    let prods = vec![
        annotated(
            "top",
            array(),
            vec![vec![reference("n"), reference("s"), reference("m")]],
        ),
        prod("n", vec![vec![digits()]]),
        prod("s", vec![vec![term("+")]]),
        prod("m", vec![vec![digits()]]),
    ];
    let err = emit_err(prods, "top");
    assert!(
        err.message.contains("annotation for 3 parts but builds 2"),
        "{err}"
    );
}

// ---- The leading fold, followed the whole way ----------------------

#[test]
fn follows_an_alias_chain() {
    let prods = vec![
        annotated(
            "top",
            object(&["a", "c"]),
            vec![vec![reference("a"), term(","), reference("c")]],
        ),
        prod("a", vec![vec![reference("b")]]),
        prod("b", vec![vec![digits(), term(":")]]),
        prod("c", vec![vec![digits()]]),
    ];
    let err = emit_err(prods, "top");
    assert!(err.message.contains("not a single part"), "{err}");
}

#[test]
fn refuses_an_annotated_leading_member() {
    let prods = vec![
        annotated(
            "top",
            object(&["a", "c"]),
            vec![vec![reference("a"), term(","), reference("c")]],
        ),
        annotated("a", object(&["x"]), vec![vec![reference("x")]]),
        prod("x", vec![vec![digits()]]),
        prod("c", vec![vec![digits()]]),
    ];
    let err = emit_err(prods, "top");
    assert!(
        err.message
            .contains("erases the value 'a' is annotated to build"),
        "{err}"
    );
}

// The escape hatch the diagnostic above offers has to actually work.
#[test]
fn nests_a_guarded_leading_member() {
    let prods = vec![
        annotated(
            "top",
            object(&["a", "c"]),
            vec![vec![term("v"), reference("a"), term(","), reference("c")]],
        ),
        annotated("a", object(&["x"]), vec![vec![reference("x")]]),
        prod("x", vec![vec![digits()]]),
        prod("c", vec![vec![digits()]]),
    ];
    assert_eq!(
        build_value(prods, "top", "v1,2"),
        json!({"a": {"x": "1"}, "c": "2"})
    );
}

#[test]
fn terminates_on_an_alias_cycle() {
    let prods = vec![
        annotated("top", object(&["a"]), vec![vec![reference("a")]]),
        prod("a", vec![vec![reference("b")]]),
        prod("b", vec![vec![reference("a")]]),
    ];
    let err = emit_err(prods, "top");
    assert!(err.message.contains("not a single part"), "{err}");
}

// ---- `members` is data, not a type promise -------------------------

#[test]
fn refuses_an_empty_member_name() {
    let prods = vec![
        annotated(
            "top",
            object(&["", "b"]),
            vec![vec![reference("a"), reference("b")]],
        ),
        prod("a", vec![vec![letters()]]),
        prod("b", vec![vec![digits()]]),
    ];
    let err = emit_err(prods, "top");
    assert!(err.message.contains("is not a name"), "{err}");
}

#[test]
fn refuses_a_named_array() {
    let prods = vec![
        annotated(
            "top",
            Some(ValueAnnotation {
                kind: "array".into(),
                members: vec!["a".into()],
            }),
            vec![vec![reference("a")]],
        ),
        prod("a", vec![vec![digits()]]),
    ];
    let err = emit_err(prods, "top");
    assert!(
        err.message.contains("positional and are not named"),
        "{err}"
    );
}

// ---- Diagnostics ---------------------------------------------------

#[test]
fn diagnostics_name_the_notation() {
    for tag in ["gbnf", "ebnf"] {
        let prods = vec![annotated(
            "top",
            Some(ValueAnnotation {
                kind: "nope".into(),
                members: vec![],
            }),
            vec![vec![digits()]],
        )];
        let err = emit_grammar_spec(
            &Grammar::new(prods),
            &ConvertOptions::tag(tag).start("top").builtins(true),
        )
        .expect_err("a refusal");
        assert!(
            err.message.starts_with(&format!("{tag}: ")),
            "tag {tag}: got {err}"
        );
    }
}

#[test]
fn the_diagnostic_carries_the_span() {
    let prods = vec![Production {
        sp: Some(tabnas_bnf::SrcSpan::new(12, 20)),
        ..annotated(
            "top",
            Some(ValueAnnotation {
                kind: "nope".into(),
                members: vec![],
            }),
            vec![vec![digits()]],
        )
    }];
    let err = emit_err(prods, "top");
    assert_eq!(err.rule.as_deref(), Some("top"));
    let sp = err.sp.expect("a span");
    assert_eq!((sp.s, sp.e), (12, 20));
}

// Two conversions at once must not read each other's plan.
#[test]
fn concurrent_emits_do_not_share_a_plan() {
    let nested = || {
        vec![
            annotated(
                "top",
                array(),
                vec![vec![term("<"), reference("one"), term(">")]],
            ),
            annotated("one", object(&["p"]), vec![vec![reference("p")]]),
            prod("p", vec![vec![digits()]]),
        ]
    };
    let flat = || {
        vec![
            annotated(
                "top",
                array(),
                vec![vec![term("<"), reference("one"), term(">")]],
            ),
            prod("one", vec![vec![digits()]]),
        ]
    };
    let errors = std::sync::Mutex::new(Vec::<String>::new());
    std::thread::scope(|scope| {
        for _ in 0..25 {
            for (prods, want) in [(nested(), json!([{"p": "4"}])), (flat(), json!(["4"]))] {
                let errors = &errors;
                scope.spawn(move || {
                    let spec = match emit_grammar_spec(
                        &Grammar::new(prods),
                        &ConvertOptions::tag("tst").start("top").builtins(true),
                    ) {
                        Ok(s) => s,
                        Err(e) => {
                            errors.lock().unwrap().push(format!("emit: {e}"));
                            return;
                        }
                    };
                    let mut parser = tabnas::Tabnas::new();
                    if let Err(e) = spec.install(&mut parser) {
                        errors.lock().unwrap().push(format!("install: {e}"));
                        return;
                    }
                    match parser.parse("<4>") {
                        Ok(v) => {
                            if v.to_json() != want {
                                errors
                                    .lock()
                                    .unwrap()
                                    .push(format!("got {}, want {want}", v.to_json()));
                            }
                        }
                        Err(e) => errors.lock().unwrap().push(format!("parse: {e}")),
                    }
                });
            }
        }
    });
    let errors = errors.into_inner().unwrap();
    assert!(errors.is_empty(), "{}", errors.join("\n"));
}

// Two conversions running at once must each report their OWN notation.
#[test]
fn concurrent_emits_keep_their_own_diag_prefix() {
    let bad = || {
        vec![annotated(
            "top",
            Some(ValueAnnotation {
                kind: "nope".into(),
                members: vec![],
            }),
            vec![vec![digits()]],
        )]
    };
    let errors = std::sync::Mutex::new(Vec::<String>::new());
    std::thread::scope(|scope| {
        for _ in 0..50 {
            for tag in ["gbnf", "ebnf"] {
                let errors = &errors;
                let prods = bad();
                scope.spawn(move || {
                    match emit_grammar_spec(
                        &Grammar::new(prods),
                        &ConvertOptions::tag(tag).start("top").builtins(true),
                    ) {
                        Ok(_) => errors
                            .lock()
                            .unwrap()
                            .push(format!("expected a refusal for tag {tag}")),
                        Err(e) => {
                            if !e.message.starts_with(&format!("{tag}: ")) {
                                errors
                                    .lock()
                                    .unwrap()
                                    .push(format!("tag {tag} got {:?}", e.message));
                            }
                        }
                    }
                });
            }
        }
    });
    let errors = errors.into_inner().unwrap();
    assert!(errors.is_empty(), "{}", errors.join("\n"));
}

// An UNANNOTATED caller erases an annotated rule just as thoroughly.
#[test]
fn refuses_an_unannotated_leading_caller() {
    let prods = vec![
        prod("top", vec![vec![reference("leaf"), term(",")]]),
        annotated("leaf", object(&["d"]), vec![vec![reference("d")]]),
        prod("d", vec![vec![digits()]]),
    ];
    let err = emit_err(prods, "top");
    assert!(
        err.message
            .contains("erases the value 'leaf' is annotated to build"),
        "{err}"
    );
}

// A pure alias is the one caller Paull's pass does NOT substitute into.
#[test]
fn nests_through_an_annotated_alias() {
    let prods = vec![
        annotated("top", object(&["child"]), vec![vec![reference("child")]]),
        annotated("child", object(&["d"]), vec![vec![reference("d")]]),
        prod("d", vec![vec![digits()]]),
    ];
    assert_eq!(build_value(prods, "top", "7"), json!({"child": {"d": "7"}}));
}

// `add = [0-9]+ [ "+" add ]` is the tail-repeat shape. The refusal comes
// from the planner: the repeated part references `add`, which builds a
// value, so it cannot be taken as source text.
#[test]
fn self_recursive_part_refusal_carries_the_span() {
    let prods = vec![
        prod("top", vec![vec![reference("add")]]),
        Production {
            sp: Some(tabnas_bnf::SrcSpan::new(5, 25)),
            ..annotated(
                "add",
                array(),
                vec![vec![
                    rx("[0-9]+", ""),
                    opt(group(vec![vec![term("+"), reference("add")]])),
                ]],
            )
        },
    ];
    let err = emit_err(prods, "top");
    assert!(err.message.contains("reaches 'add' itself"), "{err}");
    let sp = err.sp.expect("a span");
    assert_eq!((sp.s, sp.e), (5, 25));
}

// A composed `a` must stay FLAT when a user action or slot is attached.
#[test]
fn a_composed_action_stays_flat() {
    let prods = vec![
        annotated(
            "top",
            object(&["a", "b"]),
            vec![vec![reference("a"), term("."), reference("b")]],
        ),
        prod("a", vec![vec![digits()]]),
        prod("b", vec![vec![digits()]]),
    ];
    let mut spec = emit_grammar_spec(
        &Grammar::new(prods),
        &ConvertOptions::tag("tst")
            .start("top")
            .builtins(true)
            .marks(true),
    )
    .expect("emit");
    let mark = spec.rule["top"].as_ref().unwrap().open[0]
        .m
        .clone()
        .expect("a mark");
    let slot = format!("@top:o:{mark}");
    attach_action_slots(&mut spec, &[slot.as_str()]).expect("attach");
    let open = &spec.rule["top"].as_ref().unwrap().open[0];
    assert_eq!(open.get("a"), Some(&json!(["@object$", "@key$", slot])));
}

// append_action itself must flatten an array, not nest it.
#[test]
fn append_action_flattens_an_array() {
    let mut alt = AltSpec::new();
    alt.set("a", json!(["x", "y"]));
    alt.append_action("z");
    assert_eq!(alt.get("a"), Some(&json!(["x", "y", "z"])));
    let mut scalar = AltSpec::new();
    scalar.set("a", "x");
    scalar.append_action("z");
    assert_eq!(scalar.get("a"), Some(&json!(["x", "z"])));
    let mut none = AltSpec::new();
    none.append_action("z");
    assert_eq!(none.get("a"), Some(&json!("z")));
}

// A rule that builds a value contributes no TEXT to the node above it,
// so a member resolved to source text came out missing that rule's
// match, or empty.
#[test]
fn refuses_a_src_member_reaching_a_value() {
    let inner = || {
        vec![
            annotated("inner", object(&["d"]), vec![vec![reference("d")]]),
            prod("d", vec![vec![digits()]]),
        ]
    };
    let cases = [
        ("a group", group(vec![vec![reference("inner")]])),
        (
            "a group with text",
            group(vec![vec![term("["), reference("inner"), term("]")]]),
        ),
    ];
    for (label, el) in cases {
        let mut prods = vec![annotated(
            "top",
            array(),
            vec![vec![term("<"), el, term(">")]],
        )];
        prods.extend(inner());
        let err = emit_err(prods, "top");
        assert!(
            err.message.contains("builds a value of its own"),
            "{label}: {err}"
        );
    }
}

// A repetition does not take its run as text: it fills the array.
#[test]
fn collects_a_repetition_of_an_annotated_rule() {
    let prods = vec![
        annotated(
            "top",
            array(),
            vec![vec![term("<"), star(reference("inner")), term(">")]],
        ),
        annotated("inner", object(&["d"]), vec![vec![reference("d")]]),
        // ONE digit, not `1*DIGIT`: a greedy member would swallow the
        // whole run into a single iteration.
        prod("d", vec![vec![rx("[0-9]", "")]]),
    ];
    assert_eq!(
        build_value(prods, "top", "<12>"),
        json!([{"d": "1"}, {"d": "2"}])
    );
}

// A repetition of pure terminals has nothing to collect, so it stays its
// matched text.
#[test]
fn keeps_a_terminal_repetition_as_text() {
    let bare = vec![annotated(
        "top",
        array(),
        vec![vec![star(group(vec![vec![term(",")]]))]],
    )];
    assert_eq!(build_value(bare, "top", ",,"), json!([",,"]));

    let lifted = vec![
        annotated("top", array(), vec![vec![star(reference("item"))]]),
        prod("item", vec![vec![term("x")]]),
    ];
    assert_eq!(build_value(lifted, "top", "xxx"), json!(["xxx"]));
}

#[test]
fn refuses_a_value_behind_a_wrapper_in_a_repetition() {
    let prods = vec![
        annotated("top", array(), vec![vec![star(reference("mid"))]]),
        prod("mid", vec![vec![term("["), reference("inner"), term("]")]]),
        annotated("inner", object(&["d"]), vec![vec![reference("d")]]),
        prod("d", vec![vec![digits()]]),
    ];
    let err = emit_err(prods, "top");
    assert!(err.message.contains("builds a value of its own"), "{err}");
}

#[test]
fn refuses_through_a_plain_intermediate_rule() {
    let prods = vec![
        annotated(
            "top",
            array(),
            vec![vec![term("<"), reference("mid"), term(">")]],
        ),
        prod("mid", vec![vec![term("["), reference("inner"), term("]")]]),
        annotated("inner", object(&["d"]), vec![vec![reference("d")]]),
        prod("d", vec![vec![digits()]]),
    ];
    let err = emit_err(prods, "top");
    assert!(err.message.contains("builds a value of its own"), "{err}");
}

// The control the refusal above must not swallow.
#[test]
fn still_takes_an_ordinary_part_as_text() {
    let prods = vec![
        annotated(
            "top",
            array(),
            vec![vec![
                term("<"),
                group(vec![vec![term("["), reference("p"), term("]")]]),
                term(">"),
            ]],
        ),
        prod("p", vec![vec![digits()]]),
    ];
    assert_eq!(build_value(prods, "top", "<[7]>"), json!(["[7]"]));
}

// Prose resolution drops `NR = <number>` outright, so the rule is gone
// before anything could build its value.
#[test]
fn refuses_a_prose_production() {
    let prods = vec![
        prod("top", vec![vec![reference("a"), reference("NR")]]),
        prod("a", vec![vec![digits()]]),
        annotated("NR", object(&[]), vec![vec![Element::prose("number")]]),
    ];
    let err = emit_err(prods, "top");
    assert!(err.message.contains("body is prose"), "{err}");
}

// Not `a: []`: an empty link carries NO `a` key at all.
#[test]
fn emits_no_action_key_for_an_empty_link() {
    let prods = vec![
        annotated(
            "top",
            array(),
            vec![vec![reference("a"), term(","), reference("b")]],
        ),
        prod("a", vec![vec![digits()]]),
        prod("b", vec![vec![digits()]]),
    ];
    let spec = emit_value(prods, "top");
    let step = spec.rule["top$step1"]
        .as_ref()
        .expect("the chain should have a step rule");
    for alt in &step.open {
        assert!(
            !alt.contains("a"),
            "expected no action, got {:?}",
            alt.get("a")
        );
    }
}

// `src` is how the close-phase capture tells a parse-tree node from
// anything else; `rule` and `kids` are not confusable and stay allowed.
#[test]
fn refuses_only_the_src_member_name() {
    let prods = |member: &str| {
        vec![
            prod("top", vec![vec![term("<"), reference("inner"), term(">")]]),
            annotated("inner", object(&[member]), vec![vec![reference("d")]]),
            prod("d", vec![vec![digits()]]),
        ]
    };
    let err = emit_err(prods("src"), "top");
    assert!(err.message.contains("names a member 'src'"), "{err}");

    for name in ["rule", "kids"] {
        assert_eq!(
            build_value(prods(name), "top", "<7>"),
            json!({"rule": "top", "src": "<>", "kids": [{name: "7"}]}),
            "member {name:?}"
        );
    }
}

#[test]
fn refuses_a_repeated_member_name() {
    let prods = vec![
        annotated(
            "top",
            object(&["x", "x"]),
            vec![vec![reference("a"), term("."), reference("b")]],
        ),
        prod("a", vec![vec![digits()]]),
        prod("b", vec![vec![digits()]]),
    ];
    let err = emit_err(prods, "top");
    assert!(err.message.contains("names the member 'x' twice"), "{err}");
}

// `top = [ "a" "!" ] "a"`: the optional prefix shares vocabulary with the
// tail, so both are compiled into ONE dispatch helper.
#[test]
fn refuses_a_probe_dispatched_alternative() {
    let prods = vec![Production {
        sp: Some(tabnas_bnf::SrcSpan::new(1, 9)),
        ..annotated(
            "top",
            array(),
            vec![vec![
                opt(group(vec![vec![term("a"), term("!")]])),
                term("a"),
            ]],
        )
    }];
    let err = emit_err(prods, "top");
    assert!(err.message.contains("one dispatch helper"), "{err}");
    let sp = err.sp.expect("a span");
    assert_eq!((sp.s, sp.e), (1, 9));
}

// Factoring inlines `leaf`, dissolving the rule, and its value vanished.
#[test]
fn refuses_factoring_away_a_value_rule() {
    let prods = vec![
        prod(
            "top",
            vec![
                vec![group(vec![vec![letters(), term("x")]])],
                vec![group(vec![vec![reference("leaf"), term("y")]])],
            ],
        ),
        Production {
            sp: Some(tabnas_bnf::SrcSpan::new(3, 11)),
            ..annotated("leaf", object(&["w"]), vec![vec![letters()]])
        },
    ];
    let err = emit_err(prods, "top");
    assert!(
        err.message.contains("shared prefix of two alternatives"),
        "{err}"
    );
    let sp = err.sp.expect("a span");
    assert_eq!((sp.s, sp.e), (3, 11));
}

// The emit-time count refusals fire AFTER the rewrites, on a reachable
// path, and carry the production's span.
#[test]
fn post_rewrite_count_refusal_carries_the_span() {
    let prods = vec![
        Production {
            sp: Some(tabnas_bnf::SrcSpan::new(5, 25)),
            ..annotated(
                "top",
                object(&["n", "s"]),
                vec![vec![reference("n"), reference("s")]],
            )
        },
        prod("n", vec![vec![digits()]]),
        prod("s", vec![vec![term("+")]]),
    ];
    let err = emit_err(prods, "top");
    assert!(
        err.message.contains("names 2 members but builds 1"),
        "{err}"
    );
    let sp = err.sp.expect("a span");
    assert_eq!((sp.s, sp.e), (5, 25));
}

// `__proto__` stays an ordinary member name: every runtime builds a
// plain map, and reserving it would refuse a name that works.
#[test]
fn allows_proto_as_a_member_name() {
    let prods = vec![
        annotated(
            "top",
            object(&["__proto__", "b"]),
            vec![vec![term("<"), reference("a"), term("."), reference("b")]],
        ),
        annotated("a", object(&["d"]), vec![vec![reference("d")]]),
        prod("d", vec![vec![digits()]]),
        prod("b", vec![vec![digits()]]),
    ];
    assert_eq!(
        build_value(prods, "top", "<1.2"),
        json!({"__proto__": {"d": "1"}, "b": "2"})
    );
}

// The refusal must fire where factoring is COMMITTED, not where it is
// speculated: here the heads differ, so nothing is factored.
#[test]
fn factors_only_when_factoring_happens() {
    let prods = vec![
        prod(
            "top",
            vec![
                vec![group(vec![vec![letters(), term("x")]])],
                vec![group(vec![vec![reference("leaf"), term("y")]])],
            ],
        ),
        annotated("leaf", object(&["w"]), vec![vec![digits()]]),
    ];
    assert_eq!(
        build_value(prods, "top", "12y"),
        json!({"rule": "top", "src": "y", "kids": [{"w": "12"}]})
    );
}

// `[ "a" NR ] "a"`: the probe predicate accepts a token disambiguator as
// readily as a literal one.
#[test]
fn refuses_a_probe_with_a_token_disambiguator() {
    let prods = vec![annotated(
        "top",
        array(),
        vec![vec![
            opt(group(vec![vec![term("a"), tok("#NR")]])),
            term("a"),
        ]],
    )];
    let err = emit_err(prods, "top");
    assert!(err.message.contains("one dispatch helper"), "{err}");
}
