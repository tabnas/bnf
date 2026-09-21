// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

//! Value annotations: validating them against the AUTHORED grammar and
//! planning which members nest, before any rewrite runs. Mirrors
//! `planValueAnnotations`, `planArrayHelpers` and their helpers in
//! `ts/src/compiler.ts`.
//!
//! The rewrites are exactly why this cannot wait for emit time. Paull's
//! pass INLINES a leading reference, so by then the first member's rule is
//! gone and the segments no longer correspond to the parts the author
//! named. The shape the AUTHOR wrote is the only place those are still
//! visible.

use indexmap::{IndexMap, IndexSet};

use crate::ir::{diag_name, origin_of, Element, EmitError, Grammar, Kind, NodeKind, Production};
use crate::leftrec::find_leading_ref_cycle_members;

/// What the planner predicts, per annotated production: one flag per
/// part that produces a value. `plan` says whether that part nests (its
/// own rule builds a value); `collect` (arrays only) says whether it is
/// sugar, and so contributes its own parts as elements instead of being
/// one.
#[derive(Debug, Default)]
pub(crate) struct ValuePlan {
    pub plan: IndexMap<String, Vec<bool>>,
    pub collect: IndexMap<String, Vec<bool>>,
}

/// The field a parse-tree node carries its matched text in, and the one
/// the close-phase capture tests for to tell a node from anything else.
const SRC_FIELD: &str = "src";

