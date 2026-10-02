// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

mod common;

use common::{group, prod, reference, sens_term};
use tabnas_bnf::{
    emit_grammar_spec, ConvertOptions, Element, Grammar, Kind, SrcSpan, MAX_REPEAT_EXPANSION,
};

fn emit(grammar: &Grammar) -> Result<tabnas_bnf::GrammarSpec, tabnas_bnf::EmitError> {
    emit_grammar_spec(grammar, &ConvertOptions::tag("safe").start("top"))
}

#[test]
fn keeps_the_public_element_literal_shape_source_compatible() {
    let element = Element {
        kind: Kind::Token { name: "#TX".into() },
        sp: None,
    };
    assert_eq!(element, Element::token("#TX"));
}

#[test]
fn refuses_an_unbounded_repetition_whose_item_is_nullable() {
    let sp = SrcSpan::at(4, 18, 1, 5);
    let mut top = prod(
        "top",
        vec![vec![Element::star(group(vec![
            vec![sens_term("a"), sens_term("b")],
            vec![],
        ]))]],
    );
    top.sp = Some(sp);
    let err = emit(&Grammar::new(vec![top])).expect_err("nullable star compiled");
    assert_eq!(err.rule.as_deref(), Some("top"));
    assert_eq!(err.sp, Some(sp));
    assert!(
        err.message.contains("rule 'top'") && err.message.contains("without consuming input"),
        "{err}"
    );
}

#[test]
fn finds_nullability_through_a_rule_reference() {
    let err = emit(&Grammar::new(vec![
        prod("top", vec![vec![Element::plus(reference("empty"))]]),
        prod("empty", vec![vec![]]),
    ]))
    .expect_err("nullable plus compiled");
    assert!(err.message.contains("unbounded repetition"), "{err}");
}

#[test]
fn refuses_a_regex_that_can_match_zero_width_only_in_context() {
    let err = emit(&Grammar::new(vec![prod(
        "top",
        vec![vec![Element::star(Element::regex(r"\b", ""))]],
    )]))
    .expect_err("zero-width boundary loop compiled");
    assert!(err.message.contains("unbounded repetition"), "{err}");
}

#[test]
fn allows_a_boundary_regex_that_must_also_consume_input() {
    emit(&Grammar::new(vec![prod(
        "top",
        vec![vec![Element::star(Element::regex(r"\b[a-z]+", ""))]],
    )]))
    .expect("consuming boundary regex");
}

#[test]
fn refuses_an_extended_word_boundary_without_consuming_input() {
    for boundary in [r"\b{start}", r"\b{end}", r"\b{start-half}", r"\b{end-half}"] {
        let err = emit(&Grammar::new(vec![prod(
            "top",
            vec![vec![Element::star(Element::regex(boundary, ""))]],
        )]))
        .expect_err("extended zero-width boundary loop compiled");
        assert!(err.message.contains("unbounded repetition"), "{err}");
    }

    emit(&Grammar::new(vec![prod(
        "top",
        vec![vec![Element::star(Element::regex(r"\b{start}[a-z]+", ""))]],
    )]))
    .expect("consuming extended boundary regex");
}

#[test]
fn refuses_a_quantified_group_containing_only_an_assertion() {
    let err = emit(&Grammar::new(vec![prod(
        "top",
        vec![vec![Element::star(Element::regex(r"(?:\b)+", ""))]],
    )]))
    .expect_err("quantified zero-width group compiled");
    assert!(err.message.contains("unbounded repetition"), "{err}");
}

#[test]
fn validates_the_implicit_loop_produced_by_a_tail_repeat_rewrite() {
    let boundary = || Element::regex(r"\b", "");
    let err = emit(&Grammar::new(vec![
        prod("top", vec![vec![reference("X")]]),
        prod(
            "X",
            vec![vec![
                boundary(),
                Element::opt(group(vec![vec![boundary(), reference("X")]])),
            ]],
        ),
    ]))
    .expect_err("non-consuming tail repeat compiled");
    assert!(err.message.contains("unbounded tail repetition"), "{err}");
}

#[test]
fn validates_the_implicit_loop_produced_by_a_probe_helper() {
    let nullable = || Element::regex("[a-z]*", "");
    let err = emit(&Grammar::new(vec![prod(
        "top",
        vec![vec![
            Element::opt(group(vec![vec![nullable(), sens_term("!")]])),
            nullable(),
        ]],
    )]))
    .expect_err("non-consuming probe helper compiled");
    assert!(
        err.message.contains("probe helper matcher")
            && err.message.contains("without consuming input"),
        "{err}"
    );
}

