// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

//! Left-recursion elimination (Paull's algorithm, with hidden left
//! recursion behind nullable sugar made direct first) and the tail-repeat
//! rewrite. Mirrors `eliminateLeftRecursion` and `rewriteTailRepeats` in
//! `ts/src/compiler.ts`.

use indexmap::{IndexMap, IndexSet};

use crate::annotate::exempt_alias;
use crate::ir::{
    diag_name, refs_in, Element, EmitError, Grammar, Kind, Production, Sequence, TailRepeatSpec,
};

/// Sugar that can match nothing: `[X]`, `*X`, and `m*nX` with m = 0.
/// Returns the element sequence for the branch where the sugar DOES
/// match at least once, or `None` if the element is not nullable sugar.
///
/// Deliberately narrow: a nullable REFERENCE is already handled by
/// Paull's substitution; what that machinery cannot see is sugar,
/// because this pass runs before desugaring.
fn nullable_sugar_present(el: &Element) -> Option<Sequence> {
    match &el.kind {
        Kind::Opt { inner } => Some(vec![(**inner).clone()]),
        Kind::Star { inner, .. } => Some(vec![(**inner).clone(), el.clone()]),
        Kind::Rep { min: 0, max, inner } => match max {
            Some(0) => None,
            None => Some(vec![(**inner).clone(), el.clone()]),
            Some(1) => Some(vec![(**inner).clone()]),
            Some(max) => Some(vec![
                (**inner).clone(),
                Element::rep(0, Some(max - 1), (**inner).clone()),
            ]),
        },
        _ => None,
    }
}

/// Does this alternative re-enter `name` at the same input position,
/// reachable only because everything before the self-reference can
/// match nothing? `A = ["x"] A "y"` is the shape. Direct left recursion
/// is excluded: there is no sugar to split.
fn is_hidden_left_recursive(alt: &[Element], name: &str) -> bool {
    let mut saw_sugar = false;
    for el in alt {
        if let Kind::Ref { name: n, .. } = &el.kind {
            return saw_sugar && n == name;
        }
        if nullable_sugar_present(el).is_none() {
            return false;
        }
        saw_sugar = true;
    }
    false
}

/// Split leading nullable sugar into explicit present/absent
/// alternatives, so that hidden left recursion becomes DIRECT left
/// recursion that `eliminate_direct_left_rec` can remove:
///
/// `A = ["x"] A "y" / "z"` becomes `A = "x" A "y" / A "y" / "z"`
fn expand_nullable_left_prefixes(prods: Vec<Production>) -> Vec<Production> {
    prods
        .into_iter()
        .map(|p| {
            let mut alts = p.alts.clone();
            let mut changed = false;
            // Each expansion shortens the leading sugar run of the
            // alternative it splits, so this terminates; the guard is
            // belt-and-braces.
            let guard = alts.iter().map(Vec::len).sum::<usize>() + 1;
            for _ in 0..guard {
                let Some(idx) = alts
                    .iter()
                    .position(|a| is_hidden_left_recursive(a, &p.name))
                else {
                    break;
                };
                let alt = alts[idx].clone();
                let present = nullable_sugar_present(&alt[0])
                    .expect("hidden left recursion starts with nullable sugar");
                let mut with: Sequence = present;
                with.extend(alt[1..].iter().cloned());
                let without: Sequence = alt[1..].to_vec();
                alts.splice(idx..=idx, [with, without]);
                changed = true;
            }
            if changed {
                Production { alts, ..p }
            } else {
                p
            }
        })
        .collect()
}

/// Eliminate left recursion, both direct (P → P α) and indirect
/// (P → Q α, Q → P β), via Paull's algorithm. Order the productions,
/// and for each A_i walk back over A_1..A_{i-1} inlining any leading
/// reference into A_i's alternatives; once the only remaining leading
/// self-reference on A_i is direct, rewrite to the iterative form
/// `P → (β_1 | … | β_m) (α_1 | … | α_n)*`.
///
/// Returns a new grammar carrying only productions, in the caller's
/// declared order. The input is not modified.
pub fn eliminate_left_recursion(grammar: &Grammar) -> Result<Grammar, EmitError> {
    eliminate_left_recursion_keeping(grammar, &IndexSet::new())
}

