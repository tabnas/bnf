// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

//! The transducer part of an emitted grammar, the Rust port of
//! `ts/src/transduce.ts`, which says what the part is and when a grammar
//! is read whole instead.
//!
//! The part is an alchemy program that makes the JSON events of the value
//! the grammar builds from its rule passes (alchemy's `ParserEvents/1`):
//! alchemy's declarative `transduce-tree`, with a spec keyed by rule name
//! that says what each rule's value actions do. `@object$` or `@array$`
//! on a rule's open alternates is its `:container`, which a rule its close
//! replaces it with carries on; `@key$` with a constant is its `:member`;
//! `@setval$` or `@push$` with `src` on its close makes it a `:text` rule.
//! The text is the same in every runtime: the entries in byte order of
//! their rule names, a member name quoted alike, every refusal checked in
//! the same order.

use std::collections::{BTreeMap, HashSet};

use serde_json::Value;

use crate::spec::{AltSpec, GrammarSpec};

/// The transducer part of an emitted grammar, or why its documents are
/// read whole.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum TransducePart {
    /// The part: `entry` is the definition it makes, `<name>-transduce`,
    /// a function of the input's parser events; `source` is its alchemy
    /// text.
    Part { entry: String, source: String },
    /// Why the grammar's documents are read whole.
    Whole(String),
}

