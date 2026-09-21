// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

// The limits of the overlapping-class partition, driven through the IR
// rather than a notation. Mirrors ts/test/class-partition.test.js and
// go/class_partition_test.go.
//
// These cases exist because the first cut of the partition got them
// wrong, and no front-end in the fleet can express them: ABNF's `%x`
// ranges are always single-code-point and case-sensitive, so its suite,
// the usual oracle for this compiler, cannot reach a multi-character
// regex terminal or a case-insensitive class at all. A notation-neutral
// compiler has to be safe for the front-ends that can.

mod common;

use common::{alt_marks, alt_seqs, emit_ir, match_sources, prod, rx, term, token_set_names};
use tabnas_bnf::ConvertOptions;

// patternCharRanges answers "what can this pattern's FIRST character
// be?": right for contest detection, wrong for deciding a class's full
// coverage. Partitioning REPLACES the matcher with one-character atoms,
// so a pattern that matches more than one code point would lose the rest
// of itself. Each of these three lost something real.
#[test]
fn partition_leaves_multi_char_regex_alone() {
    for (label, pattern) in [
        ("top-level alternation", "a|bc"),
        ("quantifier", "[a-z]+"),
        ("two classes in sequence", "[aA][bB]"),
    ] {
        // `[a]` overlaps the first character of every pattern above, so
        // without the guard each one becomes "contested" and is replaced.
        let spec = emit_ir(
            vec![prod(
                "top",
                vec![vec![rx(pattern, "")], vec![rx("[a]", "")]],
            )],
            None,
        );
        let sources = match_sources(&spec);
        assert!(
            sources.iter().any(|(_, src)| src.contains(pattern)),
            "{label}: {pattern:?} lost its matcher; emitted {sources:?}"
        );
        assert!(
            token_set_names(&spec).is_empty(),
            "{label}: a pattern that can match more than one code point must not be \
             partitioned; got sets {:?}",
            token_set_names(&spec)
        );
    }
}

// foldCaseRanges folds ASCII A-Z/a-z and nothing else, so atoms derived
// from `[é]/i` would cover `é` but not `É`: the matcher would say one
// thing and the ranges another.
#[test]
fn partition_leaves_case_insensitive_class_alone() {
    let spec = emit_ir(
        vec![prod(
            "top",
            vec![vec![rx("[\\u00e9]", "i")], vec![rx("[\\u00e9]", "")]],
        )],
        None,
    );
    let insensitive: Vec<(String, String)> = match_sources(&spec)
        .into_iter()
        .filter(|(_, src)| src.ends_with("/i"))
        .collect();
    assert_eq!(
        insensitive.len(),
        1,
        "the case-insensitive matcher must survive; got {:?}",
        match_sources(&spec)
    );
    // `@~/^[é]/i` on the wire; the pattern between the slashes must
    // still cover É under the engine's own regex dialect.
    let src = &insensitive[0].1;
    let body = src
        .strip_prefix("@~/")
        .and_then(|s| s.strip_suffix("/i"))
        .expect("an eager, case-insensitive matcher");
    let re = regex::RegexBuilder::new(body)
        .case_insensitive(true)
        .build()
        .expect("the matcher compiles");
    assert!(re.is_match("É"), "{} must still cover É", insensitive[0].0);
    assert!(
        token_set_names(&spec).is_empty(),
        "a case-insensitive class must not be partitioned; got {:?}",
        token_set_names(&spec)
    );
}

// The guard above must not have disarmed the fix itself.
#[test]
fn partition_still_partitions_plain_classes() {
    let spec = emit_ir(
        vec![prod(
            "top",
            vec![
                vec![rx("[\\u0030-\\u0039]", "")],
                vec![rx("[\\u0031-\\u0039]", "")],
            ],
        )],
        None,
    );
    let mut names = token_set_names(&spec);
    names.sort();
    assert_eq!(
        names,
        vec!["RX___U0030__U0039", "RX___U0031__U0039"],
        "expected a set per overlapping class"
    );
}

// Marks come from altDiscriminator, which reads the token name out of
// regexTokens. Pointing a one-atom class at the atom renamed it, so a
// user action bound to `@top:o:<mark>` silently detached the moment some
// OTHER production mentioned an overlapping class.
#[test]
fn partition_keeps_class_token_names() {
    let alts = || vec![vec![rx("[123456789]", ""), term("x")], vec![term("y")]];
    let opts = || ConvertOptions::tag("t").marks(true);

    let alone = emit_ir(vec![prod("top", alts())], Some(opts()));
    let contested = emit_ir(
        vec![
            prod("top", alts()),
            prod("other", vec![vec![rx("[0-9]", "")]]),
        ],
        Some(opts()),
    );

    assert_eq!(
        alt_marks(&contested, "top"),
        alt_marks(&alone, "top"),
        "an unrelated overlapping class moved this rule's marks"
    );
    assert_eq!(
        alt_seqs(&contested, "top"),
        alt_seqs(&alone, "top"),
        "token sequences moved"
    );
}

// An atom minted as `rx_<pattern>` collides with the natural name of any
// class spelling the same span: `%x31-39`'s atom took that name first
// and pushed the class to a suffixed one.
#[test]
fn partition_names_atoms_apart_from_classes() {
    let spec = emit_ir(
        vec![prod(
            "top",
            vec![
                vec![rx("[\\u0030-\\u0039]", "")],
                vec![rx("[\\u0031-\\u0039]", "")],
            ],
        )],
        None,
    );
    assert_eq!(
        alt_seqs(&spec, "top"),
        vec![
            Some("#RX___U0030__U0039".to_string()),
            Some("#RX___U0031__U0039".to_string())
        ],
        "both classes keep the name they would have had unpartitioned"
    );
    for (name, _) in match_sources(&spec) {
        assert!(
            name.starts_with("#RXA"),
            "{name} should be an atom token, not a class token"
        );
    }
    for name in token_set_names(&spec) {
        assert!(!name.starts_with("RXA"), "set {name} took an atom's name");
    }
}
