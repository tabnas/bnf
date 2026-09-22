// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

//! The grammar IR: what a front-end lowers its notation into, and the
//! options and errors around compiling it. Mirrors the type half of
//! `ts/src/compiler.ts`.
//!
//! Nothing here knows what notation a grammar was WRITTEN in. A
//! front-end parses its own syntax into a [`Grammar`] and hands it to
//! [`emit_grammar_spec`](crate::emit_grammar_spec).

use std::cell::RefCell;
use std::fmt;

use indexmap::{IndexMap, IndexSet};
use serde::{Deserialize, Serialize};

/// Where an IR node came from in the front-end's grammar text.
///
/// `s` is the start offset (inclusive), `e` the end offset (exclusive),
/// `r` the 1-based row of the start and `c` the 1-based column, the last
/// two optional. Offsets and row/column are in the SAME UNITS the
/// front-end's own engine tokens use, so a front-end copies them straight
/// across with no arithmetic. That does mean the units are runtime-native
/// and not identical across ports: TypeScript offsets count UTF-16 code
/// units, Go's and this port's count bytes, the same divergence the
/// engine already records for token positions.
///
/// Spans are optional everywhere. A front-end that records them gets
/// ranged compile errors (see [`EmitError::sp`]); one that does not
/// compiles to exactly the same grammar.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub struct SrcSpan {
    pub s: usize,
    pub e: usize,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub r: Option<usize>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub c: Option<usize>,
}

impl SrcSpan {
    pub fn new(s: usize, e: usize) -> Self {
        Self {
            s,
            e,
            r: None,
            c: None,
        }
    }

    pub fn at(s: usize, e: usize, r: usize, c: usize) -> Self {
        Self {
            s,
            e,
            r: Some(r),
            c: Some(c),
        }
    }
}

/// One element of a sequence: a terminal in one of its spellings, a rule
/// reference, or EBNF sugar around further elements. The `kind` carries
/// the variant; `sp` is where the element came from, when the front-end
/// recorded it. Mirrors the TypeScript `Element` union.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Element {
    #[serde(flatten)]
    pub kind: Kind,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub sp: Option<SrcSpan>,
}

/// The element variants. Serialized with the TypeScript field names, so
/// an IR built in one runtime loads in the other.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "lowercase")]
pub enum Kind {
    /// A string literal. ABNF quoted strings are case-insensitive by
    /// default; `case_sensitive` is the front-end's statement of intent
    /// (omitting it preserves the RFC 5234 default). `token_name` is the
    /// preferred lexer token name, set by literal lifting when this
    /// terminal came from a production that names it (`PL = "+"` gives
    /// `#PL`).
    Term {
        literal: String,
        #[serde(
            default,
            rename = "caseSensitive",
            skip_serializing_if = "Option::is_none"
        )]
        case_sensitive: Option<bool>,
        #[serde(default, rename = "tokenName", skip_serializing_if = "Option::is_none")]
        token_name: Option<String>,
    },
    /// A rule reference. `debt` holds the suffix-debt counter mutations to
    /// emit on the alt that pushes this reference, written by
    /// `resolve_suffix_debts`; absent on every reference in a grammar with
    /// no contested tail loop.
    Ref {
        name: String,
        #[serde(default, skip_serializing_if = "Option::is_none")]
        debt: Option<IndexMap<String, i64>>,
    },
    /// A terminal that matches a built-in engine lexer token directly
    /// (`#TX`, `#NR`, `#ST`, `#VL`). `name` is the full token name.
    Token { name: String },
    /// RFC 5234 `prose-val`, `<free text>`. Informational: accepted only as
    /// the entire body of a production naming a built-in lexer token, or
    /// as the `<remove>` directive. Anywhere else it is an error.
    Prose { text: String },
    /// A character class or other regular expression terminal.
    Regex { pattern: String, flags: String },
    /// `[ A ]`
    Opt { inner: Box<Element> },
    /// `*A`. `debt_guard` names the suffix-debt counter guarding this
    /// repetition, set by left-recursion elimination on the tail loop it
    /// generates.
    Star {
        inner: Box<Element>,
        #[serde(default, rename = "debtGuard", skip_serializing_if = "Option::is_none")]
        debt_guard: Option<String>,
    },
    /// `1*A`
    Plus { inner: Box<Element> },
    /// `m*nA`. `max` is `None` for an unbounded upper bound (TypeScript's
    /// `Infinity`, which JSON carries as `null`).
    Rep {
        min: usize,
        #[serde(default, deserialize_with = "deserialize_max")]
        max: Option<usize>,
        inner: Box<Element>,
    },
    /// `( A / B )`
    Group { alts: Vec<Sequence> },
}

