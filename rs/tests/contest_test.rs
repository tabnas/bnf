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
