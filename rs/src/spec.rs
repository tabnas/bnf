// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

//! The emitted grammar: an in-memory `GrammarSpec` with typed alternates
//! (so marks and closure descriptors have somewhere to live), its wire
//! form (the engine's own pure-data document), and the spec-level
//! transforms: pure and recognition reductions, jsonic serialisation, and
//! user semantic actions. Mirrors `ts/src/spec.ts` plus the `GrammarSpec`
//! shape the TypeScript emitter builds.

use std::cell::RefCell;
use std::fmt;
use std::rc::Rc;
use std::sync::Arc;

use indexmap::IndexMap;
use serde_json::{json, Map, Value};
use tabnas::{Context, GrammarError, Rule, Tabnas};

use crate::ir::{diag_name, NodeKind};

/// A user semantic action: run after the compiler's own action on the alt
/// it is attached to, with the engine's rule and context.
pub type ActionFn =
    Arc<dyn Fn(&mut Rule, &mut Context) -> Result<(), tabnas::ActionError> + Send + Sync>;

/// Action refs to attach: `@<rule>:<phase>` (bo/ao/bc/ac) or
/// `@<rule>:o|c:<mark>`, each with the functions to run in order.
pub type ActionsMap = Vec<(String, Vec<ActionFn>)>;

/// What a `@`-ref in the spec's `ref` map stands for. In closure mode the
/// emitter registers one of these per tree-building or probe-control
/// action instead of a `$`-builtin; [`GrammarSpec::bind`] turns them into
/// engine closures by name. User actions land here too.
#[derive(Clone)]
pub enum RefAction {
    /// Allocate (when `init`) and accumulate the tree node: the closure
    /// twin of `@node$`.
    Node {
        init: bool,
        rule: String,
        kind: NodeKind,
        nterms: usize,
    },
    /// Merge the returned child's node into this rule's: `@capture$`.
    Capture { rule: String, kind: NodeKind },
    /// Lift the committed child's node up: `@bubble$`.
    Bubble,
    /// Fold a tail-repeat iteration into its parent: `@fold$`.
    Fold { c_n: usize },
    /// Probe dispatch phase 0 open: mark the position: `@probeInit$`.
    ProbeInit,
    /// Probe dispatch phase 0 close: peek, rewind, decide: `@probeDecide$`.
    ProbeDecide { disambiguator: String },
    /// A probe phase condition: `@probePhaseN$`.
    ProbePhase(u8),
    /// A user action attached to an alt (`@bnf_userN`).
    User(ActionFn),
    /// User actions composed onto a rule-phase hook (`@<rule>-<phase>`).
    Phase(Vec<ActionFn>),
}

impl fmt::Debug for RefAction {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            RefAction::Node {
                init,
                rule,
                kind,
                nterms,
            } => write!(f, "Node({init}, {rule}, {}, {nterms})", kind.as_str()),
            RefAction::Capture { rule, kind } => write!(f, "Capture({rule}, {})", kind.as_str()),
            RefAction::Bubble => write!(f, "Bubble"),
            RefAction::Fold { c_n } => write!(f, "Fold({c_n})"),
            RefAction::ProbeInit => write!(f, "ProbeInit"),
            RefAction::ProbeDecide { disambiguator } => write!(f, "ProbeDecide({disambiguator})"),
            RefAction::ProbePhase(n) => write!(f, "ProbePhase({n})"),
            RefAction::User(_) => write!(f, "User(fn)"),
            RefAction::Phase(fns) => write!(f, "Phase({} fns)", fns.len()),
        }
    }
}

/// One emitted alternate: the engine's alt fields in the order they were
/// written, plus the compiler-internal mark `m`, which never reaches the
/// wire.
///
/// The fields are kept as an insertion-ordered map with JavaScript
/// assignment semantics (`set` on an existing key updates it in place, on
/// a new key appends it), because the canonical compiler builds each
/// alternate as an object literal and the serialised text is byte-stable
/// only when this port writes the keys in the same order at every site.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct AltSpec {
    fields: Map<String, Value>,
    pub m: Option<String>,
}

impl AltSpec {
    pub fn new() -> Self {
        Self::default()
    }

    /// `{ g: tag }`, the shape most alternates start from.
    pub fn with_tag(tag: &str) -> Self {
        let mut alt = Self::default();
        alt.set("g", tag);
        alt
    }

    /// Assign a field: an existing key keeps its position, a new one is
    /// appended.
    pub fn set(&mut self, key: &str, value: impl Into<Value>) -> &mut Self {
        self.fields.insert(key.to_string(), value.into());
        self
    }

    pub fn remove(&mut self, key: &str) -> Option<Value> {
        self.fields.shift_remove(key)
    }

    pub fn get(&self, key: &str) -> Option<&Value> {
        self.fields.get(key)
    }

    pub fn contains(&self, key: &str) -> bool {
        self.fields.contains_key(key)
    }

