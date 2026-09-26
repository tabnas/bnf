// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

//! Left factoring, and the structural comparisons and token-span
//! measures it needs. Mirrors `leftFactor`, `factorOnce`, `inlineHeadRef`,
//! `elemEqual`, `seqTokenSpan`, `firstCharRangesOfElement` and
//! `unwrapAlt` in `ts/src/compiler.ts`.
//!
//! tabnas alternates are first-match-wins: once an alternative's first
//! token matches, the engine commits to it. Two alternatives sharing a
//! non-trivial prefix can therefore never both be reachable, and no
//! finite token lookahead can separate them when the shared prefix has
//! unbounded token length. The classical fix is mechanical: factor the
//! prefix out and defer the choice to a helper that dispatches on the
//! first token AFTER the prefix.
//!
//! `P = α β1 / α β2   ⇒   P = α P$factN ; P$factN = β1 / β2`

use std::collections::VecDeque;

use indexmap::{IndexMap, IndexSet};

use crate::ir::{
    diag_name, is_effectively_case_sensitive, origin_of, term_key_of, Element, EmitError, Grammar,
    Kind, Production, Sequence,
};
use crate::ranges::{char_ranges_overlap, code_unit_reach, pattern_char_ranges, CharRange};

/// How many concrete tokens the multi-alt dispatcher fans each
/// alternative's prefix out to. Left factoring uses the same bound to
/// decide which shared prefixes the dispatcher can already see past.
pub(crate) const LOOKAHEAD_K: usize = 4;

/// Order-independent identity for a reference's suffix-debt mutations.
/// Empty and absent are the same thing.
fn debt_key(debt: Option<&IndexMap<String, i64>>) -> String {
    let Some(debt) = debt else {
        return String::new();
    };
    let mut keys: Vec<&String> = debt.keys().collect();
    keys.sort_by_key(|k| k.encode_utf16().collect::<Vec<u16>>());
    keys.iter()
        .map(|k| format!("{}={}", k, debt[*k]))
        .collect::<Vec<_>>()
        .join(",")
}

/// Structural equality of IR elements, the comparison left factoring
/// uses to recognise a shared prefix. Terms compare by their token key,
/// refs and built-in tokens by name (refs also by their debt), the sugar
/// forms recursively. Prose never equals anything.
pub(crate) fn elem_equal(a: &Element, b: &Element) -> bool {
    match (&a.kind, &b.kind) {
        (Kind::Term { .. }, Kind::Term { .. }) => term_key_of(a) == term_key_of(b),
        (Kind::Token { name: x }, Kind::Token { name: y }) => x == y,
        (Kind::Ref { name: x, debt: dx }, Kind::Ref { name: y, debt: dy }) => {
            x == y && debt_key(dx.as_ref()) == debt_key(dy.as_ref())
        }
        (
            Kind::Regex {
                pattern: px,
                flags: fx,
            },
            Kind::Regex {
                pattern: py,
                flags: fy,
            },
        ) => px == py && fx == fy,
        (Kind::Prose { .. }, Kind::Prose { .. }) => false,
        (
            Kind::Star {
                inner: ix,
                debt_guard: gx,
            },
            Kind::Star {
                inner: iy,
                debt_guard: gy,
            },
        ) => gx == gy && elem_equal(ix, iy),
        (Kind::Opt { inner: ix }, Kind::Opt { inner: iy })
        | (Kind::Plus { inner: ix }, Kind::Plus { inner: iy }) => elem_equal(ix, iy),
        (
            Kind::Rep {
                min: mx,
                max: nx,
                inner: ix,
            },
            Kind::Rep {
                min: my,
                max: ny,
                inner: iy,
            },
        ) => mx == my && nx == ny && elem_equal(ix, iy),
        (Kind::Group { alts: ax }, Kind::Group { alts: ay }) => {
            ax.len() == ay.len() && ax.iter().zip(ay).all(|(x, y)| seq_equal(x, y))
        }
        _ => false,
    }
}

pub(crate) fn seq_equal(a: &[Element], b: &[Element]) -> bool {
    a.len() == b.len() && a.iter().zip(b).all(|(x, y)| elem_equal(x, y))
}

