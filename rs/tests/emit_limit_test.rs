// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

// What compiling one grammar may write is bounded (`MAX_EMIT_UNITS`), and so
// is what the enumeration that tells a choice's alternatives apart may hold
// while it runs. Each is charged before the allocation that would pass it,
// so a grammar is refused before the step that would pass the limit is made.
// Mirrors ts/test/emit-limit.test.js; every count below is exact. A probe
// shape's refusal, that of the forty alternatives of up to forty options and
// that of a removal list are also shared fixtures,
// tests/oracle/ir-emit-units-*.json, which the oracle test grades byte for
// byte here and in the other two runtimes. That the memo limit changes no
// output is a unit test in src/emit.rs, since it lifts the limit.

mod common;

use std::time::{Duration, Instant};

use common::{group, opt, prod, reference, rx, sens_term};
use tabnas_bnf::{
    emit_grammar_spec, ConvertOptions, EmitError, Grammar, GrammarSpec, SrcSpan, MAX_EMIT_UNITS,
};

/// top = "k0" t0 / … ; t(i) = ["a" "d"] "a" v ; v = "w0" / … / "w(V-1)".
/// Each t(i) is a probe over the V + 1 tokens of v and "a".
fn probes(p: usize, v: usize) -> Grammar {
    let mut prods = vec![prod(
        "top",
        (0..p)
            .map(|i| vec![sens_term(&format!("k{i}")), reference(&format!("t{i}"))])
            .collect(),
    )];
    for i in 0..p {
        let mut t = prod(
            &format!("t{i}"),
            vec![vec![
                opt(group(vec![vec![sens_term("a"), sens_term("d")]])),
                sens_term("a"),
                reference("v"),
            ]],
        );
        t.sp = Some(SrcSpan::at(10 * i, 10 * i + 9, i + 2, 1));
        prods.push(t);
    }
    prods.push(prod(
        "v",
        (0..v).map(|i| vec![sens_term(&format!("w{i}"))]).collect(),
    ));
    Grammar::new(prods)
}

/// A = [x0] … [x(k-1)] "y" / [x1] … [x(k-1)] "y" / … / "y" / "z": every
/// first token but "z" begins several alternatives.
fn options(k: usize) -> Grammar {
    let mut alts: Vec<_> = (0..k)
        .map(|j| {
            let mut alt: Vec<_> = (0..k - j)
                .map(|i| opt(sens_term(&format!("x{}", i + j))))
                .collect();
            alt.push(sens_term("y"));
            alt
        })
        .collect();
    alts.push(vec![sens_term("z")]);
    Grammar::new(vec![prod("A", alts)])
}

/// A = [x0] … [x(k-1)] "y" / "x0" "w": one run of options, contested at
/// its first token alone.
fn run(k: usize) -> Grammar {
    let mut first: Vec<_> = (0..k).map(|i| opt(sens_term(&format!("x{i}")))).collect();
    first.push(sens_term("y"));
    Grammar::new(vec![prod(
        "A",
        vec![first, vec![sens_term("x0"), sens_term("w")]],
    )])
}

fn class(last: u32) -> String {
    format!(
        "[{}-{}]",
        char::from_u32(0x100).expect("a code point"),
        char::from_u32(last).expect("a code point")
    )
}

/// C character classes nested inside one another, [Ā-ā], [Ā-Ă], …, each
/// beside a literal of its own: C(C + 1)/2 token-set members.
fn nested(c: usize) -> Grammar {
    Grammar::new(vec![prod(
        "A",
        (0..c)
            .map(|i| {
                vec![
                    rx(&class(0x101 + i as u32), "u"),
                    sens_term(&format!("e{i}")),
                ]
            })
            .collect(),
    )])
}

/// S = "s" F D: F holds C nested classes, as `nested`'s A does, and D N
/// alternatives of distinct heads, each past a reference, which a
/// dispatcher decides.
fn dispatched(c: usize, n: usize) -> Grammar {
    let mut f = nested(c).productions.remove(0);
    f.name = "F".to_string();
    Grammar::new(vec![
        prod(
            "S",
            vec![vec![sens_term("s"), reference("F"), reference("D")]],
        ),
        f,
        prod(
            "D",
            (0..n)
                .map(|i| vec![sens_term(&format!("k{i}")), reference("B"), sens_term("e")])
                .collect(),
        ),
        prod("B", vec![vec![sens_term("b")], vec![sens_term("c")]]),
    ])
}