    /// Copy every field of `other` onto this alternate, in `other`'s
    /// order: the JavaScript spread `{ ...this, ...other }`.
    pub fn assign_from(&mut self, other: &AltSpec) -> &mut Self {
        for (key, value) in &other.fields {
            self.fields.insert(key.clone(), value.clone());
        }
        self
    }

    fn str_field(&self, key: &str) -> Option<&str> {
        self.fields.get(key).and_then(Value::as_str)
    }

    /// The token sequence this alternate matches.
    pub fn s(&self) -> Option<&str> {
        self.str_field("s")
    }

    /// The rule this alternate pushes.
    pub fn p(&self) -> Option<&str> {
        self.str_field("p")
    }

    /// The rule this alternate replaces itself with.
    pub fn r(&self) -> Option<&str> {
        self.str_field("r")
    }

    /// The group tag.
    pub fn g(&self) -> Option<&str> {
        self.str_field("g")
    }

    /// How many matched tokens are pushed back.
    pub fn b(&self) -> Option<u64> {
        self.fields.get("b").and_then(Value::as_u64)
    }

    /// The action refs, as a list: a scalar `a` is one, an array is each.
    pub fn actions(&self) -> Vec<String> {
        match self.fields.get("a") {
            Some(Value::String(a)) => vec![a.clone()],
            Some(Value::Array(list)) => list
                .iter()
                .filter_map(|v| v.as_str().map(str::to_string))
                .collect(),
            _ => Vec::new(),
        }
    }

    /// Assign the action refs: none removes `a`, one is a scalar on the
    /// wire, several are an array.
    pub fn set_actions(&mut self, actions: &[&str]) {
        match actions {
            [] => {
                self.remove("a");
            }
            [one] => {
                self.set("a", *one);
            }
            many => {
                self.set("a", Value::Array(many.iter().map(|s| json!(s)).collect()));
            }
        }
    }

    /// Append an action ref, producing the engine's array-`a` form so the
    /// alt's own action runs first.
    pub fn append_action(&mut self, added: &str) {
        let next = match self.fields.get("a") {
            None | Some(Value::Null) => json!(added),
            Some(Value::Array(list)) => {
                let mut out = list.clone();
                out.push(json!(added));
                Value::Array(out)
            }
            Some(existing) => Value::Array(vec![existing.clone(), json!(added)]),
        };
        self.set("a", next);
    }

    /// The builtin config bag.
    pub fn k(&self) -> Option<&Map<String, Value>> {
        self.fields.get("k").and_then(Value::as_object)
    }

    pub fn k_mut(&mut self) -> Option<&mut Map<String, Value>> {
        self.fields.get_mut("k").and_then(Value::as_object_mut)
    }

    /// The alternate as the engine reads it, marks stripped.
    pub fn to_value(&self) -> Value {
        Value::Object(self.fields.clone())
    }
}

/// One emitted rule: its open alternates and, when it captures or
/// replaces, its close alternates.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct RuleSpec {
    pub open: Vec<AltSpec>,
    pub close: Option<Vec<AltSpec>>,
}

impl RuleSpec {
    pub fn to_value(&self) -> Value {
        let mut m = Map::new();
        m.insert(
            "open".into(),
            Value::Array(self.open.iter().map(AltSpec::to_value).collect()),
        );
        if let Some(close) = &self.close {
            m.insert(
                "close".into(),
                Value::Array(close.iter().map(AltSpec::to_value).collect()),
            );
        }
        Value::Object(m)
    }

    pub(crate) fn alts_mut(&mut self, phase: &str) -> &mut Vec<AltSpec> {
        if phase == "open" {
            &mut self.open
        } else {
            self.close.get_or_insert_with(Vec::new)
        }
    }

    pub(crate) fn alts(&self, phase: &str) -> &[AltSpec] {
        if phase == "open" {
            &self.open
        } else {
            self.close.as_deref().unwrap_or(&[])
        }
    }
}

/// The emitted grammar.
///
/// `options` is the engine's options block as data (`fixed.token`,
/// `match.token`, `rule.start`, `lex.empty`, `tokenSet`); a front-end may
/// add its own lexer settings to it. `rule` maps each rule name to its
/// spec, or to `None` for a removal. `refs` holds the closure descriptors
/// of a closure-mode emit and any attached user actions; it is empty for
/// a `builtins: true` emit with no actions attached, which is what makes
/// the spec pure data.
#[derive(Debug, Clone, Default)]
pub struct GrammarSpec {
    pub refs: IndexMap<String, RefAction>,
    pub options: Map<String, Value>,
    pub rule: IndexMap<String, Option<RuleSpec>>,
    /// Engine-ignored tool metadata: `{ "provenance": { generated: origin } }`.
    pub meta: Option<Value>,
    /// `<all> = <remove>`: wipe the instance before applying the rest.
    pub clear: bool,
}

impl GrammarSpec {
    /// The `rule` block as data, marks stripped.
    fn rules_value(&self) -> Value {
        let mut m = Map::new();
        for (name, rule) in &self.rule {
            m.insert(
                name.clone(),
                match rule {
                    Some(r) => r.to_value(),
                    None => Value::Null,
                },
            );
        }
        Value::Object(m)
    }

