// Copyright (c) 2026 tabnas, MIT License

//! The schema `grammar-rules`: a grammar's rules as its text writes them,
//! uncompiled and in source order, which the ABNF, EBNF and GBNF front ends
//! read their documents as and write them from (alchemy's
//! `docs/streaming-plan.md`, section 13.1; `docs/grammar-rules.md` here).
//!
//! ```text
//! { "type": "grammar", ["start": name], "rules": [ rule, ... ] }
//! rule     { "type": "rule", "name", ["incremental": true], "alternatives": [ [ element, ... ], ... ] }
//! element  { "type", ...its members, ["repeat": [ { "min", ["max"] }, ... ]] }
//! ```
//!
//! Every object's members come in the schema's order, `type` first, since a
//! reader streams a tree as its rule passes reach each member. Rust port of
//! `ts/src/rules.ts`.

use serde_json::{Map, Value};

/// The largest code point.
pub const MAX_CODE_POINT: u64 = 0x10ffff;

/// A tree the schema refuses: `code` names what was wrong (`rules_root`,
/// `rules_type`, `rules_member`, `rules_value`) and `path` where.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RulesError {
    pub code: &'static str,
    pub path: String,
    pub message: String,
}

impl std::fmt::Display for RulesError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "grammar-rules: {}: {}", self.path, self.message)
    }
}

impl std::error::Error for RulesError {}

fn fail(code: &'static str, path: &str, message: &str) -> RulesError {
    RulesError {
        code,
        path: path.to_string(),
        message: message.to_string(),
    }
}

type Check = fn(&Value, &str) -> Result<(), RulesError>;

/// A member of a node: its name, whether every node of the kind has it,
/// and the check its value meets.
type Member = (&'static str, bool, Check);

fn any(_: &Value, _: &str) -> Result<(), RulesError> {
    Ok(())
}

fn text(v: &Value, at: &str) -> Result<(), RulesError> {
    if v.is_string() {
        Ok(())
    } else {
        Err(fail("rules_value", at, "is not text"))
    }
}

fn name(v: &Value, at: &str) -> Result<(), RulesError> {
    match v.as_str() {
        None => Err(fail("rules_value", at, "is not text")),
        Some("") => Err(fail("rules_value", at, "is empty")),
        Some(_) => Ok(()),
    }
}

fn boolean(v: &Value, at: &str) -> Result<(), RulesError> {
    if v.is_boolean() {
        Ok(())
    } else {
        Err(fail("rules_value", at, "is not true or false"))
    }
}

fn yes(v: &Value, at: &str) -> Result<(), RulesError> {
    if Some(true) == v.as_bool() {
        Ok(())
    } else {
        Err(fail("rules_value", at, "is true where it is written"))
    }
}

/// A whole number, as JSON spells one: an integer, or a float with no
/// fraction, which TypeScript's `Number.isInteger` takes.
fn whole(v: &Value) -> Option<u64> {
    if let Some(n) = v.as_u64() {
        return Some(n);
    }
    let f = v.as_f64()?;
    // `f.fract() == 0` is a float compared exactly, which clippy forbids;
    // the round trip through the integer says the same.
    if f >= 0.0 && f <= u64::MAX as f64 && (f as u64) as f64 == f {
        Some(f as u64)
    } else {
        None
    }
}

fn count(v: &Value, at: &str) -> Result<(), RulesError> {
    whole(v)
        .map(|_| ())
        .ok_or_else(|| fail("rules_value", at, "is not a whole number"))
}

fn code_point(v: &Value, at: &str) -> Result<(), RulesError> {
    match whole(v) {
        Some(n) if n <= MAX_CODE_POINT => Ok(()),
        _ => Err(fail("rules_value", at, "is not a code point")),
    }
}

/// An object's members, held to the kind's list: each in the list's order,
/// none twice, every required one there, and no other.
fn members(node: &Map<String, Value>, list: &[Member], at: &str) -> Result<(), RulesError> {
    let mut next = 0;
    for (key, value) in node {
        let Some(found) = (next..list.len()).find(|&i| list[i].0 == key) else {
            let message = if list.iter().any(|m| m.0 == key) {
                "comes out of its place, or twice"
            } else {
                "is no member of this node"
            };
            return Err(fail("rules_member", &format!("{at}.{key}"), message));
        };
        if let Some(missing) = list[next..found].iter().find(|m| m.1) {
            return Err(fail(
                "rules_member",
                &format!("{at}.{}", missing.0),
                "is missing",
            ));
        }
        (list[found].2)(value, &format!("{at}.{key}"))?;
        next = found + 1;
    }
    if let Some(missing) = list[next..].iter().find(|m| m.1) {
        return Err(fail(
            "rules_member",
            &format!("{at}.{}", missing.0),
            "is missing",
        ));
    }
    Ok(())
}

fn span(node: &Map<String, Value>, at: &str) -> Result<(), RulesError> {
    let from = node.get("from").and_then(whole).unwrap_or(0);
    let to = node.get("to").and_then(whole).unwrap_or(0);
    if to < from {
        Err(fail(
            "rules_value",
            &format!("{at}.to"),
            "is less than from",
        ))
    } else {
        Ok(())
    }
}

fn repeats(v: &Value, at: &str) -> Result<(), RulesError> {
    let list = match v.as_array() {
        Some(list) if !list.is_empty() => list,
        _ => return Err(fail("rules_value", at, "is not a list of repetitions")),
    };
    for (i, r) in list.iter().enumerate() {
        let rat = format!("{at}[{i}]");
        let Some(r) = r.as_object() else {
            return Err(fail("rules_value", &rat, "is not a repetition"));
        };
        members(r, &[("min", true, count), ("max", false, count)], &rat)?;
        if let (Some(max), Some(min)) = (r.get("max").and_then(whole), r.get("min").and_then(whole))
        {
            if max < min {
                return Err(fail(
                    "rules_value",
                    &format!("{rat}.max"),
                    "is less than min",
                ));
            }
        }
    }
    Ok(())
}

fn class_members(v: &Value, at: &str) -> Result<(), RulesError> {
    let Some(list) = v.as_array() else {
        return Err(fail("rules_value", at, "is not a list"));
    };
    for (i, m) in list.iter().enumerate() {
        let mat = format!("{at}[{i}]");
        let Some(m) = m.as_object() else {
            return Err(fail("rules_value", &mat, "is not a span of code points"));
        };
        members(
            m,
            &[("from", true, code_point), ("to", true, code_point)],
            &mat,
        )?;
        span(m, &mat)?;
    }
    Ok(())
}

/// An element type's members, between its `type` and its `repeat`.
fn element_members(kind: &str) -> Option<&'static [Member]> {
    Some(match kind {
        "reference" => &[("name", true, name)],
        "literal" => &[("text", true, text), ("caseSensitive", true, boolean)],
        "char" => &[("value", true, code_point)],
        "range" => &[("from", true, code_point), ("to", true, code_point)],
        "class" => &[("negated", true, boolean), ("members", true, class_members)],
        "any" => &[],
        "token" => &[("negated", true, boolean), ("text", true, text)],
        "prose" => &[("text", true, text)],
        "group" => &[("alternatives", true, alternatives)],
        _ => return None,
    })
}