const PROBE: &str = "Write that optional prefix and what follows it as two alternatives, \
                     one with the prefix and one without.";
const ALTERNATES: &str = "Factor what its alternatives share into rules of their own.";
const TOKEN_SET: &str = "Write fewer character classes that overlap it.";
const REMOVE: &str = "Remove fewer names.";

fn adds(subject: &str, doing: &str, room: usize, written: usize, advice: &str) -> String {
    format!(
        "x: {subject} exceeds the emit limit of 3500000 units while {doing}, which would add \
         more than {room} units to the {written} already written. {advice}"
    )
}

fn holds(rule: &str, room: usize, written: usize) -> String {
    format!(
        "x: rule '{rule}' exceeds the emit limit of 3500000 units while enumerating the token \
         sequences that tell its alternatives apart, which would hold more than {room} units \
         on top of the {written} already written. Give its alternatives first tokens that \
         tell them apart, or factor what they begin with into a rule of its own."
    )
}

fn emit(grammar: &Grammar) -> Result<GrammarSpec, EmitError> {
    emit_grammar_spec(grammar, &ConvertOptions::tag("x"))
}

/// Three seconds in an optimised build, as the TypeScript and Go tests
/// give: the refusals take 0.35 s and 0.45 s where they were measured, a
/// slower machine has taken twice that, and what the limit guards against
/// is the minutes these shapes took unbounded. An unoptimised test build
/// takes several times as long.
fn within() -> Duration {
    if cfg!(debug_assertions) {
        Duration::from_secs(10)
    } else {
        Duration::from_secs(3)
    }
}

/// Compiles what `make` builds, afresh each time, until a compile is
/// refused within `within()`, at most three times, and returns the last
/// refusal and the fastest time. Whatever else the machine is running can
/// slow one compile down, which says nothing about the work the refusal
/// takes.
fn refused_within(make: impl Fn() -> Grammar) -> (EmitError, Duration) {
    let mut best = Duration::MAX;
    let mut last = None;
    for _ in 0..3 {
        let grammar = make();
        let start = Instant::now();
        let result = emit(&grammar);
        best = best.min(start.elapsed());
        last = Some(result.expect_err("the grammar compiled"));
        if best < within() {
            break;
        }
    }
    (last.expect("one try"), best)
}

#[test]
fn is_three_and_a_half_million_units() {
    assert_eq!(MAX_EMIT_UNITS, 3_500_000);
}

#[test]
fn refuses_a_thousand_probes_over_a_thousand_tokens_in_seconds() {
    // Each probe's list is charged with the rule it becomes, 4·1,001 + 2
    // units, as it is made: t873's would pass the limit.
    let (err, elapsed) = refused_within(|| probes(1000, 1000));
    assert_eq!(
        err.message,
        adds(
            "rule 't873'",
            "writing the probe for one of its optional prefixes",
            2762,
            3_497_238,
            PROBE
        )
    );
    assert_eq!(err.rule.as_deref(), Some("t873"));
    assert_eq!(err.sp, Some(SrcSpan::at(8730, 8739, 875, 1)));
    assert!(elapsed < within(), "refused after {elapsed:?} at best");
}

#[test]
fn counts_every_unit_the_probe_shape_writes_up_to_the_limit_and_no_further() {
    // P probes over V tokens write 4PV + 44P + 3V + 10 units: 3,498,599 for
    // a thousand over 863, which compiles, and 3,502,602 over 864, which is
    // refused while t881's dispatcher is written.
    let spec = emit(&probes(1000, 863)).expect("a thousand probes over 863");
    let probe = spec.rule["t999$pd0$probe"].as_ref().expect("a rule");
    assert_eq!(probe.open.len(), 865);
    let err = emit(&probes(1000, 864)).expect_err("the probes compiled");
    assert_eq!(
        err.message,
        adds(
            "rule 't881'",
            "writing its alternates",
            9,
            3_499_991,
            ALTERNATES
        )
    );
}

