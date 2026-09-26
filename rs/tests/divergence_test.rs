// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

// The executable register for the differences `DIVERGENCE.md` records
// under "Rust". A divergence that only exists in prose cannot fail, so
// it drifts: the page keeps describing a behaviour the port stopped
// having, or stopped having a behaviour the port still has. Every entry
// in that section has a test here, and repairing an entry means deleting
// its row from the page AND its test from this file.
//
// The engine-level entries are registered elsewhere, in
// `oracle_test.rs`: `ENGINE_VALUE_DIVERGENCES` and
// `ENGINE_REJECTS_WHAT_TYPESCRIPT_ACCEPTS` are asserted both ways there,
// against the recorded TypeScript verdict, which is a claim this file
// cannot make without the oracle fixtures.

mod common;

use common::{match_sources, opt, prod, reference, rx, sens_term, term};
use tabnas_bnf::{
    emit_grammar_spec, ConvertOptions, Element, Grammar, Production, MAX_ELEMENT_DEPTH,
};

fn emit(prods: Vec<Production>, opts: ConvertOptions) -> Result<tabnas_bnf::GrammarSpec, String> {
    emit_grammar_spec(&Grammar::new(prods), &opts).map_err(|e| e.to_string())
}

// DIVERGENCE.md, "The word-keyword guard is `\b`, not
// `(?![A-Za-z0-9_])`". TypeScript emits the negative lookahead; the
// engine's regex crate has no lookaround, so this port and the Go port
// emit `\b`. The two agree on ASCII and differ only for a keyword
// followed immediately by a non-ASCII letter.
#[test]
fn the_word_keyword_guard_is_a_word_boundary() {
    let spec = emit(
        vec![prod("stmt", vec![vec![term("option"), term(";")]])],
        ConvertOptions::tag("tst").start("stmt").word_keywords(true),
    )
    .expect("emit");
    let sources: Vec<String> = match_sources(&spec).into_iter().map(|(_, s)| s).collect();
    assert!(
        sources.iter().any(|s| s.contains("option\\b")),
        "no word-boundary guard in {sources:?}"
    );
    assert!(
        !sources.iter().any(|s| s.contains("(?!")),
        "a negative lookahead reached the matcher, which the engine's \
         regex dialect cannot compile: {sources:?}"
    );
}

// DIVERGENCE.md, "Regular expression terminals are refused at emit
// time". Both compilers build the matcher as the token is allocated, so
// an invalid pattern fails the emit in each; the DIALECTS differ. These
// patterns are ones `new RegExp` accepts and the engine's regex crate
// does not, so TypeScript emits them and the Rust engine refuses them at
// install, while this port refuses them at emit and names the token.
#[test]
fn a_pattern_outside_the_engine_dialect_is_refused_at_emit() {
    for pattern in ["a(?=b)", "a(?!b)", "(?<=a)b", r"(a)\1"] {
        let err = emit(
            vec![prod("top", vec![vec![rx(pattern, "")]])],
            ConvertOptions::tag("tst").start("top"),
        )
        .expect_err(&format!("pattern {pattern:?} should be refused"));
        assert!(
            err.starts_with("tst: invalid regular expression for token "),
            "pattern {pattern:?}: unexpected refusal: {err}"
        );
    }
}

// A pattern BOTH dialects accept still emits, so the entry above is
// about the dialect and not about regex terminals in general.
#[test]
fn a_pattern_both_dialects_accept_still_emits() {
    emit(
        vec![prod("top", vec![vec![rx("[a-z]+", "i")]])],
        ConvertOptions::tag("tst").start("top"),
    )
    .expect("a shared-dialect pattern emits");
}