/// TypeScript writes `Infinity` for an unbounded repetition, which
/// `JSON.stringify` turns into `null`; a missing key means the same.
fn deserialize_max<'de, D>(deserializer: D) -> Result<Option<usize>, D::Error>
where
    D: serde::Deserializer<'de>,
{
    let value: Option<f64> = Option::deserialize(deserializer)?;
    Ok(match value {
        None => None,
        Some(n) if n.is_finite() && n >= 0.0 => Some(n as usize),
        Some(_) => None,
    })
}

/// A sequence of elements: one alternative of a production.
pub type Sequence = Vec<Element>;

impl Element {
    fn of(kind: Kind) -> Self {
        Self { kind, sp: None }
    }

    /// A literal with the notation's default case-sensitivity unstated.
    pub fn term(literal: impl Into<String>) -> Self {
        Self::of(Kind::Term {
            literal: literal.into(),
            case_sensitive: None,
            token_name: None,
        })
    }

    /// A literal whose case-sensitivity the front-end states.
    pub fn term_cs(literal: impl Into<String>, case_sensitive: bool) -> Self {
        Self::of(Kind::Term {
            literal: literal.into(),
            case_sensitive: Some(case_sensitive),
            token_name: None,
        })
    }

    /// A rule reference.
    pub fn reference(name: impl Into<String>) -> Self {
        Self::of(Kind::Ref {
            name: name.into(),
            debt: None,
        })
    }

    /// A built-in engine lexer token, named with its `#`.
    pub fn token(name: impl Into<String>) -> Self {
        Self::of(Kind::Token { name: name.into() })
    }

    /// A prose terminal (`<text>`).
    pub fn prose(text: impl Into<String>) -> Self {
        Self::of(Kind::Prose { text: text.into() })
    }

    /// A regular expression terminal.
    pub fn regex(pattern: impl Into<String>, flags: impl Into<String>) -> Self {
        Self::of(Kind::Regex {
            pattern: pattern.into(),
            flags: flags.into(),
        })
    }

    /// `[ inner ]`
    pub fn opt(inner: Element) -> Self {
        Self::of(Kind::Opt {
            inner: Box::new(inner),
        })
    }

    /// `*inner`
    pub fn star(inner: Element) -> Self {
        Self::of(Kind::Star {
            inner: Box::new(inner),
            debt_guard: None,
        })
    }

    /// `1*inner`
    pub fn plus(inner: Element) -> Self {
        Self::of(Kind::Plus {
            inner: Box::new(inner),
        })
    }

    /// `min*max inner`; `None` is an unbounded maximum.
    pub fn rep(min: usize, max: Option<usize>, inner: Element) -> Self {
        Self::of(Kind::Rep {
            min,
            max,
            inner: Box::new(inner),
        })
    }

    /// `( alts... )`
    pub fn group(alts: Vec<Sequence>) -> Self {
        Self::of(Kind::Group { alts })
    }

    /// The same element carrying a source span.
    pub fn with_span(mut self, sp: SrcSpan) -> Self {
        self.sp = Some(sp);
        self
    }

    /// The referenced rule name, for a reference.
    pub fn ref_name(&self) -> Option<&str> {
        match &self.kind {
            Kind::Ref { name, .. } => Some(name),
            _ => None,
        }
    }

    pub fn is_ref(&self) -> bool {
        matches!(self.kind, Kind::Ref { .. })
    }

    pub fn is_ref_to(&self, name: &str) -> bool {
        matches!(&self.kind, Kind::Ref { name: n, .. } if n == name)
    }