    /// The document the engine loads: `options`, `rule`, `meta` and
    /// `clear`. Closure refs are names in the alternates and nothing more;
    /// [`bind`](Self::bind) registers what they stand for.
    pub fn to_value(&self) -> Value {
        let mut m = Map::new();
        m.insert("options".into(), Value::Object(self.options.clone()));
        m.insert("rule".into(), self.rules_value());
        if let Some(meta) = &self.meta {
            m.insert("meta".into(), meta.clone());
        }
        if self.clear {
            m.insert("clear".into(), json!(true));
        }
        Value::Object(m)
    }

    /// The document as the engine's own type.
    pub fn to_engine(&self) -> Result<tabnas::GrammarSpec, GrammarError> {
        tabnas::GrammarSpec::from_value(self.to_value())
    }

    /// Whether the spec still names functions: a closure-mode emit, or
    /// user actions attached by [`attach_actions`].
    pub fn has_closures(&self) -> bool {
        !self.refs.is_empty()
    }

    /// Register every `@`-ref this spec names as an engine callback, so
    /// the document loads. Call before `grammar()`; [`install`](Self::install)
    /// does both.
    pub fn bind(&self, parser: &mut Tabnas) {
        for (name, action) in &self.refs {
            bind_ref(parser, name, action);
        }
    }

    /// Bind the refs and install the grammar on a parser.
    pub fn install(&self, parser: &mut Tabnas) -> Result<(), GrammarError> {
        self.bind(parser);
        let engine = self.to_engine()?;
        parser.grammar(&engine)?;
        Ok(())
    }
}

fn ast_node(rule: &str, kind: NodeKind) -> tabnas::Value {
    let mut node = IndexMap::new();
    if kind == NodeKind::User {
        node.insert("rule".to_string(), tabnas::Value::String(rule.to_string()));
    }
    node.insert("src".to_string(), tabnas::Value::String(String::new()));
    node.insert("kids".to_string(), tabnas::Value::array(Vec::new()));
    tabnas::Value::object(node)
}

fn append_src(node: &mut IndexMap<String, tabnas::Value>, source: &str) {
    if let Some(tabnas::Value::String(current)) = node.get_mut("src") {
        current.push_str(source);
    }
}

fn append_kid(node: &mut IndexMap<String, tabnas::Value>, child: tabnas::Value) {
    if let Some(kids) = node.get_mut("kids").and_then(tabnas::Value::as_array_mut) {
        kids.push(child);
    }
}

/// Merge a returned child node into a rule's own: tagged children (user
/// rules) are pushed verbatim into `kids`; untagged ones flatten, their
/// `src` appending and their `kids` extending.
fn capture_child(node: &mut IndexMap<String, tabnas::Value>, child: tabnas::Value) {
    if let tabnas::Value::Object(child_map) = &child {
        if let Some(tabnas::Value::String(source)) = child_map.get("src") {
            append_src(node, source);
            if child_map
                .get("rule")
                .is_some_and(|v| !matches!(v, tabnas::Value::String(r) if r.is_empty()))
            {
                append_kid(node, child);
            } else if let Some(tabnas::Value::Array(children)) = child_map.get("kids") {
                for nested in children.iter() {
                    append_kid(node, nested.clone());
                }
            }
            return;
        }
    }
    // Legacy shape: wrap as a leaf kid.
    append_kid(node, child);
}

fn phase_of(rule: &Rule) -> u8 {
    match rule.k.get("pd_phase") {
        Some(tabnas::Value::Number(n)) => *n as u8,
        _ => 0,
    }
}

