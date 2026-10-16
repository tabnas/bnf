//! A closure-mode probe flags its passes as the function-free mode's does.
//!
//! `RefAction::ProbeInit` and `ProbeDecide`, which `GrammarSpec::install`
//! binds, open and close the engine's probe (`Context::probe_open` and
//! `probe_close`), so the engine flags every pass made while the probe is
//! open (`RuleDone::probe`) exactly as it does for `@probeInit$` and
//! `@probeDecide$`, the builtins the function-free mode emits. A consumer
//! that builds a document's structure from `ruleDone`, such as transduce's
//! `ParserEventSource`, drops a flagged pass; without the calls the
//! closure mode's speculative passes reached it as the document's.

mod common;

use common::{emit_ir, group, opt, prod, reference, rx, term};
use std::sync::{Arc, Mutex};
use tabnas::{RuleState, Tabnas};
use tabnas_bnf::{to_pure_spec, ConvertOptions, Production};

/// `top = [ X "@" ] Y`, `X = 1*ALPHA`, `Y = 1*ALPHA`: the optional prefix
/// shares its vocabulary with the tail, so the compiler emits a probe. It
/// is the grammar of the engine's own `test/spec/probe.tsv`.
fn probing() -> Vec<Production> {
    vec![
        prod(
            "top",
            vec![vec![
                opt(group(vec![vec![reference("X"), term("@")]])),
                reference("Y"),
            ]],
        ),
        prod("X", vec![vec![rx("[a-z]+", "")]]),
        prod("Y", vec![vec![rx("[a-z]+", "")]]),
    ]
}

/// The passes flagged as a probe's, as `rule:state` sorted, and the count
/// of passes left unflagged.
fn flagged(mut parser: Tabnas, input: &str) -> (Vec<String>, usize) {
    let passes: Arc<Mutex<(Vec<String>, usize)>> = Arc::default();
    let seen = passes.clone();
    parser.subscribe_rule_done(move |rule, _context, done| {
        let mut seen = seen.lock().unwrap();
        if done.probe {
            let state = match done.state {
                RuleState::Open => "open",
                RuleState::Close => "close",
            };
            seen.0.push(format!("{}:{state}", rule.name.as_str()));
        } else {
            seen.1 += 1;
        }
    });
    parser
        .parse(input)
        .unwrap_or_else(|e| panic!("{input:?} parses: {e:?}"));
    let (mut probe, kept) = passes.lock().unwrap().clone();
    probe.sort();
    (probe, kept)
}

/// The grammar in closure mode: the probe's actions are registered
/// closures, bound by `install`.
fn closure_parser() -> Tabnas {
    let spec = emit_ir(probing(), Some(ConvertOptions::tag("tst").start("top")));
    assert!(
        spec.has_closures(),
        "the closure mode registers the probe's actions"
    );
    let mut parser = Tabnas::new();
    spec.install(&mut parser).expect("install");
    parser
}

/// The same grammar function-free: the probe is the engine's builtins,
/// and the spec installs as pure data.
fn builtin_parser() -> Tabnas {
    let spec = emit_ir(
        probing(),
        Some(ConvertOptions::tag("tst").start("top").builtins(true)),
    );
    let pure = to_pure_spec(&spec).expect("pure");
    let mut parser = Tabnas::new();
    parser
        .grammar_json(&pure.to_string())
        .expect("the pure spec installs");
    parser
}

#[test]
fn a_closure_mode_probe_flags_its_passes_as_the_builtins_do() {
    // With the prefix, without it, and with a look-ahead longer than the
    // prefix: the engine's fixture's three inputs.
    for input in ["ab@cd", "cd", "abcdef"] {
        let closures = flagged(closure_parser(), input);
        let builtins = flagged(builtin_parser(), input);
        assert_eq!(closures, builtins, "{input:?}: the two modes flag alike");
        assert!(
            !closures.0.is_empty(),
            "{input:?}: the probe's passes are flagged"
        );
        assert!(closures.1 > 0, "{input:?}: the document's passes are not");
    }
}
