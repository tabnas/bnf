// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

mod common;

use common::{group, prod, reference, sens_term};
use tabnas_bnf::{
    emit_grammar_spec, ConvertOptions, Element, Grammar, SrcSpan, MAX_REPEAT_EXPANSION,
};

fn emit(grammar: &Grammar) -> Result<tabnas_bnf::GrammarSpec, tabnas_bnf::EmitError> {
    emit_grammar_spec(grammar, &ConvertOptions::tag("safe").start("top"))
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
        err.message.contains("rule 'top'")
            && err.message.contains("item can match the empty string"),
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