/// [`eliminate_left_recursion`] with a set of productions that are never
/// substituted into the alternatives they lead: the token classes of
/// `ConvertOptions::token_classes`. A class holds no reference, so no
/// left-recursive cycle can run through it, and Paull's invariant is
/// unaffected by leaving it in place.
pub(crate) fn eliminate_left_recursion_keeping(
    grammar: &Grammar,
    keep: &IndexSet<String>,
) -> Result<Grammar, EmitError> {
    let original_order: Vec<String> = grammar.productions.iter().map(|p| p.name.clone()).collect();
    // Suffix-debt counter names handed out across the whole grammar.
    let mut debt_names: IndexSet<String> = IndexSet::new();

    // Order productions so that rules referenced at a leading position
    // are processed before the rules that reference them.
    let copies: Vec<Production> = grammar
        .productions
        .iter()
        .map(|p| p.rebuilt(p.alts.clone()))
        .collect();
    let mut prods = topo_order_for_paull(expand_nullable_left_prefixes(copies));

    // Substitution normally runs for every production, even a cycle-free
    // one. One case is exempt: a PURE ALIAS, a production whose single
    // alternative is a single rule reference. Inlining it dissolves the
    // alias name, so the rule vanishes from the emitted AST. Aliases
    // caught up in a leading-reference cycle are still inlined.
    let cyclic = find_leading_ref_cycle_members(&prods);

    for i in 0..prods.len() {
        // Paull's invariant is that after this inner loop no alternative
        // of A_i begins with a ref to any A_j, j < i. The pure-alias
        // exemption leaves some A_j un-substituted, so inlining such an
        // alias can (re)introduce a leading ref to an earlier A_k. Re-run
        // the pass until it reaches a fixed point.
        if !exempt_alias(&prods[i], &cyclic) {
            let guard = prods.len() + 1;
            for _ in 0..guard {
                let mut changed = false;
                for j in 0..i {
                    if !has_leading_ref_to(&prods[i], &prods[j].name) {
                        continue;
                    }
                    if keep.contains(&prods[j].name) {
                        // A token class: substituted, exactly where any
                        // other leading reference is, by ONE token element
                        // naming its set (`#ident`) rather than by its
                        // alternatives. The tree is the one the plain
                        // substitution gives (the token consumed, no node)
                        // without the one-alternate-per-member fan-out.
                        let class_name = prods[j].name.clone();
                        prods[i] = substitute_leading_ref_by_token(&prods[i], &class_name);
                    } else {
                        let source = prods[j].clone();
                        prods[i] = substitute_leading_ref(&prods[i], &source);
                    }
                    changed = true;
                }
                if !changed {
                    break;
                }
            }
        }
        prods[i] = eliminate_direct_left_rec(&prods[i], &mut debt_names)?;
    }

    // Restore the caller's declared order, so the start rule still ends
    // up first.
    let mut by_name: IndexMap<String, Production> =
        prods.into_iter().map(|p| (p.name.clone(), p)).collect();
    let mut ordered = Vec::new();
    for name in &original_order {
        if let Some(p) = by_name.shift_remove(name) {
            ordered.push(p);
        }
    }
    ordered.extend(by_name.into_values());

    Ok(Grammar {
        productions: ordered,
        ..Default::default()
    })
}

