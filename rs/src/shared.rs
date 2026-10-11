// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

//! The compiler's working copy of a grammar: the IR a front-end built,
//! with each element of a production's sequences behind an [`Rc`].
//!
//! The passes rebuild sequences out of the elements of others. Paull's
//! substitution writes each alternative of the rule it inlines in front of
//! the tail of each alternative that rule begins, factoring splits a run
//! into a prefix and its tails, desugaring maps a sequence element by
//! element. The TypeScript and Go compilers hold an element by reference,
//! so a rebuilt sequence costs a pointer per element and the elements are
//! shared between every sequence that holds them; neither changes an
//! element in place, and a pass that changes one writes a new element into
//! its sequence. With owned elements each of those copies was a deep one,
//! the element and every string it holds, and a chain of n
//! single-alternative rules, each beginning with the next, gives its rules
//! n²/2 elements between them: the owned copies, with their rewrite
//! provenance, took 1.7 GB at n = 2,000, and 6 GB was not enough at
//! n = 5,000. These types are the shape the canonical compilers have: a
//! sequence is a vector of shared elements, and the one pass that writes
//! into an element (`resolve_suffix_debts`) copies it on write.
//!
//! The names are the IR's own, [`Grammar`], [`Production`] and
//! [`Sequence`], since the passes are the canonical compiler's passes over
//! the canonical IR; only the ownership of an element differs. The public
//! IR ([`crate::ir`]) keeps owned elements, so a front-end still builds
//! `Element { kind, sp }` values. [`Grammar::from_ir`] makes the working
//! copy, and [`Grammar::into_ir`] gives a grammar back to the public pass
//! that returns one. The sequences inside a group stay owned: they are
//! part of the public [`Kind`], and nothing rebuilds them by the million.

use std::rc::Rc;

use indexmap::IndexSet;

use crate::ir::{
    self, AmbiguityReport, Element, Kind, NodeKind, ProbeDispatchSpec, ProbeHelperSpec, SrcSpan,
    TailRepeatSpec, ValueAnnotation,
};

/// One alternative of a production: its elements, each shared with every
/// other sequence that holds it.
pub(crate) type Sequence = Vec<Rc<Element>>;

/// One rule of the working copy: [`ir::Production`] field for field, with
/// shared sequences.
#[derive(Debug, Clone, Default)]
pub(crate) struct Production {
    pub name: String,
    pub alts: Vec<Sequence>,
    pub incremental: bool,
    pub node_kind: NodeKind,
    pub origin: Option<String>,
    pub sp: Option<SrcSpan>,
    pub value: Option<ValueAnnotation>,
    pub tail_repeat: Option<TailRepeatSpec>,
    pub repeat_helper: bool,
    pub repeat_loop: bool,
    pub debt_guard: Option<String>,
    pub debt_owed: Option<Vec<String>>,
    pub probe_dispatch: Option<ProbeDispatchSpec>,
    pub probe_helper: Option<ProbeHelperSpec>,
}

impl Production {
    /// The production rebuilt from its author-facing fields only, with
    /// new alternatives. The TypeScript passes that rebuild a production
    /// copy exactly `name`, `alts`, `nodeKind`, `origin`, `sp` and
    /// `value`; every rewrite flag is left behind.
    pub(crate) fn rebuilt(&self, alts: Vec<Sequence>) -> Self {
        Self {
            name: self.name.clone(),
            alts,
            node_kind: self.node_kind,
            origin: self.origin.clone(),
            sp: self.sp,
            value: self.value.clone(),
            ..Default::default()
        }
    }

    pub(crate) fn helper(name: impl Into<String>, alts: Vec<Sequence>, origin: &str) -> Self {
        Self {
            name: name.into(),
            alts,
            node_kind: NodeKind::Helper,
            origin: Some(origin.to_string()),
            ..Default::default()
        }
    }

    fn from_ir(p: &ir::Production) -> Self {
        Self {
            name: p.name.clone(),
            alts: p
                .alts
                .iter()
                .map(|alt| alt.iter().map(AsElement::share).collect())
                .collect(),
            incremental: p.incremental,
            node_kind: p.node_kind,
            origin: p.origin.clone(),
            sp: p.sp,
            value: p.value.clone(),
            tail_repeat: p.tail_repeat.clone(),
            repeat_helper: p.repeat_helper,
            repeat_loop: p.repeat_loop,
            debt_guard: p.debt_guard.clone(),
            debt_owed: p.debt_owed.clone(),
            probe_dispatch: p.probe_dispatch.clone(),
            probe_helper: p.probe_helper.clone(),
        }
    }

