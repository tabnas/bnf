// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

//! Token-level analysis over the desugared grammar: FIRST sets and
//! nullability, FOLLOW and FOLLOW₂, suffix debts for contested
//! left-recursion tail loops, and the K-token prefix enumeration the
//! dispatcher fans out to. Mirrors `computeFirstSets`, `firstOfAlt`,
//! `firstOfSeq`, `computeFollowSets`, `computeFollowPairs`,
//! `resolveSuffixDebts`, `refCallersOf`, `altPrefixesRaw` and
//! `altPrefixes` in `ts/src/compiler.ts`.

use indexmap::{IndexMap, IndexSet};

use crate::ir::{diag_name, refs_in, regex_key, term_key_of, Element, Grammar, Kind, Sequence};

/// The allocated token tables: literal key -> token name and regex key
/// -> token name.
#[derive(Debug, Default)]
pub(crate) struct Tokens {
    pub literals: IndexMap<String, String>,
    pub regex_tokens: IndexMap<String, String>,
}

impl Tokens {
    /// The allocated token name of a terminal element, if it has one.
    pub fn of(&self, el: &Element) -> Option<String> {
        match &el.kind {
            Kind::Term { .. } => self.literals.get(&term_key_of(el)).cloned(),
            Kind::Regex { pattern, flags } => {
                self.regex_tokens.get(&regex_key(pattern, flags)).cloned()
            }
            Kind::Token { name } => Some(name.clone()),
            _ => None,
        }
    }

    /// The token name of a terminal the allocator has already seen. A
    /// terminal every pass walked cannot be missing; TypeScript reads it
    /// with a bare cast and would emit `undefined` there.
    pub fn name(&self, el: &Element) -> String {
        self.of(el).unwrap_or_default()
    }
}

pub(crate) type FirstSets = IndexMap<String, IndexSet<String>>;
pub(crate) type Nullable = IndexSet<String>;
pub(crate) type FollowSets = IndexMap<String, IndexSet<String>>;
pub(crate) type FollowPairs = IndexMap<String, IndexMap<String, IndexSet<String>>>;

/// FIRST(ref) for every production, plus which productions are nullable.
/// Iterates to a fixed point; terminals are represented by their
/// allocated token names.
pub(crate) fn compute_first_sets(grammar: &Grammar, tokens: &Tokens) -> (FirstSets, Nullable) {
    let mut first_sets: FirstSets = grammar
        .productions
        .iter()
        .map(|p| (p.name.clone(), IndexSet::new()))
        .collect();
    let mut nullable: Nullable = IndexSet::new();

    let mut changed = true;
    while changed {
        changed = false;
        for prod in &grammar.productions {
            for alt in &prod.alts {
                let mut alt_nullable = true;
                for el in alt {
                    match &el.kind {
                        Kind::Term { .. } | Kind::Regex { .. } | Kind::Token { .. } => {
                            let tok = tokens.name(el);
                            if first_sets[&prod.name].insert(tok) {
                                changed = true;
                            }
                            alt_nullable = false;
                            break;
                        }
                        Kind::Ref { name, .. } => {
                            let ref_first: Vec<String> = first_sets
                                .get(name)
                                .map(|s| s.iter().cloned().collect())
                                .unwrap_or_default();
                            for tok in ref_first {
                                if first_sets[&prod.name].insert(tok) {
                                    changed = true;
                                }
                            }
                            if !nullable.contains(name) {
                                alt_nullable = false;
                                break;
                            }
                        }
                        _ => panic!(
                            "{}: internal — unexpected kind in FIRST: {}",
                            diag_name(),
                            kind_name(el)
                        ),
                    }
                }
                if alt_nullable && nullable.insert(prod.name.clone()) {
                    changed = true;
                }
            }
        }
    }
    (first_sets, nullable)
}

