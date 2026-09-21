// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

//! Desugaring: rewrite a grammar so that the only element kinds
//! remaining are terminals and references. Each `[X]`, `*X`, `1*X`,
//! `m*nX` and `( … )` is replaced by a reference to a generated helper
//! production that expresses the same language. Mirrors `desugar` in
//! `ts/src/compiler.ts`, including the helper names it mints.

use indexmap::IndexSet;

use crate::ir::{diag_name, origin_of, Element, Grammar, Kind, Production, Sequence};

struct Desugar {
    extra: Vec<Production>,
    used: IndexSet<String>,
    /// Origin of the production currently being desugared: every helper
    /// minted belongs to it, so the provenance map can point
    /// `_gen7_star_DIGIT` back at the rule the author wrote.
    origin: String,
}

impl Desugar {
    /// Collision-avoiding name like `_gen1_<hint>`, `_gen2_<hint>`, …
    fn fresh_name(&mut self, hint: &str) -> String {
        let mut i = self.extra.len();
        loop {
            i += 1;
            let name = format!("_gen{i}_{hint}");
            if !self.used.contains(&name) {
                self.used.insert(name.clone());
                return name;
            }
        }
    }

    fn helper(&mut self, name: String, alts: Vec<Sequence>, repeat_helper: bool) -> Element {
        let mut prod = Production::helper(&name, alts, &self.origin);
        prod.repeat_helper = repeat_helper;
        self.extra.push(prod);
        Element::reference(name)
    }

    fn alt(&mut self, alt: &[Element]) -> Sequence {
        alt.iter().map(|el| self.element(el)).collect()
    }

    fn element(&mut self, el: &Element) -> Element {
        match &el.kind {
            Kind::Term { .. } | Kind::Ref { .. } | Kind::Regex { .. } | Kind::Token { .. } => {
                return el.clone()
            }
            Kind::Prose { text } => {
                // Unreachable: prose resolution drops every prose element
                // (or fails) before desugaring runs.
                panic!(
                    "{}: internal: unresolved prose terminal '<{}>'",
                    diag_name(),
                    text
                );
            }
            Kind::Group { alts } => {
                // Recurse into the group's alts so nested sugar is
                // flattened, then emit a helper production whose body is
                // those alts.
                let inner_alts: Vec<Sequence> = alts.iter().map(|a| self.alt(a)).collect();
                let name = self.fresh_name("group");
                return self.helper(name, inner_alts, false);
            }
            _ => {}
        }

        // `opt`, `star`, `plus` and `rep` all wrap a single inner element.
        let inner = self.element(el.inner().expect("sugar wraps an inner element"));
        // Name the generated helper after what it repeats. A literal
        // lifted from a named production carries that name, and a
        // built-in token carries its own.
        let hint: String = match &inner.kind {
            Kind::Ref { name, .. } => name.clone(),
            Kind::Term { token_name, .. } => token_name.clone().unwrap_or_else(|| "term".into()),
            Kind::Token { name } => name.trim_start_matches('#').to_string(),
            _ => "x".into(),
        };

        match &el.kind {
            Kind::Opt { .. } => {
                // H ::= inner | (empty)
                let name = self.fresh_name(&format!("opt_{hint}"));
                self.helper(name, vec![vec![inner], vec![]], true)
            }
            Kind::Star { debt_guard, .. } => {
                // H = inner H / (empty)
                let name = self.fresh_name(&format!("star_{hint}"));
                let self_ref = Element::reference(name.clone());
                let mut prod =
                    Production::helper(&name, vec![vec![inner, self_ref], vec![]], &self.origin);
                prod.repeat_helper = true;
                // A left-recursion tail loop that may have to yield to an
                // enclosing suffix carries its counter onto the helper it
                // becomes.
                prod.debt_guard = debt_guard.clone();
                self.extra.push(prod);
                Element::reference(name)
            }
            Kind::Plus { .. } => {
                // H = inner Tail   where   Tail = inner Tail / (empty)
                let tail_name = self.fresh_name(&format!("star_{hint}"));
                let plus_name = self.fresh_name(&format!("plus_{hint}"));
                let tail_ref = Element::reference(tail_name.clone());
                self.helper(
                    tail_name,
                    vec![vec![inner.clone(), tail_ref.clone()], vec![]],
                    true,
                );
                self.helper(plus_name, vec![vec![inner, tail_ref]], false)
            }
            Kind::Rep { min, max, .. } => {
                // Bounded repetition: `min` mandatory copies of the inner
                // element followed by a tail that accepts up to
                // `(max - min)` more.
                let rep_name = self.fresh_name(&format!("rep_{hint}"));
                let mut rep_alt: Sequence = Vec::new();
                for _ in 0..*min {
                    rep_alt.push(inner.clone());
                }
                match max {
                    None => {
                        // Tail: unbounded star of inner.
                        let tail_star_name = self.fresh_name(&format!("star_{hint}"));
                        let tail_star_ref = Element::reference(tail_star_name.clone());
                        self.helper(
                            tail_star_name,
                            vec![vec![inner.clone(), tail_star_ref.clone()], vec![]],
                            true,
                        );
                        rep_alt.push(tail_star_ref);
                    }
                    Some(max) => {
                        // Nest (max - min) optionals: [A [A [A ...]]], built
                        // bottom-up as an explicit chain of helper
                        // productions. Each level is exactly what
                        // `element` would have emitted for
                        // opt(group([[inner, <previous level>]])): the group
                        // helper first, then the optional wrapping a
                        // reference to it.
                        let mut nested_ref: Option<Element> = None;
                        for _ in 0..max.saturating_sub(*min) {
                            let seq: Sequence = match &nested_ref {
                                Some(r) => vec![inner.clone(), r.clone()],
                                None => vec![inner.clone()],
                            };
                            let group_name = self.fresh_name("group");
                            self.helper(group_name.clone(), vec![seq], false);
                            let group_ref = Element::reference(group_name.clone());
                            let opt_name = self.fresh_name(&format!("opt_{group_name}"));
                            self.helper(opt_name.clone(), vec![vec![group_ref], vec![]], true);
                            nested_ref = Some(Element::reference(opt_name));
                        }
                        if let Some(r) = nested_ref {
                            rep_alt.push(r);
                        }
                    }
                }
                let alt = self.alt(&rep_alt);
                self.helper(rep_name, vec![alt], false)
            }
            _ => unreachable!("every other kind returned above"),
        }
    }
}

pub(crate) fn desugar(grammar: &Grammar) -> Grammar {
    let mut d = Desugar {
        extra: Vec::new(),
        used: grammar.productions.iter().map(|p| p.name.clone()).collect(),
        origin: String::new(),
    };

    let mut rewritten: Vec<Production> = Vec::with_capacity(grammar.productions.len());
    for p in &grammar.productions {
        d.origin = origin_of(p).to_string();
        let alts: Vec<Sequence> = p.alts.iter().map(|a| d.alt(a)).collect();
        let mut out = p.rebuilt(alts);
        // Probe-dispatch and tail-repeat flags survive desugaring
        // unchanged: the emitter routes around the standard
        // alt-compilation path for these. `repeat_helper` also arrives
        // from upstream: left factoring flags its nullable tail helpers.
        out.probe_dispatch = p.probe_dispatch.clone();
        out.probe_helper = p.probe_helper.clone();
        out.tail_repeat = p.tail_repeat.clone();
        out.repeat_helper = p.repeat_helper;
        out.debt_guard = p.debt_guard.clone();
        rewritten.push(out);
    }
    rewritten.extend(d.extra);
    Grammar {
        productions: rewritten,
        ..Default::default()
    }
}
