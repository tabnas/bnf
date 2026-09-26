// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

//! Whether two dispatch heads CONTEST (can the lexer hand the same input
//! to both) decides how deep the dispatcher looks. An answer of "no" that
//! is wrong emits one-token entries for a decision that needs two, and
//! the first branch commits on the first token and rejects input the
//! other branch accepts. Mirrors `ts/test/contest.test.js`.

mod common;

use common::{prod, reference, rx, sens_term, term, tok};
use tabnas_bnf::{emit_grammar_spec, ConvertOptions, Element, Grammar, GrammarSpec};

// doc = x ; x = <a> t u / <b> u t ; t = "." "." ; u = "," ","
fn grammar(a: Element, b: Element) -> Grammar {
    Grammar::new(vec![
        prod("doc", vec![vec![reference("x")]]),
        prod(
            "x",
            vec![
                vec![a, reference("t"), reference("u")],
                vec![b, reference("u"), reference("t")],
            ],
        ),
        prod("t", vec![vec![sens_term("."), sens_term(".")]]),
        prod("u", vec![vec![sens_term(","), sens_term(",")]]),
    ])
}

fn depths(spec: &GrammarSpec) -> Vec<usize> {
    spec.rule["x"]
        .as_ref()
        .expect("x")
        .open
        .iter()
        .map(|o| o.s().unwrap_or("").split_whitespace().count())
        .collect()
}

fn parses(spec: &GrammarSpec, src: &str, relex: bool) -> bool {
    let mut options = tabnas::Options::default();
    options.lex.relex = relex;
    let mut parser = tabnas::Tabnas::with_options(options);
    spec.install(&mut parser).expect("install");
    parser.parse(src).is_ok()
}

fn opts(word_keywords: bool) -> ConvertOptions {
    ConvertOptions::tag("ct")
        .start("doc")
        .word_keywords(word_keywords)
}

#[test]
fn a_word_keyword_contests_a_longer_literal_that_continues_with_punctuation() {
    let spec = emit_grammar_spec(&grammar(sens_term("a"), sens_term("a-b")), &opts(true)).unwrap();
    assert_eq!(depths(&spec), [2, 2]);
    assert!(parses(&spec, "a-b,,..", true));
    assert!(parses(&spec, "a..,,", true));
    let word = emit_grammar_spec(&grammar(sens_term("a"), sens_term("ab")), &opts(true)).unwrap();
    assert_eq!(depths(&word), [1, 1], "a word continuation keeps the guard");
}

#[test]
fn case_insensitive_literals_contest_by_what_their_matchers_fold() {
    // A literal with no ASCII letter compiles to an exact fixed token, so
    // Σ and ς never meet; with ASCII letters the matcher folds case.
    let spec = emit_grammar_spec(&grammar(term("Σ"), term("ς")), &opts(false)).unwrap();
    assert_eq!(depths(&spec), [1, 1]);
    assert!(parses(&spec, "ς,,..", false));
    assert!(parses(&spec, "Σ..,,", false));
    let ascii = emit_grammar_spec(&grammar(term("a"), term("A-b")), &opts(false)).unwrap();
    assert_eq!(depths(&ascii), [2, 2]);
    assert!(parses(&ascii, "A-B,,..", true));
    assert!(parses(&ascii, "A..,,", true));
}

#[test]
fn a_regex_head_that_is_not_one_atom_contests_by_what_it_can_match() {
    let spec = emit_grammar_spec(&grammar(rx("a|b", ""), sens_term("b")), &opts(false)).unwrap();
    assert_eq!(depths(&spec), [2, 2]);
    assert!(parses(&spec, "b,,..", true));
    assert!(parses(&spec, "a..,,", true));
}

#[test]
fn the_any_token_contests_every_head() {
    let spec = emit_grammar_spec(&grammar(tok("#AA"), sens_term("b")), &opts(false)).unwrap();
    assert_eq!(depths(&spec), [2, 2]);
    assert!(parses(&spec, "b,,..", false));
    assert!(parses(&spec, "z..,,", false));
}

#[test]
fn a_regex_head_whose_first_character_the_coverage_cannot_name_contests() {
    // `\n`, `\t` and `\v` are one atom each, but `pattern_char_ranges`
    // declines to name what a control escape covers, so nothing says the
    // literal character is not what the pattern matches.
    for (pattern, literal) in [(r"\n", "\n"), (r"\t", "\t"), (r"\v", "\u{b}")] {
        let spec =
            emit_grammar_spec(&grammar(rx(pattern, ""), sens_term(literal)), &opts(false)).unwrap();
        assert_eq!(depths(&spec), [2, 2], "{pattern}");
    }
    let spec =
        emit_grammar_spec(&grammar(rx(r"\v", ""), sens_term("\u{b}")), &opts(false)).unwrap();
    assert!(parses(&spec, "\u{b}..,,", true));
    assert!(parses(&spec, "\u{b},,..", true));
}