/// What one rule's value actions do.
#[derive(Debug, Clone, Default)]
struct Entry {
    container: Option<&'static str>,
    /// The container is the one a rule it replaced handed on.
    continues: bool,
    member: Option<String>,
    /// What the member its close alternates fill holds: the text of the
    /// rule it pushed, or that rule's own value.
    fills: Option<Fills>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Fills {
    Text,
    Value,
}

/// The engine's value builders. The compiler emits the first five; the
/// other two are a refusal here.
const VALUE_ACTIONS: [&str; 7] = [
    "@object$", "@array$", "@key$", "@setval$", "@push$", "@value$", "@reset$",
];

/// A name alchemy reads as a keyword's, and as a definition's: a symbol
/// that is not a number, and not one of the value words. Every name the
/// compiler gives a rule is one.
fn is_symbol(name: &str) -> bool {
    let mut chars = name.chars();
    let Some(first) = chars.next() else {
        return false;
    };
    (first.is_ascii_alphabetic() || first == '_')
        && chars.all(|c| c.is_ascii_alphanumeric() || "_.$-".contains(c))
        && !matches!(name, "true" | "false" | "null")
}

/// An alchemy string: double quotes, a backslash before a quote or a
/// backslash, a control character as `\u00XX`, and every other character
/// as it is.
fn quoted(text: &str) -> String {
    let mut out = String::from("\"");
    for ch in text.chars() {
        match ch {
            '"' | '\\' => {
                out.push('\\');
                out.push(ch);
            }
            c if (c as u32) < 0x20 => out.push_str(&format!("\\u{:04x}", c as u32)),
            c => out.push(c),
        }
    }
    out.push('"');
    out
}

/// An alternate's value actions, each with the config the engine binds it
/// (`k.key$` for `@key$`), and the same as a comparable signature.
fn acts_of(alt: &AltSpec) -> (Vec<(String, Value)>, String) {
    let acts: Vec<(String, Value)> = alt
        .actions()
        .into_iter()
        .filter(|a| VALUE_ACTIONS.contains(&a.as_str()))
        .map(|a| {
            let cfg = alt
                .k()
                .and_then(|k| k.get(&a[1..]))
                .cloned()
                .unwrap_or(Value::Null);
            (a, cfg)
        })
        .collect();
    let signature = acts
        .iter()
        .map(|(a, cfg)| format!("{a} {cfg};"))
        .collect::<String>();
    (acts, signature)
}

fn whole(why: impl Into<String>) -> TransducePart {
    TransducePart::Whole(why.into())
}

/// The transducer part of an emitted grammar, defining `<name>-transduce`,
/// or why its documents are read whole (`ts/src/transduce.ts` says when).
pub fn transduce_alc(spec: &GrammarSpec, name: &str) -> TransducePart {
    if !is_symbol(name) {
        return whole(format!(
            "the name {} is not one alchemy can give a definition",
            quoted(name)
        ));
    }
    let mut names: Vec<&str> = spec
        .rule
        .iter()
        .filter(|(_, rs)| rs.is_some())
        .map(|(name, _)| name.as_str())
        .collect();
    names.sort_unstable();
    let rule = |name: &str| spec.rule.get(name).and_then(Option::as_ref);
    let alts = |name: &str, side: &str| -> Vec<AltSpec> {
        rule(name)
            .map(|r| r.alts(side).to_vec())
            .unwrap_or_default()
    };

    // What each rule's value actions do.
    let mut entries: BTreeMap<String, Entry> = BTreeMap::new();
    for &name in &names {
        let mut entry = Entry::default();
        for side in ["open", "close"] {
            let mut first: Option<Vec<(String, Value)>> = None;
            let mut signature = String::new();
            for alt in alts(name, side) {
                let (acts, sig) = acts_of(&alt);
                match first {
                    None => {
                        first = Some(acts);
                        signature = sig;
                    }
                    Some(_) if sig != signature => {
                        return whole(format!(
                            "rule {name}'s {side} alternates build its value differently"
                        ));
                    }
                    Some(_) => {}
                }
            }
            for (act, cfg) in first.unwrap_or_default() {
                let lit = cfg.get("lit").and_then(Value::as_str);
                match act.as_str() {
                    "@object$" | "@array$" if side == "open" && entry.container.is_none() => {
                        entry.container = Some(if act == "@object$" { "object" } else { "array" });
                    }
                    "@key$" if side == "open" && lit.is_some() && entry.member.is_none() => {
                        entry.member = lit.map(str::to_string);
                    }
                    "@setval$" | "@push$" if side == "close" && entry.fills.is_none() => {
                        entry.fills = Some(if cfg.get("src") == Some(&Value::Bool(true)) {
                            Fills::Text
                        } else {
                            Fills::Value
                        });
                    }
                    _ => {
                        return whole(format!(
                            "rule {name}'s {side} alternates use {act} as no transducer part reads it"
                        ));
                    }
                }
            }
        }
        if entry.container.is_some() || entry.member.is_some() || entry.fills.is_some() {
            entries.insert(name.to_string(), entry);
        }
    }

    // The rules a container rule's close replaces it with carry the
    // container on, and the rules theirs replace them with, to a fixed
    // point.
    let mut changed = true;
    while changed {
        changed = false;
        for &name in &names {
            let Some(container) = entries.get(name).and_then(|e| e.container) else {
                continue;
            };
            for alt in alts(name, "close") {
                let Some(next) = alt.r().filter(|r| !r.is_empty()) else {
                    continue;
                };
                if rule(next).is_none() {
                    return whole(format!(
                        "rule {name} hands its {container} on to rule {next}, which the grammar does not define"
                    ));
                }
                let target = entries.entry(next.to_string()).or_default();
                match target.container {
                    None => {
                        target.container = Some(container);
                        target.continues = true;
                        changed = true;
                    }
                    Some(kind) if kind != container => {
                        let article = if kind == "object" {
                            "an object"
                        } else {
                            "an array"
                        };
                        return whole(format!(
                            "rule {name} hands its {container} on to rule {next}, a rule of {article}"
                        ));
                    }
                    Some(_) if !target.continues => {
                        return whole(format!(
                            "rule {name} hands its {container} on to rule {next}, which begins one of its own"
                        ));
                    }
                    Some(_) => {}
                }
            }
        }
    }

    if let Some(bad) = entries.keys().find(|r| !is_symbol(r)) {
        return whole(format!(
            "rule {} has no name alchemy can write as a keyword",
            quoted(bad)
        ));
    }

    // A container ends, and a text is yielded, on its rule's close, which
    // an open alternate that replaces the rule skips.
    for (name, entry) in &entries {
        if entry.container.is_none() && entry.fills != Some(Fills::Text) {
            continue;
        }
        if alts(name, "open")
            .iter()
            .any(|alt| alt.r().is_some_and(|r| !r.is_empty()))
        {
            return whole(format!(
                "rule {name}'s open alternate replaces it, so its close never runs"
            ));
        }
    }

    // A member that is a rule's text holds no value of its own: a value
    // rule beneath it builds what the text does not hold.
    let beneath = |start: &str| -> Option<String> {
        let mut seen: HashSet<String> = HashSet::new();
        let mut stack = vec![start.to_string()];
        while let Some(name) = stack.pop() {
            if seen.contains(&name) || rule(&name).is_none() {
                continue;
            }
            seen.insert(name.clone());
            if entries.contains_key(&name) {
                return Some(name);
            }
            let mut next: Vec<String> = Vec::new();
            for side in ["open", "close"] {
                for alt in alts(&name, side) {
                    for to in [alt.p(), alt.r()].into_iter().flatten() {
                        if !to.is_empty() {
                            next.push(to.to_string());
                        }
                    }
                }
            }
            stack.extend(next.into_iter().rev());
        }
        None
    };
    for (name, entry) in &entries {
        if entry.fills != Some(Fills::Text) {
            continue;
        }
        for alt in alts(name, "open") {
            let Some(pushed) = alt.p().filter(|p| !p.is_empty()) else {
                continue;
            };
            if let Some(found) = beneath(pushed) {
                return whole(format!(
                    "rule {name}'s member is the text of rule {pushed}, beneath which rule {found} builds a value of its own"
                ));
            }
        }
    }

    // The value is the start rule's, which the compiler's start wrapper
    // pushes.
    let wrapper = spec
        .options
        .get("rule")
        .and_then(|r| r.get("start"))
        .and_then(Value::as_str);
    let start = wrapper.filter(|w| rule(w).is_some()).and_then(|w| {
        alts(w, "open")
            .iter()
            .find_map(|alt| alt.p().filter(|p| !p.is_empty()).map(str::to_string))
    });
    let Some(start) = start else {
        return whole("the grammar has no start rule a wrapper pushes");
    };
    match entries.get(&start) {
        Some(root) if root.container.is_some() && !root.continues => {}
        _ => {
            return whole(format!(
                "the start rule {start} has no annotation, so the value is the {{rule, src, kids}} tree, \
                 whose src comes before its kids and holds the text of every node beneath it: \
                 no stream writes the root before the document has ended"
            ));
        }
    }

    let entry = format!("{name}-transduce");
    let mut lines = vec![
        format!("; The transducer part of the grammar {name}: the JSON events of the value"),
        "; its @object and @array annotations build, from its rule passes, as".to_string(),
        "; tabnas-bnf wrote them from the value actions those annotations attach.".to_string(),
        format!("def {entry} [input]"),
        "  transduce-tree".to_string(),
        "    record".to_string(),
    ];
    for (rule, e) in &entries {
        let mut fields: Vec<String> = Vec::new();
        if let Some(container) = e.container {
            fields.push(format!("(entry :container :{container})"));
        }
        if let Some(member) = &e.member {
            fields.push(format!("(entry :member {})", quoted(member)));
        }
        if e.fills == Some(Fills::Text) {
            fields.push("(entry :text true)".to_string());
        }
        if !fields.is_empty() {
            lines.push(format!("      entry :{rule} (record {})", fields.join(" ")));
        }
    }
    lines.push("    input".to_string());
    lines.push(String::new());
    TransducePart::Part {
        entry,
        source: lines.join("\n"),
    }
}