pub(crate) fn kind_name(el: &Element) -> &'static str {
    match el.kind {
        Kind::Term { .. } => "term",
        Kind::Ref { .. } => "ref",
        Kind::Token { .. } => "token",
        Kind::Prose { .. } => "prose",
        Kind::Regex { .. } => "regex",
        Kind::Opt { .. } => "opt",
        Kind::Star { .. } => "star",
        Kind::Plus { .. } => "plus",
        Kind::Rep { .. } => "rep",
        Kind::Group { .. } => "group",
    }
}

/// FIRST set for one alternative, or `None` if the alternative is
/// nullable (the caller treats that case separately).
pub(crate) fn first_of_alt(
    alt: &[Element],
    tokens: &Tokens,
    first_sets: &FirstSets,
    nullable: &Nullable,
) -> Option<IndexSet<String>> {
    let (out, is_nullable) = first_of_seq(alt, tokens, first_sets, nullable);
    if is_nullable {
        None
    } else {
        Some(out)
    }
}

/// FIRST of a sequence, reporting nullability separately: the tokens a
/// suffix can start with, AND whether that suffix can vanish.
pub(crate) fn first_of_seq(
    seq: &[Element],
    tokens: &Tokens,
    first_sets: &FirstSets,
    nullable: &Nullable,
) -> (IndexSet<String>, bool) {
    let mut out: IndexSet<String> = IndexSet::new();
    for el in seq {
        match &el.kind {
            Kind::Term { .. } | Kind::Regex { .. } | Kind::Token { .. } => {
                out.insert(tokens.name(el));
                return (out, false);
            }
            Kind::Ref { name, .. } => {
                if let Some(rf) = first_sets.get(name) {
                    out.extend(rf.iter().cloned());
                }
                if !nullable.contains(name) {
                    return (out, false);
                }
            }
            _ => panic!(
                "{}: internal — unexpected kind in firstOfSeq: {}",
                diag_name(),
                kind_name(el)
            ),
        }
    }
    (out, true)
}

/// FOLLOW sets: the tokens that may legitimately appear immediately
/// after each production. The engine lexes under the direction of the
/// active rule, so a repetition helper's empty terminating alternative
/// has to NAME the tokens that may follow it, or the lexer never offers
/// them there.
pub(crate) fn compute_follow_sets(
    grammar: &Grammar,
    tokens: &Tokens,
    first_sets: &FirstSets,
    nullable: &Nullable,
    start: &str,
) -> FollowSets {
    let mut follow: FollowSets = grammar
        .productions
        .iter()
        .map(|p| (p.name.clone(), IndexSet::new()))
        .collect();
    // End-of-source can follow the start rule.
    if let Some(f) = follow.get_mut(start) {
        f.insert("#ZZ".to_string());
    }

    let mut changed = true;
    while changed {
        changed = false;
        for prod in &grammar.productions {
            for alt in &prod.alts {
                for (i, el) in alt.iter().enumerate() {
                    let Kind::Ref { name, .. } = &el.kind else {
                        continue;
                    };
                    if !follow.contains_key(name) {
                        continue;
                    }
                    let (rest, rest_nullable) =
                        first_of_seq(&alt[i + 1..], tokens, first_sets, nullable);
                    let mut add: Vec<String> = rest.into_iter().collect();
                    // Nothing (or nothing mandatory) follows this reference
                    // inside the alternative, so whatever can follow the
                    // enclosing production can follow the reference too.
                    if rest_nullable {
                        add.extend(follow[&prod.name].iter().cloned());
                    }
                    let target = follow.get_mut(name).expect("checked above");
                    for tok in add {
                        if target.insert(tok) {
                            changed = true;
                        }
                    }
                }
            }
        }
    }
    follow
}