/// Tarjan-flavoured SCC scan over the leading-reference graph: the names
/// of productions that participate in at least one cycle (self-loop or
/// longer).
pub(crate) fn find_leading_ref_cycle_members(prods: &[Production]) -> IndexSet<String> {
    let by_name: IndexMap<&str, &Production> = prods.iter().map(|p| (p.name.as_str(), p)).collect();
    let leading_refs = |p: &Production| -> Vec<String> {
        p.alts
            .iter()
            .filter_map(|alt| alt.first())
            .filter_map(|first| first.ref_name())
            .filter(|name| by_name.contains_key(name))
            .map(str::to_string)
            .collect()
    };

    struct State<'a> {
        index: usize,
        stack: Vec<String>,
        on_stack: IndexSet<String>,
        indices: IndexMap<String, usize>,
        lowlinks: IndexMap<String, usize>,
        cyclic: IndexSet<String>,
        by_name: &'a IndexMap<&'a str, &'a Production>,
    }

    // Tarjan's algorithm, with the depth-first search carried on an
    // explicit stack rather than the call stack. A grammar is untrusted
    // input and its reference graph is as deep as the author made it: a
    // chain of a few thousand rules is enough to overflow a recursive
    // walk, and a Rust stack that runs out aborts the process. The order
    // of visits, of the lowlink updates and of the popped components is
    // exactly the recursive one.
    struct Frame {
        name: String,
        targets: Vec<String>,
        next: usize,
    }

    fn open_frame(
        name: &str,
        st: &mut State<'_>,
        leading_refs: &dyn Fn(&Production) -> Vec<String>,
    ) -> Frame {
        st.indices.insert(name.to_string(), st.index);
        st.lowlinks.insert(name.to_string(), st.index);
        st.index += 1;
        st.stack.push(name.to_string());
        st.on_stack.insert(name.to_string());
        let targets = st
            .by_name
            .get(name)
            .map(|prod| leading_refs(prod))
            .unwrap_or_default();
        Frame {
            name: name.to_string(),
            targets,
            next: 0,
        }
    }

    fn strong_connect(
        root: &str,
        st: &mut State<'_>,
        leading_refs: &dyn Fn(&Production) -> Vec<String>,
    ) {
        let mut frames: Vec<Frame> = vec![open_frame(root, st, leading_refs)];
        while let Some(frame) = frames.last_mut() {
            if frame.next < frame.targets.len() {
                let target = frame.targets[frame.next].clone();
                frame.next += 1;
                if !st.indices.contains_key(&target) {
                    let child = open_frame(&target, st, leading_refs);
                    frames.push(child);
                } else if st.on_stack.contains(&target) {
                    let name = frame.name.clone();
                    let low = st.lowlinks[&name].min(st.indices[&target]);
                    st.lowlinks.insert(name, low);
                }
                continue;
            }

            let name = frames
                .pop()
                .expect("the frame just read is still there")
                .name;
            if st.lowlinks[&name] == st.indices[&name] {
                let mut scc = Vec::new();
                loop {
                    let w = st.stack.pop().expect("the SCC root is on the stack");
                    st.on_stack.shift_remove(&w);
                    let done = w == name;
                    scc.push(w);
                    if done {
                        break;
                    }
                }
                let is_cycle = scc.len() > 1
                    || (scc.len() == 1
                        && st
                            .by_name
                            .get(scc[0].as_str())
                            .is_some_and(|p| leading_refs(p).contains(&scc[0])));
                if is_cycle {
                    for n in scc {
                        st.cyclic.insert(n);
                    }
                }
            }
            // What the recursive call did on return: carry the child's
            // lowlink up to the parent that pushed it.
            if let Some(parent) = frames.last() {
                let low = st.lowlinks[&parent.name].min(st.lowlinks[&name]);
                st.lowlinks.insert(parent.name.clone(), low);
            }
        }
    }

    let mut st = State {
        index: 0,
        stack: Vec::new(),
        on_stack: IndexSet::new(),
        indices: IndexMap::new(),
        lowlinks: IndexMap::new(),
        cyclic: IndexSet::new(),
        by_name: &by_name,
    };
    for p in prods {
        if !st.indices.contains_key(&p.name) {
            strong_connect(&p.name, &mut st, &leading_refs);
        }
    }
    st.cyclic
}