/// The most tokens a sequence can span, for deciding whether the
/// dispatcher's K-token lookahead can see past it. Runs on the raw IR,
/// so each sugar kind is counted on its own terms. `None` stands for
/// infinity: anything unbounded (a star/plus, an unbounded rep, a cycle)
/// or unknown (prose).
pub(crate) fn seq_token_span(
    seq: &[Element],
    grammar: &Grammar,
    visited: &IndexSet<String>,
) -> Option<usize> {
    let mut total = 0usize;
    for el in seq {
        total += element_token_span(el, grammar, visited)?;
        if total > LOOKAHEAD_K {
            return None;
        }
    }
    Some(total)
}

fn element_token_span(
    el: &Element,
    grammar: &Grammar,
    visited: &IndexSet<String>,
) -> Option<usize> {
    match &el.kind {
        Kind::Term { .. } | Kind::Regex { .. } | Kind::Token { .. } => Some(1),
        Kind::Prose { .. } => None,
        // Absent contributes 0, present the inner span; the MOST it can
        // span is the inner span.
        Kind::Opt { inner } => element_token_span(inner, grammar, visited),
        Kind::Star { .. } | Kind::Plus { .. } => None,
        Kind::Rep { max, inner, .. } => {
            let max = (*max)?;
            Some(max * element_token_span(inner, grammar, visited)?)
        }
        Kind::Group { alts } => {
            let mut most = 0;
            for alt in alts {
                let n = seq_token_span(alt, grammar, visited)?;
                most = most.max(n);
            }
            Some(most)
        }
        Kind::Ref { name, .. } => {
            if visited.contains(name) {
                return None;
            }
            let target = grammar.find(name)?;
            if target.alts.is_empty() {
                return None;
            }
            let mut sub = visited.clone();
            sub.insert(name.clone());
            let mut most = 0;
            for alt in &target.alts {
                let n = seq_token_span(alt, grammar, &sub)?;
                most = most.max(n);
            }
            Some(most)
        }
    }
}

/// First-character coverage of an element, from the raw IR. `None` when
/// coverage cannot be established (a nullable element leaks its FOLLOW,
/// a cycle or unparseable regex is unknown), which the caller treats as
/// "not provably disjoint".
fn first_char_ranges_of_element(
    el: &Element,
    grammar: &Grammar,
    visited: &IndexSet<String>,
) -> Option<Vec<CharRange>> {
    match &el.kind {
        Kind::Term {
            literal,
            case_sensitive,
            ..
        } => {
            let c = literal.chars().next()?;
            let cp = c as u32;
            if !is_effectively_case_sensitive(literal, *case_sensitive) {
                let lo = c.to_lowercase().next().unwrap_or(c) as u32;
                let up = c.to_uppercase().next().unwrap_or(c) as u32;
                return Some(code_unit_reach(
                    &if lo == up {
                        vec![(cp, cp)]
                    } else {
                        vec![(lo, lo), (up, up)]
                    },
                    "",
                ));
            }
            Some(code_unit_reach(&[(cp, cp)], ""))
        }
        Kind::Regex { pattern, flags } => {
            pattern_char_ranges(pattern, flags).map(|r| code_unit_reach(&r, flags))
        }
        Kind::Ref { name, .. } => {
            if visited.contains(name) {
                return None;
            }
            let target = grammar.find(name)?;
            if target.alts.is_empty() {
                return None;
            }
            let mut sub = visited.clone();
            sub.insert(name.clone());
            let mut out = Vec::new();
            for alt in &target.alts {
                let first = alt.first()?;
                out.extend(first_char_ranges_of_element(first, grammar, &sub)?);
            }
            Some(out)
        }
        Kind::Group { alts } => {
            let mut out = Vec::new();
            for alt in alts {
                let first = alt.first()?;
                out.extend(first_char_ranges_of_element(first, grammar, visited)?);
            }
            Some(out)
        }
        Kind::Plus { inner } => first_char_ranges_of_element(inner, grammar, visited),
        Kind::Rep { min, inner, .. } => {
            if *min > 0 {
                first_char_ranges_of_element(inner, grammar, visited)
            } else {
                None
            }
        }
        Kind::Opt { .. } | Kind::Star { .. } | Kind::Token { .. } | Kind::Prose { .. } => None,
    }
}

/// See through a single-alternative group: `( a b )` as an entire
/// alternative is just `a b`.
fn unwrap_alt(alt: &[Element]) -> Sequence {
    let mut a: Sequence = alt.to_vec();
    loop {
        if a.len() == 1 {
            if let Kind::Group { alts } = &a[0].kind {
                if alts.len() == 1 {
                    a = alts[0].clone();
                    continue;
                }
            }
        }
        return a;
    }
}