/// Register one closure-mode ref on the parser. Each closure is the twin
/// of the engine builtin it stands in for, so closure mode and builtins
/// mode produce the same tree.
fn bind_ref(parser: &mut Tabnas, name: &str, action: &RefAction) {
    match action.clone() {
        RefAction::Node {
            init,
            rule,
            kind,
            nterms,
        } => {
            parser.action(name, move |r: &mut Rule| {
                if init {
                    r.node = Rc::new(RefCell::new(ast_node(&rule, kind)));
                }
                let sources: Vec<String> =
                    r.o.iter().take(nterms).map(|t| t.src.to_string()).collect();
                if let Some(node) = r.node.borrow_mut().as_object_mut() {
                    for src in &sources {
                        append_src(node, src);
                    }
                }
            });
        }
        RefAction::Capture { rule, kind } => {
            parser.action(name, move |r: &mut Rule| {
                if r.node.borrow().is_undefined() {
                    r.node = Rc::new(RefCell::new(ast_node(&rule, kind)));
                }
                if r.child_node.is_undefined() {
                    return;
                }
                let child = r.child_node.clone();
                if let Some(node) = r.node.borrow_mut().as_object_mut() {
                    capture_child(node, child);
                }
            });
        }
        RefAction::Bubble => {
            parser.action(name, |r: &mut Rule| {
                if !r.child_node.is_undefined() {
                    r.node = Rc::new(RefCell::new(r.child_node.clone()));
                }
            });
        }
        RefAction::Fold { c_n } => {
            parser.action(name, move |r: &mut Rule| {
                if let Some(parent_node) = r.parent_node.clone() {
                    let same_node = Rc::ptr_eq(&parent_node, &r.node);
                    let own = r.node.borrow().clone();
                    let closes: Vec<String> =
                        r.c.iter().take(c_n).map(|t| t.src.to_string()).collect();
                    if let Some(parent) = parent_node.borrow_mut().as_object_mut() {
                        if parent.contains_key("src") {
                            if !same_node {
                                if let tabnas::Value::Object(own_map) = &own {
                                    if own_map.contains_key("src") {
                                        capture_child(parent, own);
                                    }
                                }
                            }
                            for src in &closes {
                                append_src(parent, src);
                            }
                        }
                    }
                }
                r.node = Rc::new(RefCell::new(tabnas::Value::Undefined));
            });
        }
        RefAction::ProbeInit => {
            parser.action_with_context(name, |r: &mut Rule, ctx: &mut Context| {
                r.k_mut()
                    .insert("pd_phase".into(), tabnas::Value::Number(0.0));
                let mark = tabnas::Value::Number(ctx.mark() as f64);
                r.k_mut().insert("pd_mark".into(), mark);
                Ok(())
            });
        }
        RefAction::ProbeDecide { disambiguator } => {
            parser.action_with_context(name, move |r: &mut Rule, ctx: &mut Context| {
                // The first token the probe did not consume. The probe
                // never fails, so this always reflects a real position.
                let peek = ctx.t0().map(|t| t.name.to_string());
                let mark = match r.k.get("pd_mark") {
                    Some(tabnas::Value::Number(m)) => *m as usize,
                    _ => ctx.mark(),
                };
                ctx.rewind(mark)?;
                let matched = peek.as_deref() == Some(disambiguator.as_str());
                r.k_mut().insert(
                    "pd_phase".into(),
                    tabnas::Value::Number(if matched { 1.0 } else { 2.0 }),
                );
                Ok(())
            });
        }
        RefAction::ProbePhase(phase) => {
            parser.alt_condition(name, move |r: &mut Rule, _ctx: &mut Context| {
                phase_of(r) == phase
            });
        }
        RefAction::User(f) => {
            parser.action_with_context(name, move |r: &mut Rule, ctx: &mut Context| f(r, ctx));
        }
        RefAction::Phase(fns) => {
            parser.state_action_ref(name, move |r: &mut Rule, ctx: &mut Context| {
                for f in &fns {
                    f(r, ctx)?;
                }
                Ok(())
            });
        }
    }
}

/// A grammar that cannot be reduced to pure data.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CompileError {
    pub message: String,
    pub rules: Vec<String>,
}

impl fmt::Display for CompileError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.message)
    }
}

impl std::error::Error for CompileError {}

/// Hook fields whose string value is a `@`-ref into the spec's `ref`
/// map: the AST-building actions recognition mode drops.
const REF_FIELDS: [&str; 3] = ["a", "bo", "bc"];

/// Output-building `$`-builtins, dropped when a spec is reduced to pure
/// recognition. The value builders belong here for the same reason the
/// tree builders do: a recognition-only grammar must recognise and build
/// nothing.
const TREE_BUILTINS: [&str; 11] = [
    "@node$",
    "@capture$",
    "@bubble$",
    "@fold$",
    "@object$",
    "@array$",
    "@reset$",
    "@key$",
    "@setval$",
    "@push$",
    "@value$",
];
const TREE_CONFIG_KEYS: [&str; 9] = [
    "node$", "capture$", "fold$", "object$", "array$", "key$", "setval$", "push$", "value$",
];

/// Like a plain deep copy, but dropping the AST-building hooks: `a`
/// fields that point at a dropped action, and the now-orphaned tree
/// config in `k`. Control builtins and structural fields are preserved.
fn clone_recognition(v: &Value, is_dropped: &dyn Fn(&str) -> bool) -> Value {
    match v {
        Value::Array(items) => Value::Array(
            items
                .iter()
                .map(|x| clone_recognition(x, is_dropped))
                .collect(),
        ),
        Value::Object(map) => {
            let mut o = Map::new();
            for (k, x) in map {
                if REF_FIELDS.contains(&k.as_str()) {
                    match x {
                        Value::String(s) => {
                            if is_dropped(s) {
                                continue;
                            }
                        }
                        Value::Array(list) => {
                            // Array-`a` composition: filter the dropped
                            // actions out of the list rather than keeping
                            // the whole list because it is not a string.
                            let kept: Vec<Value> = list
                                .iter()
                                .filter(|e| !matches!(e, Value::String(s) if is_dropped(s)))
                                .cloned()
                                .collect();
                            if kept.is_empty() {
                                continue;
                            }
                            o.insert(
                                k.clone(),
                                if kept.len() == 1 {
                                    kept[0].clone()
                                } else {
                                    Value::Array(kept)
                                },
                            );
                            continue;
                        }
                        _ => {}
                    }
                }
                if k == "k" {
                    if let Value::Object(_) = x {
                        let kc = clone_recognition(x, is_dropped);
                        let Value::Object(mut kc) = kc else {
                            unreachable!()
                        };
                        for tk in TREE_CONFIG_KEYS {
                            kc.remove(tk);
                        }
                        if kc.is_empty() {
                            continue;
                        }
                        o.insert(k.clone(), Value::Object(kc));
                        continue;
                    }
                }
                o.insert(k.clone(), clone_recognition(x, is_dropped));
            }
            Value::Object(o)
        }
        _ => v.clone(),
    }
}

