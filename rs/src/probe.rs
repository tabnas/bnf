// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

//! The probe-dispatch analyser and rewriter. Mirrors `isProbeableOpt`,
//! `collectTerminalVocabElements` and `rewriteProbeDispatches` in
//! `ts/src/compiler.ts`; the emitters for the productions it synthesises
//! live beside the rest of the emitter.
//!
//! A large family of grammars are not LL(k) for any bounded k. The
//! canonical example is RFC 3986's `authority = [ userinfo "@" ] host`,
//! where `userinfo` and `reg-name` share a character vocabulary, so a
//! FIRST-set dispatcher cannot decide which branch the optional belongs
//! to. For the pattern `[X D] Y`, an optional group whose body ends with
//! a terminal D, followed by a sequence Y whose leading terminals overlap
//! with X's, the rule is rewritten to a probe plus phase-retry
//! dispatcher: mark the token position and push a failure-proof probe
//! that greedily consumes the joint vocabulary; on return, peek the next
//! token, rewind, and commit to the `X D Y` or the `Y` branch.

use indexmap::{IndexMap, IndexSet};

use crate::ir::{
    diag_name, origin_of, regex_key, term_key_of, AmbiguityReport, Element, EmitError, Grammar,
    Kind, ProbeDispatchSpec, ProbeHelperSpec, Production, Sequence,
};

/// Element is `[ X D ]` where X is one or more elements and D is a
/// terminal: returns X and D.
fn is_probeable_opt(el: &Element) -> Option<(Sequence, Element)> {
    let Kind::Opt { inner } = &el.kind else {
        return None;
    };
    let Kind::Group { alts } = &inner.kind else {
        return None;
    };
    if alts.len() != 1 {
        return None;
    }
    let seq = &alts[0];
    if seq.len() < 2 {
        return None;
    }
    let last = &seq[seq.len() - 1];
    if !last.is_terminal() {
        return None;
    }
    Some((seq[..seq.len() - 1].to_vec(), last.clone()))
}

/// The key a terminal is collected under.
fn vocab_key(el: &Element) -> Option<String> {
    match &el.kind {
        Kind::Term { .. } => Some(term_key_of(el)),
        Kind::Regex { pattern, flags } => Some(regex_key(pattern, flags)),
        Kind::Token { name } => Some(name.clone()),
        _ => None,
    }
}

/// Union of every terminal reachable by walking an element's subtree,
/// following refs transitively. Cycles are broken by the visited set.
fn collect_terminal_vocab_elements(
    el: &Element,
    grammar: &Grammar,
    out: &mut IndexMap<String, Element>,
    visited: &mut IndexSet<String>,
) {
    match &el.kind {
        Kind::Term { .. } | Kind::Regex { .. } | Kind::Token { .. } => {
            let key = vocab_key(el).expect("a terminal has a vocab key");
            out.entry(key).or_insert_with(|| el.clone());
        }
        Kind::Ref { name, .. } => {
            if visited.contains(name) {
                return;
            }
            visited.insert(name.clone());
            let Some(prod) = grammar.find(name) else {
                return;
            };
            for alt in &prod.alts {
                for sub in alt {
                    collect_terminal_vocab_elements(sub, grammar, out, visited);
                }
            }
        }
        Kind::Opt { inner }
        | Kind::Star { inner, .. }
        | Kind::Plus { inner }
        | Kind::Rep { inner, .. } => collect_terminal_vocab_elements(inner, grammar, out, visited),
        Kind::Group { alts } => {
            for alt in alts {
                for sub in alt {
                    collect_terminal_vocab_elements(sub, grammar, out, visited);
                }
            }
        }
        Kind::Prose { .. } => {}
    }
}

fn collect_seq_vocab_elements(seq: &[Element], grammar: &Grammar) -> IndexMap<String, Element> {
    let mut out = IndexMap::new();
    let mut visited = IndexSet::new();
    for el in seq {
        collect_terminal_vocab_elements(el, grammar, &mut out, &mut visited);
    }
    out
}