#[test]
fn a_case_insensitive_head_beyond_ascii_contests_what_unicode_folding_lets_it_meet() {
    // `(?i)[Σ]` takes `ς`, and the coverage folds ASCII letters alone.
    let spec = emit_grammar_spec(&grammar(rx("[Σ]", "i"), sens_term("ς")), &opts(false)).unwrap();
    assert_eq!(depths(&spec), [2, 2]);
    for src in ["ς..,,", "Σ..,,", "ς,,.."] {
        assert!(parses(&spec, src, true), "{src}");
    }
    // Within ASCII the folded coverage stays exact: `(?i)[b]` meets no `a`.
    let ascii = emit_grammar_spec(&grammar(rx("[b]", "i"), sens_term("a")), &opts(false)).unwrap();
    assert_eq!(depths(&ascii), [1, 1]);
}

// `grammar` with tails no engine matcher can take (a number would swallow
// the `.` of `1..`): t = ";" ";" ; u = "!" "!".
fn semi(a: Element, b: Element) -> Grammar {
    Grammar::new(vec![
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
    ])
}

#[test]
fn an_engine_token_contests_what_its_matcher_can_take_when_the_parser_asks() {
    // Negotiated lexing runs only the matchers that can produce the token
    // an alternative wants: the number matcher takes a leading digit, the
    // string matcher a quote, and the text matcher any text no fixed
    // literal claims. The four-token dispatch this replaced kept each of
    // these pairs apart; a one-token dispatch did not.
    let cases = [
        (tok("#NR"), sens_term("1"), "1"),
        (tok("#ST"), sens_term("'a'"), "'a'"),
        (tok("#TX"), term("let"), "let"),
        (tok("#NR"), tok("#TX"), "1"),
        (tok("#NR"), rx("[0-9]", ""), "1"),
        (tok("#TX"), rx("[a-z]+", ""), "let"),
    ];
    for (a, b, text) in cases {
        let spec = emit_grammar_spec(&semi(a, b), &opts(false)).unwrap();
        assert_eq!(depths(&spec), [2, 2], "{text}");
        assert!(parses(&spec, &format!("{text};;!!"), true), "{text};;!!");
        assert!(parses(&spec, &format!("{text}!!;;"), true), "{text}!!;;");
    }
    // The text matcher defers to a fixed literal, and an emitted grammar
    // lexes no values: those stay one token deep.
    for (a, b) in [
        (tok("#TX"), sens_term("let")),
        (tok("#VL"), sens_term("true")),
    ] {
        let spec = emit_grammar_spec(&semi(a, b), &opts(false)).unwrap();
        assert_eq!(depths(&spec), [1, 1]);
    }
}

#[test]
fn a_brace_escape_is_a_code_point_only_under_the_u_flag() {
    // Without `u` or `v`, `\u{1}` is `u` once in the JavaScript matcher
    // the canonical runtime emits for, not U+0001, so the emitted dispatch
    // treats the head as inexact. The depths are the emitted-spec
    // contract; the Rust regex crate reads `\u{1}` as a code point under
    // any flags, so the parse side is pinned in TypeScript alone.
    let legacy = emit_grammar_spec(&semi(rx(r"\u{1}", ""), sens_term("u")), &opts(false)).unwrap();
    assert_eq!(depths(&legacy), [2, 2]);
    let unicode =
        emit_grammar_spec(&semi(rx(r"\u{1}", "u"), sens_term("u")), &opts(false)).unwrap();
    assert_eq!(depths(&unicode), [1, 1]);
}

#[test]
fn an_escape_whose_code_point_cannot_be_read_is_not_an_exact_head() {
    // A head the coverage cannot read contests every head. `\p{L}` is a
    // property, not the letter `p`, so a class holding it meets `é`. The
    // escapes the regex crate refuses outright (`\u1`, `\x1`, `\cA`, a
    // digit escape) never reach the dispatcher here; the two readers pin
    // them (`an_escape_is_one_code_point_only_when_it_can_be_read` in
    // `src/ranges.rs`), and TypeScript pins the dispatch.
    let property =
        emit_grammar_spec(&semi(rx(r"[\p{L}]", "u"), sens_term("é")), &opts(false)).unwrap();
    assert_eq!(depths(&property), [2, 2]);
    // Four hex digits are the code point they spell.
    let exact = emit_grammar_spec(&semi(rx(r"\u0041", ""), sens_term("u")), &opts(false)).unwrap();
    assert_eq!(depths(&exact), [1, 1]);
}