/// Rules that reference the ref map from a field OTHER than the droppable
/// AST hooks, which means control functions (probe guards and dispatch
/// actions). Their presence means the grammar cannot be represented
/// purely structurally.
fn control_ref_rules(rules: &Value, is_ref: &dyn Fn(&str) -> bool) -> Vec<String> {
    fn scan(o: &Value, rule: &str, is_ref: &dyn Fn(&str) -> bool, offenders: &mut Vec<String>) {
        match o {
            Value::Array(items) => {
                for x in items {
                    scan(x, rule, is_ref, offenders);
                }
            }
            Value::Object(map) => {
                for (k, x) in map {
                    match x {
                        Value::String(s) if !REF_FIELDS.contains(&k.as_str()) && is_ref(s) => {
                            if !offenders.iter().any(|r| r == rule) {
                                offenders.push(rule.to_string());
                            }
                        }
                        _ => scan(x, rule, is_ref, offenders),
                    }
                }
            }
            _ => {}
        }
    }
    let mut offenders: Vec<String> = Vec::new();
    if let Value::Object(rules) = rules {
        for (name, spec) in rules {
            scan(spec, name, is_ref, &mut offenders);
        }
    }
    offenders.sort_by_key(|s| s.encode_utf16().collect::<Vec<u16>>());
    offenders
}

fn carry_meta(from: &GrammarSpec, to: &mut Map<String, Value>) {
    if let Some(meta) = &from.meta {
        to.insert("meta".into(), meta.clone());
    }
}

/// Strip a converted spec down to a function-free recognition grammar,
/// dropping the tree and value builders. Fails for grammars whose
/// control logic is still closures (a probe dispatcher converted without
/// `builtins`).
pub fn to_recognition_spec(spec: &GrammarSpec) -> Result<Value, CompileError> {
    let is_ref = |s: &str| spec.refs.contains_key(s);
    let rules = spec.rules_value();
    let offenders = control_ref_rules(&rules, &is_ref);
    if !offenders.is_empty() {
        return Err(CompileError {
            message: format!(
                "{}: grammar needs control functions (probe / unbounded lookahead) and \
                 cannot be emitted as a pure recognition grammar; recompile with \
                 `builtins: true`. Offending rule(s): {}",
                diag_name(),
                offenders.join(", ")
            ),
            rules: offenders,
        });
    }
    let is_dropped = |s: &str| is_ref(s) || TREE_BUILTINS.contains(&s);
    let mut doc = Map::new();
    doc.insert("options".into(), Value::Object(spec.options.clone()));
    doc.insert("rule".into(), rules);
    let out = clone_recognition(&Value::Object(doc), &is_dropped);
    let Value::Object(mut out) = out else {
        unreachable!()
    };
    // Declare the builtin config-schema version so the engine can refuse
    // a grammar that needs a newer schema than it implements.
    out.insert("v".into(), json!(tabnas::grammar::BUILTIN_SCHEMA_VERSION));
    carry_meta(spec, &mut out);
    Ok(Value::Object(out))
}

/// Reduce a spec to a pure-data, function-free grammar that KEEPS the
/// AST-building `$`-builtins. Requires a `builtins: true` conversion with
/// no user actions attached: any remaining ref is refused.
pub fn to_pure_spec(spec: &GrammarSpec) -> Result<Value, CompileError> {
    if !spec.refs.is_empty() {
        let closures: Vec<&str> = spec.refs.keys().map(String::as_str).take(3).collect();
        return Err(CompileError {
            message: format!(
                "{}: spec still contains closures; convert with `builtins: true` for \
                 pure-data output. Stray ref(s): {}",
                diag_name(),
                closures.join(", ")
            ),
            rules: Vec::new(),
        });
    }
    let mut out = Map::new();
    out.insert("options".into(), Value::Object(spec.options.clone()));
    out.insert("rule".into(), spec.rules_value());
    out.insert("v".into(), json!(tabnas::grammar::BUILTIN_SCHEMA_VERSION));
    carry_meta(spec, &mut out);
    Ok(Value::Object(out))
}

/// How [`to_jsonic`] writes its text.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct JsonicOptions {
    /// Valid JSON (double quotes, comma-separated) rather than relaxed
    /// jsonic (bare identifier keys, single quotes, newline-separated).
    pub strict: bool,
    /// Spaces per level (default 2).
    pub indent: Option<usize>,
}