/// FOLLOW₂ pairs: for each production R, the pairs (t, u) such that R
/// may be followed by token t and then token u. Deliberately
/// approximate in one direction: pairs whose t would come from inside a
/// following REFERENCE are not collected.
pub(crate) fn compute_follow_pairs(
    grammar: &Grammar,
    tokens: &Tokens,
    first_sets: &FirstSets,
    nullable: &Nullable,
    follow: &FollowSets,
) -> FollowPairs {
    let mut pairs: FollowPairs = grammar
        .productions
        .iter()
        .map(|p| (p.name.clone(), IndexMap::new()))
        .collect();

    fn add_pair(pairs: &mut FollowPairs, r: &str, t: &str, u: &str) -> bool {
        let Some(m) = pairs.get_mut(r) else {
            return false;
        };
        m.entry(t.to_string()).or_default().insert(u.to_string())
    }

    let mut changed = true;
    while changed {
        changed = false;
        for prod in &grammar.productions {
            let prod_follow: Vec<String> = follow
                .get(&prod.name)
                .map(|s| s.iter().cloned().collect())
                .unwrap_or_default();
            for alt in &prod.alts {
                for (i, el) in alt.iter().enumerate() {
                    let Kind::Ref { name, .. } = &el.kind else {
                        continue;
                    };
                    if !pairs.contains_key(name) {
                        continue;
                    }

                    let mut j = i + 1;
                    let mut blocked = false;
                    while j < alt.len() {
                        let ej = &alt[j];
                        if let Kind::Ref { name: nj, .. } = &ej.kind {
                            // A following reference blocks the walk unless
                            // it can vanish.
                            if nullable.contains(nj) {
                                j += 1;
                                continue;
                            }
                            blocked = true;
                            break;
                        }
                        let Some(t) = tokens.of(ej) else {
                            blocked = true;
                            break;
                        };
                        let (rest, rest_nullable) =
                            first_of_seq(&alt[j + 1..], tokens, first_sets, nullable);
                        for u in &rest {
                            if add_pair(&mut pairs, name, &t, u) {
                                changed = true;
                            }
                        }
                        if rest_nullable {
                            for u in &prod_follow {
                                if add_pair(&mut pairs, name, &t, u) {
                                    changed = true;
                                }
                            }
                        }
                        blocked = true;
                        break;
                    }
                    if !blocked {
                        // Nothing mandatory follows the reference, so the
                        // enclosing production's pairs follow it too.
                        let prod_pairs: Vec<(String, Vec<String>)> = pairs[&prod.name]
                            .iter()
                            .map(|(t, us)| (t.clone(), us.iter().cloned().collect()))
                            .collect();
                        for (t, us) in prod_pairs {
                            for u in us {
                                if add_pair(&mut pairs, name, &t, &u) {
                                    changed = true;
                                }
                            }
                        }
                    }
                }
            }
        }
    }
    pairs
}

