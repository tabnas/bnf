// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

//! The passes that run on the grammar as written, before any structural
//! rewrite: prose resolution, literal lifting, built-in token
//! normalisation and the nullability of the start rule. Mirrors
//! `resolveProseTerminals`, `liftLiteralTokens`, `normalizeBuiltinTokens`
//! and `nullableRules` in `ts/src/compiler.ts`.

use indexmap::{IndexMap, IndexSet};

use crate::ir::{
    builtin_token, diag_name, is_prose_name, term_key, Element, EmitError, Grammar, Kind, NodeKind,
    Production, Sequence, BUILTIN_TOKENS, REMOVE_ALL, REMOVE_PROSE,
};

/// Resolve RFC 5234 `prose-val` terminals (`<free text>`).
///
/// Prose is informational, so the compiler accepts it in exactly one
/// position: as the ENTIRE body of a production naming a built-in lexer
/// token (`NR = <number>`), which is then dropped so references fall
/// through to the built-in. `<remove>` is the one prose form that does
/// compile to something: a removal directive. Prose anywhere else is an
/// error.
pub(crate) fn resolve_prose_terminals(grammar: &mut Grammar) -> Result<(), EmitError> {
    let mut kept: Vec<Production> = Vec::new();

    fn find_stray(el: &Element) -> Option<&Element> {
        match &el.kind {
            Kind::Prose { .. } => Some(el),
            Kind::Opt { inner }
            | Kind::Star { inner, .. }
            | Kind::Plus { inner }
            | Kind::Rep { inner, .. } => find_stray(inner),
            Kind::Group { alts } => alts.iter().flatten().find_map(find_stray),
            _ => None,
        }
    }

    for prod in grammar.productions.drain(..) {
        let only_prose = prod.alts.len() == 1
            && prod.alts[0].len() == 1
            && matches!(prod.alts[0][0].kind, Kind::Prose { .. });

        // A prose name is only ever the removal directive. Checked here as
        // well as in the prose-body branch below, because `<all> = "x"` has
        // a LITERAL body and would otherwise be lifted into a token
        // literally named `#<all>`.
        if is_prose_name(&prod.name) && !only_prose {
            return Err(EmitError::new(format!(
                "{}: '{}' is prose, and prose is only valid as a production name for \
                 the removal directive '<all> = <remove>'.",
                diag_name(),
                prod.name
            )));
        }

        if only_prose {
            let Kind::Prose { text } = &prod.alts[0][0].kind else {
                unreachable!()
            };

            if REMOVE_PROSE == text.trim().to_lowercase() {
                if is_prose_name(&prod.name) {
                    let target = prod.name[1..prod.name.len() - 1].trim().to_lowercase();
                    if REMOVE_ALL != target {
                        return Err(EmitError::new(format!(
                            "{}: '<{}>' is not a removal target. The only prose name is \
                             '<all>', as in '<all> = <remove>', which clears the whole \
                             grammar. To remove one rule or token, name it directly: \
                             '{} = <remove>'.",
                            diag_name(),
                            target,
                            target
                        )));
                    }
                    grammar.clear_all = true;
                } else {
                    grammar.remove.push(prod.name.clone());
                }
                continue;
            }

            if is_prose_name(&prod.name) {
                return Err(EmitError::new(format!(
                    "{}: '{}' is prose, and prose is only valid as a production name \
                     for the removal directive '<all> = <remove>'.",
                    diag_name(),
                    prod.name
                )));
            }

            if builtin_token(&prod.name).is_some() {
                continue; // informational: the lexer defines it
            }
            return Err(EmitError::at(
                format!(
                    "{}: rule '{}' is defined only by prose ('<{}>'), which describes a \
                     terminal but does not define one. Prose is allowed only for \
                     built-in lexer tokens ({}).",
                    diag_name(),
                    prod.name,
                    text,
                    BUILTIN_TOKENS
                        .iter()
                        .map(|(bare, _)| *bare)
                        .collect::<Vec<_>>()
                        .join(", ")
                ),
                &prod.name,
                prod.sp,
            ));
        }

        // Any surviving prose is embedded in a larger expression, where it
        // cannot be given a meaning.
        for alt in &prod.alts {
            for el in alt {
                if let Some(stray) = find_stray(el) {
                    let Kind::Prose { text } = &stray.kind else {
                        unreachable!()
                    };
                    return Err(EmitError::at(
                        format!(
                            "{}: rule '{}' uses prose ('<{}>') inside an expression; prose \
                             may only stand alone as the whole definition of a built-in \
                             lexer token.",
                            diag_name(),
                            prod.name,
                            text
                        ),
                        &prod.name,
                        stray.sp.or(prod.sp),
                    ));
                }
            }
        }
        kept.push(prod);
    }

    if kept.is_empty() && !grammar.clear_all && grammar.remove.is_empty() {
        return Err(EmitError::new(format!(
            "{}: grammar defines no rules — only informational prose terminals.",
            diag_name()
        )));
    }

    grammar.productions = kept;
    Ok(())
}