/// Left-factor alternatives that share a leading element prefix. Only
/// CONSECUTIVE alternatives merge. The helper is a transparent helper
/// node, so factoring never changes the emitted tree; a helper whose
/// tails include the empty sequence is flagged `repeat_helper`.
pub(crate) fn left_factor(grammar: &Grammar) -> Result<Grammar, EmitError> {
    let mut used: IndexSet<String> = grammar.productions.iter().map(|p| p.name.clone()).collect();
    let mut out: Vec<Production> = Vec::new();
    let mut queue: VecDeque<Production> = grammar.productions.iter().cloned().collect();

    while let Some(prod) = queue.pop_front() {
        if prod.probe_dispatch.is_some()
            || prod.probe_helper.is_some()
            || prod.tail_repeat.is_some()
        {
            out.push(prod);
            continue;
        }
        let mut alts = prod.alts.clone();
        let mut changed = false;
        loop {
            let next = factor_once(
                &prod.name,
                origin_of(&prod),
                &alts,
                &mut used,
                &mut queue,
                grammar,
            )?;
            match next {
                None => break,
                Some(a) => {
                    alts = a;
                    changed = true;
                }
            }
        }
        if changed {
            out.push(Production { alts, ..prod });
        } else {
            out.push(prod);
        }
    }

    Ok(Grammar {
        productions: out,
        ..Default::default()
    })
}

fn fresh_fact_name(base: &str, used: &mut IndexSet<String>) -> String {
    let mut i = 0;
    loop {
        let name = format!("{base}$fact{i}");
        i += 1;
        if !used.contains(&name) {
            used.insert(name.clone());
            return name;
        }
    }
}