/// Rewrite every ambiguous `[X D] Y` subsequence into a probe-dispatch
/// pattern. The grammar still has its sugar at this point, which is
/// where the pattern is easy to recognise, and runs before token
/// allocation: probe metadata stores elements, and the emitter resolves
/// them to token names at emit time.
pub(crate) fn rewrite_probe_dispatches(grammar: &Grammar) -> Result<Grammar, EmitError> {
    let mut reports: Vec<AmbiguityReport> = grammar.ambiguities.clone();
    let mut extra: Vec<Production> = Vec::new();
    let mut used: IndexSet<String> = grammar.productions.iter().map(|p| p.name.clone()).collect();

    let fresh_name = |hint: &str, used: &mut IndexSet<String>| -> String {
        let mut name = hint.to_string();
        let mut i = 1;
        while used.contains(&name) {
            name = format!("{hint}{i}");
            i += 1;
        }
        used.insert(name.clone());
        name
    };

    let mut rewritten: Vec<Production> = Vec::new();

    for prod in &grammar.productions {
        let mut new_alts: Vec<Sequence> = Vec::new();
        let mut touched = false;
        for (alt_idx, alt) in prod.alts.iter().enumerate() {
            let mut result_alt: Sequence = Vec::new();
            let mut i = 0;
            while i < alt.len() {
                let el = &alt[i];
                let Some((x_seq, disambiguator)) = is_probeable_opt(el) else {
                    result_alt.push(el.clone());
                    i += 1;
                    continue;
                };
                let y_seq: Sequence = alt[i + 1..].to_vec();
                if y_seq.is_empty() {
                    // `[X D]` is the last thing in the alt: nothing to
                    // disambiguate against.
                    result_alt.push(el.clone());
                    i += 1;
                    continue;
                }
                let x_vocab = collect_seq_vocab_elements(&x_seq, grammar);
                let y_vocab = collect_seq_vocab_elements(&y_seq, grammar);
                if !x_vocab.keys().any(|k| y_vocab.contains_key(k)) {
                    // The normal FIRST-based dispatcher can decide.
                    result_alt.push(el.clone());
                    i += 1;
                    continue;
                }

                // Joint vocab: union of everything the probe might need
                // to consume, minus the disambiguator so the probe stops
                // on it and the peek works.
                let mut vocab: IndexMap<String, Element> = x_vocab;
                for (k, v) in y_vocab {
                    vocab.entry(k).or_insert(v);
                }
                if let Some(d_key) = vocab_key(&disambiguator) {
                    vocab.shift_remove(&d_key);
                }

                let dispatch_name = fresh_name(&format!("{}$pd{}", prod.name, i), &mut used);
                let probe_name = fresh_name(&format!("{dispatch_name}$probe"), &mut used);
                let with_name = fresh_name(&format!("{dispatch_name}$with"), &mut used);
                let no_name = fresh_name(&format!("{dispatch_name}$no"), &mut used);

                let origin = origin_of(prod);
                // The probe helper.
                let mut probe = Production::helper(&probe_name, Vec::new(), origin);
                probe.probe_helper = Some(ProbeHelperSpec {
                    vocab_elements: vocab.into_values().collect(),
                });
                extra.push(probe);
                // The committed branches: `with` = X D Y, `no` = Y.
                let mut with_alt: Sequence = x_seq.clone();
                with_alt.push(disambiguator.clone());
                with_alt.extend(y_seq.iter().cloned());
                extra.push(Production::helper(&with_name, vec![with_alt], origin));
                extra.push(Production::helper(&no_name, vec![y_seq.clone()], origin));
                // The dispatcher. Its `alts` are a virtual spec, two
                // ref-only alts, that exists solely to feed the FIRST-set
                // computation; the emitter checks `probe_dispatch` first.
                let mut dispatch = Production::helper(
                    &dispatch_name,
                    vec![
                        vec![Element::reference(with_name.clone())],
                        vec![Element::reference(no_name.clone())],
                    ],
                    origin,
                );
                dispatch.probe_dispatch = Some(ProbeDispatchSpec {
                    probe_rule: probe_name,
                    disambiguator,
                    with_branch: with_name,
                    no_branch: no_name,
                });
                extra.push(dispatch);

                reports.push(AmbiguityReport {
                    rule: prod.name.clone(),
                    alt_idx,
                    opt_idx: i,
                    reason: "optional prefix shares vocabulary with tail".into(),
                    resolved: true,
                });

                // The dispatcher swallows the optional AND everything
                // after it, so one reference now covers two regions the
                // author drew a boundary between. A member count still
                // matches, which is why this cannot be caught downstream.
                if prod.value.is_some() {
                    return Err(EmitError::at(
                        format!(
                            "{}: rule '{}' has a value annotation, but its optional prefix \
                             shares vocabulary with what follows it, so the two are \
                             compiled into one dispatch helper — a member cannot cover the \
                             optional alone any more. Give the optional its own rule and \
                             annotate that, or make the prefix and the tail start with \
                             different tokens.",
                            diag_name(),
                            origin
                        ),
                        origin,
                        prod.sp,
                    ));
                }
                result_alt.push(Element::reference(dispatch_name));
                // Everything that followed the opt is now inside the
                // dispatcher, so skip the rest of the alt.
                i = alt.len();
                touched = true;
            }
            new_alts.push(result_alt);
        }
        if touched {
            rewritten.push(prod.rebuilt(new_alts));
        } else {
            rewritten.push(prod.clone());
        }
    }

    rewritten.extend(extra);
    Ok(Grammar {
        productions: rewritten,
        ambiguities: reports,
        ..Default::default()
    })
}