    /// The literal, for a term.
    pub fn literal(&self) -> Option<&str> {
        match &self.kind {
            Kind::Term { literal, .. } => Some(literal),
            _ => None,
        }
    }

    /// Whether this is a terminal in any of its consuming spellings.
    pub fn is_terminal(&self) -> bool {
        matches!(
            self.kind,
            Kind::Term { .. } | Kind::Token { .. } | Kind::Regex { .. }
        )
    }

    pub(crate) fn inner(&self) -> Option<&Element> {
        match &self.kind {
            Kind::Opt { inner }
            | Kind::Star { inner, .. }
            | Kind::Plus { inner }
            | Kind::Rep { inner, .. } => Some(inner),
            _ => None,
        }
    }
}

/// How a production contributes to the output AST.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum NodeKind {
    /// Emit a tagged node `{ rule, src, kids }`.
    #[default]
    User,
    /// The RFC 5234 core rules: flatten into the enclosing rule's `src`.
    Core,
    /// Synthetic sugar, dispatcher and chain rules: also flatten.
    Helper,
}

impl NodeKind {
    pub fn as_str(self) -> &'static str {
        match self {
            NodeKind::User => "user",
            NodeKind::Core => "core",
            NodeKind::Helper => "helper",
        }
    }
}

/// What a production builds when it carries a value annotation.
///
/// `members` names one member per PUSHING segment of the alternative, in
/// order: the parts the author named. An array has no member names; every
/// pushing segment is an element. `kind` is `"object"` or `"array"`; it
/// stays a string because a grammar deserialized from JSON reaches the
/// compiler with anything, and the refusal for an unknown kind is part of
/// the contract.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ValueAnnotation {
    pub kind: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub members: Vec<String>,
}

impl ValueAnnotation {
    pub fn object(members: impl IntoIterator<Item = impl Into<String>>) -> Self {
        Self {
            kind: "object".into(),
            members: members.into_iter().map(Into::into).collect(),
        }
    }

    pub fn array() -> Self {
        Self {
            kind: "array".into(),
            members: Vec::new(),
        }
    }
}

/// Configuration attached to a synthesised dispatcher production for an
/// ambiguous `[X D] Y` subsequence. See `rewrite_probe_dispatches`.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ProbeDispatchSpec {
    #[serde(rename = "probeRule")]
    pub probe_rule: String,
    pub disambiguator: Element,
    #[serde(rename = "withBranch")]
    pub with_branch: String,
    #[serde(rename = "noBranch")]
    pub no_branch: String,
}

/// The vocabulary of a synthesised probe-helper production.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ProbeHelperSpec {
    #[serde(rename = "vocabElements")]
    pub vocab_elements: Vec<Element>,
}

/// The separator of a production rewritten as a tail repeat.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct TailRepeatSpec {
    pub sep: Sequence,
}

/// One rule of the grammar. Mirrors the TypeScript `Production`.
///
/// The fields after `value` are written by the rewrite passes; a
/// front-end leaves them at their defaults.
#[derive(Debug, Clone, PartialEq, Default, Serialize, Deserialize)]
pub struct Production {
    pub name: String,
    pub alts: Vec<Sequence>,
    /// ABNF `name =/ alt`, flagged during parse and merged away before the
    /// grammar reaches the emitter.
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub incremental: bool,
    #[serde(default, rename = "nodeKind")]
    pub node_kind: NodeKind,
    /// The author-written production this one descends from. Absent
    /// means the production is itself author-written; read it through
    /// [`origin_of`].
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub origin: Option<String>,
    /// Where the author wrote this production, when the front-end records
    /// it.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub sp: Option<SrcSpan>,
    /// A value this production should BUILD rather than the AST node the
    /// tree builders produce by default. Set by a front-end.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub value: Option<ValueAnnotation>,
    #[serde(
        default,
        rename = "tailRepeat",
        skip_serializing_if = "Option::is_none"
    )]
    pub tail_repeat: Option<TailRepeatSpec>,
    #[serde(
        default,
        rename = "repeatHelper",
        skip_serializing_if = "std::ops::Not::not"
    )]
    pub repeat_helper: bool,
    #[serde(default, rename = "debtGuard", skip_serializing_if = "Option::is_none")]
    pub debt_guard: Option<String>,
    #[serde(default, rename = "debtOwed", skip_serializing_if = "Option::is_none")]
    pub debt_owed: Option<Vec<String>>,
    #[serde(
        default,
        rename = "probeDispatch",
        skip_serializing_if = "Option::is_none"
    )]
    pub probe_dispatch: Option<ProbeDispatchSpec>,
    #[serde(
        default,
        rename = "probeHelper",
        skip_serializing_if = "Option::is_none"
    )]
    pub probe_helper: Option<ProbeHelperSpec>,
}