/// Settle the contested left-recursion tail loops flagged during
/// elimination: confirm the competition against real FIRST sets, record
/// the loop tokens an enclosing suffix owes, and annotate every push
/// that can reach the recursive rule with its counter mutation. Runs on
/// the desugared grammar, and only annotates.
///
/// For each contested loop, on every push that can reach the recursive
/// rule: a suffix that is empty or can derive ε inherits the ancestor's
/// debt; a mandatory suffix whose FIRST hits the loop adds one; a
/// mandatory suffix whose FIRST is disjoint resets to zero, a barrier.
/// The loop branches that could eat what is owed are then guarded with
/// `n.<counter> == 0`. Nothing is emitted unless some push actually owes
/// the loop's token.
pub(crate) fn resolve_suffix_debts(
    grammar: &mut Grammar,
    tokens: &Tokens,
    first_sets: &FirstSets,
    nullable: &Nullable,
    tokens_overlap: &dyn Fn(&str, &str) -> bool,
) {
    let guarded: Vec<usize> = grammar
        .productions
        .iter()
        .enumerate()
        .filter(|(_, p)| p.debt_guard.is_some())
        .map(|(i, _)| i)
        .collect();
    if guarded.is_empty() {
        return;
    }

    for loop_idx in guarded {
        let loop_name = grammar.productions[loop_idx].name.clone();
        let counter = grammar.productions[loop_idx]
            .debt_guard
            .clone()
            .expect("filtered on debt_guard");

        // The recursive rule is whatever references the loop helper:
        // there is exactly one candidate.
        let owner = grammar.productions.iter().enumerate().find(|(i, p)| {
            *i != loop_idx
                && p.alts
                    .iter()
                    .any(|alt| alt.iter().any(|el| el.is_ref_to(&loop_name)))
        });

        // FIRST of the helper is FIRST of what it repeats.
        let loop_first: IndexSet<String> = first_sets.get(&loop_name).cloned().unwrap_or_default();
        let Some((_, owner)) = owner else {
            grammar.productions[loop_idx].debt_guard = None;
            continue;
        };
        if loop_first.is_empty() {
            grammar.productions[loop_idx].debt_guard = None;
            continue;
        }

        // Only a push into something that can still reach the recursive
        // rule can end up inside the contested loop.
        let carries = ref_callers_of(grammar, &owner.name);

        let mut pending: Vec<(usize, usize, usize, i64)> = Vec::new();
        let mut owed: IndexSet<String> = IndexSet::new();
        for (pi, prod) in grammar.productions.iter().enumerate() {
            for (ai, alt) in prod.alts.iter().enumerate() {
                for (i, el) in alt.iter().enumerate() {
                    let Kind::Ref { name, .. } = &el.kind else {
                        continue;
                    };
                    if !carries.contains(name) {
                        continue;
                    }
                    let suffix = &alt[i + 1..];
                    if suffix.is_empty() {
                        continue;
                    }
                    let (f_tokens, f_nullable) = first_of_seq(suffix, tokens, first_sets, nullable);
                    // A suffix that can vanish commits the frame to
                    // nothing, so the push stays in tail position and
                    // inherits.
                    if f_nullable {
                        continue;
                    }
                    let mut hits = false;
                    for u in &f_tokens {
                        for t in &loop_first {
                            if t == u || tokens_overlap(t, u) {
                                owed.insert(t.clone());
                                hits = true;
                            }
                        }
                    }
                    pending.push((pi, ai, i, if hits { 1 } else { 0 }));
                }
            }
        }

        if owed.is_empty() {
            // Nothing anywhere competes with this loop: the shape matched
            // syntactically but the tokens never collide.
            grammar.productions[loop_idx].debt_guard = None;
            continue;
        }
        grammar.productions[loop_idx].debt_owed = Some(owed.into_iter().collect());

        for (pi, ai, i, delta) in pending {
            let el = &mut grammar.productions[pi].alts[ai][i];
            if let Kind::Ref { debt, .. } = &mut el.kind {
                let mut d = debt.clone().unwrap_or_default();
                d.insert(counter.clone(), delta);
                *debt = Some(d);
            }
        }
    }
}

/// Names of the productions from which `target` can be reached through
/// rule references, `target` itself included. A backward walk.
fn ref_callers_of(grammar: &Grammar, target: &str) -> IndexSet<String> {
    let mut rev: IndexMap<String, Vec<String>> = IndexMap::new();
    for p in &grammar.productions {
        let mut out: IndexSet<String> = IndexSet::new();
        for alt in &p.alts {
            refs_in(alt, &mut out);
        }
        // A dispatcher's branches live outside `alts`, but a push into
        // one is still a push.
        if let Some(pd) = &p.probe_dispatch {
            out.insert(pd.probe_rule.clone());
            out.insert(pd.with_branch.clone());
            out.insert(pd.no_branch.clone());
        }
        for to in out {
            rev.entry(to).or_default().push(p.name.clone());
        }
    }

    let mut seen: IndexSet<String> = IndexSet::new();
    seen.insert(target.to_string());
    let mut queue = vec![target.to_string()];
    while let Some(next) = queue.pop() {
        if let Some(from) = rev.get(&next) {
            for f in from {
                if seen.insert(f.clone()) {
                    queue.push(f.clone());
                }
            }
        }
    }
    seen
}