/// Topological order over the leading-position reference graph: an edge
/// A → B exists when A has an alternative whose first element is a
/// reference to B. Cycles are preserved as-is.
fn topo_order_for_paull(prods: Vec<Production>) -> Vec<Production> {
    let names: Vec<String> = prods.iter().map(|p| p.name.clone()).collect();
    let mut by_name: IndexMap<String, Production> =
        prods.into_iter().map(|p| (p.name.clone(), p)).collect();
    let mut colour: IndexMap<String, u8> = IndexMap::new(); // 0 unseen, 1 in progress, 2 done
    let mut order: Vec<Production> = Vec::new();

    // Depth first, on an explicit stack for the same reason Tarjan above
    // uses one: the graph's depth is the grammar author's choice. `Exit`
    // is what the recursive call did after its children returned, so the
    // post-order is unchanged.
    enum Step {
        Enter(String),
        Exit(String),
    }

    fn visit(
        root: &str,
        by_name: &mut IndexMap<String, Production>,
        colour: &mut IndexMap<String, u8>,
        order: &mut Vec<Production>,
    ) {
        let mut steps: Vec<Step> = vec![Step::Enter(root.to_string())];
        while let Some(step) = steps.pop() {
            match step {
                Step::Enter(name) => {
                    if colour.get(&name).copied().unwrap_or(0) != 0 {
                        continue;
                    }
                    colour.insert(name.clone(), 1);
                    let leading: Option<Vec<String>> = by_name.get(&name).map(|p| {
                        p.alts
                            .iter()
                            .filter_map(|alt| alt.first().and_then(|el| el.ref_name()))
                            .filter(|n| by_name.contains_key(*n))
                            .map(str::to_string)
                            .collect()
                    });
                    match leading {
                        Some(targets) => {
                            steps.push(Step::Exit(name));
                            for target in targets.into_iter().rev() {
                                steps.push(Step::Enter(target));
                            }
                        }
                        None => {
                            colour.insert(name, 2);
                        }
                    }
                }
                Step::Exit(name) => {
                    colour.insert(name.clone(), 2);
                    if let Some(p) = by_name.get(&name) {
                        order.push(p.clone());
                    }
                }
            }
        }
    }

    for name in &names {
        visit(name, &mut by_name, &mut colour, &mut order);
    }
    order
}

/// True when at least one alternative of `prod` begins with a reference
/// to `name`.
fn has_leading_ref_to(prod: &Production, name: &str) -> bool {
    prod.alts
        .iter()
        .any(|alt| alt.first().is_some_and(|el| el.is_ref_to(name)))
}

/// For every alternative of `target` that begins with a ref to `source`,
/// replace that alt with |source.alts| copies, each with the leading
/// source-ref expanded to one of source's alts.
fn substitute_leading_ref(target: &Production, source: &Production) -> Production {
    let mut new_alts: Vec<Sequence> = Vec::new();
    for alt in &target.alts {
        if alt.first().is_some_and(|el| el.is_ref_to(&source.name)) {
            let tail = &alt[1..];
            for src_alt in &source.alts {
                let mut combined = src_alt.clone();
                combined.extend(tail.iter().cloned());
                new_alts.push(combined);
            }
        } else {
            new_alts.push(alt.clone());
        }
    }
    target.rebuilt(new_alts)
}

/// Replace a leading reference to a token class by the token element
/// naming the class's set (`ident` -> `#ident`). The set is minted by
/// `emit_grammar_spec` under exactly that name; see `token_class_names`.
fn substitute_leading_ref_by_token(target: &Production, class_name: &str) -> Production {
    let new_alts: Vec<Sequence> = target
        .alts
        .iter()
        .map(|alt| {
            if alt.first().is_some_and(|el| el.is_ref_to(class_name)) {
                let mut combined = vec![Element::token(format!("#{class_name}"))];
                combined.extend(alt[1..].iter().cloned());
                combined
            } else {
                alt.clone()
            }
        })
        .collect();
    target.rebuilt(new_alts)
}

/// Allocate a suffix-debt counter name for a production. Counter names
/// end up in a declarative condition path (`n.<counter>`), which the
/// engine splits on `.`, so reduce the rule name to word characters (one
/// underscore per code point) and disambiguate against what has already
/// been handed out.
fn fresh_debt_counter(rule_name: &str, used: &mut IndexSet<String>) -> String {
    let mut base = String::from("debt_");
    for c in rule_name.chars() {
        if c.is_ascii_alphanumeric() || c == '_' {
            base.push(c);
        } else {
            base.push('_');
        }
    }
    let mut name = base.clone();
    let mut i = 0;
    while used.contains(&name) {
        i += 1;
        name = format!("{base}_{i}");
    }
    used.insert(name.clone());
    name
}

/// Does any seed alternative re-enter this rule at all? A cheap
/// pre-filter for allocating a counter; `resolve_suffix_debts` does the
/// real analysis on the desugared grammar.
fn seeds_reference_self(seeds: &[Sequence], name: &str) -> bool {
    let mut refs = IndexSet::new();
    for alt in seeds {
        refs_in(alt, &mut refs);
    }
    refs.contains(name)
}

