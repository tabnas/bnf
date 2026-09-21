// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

// The options a front-end sets ARE part of the accepted language, so a
// serialized spec that loses them is not a smaller spec, it is a
// different grammar. Mirrors go/options_data_test.go, which pins the
// round trip through the engine's own reader. Here the options block is
// plain data by construction (`GrammarSpec::options` is a JSON map), so
// there is nothing function-valued to refuse; what is pinned is that the
// data a front-end adds survives the reductions, strict serialisation
// and the engine's loader unchanged.

mod common;

use common::{prod, tok};
use serde_json::{json, Map, Value};
use tabnas_bnf::{
    emit_grammar_spec, to_jsonic, to_pure_spec, to_recognition_spec, ConvertOptions, Grammar,
    GrammarSpec, JsonicOptions,
};

// What a scannerless front-end sets: every default matcher off, an empty
// ignore set, negotiated lexing on. Before this was serialized, a
// reloaded GBNF grammar lexed "a+b" as one text token and rejected input
// a natively installed copy accepted.
fn exact_lexing() -> Map<String, Value> {
    json!({
        "tokenSet": { "IGNORE": [] },
        "space": { "lex": false },
        "line": { "lex": false },
        "comment": { "lex": false },
        "string": { "lex": false },
        "number": { "lex": false },
        "text": { "lex": false },
        "value": { "lex": false },
        "lex": { "empty": false, "relex": true },
    })
    .as_object()
    .unwrap()
    .clone()
}

/// A compiled spec with a front-end's options merged in, the way a
/// front-end leaves it.
fn spec_with(options: Map<String, Value>) -> GrammarSpec {
    let mut spec = emit_grammar_spec(
        &Grammar::new(vec![prod("top", vec![vec![tok("#NR")]])]),
        &ConvertOptions::tag("tst").start("top").builtins(true),
    )
    .expect("emit");
    for (k, v) in options {
        spec.options.insert(k, v);
    }
    spec
}

/// Through text and back into a live engine, the way a serialized
/// grammar travels.
fn reload(data: Value) -> tabnas::Options {
    let text = to_jsonic(
        &data,
        JsonicOptions {
            strict: true,
            indent: None,
        },
    );
    let _: Value = serde_json::from_str(&text).expect("emitted options are valid JSON");
    let engine = tabnas::GrammarSpec::from_json(&text).expect("the engine reads the text");
    let mut parser = tabnas::Tabnas::new();
    parser.grammar(&engine).expect("install");
    parser.config()
}

#[test]
fn exact_lexing_survives_serialisation() {
    let spec = spec_with(exact_lexing());
    for (shape, data) in [
        ("to_pure_spec", to_pure_spec(&spec).unwrap()),
        ("to_recognition_spec", to_recognition_spec(&spec).unwrap()),
    ] {
        let got = reload(data);
        for (name, lex) in [
            ("space", got.space.lex),
            ("line", got.line.lex),
            ("comment", got.comment.lex),
            ("string", got.string.lex),
            ("number", got.number.lex),
            ("text", got.text.lex),
            ("value", got.value.lex),
        ] {
            assert!(!lex, "{shape}: {name}.lex did not survive");
        }
        assert!(
            got.lex.relex,
            "{shape}: lex.relex did not survive; alternates cannot re-cut a span"
        );
        assert!(!got.lex.empty, "{shape}: lex.empty did not survive");
        assert_eq!(
            got.token_set.get("IGNORE").map(Vec::len),
            Some(0),
            "{shape}: the empty IGNORE token set did not survive: {:?}",
            got.token_set.keys().collect::<Vec<_>>()
        );
        assert_eq!(
            got.rule.start, "__start__",
            "{shape}: rule.start did not survive"
        );
    }
}

// JSON forbids raw C0 controls in strings. `space.chars` carrying a tab
// is entirely plausible, and has to serialise to text that parses back.
#[test]
fn control_characters_survive_serialisation() {
    for ch in ["\t", "\r", "\n", "\u{1}", "\u{1f}"] {
        let mut options = exact_lexing();
        options.insert("space".into(), json!({ "lex": true, "chars": ch }));
        let spec = spec_with(options);
        let got = reload(to_pure_spec(&spec).unwrap());
        assert_eq!(got.space.chars, ch, "chars={ch:?} came back changed");
    }
}

// Diagnostics are plain data and travel through every shape, as the
// canonical compiler's `cloneData` carries them.
#[test]
fn diagnostic_options_are_carried() {
    let mut options = exact_lexing();
    options.insert("error".into(), json!({ "unexpected": "custom message" }));
    options.insert("hint".into(), json!({ "unexpected": "try a comma" }));
    let spec = spec_with(options);
    for (shape, data) in [
        ("to_pure_spec", to_pure_spec(&spec).unwrap()),
        ("to_recognition_spec", to_recognition_spec(&spec).unwrap()),
    ] {
        let got = reload(data);
        assert_eq!(
            got.error.get("unexpected").map(String::as_str),
            Some("custom message"),
            "{shape}: error templates did not survive"
        );
        assert_eq!(
            got.hint.get("unexpected").map(String::as_str),
            Some("try a comma"),
            "{shape}: hints did not survive"
        );
    }
}

// A spec with no front-end options carries exactly the compiler's own
// block, and nothing invents an empty one.
#[test]
fn compiler_options_alone_are_the_compiler_block() {
    let spec = spec_with(Map::new());
    let keys: Vec<&String> = spec.options.keys().collect();
    assert_eq!(keys, vec!["fixed", "rule", "lex"]);
}