/// Token names the engine's own matchers produce. A production so named
/// is never lifted: `TX = "literal"` stays an ordinary rule shadowing the
/// bareword, since the engine refuses to bind a matcher-owned name to a
/// fixed literal. This is the engine's `MATCHER_TOKEN_NAMES`.
pub(crate) fn is_matcher_token_name(name: &str) -> bool {
    matches!(
        name,
        "BD" | "ZZ" | "UK" | "AA" | "SP" | "LN" | "CM" | "NR" | "ST" | "TX" | "VL"
    )
}

/// Lift single-literal productions into NAMED lexer tokens.
///
/// A production whose whole body is one string literal (`PL = "+"`) is a
/// lexical definition, not a syntactic rule: lifting binds the literal to
/// `#PL` directly and drops the rule. The start rule is never lifted,
/// multi-alternative productions are real choices and are left alone, as
/// are the core rules, annotated rules, matcher-owned names and the empty
/// literal. Two names claiming one literal cancel each other. Returns
/// every lifted definition, referenced or not, so the emitter can
/// allocate the token even when nothing references it.
pub(crate) fn lift_literal_tokens(grammar: &mut Grammar, start: &str) -> Vec<Element> {
    let mut lifted: IndexMap<String, (String, Option<bool>)> = IndexMap::new();

    for prod in &grammar.productions {
        if prod.name == start
            || is_matcher_token_name(&prod.name)
            || prod.node_kind == NodeKind::Core
            || prod.value.is_some()
        {
            continue;
        }
        if prod.alts.len() != 1 || prod.alts[0].len() != 1 {
            continue;
        }
        let Kind::Term {
            literal,
            case_sensitive,
            ..
        } = &prod.alts[0][0].kind
        else {
            continue;
        };
        // `path-empty = ""` matches the empty string: a rule that derives
        // epsilon, not a token the lexer could ever emit.
        if literal.is_empty() {
            continue;
        }
        lifted.insert(prod.name.clone(), (literal.clone(), *case_sensitive));
    }

    // The engine keys its fixed tokens by literal, so one literal is one
    // token: when two names claim the same literal, neither is lifted.
    let mut by_literal: IndexMap<String, Vec<String>> = IndexMap::new();
    for (name, (literal, cs)) in &lifted {
        by_literal
            .entry(term_key(literal, *cs))
            .or_default()
            .push(name.clone());
    }
    for names in by_literal.values() {
        if names.len() > 1 {
            for n in names {
                lifted.shift_remove(n);
            }
        }
    }

    if lifted.is_empty() {
        return Vec::new();
    }

    let mk = |name: &str, literal: &str, cs: Option<bool>| Element {
        kind: Kind::Term {
            literal: literal.to_string(),
            case_sensitive: cs,
            token_name: Some(name.to_string()),
        },
        sp: None,
    };

    fn walk(el: &Element, lifted: &IndexMap<String, (String, Option<bool>)>) -> Element {
        match &el.kind {
            Kind::Ref { name, .. } => match lifted.get(name) {
                Some((literal, cs)) => Element {
                    kind: Kind::Term {
                        literal: literal.clone(),
                        case_sensitive: *cs,
                        token_name: Some(name.clone()),
                    },
                    sp: None,
                },
                None => el.clone(),
            },
            Kind::Opt { inner } => Element {
                kind: Kind::Opt {
                    inner: Box::new(walk(inner, lifted)),
                },
                sp: el.sp,
            },
            Kind::Star { inner, debt_guard } => Element {
                kind: Kind::Star {
                    inner: Box::new(walk(inner, lifted)),
                    debt_guard: debt_guard.clone(),
                },
                sp: el.sp,
            },
            Kind::Plus { inner } => Element {
                kind: Kind::Plus {
                    inner: Box::new(walk(inner, lifted)),
                },
                sp: el.sp,
            },
            Kind::Rep { min, max, inner } => Element {
                kind: Kind::Rep {
                    min: *min,
                    max: *max,
                    inner: Box::new(walk(inner, lifted)),
                },
                sp: el.sp,
            },
            Kind::Group { alts } => Element::group(
                alts.iter()
                    .map(|a| a.iter().map(|e| walk(e, lifted)).collect())
                    .collect(),
            ),
            _ => el.clone(),
        }
    }

    grammar.productions = grammar
        .productions
        .drain(..)
        .filter(|p| !lifted.contains_key(&p.name))
        .map(|mut p| {
            p.alts = p
                .alts
                .iter()
                .map(|alt| alt.iter().map(|e| walk(e, &lifted)).collect::<Sequence>())
                .collect();
            p
        })
        .collect();

    lifted
        .iter()
        .map(|(name, (literal, cs))| mk(name, literal, *cs))
        .collect()
}