#[test]
fn an_escape_is_read_as_the_regex_crate_reads_it() {
    // This port compiles every matcher with the `regex` crate, so an
    // escape is read as that crate reads it, not as JavaScript does. `\a`
    // is BEL, which a JavaScript matcher reads as the letter `a`, and
    // `\U` spells a code point in eight hex digits or in braces. Read as
    // the letter after the backslash, each of these heads was held apart
    // from a literal BEL it takes, and the literal's branch was never
    // reached. Mirrors TestContestEscapeIsReadAsRE2ReadsIt in
    // go/contest_test.go.
    for pattern in [
        r"\a",
        r"[\a]",
        r"\a+",
        r"[\U00000007]",
        r"[\U{7}]",
        r"\x{7}",
    ] {
        let spec =
            emit_grammar_spec(&semi(rx(pattern, ""), sens_term("\x07")), &opts(false)).unwrap();
        assert_eq!(depths(&spec), [2, 2], "{pattern}");
        assert!(parses(&spec, "\x07;;!!", true), "{pattern}");
        assert!(parses(&spec, "\x07!!;;", true), "{pattern}");
    }
}

// A head the canonical matcher reads in code units (no `u` or `v`) that
// names a lead surrogate meets every astral character that surrogate
// begins: there it takes U+D800 as the first half of U+10000. Compared as
// code points the heads were disjoint and the decision stayed one token
// deep. The crate never meets a surrogate, but this port decides alike,
// so the three ports emit the same grammar (tabnas/bnf#75 review). The
// crate spells no surrogate, so the head names the lead surrogates by
// spanning them, and the TS lone-surrogate literal has no spelling here.
#[test]
fn a_head_read_in_code_units_naming_a_lead_surrogate_meets_an_astral_head() {
    let emit =
        |g: Grammar| emit_grammar_spec(&g, &ConvertOptions::tag("ct").start("doc")).expect("emit");
    let spanning = r"[\x{D7FF}-\x{E000}]";
    let astral = || rx(r"\x{10000}", "u");
    for (label, b) in [
        ("escape", astral()),
        ("class", rx(r"[\x{10000}-\x{10FFFF}]", "u")),
        ("astral literal", sens_term("\u{10000}")),
    ] {
        let spec = emit(semi(rx(spanning, ""), b));
        assert_eq!(depths(&spec), [2, 2], "{label}");
        assert!(parses(&spec, "\u{10000}!!;;", true), "{label}");
    }
    // Read in code points a lead surrogate is only ever one standing
    // alone, and meets no astral character, whether the class keeps its
    // own matcher or is laid over the partition, whose atoms read as the
    // classes they stand for.
    let alone = emit(semi(rx(spanning, "u"), astral()));
    let mut contested = semi(rx(spanning, "u"), astral());
    contested.productions[0]
        .alts
        .push(vec![rx(r"[\x{D000}-\x{E000}]", "u")]);
    let contested = emit(contested);
    let sets = |spec: &GrammarSpec| {
        spec.options
            .get("tokenSet")
            .is_some_and(|s| s.as_object().is_some_and(|o| !o.is_empty()))
    };
    assert!(!sets(&alone), "alone");
    assert!(sets(&contested), "partitioned");
    for (label, spec) in [("alone", alone), ("partitioned", contested)] {
        assert_eq!(depths(&spec), [1, 1], "{label}");
        assert!(parses(&spec, "\u{10000}!!;;", false), "{label}");
    }
}

// A class the canonical matcher reads in code units (no `u` or `v`) holds
// an astral character written in it as its lead and trail surrogates, so
// `[😀]` meets a `😁` head through the lead. The crate reads the code
// point, but this port adds the units beside it, so the three ports emit
// the same grammar (tabnas/bnf#75 review). Mirrors the TS and Go tests.
#[test]
fn a_class_read_in_code_units_takes_an_astral_character_as_two() {
    let emit =
        |g: Grammar| emit_grammar_spec(&g, &ConvertOptions::tag("ct").start("doc")).expect("emit");
    for p in ["[\u{1F600}]", "[b\u{1F600}]"] {
        let spec = emit(semi(rx(p, ""), sens_term("\u{1F601}")));
        assert_eq!(depths(&spec), [2, 2], "{p}");
        assert!(parses(&spec, "\u{1F601}!!;;", true), "{p}");
    }
    for (p, f) in [("[\u{1F600}]", "u"), ("\u{1F600}", "")] {
        let spec = emit(semi(rx(p, f), sens_term("\u{1F601}")));
        assert_eq!(depths(&spec), [1, 1], "{p} {f}");
    }
}
