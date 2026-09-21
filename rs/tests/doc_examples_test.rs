// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

// The behaviour the crate documentation states, run. Mirrors
// go/doc_examples_test.go: a documented example that no longer holds is
// a defect in the documentation, and the prose gate cannot see it. The
// README's own Rust fences run as doctests through src/lib.rs; these
// cover the claims the README makes in prose.

mod common;

use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Arc;

use common::{parse_with, prod, reference, star, term, tok};
use serde_json::json;
use tabnas_bnf::{
    attach_action_slots, attach_actions, builtin_token, emit_grammar_spec, escape_regexp,
    is_effectively_case_sensitive, is_prose_name, mark_listing, refs_in, term_key, to_jsonic,
    to_pure_spec, to_recognition_spec, ActionFn, ConvertOptions, Grammar, JsonicOptions,
    Production, BUILTIN_TOKENS,
};

// The first compile: one production, one token, the tagged node back.
#[test]
fn first_compile() {
    let grammar = Grammar::new(vec![prod("val", vec![vec![tok("#NR")]])]);
    let spec =
        emit_grammar_spec(&grammar, &ConvertOptions::tag("demo").start("val")).expect("emit");
    let out = parse_with(&spec, "42").expect("parse");
    assert_eq!(out, json!({"rule": "val", "src": "42", "kids": []}));
}

// Nesting: the rule count and the parse the page states.
#[test]
fn nesting() {
    let grammar = Grammar::new(vec![
        prod(
            "list",
            vec![vec![term("("), star(reference("item")), term(")")]],
        ),
        prod("item", vec![vec![tok("#NR")], vec![reference("list")]]),
    ]);
    let spec =
        emit_grammar_spec(&grammar, &ConvertOptions::tag("demo").start("list")).expect("emit");
    assert_eq!(spec.rule.len(), 13, "two productions give thirteen rules");
    let out = parse_with(&spec, "(1 2 (3))").expect("parse");
    assert_eq!(out["rule"], "list");
    assert_eq!(out["src"], "(12(3))");
    assert_eq!(out["kids"].as_array().map(Vec::len), Some(3));
}

// The message, the type, the prefix.
#[test]
fn unknown_rule_error() {
    let err = emit_grammar_spec(
        &Grammar::new(vec![prod("a", vec![vec![reference("missing")]])]),
        &ConvertOptions::tag("demo"),
    )
    .expect_err("a refusal");
    assert_eq!(
        err.to_string(),
        "demo: rule 'a' references unknown rule 'missing'"
    );
    assert_eq!(err.rule.as_deref(), Some("a"));
}

// A purely left-recursive rule RETURNS an error.
#[test]
fn purely_left_recursive_returns() {
    let err = emit_grammar_spec(
        &Grammar::new(vec![prod("a", vec![vec![reference("a"), term("y")]])]),
        &ConvertOptions::tag("demo").start("a"),
    )
    .expect_err("a refusal");
    assert!(err.message.contains("purely left-recursive"), "{err}");
}

// Marks, the listing format, and binding an action to one.
#[test]
fn marks_and_actions() {
    let mut spec = emit_grammar_spec(
        &Grammar::new(vec![prod("op", vec![vec![term("inc")], vec![term("dec")]])]),
        &ConvertOptions::tag("demo").start("op").marks(true),
    )
    .expect("emit");
    assert_eq!(mark_listing(&spec), "op  o:INC  s:#INC\nop  o:DEC  s:#DEC");

    let ran = Arc::new(AtomicUsize::new(0));
    let counter = ran.clone();
    let action: ActionFn = Arc::new(move |_rule, _ctx| {
        counter.fetch_add(1, Ordering::SeqCst);
        Ok(())
    });
    attach_actions(&mut spec, vec![("@op:o:INC".to_string(), vec![action])]).expect("attach");
    parse_with(&spec, "inc").expect("parse");
    assert_eq!(ran.load(Ordering::SeqCst), 1, "the action runs once");
}

// A slot is a declaration, and a spec carrying one the installer has not
// filled in is refused by the engine.
#[test]
fn action_slots() {
    let mut spec = emit_grammar_spec(
        &Grammar::new(vec![prod("op", vec![vec![term("dec")]])]),
        &ConvertOptions::tag("demo").start("op").marks(true),
    )
    .expect("emit");
    attach_action_slots(&mut spec, &["@op:o:DEC"]).expect("slots");
    assert!(
        attach_action_slots(&mut spec, &["@op:bo"]).is_err(),
        "a rule-phase ref is refused as a slot"
    );
    let mut parser = tabnas::Tabnas::new();
    let err = spec
        .install(&mut parser)
        .expect_err("an unfilled slot is refused at install");
    assert!(
        err.to_string().contains("@op:o:DEC"),
        "the refusal names the slot: {err}"
    );
}

// An unmatched action ref is an error, and names the mark.
#[test]
fn unmatched_action_ref() {
    let mut spec = emit_grammar_spec(
        &Grammar::new(vec![prod("op", vec![vec![term("inc")]])]),
        &ConvertOptions::tag("demo").start("op").marks(true),
    )
    .expect("emit");
    let noop: ActionFn = Arc::new(|_r, _c| Ok(()));
    let err = attach_actions(&mut spec, vec![("@op:o:inc".to_string(), vec![noop])])
        .expect_err("a refusal");
    assert_eq!(
        err.to_string(),
        "demo: action ref '@op:o:inc' matches no open alt with mark 'inc' in rule 'op'"
    );
}

