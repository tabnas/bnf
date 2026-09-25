// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

//! Token-level analysis over the desugared grammar: FIRST sets and
//! nullability, FOLLOW and FOLLOW₂, suffix debts for contested
//! left-recursion tail loops, and the K-token prefix enumeration the
//! dispatcher fans out to. Mirrors `computeFirstSets`, `firstOfAlt`,
//! `firstOfSeq`, `computeFollowSets`, `computeFollowPairs`,
//! `resolveSuffixDebts`, `refCallersOf`, `altPrefixesRaw` and
//! `altPrefixes` in `ts/src/compiler.ts`.

use indexmap::{IndexMap, IndexSet};

use crate::ir::{
    diag_name, refs_in, regex_key, term_key_of, tokens_in, Element, Grammar, Kind, Sequence,
};

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
/// `class_sets` maps a token-class production to the token set it
/// compiles to (`ConvertOptions::token_classes`): FIRST of such a
/// production is its set, one name, wherever it is referenced.
pub(crate) fn compute_first_sets(
    grammar: &Grammar,
    tokens: &Tokens,
    class_sets: &IndexMap<String, String>,
) -> (FirstSets, Nullable) {
    let mut first_sets: FirstSets = grammar
        .productions
        .iter()
        .map(|p| {
            let mut first = IndexSet::new();
            if let Some(set) = class_sets.get(&p.name) {
                first.insert(set.clone());
            }
            (p.name.clone(), first)
        })
        .collect();
    let mut nullable: Nullable = IndexSet::new();

    let mut changed = true;
    while changed {
        changed = false;
        for prod in &grammar.productions {
            if class_sets.contains_key(&prod.name) {
                continue;
            }
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
/// `head_filter`, when given, keeps only the paths whose FIRST token it
/// admits: the dispatcher deepens the lookahead under a contested head
/// alone, and enumerating every path to depth K only to discard the rest
/// is the cost tabnas/bnf#71 measured in seconds. `class_sets` maps a
/// token-class production to its set: a reference to one is one token,
/// its set, rather than one path per member.
pub(crate) fn alt_prefixes_raw(
    alt: &[Element],
    grammar: &Grammar,
    tokens: &Tokens,
    max_k: usize,
    visited: &IndexSet<String>,
    head_filter: Option<&dyn Fn(&str) -> bool>,
    class_sets: Option<&IndexMap<String, String>>,
) -> Vec<PrefixPath> {
    let mut paths: Vec<PrefixPath> = vec![PrefixPath {
        tokens: Vec::new(),
        done: false,
    }];

    // A path that already carries a token has had its head admitted; only
    // a path still at length zero can be pruned by what it gains next.
    let admit = |before: &PrefixPath, tokens: &[String]| -> bool {
        match head_filter {
            None => true,
            Some(f) => !before.tokens.is_empty() || tokens.is_empty() || f(&tokens[0]),
        }
    };

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
                    if admit(p, &t) {
                        next.push(PrefixPath {
                            tokens: t,
                            done: false,
                        });
                    }
                }
                Kind::Ref { name, .. } => {
                    if let Some(set) = class_sets.and_then(|c| c.get(name)) {
                        let mut t = p.tokens.clone();
                        t.push(set.clone());
                        if admit(p, &t) {
                            next.push(PrefixPath {
                                tokens: t,
                                done: false,
                            });
                        }
                        continue;
                    }
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
                    // The head of the outer path is the head of the sub-path
                    // while nothing has been consumed yet.
                    let sub_filter = if p.tokens.is_empty() {
                        head_filter
                    } else {
                        None
                    };
                    for sub in &target.alts {
                        let sub_paths = alt_prefixes_raw(
                            sub,
                            grammar,
                            tokens,
                            max_k - p.tokens.len(),
                            &child_visited,
                            sub_filter,
                            class_sets,
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
    class_sets: Option<&IndexMap<String, String>>,
) -> Vec<Vec<String>> {
    let raw = alt_prefixes_raw(
        alt,
        grammar,
        tokens,
        max_k,
        &IndexSet::new(),
        None,
        class_sets,
    );
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

/// The token classes of a grammar (`ConvertOptions::token_classes`): every
/// alternative one literal or engine token, at least two of them. A
/// character class (`regex`) is not a member: those are laid over the
/// partition the class analysis builds, and a set over them is that
/// machinery's to mint. Mirrors the TypeScript `tokenClassNames`.
pub(crate) fn token_class_names(grammar: &Grammar) -> IndexSet<String> {
    let mut out = IndexSet::new();
    for prod in &grammar.productions {
        if prod.probe_helper.is_some()
            || prod.probe_dispatch.is_some()
            || prod.tail_repeat.is_some()
        {
            continue;
        }
        if prod.value.is_some() || prod.alts.len() < 2 {
            continue;
        }
        // The class's set is named after it (`#ident`), and a reference
        // the substitution pass consumes as that token has to resolve to
        // the set: a production named like an engine token (`TX`, `ZZ`)
        // cannot take its own name, and an empty name has none to take
        // (`alloc_token_name` names the set after its content instead),
        // so neither is a class.
        if prod.name.is_empty() || crate::emit::is_engine_owned_token(&format!("#{}", prod.name)) {
            continue;
        }
        let all = prod.alts.iter().all(|alt| {
            alt.len() == 1 && matches!(alt[0].kind, Kind::Term { .. } | Kind::Token { .. })
        });
        if all {
            out.insert(prod.name.clone());
        }
    }
    // A token the grammar spells under a class's set name would be read
    // as the set, and a class with its own set, or another class's, among
    // its members would be a set of sets, which the engine cannot resolve
    // and whose expansion never ends (`C = #C / "a"`). Such a class stays
    // a plain production, as it is with the option off.
    if !out.is_empty() {
        let mut spelled: IndexSet<String> = IndexSet::new();
        for prod in &grammar.productions {
            for alt in &prod.alts {
                tokens_in(alt, &mut spelled);
            }
        }
        out.retain(|name| !spelled.contains(&format!("#{name}")));
    }
    out
}

/// One entry per alternative of a choice: the prefixes to dispatch it
/// on, and whether it can derive ε.
pub(crate) struct DispatchPrefixes {
    pub prefixes: Vec<Vec<String>>,
    pub nullable: bool,
}

/// The dispatch prefixes of a choice, using the least lookahead each
/// decision needs (tabnas/bnf#71). Mirrors the TypeScript
/// `dispatchPrefixes`.
///
/// Every alternative is dispatched on its first tokens alone, and the
/// lookahead deepens only under a head two alternatives share, one token
/// at a time, until the paths through that head no longer collide, or
/// the window (`max_k`) runs out and the paths are emitted whole, as they
/// always were. "Collide" is `contest(a, b)`: the same token, a pair the
/// lexer can hand to either alternative, or a token set meeting one of
/// its members. Two paths collide up to depth d when their tokens collide
/// at every position below d that both have; a path that ends sooner is
/// open, so it keeps colliding for as long as it lasts, and the longer
/// path is then emitted whole. `exit_paths` are the ways OUT of a choice
/// that has an empty alternative: each FOLLOW token as an open one-token
/// path, and each FOLLOW₂ pair.
pub(crate) fn dispatch_prefixes(
    alts: &[Sequence],
    grammar: &Grammar,
    tokens: &Tokens,
    max_k: usize,
    contest: &dyn Fn(&str, &str) -> bool,
    exit_paths: &[Vec<String>],
    class_sets: &IndexMap<String, String>,
) -> Vec<DispatchPrefixes> {
    let n = alts.len();
    let mut one: Vec<Vec<PrefixPath>> = Vec::with_capacity(n);
    let mut nullable: Vec<bool> = Vec::with_capacity(n);
    let mut heads: Vec<Vec<String>> = Vec::with_capacity(n);
    for alt in alts {
        let paths = if alt.is_empty() {
            Vec::new()
        } else {
            alt_prefixes_raw(
                alt,
                grammar,
                tokens,
                1,
                &IndexSet::new(),
                None,
                Some(class_sets),
            )
        };
        nullable.push(paths.iter().any(|p| p.tokens.is_empty() && !p.done));
        let mut seen: IndexSet<String> = IndexSet::new();
        let mut hs: Vec<String> = Vec::new();
        for p in &paths {
            if p.tokens.len() != 1 {
                continue;
            }
            if seen.insert(p.tokens[0].clone()) {
                hs.push(p.tokens[0].clone());
            }
        }
        heads.push(hs);
        one.push(paths);
    }

    // A head is contested when another alternative, or an exit, has a
    // head the lexer could hand to either.
    let mut contested: Vec<IndexSet<String>> = Vec::with_capacity(n);
    for i in 0..n {
        let mut out: IndexSet<String> = IndexSet::new();
        for h in &heads[i] {
            let mut hit = false;
            for (j, others) in heads.iter().enumerate() {
                if j == i || hit {
                    continue;
                }
                if others.iter().any(|o| contest(h, o)) {
                    hit = true;
                }
            }
            if !hit && exit_paths.iter().any(|e| contest(h, &e[0])) {
                hit = true;
            }
            if hit {
                out.insert(h.clone());
            }
        }
        contested.push(out);
    }

    // Deep paths, only under the contested heads.
    let mut deep: Vec<Vec<PrefixPath>> = Vec::with_capacity(n);
    for (i, alt) in alts.iter().enumerate() {
        if contested[i].is_empty() {
            deep.push(Vec::new());
            continue;
        }
        let c = &contested[i];
        let filter = |t: &str| c.contains(t);
        deep.push(
            alt_prefixes_raw(
                alt,
                grammar,
                tokens,
                max_k,
                &IndexSet::new(),
                Some(&filter),
                Some(class_sets),
            )
            .into_iter()
            .filter(|p| !p.tokens.is_empty())
            .collect(),
        );
    }

    // The depth at which `p` stops colliding with every rival: the
    // position after the first one where they differ, over all rivals;
    // or the whole path when some rival never differs within what both
    // have.
    let depth_of = |p: &PrefixPath, i: usize| -> usize {
        let mut d = 1usize;
        let mut against = |q: &[String]| -> bool {
            let m = p.tokens.len().min(q.len());
            let mut k = 0;
            while k < m && contest(&p.tokens[k], &q[k]) {
                k += 1;
            }
            if k == m {
                return false;
            }
            if k + 1 > d {
                d = k + 1;
            }
            true
        };
        for (j, paths) in deep.iter().enumerate() {
            if j == i {
                continue;
            }
            for q in paths {
                if !against(&q.tokens) {
                    return p.tokens.len();
                }
            }
        }
        for e in exit_paths {
            if !against(e) {
                return p.tokens.len();
            }
        }
        d
    };

    let mut out: Vec<DispatchPrefixes> = Vec::with_capacity(n);
    for i in 0..n {
        let mut seen: IndexSet<String> = IndexSet::new();
        let mut prefixes: Vec<Vec<String>> = Vec::new();
        for h in &heads[i] {
            if !contested[i].contains(h) {
                if seen.insert(h.clone()) {
                    prefixes.push(vec![h.clone()]);
                }
                continue;
            }
            for p in &deep[i] {
                if &p.tokens[0] != h {
                    continue;
                }
                let d = depth_of(p, i);
                let prefix: Vec<String> = p.tokens[..d].to_vec();
                if seen.insert(prefix.join(" ")) {
                    prefixes.push(prefix);
                }
            }
        }
        out.push(DispatchPrefixes {
            prefixes,
            nullable: nullable[i],
        });
    }
    let _ = one;
    out
}
