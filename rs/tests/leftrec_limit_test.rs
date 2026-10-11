// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

// What one rule may gain from Paull's substitution is bounded
// (`MAX_LEFT_RECURSION_EXPANSION`), and so is what the whole grammar may gain
// in element occurrences (`MAX_LEFT_RECURSION_ELEMENTS`). Each bound refuses
// the grammar before the substitution that would pass it is made. Mirrors
// ts/test/leftrec-limit.test.js; every count below is exact. The refusals of
// the twenty-rule cycle and of the shortest chain the element bound refuses
// are also shared fixtures, tests/oracle/ir-leftrec-limit.json and
// tests/oracle/ir-leftrec-elements.json, graded byte for byte by the oracle
// test.

mod common;

use std::time::{Duration, Instant};

use common::{prod, reference, sens_term};
use serde_json::Value;
use tabnas_bnf::{
    eliminate_left_recursion, emit_grammar_spec, ConvertOptions, EmitError, Grammar, Kind,
    Production, SrcSpan, MAX_LEFT_RECURSION_ELEMENTS, MAX_LEFT_RECURSION_EXPANSION,
};

/// root = r0 ; r(i) = r(i+1) "a(i)" / r(i+1) "b(i)" / "x(i)", wrapping at k.
fn cycle(k: usize) -> Grammar {
    let mut prods = vec![prod("root", vec![vec![reference("r0")]])];
    for i in 0..k {
        let next = format!("r{}", (i + 1) % k);
        let mut p = prod(
            &format!("r{i}"),
            vec![
                vec![reference(&next), sens_term(&format!("a{i}"))],
                vec![reference(&next), sens_term(&format!("b{i}"))],
                vec![sens_term(&format!("x{i}"))],
            ],
        );
        p.sp = Some(SrcSpan::at(10 * i, 10 * i + 9, i + 2, 1));
        prods.push(p);
    }
    Grammar::new(prods)
}

/// A rule of `n` single-literal alternatives.
fn choice(name: &str, n: usize, prefix: &str) -> Production {
    prod(
        name,
        (0..n)
            .map(|i| vec![sens_term(&format!("{prefix}{i}"))])
            .collect(),
    )
}

fn refusal(rule: &str, source: &str, led: usize, has: usize) -> String {
    format!(
        "cyc: rule '{rule}' exceeds the left-recursion expansion limit of 512 alternatives \
         while inlining '{source}', which begins {led} of its alternatives and has {has} of \
         its own. Factor '{source}' out of the alternatives it begins."
    )
}

fn emit(grammar: &Grammar) -> Result<tabnas_bnf::GrammarSpec, EmitError> {
    emit_grammar_spec(grammar, &ConvertOptions::tag("cyc"))
}

#[test]
fn is_512_alternatives() {
    assert_eq!(MAX_LEFT_RECURSION_EXPANSION, 512);
}

#[test]
fn refuses_a_cycle_of_twenty_rules_within_a_second() {
    let start = Instant::now();
    let err = emit(&cycle(20)).expect_err("the cycle compiled");
    let elapsed = start.elapsed();
    // The topological order processes r19 first, so r12 has grown to 511
    // alternatives when r11 inlines it twice: a gain of 1020.
    assert_eq!(err.message, refusal("r11", "r12", 2, 511));
    assert_eq!(err.rule.as_deref(), Some("r11"));
    assert_eq!(err.sp, Some(SrcSpan::at(110, 119, 13, 1)));
    assert!(
        elapsed < Duration::from_secs(1),
        "refused after {elapsed:?}"
    );
}

#[test]
fn refuses_the_cycle_in_the_standalone_pass_as_well() {
    let err = eliminate_left_recursion(&cycle(20)).expect_err("the cycle was eliminated");
    assert!(
        err.message
            .contains("rule 'r11' exceeds the left-recursion expansion limit"),
        "{err}"
    );
}

#[test]
fn admits_the_cycle_while_each_rule_gains_at_most_the_limit() {
    // k = 8: r0 inlines r1 (255 alternatives) twice, a gain of 508.
    let out = eliminate_left_recursion(&cycle(8)).expect("k = 8 is within the limit");
    let r0 = out.productions.iter().find(|p| p.name == "r0").expect("r0");
    assert_eq!(r0.alts.len(), 1);
    let Kind::Group { alts: seeds } = &r0.alts[0][0].kind else {
        panic!("r0 does not open with its seeds: {:?}", r0.alts[0][0]);
    };
    let Kind::Star { inner, .. } = &r0.alts[0][1].kind else {
        panic!("r0 does not close with its tail loop: {:?}", r0.alts[0][1]);
    };
    let Kind::Group { alts: tails } = &inner.kind else {
        panic!("the tail loop is not a group: {inner:?}");
    };
    assert_eq!((seeds.len(), tails.len()), (255, 256));
    // k = 9: r1 has 511 when r0 inlines it twice, a gain of 1020.
    let err = emit(&cycle(9)).expect_err("k = 9 compiled");
    assert_eq!(err.message, refusal("r0", "r1", 2, 511));
}

