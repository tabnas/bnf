// Shared test helpers. Cargo compiles this module into EVERY integration
// test binary, so an item only one binary uses is dead code in the
// others; the allow keeps that from being a warning rather than hiding
// anything real.
#![allow(dead_code)]

use serde_json::Value;
use tabnas_bnf::{emit_grammar_spec, ConvertOptions, Element, Grammar, GrammarSpec, Production};

pub fn term(literal: &str) -> Element {
    Element::term(literal)
}

/// A literal the front-end declared case-sensitive.
pub fn sens_term(literal: &str) -> Element {
    Element::term_cs(literal, true)
}

pub fn reference(name: &str) -> Element {
    Element::reference(name)
}

pub fn tok(name: &str) -> Element {
    Element::token(name)
}

pub fn rx(pattern: &str, flags: &str) -> Element {
    Element::regex(pattern, flags)
}

pub fn opt(inner: Element) -> Element {
    Element::opt(inner)
}

pub fn star(inner: Element) -> Element {
    Element::star(inner)
}

pub fn plus(inner: Element) -> Element {
    Element::plus(inner)
}

pub fn group(alts: Vec<Vec<Element>>) -> Element {
    Element::group(alts)
}

pub fn prod(name: &str, alts: Vec<Vec<Element>>) -> Production {
    Production::new(name, alts)
}

/// Emit with the tag `t`, failing the test on a compile error.
pub fn emit_ir(prods: Vec<Production>, opts: Option<ConvertOptions>) -> GrammarSpec {
    let opts = opts.unwrap_or_else(|| ConvertOptions::tag("t"));
    match emit_grammar_spec(&Grammar::new(prods), &opts) {
        Ok(spec) => spec,
        Err(e) => panic!("emit: {e}"),
    }
}

/// Every match-token source in an emitted spec, by token name.
pub fn match_sources(spec: &GrammarSpec) -> Vec<(String, String)> {
    spec.options
        .get("match")
        .and_then(|m| m.get("token"))
        .and_then(Value::as_object)
        .map(|tokens| {
            tokens
                .iter()
                .map(|(n, v)| (n.clone(), v.as_str().unwrap_or("").to_string()))
                .collect()
        })
        .unwrap_or_default()
}

/// The token-set names in an emitted spec.
pub fn token_set_names(spec: &GrammarSpec) -> Vec<String> {
    spec.options
        .get("tokenSet")
        .and_then(Value::as_object)
        .map(|sets| sets.keys().cloned().collect())
        .unwrap_or_default()
}

/// The `s` field of every open alternate of a rule.
pub fn alt_seqs(spec: &GrammarSpec, rule: &str) -> Vec<Option<String>> {
    spec.rule[rule]
        .as_ref()
        .unwrap_or_else(|| panic!("no {rule} rule"))
        .open
        .iter()
        .map(|a| a.s().map(str::to_string))
        .collect()
}

/// The mark of every open alternate of a rule (empty when unmarked).
pub fn alt_marks(spec: &GrammarSpec, rule: &str) -> Vec<String> {
    spec.rule[rule]
        .as_ref()
        .unwrap_or_else(|| panic!("no {rule} rule"))
        .open
        .iter()
        .map(|a| a.m.clone().unwrap_or_default())
        .collect()
}

/// Install a spec on a fresh engine (closure refs bound) and parse.
pub fn parse_with(spec: &GrammarSpec, src: &str) -> Result<Value, Box<tabnas::TabnasError>> {
    let mut parser = tabnas::Tabnas::new();
    spec.install(&mut parser).expect("install");
    parser.parse(src).map(|v| v.to_json()).map_err(Box::new)
}