    fn into_ir(self) -> ir::Production {
        ir::Production {
            name: self.name,
            alts: self
                .alts
                .into_iter()
                .map(|alt| alt.into_iter().map(Rc::unwrap_or_clone).collect())
                .collect(),
            incremental: self.incremental,
            node_kind: self.node_kind,
            origin: self.origin,
            sp: self.sp,
            value: self.value,
            tail_repeat: self.tail_repeat,
            repeat_helper: self.repeat_helper,
            repeat_loop: self.repeat_loop,
            debt_guard: self.debt_guard,
            debt_owed: self.debt_owed,
            probe_dispatch: self.probe_dispatch,
            probe_helper: self.probe_helper,
        }
    }
}

/// The working copy of a grammar: [`ir::Grammar`] field for field.
#[derive(Debug, Clone, Default)]
pub(crate) struct Grammar {
    pub productions: Vec<Production>,
    pub ambiguities: Vec<AmbiguityReport>,
    pub remove: Vec<String>,
    pub clear_all: bool,
}

impl Grammar {
    /// The working copy of a front-end's grammar, which the passes cannot
    /// disturb: every element copied once, into an allocation of its own
    /// that the copies the passes make go on sharing.
    pub(crate) fn from_ir(grammar: &ir::Grammar) -> Self {
        Self {
            productions: grammar
                .productions
                .iter()
                .map(Production::from_ir)
                .collect(),
            ambiguities: grammar.ambiguities.clone(),
            remove: grammar.remove.clone(),
            clear_all: grammar.clear_all,
        }
    }

    /// The grammar with owned elements again, each element that one
    /// sequence alone still holds moved rather than copied.
    pub(crate) fn into_ir(self) -> ir::Grammar {
        ir::Grammar {
            productions: self
                .productions
                .into_iter()
                .map(Production::into_ir)
                .collect(),
            ambiguities: self.ambiguities,
            remove: self.remove,
            clear_all: self.clear_all,
        }
    }

    pub(crate) fn find(&self, name: &str) -> Option<&Production> {
        self.productions.iter().find(|p| p.name == name)
    }
}

/// The author-written production a (possibly synthesised) production
/// descends from. See [`ir::origin_of`].
pub(crate) fn origin_of(prod: &Production) -> &str {
    prod.origin.as_deref().unwrap_or(&prod.name)
}

/// An element as a sequence holds it: owned inside a group, where the
/// public [`Kind`] keeps it, or shared at the top level of a production.
/// A walk that starts on a production's sequence and descends into its
/// groups meets both.
pub(crate) trait AsElement {
    fn element(&self) -> &Element;

    /// The element as a shared one: the same allocation when it already
    /// is one, a new allocation holding a copy when it is owned.
    fn share(&self) -> Rc<Element>;
}

impl AsElement for Element {
    fn element(&self) -> &Element {
        self
    }

    fn share(&self) -> Rc<Element> {
        Rc::new(self.clone())
    }
}

impl AsElement for Rc<Element> {
    fn element(&self) -> &Element {
        self
    }

    fn share(&self) -> Rc<Element> {
        Rc::clone(self)
    }
}

/// A sequence holding owned copies of shared elements: what a group made
/// out of sequences of the working copy holds, the public [`Kind`] taking
/// owned ones.
pub(crate) fn owned(seq: &[Rc<Element>]) -> ir::Sequence {
    seq.iter().map(|el| Element::clone(el)).collect()
}

/// [`ir::refs_in`] over either kind of sequence.
pub(crate) fn refs_in<E: AsElement>(alt: &[E], out: &mut IndexSet<String>) {
    for el in alt {
        match &el.element().kind {
            Kind::Ref { name, .. } => {
                out.insert(name.clone());
            }
            Kind::Opt { inner }
            | Kind::Star { inner, .. }
            | Kind::Plus { inner }
            | Kind::Rep { inner, .. } => refs_in(std::slice::from_ref(inner.as_ref()), out),
            Kind::Group { alts } => {
                for a in alts {
                    refs_in(a, out);
                }
            }
            _ => {}
        }
    }
}

/// Collect every token element's name in a sequence, nested ones
/// included. Mirrors the TypeScript `tokensIn`.
pub(crate) fn tokens_in<E: AsElement>(alt: &[E], out: &mut IndexSet<String>) {
    for el in alt {
        match &el.element().kind {
            Kind::Token { name, .. } => {
                out.insert(name.clone());
            }
            Kind::Opt { inner }
            | Kind::Star { inner, .. }
            | Kind::Plus { inner }
            | Kind::Rep { inner, .. } => tokens_in(std::slice::from_ref(inner.as_ref()), out),
            Kind::Group { alts } => {
                for a in alts {
                    tokens_in(a, out);
                }
            }
            _ => {}
        }
    }
}