pub(crate) fn plan_value_annotations(grammar: &Grammar) -> Result<ValuePlan, EmitError> {
    let by_name: IndexMap<&str, &Production> = grammar
        .productions
        .iter()
        .map(|p| (p.name.as_str(), p))
        .collect();
    let mut out = ValuePlan::default();

    // Nothing annotated means nothing to predict, and this is the common
    // case by a wide margin.
    if !grammar.productions.iter().any(|p| p.value.is_some()) {
        return Ok(out);
    }

    // Which productions keep their leading reference. Computed on the
    // AUTHORED grammar for the same reason everything else here is.
    let cyclic = find_leading_ref_cycle_members(&grammar.productions);

    // Inlining an annotated rule erases the builders it was going to run,
    // and that happens wherever the rule is a LEADING reference, not only
    // where the caller is itself annotated. Every alternative, not just
    // the first: Paull's pass substitutes into any alternative whose
    // leading element is a reference.
    for prod in &grammar.productions {
        if exempt_alias(prod, &cyclic) {
            continue;
        }
        for alt in &prod.alts {
            let Some(first) = alt.first() else { continue };
            let Some(first_name) = first.ref_name() else {
                continue;
            };
            let (_, annotated) = resolve_leading_fold(first_name, &by_name);
            let Some(hit) = annotated else { continue };
            return Err(EmitError::at(
                format!(
                    "{}: rule '{}' begins with '{}', and '{}' is folded into '{}' by \
                     left-recursion elimination — which erases the value '{}' is \
                     annotated to build, so nothing would produce it. Put a literal \
                     before '{}', or remove the annotation on '{}'.",
                    diag_name(),
                    prod.name,
                    first_name,
                    hit,
                    prod.name,
                    hit,
                    first_name,
                    hit
                ),
                &prod.name,
                prod.sp,
            ));
        }
    }

    for prod in &grammar.productions {
        let Some(v) = &prod.value else { continue };
        let at = |message: String| EmitError::at(message, &prod.name, prod.sp);

        if v.kind != "object" && v.kind != "array" {
            return Err(at(format!(
                "{}: rule '{}' has a value annotation of unknown kind '{}'. A rule \
                 builds an 'object' or an 'array'.",
                diag_name(),
                prod.name,
                v.kind
            )));
        }
        if prod.alts.len() != 1 {
            return Err(at(format!(
                "{}: rule '{}' has a value annotation and {} alternatives. A value \
                 annotation names the parts of ONE alternative; with more than one it \
                 is ambiguous which alternative's parts are named. Split the rule, or \
                 annotate the alternatives' own rules.",
                diag_name(),
                prod.name,
                prod.alts.len()
            )));
        }

        let mut seen: IndexSet<&str> = IndexSet::new();
        for m in &v.members {
            if m.is_empty() {
                return Err(at(format!(
                    "{}: rule '{}' has a value annotation naming a member that is not a \
                     name (\"\"). Every member of an object is named by a non-empty \
                     string.",
                    diag_name(),
                    prod.name
                )));
            }
            // Each member is a separate KEY. Two parts named the same
            // thing both write to it, so the second silently overwrites
            // the first.
            if seen.contains(m.as_str()) {
                return Err(at(format!(
                    "{}: rule '{}' names the member '{}' twice. Each member is a \
                     separate key, so the second part would overwrite the first. Give \
                     them different names.",
                    diag_name(),
                    prod.name,
                    m
                )));
            }
            seen.insert(m);
            // `src` is how a parse-tree node is told apart from a value,
            // so a value that HAS a `src` member is taken for a node and
            // dropped wherever a tree rule captures it.
            if m == SRC_FIELD {
                return Err(at(format!(
                    "{}: rule '{}' names a member '{}'. That name is how a value is \
                     told apart from a parse-tree node, so a value carrying it is \
                     mistaken for a node and dropped wherever an ordinary rule captures \
                     this one. Name the member something else.",
                    diag_name(),
                    prod.name,
                    SRC_FIELD
                )));
            }
        }

        let alt = &prod.alts[0];

        // Prose resolution drops an informational prose definition
        // outright, so this rule is gone before anything could build its
        // value.
        if alt.len() == 1 {
            if let Kind::Prose { text } = &alt[0].kind {
                return Err(at(format!(
                    "{}: rule '{}' has a value annotation, but its body is prose \
                     ('<{}>'), which describes a built-in token rather than defining a \
                     rule — the rule is dropped, so nothing would build the value. \
                     Remove the annotation.",
                    diag_name(),
                    prod.name,
                    text
                )));
            }
        }

        // Every part that PUSHES is a member, not every part that is a
        // reference: a group or a repetition becomes a reference to a
        // generated helper before the emitter sees it.
        let parts: Vec<&Element> = alt.iter().filter(|el| pushes_value(el)).collect();

        if v.kind == "array" {
            // An array's parts are positional; there is nothing for a
            // name to attach to.
            if !v.members.is_empty() {
                return Err(at(format!(
                    "{}: rule '{}' builds an array and names {} member{}. An array's \
                     parts are positional and are not named; annotate it as an object \
                     to name them.",
                    diag_name(),
                    prod.name,
                    v.members.len(),
                    if v.members.len() == 1 { "" } else { "s" }
                )));
            }
        } else {
            let named = v.members.len();
            if named != parts.len() {
                return Err(at(format!(
                    "{}: rule '{}' names {} member{} but has {} part{} a value. A value \
                     annotation names one member per part that produces a value; a \
                     literal produces no value and is not a member.",
                    diag_name(),
                    prod.name,
                    named,
                    if named == 1 { "" } else { "s" },
                    parts.len(),
                    if parts.len() == 1 {
                        " that produces"
                    } else {
                        "s that produce"
                    }
                )));
            }
        }

        // The LEADING reference is folded into this rule by left-recursion
        // elimination. Its rule has to reduce to exactly one part, or the
        // boundary the author drew is lost. Unless this rule is a pure
        // alias, which that pass does not substitute into at all.
        if let Some(first) = alt.first() {
            if let Some(first_name) = first.ref_name() {
                if !exempt_alias(prod, &cyclic) {
                    let (ok, _) = resolve_leading_fold(first_name, &by_name);
                    if !ok {
                        return Err(at(format!(
                            "{}: rule '{}' names '{}' as its first member, but '{}' is \
                             folded into this rule by left-recursion elimination and its \
                             body is not a single part, so the member would not cover \
                             what the author wrote. Give '{}' a body that is one part (a \
                             reference, a repetition or a group), or put a literal before \
                             it.",
                            diag_name(),
                            prod.name,
                            first_name,
                            first_name,
                            first_name
                        )));
                    }
                }
            }
        }

        let flags: Vec<bool> = parts
            .iter()
            .map(|el| {
                el.ref_name()
                    .and_then(|n| by_name.get(n))
                    .is_some_and(|p| p.value.is_some())
            })
            .collect();

        // A member that is not itself annotated resolves to the source
        // text its tree builders accumulated, which a rule that builds a
        // value does not contribute to. Refuse rather than hand back the
        // hole.
        for (i, el) in parts.iter().enumerate() {
            if flags[i] {
                continue;
            }
            let mut hit = reaches_annotated(el, &by_name, &mut IndexSet::new());
            let Some(mut hit_name) = hit.take() else {
                continue;
            };
            // A repetition under an array is COLLECTED, not taken as
            // text, so the question has to be asked again of each thing
            // its helper pushes. Reaching this rule ITSELF is still
            // refused.
            if v.kind == "array" && collects_values(el) && hit_name != prod.name {
                match hidden_annotated(el, &by_name) {
                    None => continue,
                    Some(h) => hit_name = h,
                }
            }
            let which = if v.kind == "array" {
                format!("element {}", i + 1)
            } else {
                format!("member '{}'", v.members[i])
            };
            if hit_name == prod.name {
                return Err(at(format!(
                    "{}: rule '{}' takes {} as source text, but that part reaches '{}' \
                     itself, which builds a value — a rule that builds a value \
                     contributes no text to the part above it, so a recursive rule \
                     cannot take its own repetition as text. Annotate the rule the \
                     recursion pushes instead.",
                    diag_name(),
                    prod.name,
                    which,
                    prod.name
                )));
            }
            return Err(at(format!(
                "{}: rule '{}' takes {} as source text, but '{}' is reached from it and \
                 builds a value of its own — a rule that builds a value contributes no \
                 text to the part above it, so the member would be missing '{}'s match, \
                 or empty. Make that part '{}' itself so it nests, or remove the \
                 annotation on '{}'.",
                diag_name(),
                prod.name,
                which,
                hit_name,
                hit_name,
                hit_name,
                hit_name
            )));
        }

        out.plan.insert(prod.name.clone(), flags);
        // Which parts are SUGAR the author wrote here, and so collect
        // into the array rather than becoming one element of it.
        if v.kind == "array" {
            out.collect.insert(
                prod.name.clone(),
                parts.iter().map(|el| collects_values(el)).collect(),
            );
        }
    }
    Ok(out)
}

