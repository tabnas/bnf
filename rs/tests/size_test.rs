// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

//! The size of what the emitter produces is a contract (tabnas/bnf#71).
//! Mirrors `ts/test/size.test.js` and `go/size_test.go`: a choice is
//! dispatched on the first token of each alternative, and the lookahead
//! deepens only under a head two alternatives share, so the emitted table
//! grows with the number of decisions rather than with the product of the
//! tokens that can fill four positions. Every count is exact and pinned.

mod common;

use common::{group, prod, reference, sens_term, star};
use tabnas_bnf::{emit_grammar_spec, ConvertOptions, Element, Grammar, GrammarSpec, Production};

fn kw(n: usize) -> Production {
    let alts: Vec<Vec<Element>> = (1..=n).map(|i| vec![sens_term(&format!("k{i}"))]).collect();
    prod("kw", alts)
}

// doc = "{" *entry "}" ; entry = kw kw ; kw = "k1" / ... / "kN"
fn starred(n: usize) -> Grammar {
    Grammar::new(vec![
        prod(
            "doc",
            vec![vec![
                sens_term("{"),
                star(reference("entry")),
                sens_term("}"),
            ]],
        ),
        prod("entry", vec![vec![reference("kw"), reference("kw")]]),
        kw(n),
    ])
}

// doc = "{" x "}" ; x = entry entry / "z" ; entry = kw kw ; kw = ...
fn choice(n: usize) -> Grammar {
    Grammar::new(vec![
        prod(
            "doc",
            vec![vec![sens_term("{"), reference("x"), sens_term("}")]],
        ),
        prod(
            "x",
            vec![
                vec![reference("entry"), reference("entry")],
                vec![sens_term("z")],
            ],
        ),
        prod("entry", vec![vec![reference("kw"), reference("kw")]]),
        kw(n),
    ])
}

fn opens(spec: &GrammarSpec, name: &str) -> usize {
    spec.rule
        .get(name)
        .and_then(|r| r.as_ref())
        .map(|r| r.open.len())
        .unwrap_or_else(|| panic!("no rule {name}"))
}

fn rule_matching(spec: &GrammarSpec, f: impl Fn(&str) -> bool) -> String {
    spec.rule
        .keys()
        .find(|n| f(n))
        .cloned()
        .unwrap_or_else(|| panic!("no matching rule"))
}

fn total_opens(spec: &GrammarSpec) -> usize {
    spec.rule
        .values()
        .filter_map(|r| r.as_ref())
        .map(|r| r.open.len())
        .sum()
}

fn parses(spec: &GrammarSpec, src: &str) -> bool {
    let mut parser = tabnas::Tabnas::new();
    spec.install(&mut parser).expect("install");
    parser.parse(src).is_ok()
}

fn opts() -> ConvertOptions {
    ConvertOptions::tag("rp").start("doc").word_keywords(true)
}

#[test]
fn a_repeated_entry_dispatches_on_heads_not_paths() {
    // The star helper: one entry per keyword head, one FOLLOW peek for
    // the `}` that ends the loop, and the bare fallback.
    for n in [2usize, 8, 26] {
        let spec = emit_grammar_spec(&starred(n), &opts()).expect("emit");
        let helper = rule_matching(&spec, |name| name.ends_with("star_entry"));
        assert_eq!(opens(&spec, &helper), n + 2, "N={n}: {helper}");
    }
}

#[test]
fn an_uncontested_choice_dispatches_on_one_token_per_head() {
    let spec = emit_grammar_spec(&choice(26), &opts()).expect("emit");
    // 26 keyword heads for the first alternative, one for "z".
    assert_eq!(opens(&spec, "x"), 27);
}

#[test]
fn a_token_class_is_one_lookahead_token() {
    let spec = emit_grammar_spec(&starred(26), &opts().token_classes(true)).expect("emit");
    // The class is a set; the helper peeks it once. The class rule
    // itself keeps its 26 alternates, one per member: it is the rule
    // that builds the node.
    let sets = spec.options.get("tokenSet").expect("token sets");
    let kw_set = sets.get("kw").expect("kw set");
    assert_eq!(kw_set.as_array().map(|a| a.len()), Some(26));
    assert_eq!(sets.as_object().map(|o| o.len()), Some(1));
    let helper = rule_matching(&spec, |name| name.ends_with("star_entry"));
    assert_eq!(opens(&spec, &helper), 3, "{helper}");
    assert_eq!(opens(&spec, "kw"), 26);
    let x = emit_grammar_spec(&choice(26), &opts().token_classes(true)).expect("emit");
    assert_eq!(opens(&x, "x"), 2);
    // The whole grammar stays small, and installs and parses.
    assert!(
        total_opens(&spec) < 40,
        "{} open alternates",
        total_opens(&spec)
    );
    let mut parser = tabnas::Tabnas::new();
    spec.install(&mut parser).expect("install");
    let out = parser.parse("{k1 k2 k26 k1}").expect("parse");
    // The class keeps its node: it is a rule of its own, not inlined.
    assert!(out.to_json().to_string().contains("\"kw\""));
    assert!(parser.parse("{k1}").is_err());
}