fn is_ident(k: &str) -> bool {
    let mut chars = k.chars();
    match chars.next() {
        Some(c) if c.is_ascii_alphabetic() || c == '_' || c == '$' => {}
        _ => return false,
    }
    chars.all(|c| c.is_ascii_alphanumeric() || c == '_' || c == '$')
}

/// The largest magnitude an integer can have and still be exactly a
/// JavaScript number: 2^53 - 1.
const JS_SAFE_INTEGER: u64 = 9_007_199_254_740_991;

/// JavaScript's `String(n)` for an `f64`, i.e. ECMAScript
/// `Number::toString` with radix 10.
///
/// The canonical serialiser is `String(v)` (`ts/src/spec.ts`), so the
/// emitted text only equals TypeScript's when this reproduces it. A
/// narrowing `as i64` does not: it saturates every magnitude above
/// `i64::MAX` to `9223372036854775807`, and it cannot express the
/// switch to exponent form that JavaScript makes at `1e21`.
///
/// The shape is the spec's: take the shortest decimal digit string `s`
/// (`k` digits) that reads back as the same `f64`, with `n` the
/// position of the decimal point, then print plain decimal while `n`
/// stays inside `(-6, 21]` and exponent form outside it.
fn js_number(f: f64) -> String {
    if f.is_nan() {
        return "NaN".to_string();
    }
    if f.is_infinite() {
        return if f > 0.0 { "Infinity" } else { "-Infinity" }.to_string();
    }
    // Covers -0.0, which JavaScript prints as "0".
    if f == 0.0 {
        return "0".to_string();
    }
    let magnitude = f.abs();
    // The spec's `s` and `n`: the fewest digits that read back as this
    // same `f64`, correctly rounded. Rust's fixed-precision `{:e}` is
    // correctly rounded and breaks ties to even, which is the rule the
    // spec states for the case two digit strings are equally close, so
    // the first width that round-trips gives the spec's digits. Plain
    // `{:e}` (shortest) is NOT a substitute: it breaks those ties the
    // other way, and prints 137839762462415.63 where JavaScript prints
    // 137839762462415.62. Seventeen digits always suffice for an `f64`.
    let (digits, exponent) = (0..17u32)
        .map(|p| format!("{magnitude:.*e}", p as usize))
        .find(|text| text.parse::<f64>() == Ok(magnitude))
        .map(|text| {
            let (mantissa, exponent) = text.split_once('e').expect("{:e} emits an exponent");
            (
                mantissa.chars().filter(|c| *c != '.').collect::<String>(),
                exponent
                    .parse::<i32>()
                    .expect("{:e} emits an integer exponent"),
            )
        })
        .expect("17 significant digits round-trip every finite f64");
    let k = digits.len() as i32;
    let n = exponent + 1;

    let body = if k <= n && n <= 21 {
        // 12 -> "12", 1e19 -> "10000000000000000000"
        format!("{}{}", digits, "0".repeat((n - k) as usize))
    } else if 0 < n && n <= 21 {
        // 1.5 -> "1.5"
        format!("{}.{}", &digits[..n as usize], &digits[n as usize..])
    } else if -6 < n && n <= 0 {
        // 1e-6 -> "0.000001"
        format!("0.{}{}", "0".repeat(-n as usize), digits)
    } else {
        // 1e21 -> "1e+21", 1e-7 -> "1e-7"
        let e = n - 1;
        let head = if k == 1 {
            digits.clone()
        } else {
            format!("{}.{}", &digits[..1], &digits[1..])
        };
        format!("{}e{}{}", head, if e < 0 { '-' } else { '+' }, e.abs())
    };
    if f < 0.0 {
        format!("-{body}")
    } else {
        body
    }
}

