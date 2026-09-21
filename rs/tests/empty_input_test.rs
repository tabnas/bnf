// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

// Whether the empty input is in the language. Mirrors
// ts/test/empty-input.test.js and go/empty_input_test.go: the engine
// short-circuits "" before the parse loop starts, so no rule ever sees
// it and `lex.empty` alone decides. A grammar's nullability is a
// property of the IR rather than of any notation, which is why it is
// answered here and not in a front-end.

mod common;

use common::{emit_ir, opt, plus, prod, reference, rx, star, term, tok};
use tabnas_bnf::{ConvertOptions, Element, Production};

fn empty_of(prods: Vec<Production>, opts: Option<ConvertOptions>) -> bool {
    let spec = emit_ir(prods, opts);
    spec.options["lex"]["empty"]
        .as_bool()
        .expect("lex.empty was not set at all")
}

fn one_alt(alt: Vec<Element>) -> Vec<Production> {
    vec![prod("S", vec![alt])]
}

#[test]
fn empty_input_from_the_grammar() {
    let cases: Vec<(&str, Vec<Element>, bool)> = vec![
        // A terminal consumes.
        ("a literal", vec![term("a")], false),
        ("a sequence", vec![term("a"), term("b")], false),
        ("1*A", vec![plus(term("a"))], false),
        ("a class", vec![rx("[a-z]", "")], false),
        ("a built-in token", vec![tok("#TX")], false),
        // The engine's own zero-width tokens: neither is reachable from
        // grammar text, but the IR is the shared contract, and calling
        // either consuming rejects a grammar's only string.
        ("#ZZ, end of source", vec![tok("#ZZ")], true),
        ("#AA, the ANY wildcard", vec![tok("#AA")], true),
        ("#SP still consumes", vec![tok("#SP")], false),
        (
            "a zero-width token does not make its sequence empty",
            vec![term("a"), tok("#ZZ")],
            false,
        ),
        (
            "a group of consuming alternatives",
            vec![Element::group(vec![vec![term("a")], vec![term("b")]])],
            false,
        ),
        // These derive empty.
        ("*A", vec![star(term("a"))], true),
        ("[ A ]", vec![opt(term("a"))], true),
        ("0*2A", vec![Element::rep(0, Some(2), term("a"))], true),
        (
            "a group with one empty-deriving branch",
            vec![Element::group(vec![vec![term("a")], vec![opt(term("b"))]])],
            true,
        ),
        // Two kinds can match nothing without looking like it. Reading
        // either as consuming would reject input the grammar admits.
        ("an empty literal", vec![term("")], true),
        (
            "a class that can match nothing",
            vec![rx("[a-z]*", "")],
            true,
        ),
        (
            "an alternation with an empty branch",
            vec![rx("a|", "")],
            true,
        ),
    ];
    for (label, alt, want) in cases {
        let got = empty_of(one_alt(alt), None);
        assert_eq!(got, want, "{label}: lex.empty = {got}, want {want}");
    }
}

// Nullability is a least fixed point over the rules, not a property of
// one production read alone. Each of these needs more than one pass.
#[test]
fn empty_input_follows_other_rules() {
    let through = vec![
        prod("S", vec![vec![reference("A")]]),
        prod("A", vec![vec![reference("B")]]),
        prod("B", vec![vec![star(term("a"))]]),
    ];
    assert!(
        empty_of(through, None),
        "nullability through a chain of rules was missed"
    );

    let consuming = vec![
        prod("S", vec![vec![reference("A")]]),
        prod("A", vec![vec![reference("B")]]),
        prod("B", vec![vec![term("a")]]),
    ];
    assert!(
        !empty_of(consuming, None),
        "a consuming chain was reported as deriving empty"
    );

    // A single pass over the productions in order answers false here.
    let later = vec![
        prod("S", vec![vec![reference("A"), reference("B")]]),
        prod("A", vec![vec![opt(term("x"))]]),
        prod("B", vec![vec![opt(term("y"))]]),
    ];
    assert!(
        empty_of(later, None),
        "rules defined after their use were missed"
    );
}

// Every alternative consumes an `a` before reaching the recursion, so
// the rule that reaches itself stays at the bottom of the fixed point.
#[test]
fn recursion_is_not_nullability() {
    let prods = vec![prod(
        "S",
        vec![vec![term("a"), reference("S")], vec![term("a")]],
    )];
    assert!(
        !empty_of(prods, None),
        "a recursive rule that always consumes was reported as nullable"
    );
}

#[test]
fn empty_input_follows_the_start_rule() {
    let prods = || {
        vec![
            prod("S", vec![vec![term("a")]]),
            prod("T", vec![vec![star(term("b"))]]),
        ]
    };
    assert!(
        !empty_of(prods(), Some(ConvertOptions::tag("t"))),
        "default start S consumes, but lex.empty was true"
    );
    assert!(
        empty_of(prods(), Some(ConvertOptions::tag("t").start("T"))),
        "start T derives empty, but lex.empty was false"
    );
}