#[test]
fn counts_the_alternatives_a_substitution_adds_up_to_the_limit_and_no_further() {
    // top inlines s once: s's 513 alternatives replace one, a gain of 512.
    let top = || prod("top", vec![vec![reference("s"), sens_term("x")]]);
    let at = eliminate_left_recursion(&Grammar::new(vec![top(), choice("s", 513, "s")]))
        .expect("a gain of 512 is within the limit");
    assert_eq!(at.productions[0].alts.len(), 513);
    let err = emit(&Grammar::new(vec![top(), choice("s", 514, "s")]))
        .expect_err("a gain of 513 compiled");
    assert_eq!(err.message, refusal("top", "s", 1, 514));
}

#[test]
fn sums_every_substitution_into_one_rule() {
    // 299 from `a`, then 299 from `b`: 598.
    let err = emit(&Grammar::new(vec![
        prod(
            "top",
            vec![
                vec![reference("a"), sens_term("1")],
                vec![reference("b"), sens_term("2")],
            ],
        ),
        choice("a", 300, "a"),
        choice("b", 300, "b"),
    ]))
    .expect_err("a gain of 598 compiled");
    assert_eq!(err.message, refusal("top", "b", 1, 300));
}

#[test]
fn charges_nothing_for_a_rules_own_alternatives_or_a_one_for_one_inlining() {
    // 2000 alternatives of its own and 2000 led by a rule of one: no gain.
    let mut top = choice("top", 2000, "k");
    top.alts
        .extend((0..2000).map(|i| vec![reference("one"), sens_term(&format!("t{i}"))]));
    let out = eliminate_left_recursion(&Grammar::new(vec![
        top,
        prod("one", vec![vec![sens_term("o")]]),
    ]))
    .expect("no gain at all");
    assert_eq!(out.productions[0].alts.len(), 4000);
}

#[test]
fn charges_nothing_for_a_token_class_which_is_inlined_as_its_token() {
    // Inlined member by member, kw's 600 alternatives would add 1198.
    let grammar = || {
        Grammar::new(vec![
            prod(
                "top",
                vec![
                    vec![reference("kw"), sens_term("x")],
                    vec![reference("kw"), sens_term("y")],
                ],
            ),
            choice("kw", 600, "w"),
        ])
    };
    let err = emit(&grammar()).expect_err("the class was inlined member by member");
    assert_eq!(err.message, refusal("top", "kw", 2, 600));
    let spec = emit_grammar_spec(&grammar(), &ConvertOptions::tag("cyc").token_classes(true))
        .expect("a token class gains nothing");
    let members = spec
        .options
        .get("tokenSet")
        .and_then(|sets| sets.get("kw"))
        .and_then(Value::as_array)
        .map(Vec::len);
    assert_eq!(members, Some(600));
}

/// root = r0 ; r(i) = r(i+1) "x(i)" ; r(n-1) = "end"
fn chain(n: usize) -> Grammar {
    let mut prods = vec![prod("root", vec![vec![reference("r0")]])];
    for i in 0..n - 1 {
        prods.push(prod(
            &format!("r{i}"),
            vec![vec![
                reference(&format!("r{}", i + 1)),
                sens_term(&format!("x{i}")),
            ]],
        ));
    }
    prods.push(prod(&format!("r{}", n - 1), vec![vec![sens_term("end")]]));
    Grammar::new(prods)
}

/// top = s "x1" … "x(tail)", for each name in `tops`; s = m single
/// literals. Inlining s into one of them adds m + (m − 1)·tail − 1.
fn fan(tops: &[&str], m: usize, tail: usize) -> Grammar {
    let mut prods: Vec<Production> = tops
        .iter()
        .map(|name| {
            let mut alt = vec![reference("s")];
            alt.extend((0..tail).map(|i| sens_term(&format!("x{i}"))));
            prod(name, vec![alt])
        })
        .collect();
    prods.push(choice("s", m, "s"));
    Grammar::new(prods)
}

fn elements(rule: &str, source: &str, led: usize, gain: usize, added: usize) -> String {
    format!(
        "cyc: rule '{rule}' exceeds the left-recursion element limit of 32768 elements while \
         inlining '{source}', which begins {led} of its alternatives and would add {gain} \
         elements to the {added} already added to the grammar. Factor '{source}' out of the \
         alternatives it begins."
    )
}

