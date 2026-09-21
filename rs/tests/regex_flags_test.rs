// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

// The flag half of a regex terminal, driven through the IR. The
// canonical compiler builds every emitted matcher with `new RegExp`, so
// the flag string is checked by the `RegExp` constructor there: it
// knows exactly `d g i m s u v y`, refuses a repeat, refuses `u` and
// `v` together, and reports the flags back in that fixed order rather
// than the order they were written. An emitter that checks only for
// `i` accepts IR TypeScript refuses, writes the bad flags into the
// matcher and leaves the failure to whoever installs the spec.
//
// Every expectation below was taken from node against `ts/dist`; where
// one looks arbitrary (a flag that is refused only sometimes) the
// comment says which canonical behaviour it is holding.

mod common;

use common::{match_sources, prod, rx};
use tabnas_bnf::{emit_grammar_spec, ConvertOptions, EmitError, Grammar, Production};

fn emit(prods: Vec<Production>) -> Result<Vec<(String, String)>, EmitError> {
    emit_grammar_spec(
        &Grammar::new(prods),
        &ConvertOptions::tag("tst").start("top"),
    )
    .map(|spec| match_sources(&spec))
}

/// One class terminal, alone, so the class keeps its own matcher and the
/// flags reach the emitted text.
fn lone(flags: &str) -> Result<Vec<(String, String)>, EmitError> {
    emit(vec![prod("top", vec![vec![rx("[a-z]", flags)]])])
}

/// The serialised matcher for the single class `lone` emits.
fn lone_matcher(flags: &str) -> String {
    let tokens = lone(flags).unwrap_or_else(|e| panic!("flags {flags:?} should emit: {e}"));
    let (_, source) = tokens
        .iter()
        .find(|(name, _)| name.starts_with("#RX"))
        .unwrap_or_else(|| panic!("flags {flags:?}: no class matcher in {tokens:?}"));
    source.clone()
}

// `new RegExp('[a-z]', 'q')` throws, so the canonical compiler never
// emits this grammar. The port used to, with `q` written into the
// matcher for the engine to choke on.
#[test]
fn an_unknown_flag_is_refused() {
    for flags in ["q", "z", "iq", "qi", "e", "x", "I", "U"] {
        let err = lone(flags).expect_err(&format!("flags {flags:?} should be refused"));
        assert!(
            err.to_string().contains("invalid regular expression flags"),
            "flags {flags:?}: unexpected refusal: {err}"
        );
    }
}

// `new RegExp('[a-z]', 'ii')` throws too: a flag may appear once.
#[test]
fn a_duplicated_flag_is_refused() {
    for flags in ["ii", "gg", "uu", "iis", "smsi", "dgimsuyy"] {
        let err = lone(flags).expect_err(&format!("flags {flags:?} should be refused"));
        assert!(
            err.to_string().contains("duplicate flag"),
            "flags {flags:?}: expected a duplicate-flag refusal, got: {err}"
        );
    }
}

// `u` and `v` are each valid and are mutually exclusive, which is a
// separate rule from either of the two above.
#[test]
fn unicode_and_unicode_sets_together_are_refused() {
    for flags in ["uv", "vu", "iuv"] {
        let err = lone(flags).expect_err(&format!("flags {flags:?} should be refused"));
        assert!(
            err.to_string().contains("set both u and v"),
            "flags {flags:?}: expected a u/v refusal, got: {err}"
        );
    }
}

// A refusal has to say WHICH terminal was wrong; a grammar has many.
#[test]
fn the_refusal_names_the_token_and_the_flags() {
    let err = lone("q").expect_err("refused");
    let text = err.to_string();
    assert!(text.starts_with("tst: "), "no diagnostic prefix: {text}");
    assert!(text.contains("#RX__A_Z"), "the token is not named: {text}");
    assert!(
        text.contains('q'),
        "the offending flag is not named: {text}"
    );
}

// The whole set the canonical constructor accepts, one at a time and all
// together. `d`, `g` and `y` change nothing the engine's `regex` crate
// acts on and `v` it refuses outright, but all four are IR TypeScript
// emits, so refusing them here would be the port inventing a rule.
#[test]
fn every_flag_the_canonical_constructor_accepts_is_accepted() {
    for flags in ["", "d", "g", "i", "m", "s", "u", "v", "y"] {
        assert_eq!(
            lone_matcher(flags),
            format!("@~/^[a-z]/{flags}"),
            "flags {flags:?} should emit unchanged"
        );
    }
    assert_eq!(lone_matcher("dgimsuy"), "@~/^[a-z]/dgimsuy");
    assert_eq!(lone_matcher("dgimsvy"), "@~/^[a-z]/dgimsvy");
}

// `RegExp.prototype.flags` reports `d g i m s u v y` in that order
// whatever order they were supplied in, and the canonical compiler
// serialises what it reports. Emitting them as written diverges from
// TypeScript's text for the same IR, which is the one thing this port
// promises not to do.
#[test]
fn flags_are_emitted_in_the_canonical_order() {
    for (written, canonical) in [
        ("yu", "uy"),
        ("ymsigud", "dgimsuy"),
        ("sm", "ms"),
        ("gd", "dg"),
        ("yvimsgd", "dgimsvy"),
    ] {
        assert_eq!(
            lone_matcher(written),
            format!("@~/^[a-z]/{canonical}"),
            "flags {written:?} should serialise as {canonical:?}"
        );
    }
}

// Where a class overlaps another it is replaced by a token SET over
// one-character atoms, and its own flags never reach a matcher — so the
// canonical compiler never constructs that `RegExp` and never sees the
// bad flag. Checking the flags anywhere but at the matcher would refuse
// a grammar TypeScript emits. (`ii` is still refused here, because case
// folding takes the class out of the contest and back to a matcher of
// its own.)
#[test]
fn a_flag_that_never_reaches_a_matcher_is_not_checked() {
    let contested = |flags: &str| {
        emit(vec![prod(
            "top",
            vec![vec![rx("[a-z]", flags)], vec![rx("[a-m]", "")]],
        )])
    };
    let tokens =
        contested("q").expect("a contested class drops its own matcher, bad flags and all");
    assert!(
        tokens.iter().all(|(name, _)| name.starts_with("#RXA")),
        "expected only partition atoms, got {tokens:?}"
    );
    assert_eq!(
        contested("q").unwrap(),
        contested("").unwrap(),
        "the unused flag should not change the partition"
    );
    contested("ii").expect_err("case folding keeps the class, so its flags are checked");
}

// The literal path builds a matcher too (case-insensitive literals, and
// word keywords needing a boundary guard). Its flags are the emitter's
// own, so they must always pass the same check.
#[test]
fn emitted_literal_matchers_carry_valid_flags() {
    let spec = emit_grammar_spec(
        &Grammar::new(vec![prod(
            "top",
            vec![vec![common::term("if"), common::sens_term("X")]],
        )]),
        &ConvertOptions::tag("tst").start("top").word_keywords(true),
    )
    .expect("emit");
    let tokens = match_sources(&spec);
    assert!(!tokens.is_empty(), "expected literal matchers");
    for (name, source) in tokens {
        let flags = source.rsplit('/').next().unwrap_or("");
        assert!(
            flags.chars().all(|f| "dgimsuvy".contains(f)),
            "{name} carries unknown flags {flags:?}"
        );
        let mut seen = String::new();
        for f in flags.chars() {
            assert!(!seen.contains(f), "{name} repeats flag {f:?} in {flags:?}");
            seen.push(f);
        }
    }
}