/// Whether an object's first member is its type.
fn typed(node: &Map<String, Value>) -> Option<&str> {
    match node.iter().next() {
        Some((key, Value::String(kind))) if "type" == key => Some(kind),
        _ => None,
    }
}

fn element(v: &Value, at: &str) -> Result<(), RulesError> {
    let Some(node) = v.as_object() else {
        return Err(fail("rules_value", at, "is not an element"));
    };
    let kind = typed(node).unwrap_or("");
    let Some(list) = element_members(kind) else {
        return Err(fail(
            "rules_type",
            at,
            &format!(
                "is no element: its first member is not a type an element has ({})",
                node.get("type")
                    .map_or("undefined".to_string(), Value::to_string)
            ),
        ));
    };
    let mut full: Vec<Member> = vec![("type", true, any)];
    full.extend_from_slice(list);
    full.push(("repeat", false, repeats));
    members(node, &full, at)?;
    if "range" == kind {
        span(node, at)?;
    }
    Ok(())
}

fn alternatives(v: &Value, at: &str) -> Result<(), RulesError> {
    let list = match v.as_array() {
        Some(list) if !list.is_empty() => list,
        _ => return Err(fail("rules_value", at, "is not a list of alternatives")),
    };
    for (i, alt) in list.iter().enumerate() {
        let aat = format!("{at}[{i}]");
        let Some(elements) = alt.as_array() else {
            return Err(fail("rules_value", &aat, "is not a list of elements"));
        };
        for (j, el) in elements.iter().enumerate() {
            element(el, &format!("{aat}[{j}]"))?;
        }
    }
    Ok(())
}

fn rule(v: &Value, at: &str) -> Result<(), RulesError> {
    match v.as_object() {
        Some(node) if Some("rule") == typed(node) => members(
            node,
            &[
                ("type", true, any),
                ("name", true, name),
                ("incremental", false, yes),
                ("alternatives", true, alternatives),
            ],
            at,
        ),
        _ => Err(fail(
            "rules_type",
            at,
            "is no rule: its first member is not the type rule",
        )),
    }
}

fn rules(v: &Value, at: &str) -> Result<(), RulesError> {
    let Some(list) = v.as_array() else {
        return Err(fail("rules_value", at, "is not a list of rules"));
    };
    for (i, r) in list.iter().enumerate() {
        rule(r, &format!("{at}[{i}]"))?;
    }
    Ok(())
}

/// Hold a value to the schema `grammar-rules`, its objects' members in
/// their order, or answer a [`RulesError`] naming what was wrong and where.
///
/// ```
/// let tree = serde_json::json!({"type": "grammar", "rules": [
///     {"type": "rule", "name": "a", "alternatives": [[{"type": "literal", "text": "x", "caseSensitive": true}]]}
/// ]});
/// assert!(tabnas_bnf::check_rules(&tree).is_ok());
/// let refused = tabnas_bnf::check_rules(&serde_json::json!([])).unwrap_err();
/// assert_eq!(refused.code, "rules_root");
/// ```
pub fn check_rules(tree: &Value) -> Result<(), RulesError> {
    match tree.as_object() {
        Some(node) if Some("grammar") == typed(node) => members(
            node,
            &[
                ("type", true, any),
                ("start", false, name),
                ("rules", true, rules),
            ],
            "$",
        ),
        _ => Err(fail(
            "rules_root",
            "$",
            "is not a grammar: its first member is not the type grammar",
        )),
    }
}