impl Production {
    pub fn new(name: impl Into<String>, alts: Vec<Sequence>) -> Self {
        Self {
            name: name.into(),
            alts,
            ..Default::default()
        }
    }

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
}

/// The author-written production a (possibly synthesised) production
/// descends from.
pub fn origin_of(prod: &Production) -> &str {
    prod.origin.as_deref().unwrap_or(&prod.name)
}

/// One entry of the probe-dispatch analyser's report.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct AmbiguityReport {
    pub rule: String,
    #[serde(rename = "altIdx")]
    pub alt_idx: usize,
    #[serde(rename = "optIdx")]
    pub opt_idx: usize,
    pub reason: String,
    pub resolved: bool,
}

/// A grammar: the productions a front-end lowered, plus the `<remove>`
/// directives and the ambiguity report.
#[derive(Debug, Clone, PartialEq, Default, Serialize, Deserialize)]
pub struct Grammar {
    pub productions: Vec<Production>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub ambiguities: Vec<AmbiguityReport>,
    /// Rules and tokens to drop.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub remove: Vec<String>,
    /// `<all> = <remove>`: wipe the instance first.
    #[serde(
        default,
        rename = "clearAll",
        skip_serializing_if = "std::ops::Not::not"
    )]
    pub clear_all: bool,
}

impl Grammar {
    pub fn new(productions: Vec<Production>) -> Self {
        Self {
            productions,
            ..Default::default()
        }
    }

    pub(crate) fn find(&self, name: &str) -> Option<&Production> {
        self.productions.iter().find(|p| p.name == name)
    }
}

/// Compiler options. Mirrors the TypeScript `ConvertOptions`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ConvertOptions {
    /// Start rule name (default: the first production).
    pub start: Option<String>,
    /// Group tag stamped on every emitted alt, and the prefix the shared
    /// compiler's own diagnostics carry (default `bnf`; a front-end
    /// passes its own). It reaches a diagnostic only from the moment
    /// [`emit_grammar_spec`](crate::emit_grammar_spec) applies these
    /// options, so a front-end's parse error on the grammar source is
    /// raised earlier and keeps that front-end's own fixed prefix.
    pub tag: Option<String>,
    /// Emit the probe/phase-retry dispatcher and the tree builders as
    /// engine `$`-builtin refs plus `k` config instead of registered
    /// closures. This keeps the spec function-free so it survives
    /// compilation (pure-recognition) mode.
    pub builtins: bool,
    /// Emit a stable `m` mark on each user-rule alt, enabling
    /// `@<rule>:o|c:<mark>` user-action references.
    pub marks: bool,
    /// Treat word-like literals as whole-word keywords, so `"option"`
    /// does not match the prefix of `optional`.
    pub word_keywords: bool,
    /// Emit `meta.provenance`, the map from each generated rule name back
    /// to the author-written production it came from. On by default.
    pub provenance: bool,
}

impl Default for ConvertOptions {
    fn default() -> Self {
        Self {
            start: None,
            tag: None,
            builtins: false,
            marks: false,
            word_keywords: false,
            provenance: true,
        }
    }
}

impl ConvertOptions {
    pub fn tag(tag: impl Into<String>) -> Self {
        Self {
            tag: Some(tag.into()),
            ..Default::default()
        }
    }

    pub fn start(mut self, start: impl Into<String>) -> Self {
        self.start = Some(start.into());
        self
    }

    pub fn builtins(mut self, on: bool) -> Self {
        self.builtins = on;
        self
    }

    pub fn marks(mut self, on: bool) -> Self {
        self.marks = on;
        self
    }