/// The first value-building rule that a COLLECTING part would still take
/// as text: the walk descends through collecting sugar and then asks the
/// ordinary question of each reference it arrives at.
fn hidden_annotated(el: &Element, by_name: &IndexMap<&str, &Production>) -> Option<String> {
    match &el.kind {
        Kind::Ref { name, .. } => {
            let target = by_name.get(name.as_str())?;
            // Annotated: the helper pushes it whole, so it nests.
            if target.value.is_some() {
                return None;
            }
            reaches_annotated(el, by_name, &mut IndexSet::new())
        }
        Kind::Opt { inner }
        | Kind::Star { inner, .. }
        | Kind::Plus { inner }
        | Kind::Rep { inner, .. } => hidden_annotated(inner, by_name),
        Kind::Group { alts } => alts
            .iter()
            .flatten()
            .find_map(|inner| hidden_annotated(inner, by_name)),
        _ => None,
    }
}

/// The first rule that BUILDS A VALUE reachable from this element, or
/// `None`. Walks sugar and follows rule references transitively.
fn reaches_annotated(
    el: &Element,
    by_name: &IndexMap<&str, &Production>,
    seen: &mut IndexSet<String>,
) -> Option<String> {
    match &el.kind {
        Kind::Ref { name, .. } => {
            let target = by_name.get(name.as_str())?;
            if target.value.is_some() {
                return Some(target.name.clone());
            }
            if seen.contains(&target.name) {
                return None;
            }
            seen.insert(target.name.clone());
            for alt in &target.alts {
                for inner in alt {
                    if let Some(hit) = reaches_annotated(inner, by_name, seen) {
                        return Some(hit);
                    }
                }
            }
            None
        }
        Kind::Opt { inner }
        | Kind::Star { inner, .. }
        | Kind::Plus { inner }
        | Kind::Rep { inner, .. } => reaches_annotated(inner, by_name, seen),
        Kind::Group { alts } => {
            for alt in alts {
                for inner in alt {
                    if let Some(hit) = reaches_annotated(inner, by_name, seen) {
                        return Some(hit);
                    }
                }
            }
            None
        }
        _ => None,
    }
}

/// Whether left-recursion elimination will leave this production's
/// leading reference ALONE: a pure alias (one alternative that is one
/// reference) is exempt from substitution unless it or its target is
/// caught in a leading-reference cycle.
pub(crate) fn exempt_alias(p: &Production, cyclic: &IndexSet<String>) -> bool {
    if p.alts.len() != 1 || p.alts[0].len() != 1 {
        return false;
    }
    match p.alts[0][0].ref_name() {
        Some(name) => !cyclic.contains(&p.name) && !cyclic.contains(name),
        None => false,
    }
}

