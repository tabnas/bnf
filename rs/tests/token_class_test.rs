// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

//! `token_classes` compiles a production whose alternatives are all
//! single literals or engine tokens to one engine token set. What a
//! grammar accepts must not depend on the option: each case here is one
//! where it once did (tabnas/bnf#74 review). Mirrors
//! `ts/test/token-class.test.js`.

mod common;

use common::{alt_seqs, prod, reference, rx, sens_term, tok, token_set_names};
use tabnas_bnf::{emit_grammar_spec, ConvertOptions, Grammar, GrammarSpec, Production};

fn emit(prods: Vec<Production>, token_classes: bool) -> GrammarSpec {
    let opts = ConvertOptions::tag("tc")
        .start("doc")
        .token_classes(token_classes);
    emit_grammar_spec(&Grammar::new(prods), &opts).expect("emit")
}

fn parses(spec: &GrammarSpec, src: &str, relex: bool) -> bool {
    let mut options = tabnas::Options::default();
    options.lex.relex = relex;
    let mut parser = tabnas::Tabnas::with_options(options);
    spec.install(&mut parser).expect("install");
    parser.parse(src).is_ok()
}

#[test]
fn a_class_set_head_takes_the_keyword_shadow_order_its_members_had() {
    // doc = x ; x = C / [a-z] D ; C = "a" / "b" ; D = [0-9]
    // With the option off C's literals are heads of x, each guarded ahead
    // of the character class that shadows it. The set that stands for
    // them is ordered the same way, or `a1` commits to C at `a` and fails
    // at `1`.
    let g = || {
        vec![
            prod("doc", vec![vec![reference("x")]]),
            prod(
                "x",
                vec![vec![reference("C")], vec![rx("[a-z]", ""), reference("D")]],
            ),
            prod("C", vec![vec![sens_term("a")], vec![sens_term("b")]]),
            prod("D", vec![vec![rx("[0-9]", "")]]),
        ]
    };
    let off = emit(g(), false);
    let on = emit(g(), true);
    assert_eq!(
        alt_seqs(&on, "x"),
        [
            Some("#C #ZZ".to_string()),
            Some("#RX__A_Z".to_string()),
            Some("#C".to_string())
        ]
    );
    for src in ["a1", "a", "b", "c1", "b1"] {
        assert!(parses(&off, src, true), "off: {src}");
        assert!(parses(&on, src, true), "on: {src}");
    }
}

#[test]
fn a_class_with_a_set_among_its_members_stays_a_production() {
    // C = #C / "a" names its own set; C = #D / "a" beside D = #C / "b"
    // names another class's. A set of sets is one the engine cannot
    // resolve, and expanding it never ended.
    let classes: [fn() -> Vec<Production>; 2] = [
        || vec![prod("C", vec![vec![tok("#C")], vec![sens_term("a")]])],
        || {
            vec![
                prod("C", vec![vec![tok("#D")], vec![sens_term("a")]]),
                prod("D", vec![vec![tok("#C")], vec![sens_term("b")]]),
            ]
        },
    ];
    for class in classes {
        let g = || {
            let mut out = vec![
                prod("doc", vec![vec![reference("x")]]),
                prod(
                    "x",
                    vec![
                        vec![reference("C"), reference("t"), reference("u")],
                        vec![sens_term("z"), reference("u"), reference("t")],
                    ],
                ),
                prod("t", vec![vec![sens_term("."), sens_term(".")]]),
                prod("u", vec![vec![sens_term(","), sens_term(",")]]),
            ];
            out.extend(class());
            out
        };
        let on = emit(g(), true);
        assert!(
            token_set_names(&on).is_empty(),
            "{:?}",
            token_set_names(&on)
        );
        assert_eq!(alt_seqs(&on, "x"), alt_seqs(&emit(g(), false), "x"));
        assert!(parses(&on, "a..,,", false));
        assert!(parses(&on, "z,,..", false));
    }
}

#[test]
fn an_empty_production_name_is_not_a_class() {
    // doc = x ; x = <""> "!" ; <""> = "a" / "b". The set would be named
    // `#`, which names nothing, and so would the token standing for the
    // reference.
    let g = || {
        vec![
            prod("doc", vec![vec![reference("x")]]),
            prod("x", vec![vec![reference(""), sens_term("!")]]),
            prod("", vec![vec![sens_term("a")], vec![sens_term("b")]]),
        ]
    };
    let on = emit(g(), true);
    assert!(
        token_set_names(&on).is_empty(),
        "{:?}",
        token_set_names(&on)
    );
    assert_eq!(alt_seqs(&on, "x"), alt_seqs(&emit(g(), false), "x"));
    assert!(parses(&on, "a!", false));
    assert!(parses(&on, "b!", false));
}

#[test]
fn a_production_with_a_member_that_consumes_nothing_is_not_a_class() {
    // doc = x ; x = C "b" ; C = <member> / "a". An empty literal matches
    // nothing, and #ZZ and #AA can be satisfied without input, while the
    // set standing for a class is one token and never empty.
    for member in [sens_term(""), tok("#ZZ"), tok("#AA")] {
        let g = || {
            vec![
                prod("doc", vec![vec![reference("x")]]),
                prod("x", vec![vec![reference("C"), sens_term("b")]]),
                prod("C", vec![vec![member.clone()], vec![sens_term("a")]]),
            ]
        };
        let on = emit(g(), true);
        assert!(
            token_set_names(&on).is_empty(),
            "{:?}",
            token_set_names(&on)
        );
        assert_eq!(alt_seqs(&on, "x"), alt_seqs(&emit(g(), false), "x"));
        assert!(parses(&on, "ab", false));
    }
}