/// One factoring step over one production's alternatives: find the
/// first consecutive run sharing a leading element, replace it with a
/// single factored alternative, and queue the tail helper. `None` when
/// nothing shares a prefix beyond the dispatch lookahead.
fn factor_once(
    prod_name: &str,
    prod_origin: &str,
    alts: &[Sequence],
    used: &mut IndexSet<String>,
    queue: &mut VecDeque<Production>,
    grammar: &Grammar,
) -> Result<Option<Vec<Sequence>>, EmitError> {
    let views: Vec<Sequence> = alts.iter().map(|a| unwrap_alt(a)).collect();
    let prefix_beyond_lookahead = |prefix: &[Element]| -> bool {
        match seq_token_span(prefix, grammar, &IndexSet::new()) {
            None => true,
            Some(n) => n > LOOKAHEAD_K,
        }
    };

    for i in 0..alts.len().saturating_sub(1) {
        if views[i].is_empty() {
            continue;
        }
        let head_el = &views[i][0];

        // Gather run members. A later alternative joins the run when its
        // first element equals the head, directly or after inlining a
        // single-alternative rule it starts with. Alternatives between
        // members are skipped over only when their first characters are
        // provably disjoint from the head's.
        let mut members: Vec<usize> = vec![i];
        let mut member_views: IndexMap<usize, Sequence> = IndexMap::new();
        member_views.insert(i, views[i].clone());
        let mut annotated_inlines: IndexMap<usize, String> = IndexMap::new();
        let mut head_ranges: Option<Option<Vec<CharRange>>> = None;
        for (j, v) in views.iter().enumerate().skip(i + 1) {
            if !v.is_empty() && elem_equal(head_el, &v[0]) {
                members.push(j);
                member_views.insert(j, v.clone());
                continue;
            }
            if !v.is_empty() {
                if let Some((seq, annotated)) = inline_head_ref(v, head_el, grammar) {
                    members.push(j);
                    member_views.insert(j, seq);
                    if let Some(name) = annotated {
                        annotated_inlines.insert(j, name);
                    }
                    continue;
                }
            }
            // Not a member: skippable only if provably disjoint. The
            // ranges are compared as the walk returns them, without
            // normalising, exactly as the canonical compiler does.
            let hr = head_ranges.get_or_insert_with(|| {
                first_char_ranges_of_element(head_el, grammar, &IndexSet::new())
            });
            let Some(hr) = hr else { break };
            if v.is_empty() {
                break;
            }
            let Some(r) = first_char_ranges_of_element(&v[0], grammar, &IndexSet::new()) else {
                break;
            };
            if char_ranges_overlap(hr, &r) {
                break;
            }
        }
        if members.len() < 2 {
            continue;
        }

        let run: Vec<&Sequence> = members.iter().map(|m| &member_views[m]).collect();
        // Longest element-wise common prefix of the run.
        let mut plen = 1;
        'outer: loop {
            for v in &run {
                if v.len() <= plen || !elem_equal(&run[0][plen], &v[plen]) {
                    break 'outer;
                }
            }
            plen += 1;
        }

        let prefix: Sequence = run[0][..plen].to_vec();
        if !prefix_beyond_lookahead(&prefix) {
            // Dispatch lookahead already separates these.
            continue;
        }
        // Factoring is now committed for this run, so an annotated rule
        // that was inlined to form it really is about to be dissolved.
        for m in &members {
            let Some(name) = annotated_inlines.get(m) else {
                continue;
            };
            let sp = grammar.find(name).and_then(|t| t.sp);
            return Err(EmitError::at(
                format!(
                    "{}: rule '{}' builds a value, but it is the shared prefix of two \
                     alternatives of '{}' that have to be left-factored — factoring \
                     inlines it, which would erase the value it is annotated to build. \
                     Give the alternatives leading tokens that tell them apart, or move \
                     the annotation to a rule that is not a shared prefix.",
                    diag_name(),
                    name,
                    prod_name
                ),
                name,
                sp,
            ));
        }
        // Structurally duplicate tails collapse.
        let mut tails: Vec<Sequence> = Vec::new();
        for v in &run {
            let tail: Sequence = v[plen..].to_vec();
            if !tails.iter().any(|t| seq_equal(t, &tail)) {
                tails.push(tail);
            }
        }
        // Empty tail last: it matches anything, so the longer
        // continuations must be offered first. A stable partition.
        let has_empty = tails.iter().any(Vec::is_empty);
        let mut ordered: Vec<Sequence> = tails.iter().filter(|t| !t.is_empty()).cloned().collect();
        if has_empty {
            ordered.push(Vec::new());
        }
        let tails = ordered;

        let factored: Sequence = if tails.len() == 1 {
            // All run members were structurally identical.
            let mut f = prefix.clone();
            f.extend(tails[0].iter().cloned());
            f
        } else {
            let helper = fresh_fact_name(prod_name, used);
            let mut helper_prod = Production::helper(&helper, tails, prod_origin);
            if has_empty {
                helper_prod.repeat_helper = true;
            }
            queue.push_back(helper_prod);
            let mut f = prefix.clone();
            f.push(Element::reference(helper));
            f
        };

        // Preserve the enclosing shape: when every run member arrived
        // wrapped in its own single-alt group, keep the factored
        // alternative wrapped too.
        let wrapped = members
            .iter()
            .all(|m| alts[*m].len() == 1 && matches!(alts[*m][0].kind, Kind::Group { .. }));
        let replacement: Sequence = if wrapped {
            vec![Element::group(vec![factored])]
        } else {
            factored
        };

        let removed: IndexSet<usize> = members[1..].iter().copied().collect();
        let mut out: Vec<Sequence> = Vec::new();
        for (k, alt) in alts.iter().enumerate() {
            if removed.contains(&k) {
                continue;
            }
            if k == i {
                out.push(replacement.clone());
            } else {
                out.push(alt.clone());
            }
        }
        return Ok(Some(out));
    }

    Ok(None)
}

/// If `v` starts with a ref to a single-alternative production whose
/// body's first element equals `head_el`, return `v` with the ref
/// replaced by that body, and the name of that production when it
/// builds a value (the caller refuses only once factoring is committed).
fn inline_head_ref(
    v: &[Element],
    head_el: &Element,
    grammar: &Grammar,
) -> Option<(Sequence, Option<String>)> {
    let name = v[0].ref_name()?;
    let target = grammar.find(name)?;
    if target.alts.len() != 1 {
        return None;
    }
    if target.probe_dispatch.is_some()
        || target.probe_helper.is_some()
        || target.tail_repeat.is_some()
    {
        return None;
    }
    let body = unwrap_alt(&target.alts[0]);
    if body.is_empty() || !elem_equal(&body[0], head_el) {
        return None;
    }
    let mut seq = body;
    seq.extend(v[1..].iter().cloned());
    let annotated = if target.value.is_some() {
        Some(target.name.clone())
    } else {
        None
    };
    Some((seq, annotated))
}