/// Whether an element produces a value when it is matched. A reference,
/// a repetition and a group all become a push of one child; a terminal
/// in any of its spellings consumes input and pushes nothing.
pub(crate) fn pushes_value(el: &Element) -> bool {
    !matches!(
        el.kind,
        Kind::Term { .. } | Kind::Regex { .. } | Kind::Token { .. } | Kind::Prose { .. }
    )
}

/// Whether this part COLLECTS into an enclosing array (contributes the
/// parts inside it as elements) rather than being one element itself. A
/// repetition does; a bare group does not.
fn collects_values(el: &Element) -> bool {
    matches!(
        el.kind,
        Kind::Star { .. } | Kind::Plus { .. } | Kind::Opt { .. } | Kind::Rep { .. }
    )
}

/// Follow a leading reference the way left-recursion elimination will:
/// an alias chain collapses all the way down. Returns whether the chain
/// ends in a body of exactly one pushing part, and the first rule in the
/// chain that builds a value of its own.
fn resolve_leading_fold(
    name: &str,
    by_name: &IndexMap<&str, &Production>,
) -> (bool, Option<String>) {
    let mut seen: IndexSet<String> = IndexSet::new();
    let mut prod = by_name.get(name).copied();
    while let Some(p) = prod {
        // A cycle is left recursion reached through aliases: stop rather
        // than guess.
        if seen.contains(&p.name) {
            return (false, None);
        }
        seen.insert(p.name.clone());
        if p.value.is_some() {
            return (false, Some(p.name.clone()));
        }
        if p.alts.len() != 1 || p.alts[0].len() != 1 {
            return (false, None);
        }
        let el = &p.alts[0][0];
        if !pushes_value(el) {
            return (false, None);
        }
        // A group or repetition is opaque to the fold: it becomes one
        // helper reference, which is one part.
        let Some(next) = el.ref_name() else {
            return (true, None);
        };
        prod = by_name.get(next).copied();
    }
    // An undefined rule is not this check's to refuse.
    (true, None)
}

/// Which generated helpers stand inside an annotated array, and so must
/// FILL that array rather than build a node of their own. Walks from
/// each annotated array production through helper references only; a
/// user rule ENDS the walk.
pub(crate) fn plan_array_helpers(
    grammar: &Grammar,
    collect: &IndexMap<String, Vec<bool>>,
) -> IndexSet<String> {
    let by_name: IndexMap<&str, &Production> = grammar
        .productions
        .iter()
        .map(|p| (p.name.as_str(), p))
        .collect();
    let mut helpers: IndexSet<String> = IndexSet::new();

    fn walk(name: &str, by_name: &IndexMap<&str, &Production>, helpers: &mut IndexSet<String>) {
        let Some(prod) = by_name.get(name) else {
            return;
        };
        if prod.node_kind != NodeKind::Helper || helpers.contains(name) {
            return;
        }
        helpers.insert(name.to_string());
        for alt in &prod.alts {
            for el in alt {
                if let Some(r) = el.ref_name() {
                    walk(r, by_name, helpers);
                }
            }
        }
    }

    // Whether anything inside this helper would become an ELEMENT: a
    // reference to a rule of the author's, rather than more helpers and
    // terminals. A repetition of pure terminals has nothing to collect.
    fn yields_elements(
        name: &str,
        by_name: &IndexMap<&str, &Production>,
        seen: &mut IndexSet<String>,
    ) -> bool {
        let Some(prod) = by_name.get(name) else {
            return false;
        };
        if seen.contains(name) {
            return false;
        }
        seen.insert(name.to_string());
        for alt in &prod.alts {
            for el in alt {
                let Some(r) = el.ref_name() else { continue };
                match by_name.get(r) {
                    Some(target) if target.node_kind == NodeKind::Helper => {
                        if yields_elements(r, by_name, seen) {
                            return true;
                        }
                    }
                    _ => return true,
                }
            }
        }
        false
    }

    for prod in &grammar.productions {
        if prod.value.as_ref().map(|v| v.kind.as_str()) != Some("array") {
            continue;
        }
        let Some(sugar) = collect.get(origin_of(prod)) else {
            continue;
        };
        for alt in &prod.alts {
            // Desugaring leaves every pushing part a reference, so the
            // k-th reference here is the k-th part the annotation was
            // planned against.
            let mut k = 0;
            for el in alt {
                let Some(r) = el.ref_name() else { continue };
                if sugar.get(k).copied().unwrap_or(false)
                    && yields_elements(r, &by_name, &mut IndexSet::new())
                {
                    walk(r, &by_name, &mut helpers);
                }
                k += 1;
            }
        }
    }
    helpers
}