    pub fn word_keywords(mut self, on: bool) -> Self {
        self.word_keywords = on;
        self
    }

    pub fn provenance(mut self, on: bool) -> Self {
        self.provenance = on;
        self
    }
}

/// A compile failure that can say WHERE.
///
/// `rule` is the rule being compiled when the failure was raised and `sp`
/// where in the grammar source, when the IR knew. Both are populated only
/// when the offending IR node carries the information, so a front-end
/// that records no spans gets the message it always got. Every diagnostic
/// this compiler raises is one of these; the message text matches the
/// TypeScript compiler byte for byte.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct EmitError {
    pub message: String,
    pub rule: Option<String>,
    pub sp: Option<SrcSpan>,
}

impl EmitError {
    pub fn new(message: impl Into<String>) -> Self {
        Self {
            message: message.into(),
            rule: None,
            sp: None,
        }
    }

    pub fn at(message: impl Into<String>, rule: &str, sp: Option<SrcSpan>) -> Self {
        Self {
            message: message.into(),
            rule: Some(rule.to_string()),
            sp,
        }
    }
}

impl fmt::Display for EmitError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.message)
    }
}

impl std::error::Error for EmitError {}

// Diagnostics name the NOTATION the grammar was written in, not this
// package: a front-end's users should not see "bnf:" on an error about
// their own syntax. `emit_grammar_spec` sets this from `opts.tag` for the
// duration of one emit, exactly as the TypeScript compiler's module-scoped
// `_diagName`. Thread-local rather than global so two conversions on two
// threads each keep their own prefix, which is what Go's emit lock buys.
thread_local! {
    static DIAG_NAME: RefCell<String> = RefCell::new("bnf".to_string());
}

/// The prefix the shared compiler's own diagnostics carry: the tag of the
/// most recent emit on this thread, or `bnf` when no emit has run on it.
///
/// A diagnostic raised before an emit applies its options, a front-end's
/// parse error on the grammar source being the usual one, is outside this
/// and carries that front-end's own fixed prefix.
pub fn diag_name() -> String {
    DIAG_NAME.with(|name| name.borrow().clone())
}

pub(crate) fn set_diag_name(tag: &str) {
    DIAG_NAME.with(|name| *name.borrow_mut() = tag.to_string());
}

/// How deeply one element of the IR may nest before `emit_grammar_spec`
/// refuses the grammar.
///
/// The passes over an element (desugaring, FIRST sets, factoring, the
/// emitter itself) are written as the canonical TypeScript writes them,
/// one recursive call per level, and a Rust stack that runs out aborts
/// the process rather than unwinding. A grammar is untrusted input, so
/// nesting that deep has to become an error return instead. The limit is
/// the same one `serde_json` applies by default to a nested document, so
/// an IR that arrives as JSON is already held to it, and it is an order
/// of magnitude past anything a grammar author writes: the deepest
/// nesting in the ABNF conformance corpus is in single figures.
///
/// TypeScript raises a catchable `RangeError` instead, several hundred
/// levels later; `DIVERGENCE.md` records the difference.
pub const MAX_ELEMENT_DEPTH: usize = 128;

/// Refuse a grammar whose element nesting would overflow the stack of a
/// pass that walks it. Measured with an explicit stack: finding the depth
/// must not be able to overflow either.
pub(crate) fn check_element_depth(grammar: &Grammar) -> Result<(), EmitError> {
    for prod in &grammar.productions {
        let mut roots: Vec<&Element> = Vec::new();
        for alt in &prod.alts {
            roots.extend(alt.iter());
        }
        if let Some(tail) = &prod.tail_repeat {
            roots.extend(tail.sep.iter());
        }
        if let Some(helper) = &prod.probe_helper {
            roots.extend(helper.vocab_elements.iter());
        }
        let mut stack: Vec<(&Element, usize)> = roots.into_iter().map(|el| (el, 1)).collect();
        while let Some((el, depth)) = stack.pop() {
            if depth > MAX_ELEMENT_DEPTH {
                return Err(EmitError::at(
                    format!(
                        "{}: rule '{}' nests elements more than {} deep, which is \
                         past what this compiler will walk. Split the \
                         rule into named rules.",
                        diag_name(),
                        prod.name,
                        MAX_ELEMENT_DEPTH
                    ),
                    &prod.name,
                    prod.sp,
                ));
            }
            match &el.kind {
                Kind::Opt { inner }
                | Kind::Star { inner, .. }
                | Kind::Plus { inner }
                | Kind::Rep { inner, .. } => stack.push((inner, depth + 1)),
                Kind::Group { alts } => {
                    for alt in alts {
                        for inner in alt.iter() {
                            stack.push((inner, depth + 1));
                        }
                    }
                }
                _ => {}
            }
        }
    }
    Ok(())
}