/// Rewrite a single production's direct left recursion to its iterative
/// equivalent.
fn eliminate_direct_left_rec(
    prod: &Production,
    debt_names: &mut IndexSet<String>,
) -> Result<Production, EmitError> {
    let mut recursive: Vec<Sequence> = Vec::new();
    let mut seeds: Vec<Sequence> = Vec::new();
    for alt in &prod.alts {
        if alt.first().is_some_and(|el| el.is_ref_to(&prod.name)) {
            recursive.push(alt[1..].to_vec());
        } else {
            seeds.push(alt.clone());
        }
    }

    // A trivial recursive alt (P ::= P, nothing else) derives P from P
    // with no progress. Drop it silently: nullable-prefix expansion can
    // legitimately produce one.
    let non_trivial: Vec<Sequence> = recursive.into_iter().filter(|t| !t.is_empty()).collect();
    if non_trivial.is_empty() {
        return Ok(prod.rebuilt(seeds));
    }
    if seeds.is_empty() {
        return Err(EmitError::at(
            format!(
                "{}: rule '{}' is purely left-recursive (no seed alternative); cannot eliminate",
                diag_name(),
                prod.name
            ),
            &prod.name,
            prod.sp,
        ));
    }

    let seed_element = if seeds.len() == 1 && seeds[0].len() == 1 {
        seeds[0][0].clone()
    } else {
        Element::group(seeds.clone())
    };
    let tail_inner = if non_trivial.len() == 1 && non_trivial[0].len() == 1 {
        non_trivial[0][0].clone()
    } else {
        Element::group(non_trivial)
    };

    // The rewrite is correct as a CFG, but it introduces a repetition
    // whose greediness can compete with a suffix of the very alternative
    // it was derived from. Flag the loop here; `resolve_suffix_debts`
    // confirms the contest against real FIRST sets and wires up the
    // counter, or drops the flag when the suffix and the loop cannot
    // collide.
    let debt_guard = if seeds_reference_self(&seeds, &prod.name) {
        Some(fresh_debt_counter(&prod.name, debt_names))
    } else {
        None
    };
    let star = Element {
        kind: Kind::Star {
            inner: Box::new(tail_inner),
            debt_guard,
        },
        sp: None,
    };

    Ok(prod.rebuilt(vec![vec![seed_element, star]]))
}

/// Rewrite tail self-references into same-depth repeats:
/// `X = prefix [ sep X ]` compiles to a rule that repeats itself (`r: X`)
/// from its close phase. Applies only when the production has exactly
/// one alternative, its last element is an option wrapping `sep… X` with
/// the self-reference LAST, every prefix and separator element is a
/// terminal, and the production is not the start production.
pub(crate) fn rewrite_tail_repeats(mut grammar: Grammar, start: &str) -> Grammar {
    for prod in &mut grammar.productions {
        if prod.probe_dispatch.is_some() || prod.probe_helper.is_some() {
            continue;
        }
        if prod.name == start || prod.alts.len() != 1 {
            continue;
        }
        let alt = &prod.alts[0];
        if alt.len() < 2 {
            continue;
        }
        let Kind::Opt { inner: last_inner } = &alt[alt.len() - 1].kind else {
            continue;
        };

        // Normalize the option body to a sequence: `[ a b ]` parses as
        // opt(group([[a, b]])); `[ a ]` as opt(a).
        let seq: Sequence = match &last_inner.kind {
            Kind::Group { alts } => {
                if alts.len() != 1 {
                    continue;
                }
                alts[0].clone()
            }
            _ => vec![(**last_inner).clone()],
        };
        if seq.len() < 2 {
            continue;
        }
        if !seq[seq.len() - 1].is_ref_to(&prod.name) {
            continue;
        }
        let sep: Sequence = seq[..seq.len() - 1].to_vec();
        if !sep.iter().all(Element::is_terminal) {
            continue;
        }
        let prefix: Sequence = alt[..alt.len() - 1].to_vec();
        if prefix.is_empty() || !prefix.iter().all(Element::is_terminal) {
            continue;
        }

        prod.alts = vec![prefix];
        prod.tail_repeat = Some(TailRepeatSpec { sep });
    }
    grammar
}