#[test]
fn the_element_limit_is_32768_occurrences() {
    assert_eq!(MAX_LEFT_RECURSION_ELEMENTS, 32768);
}

#[test]
fn refuses_a_chain_of_five_thousand_rules_within_a_second() {
    let start = Instant::now();
    let err = emit(&chain(5000)).expect_err("the chain compiled");
    let elapsed = start.elapsed();
    // The last rule is lifted into a token, and r(4998 - g) inlines
    // r(4999 - g) with g + 1 elements, adding g. The substitutions add
    // 1 + 2 + … + 255 = 32640, and the one adding 256 passes the limit.
    assert_eq!(err.message, elements("r4742", "r4743", 1, 256, 32640));
    assert_eq!(err.rule.as_deref(), Some("r4742"));
    assert!(
        elapsed < Duration::from_secs(1),
        "refused after {elapsed:?}"
    );
}

#[test]
fn refuses_the_chain_in_the_standalone_pass_as_well() {
    // Nothing is lifted here, and r4998 inlines "end" one for one, adding
    // nothing. The prefix is whatever the last emit on this thread set.
    let err = eliminate_left_recursion(&chain(5000)).expect_err("the chain was eliminated");
    let want = elements("r4742", "r4743", 1, 256, 32640);
    assert!(err.message.ends_with(&want["cyc".len()..]), "{err}");
}

#[test]
fn admits_the_longest_chain_the_element_limit_allows() {
    // n = 257 adds 1 + 2 + … + 255 = 32640; n = 258 would add 256 more.
    let spec = emit(&chain(257)).expect("n = 257 is within the limit");
    let r0 = spec.rule["r0"].as_ref().expect("r0 is emitted");
    let s = r0.open[0].s().expect("r0 matches tokens");
    assert_eq!(s.split(' ').count(), 257);
    let err = emit(&chain(258)).expect_err("n = 258 compiled");
    assert_eq!(err.message, elements("r0", "r1", 1, 256, 32640));
}

#[test]
fn counts_a_tail_once_per_alternative_of_the_rule_inlined_before_it() {
    // 65 + 64 × 511 − 1 = 32768 fills the limit; a tail of 512 adds 32832.
    let at = eliminate_left_recursion(&fan(&["top"], 65, 511)).expect("a gain of 32768");
    assert_eq!(at.productions[0].alts.len(), 65);
    let err = emit(&fan(&["top"], 65, 512)).expect_err("a gain of 32832 compiled");
    assert_eq!(err.message, elements("top", "s", 1, 32832, 0));
}

#[test]
fn sums_every_substitution_in_the_grammar() {
    // Each rule adds 65 + 64 × 255 − 1 = 16384: two fill the limit.
    eliminate_left_recursion(&fan(&["t1", "t2"], 65, 255)).expect("a gain of 32768");
    let err = emit(&fan(&["t1", "t2", "t3"], 65, 255)).expect_err("a gain of 49152 compiled");
    assert_eq!(err.message, elements("t3", "s", 1, 16384, 32768));
}

#[test]
fn floors_at_zero_what_an_alternative_replaced_by_fewer_elements_adds() {
    // Inlining e, one empty alternative, first takes an element out of each
    // of 100 alternatives. That frees nothing: s still adds 32832.
    let mut grammar = fan(&["top"], 65, 512);
    let lead: Vec<Vec<_>> = (0..100)
        .map(|i| vec![reference("e"), sens_term(&format!("y{i}"))])
        .collect();
    grammar.productions[0].alts.splice(0..0, lead);
    grammar.productions.push(prod("e", vec![vec![]]));
    let err = emit(&grammar).expect_err("the empty alternatives paid for s");
    assert_eq!(err.message, elements("top", "s", 1, 32832, 0));
}

#[test]
fn charges_no_elements_for_a_token_class_which_is_inlined_as_its_token() {
    // Inlined member by member, s would add 32832.
    let err = emit(&fan(&["top"], 65, 512)).expect_err("the class was inlined member by member");
    assert_eq!(err.message, elements("top", "s", 1, 32832, 0));
    let spec = emit_grammar_spec(
        &fan(&["top"], 65, 512),
        &ConvertOptions::tag("cyc").token_classes(true),
    )
    .expect("a token class adds no elements");
    let members = spec
        .options
        .get("tokenSet")
        .and_then(|sets| sets.get("s"))
        .and_then(Value::as_array)
        .map(Vec::len);
    assert_eq!(members, Some(65));
}