/// Serialise a (function-free) value as jsonic text. Regular expressions
/// are already `@/pattern/flags` strings in the data.
pub fn to_jsonic(value: &Value, opts: JsonicOptions) -> String {
    let strict = opts.strict;
    let ind = opts.indent.unwrap_or(2);
    let sep = if strict { ",\n" } else { "\n" };
    let pad = |n: usize| " ".repeat(ind * n);

    // Every C0 control character has to be escaped, not just newline:
    // strict mode promises valid JSON.
    fn quote(s: &str, ch: char) -> String {
        let mut out = String::with_capacity(s.len() + 2);
        out.push(ch);
        for c in s.chars() {
            match c {
                '\\' => out.push_str("\\\\"),
                c if c == ch => {
                    out.push('\\');
                    out.push(ch);
                }
                '\u{8}' => out.push_str("\\b"),
                '\t' => out.push_str("\\t"),
                '\n' => out.push_str("\\n"),
                '\u{c}' => out.push_str("\\f"),
                '\r' => out.push_str("\\r"),
                c if (c as u32) < 0x20 => out.push_str(&format!("\\u{:04x}", c as u32)),
                c => out.push(c),
            }
        }
        out.push(ch);
        out
    }
    let str_ = |s: &str| {
        if strict {
            quote(s, '"')
        } else {
            quote(s, '\'')
        }
    };
    let key = |k: &str| {
        if !strict && is_ident(k) {
            k.to_string()
        } else {
            quote(k, '"')
        }
    };

    fn number(n: &serde_json::Number) -> String {
        // An integer small enough to be a JavaScript number exactly
        // prints the same either way, so keep the exact form. Beyond
        // 2^53 the canonical compiler never HAD the exact value — its
        // number is an `f64` — so round the way it did before printing.
        if let Some(i) = n.as_i64() {
            if i.unsigned_abs() <= JS_SAFE_INTEGER {
                return i.to_string();
            }
            return js_number(i as f64);
        }
        if let Some(u) = n.as_u64() {
            if u <= JS_SAFE_INTEGER {
                return u.to_string();
            }
            return js_number(u as f64);
        }
        js_number(n.as_f64().unwrap_or(0.0))
    }

    fn ser(
        v: &Value,
        depth: usize,
        sep: &str,
        pad: &dyn Fn(usize) -> String,
        str_: &dyn Fn(&str) -> String,
        key: &dyn Fn(&str) -> String,
    ) -> String {
        match v {
            Value::Null => "null".into(),
            Value::Bool(b) => b.to_string(),
            Value::Number(n) => number(n),
            Value::String(s) => str_(s),
            Value::Array(items) => {
                if items.is_empty() {
                    return "[]".into();
                }
                let lines: Vec<String> = items
                    .iter()
                    .map(|x| {
                        format!(
                            "{}{}",
                            pad(depth + 1),
                            ser(x, depth + 1, sep, pad, str_, key)
                        )
                    })
                    .collect();
                format!("[\n{}\n{}]", lines.join(sep), pad(depth))
            }
            Value::Object(map) => {
                if map.is_empty() {
                    return "{}".into();
                }
                let lines: Vec<String> = map
                    .iter()
                    .map(|(k, x)| {
                        format!(
                            "{}{}: {}",
                            pad(depth + 1),
                            key(k),
                            ser(x, depth + 1, sep, pad, str_, key)
                        )
                    })
                    .collect();
                format!("{{\n{}\n{}}}", lines.join(sep), pad(depth))
            }
        }
    }

    ser(value, 0, sep, &pad, &str_, &key)
}

/// Options for [`compile_spec`].
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct CompileOptions {
    /// Emit a pure RECOGNITION grammar (tree building dropped). `false`
    /// emits the full AST grammar with the tree builtins retained, still
    /// pure data.
    pub recognition: bool,
    pub strict: bool,
    pub indent: Option<usize>,
}

impl Default for CompileOptions {
    fn default() -> Self {
        Self {
            recognition: true,
            strict: false,
            indent: None,
        }
    }
}

/// Serialise an already-converted spec as pure-data tabnas grammar text.
/// A front-end converts with `builtins: true` first and passes the result
/// here.
pub fn compile_spec(spec: &GrammarSpec, opts: CompileOptions) -> Result<String, CompileError> {
    let out = if opts.recognition {
        to_recognition_spec(spec)?
    } else {
        to_pure_spec(spec)?
    };
    Ok(to_jsonic(
        &out,
        JsonicOptions {
            strict: opts.strict,
            indent: opts.indent,
        },
    ))
}

/// A malformed or unresolvable user action ref.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ActionError {
    pub message: String,
}

impl fmt::Display for ActionError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.message)
    }
}

impl std::error::Error for ActionError {}

enum Target {
    Phase(String),
    Alts { phase: String, indices: Vec<usize> },
}

const PHASES: [&str; 4] = ["bo", "ao", "bc", "ac"];

/// Resolve `@<rule>:<sel>` against the spec: the matched alts (for o:/c:
/// selectors) or the rule-phase (for bo/ao/bc/ac).
fn resolve_target(spec: &GrammarSpec, key: &str) -> Result<(String, Target), ActionError> {
    // `$` is reserved for engine builtins; a user action ref may not
    // contain it.
    if key.contains('$') {
        return Err(ActionError {
            message: format!(
                "{}: '$' is reserved for engine builtins; user action ref '{}' may not contain '$'",
                diag_name(),
                key
            ),
        });
    }
    let malformed = || ActionError {
        message: format!(
            "{}: malformed action ref '{}' (expected @rule:phase or @rule:o|c:mark)",
            diag_name(),
            key
        ),
    };
    let body = key.strip_prefix('@').ok_or_else(malformed)?;
    let (rule, sel) = body.split_once(':').ok_or_else(malformed)?;
    if rule.is_empty() || sel.is_empty() {
        return Err(malformed());
    }
    let Some(Some(rs)) = spec.rule.get(rule) else {
        return Err(ActionError {
            message: format!(
                "{}: action ref '{}' targets unknown rule '{}'",
                diag_name(),
                key,
                rule
            ),
        });
    };
    if PHASES.contains(&sel) {
        return Ok((rule.to_string(), Target::Phase(sel.to_string())));
    }
    let (oc, mark) = match sel.split_once(':') {
        Some((oc @ ("o" | "c"), mark)) if !mark.is_empty() => (oc, mark),
        _ => {
            return Err(ActionError {
                message: format!("{}: malformed action ref '{}'", diag_name(), key),
            })
        }
    };
    let phase = if oc == "o" { "open" } else { "close" };
    let indices: Vec<usize> = rs
        .alts(phase)
        .iter()
        .enumerate()
        .filter(|(_, a)| a.m.as_deref() == Some(mark))
        .map(|(i, _)| i)
        .collect();
    if indices.is_empty() {
        return Err(ActionError {
            message: format!(
                "{}: action ref '{}' matches no {} alt with mark '{}' in rule '{}'",
                diag_name(),
                key,
                phase,
                mark,
                rule
            ),
        });
    }
    Ok((
        rule.to_string(),
        Target::Alts {
            phase: phase.to_string(),
            indices,
        },
    ))
}