#[test]
fn attributes_a_copied_left_recursion_loop_to_its_source_rule() {
    let sp = SrcSpan::at(20, 21, 2, 1);
    let mut source = prod(
        "A",
        vec![
            vec![
                Element::opt(sens_term("x")),
                reference("A"),
                Element::opt(sens_term("y")),
            ],
            vec![sens_term("z")],
        ],
    );
    source.sp = Some(sp);
    let err = emit(&Grammar::new(vec![
        prod("top", vec![vec![reference("A")]]),
        source,
    ]))
    .expect_err("nullable copied loop compiled");
    assert_eq!(err.rule.as_deref(), Some("A"));
    assert_eq!(err.sp, Some(sp));
}

#[test]
fn attributes_a_nullable_grouped_tail_to_the_alternative_that_made_it_nullable() {
    let csp = SrcSpan::at(40, 41, 4, 1);
    let mut c = prod(
        "C",
        vec![vec![reference("A"), Element::opt(sens_term("y"))]],
    );
    c.sp = Some(csp);
    let err = emit(&Grammar::new(vec![
        prod("top", vec![vec![reference("A")]]),
        prod(
            "A",
            vec![
                vec![reference("B")],
                vec![reference("C")],
                vec![sens_term("z")],
            ],
        ),
        prod("B", vec![vec![reference("A"), sens_term("x")]]),
        c,
    ]))
    .expect_err("nullable grouped tail compiled");
    assert_eq!(err.rule.as_deref(), Some("C"));
    assert_eq!(err.sp, Some(csp));
}

#[test]
fn rejects_an_inverted_repetition_range() {
    let err = emit(&Grammar::new(vec![prod(
        "top",
        vec![vec![Element::rep(3, Some(2), sens_term("a"))]],
    )]))
    .expect_err("inverted repetition range compiled");
    assert!(err.message.contains("invalid repetition bounds"), "{err}");
}

#[test]
fn refuses_a_numeric_expansion_before_allocating_helpers() {
    assert_eq!(MAX_REPEAT_EXPANSION, 8192);
    let err = emit(&Grammar::new(vec![prod(
        "top",
        vec![vec![Element::rep(1, Some(5000), sens_term("a"))]],
    )]))
    .expect_err("oversized repetition compiled");
    assert!(
        err.message.contains("limit of 8192") && err.message.contains("'1*5000'"),
        "{err}"
    );
}

#[test]
fn counts_the_mandatory_prefix_of_an_unbounded_numeric_repetition() {
    let err = emit(&Grammar::new(vec![prod(
        "top",
        vec![vec![Element::rep(
            MAX_REPEAT_EXPANSION + 1,
            None,
            sens_term("a"),
        )]],
    )]))
    .expect_err("oversized mandatory prefix compiled");
    assert!(err.message.contains("repetition expansion limit"), "{err}");
}

#[test]
fn counts_nested_numeric_expansions_against_the_same_budget() {
    let err = emit(&Grammar::new(vec![prod(
        "top",
        vec![vec![Element::rep(
            0,
            Some(2200),
            Element::rep(0, Some(2200), sens_term("a")),
        )]],
    )]))
    .expect_err("nested expansion compiled");
    assert!(err.message.contains("repetition expansion limit"), "{err}");
}

#[test]
fn refuses_a_huge_factored_repetition_without_overflowing() {
    let huge = Element::rep(
        0,
        Some(usize::MAX),
        group(vec![vec![sens_term("a"), sens_term("b")]]),
    );
    let err = emit(&Grammar::new(vec![prod(
        "top",
        vec![
            vec![huge.clone(), sens_term("c")],
            vec![huge, sens_term("d")],
        ],
    )]))
    .expect_err("huge factored repetition compiled");
    assert!(err.message.contains("repetition expansion limit"), "{err}");
}

#[test]
fn leaves_an_invalid_regex_for_the_specific_terminal_diagnostic() {
    let err = emit(&Grammar::new(vec![prod(
        "top",
        vec![vec![Element::star(group(vec![
            vec![sens_term("a")],
            vec![Element::regex("[z-a]", "")],
        ]))]],
    )]))
    .expect_err("invalid regex compiled");
    assert!(err.message.contains("invalid regular expression"), "{err}");
}

#[test]
fn allows_a_bounded_nullable_item_because_the_bound_terminates() {
    emit(&Grammar::new(vec![prod(
        "top",
        vec![vec![Element::rep(
            0,
            Some(2),
            group(vec![vec![sens_term("a")], vec![]]),
        )]],
    )]))
    .expect("bounded nullable item");
}