/// Built-in engine lexer tokens that a rule may reference by a bare
/// uppercase name, mapping the name to the token the lexer emits. A user
/// (or core) rule of the same name always wins.
pub const BUILTIN_TOKENS: [(&str, &str); 4] =
    [("TX", "#TX"), ("NR", "#NR"), ("ST", "#ST"), ("VL", "#VL")];

/// The engine token a bare built-in name resolves to, if it is one.
pub fn builtin_token(name: &str) -> Option<&'static str> {
    BUILTIN_TOKENS
        .iter()
        .find(|(bare, _)| *bare == name)
        .map(|(_, token)| *token)
}

/// The one prose directive the compiler acts on, matched
/// case-insensitively after trimming.
pub const REMOVE_PROSE: &str = "remove";

/// The one prose NAME: `<all> = <remove>` clears the whole grammar.
pub const REMOVE_ALL: &str = "all";

/// Whether a production name came from a prose token. Such a name keeps
/// its angle brackets, which no ordinary rulename can contain.
pub fn is_prose_name(name: &str) -> bool {
    name.starts_with('<') && name.ends_with('>')
}

/// Collect the rule references in a sequence, sugar included.
pub fn refs_in(alt: &[Element], out: &mut IndexSet<String>) {
    for el in alt {
        match &el.kind {
            Kind::Ref { name, .. } => {
                out.insert(name.clone());
            }
            Kind::Opt { inner }
            | Kind::Star { inner, .. }
            | Kind::Plus { inner }
            | Kind::Rep { inner, .. } => refs_in(std::slice::from_ref(inner), out),
            Kind::Group { alts } => {
                for a in alts {
                    refs_in(a, out);
                }
            }
            _ => {}
        }
    }
}

/// A quoted-string literal is effectively case-sensitive either when the
/// front-end said so or when it contains no ASCII letters (there is
/// nothing to fold: `"+"` matches `+` in any "case").
pub fn is_effectively_case_sensitive(literal: &str, case_sensitive: Option<bool>) -> bool {
    if case_sensitive == Some(true) {
        return true;
    }
    !literal.bytes().any(|b| b.is_ascii_alphabetic())
}

/// The key a term is looked up (or allocated) under: the literal and its
/// effective case-sensitivity, so a sensitive and an insensitive
/// occurrence of the same string are distinct tokens.
pub fn term_key(literal: &str, case_sensitive: Option<bool>) -> String {
    let prefix = if is_effectively_case_sensitive(literal, case_sensitive) {
        "cs:"
    } else {
        "ci:"
    };
    format!("{prefix}{literal}")
}

/// `term_key` of a term element. Panics on any other kind: the callers
/// have already matched on it.
pub(crate) fn term_key_of(el: &Element) -> String {
    match &el.kind {
        Kind::Term {
            literal,
            case_sensitive,
            ..
        } => term_key(literal, *case_sensitive),
        _ => unreachable!("term_key_of on a non-term element"),
    }
}

pub(crate) fn regex_key(pattern: &str, flags: &str) -> String {
    format!("/{pattern}/{flags}")
}

/// Escape the regular-expression metacharacters in a literal, so the
/// literal matches itself.
pub fn escape_regexp(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for c in s.chars() {
        if matches!(
            c,
            '\\' | '^' | '$' | '.' | '*' | '+' | '?' | '(' | ')' | '[' | ']' | '{' | '}' | '|'
        ) {
            out.push('\\');
        }
        out.push(c);
    }
    out
}