#[test]
fn refuses_forty_alternatives_of_up_to_forty_options_in_seconds() {
    let (err, elapsed) = refused_within(|| options(40));
    assert_eq!(err.message, holds("A", 3_486_837, 13163));
    assert_eq!(err.rule.as_deref(), Some("A"));
    assert!(elapsed < within(), "refused after {elapsed:?} at best");
}

#[test]
fn refuses_the_options_shape_from_thirty_nine_alternatives_on() {
    let err = emit(&options(39)).expect_err("the options compiled");
    assert_eq!(err.message, holds("A", 3_487_738, 12262));
}

#[test]
fn counts_what_a_choice_enumerates_up_to_the_limit_and_no_further() {
    // The run of 133 options enumerates just under the limit and compiles;
    // the run of 134 passes it while the enumeration is under way.
    emit(&run(133)).expect("the run of 133");
    let err = emit(&run(134)).expect_err("the run of 134 compiled");
    assert_eq!(err.message, holds("A", 3_490_551, 9449));
}

#[test]
fn counts_every_token_set_member_up_to_the_limit_and_no_further() {
    // C nested classes write C(C + 1)/2 members and 4C + 7 units more:
    // 3,499,332 for 2,641 classes, which compile, and 3,501,978 for 2,642,
    // refused while A is written.
    let spec = emit(&nested(2641)).expect("2,641 nested classes");
    let members: usize = spec.options["tokenSet"]
        .as_object()
        .expect("token sets")
        .values()
        .map(|set| set.as_array().expect("members").len())
        .sum();
    assert_eq!(members, 2641 * 2642 / 2);
    let err = emit(&nested(2642)).expect_err("2,642 nested classes compiled");
    assert_eq!(
        err.message,
        adds(
            "rule 'A'",
            "writing its alternates",
            5954,
            3_494_046,
            ALTERNATES
        )
    );
}

#[test]
fn holds_the_prefixes_a_choice_dispatches_on_until_its_alternates_are_written() {
    // The nested classes fill most of the limit; D's dispatcher then holds
    // the prefixes it decided on, two units for each of its N heads, while
    // it writes the alternates made from them.
    let spec = emit(&dispatched(2635, 569)).expect("569 dispatched alternatives");
    assert!(spec.rule.contains_key("D"));
    let err = emit(&dispatched(2635, 570)).expect_err("570 dispatched alternatives compiled");
    assert_eq!(
        err.message,
        adds(
            "rule 'D'",
            "writing its alternates",
            15372,
            3_484_628,
            ALTERNATES
        )
    );
}

#[test]
fn counts_the_entries_the_removals_write_up_to_the_limit_and_no_further() {
    // Each name a grammar removes writes a null rule and a null fixed token
    // in the spec, 2 units. Beside the 3,499,332 units 2,641 nested classes
    // write, 334 names fill the limit exactly, and 335 would pass it.
    let removing = |n: usize| {
        let mut g = nested(2641);
        g.remove = (0..n).map(|i| format!("gone{i}")).collect();
        g
    };
    let spec = emit(&removing(334)).expect("334 removals");
    assert_eq!(spec.rule.get("gone333"), Some(&None));
    let err = emit(&removing(335)).expect_err("335 removals compiled");
    assert_eq!(
        err.message,
        adds(
            "the removal list of 335 names",
            "writing the rules and tokens it removes",
            668,
            3_499_332,
            REMOVE
        )
    );
    assert_eq!(err.rule, None);
}

#[test]
fn refuses_the_token_set_that_would_pass_the_limit_naming_its_class() {
    // The first 2,645 classes list 3,499,335 members; the next would list
    // 2,646 more.
    let err = emit(&nested(2646)).expect_err("2,646 nested classes compiled");
    assert_eq!(
        err.message,
        adds(
            &format!("the character class /{}/", class(0xb56)),
            "listing the members of its token set",
            665,
            3_499_335,
            TOKEN_SET
        )
    );
    assert_eq!(err.rule, None);
}