#[test]
fn a_token_class_leaves_the_tree_as_the_option_off_compile_does() {
    // A leading reference to the class is consumed as its one token where
    // the plain compile inlines the class's alternatives, and stays a
    // node where the plain compile keeps the reference: the same parse
    // result, node for node, with the option on or off.
    let off = emit_grammar_spec(&starred(26), &opts()).expect("emit");
    let on = emit_grammar_spec(&starred(26), &opts().token_classes(true)).expect("emit");
    let mut poff = tabnas::Tabnas::new();
    off.install(&mut poff).expect("install");
    let mut pon = tabnas::Tabnas::new();
    on.install(&mut pon).expect("install");
    for src in ["{k1 k2 k26 k1}", "{}", "{k3 k3}"] {
        let a = poff.parse(src).expect("off");
        let b = pon.parse(src).expect("on");
        assert_eq!(a.to_json(), b.to_json(), "{src}");
    }
}

#[test]
fn the_emitted_grammar_installs_and_parses_at_n26_either_way() {
    let spec = emit_grammar_spec(&starred(26), &opts()).expect("emit");
    assert!(parses(&spec, "{k1 k2 k2 k1}"));
    assert!(!parses(&spec, "{k1}"));
}

#[test]
fn a_contested_head_deepens_only_as_far_as_the_decision_needs() {
    // Two alternatives share `a`; they part at the second token, so the
    // dispatcher peeks two tokens under `a` and one under `z`.
    let spec = emit_grammar_spec(
        &Grammar::new(vec![
            prod("doc", vec![vec![reference("x")]]),
            prod(
                "x",
                vec![
                    vec![sens_term("a"), reference("t"), reference("u")],
                    vec![sens_term("a"), reference("u"), reference("t")],
                    vec![sens_term("z"), reference("t"), reference("u")],
                ],
            ),
            prod("t", vec![vec![sens_term("."), sens_term(".")]]),
            prod("u", vec![vec![sens_term(","), sens_term(",")]]),
        ]),
        &ConvertOptions::tag("rp").start("doc"),
    )
    .expect("emit");
    let x = spec.rule.get("x").and_then(|r| r.as_ref()).expect("x");
    let got: Vec<(String, u64)> = x
        .open
        .iter()
        .map(|o| {
            (
                o.s().unwrap_or("").to_string(),
                o.get("b").and_then(|b| b.as_u64()).unwrap_or(0),
            )
        })
        .collect();
    assert_eq!(
        got,
        vec![
            ("#A #T".to_string(), 2),
            ("#A #T1".to_string(), 2),
            ("#Z".to_string(), 1)
        ]
    );
    for src in ["a..,,", "a,,..", "z..,,"] {
        assert!(parses(&spec, src), "{src} should parse");
    }
    assert!(!parses(&spec, "a.,.,"));
}

#[test]
fn a_repetition_contested_by_what_follows_it_looks_across_the_boundary() {
    // start = *( "a" t ) "a" "b" with t = "x" "x": at an `a` the loop may
    // continue (`a x`) or exit (`a b`). The exit is an open path, so the
    // continue entries keep the full window here, as they always did.
    let spec = emit_grammar_spec(
        &Grammar::new(vec![
            prod(
                "start",
                vec![vec![
                    star(group(vec![vec![sens_term("a"), reference("t")]])),
                    sens_term("a"),
                    sens_term("b"),
                ]],
            ),
            prod("t", vec![vec![sens_term("x"), sens_term("x")]]),
        ]),
        &ConvertOptions::tag("rp").start("start"),
    )
    .expect("emit");
    let helper = rule_matching(&spec, |name| {
        name.contains("star") && name.ends_with("group")
    });
    let first = spec.rule[&helper].as_ref().expect("helper").open[0]
        .s()
        .unwrap_or("")
        .to_string();
    assert!(first.starts_with("#A #X"), "{first}");
    for src in ["ab", "axxab", "axxaxxab"] {
        assert!(parses(&spec, src), "{src} should parse");
    }
    for src in ["b", "axx"] {
        assert!(!parses(&spec, src), "{src} should be refused");
    }
}