// A duplicate mark within a rule is suffixed.
#[test]
fn duplicate_mark() {
    let spec = emit_grammar_spec(
        &Grammar::new(vec![prod(
            "op",
            vec![vec![tok("#NR")], vec![tok("#NR"), term("!")]],
        )]),
        &ConvertOptions::tag("demo").start("op").marks(true),
    )
    .expect("emit");
    assert!(
        mark_listing(&spec).contains("o:NR~2"),
        "want a ~2 suffix, got {:?}",
        mark_listing(&spec)
    );
}

// Pure data needs builtins, and says so when it does not have it.
// Recognition does not, for an ordinary grammar.
#[test]
fn reductions() {
    let prods = || vec![prod("top", vec![vec![tok("#NR")]])];
    let with_builtins = emit_grammar_spec(
        &Grammar::new(prods()),
        &ConvertOptions::tag("demo").start("top").builtins(true),
    )
    .expect("emit");
    to_pure_spec(&with_builtins).expect("pure");
    to_recognition_spec(&with_builtins).expect("recognition");

    let plain = emit_grammar_spec(
        &Grammar::new(prods()),
        &ConvertOptions::tag("demo").start("top"),
    )
    .expect("emit");
    let err = to_pure_spec(&plain).expect_err("closures are refused");
    assert!(err.message.contains("builtins: true"), "{err}");
    to_recognition_spec(&plain).expect("recognition does not need builtins");
}

// The word-boundary guard, on and off.
#[test]
fn word_keywords() {
    let emit = |on: bool| {
        let spec = emit_grammar_spec(
            &Grammar::new(vec![prod("stmt", vec![vec![term("option")]])]),
            &ConvertOptions::tag("demo").start("stmt").word_keywords(on),
        )
        .expect("emit");
        spec.options["match"]["token"]["#OPTION"]
            .as_str()
            .expect("a match token")
            .to_string()
    };
    assert_eq!(emit(false), "@~/^option/i");
    assert_eq!(emit(true), "@~/^option\\b/i");
}

// A removal reaches the spec as a null rule entry.
#[test]
fn removal() {
    let grammar = Grammar {
        remove: vec!["val".into()],
        ..Grammar::new(vec![prod("top", vec![vec![tok("#NR")]])])
    };
    let spec =
        emit_grammar_spec(&grammar, &ConvertOptions::tag("demo").start("top")).expect("emit");
    assert_eq!(spec.rule.get("val"), Some(&None), "a present null entry");
}

// Provenance is on unless turned off.
#[test]
fn provenance() {
    let grammar = || {
        Grammar::new(vec![
            prod(
                "list",
                vec![vec![term("("), star(reference("item")), term(")")]],
            ),
            prod("item", vec![vec![tok("#NR")]]),
        ])
    };
    let on =
        emit_grammar_spec(&grammar(), &ConvertOptions::tag("demo").start("list")).expect("emit");
    let prov = on
        .meta
        .as_ref()
        .and_then(|m| m.get("provenance"))
        .expect("a provenance map");
    assert_eq!(prov["__start__"], "list");

    let small = emit_grammar_spec(
        &grammar(),
        &ConvertOptions::tag("demo").start("list").provenance(false),
    )
    .expect("emit");
    assert!(small.meta.is_none(), "no provenance when turned off");
}

// The pass returns a new grammar and leaves the input alone.
#[test]
fn eliminate_left_recursion_leaves_the_input_alone() {
    let input = Grammar::new(vec![prod(
        "expr",
        vec![
            vec![reference("expr"), term("+"), tok("#NR")],
            vec![tok("#NR")],
        ],
    )]);
    let alts = input.productions[0].alts.len();
    let out = tabnas_bnf::eliminate_left_recursion(&input).expect("eliminate");
    assert_ne!(out, input, "a new grammar");
    assert_eq!(
        alts,
        input.productions[0].alts.len(),
        "the input is unmodified"
    );
}

// The helper table.
#[test]
fn helpers() {
    for (bare, token) in [("NR", "#NR"), ("ST", "#ST"), ("TX", "#TX"), ("VL", "#VL")] {
        assert_eq!(builtin_token(bare), Some(token));
    }
    assert_eq!(
        BUILTIN_TOKENS.len(),
        4,
        "the pages list four builtin tokens"
    );
    assert_eq!(escape_regexp("a.b*c"), r"a\.b\*c");
    assert_eq!(term_key("+", None), "cs:+");
    assert!(is_effectively_case_sensitive("+", None));
    assert_eq!(term_key("if", None), "ci:if");
    assert!(!is_effectively_case_sensitive("if", None));
    assert!(is_prose_name("<remove>"));
    assert!(!is_prose_name("remove"));
    let mut refs = indexmap::IndexSet::new();
    refs_in(&[reference("a"), reference("b")], &mut refs);
    assert_eq!(refs.len(), 2);
}

// The serialisation pair.
#[test]
fn serialise() {
    let spec = emit_grammar_spec(
        &Grammar::new(vec![prod("top", vec![vec![tok("#NR")]])]),
        &ConvertOptions::tag("demo").start("top").builtins(true),
    )
    .expect("emit");
    let data = to_pure_spec(&spec).expect("pure");
    let text = to_jsonic(&data, JsonicOptions::default());
    assert!(!text.is_empty(), "want jsonic text");
    for key in ["rule", "options"] {
        assert!(data.get(key).is_some(), "want a {key:?} key");
    }
    let _: Production = prod("x", vec![]);
}