/// Attach user semantic actions to a spec, in place. Keys are
/// `@<rule>:<phase>` (bo/ao/bc/ac) or `@<rule>:o|c:<mark>`; the
/// functions run AFTER the compiler's own action, in attachment order.
/// Alt actions are injected as the engine's array-`a` form. Fails for a
/// ref matching no rule, hook or marked alt.
pub fn attach_actions(spec: &mut GrammarSpec, actions: ActionsMap) -> Result<(), ActionError> {
    // Start past whatever the ref map already holds, so a second call
    // cannot reuse a name the first handed out.
    let mut counter = 0;
    while spec.refs.contains_key(&format!("@bnf_user{counter}")) {
        counter += 1;
    }

    for (key, fns) in actions {
        let (rule, target) = resolve_target(spec, &key)?;
        match target {
            Target::Phase(phase) => {
                // Rule-phase hook: reuse the engine's `@<rule>-<phase>`
                // auto-install.
                let fkey = format!("@{rule}-{phase}");
                match spec.refs.get_mut(&fkey) {
                    Some(RefAction::Phase(existing)) => existing.extend(fns),
                    _ => {
                        spec.refs.insert(fkey, RefAction::Phase(fns));
                    }
                }
            }
            Target::Alts { phase, indices } => {
                let fns_arc: Vec<ActionFn> = fns;
                let seq: ActionFn = if fns_arc.len() == 1 {
                    fns_arc[0].clone()
                } else {
                    Arc::new(move |r: &mut Rule, ctx: &mut Context| {
                        for f in &fns_arc {
                            f(r, ctx)?;
                        }
                        Ok(())
                    })
                };
                for i in indices {
                    let user_ref = format!("@bnf_user{counter}");
                    counter += 1;
                    spec.refs
                        .insert(user_ref.clone(), RefAction::User(seq.clone()));
                    let rs = spec
                        .rule
                        .get_mut(&rule)
                        .and_then(Option::as_mut)
                        .expect("resolved above");
                    rs.alts_mut(&phase)[i].append_action(&user_ref);
                }
            }
        }
    }
    Ok(())
}

/// Declare user-action SLOTS on a (pure-data) spec without supplying
/// functions: each `@<rule>:o|c:<mark>` ref name is injected into the
/// matched alt's array-`a`, to be resolved at load time from a
/// user-supplied ref map. Fails on unknown targets and on rule-phase
/// refs.
pub fn attach_action_slots(spec: &mut GrammarSpec, ref_names: &[&str]) -> Result<(), ActionError> {
    for name in ref_names {
        let (rule, target) = resolve_target(spec, name)?;
        match target {
            Target::Phase(_) => {
                return Err(ActionError {
                    message: format!(
                    "{}: slot '{}' is a rule-phase ref; slots are for @rule:o|c:mark alt actions",
                    diag_name(),
                    name
                ),
                })
            }
            Target::Alts { phase, indices } => {
                let rs = spec
                    .rule
                    .get_mut(&rule)
                    .and_then(Option::as_mut)
                    .expect("resolved above");
                for i in indices {
                    rs.alts_mut(&phase)[i].append_action(name);
                }
            }
        }
    }
    Ok(())
}

/// Human-readable listing of the marks the compiler assigned, one line
/// per marked alt: `<rule>  o|c:<mark>  s:<tokens> | p:<rule> | (empty)`.
pub fn mark_listing(spec: &GrammarSpec) -> String {
    let mut lines: Vec<String> = Vec::new();
    for (rule, entry) in &spec.rule {
        // A removal, not a rule: it has no alternates to mark.
        let Some(entry) = entry else { continue };
        for (phase, sym) in [("open", "o"), ("close", "c")] {
            for a in entry.alts(phase) {
                let Some(m) = &a.m else { continue };
                let what = match (a.s(), a.p()) {
                    (Some(s), _) if !s.is_empty() => format!("s:{s}"),
                    (_, Some(p)) if !p.is_empty() => format!("p:{p}"),
                    _ => "(empty)".to_string(),
                };
                lines.push(format!("{rule}  {sym}:{m}  {what}"));
            }
        }
    }
    lines.join("\n")
}