/// How many tokens deep each branch of `x = <a> t u / <b> u t` looks,
/// where `t = ";" ";"` and `u = "!" "!"`, as in `contest_test.rs`. The
/// two branches part at the second token, so the depth says whether the
/// dispatcher found that the two heads can meet.
fn contest_depths(a: Element, b: Element) -> Vec<usize> {
    let spec = emit(
        vec![
            prod("doc", vec![vec![reference("x")]]),
            prod(
                "x",
                vec![
                    vec![a, reference("t"), reference("u")],
                    vec![b, reference("u"), reference("t")],
                ],
            ),
            prod("t", vec![vec![sens_term(";"), sens_term(";")]]),
            prod("u", vec![vec![sens_term("!"), sens_term("!")]]),
        ],
        ConvertOptions::tag("ct").start("doc"),
    )
    .expect("emit");
    spec.rule["x"]
        .as_ref()
        .expect("x")
        .open
        .iter()
        .map(|o| o.s().unwrap_or("").split_whitespace().count())
        .collect()
}

// DIVERGENCE.md, "An escape is read in the dialect of the engine that
// runs it". Whether a regex head can meet a literal decides how deep the
// dispatch looks, and this port reads the head's escapes as the `regex`
// crate, which compiles it, does. TypeScript reads them as JavaScript
// does, so the same IR emits a different depth: a `\a` head is BEL here
// and the letter `a` there. Each row is this port's side of the table on
// that page. The TypeScript side is pinned in `ts/test/bnf.test.js` and
// the Go side by TestEscapeIsReadInTheRE2Dialect in `go/bnf_test.go`, so
// repairing any port turns its test red and the three are revisited
// together.
#[test]
fn an_escape_is_read_in_the_regex_crate_dialect() {
    for (pattern, literal, depth) in [
        (r"\a", "\x07", 2),
        (r"\a", "a", 1),
        (r"\x{41}", "x", 1),
        (r"\U00000041", "U", 1),
    ] {
        assert_eq!(
            contest_depths(rx(pattern, ""), sens_term(literal)),
            [depth, depth],
            "{pattern} beside {literal:?}: if this port reads the escape as \
             JavaScript does, delete this test, its twins and the page's entry \
             together"
        );
    }
}

/// `depth` nested `opt` wrappers around a literal. The literal then sits
/// at `depth + 1`, since the outermost element is itself level one.
fn nested(depth: usize) -> Element {
    let mut el = term("x");
    for _ in 0..depth {
        el = opt(el);
    }
    el
}

// DIVERGENCE.md, "Element nesting is refused past 128 levels". The
// passes over an element recurse as the canonical compiler does, and a
// Rust stack that runs out aborts the process rather than unwinding, so
// the depth is measured first and a grammar past the limit is an error
// return naming the rule. TypeScript keeps going several hundred levels
// further and then raises a catchable `RangeError`.
#[test]
fn element_nesting_is_refused_one_level_past_the_limit() {
    let err = emit(
        vec![prod("deep", vec![vec![nested(MAX_ELEMENT_DEPTH)]])],
        ConvertOptions::tag("tst").start("deep"),
    )
    .expect_err("one level past the limit is refused");
    assert!(
        err.contains("rule 'deep' nests elements more than 128 deep"),
        "the refusal must name the rule and the limit: {err}"
    );
    assert!(err.starts_with("tst: "), "and carry the tag: {err}");
}

// The limit is the last accepted depth, not the first refused one: a
// grammar exactly at it is measured and passed through.
#[test]
fn element_nesting_at_the_limit_is_measured_and_accepted() {
    emit(
        vec![prod("deep", vec![vec![nested(MAX_ELEMENT_DEPTH - 1)]])],
        ConvertOptions::tag("tst").start("deep"),
    )
    .unwrap_or_else(|e| panic!("depth {MAX_ELEMENT_DEPTH} is inside the limit: {e}"));
}

// The limit is `serde_json`'s own default nesting depth for a document,
// which is what makes an IR arriving as JSON already held to it.
#[test]
fn the_limit_matches_the_json_nesting_depth_the_page_cites() {
    assert_eq!(MAX_ELEMENT_DEPTH, 128);
}