#[derive(Debug, Clone)]
pub(crate) struct PrefixPath {
    pub tokens: Vec<String>,
    pub done: bool,
}

/// Enumerate concrete token-sequence prefixes an alternative can start
/// with, each at most `max_k` tokens long. Refs with multiple
/// alternatives fan out into one prefix per sub-alternative. When a ref
/// cycles back or exhausts depth, the path is TERMINATED at the tokens
/// accumulated so far, and `done` is propagated out so a truncated
/// sub-prefix is never extended.
pub(crate) fn alt_prefixes_raw(
    alt: &[Element],
    grammar: &Grammar,
    tokens: &Tokens,
    max_k: usize,
    visited: &IndexSet<String>,
) -> Vec<PrefixPath> {
    let mut paths: Vec<PrefixPath> = vec![PrefixPath {
        tokens: Vec::new(),
        done: false,
    }];

    for el in alt {
        let mut next: Vec<PrefixPath> = Vec::new();
        for p in &paths {
            if p.done || p.tokens.len() >= max_k {
                next.push(p.clone());
                continue;
            }
            match &el.kind {
                Kind::Term { .. } | Kind::Regex { .. } | Kind::Token { .. } => {
                    let mut t = p.tokens.clone();
                    t.push(tokens.name(el));
                    next.push(PrefixPath {
                        tokens: t,
                        done: false,
                    });
                }
                Kind::Ref { name, .. } => {
                    if visited.contains(name) {
                        next.push(PrefixPath {
                            tokens: p.tokens.clone(),
                            done: true,
                        });
                        continue;
                    }
                    let mut child_visited = visited.clone();
                    child_visited.insert(name.clone());
                    let target = grammar.find(name);
                    let Some(target) = target.filter(|t| !t.alts.is_empty()) else {
                        next.push(PrefixPath {
                            tokens: p.tokens.clone(),
                            done: true,
                        });
                        continue;
                    };
                    for sub in &target.alts {
                        let sub_paths = alt_prefixes_raw(
                            sub,
                            grammar,
                            tokens,
                            max_k - p.tokens.len(),
                            &child_visited,
                        );
                        for sp in sub_paths {
                            let mut t = p.tokens.clone();
                            t.extend(sp.tokens);
                            next.push(PrefixPath {
                                tokens: t,
                                done: sp.done,
                            });
                        }
                    }
                }
                _ => {
                    // Desugaring has eliminated the sugar kinds.
                    next.push(PrefixPath {
                        tokens: p.tokens.clone(),
                        done: true,
                    });
                }
            }
        }
        paths = next;
        if paths.iter().all(|p| p.done || p.tokens.len() >= max_k) {
            break;
        }
    }
    paths
}

/// The distinct token prefixes of an alternative, in first-seen order.
pub(crate) fn alt_prefixes(
    alt: &[Element],
    grammar: &Grammar,
    tokens: &Tokens,
    max_k: usize,
) -> Vec<Vec<String>> {
    let raw = alt_prefixes_raw(alt, grammar, tokens, max_k, &IndexSet::new());
    let mut seen: IndexSet<String> = IndexSet::new();
    let mut out: Vec<Vec<String>> = Vec::new();
    for p in raw {
        let key = p.tokens.join(" ");
        if seen.insert(key) {
            out.push(p.tokens);
        }
    }
    out
}

/// A sequence with every element a terminal or reference: what the
/// emitter expects after desugaring.
pub(crate) fn is_single_segment(alt: &Sequence) -> bool {
    let mut saw_ref = false;
    for el in alt {
        match &el.kind {
            Kind::Ref { .. } => {
                if saw_ref {
                    return false;
                }
                saw_ref = true;
            }
            Kind::Term { .. } | Kind::Regex { .. } | Kind::Token { .. } => {
                if saw_ref {
                    return false; // terminal after a ref: multi-segment
                }
            }
            _ => return false,
        }
    }
    true
}