/// Rewrite every bareword reference whose name is a built-in token AND is
/// not a defined production into a token terminal.
pub(crate) fn normalize_builtin_tokens(grammar: &mut Grammar) {
    let defined: IndexSet<String> = grammar.productions.iter().map(|p| p.name.clone()).collect();

    fn walk(el: &Element, defined: &IndexSet<String>) -> Element {
        match &el.kind {
            Kind::Ref { name, .. } => match builtin_token(name) {
                Some(tok) if !defined.contains(name) => Element::token(tok),
                _ => el.clone(),
            },
            Kind::Opt { inner } => Element {
                kind: Kind::Opt {
                    inner: Box::new(walk(inner, defined)),
                },
                sp: el.sp,
            },
            Kind::Star { inner, debt_guard } => Element {
                kind: Kind::Star {
                    inner: Box::new(walk(inner, defined)),
                    debt_guard: debt_guard.clone(),
                },
                sp: el.sp,
            },
            Kind::Plus { inner } => Element {
                kind: Kind::Plus {
                    inner: Box::new(walk(inner, defined)),
                },
                sp: el.sp,
            },
            Kind::Rep { min, max, inner } => Element {
                kind: Kind::Rep {
                    min: *min,
                    max: *max,
                    inner: Box::new(walk(inner, defined)),
                },
                sp: el.sp,
            },
            Kind::Group { alts } => Element::group(
                alts.iter()
                    .map(|a| a.iter().map(|e| walk(e, defined)).collect())
                    .collect(),
            ),
            _ => el.clone(),
        }
    }

    for prod in &mut grammar.productions {
        prod.alts = prod
            .alts
            .iter()
            .map(|alt| alt.iter().map(|e| walk(e, &defined)).collect())
            .collect();
    }
}

/// Whether a regex terminal can match nothing, decided by asking the
/// regex. An invalid pattern answers true, the permissive direction.
fn regex_derives_empty(pattern: &str, flags: &str) -> bool {
    let mut builder = regex::RegexBuilder::new(&format!("^(?:{pattern})$"));
    builder.case_insensitive(flags.contains('i'));
    match builder.build() {
        Ok(re) => re.is_match(""),
        Err(_) => true,
    }
}

pub(crate) fn element_derives_empty(el: &Element, nullable: &IndexSet<String>) -> bool {
    match &el.kind {
        Kind::Opt { .. } | Kind::Star { .. } => true,
        Kind::Plus { inner } => element_derives_empty(inner, nullable),
        Kind::Rep { min, inner, .. } => *min == 0 || element_derives_empty(inner, nullable),
        Kind::Group { alts } => alts.iter().any(|alt| sequence_derives_empty(alt, nullable)),
        Kind::Ref { name, .. } => nullable.contains(name),
        // An empty literal is kept rather than refused, and a terminal
        // matching nothing matches nothing.
        Kind::Term { literal, .. } => literal.is_empty(),
        Kind::Regex { pattern, flags } => regex_derives_empty(pattern, flags),
        // Two of the engine's own tokens are satisfied without consuming
        // anything: `#ZZ`, end of source, and `#AA`, the ANY wildcard.
        Kind::Token { name } => name == "#ZZ" || name == "#AA",
        Kind::Prose { .. } => false,
    }
}

fn sequence_derives_empty(alt: &[Element], nullable: &IndexSet<String>) -> bool {
    alt.iter().all(|el| element_derives_empty(el, nullable))
}

/// The rules that derive the empty string: a least fixed point.
pub(crate) fn nullable_rules(prods: &[Production]) -> IndexSet<String> {
    let mut nullable: IndexSet<String> = IndexSet::new();
    let mut changed = true;
    while changed {
        changed = false;
        for p in prods {
            if nullable.contains(&p.name) {
                continue;
            }
            if p.alts
                .iter()
                .any(|alt| sequence_derives_empty(alt, &nullable))
            {
                nullable.insert(p.name.clone());
                changed = true;
            }
        }
    }
    nullable
}
