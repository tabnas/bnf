// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

//! The emitter: `emit_grammar_spec` runs the rewrite passes and turns the
//! desugared grammar into a [`GrammarSpec`]. Mirrors the emit half of
//! `ts/src/compiler.ts`: token allocation, the contest context, the ref
//! registry, and the production, chain, tail-repeat and probe emitters.
//!
//! The emitter turns each alternative into one or more tabnas rule alts.
//! A "single-segment" alternative (at most one rule reference, trailing)
//! collapses to a single tabnas alt; any alternative with two or more ref
//! boundaries is chained through synthetic continuation rules named
//! `<prodname>$stepN`.
//!
//! Every alternate is written field by field in the order the canonical
//! compiler's object literal writes it, because the serialised text is
//! compared byte for byte against TypeScript's.

use std::cell::RefCell;

use indexmap::{IndexMap, IndexSet};
use serde_json::{json, Map, Value};

use crate::analysis::{
    alt_prefixes, alt_prefixes_raw, compute_first_sets, compute_follow_pairs, compute_follow_sets,
    dispatch_prefixes, first_of_alt, is_single_segment, resolve_suffix_debts, token_class_names,
    FirstSets, FollowPairs, FollowSets, Nullable, Tokens,
};
use crate::annotate::{plan_array_helpers, plan_value_annotations};
use crate::desugar::desugar;
use crate::factor::{left_factor, seq_token_span, LOOKAHEAD_K};
use crate::ir::{
    builtin_token, check_element_depth, diag_name, escape_regexp, is_effectively_case_sensitive,
    origin_of, regex_key, set_diag_name, term_key_of, ConvertOptions, Element, EmitError, Grammar,
    Kind, NodeKind, Production, Sequence, SrcSpan, ValueAnnotation,
};
use crate::leftrec::eliminate_left_recursion_keeping;
use crate::leftrec::rewrite_tail_repeats;
use crate::probe::rewrite_probe_dispatches;
use crate::prose::{
    lift_literal_tokens, normalize_builtin_tokens, nullable_rules, resolve_prose_terminals,
};
use crate::ranges::{
    char_ranges_overlap, class_analysis, class_pattern, fold_case_ranges, normalize_ranges,
    pattern_char_ranges, CharRange,
};
use crate::spec::{AltSpec, GrammarSpec, RefAction, RuleSpec};

/// A match-token matcher as the engine's wire format carries it:
/// `@~/source/flags` when eager, `@/source/flags` otherwise.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct MatchToken {
    pub source: String,
    pub flags: String,
    pub eager: bool,
}

impl MatchToken {
    pub fn serialized(&self) -> String {
        let sentinel = if self.eager { "@~/" } else { "@/" };
        format!("{}{}/{}", sentinel, self.source, self.flags)
    }
}

/// The recovery sync group appended to a tag: `<tag>,<group>`. Only a
/// CLOSE alternate that names a token can be a sync point, and this
/// emitter produces exactly two: the start wrapper's `#ZZ` (`end`) and a
/// tail repeat's separator continuation (`comma`). Tag all of them or
/// none: one tagged alternate anywhere switches the engine's fallback
/// off for every rule below it.
fn sync_g(tag: &str, group: &str) -> String {
    format!("{tag},{group}")
}

/// Whether a pattern has top-level alternation, in which case anchoring
/// it needs a non-capturing group: `^a|bc` anchors only the first branch.
fn has_top_level_alternation(pattern: &str) -> bool {
    let chars: Vec<char> = pattern.chars().collect();
    let mut in_class = false;
    let mut depth = 0usize;
    let mut i = 0;
    while i < chars.len() {
        let c = chars[i];
        if c == '\\' {
            i += 2;
            continue;
        }
        if in_class {
            if c == ']' {
                in_class = false;
            }
        } else if c == '[' {
            in_class = true;
        } else if c == '(' {
            depth += 1;
        } else if c == ')' {
            depth = depth.saturating_sub(1);
        } else if c == '|' && depth == 0 {
            return true;
        }
        i += 1;
    }
    false
}

fn anchorable(pattern: &str) -> String {
    if has_top_level_alternation(pattern) {
        format!("(?:{pattern})")
    } else {
        pattern.to_string()
    }
}

/// The pattern as a JavaScript `RegExp.source` reads it: an unescaped `/`
/// outside a character class becomes `\/`, and every line terminator is
/// written as its escape. The serialized token strings match the
/// canonical compiler's byte for byte, and the engine's `@/…/flags`
/// reader splits on the last slash, which this keeps unambiguous.
fn js_regex_source(pattern: &str) -> String {
    let mut out = String::with_capacity(pattern.len());
    let mut in_class = false;
    let mut chars = pattern.chars().peekable();
    while let Some(c) = chars.next() {
        match c {
            '\\' => {
                out.push('\\');
                if let Some(next) = chars.next() {
                    match next {
                        '\n' => out.push_str("\\n"),
                        '\r' => out.push_str("\\r"),
                        '\u{2028}' => out.push_str("\\u2028"),
                        '\u{2029}' => out.push_str("\\u2029"),
                        other => out.push(other),
                    }
                }
            }
            '[' => {
                in_class = true;
                out.push(c);
            }
            ']' => {
                in_class = false;
                out.push(c);
            }
            '/' if !in_class => out.push_str("\\/"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\u{2028}' => out.push_str("\\u2028"),
            '\u{2029}' => out.push_str("\\u2029"),
            other => out.push(other),
        }
    }
    out
}

/// Token names the engine's own matchers own. A lifted literal that would
/// land on one must be renamed instead: the engine refuses a fixed-token
/// entry under a matcher-owned name.
pub(crate) fn is_engine_owned_token(name: &str) -> bool {
    builtin_token(name.trim_start_matches('#')).is_some()
        || matches!(name, "#BD" | "#ZZ" | "#UK" | "#AA" | "#SP" | "#LN" | "#CM")
}

/// Allocate a token name for a literal: the preferred (lifted) name when
/// free, else the literal reduced to `[A-Za-z0-9]` with underscores and
/// upper-cased (`#T` when nothing is left), numbered on collision. An
/// astral character reduces to two underscores, as it does in the
/// canonical compiler, which counts UTF-16 units.
fn alloc_token_name(literal: &str, used: &mut IndexSet<String>, preferred: Option<&str>) -> String {
    if let Some(preferred) = preferred {
        let want = format!("#{preferred}");
        if !used.contains(&want) && !is_engine_owned_token(&want) {
            used.insert(want.clone());
            return want;
        }
    }
    let mut base = String::new();
    for c in literal.chars() {
        if c.is_ascii_alphanumeric() {
            base.push(c.to_ascii_uppercase());
        } else if (c as u32) > 0xFFFF {
            base.push_str("__");
        } else {
            base.push('_');
        }
    }
    let base = base.trim_matches('_');
    let candidate = if base.is_empty() {
        "#T".to_string()
    } else {
        format!("#{base}")
    };
    if !used.contains(&candidate) && !is_engine_owned_token(&candidate) {
        used.insert(candidate.clone());
        return candidate;
    }
    let mut i = 1;
    while used.contains(&format!("{candidate}{i}")) {
        i += 1;
    }
    let chosen = format!("{candidate}{i}");
    used.insert(chosen.clone());
    chosen
}

fn ends_with_word_char(s: &str) -> bool {
    s.chars()
        .last()
        .is_some_and(|c| c.is_ascii_alphanumeric() || c == '_')
}

/// Answers "can these two tokens claim the same input character?", with
/// memoisation on the token and on the PAIR. The contest checks are
/// quadratic in dispatch entries while the distinct token pairs behind
/// them are few.
struct ContestCtx {
    fixed_tokens: IndexMap<String, Option<String>>,
    match_tokens: IndexMap<String, MatchToken>,
    /// The coverage of a class that became a token SET over atoms, so it
    /// appears in neither token table.
    set_ranges: IndexMap<String, Vec<CharRange>>,
    /// The token classes of `ConvertOptions::token_classes`: production
    /// to its set, and set to its member tokens.
    class_sets: IndexMap<String, String>,
    class_members: IndexMap<String, Vec<String>>,
    /// Every token set the emitted spec declares (class-partition sets and
    /// token classes alike), for the heads predicate.
    token_sets: IndexMap<String, Vec<String>>,
    /// Token name to the literal it was allocated for and whether it is
    /// case-sensitive, for the heads predicate's prefix test.
    literal_by_token: IndexMap<String, (String, bool)>,
    word_keywords: bool,
    range_cache: RefCell<IndexMap<String, Option<Vec<CharRange>>>>,
    overlap_cache: RefCell<IndexMap<String, bool>>,
    contest_cache: RefCell<IndexMap<String, bool>>,
}

impl ContestCtx {
    /// Character coverage per token, or `None` when unknown (and the
    /// guard emission stays conservative). A fixed token covers its
    /// literal's first code point; a match token covers whatever its
    /// leading character class covers, both cases of every ASCII letter
    /// when it is case-insensitive.
    fn token_ranges_of(&self, tok: &str) -> Option<Vec<CharRange>> {
        if let Some(hit) = self.range_cache.borrow().get(tok) {
            return hit.clone();
        }
        let ranges: Option<Vec<CharRange>> = if let Some(spanned) = self.set_ranges.get(tok) {
            Some(normalize_ranges(spanned))
        } else {
            let mut r: Option<Vec<CharRange>> = None;
            if let Some(Some(lit)) = self.fixed_tokens.get(tok) {
                if let Some(c) = lit.chars().next() {
                    r = Some(vec![(c as u32, c as u32)]);
                }
            } else if let Some(re) = self.match_tokens.get(tok) {
                // Strip the emitter's own `^` anchor (and grouping) so the
                // parser sees the pattern as written.
                let mut src = re
                    .source
                    .strip_prefix('^')
                    .unwrap_or(&re.source)
                    .to_string();
                if let Some(inner) = src.strip_prefix("(?:").and_then(|s| s.strip_suffix(')')) {
                    src = inner.to_string();
                }
                r = pattern_char_ranges(&src);
                if let Some(rr) = &r {
                    if re.flags.contains('i') {
                        r = Some(fold_case_ranges(rr));
                    }
                }
            }
            r.map(|r| normalize_ranges(&r))
        };
        self.range_cache
            .borrow_mut()
            .insert(tok.to_string(), ranges.clone());
        ranges
    }

    fn tokens_overlap(&self, a: &str, b: &str) -> bool {
        let key = if a < b {
            format!("{a}\0{b}")
        } else {
            format!("{b}\0{a}")
        };
        if let Some(hit) = self.overlap_cache.borrow().get(&key) {
            return *hit;
        }
        let ma = self.class_members.get(a);
        let mb = self.class_members.get(b);
        let out = if ma.is_some() || mb.is_some() {
            // A token class meets what any member meets, the same token
            // included; coverage alone would miss an engine token in it.
            // `token_class_names` keeps a set out of every set's members;
            // were one to get in, this provisional answer, the
            // conservative one, is what ends the expansion when it comes
            // back to the same pair.
            self.overlap_cache.borrow_mut().insert(key.clone(), true);
            let xs: Vec<String> = ma.cloned().unwrap_or_else(|| vec![a.to_string()]);
            let ys: Vec<String> = mb.cloned().unwrap_or_else(|| vec![b.to_string()]);
            xs.iter()
                .any(|x| ys.iter().any(|y| x == y || self.tokens_overlap(x, y)))
        } else {
            let ra = self.token_ranges_of(a);
            let rb = self.token_ranges_of(b);
            match (ra, rb) {
                (Some(ra), Some(rb)) => char_ranges_overlap(&ra, &rb),
                _ => false,
            }
        };
        self.overlap_cache.borrow_mut().insert(key, out);
        out
    }

    /// Can the lexer hand the same input to two dispatch heads, so that a
    /// choice between them on one token is no choice? The same token can;
    /// a literal can meet a literal it is a prefix of, or that is a prefix
    /// of it, case-folded when either is insensitive, except two
    /// whole-word keywords under `word_keywords`, whose boundary guard
    /// keeps `option` off `optional`; a token set meets whatever one of
    /// its members meets; a character class, or an atom of one, meets what
    /// its coverage overlaps; an engine token meets itself and what its
    /// matcher can take when the parser asks (`engine_token_meets`). Narrower than `tokens_overlap` on purpose: that is the
    /// lexer's question, whether two heads can claim one CHARACTER.
    /// Mirrors the TypeScript `headsContest`.
    fn heads_contest(&self, a: &str, b: &str) -> bool {
        if a == b {
            return true;
        }
        // The engine's ANY token takes every token, so it meets every head.
        if a == "#AA" || b == "#AA" {
            return true;
        }
        let key = if a < b {
            format!("{a}\0{b}")
        } else {
            format!("{b}\0{a}")
        };
        if let Some(hit) = self.contest_cache.borrow().get(&key) {
            return *hit;
        }
        let ma = self.token_sets.get(a.trim_start_matches('#'));
        let mb = self.token_sets.get(b.trim_start_matches('#'));
        let la = self.literal_by_token.get(a);
        let lb = self.literal_by_token.get(b);
        let out = if ma.is_some() || mb.is_some() {
            // A set meets what any member meets. The members are never
            // sets (`token_class_names`), and the provisional answer, the
            // conservative one, would end the expansion if one were.
            self.contest_cache.borrow_mut().insert(key.clone(), true);
            let xs: Vec<String> = ma.cloned().unwrap_or_else(|| vec![a.to_string()]);
            let ys: Vec<String> = mb.cloned().unwrap_or_else(|| vec![b.to_string()]);
            xs.iter()
                .any(|x| ys.iter().any(|y| self.heads_contest(x, y)))
        } else if let (Some((la, sa)), Some((lb, sb))) = (la, lb) {
            let (short, long) = if la.chars().count() <= lb.chars().count() {
                (la, lb)
            } else {
                (lb, la)
            };
            let is_prefix = if *sa && *sb {
                long.starts_with(short.as_str())
            } else {
                folded_prefix(short, long)
            };
            // Two whole-word keywords under `word_keywords`: the shorter's
            // boundary guard refuses the longer wherever the longer goes
            // on with a word character (`option` off `optional`), and
            // admits it wherever it goes on with anything else (`a` on
            // `a-b`).
            is_prefix
                && !(self.is_word_literal(la) && self.is_word_literal(lb) && {
                    let lr: Vec<char> = long.chars().collect();
                    let n = short.chars().count();
                    n < lr.len() && is_word_char(lr[n])
                })
        } else {
            // A character class, or an atom of one, against anything: by
            // coverage, when the coverage is exact. An engine token has no
            // coverage of its own, and meets what its matcher can take
            // (`engine_token_meets`).
            !self.regex_head_is_exact(a)
                || !self.regex_head_is_exact(b)
                || self.tokens_overlap(a, b)
                || self.engine_token_meets(a, b)
                || self.engine_token_meets(b, a)
        };
        self.contest_cache.borrow_mut().insert(key, out);
        out
    }

    /// Three of the engine's own matchers can take text another head
    /// claims, and under negotiated lexing the parser asks them to:
    /// `relex` runs only the matchers that can produce the token an
    /// alternative wants. The number matcher takes a leading digit, sign
    /// or point, the string matcher a leading quote, and the text matcher
    /// any text that no fixed token claims, a number's or a string's
    /// included; a fixed literal it defers to. Measured against the
    /// four-token dispatch this replaced, these are exactly the pairs it
    /// kept apart that a one-token dispatch would not. `#VL` is not among
    /// them: an emitted grammar lexes no values, so nothing is ever cut as
    /// one. Mirrors the TypeScript `engineTokenMeets`.
    fn engine_token_meets(&self, eng: &str, other: &str) -> bool {
        const NUMBER_START: [CharRange; 3] = [(0x2B, 0x2B), (0x2D, 0x2E), (0x30, 0x39)];
        const QUOTE_START: [CharRange; 3] = [(0x22, 0x22), (0x27, 0x27), (0x60, 0x60)];
        let starts: &[CharRange] = match eng {
            "#TX" => {
                return other == "#NR" || other == "#ST" || self.match_tokens.contains_key(other);
            }
            "#NR" => &NUMBER_START,
            "#ST" => &QUOTE_START,
            _ => return false,
        };
        self.token_ranges_of(other)
            .is_some_and(|r| char_ranges_overlap(starts, &r))
    }

    fn is_word_literal(&self, lit: &str) -> bool {
        self.word_keywords && ends_with_word_char(lit)
    }

    /// Whether a regex-backed head's first character can be read off its
    /// pattern: one atom or one class, alone or repeated with `+`, whose
    /// coverage `pattern_char_ranges` can name. Anything else (`a|b`
    /// begins with b too, `a?b` with b, `.` with anything, `\d` with a
    /// digit and `\n` with a character `pattern_char_ranges` declines to
    /// name) may meet any head, and the dispatcher must treat it so.
    /// Mirrors the TypeScript `regexHeadIsExact`.
    fn regex_head_is_exact(&self, tok: &str) -> bool {
        let Some(re) = self.match_tokens.get(tok) else {
            return true;
        };
        // A case-insensitive matcher folds case, and the coverage is
        // folded for ASCII only (`fold_case_ranges`): a head that reaches
        // beyond ASCII meets whatever Unicode folding lets it (`(?i)[Σ]`
        // takes `ς`), which the coverage does not say. That holds for a
        // case-insensitive literal as much as for a pattern.
        if re.flags.contains('i') {
            match self.token_ranges_of(tok) {
                Some(r) if r.iter().all(|(_, hi)| *hi <= 0x7F) => {}
                _ => return false,
            }
        }
        // A literal, fixed or guarded (`^option\b`), covers its first
        // character exactly whatever follows it in the matcher.
        if self.literal_by_token.contains_key(tok) {
            return true;
        }
        let mut src = re
            .source
            .strip_prefix('^')
            .unwrap_or(&re.source)
            .to_string();
        if let Some(inner) = src.strip_prefix("(?:").and_then(|s| s.strip_suffix(')')) {
            src = inner.to_string();
        }
        let Some(end) = crate::ranges::regex_head_atom_end(&src) else {
            return false;
        };
        // `\u{61}` is the code point `a` only under the `u` or `v` flag;
        // without either it is `u` repeated 61 times, which is not what
        // `pattern_char_ranges` reads it as.
        let head: String = src.chars().take(end).collect();
        if !re.flags.contains('u') && !re.flags.contains('v') && head.contains("\\u{") {
            return false;
        }
        let rest: String = src.chars().skip(end).collect();
        (rest.is_empty() || rest == "+") && self.token_ranges_of(tok).is_some()
    }

    /// What a literal head covers: a literal its first character; a token
    /// class's set what its literal members cover, since an engine token
    /// among them (`#TX`) meets no character class here, as it would not
    /// as a head of its own. Mirrors the TypeScript `literalHeadRangesOf`.
    fn literal_head_ranges_of(&self, tok: &str) -> Option<Vec<CharRange>> {
        let Some(members) = self.class_members.get(tok) else {
            return self.token_ranges_of(tok);
        };
        let mut out: Option<Vec<CharRange>> = None;
        for m in members {
            if let Some(r) = self.token_ranges_of(m) {
                out.get_or_insert_with(Vec::new).extend(r);
            }
        }
        out
    }
}

/// Whether one case-insensitive literal is a prefix of the other under
/// the matcher's own folding. Lowercase alone is not that relation:
/// `(?i)Σ` takes `ς` and `(?i)ς` takes `Σ`, while lowercase maps `Σ` to
/// `σ` and leaves `ς` alone. Folding both ways over-approximates the
/// matcher (a contest declared where the matcher would not meet) and
/// never under-approximates it, which is the safe direction here.
fn folded_prefix(short: &str, long: &str) -> bool {
    long.to_lowercase().starts_with(&short.to_lowercase())
        || long.to_uppercase().starts_with(&short.to_uppercase())
}

fn is_word_char(c: char) -> bool {
    c == '_' || c.is_ascii_alphanumeric()
}

/// Allocates unique `@`-prefixed ref names for the tree-building and
/// probe-control actions in closure mode, or emits engine `$`-builtin
/// refs plus `k` config in builtins mode. Each emitter writes `a` (and
/// `k`) onto the alternate exactly where the canonical compiler's
/// `Object.assign` or spread would.
struct RefRegistry {
    refs: IndexMap<String, RefAction>,
    counter: usize,
    use_builtins: bool,
    emit_marks: bool,
}

impl RefRegistry {
    fn register(&mut self, action: RefAction) -> String {
        let name = format!("@bnf_a{}", self.counter);
        self.counter += 1;
        self.refs.insert(name.clone(), action);
        name
    }

    fn node(&mut self, alt: &mut AltSpec, init: bool, rule: &str, kind: NodeKind, nterms: usize) {
        if self.use_builtins {
            alt.set("a", "@node$");
            alt.set(
                "k",
                json!({"node$": {"init": init, "rule": rule, "kind": kind.as_str(), "nterms": nterms}}),
            );
        } else {
            let name = self.register(RefAction::Node {
                init,
                rule: rule.to_string(),
                kind,
                nterms,
            });
            alt.set("a", name);
        }
    }

    fn capture(&mut self, alt: &mut AltSpec, rule: &str, kind: NodeKind) {
        if self.use_builtins {
            alt.set("a", "@capture$");
            alt.set(
                "k",
                json!({"capture$": {"rule": rule, "kind": kind.as_str()}}),
            );
        } else {
            let name = self.register(RefAction::Capture {
                rule: rule.to_string(),
                kind,
            });
            alt.set("a", name);
        }
    }

    fn bubble(&mut self, alt: &mut AltSpec) {
        if self.use_builtins {
            alt.set("a", "@bubble$");
        } else {
            let name = self.register(RefAction::Bubble);
            alt.set("a", name);
        }
    }

    fn fold(&mut self, alt: &mut AltSpec, c_n: Option<usize>) {
        if self.use_builtins {
            alt.set("a", "@fold$");
            let cfg = match c_n {
                Some(n) => json!({"cN": n}),
                None => json!({}),
            };
            alt.set("k", json!({"fold$": cfg}));
        } else {
            let name = self.register(RefAction::Fold {
                c_n: c_n.unwrap_or(0),
            });
            alt.set("a", name);
        }
    }
}

/// A run of terminal tokens followed by at most one rule reference.
#[derive(Debug, Clone, Default)]
struct Segment {
    terms: Vec<String>,
    reference: Option<String>,
    /// Counter mutations the pushing alt carries, from the reference's
    /// suffix-debt annotation.
    debt: Option<IndexMap<String, i64>>,
}

/// One emitted open alternate together with the IR alternative it came
/// from (`None` for a synthesised guard or a FOLLOW re-issue).
#[derive(Debug, Clone)]
struct Entry {
    o: AltSpec,
    alt: Option<Sequence>,
}

/// An alternate placed by the keyword-shadow reordering, with the index
/// of the entry it IS (as opposed to a guard synthesised from one).
struct Placed {
    o: AltSpec,
    origin: Option<usize>,
    rank: f64,
    seq: usize,
}

struct Emitter<'a> {
    grammar: &'a Grammar,
    tag: String,
    tokens: Tokens,
    known_rules: IndexSet<String>,
    first_sets: FirstSets,
    nullable: Nullable,
    follow_sets: FollowSets,
    follow_pairs: FollowPairs,
    contest: ContestCtx,
    refs: RefRegistry,
    value_plan: IndexMap<String, Vec<bool>>,
    array_helpers: IndexSet<String>,
    value_rules: IndexSet<String>,
    prov: Option<IndexMap<String, String>>,
    rule_spec: IndexMap<String, Option<RuleSpec>>,
}

/// Compile a grammar IR into a tabnas `GrammarSpec`.
///
/// The grammar is not modified: every pass works on a copy. Diagnostics
/// are prefixed with `opts.tag` (default `bnf`), the notation the grammar
/// was written in.
pub fn emit_grammar_spec(
    grammar: &Grammar,
    opts: &ConvertOptions,
) -> Result<GrammarSpec, EmitError> {
    // Diagnostics name the notation the grammar was written in, not this
    // package. Set BEFORE any pass that can raise one.
    let tag = opts.tag.clone().unwrap_or_else(|| "bnf".to_string());
    set_diag_name(&tag);

    // Before the grammar is even copied: cloning it recurses through the
    // element tree too, so the shape has to be refused first.
    check_element_depth(grammar)?;

    let mut grammar = grammar.clone();

    // Before ANY rewrite: annotations describe the grammar the AUTHOR
    // wrote, and the passes below are what make that shape unrecoverable.
    let plan = plan_value_annotations(&grammar)?;

    // Drop informational prose definitions first, so the names they
    // document fall through to the built-in tokens, and so a leading
    // prose line is never mistaken for the start rule.
    resolve_prose_terminals(&mut grammar)?;

    // Capture the `<remove>` directives before the rewrite passes: each
    // returns a fresh grammar carrying only productions.
    let remove_names = grammar.remove.clone();
    let clear_all = grammar.clear_all;

    let start = match &opts.start {
        Some(s) => s.clone(),
        None => match grammar.productions.first() {
            Some(p) => p.name.clone(),
            None => {
                return Err(EmitError::new(format!(
                    "{}: grammar has no productions to start from (a removal-only grammar \
                     needs an explicit start rule)",
                    diag_name()
                )))
            }
        },
    };
    let word_keywords = opts.word_keywords;

    // Whether the empty input is in the language, decided HERE because
    // the engine short-circuits `''` before the parse loop starts.
    // Computed on the grammar as written.
    let accepts_empty = nullable_rules(&grammar.productions).contains(&start);

    // Turn single-literal productions into named lexer tokens, then
    // resolve bare built-in token names to token terminals, both before
    // any structural pass sees them as rule references.
    let lifted_literals = lift_literal_tokens(&mut grammar, &start);
    normalize_builtin_tokens(&mut grammar);

    // The token classes, read before any rewrite: elimination would
    // otherwise inline each one into every rule it leads, which is the
    // multiplier the option exists to remove.
    let class_names: IndexSet<String> = if opts.token_classes {
        token_class_names(&grammar)
    } else {
        IndexSet::new()
    };

    let grammar = eliminate_left_recursion_keeping(&grammar, &class_names)?;
    let grammar = rewrite_probe_dispatches(&grammar)?;
    // Left factoring runs after the probe rewriter and before
    // tail-repeat detection and desugaring.
    let grammar = left_factor(&grammar)?;
    let grammar = rewrite_tail_repeats(grammar, &start);
    let mut grammar = desugar(&grammar);

    // Both are named AFTER desugar, because both are keyed by the rule
    // names the emitter will actually see.
    let array_helpers = plan_array_helpers(&grammar, &plan.collect);
    let value_rules: IndexSet<String> = grammar
        .productions
        .iter()
        .filter(|p| p.value.is_some())
        .map(|p| p.name.clone())
        .collect();

    // Allocate a fixed token for each unique literal, and a match token
    // for each unique regex terminal.
    let mut tokens = Tokens::default();
    let mut used_names: IndexSet<String> = IndexSet::new();
    // The token classes take their names first (`#ident` for the class
    // `ident`): the substitution pass has already written token elements
    // under those names, so nothing allocated below may take one. The
    // members are filled in once the tokens they are exist.
    let mut class_set_names: IndexMap<String, String> = IndexMap::new();
    for prod in &grammar.productions {
        if class_names.contains(&prod.name) {
            let name = alloc_token_name(&prod.name, &mut used_names, Some(&prod.name));
            class_set_names.insert(prod.name.clone(), name);
        }
    }
    let mut fixed_tokens: IndexMap<String, Option<String>> = IndexMap::new();
    let mut match_tokens: IndexMap<String, MatchToken> = IndexMap::new();
    let mut token_sets: IndexMap<String, Vec<String>> = IndexMap::new();
    let mut set_ranges: IndexMap<String, Vec<CharRange>> = IndexMap::new();

    // Gather every terminal first. Probe helpers store their vocab as
    // elements rather than in `alts`, and a tail repeat's separator is
    // stashed off `alts` too, so walk those as well. The lifted literals
    // are seeded up front: their productions no longer exist.
    let mut terminals: Vec<Element> = lifted_literals;
    for prod in &grammar.productions {
        for alt in &prod.alts {
            terminals.extend(alt.iter().cloned());
        }
        if let Some(ph) = &prod.probe_helper {
            terminals.extend(ph.vocab_elements.iter().cloned());
        }
        if let Some(tr) = &prod.tail_repeat {
            terminals.extend(tr.sep.iter().cloned());
        }
    }

    // Which character classes contest a position with another, and the
    // shared partition they are laid over: computed before anything is
    // allocated.
    let mut classes = class_analysis(&terminals);

    // Terminals carrying a lifted production name are allocated first,
    // so the name wins even when the same literal also appears inline in
    // an earlier rule.
    let named: Vec<Element> = terminals
        .iter()
        .filter(|el| {
            matches!(
                &el.kind,
                Kind::Term {
                    token_name: Some(_),
                    ..
                }
            )
        })
        .cloned()
        .collect();
    for el in named.iter().chain(terminals.iter()) {
        match &el.kind {
            Kind::Term {
                literal,
                case_sensitive,
                token_name,
            } => {
                let key = term_key_of(el);
                if !tokens.literals.contains_key(&key) {
                    let name = alloc_token_name(literal, &mut used_names, token_name.as_deref());
                    tokens.literals.insert(key, name.clone());
                    emit_literal_token(
                        literal,
                        *case_sensitive,
                        &name,
                        &mut fixed_tokens,
                        &mut match_tokens,
                        word_keywords,
                    )?;
                }
            }
            Kind::Regex { pattern, flags } => {
                let key = regex_key(pattern, flags);
                if !tokens.regex_tokens.contains_key(&key) {
                    // Allocated HERE, in terminal order, rather than in a
                    // pass of their own: allocation order is tin order,
                    // and tin order is what the lexer walks when it picks
                    // between the matchers a slot expects.
                    emit_class_token(
                        pattern,
                        flags,
                        &key,
                        &mut classes,
                        &mut tokens,
                        &mut token_sets,
                        &mut set_ranges,
                        &mut match_tokens,
                        &mut used_names,
                    )?;
                }
            }
            _ => {}
        }
    }

    let known_rules: IndexSet<String> =
        grammar.productions.iter().map(|p| p.name.clone()).collect();

    let mut contest = ContestCtx {
        fixed_tokens: fixed_tokens.clone(),
        match_tokens: match_tokens.clone(),
        set_ranges,
        class_sets: IndexMap::new(),
        class_members: IndexMap::new(),
        token_sets: IndexMap::new(),
        literal_by_token: IndexMap::new(),
        word_keywords,
        range_cache: RefCell::new(IndexMap::new()),
        overlap_cache: RefCell::new(IndexMap::new()),
        contest_cache: RefCell::new(IndexMap::new()),
    };
    for (key, name) in &tokens.literals {
        contest
            .literal_by_token
            .insert(name.clone(), (key[3..].to_string(), key.starts_with("cs:")));
    }

    // The token classes as engine token sets (`ConvertOptions::token_classes`):
    // one set per class, under the name allocated above, holding the
    // tokens its alternatives are. Filled after the tokens, since the
    // members must exist, and before FIRST, whose sets name them.
    for prod in &grammar.productions {
        let Some(name) = class_set_names.get(&prod.name).cloned() else {
            continue;
        };
        let mut members: Vec<String> = Vec::new();
        for alt in &prod.alts {
            let tok = tokens.name(&alt[0]);
            if !members.contains(&tok) {
                members.push(tok);
            }
        }
        token_sets.insert(name.trim_start_matches('#').to_string(), members.clone());
        contest.class_sets.insert(prod.name.clone(), name.clone());
        // The class covers what its members cover, when that is known for
        // every member; an engine token among them (`#TX`) leaves it
        // unknown, as it is for that token alone.
        let mut covered: Vec<CharRange> = Vec::new();
        let mut known = true;
        for m in &members {
            match contest.token_ranges_of(m) {
                Some(r) => covered.extend(r),
                None => {
                    known = false;
                    break;
                }
            }
        }
        if known {
            contest.set_ranges.insert(name.clone(), covered);
        }
        contest.class_members.insert(name, members);
    }
    contest.token_sets = token_sets.clone();

    let (first_sets, nullable) = compute_first_sets(&grammar, &tokens, &contest.class_sets);
    let follow_sets = compute_follow_sets(&grammar, &tokens, &first_sets, &nullable, &start);
    let follow_pairs =
        compute_follow_pairs(&grammar, &tokens, &first_sets, &nullable, &follow_sets);

    // Settle the contested left-recursion tail loops flagged during
    // elimination, now that FIRST sets can say whether the competition
    // is real. Only annotates; nothing computed above depends on it.
    resolve_suffix_debts(&mut grammar, &tokens, &first_sets, &nullable, &|a, b| {
        contest.tokens_overlap(a, b)
    });

    let refs = RefRegistry {
        refs: IndexMap::new(),
        counter: 0,
        use_builtins: opts.builtins,
        emit_marks: opts.marks,
    };

    let mut emitter = Emitter {
        grammar: &grammar,
        tag: tag.clone(),
        tokens,
        known_rules,
        first_sets,
        nullable,
        follow_sets,
        follow_pairs,
        contest,
        refs,
        value_plan: plan.plan,
        array_helpers,
        value_rules,
        prov: if opts.provenance {
            Some(IndexMap::new())
        } else {
            None
        },
        rule_spec: IndexMap::new(),
    };

    let grammar_ref: &Grammar = emitter.grammar;
    for prod in &grammar_ref.productions {
        // Productions synthesised by the rewrite passes are emitted under
        // their own names; record where each came from.
        if origin_of(prod) != prod.name {
            emitter.record_prov(&prod.name, origin_of(prod));
        }
        if prod.probe_helper.is_some() {
            emitter.emit_probe_helper(prod);
            continue;
        }
        if prod.probe_dispatch.is_some() {
            emitter.emit_probe_dispatch(prod)?;
            continue;
        }
        emitter.emit_production(prod)?;
    }

    // Wrap the user-visible start rule in a synthetic rule that
    // explicitly consumes #ZZ, so trailing content cannot slip past. The
    // IR reserves no names, so fall back to a numbered variant when the
    // grammar has a production of its own called `__start__`.
    let mut start_wrapper = "__start__".to_string();
    if emitter.known_rules.contains(&start_wrapper) {
        let mut n = 2;
        while emitter.known_rules.contains(&format!("__start{n}__")) {
            n += 1;
        }
        start_wrapper = format!("__start{n}__");
    }
    // The wrapper stands in for the start rule, so that is what it is
    // named after.
    emitter.record_prov(&start_wrapper, &start);

    let mut open = AltSpec::new();
    open.set("p", start.as_str());
    open.set("g", tag.as_str());
    // Return the start rule's AST node directly: the wrapper exists only
    // to ensure end-of-source gets consumed. End of source is the one
    // anchor every grammar has.
    let mut close = AltSpec::new();
    close.set("s", "#ZZ");
    emitter.refs.bubble(&mut close);
    close.set("g", sync_g(&tag, "end"));
    emitter.rule_spec.insert(
        start_wrapper.clone(),
        Some(RuleSpec {
            open: vec![open],
            close: Some(vec![close]),
        }),
    );

    let mut options: Map<String, Value> = Map::new();
    let mut fixed_map = Map::new();
    for (name, src) in &fixed_tokens {
        fixed_map.insert(name.clone(), src.as_ref().map_or(Value::Null, |s| json!(s)));
    }
    options.insert("fixed".into(), json!({ "token": fixed_map }));
    options.insert("rule".into(), json!({ "start": start_wrapper }));
    options.insert("lex".into(), json!({ "empty": accepts_empty }));
    if !match_tokens.is_empty() {
        let mut match_map = Map::new();
        for (name, tok) in &match_tokens {
            match_map.insert(name.clone(), json!(tok.serialized()));
        }
        options.insert("match".into(), json!({ "token": match_map }));
    }
    // One group per character class that spans several atoms of the
    // partition.
    if !token_sets.is_empty() {
        let mut sets = Map::new();
        for (name, members) in &token_sets {
            sets.insert(name.clone(), json!(members));
        }
        options.insert("tokenSet".into(), Value::Object(sets));
    }

    let Emitter {
        refs,
        prov,
        mut rule_spec,
        ..
    } = emitter;

    let mut spec = GrammarSpec {
        refs: refs.refs,
        options,
        rule: IndexMap::new(),
        meta: None,
        clear: false,
    };

    // Engine-ignored tool metadata: the map from each generated rule
    // name to the author-written production it came from, sorted so a
    // serialised grammar is byte-stable.
    if let Some(prov) = prov {
        if !prov.is_empty() {
            let mut names: Vec<&String> = prov.keys().collect();
            names.sort_by_key(|k| k.encode_utf16().collect::<Vec<u16>>());
            let mut provenance = Map::new();
            for name in names {
                provenance.insert(name.clone(), json!(prov[name]));
            }
            spec.meta = Some(json!({ "provenance": provenance }));
        }
    }

    // `<remove>` directives: `<all> = <remove>` maps to the engine's
    // `clear`; a named removal drops both the rule and the fixed token of
    // that name.
    if clear_all {
        spec.clear = true;
    }
    for name in &remove_names {
        rule_spec.insert(name.clone(), None);
        if let Some(Value::Object(fixed)) = spec.options.get_mut("fixed") {
            if let Some(Value::Object(token)) = fixed.get_mut("token") {
                token.insert(format!("#{name}"), Value::Null);
            }
        }
    }
    spec.rule = rule_spec;

    Ok(spec)
}

/// Allocate the lexer token for a string-literal terminal. A
/// case-sensitive literal is normally a fixed token and a
/// case-insensitive one an anchored `i`-flagged regex. When
/// `word_keywords` is set and the literal ends in a word character, it is
/// emitted as a regex with a trailing `\b` guard so the keyword matches
/// only as a whole word.
fn emit_literal_token(
    literal: &str,
    case_sensitive: Option<bool>,
    name: &str,
    fixed_tokens: &mut IndexMap<String, Option<String>>,
    match_tokens: &mut IndexMap<String, MatchToken>,
    word_keywords: bool,
) -> Result<(), EmitError> {
    let boundary = if word_keywords && ends_with_word_char(literal) {
        "\\b"
    } else {
        ""
    };
    let sensitive = is_effectively_case_sensitive(literal, case_sensitive);
    if sensitive && boundary.is_empty() {
        fixed_tokens.insert(name.to_string(), Some(literal.to_string()));
        return Ok(());
    }
    // Insensitive literal, or a word-keyword needing a boundary guard:
    // emit as an anchored regex, eager so the lexer fires it even when
    // the current rule's token column does not list its tin.
    let flags = if sensitive { "" } else { "i" };
    let source = js_regex_source(&format!("^{}{}", escape_regexp(literal), boundary));
    let flags = check_regex(&source, flags, name)?;
    match_tokens.insert(
        name.to_string(),
        MatchToken {
            source,
            flags,
            eager: true,
        },
    );
    Ok(())
}

/// The flags `new RegExp` accepts, in the order `RegExp.prototype.flags`
/// reports them back.
const REGEX_FLAGS: &str = "dgimsuvy";

/// Check a flag string the way the canonical `RegExp` constructor does —
/// every character a flag it knows, none of them repeated, `u` and `v`
/// never together — and return it in the order that constructor reports,
/// which is the order the canonical compiler serialises rather than the
/// order the front-end wrote (`new RegExp('a', 'yu').flags` is `uy`).
///
/// Flags the engine's `regex` crate does not act on are still accepted:
/// `d`, `g` and `y` change nothing about what the emitted matcher
/// matches, and refusing them would reject IR TypeScript emits happily.
/// `v` is accepted for the same reason, though the Rust engine refuses
/// it when the spec is installed; that refusal is the engine's, and
/// `DIVERGENCE.md` records it.
fn canonical_regex_flags(flags: &str, name: &str) -> Result<String, EmitError> {
    let refuse = |detail: String| {
        EmitError::new(format!(
            "{}: invalid regular expression flags for token {}: {}",
            diag_name(),
            name,
            detail
        ))
    };
    let mut seen = String::new();
    for flag in flags.chars() {
        if !REGEX_FLAGS.contains(flag) {
            return Err(refuse(format!("unknown flag '{flag}' in \"{flags}\"")));
        }
        if seen.contains(flag) {
            return Err(refuse(format!("duplicate flag '{flag}' in \"{flags}\"")));
        }
        seen.push(flag);
    }
    if seen.contains('u') && seen.contains('v') {
        return Err(refuse(format!("flags \"{flags}\" set both u and v")));
    }
    Ok(REGEX_FLAGS.chars().filter(|f| seen.contains(*f)).collect())
}

/// The matcher must compile in the engine's regex dialect: refuse at
/// emit time with the token named, rather than at install time with a
/// grammar the author did not write. Returns the flags to emit, in the
/// canonical order. The flag string is checked FIRST, as `new RegExp`
/// checks it, so a grammar wrong in both ways is refused for the same
/// reason in either runtime.
fn check_regex(source: &str, flags: &str, name: &str) -> Result<String, EmitError> {
    let flags = canonical_regex_flags(flags, name)?;
    let mut builder = regex::RegexBuilder::new(source);
    builder.case_insensitive(flags.contains('i'));
    builder.build().map_err(|err| {
        EmitError::new(format!(
            "{}: invalid regular expression for token {}: {}",
            diag_name(),
            name,
            err
        ))
    })?;
    Ok(flags)
}

/// Allocate the lexer token(s) for one character class, at the point the
/// class is first encountered. Uncontested: one match token. Contested:
/// the class is laid over the shared partition; each atom it covers gets
/// its own match token (minted here if this is the first class to need
/// it), and the class becomes a token SET over them, under the name it
/// would have had anyway, so nothing that consumes `regex_tokens` has to
/// know the difference.
#[allow(clippy::too_many_arguments)]
fn emit_class_token(
    pattern: &str,
    flags: &str,
    key: &str,
    classes: &mut crate::ranges::ClassAnalysis,
    tokens: &mut Tokens,
    token_sets: &mut IndexMap<String, Vec<String>>,
    set_ranges: &mut IndexMap<String, Vec<CharRange>>,
    match_tokens: &mut IndexMap<String, MatchToken>,
    used_names: &mut IndexSet<String>,
) -> Result<(), EmitError> {
    // A class fires at ANY lookahead position, not only at the slots the
    // current rule's collated token column names, which is what makes
    // the runtimes accept the same strings.
    let eager = |pattern: &str, flags: &str, name: &str| -> Result<MatchToken, EmitError> {
        let source = js_regex_source(&format!("^{}", anchorable(pattern)));
        let flags = check_regex(&source, flags, name)?;
        Ok(MatchToken {
            source,
            flags,
            eager: true,
        })
    };

    let name = alloc_token_name(&format!("rx_{pattern}"), used_names, None);
    tokens.regex_tokens.insert(key.to_string(), name.clone());

    if !classes.contested.contains(key) {
        match_tokens.insert(name.clone(), eager(pattern, flags, &name)?);
        return Ok(());
    }

    let mine = classes.coverage[key].clone();
    let mut members: Vec<String> = Vec::new();
    let atoms = classes.atoms.clone();
    for span in atoms {
        if !mine.iter().any(|&(a, b)| a <= span.0 && span.1 <= b) {
            continue;
        }
        let span_key = format!("{}-{}", span.0, span.1);
        let atom = match classes.atom_tokens.get(&span_key) {
            Some(atom) => atom.clone(),
            None => {
                let (atom_pattern, astral) = class_pattern(span.0, span.1);
                // `rxa_`, not `rx_`: an atom is synthetic, and a name
                // minted from `rx_` collides with the natural name of any
                // class spelling the same span.
                let atom = alloc_token_name(&format!("rxa_{atom_pattern}"), used_names, None);
                match_tokens.insert(
                    atom.clone(),
                    eager(&atom_pattern, if astral { "u" } else { "" }, &atom)?,
                );
                classes.atom_tokens.insert(span_key, atom.clone());
                atom
            }
        };
        members.push(atom);
    }

    // A one-member set rather than pointing `regex_tokens` straight at
    // the atom: the class keeps its own name whatever the partition does
    // underneath it. Keyed WITHOUT the leading `#`, which is how the
    // engine looks a set up.
    token_sets.insert(name.trim_start_matches('#').to_string(), members);
    set_ranges.insert(name, mine);
    Ok(())
}

/// Break an alternative into segments. Each segment is a (possibly
/// empty) run of terminal tokens followed by at most one rule reference.
fn segmentize(alt: &[Element], tokens: &Tokens) -> Vec<Segment> {
    let mut segs: Vec<Segment> = Vec::new();
    let mut current = Segment::default();
    for el in alt {
        match &el.kind {
            Kind::Term { .. } | Kind::Regex { .. } | Kind::Token { .. } => {
                current.terms.push(tokens.name(el));
            }
            Kind::Ref { name, debt } => {
                current.reference = Some(name.clone());
                if let Some(d) = debt {
                    current.debt = Some(d.clone());
                }
                segs.push(std::mem::take(&mut current));
            }
            _ => panic!(
                "{}: internal — unexpected element kind '{}' in emitter",
                diag_name(),
                crate::analysis::kind_name(el)
            ),
        }
    }
    if !current.terms.is_empty() || segs.is_empty() {
        segs.push(current);
    }
    segs
}

/// A stable, human-predictable mark for an alternative: its leading
/// discriminator, the first matched token name (sans `#`), the pushed
/// rule name, or `_` for the empty alt.
fn alt_discriminator(alt: &[Element], tokens: &Tokens) -> String {
    let Some(el) = alt.first() else {
        return "_".into();
    };
    let strip = |name: String| {
        let s = name.trim_start_matches('#').to_string();
        if s.is_empty() {
            "_".to_string()
        } else {
            s
        }
    };
    match &el.kind {
        Kind::Term { .. } | Kind::Regex { .. } => strip(tokens.of(el).unwrap_or_default()),
        Kind::Token { name } => strip(name.clone()),
        Kind::Ref { name, .. } => name.clone(),
        _ => "_".into(),
    }
}

/// Assign a unique mark per source alternative, by position. Collisions
/// get a `~N` suffix.
fn assign_marks(alts: &[Sequence], tokens: &Tokens) -> Vec<String> {
    let mut seen: IndexMap<String, usize> = IndexMap::new();
    alts.iter()
        .map(|alt| {
            let base = alt_discriminator(alt, tokens);
            let n = seen.get(&base).copied().unwrap_or(0) + 1;
            seen.insert(base.clone(), n);
            if n == 1 {
                base
            } else {
                format!("{base}~{n}")
            }
        })
        .collect()
}

fn debt_to_n(debt: &IndexMap<String, i64>) -> Map<String, Value> {
    let mut n = Map::new();
    for (k, v) in debt {
        n.insert(k.clone(), json!(v));
    }
    n
}

/// Install the value builders on an alt of a rule that BUILDS A VALUE,
/// dropping the tree builders it was emitted with. No actions at all
/// means NO `a`, not an empty list.
fn use_value_actions(spec: &mut AltSpec, actions: &[&str], cfg: Option<Map<String, Value>>) {
    spec.set_actions(actions);
    if let Some(k) = spec.k_mut() {
        // Builtins mode names its tree config; closure mode carries none.
        k.remove("node$");
        k.remove("capture$");
    }
    if let Some(cfg) = cfg {
        let mut k = spec.k().cloned().unwrap_or_default();
        for (key, value) in cfg {
            k.insert(key, value);
        }
        spec.set("k", Value::Object(k));
    }
    if spec.k().is_some_and(Map::is_empty) {
        spec.remove("k");
    }
}

/// Put the array builder on the close of a link INSIDE an annotated
/// array. No element (a separator, or a push of another collecting
/// helper) drops the tree builders and adds nothing; a rule that builds
/// a value of its own nests whole; anything else resolves to its text.
fn push_element(spec: &mut AltSpec, elem: Option<&str>, value_rules: &IndexSet<String>) {
    match elem {
        None => use_value_actions(spec, &[], None),
        Some(elem) => {
            let cfg = if value_rules.contains(elem) {
                None
            } else {
                let mut m = Map::new();
                m.insert("push$".into(), json!({"src": true}));
                Some(m)
            };
            use_value_actions(spec, &["@push$"], cfg);
        }
    }
}

/// `{ ...base, s, b }`: a copy of an alternate re-keyed on a token
/// sequence and pushing `b` of them back.
fn peek_copy(base: &AltSpec, s: String, b: usize) -> AltSpec {
    let mut g = base.clone();
    g.set("s", s);
    g.set("b", b);
    g
}

impl Emitter<'_> {
    fn record_prov(&mut self, name: &str, origin: &str) {
        if let Some(prov) = &mut self.prov {
            prov.insert(name.to_string(), origin.to_string());
        }
    }

    /// `{ g, s?, p?, n?, a, k? }`: the alternate for one segment.
    fn segment_to_alt(
        &mut self,
        seg: &Segment,
        init_node: bool,
        rule_name: &str,
        kind: NodeKind,
    ) -> AltSpec {
        let mut spec = AltSpec::with_tag(&self.tag);
        if !seg.terms.is_empty() {
            spec.set("s", seg.terms.join(" "));
        }
        if let Some(r) = &seg.reference {
            spec.set("p", r.as_str());
        }
        // Suffix-debt bookkeeping rides on the alt that does the push,
        // so the child inherits the updated counter.
        if let Some(debt) = &seg.debt {
            spec.set("n", Value::Object(debt_to_n(debt)));
        }
        // Default tree-building: accumulate each matched terminal's
        // source text into the node. Head alts also allocate a fresh node.
        let nterms = seg.terms.len();
        if nterms > 0 || init_node {
            self.refs
                .node(&mut spec, init_node, rule_name, kind, nterms);
        }
        spec
    }

    /// `{ r?, a, k?, g }`: a close alternate that captures the returned
    /// child, and replaces itself with `r` when given.
    fn capture_close(&mut self, rule_name: &str, kind: NodeKind, r: Option<&str>) -> AltSpec {
        let mut close = AltSpec::new();
        if let Some(r) = r {
            close.set("r", r);
        }
        self.refs.capture(&mut close, rule_name, kind);
        close.set("g", self.tag.as_str());
        close
    }

    fn validate_refs(&self, alt: &[Element], rule_name: &str) -> Result<(), EmitError> {
        for el in alt {
            if let Kind::Ref { name, .. } = &el.kind {
                if !self.known_rules.contains(name) {
                    return Err(EmitError::at(
                        format!(
                            "{}: rule '{}' references unknown rule '{}'",
                            diag_name(),
                            rule_name,
                            name
                        ),
                        rule_name,
                        el.sp,
                    ));
                }
            }
        }
        Ok(())
    }

    /// A self-looping rule that matches any one of the vocab tokens and
    /// restarts; a final empty-alt fallback ensures the rule NEVER
    /// fails, the property the probe pattern relies on.
    fn emit_probe_helper(&mut self, prod: &Production) {
        let elems = &prod
            .probe_helper
            .as_ref()
            .expect("probe helper")
            .vocab_elements;
        let mut opens: Vec<AltSpec> = Vec::new();
        for el in elems {
            if let Some(tok) = self.tokens.of(el) {
                let mut o = AltSpec::new();
                o.set("s", tok);
                o.set("r", prod.name.as_str());
                o.set("g", self.tag.as_str());
                opens.push(o);
            }
        }
        // Empty fallback: pops without consuming anything. Must be last.
        opens.push(AltSpec::with_tag(&self.tag));
        self.rule_spec.insert(
            prod.name.clone(),
            Some(RuleSpec {
                open: opens,
                close: None,
            }),
        );
    }

    /// The three-phase retry pattern: mark and probe on phase 0, decide
    /// and rewind on its close, commit to a branch on phases 1 and 2.
    fn emit_probe_dispatch(&mut self, prod: &Production) -> Result<(), EmitError> {
        let pd = prod.probe_dispatch.clone().expect("probe dispatch");
        let Some(disambiguator_token) = self.tokens.of(&pd.disambiguator) else {
            return Err(EmitError::new(format!(
                "{}: probe-dispatch rule '{}' has unresolvable disambiguator (kind={})",
                diag_name(),
                prod.name,
                crate::analysis::kind_name(&pd.disambiguator)
            )));
        };
        let tag = self.tag.clone();

        // `bubble` lifts the committed child's node up: pure tree-building.
        // Registered first, as the canonical compiler does, so closure-mode
        // ref names come out in the same order.
        let mut bubble = AltSpec::new();
        self.refs.bubble(&mut bubble);
        bubble.set("g", tag.as_str());

        if self.refs.use_builtins {
            // Function-free dispatcher: control logic is engine
            // `$`-builtins, the disambiguator token rides in `k` config.
            let mut open0 = AltSpec::new();
            open0.set("c", "@probePhase0$");
            open0.set("a", "@probeInit$");
            open0.set("p", pd.probe_rule.as_str());
            open0.set("k", json!({ "pd_d": disambiguator_token }));
            open0.set("g", tag.as_str());
            let mut open1 = AltSpec::new();
            open1.set("c", "@probePhase1$");
            open1.set("p", pd.with_branch.as_str());
            open1.set("g", tag.as_str());
            let mut open2 = AltSpec::new();
            open2.set("c", "@probePhase2$");
            open2.set("p", pd.no_branch.as_str());
            open2.set("g", tag.as_str());
            let mut close0 = AltSpec::new();
            close0.set("c", "@probePhase0$");
            close0.set("a", "@probeDecide$");
            close0.set("r", prod.name.as_str());
            close0.set("g", tag.as_str());
            self.rule_spec.insert(
                prod.name.clone(),
                Some(RuleSpec {
                    open: vec![open0, open1, open2],
                    close: Some(vec![close0, bubble]),
                }),
            );
            return Ok(());
        }

        let init_mark = self.refs.register(RefAction::ProbeInit);
        let decide = self.refs.register(RefAction::ProbeDecide {
            disambiguator: disambiguator_token,
        });
        // Phase 0, first pass: mark and probe.
        let phase0 = self.refs.register(RefAction::ProbePhase(0));
        let mut open0 = AltSpec::new();
        open0.set("c", phase0);
        open0.set("a", init_mark);
        open0.set("p", pd.probe_rule.as_str());
        open0.set("g", tag.as_str());
        // Phase 1, the disambiguator was seen: commit to X D Y.
        let phase1 = self.refs.register(RefAction::ProbePhase(1));
        let mut open1 = AltSpec::new();
        open1.set("c", phase1);
        open1.set("p", pd.with_branch.as_str());
        open1.set("g", tag.as_str());
        // Phase 2, the disambiguator was not seen: commit to Y alone.
        let phase2 = self.refs.register(RefAction::ProbePhase(2));
        let mut open2 = AltSpec::new();
        open2.set("c", phase2);
        open2.set("p", pd.no_branch.as_str());
        open2.set("g", tag.as_str());
        // Phase 0 close: decide the phase from the peek, rewind, retry.
        let close_phase0 = self.refs.register(RefAction::ProbePhase(0));
        let mut close0 = AltSpec::new();
        close0.set("c", close_phase0);
        close0.set("a", decide);
        close0.set("r", prod.name.as_str());
        close0.set("g", tag.as_str());
        self.rule_spec.insert(
            prod.name.clone(),
            Some(RuleSpec {
                open: vec![open0, open1, open2],
                close: Some(vec![close0, bubble]),
            }),
        );
        Ok(())
    }

    /// A production marked by the tail-repeat rewrite: the same shape a
    /// hand-written tabnas grammar uses for `X = a [ b X ]`. Every
    /// iteration folds itself into the parent.
    fn emit_tail_repeat(&mut self, prod: &Production) {
        let prod_kind = prod.node_kind;
        let prefix_alt = &prod.alts[0];
        let sep = &prod.tail_repeat.as_ref().expect("tail repeat").sep;

        // All-terminal sequences, so each segmentizes to exactly one
        // ref-free segment.
        let prefix_seg = segmentize(prefix_alt, &self.tokens).remove(0);
        let sep_seg = segmentize(sep, &self.tokens).remove(0);

        let marks = if prod_kind == NodeKind::User && self.refs.emit_marks {
            Some(assign_marks(
                &[prefix_alt.clone(), sep.clone()],
                &self.tokens,
            ))
        } else {
            None
        };

        let mut open = self.segment_to_alt(&prefix_seg, true, &prod.name, prod_kind);
        if let Some(m) = &marks {
            open.m = Some(m[0].clone());
        }

        // The separator continuation of a repetition is the single most
        // useful resync point a list grammar has.
        let mut repeat = AltSpec::new();
        repeat.set("s", sep_seg.terms.join(" "));
        repeat.set("r", prod.name.as_str());
        self.refs.fold(&mut repeat, Some(sep_seg.terms.len()));
        repeat.set("g", sync_g(&self.tag, "comma"));
        if let Some(m) = &marks {
            repeat.m = Some(m[1].clone());
        }

        let mut end = AltSpec::new();
        self.refs.fold(&mut end, None);
        end.set("g", self.tag.as_str());
        if marks.is_some() {
            end.m = Some("_".into());
        }

        self.rule_spec.insert(
            prod.name.clone(),
            Some(RuleSpec {
                open: vec![open],
                close: Some(vec![repeat, end]),
            }),
        );
    }

    /// FOLLOW₂ exit guards for a CONTESTED repetition, one whose repeated
    /// element covers a follow token at the character level. The
    /// 2-token guard writes the decision down: exit exactly when the
    /// follow token is followed by something only the exit path can
    /// accept.
    fn pair_exit_guards(&self, prod: &Production, base: &AltSpec) -> Vec<AltSpec> {
        if !prod.repeat_helper {
            return Vec::new();
        }
        let Some(pairs) = self.follow_pairs.get(&prod.name) else {
            return Vec::new();
        };
        if pairs.is_empty() {
            return Vec::new();
        }
        let empty = IndexSet::new();
        let cont_first = self.first_sets.get(&prod.name).unwrap_or(&empty);
        let mut out = Vec::new();
        let mut seen: IndexSet<String> = IndexSet::new();
        for (t, us) in pairs {
            if us.is_empty() {
                continue;
            }
            if self.contest.token_ranges_of(t).is_none() {
                continue;
            }
            let contested = cont_first
                .iter()
                .any(|f| f != t && self.contest.tokens_overlap(t, f));
            if !contested {
                continue;
            }
            for u in us {
                let s = format!("{t} {u}");
                if !seen.insert(s.clone()) {
                    continue;
                }
                out.push(peek_copy(base, s, 2));
            }
        }
        out
    }

    /// The 2-token guards for one literal-headed dispatch entry: the
    /// keyword plus a token only the keyword alternative can follow it
    /// with. `None` when no guard can be justified, and then no
    /// reordering happens either.
    fn synth_keyword_guards(
        &self,
        prod: &Production,
        o: &AltSpec,
        alt: &[Element],
        f: &str,
        consumed: i64,
    ) -> Option<Vec<AltSpec>> {
        let paths = alt_prefixes_raw(
            alt,
            self.grammar,
            &self.tokens,
            2,
            &IndexSet::new(),
            None,
            Some(&self.contest.class_sets),
        );
        let mut seconds: IndexSet<String> = IndexSet::new();
        for p in &paths {
            if p.tokens.first().map(String::as_str) != Some(f) {
                continue;
            }
            if p.tokens.len() >= 2 {
                seconds.insert(p.tokens[1].clone());
                continue;
            }
            // The literal can end the alternative: the second token is
            // whatever may follow the production. An unknown FOLLOW means
            // no guard.
            let fol = self.follow_sets.get(&prod.name)?;
            if fol.is_empty() {
                return None;
            }
            seconds.extend(fol.iter().cloned());
        }
        if seconds.is_empty() || seconds.len() > 16 {
            return None;
        }
        Some(
            seconds
                .iter()
                .map(|u| peek_copy(o, format!("{f} {u}"), (2 - consumed).max(0) as usize))
                .collect(),
        )
    }

    /// Keyword-shadow reordering. Every literal-headed dispatch entry
    /// contested by a class-headed entry gets 2-token guards placed ahead
    /// of the first contesting class entry, while its 1-token original
    /// drops behind the class entries so it can no longer steal; entries
    /// that already carry multi-token prefixes simply move ahead.
    fn reorder_keyword_shadow(&self, prod: &Production, entries: &[Entry]) -> Vec<Placed> {
        // A token class's set is a literal head here: its members are
        // literals and engine tokens, never a character class
        // (`token_class_names`), and with the option off those members
        // are literal heads this ordering places, each one.
        let lit_toks: IndexSet<&String> = self
            .tokens
            .literals
            .values()
            .chain(self.contest.class_sets.values())
            .collect();
        let class_toks: IndexSet<&String> = self.tokens.regex_tokens.values().collect();

        // Head token and lookahead length, resolved ONCE per entry.
        let n = entries.len();
        let mut heads: Vec<Option<String>> = Vec::with_capacity(n);
        let mut s_lens: Vec<usize> = Vec::with_capacity(n);
        for e in entries {
            match e.o.s() {
                Some(s) if !s.is_empty() => {
                    heads.push(Some(s.split(' ').next().unwrap_or("").to_string()));
                    s_lens.push(1 + s.matches(' ').count());
                }
                _ => {
                    heads.push(None);
                    s_lens.push(0);
                }
            }
        }

        // Class-headed entries, with their character coverage resolved once.
        let mut class_idx: Vec<usize> = Vec::new();
        let mut class_ranges: Vec<Vec<CharRange>> = Vec::new();
        for (i, head) in heads.iter().enumerate() {
            let Some(f) = head else { continue };
            if !class_toks.contains(f) {
                continue;
            }
            let Some(r) = self.contest.token_ranges_of(f) else {
                continue;
            };
            class_idx.push(i);
            class_ranges.push(r);
        }
        if class_idx.is_empty() {
            return entries
                .iter()
                .enumerate()
                .map(|(i, e)| Placed {
                    o: e.o.clone(),
                    origin: Some(i),
                    rank: i as f64,
                    seq: i,
                })
                .collect();
        }

        let mut placed: Vec<Placed> = Vec::new();
        let mut seq = 0usize;
        let mut put = |o: AltSpec, origin: Option<usize>, rank: f64| {
            placed.push(Placed {
                o,
                origin,
                rank,
                seq,
            });
            seq += 1;
        };

        // Which class entries a given literal head contests, decided once
        // per distinct head token.
        let mut contests_by_head: IndexMap<String, Vec<bool>> = IndexMap::new();

        for (i, e) in entries.iter().enumerate() {
            let f = heads[i].as_deref();
            let fr = match f {
                Some(f) if lit_toks.contains(&f.to_string()) => {
                    self.contest.literal_head_ranges_of(f)
                }
                _ => None,
            };

            // First and last contesting class entry, in one pass.
            let mut first_c: Option<usize> = None;
            let mut last_c: Option<usize> = None;
            if let (Some(fr), Some(_)) = (&fr, &e.alt) {
                let f = f.expect("a literal head is present");
                let hits = contests_by_head.entry(f.to_string()).or_insert_with(|| {
                    class_ranges
                        .iter()
                        .map(|cr| char_ranges_overlap(fr, cr))
                        .collect()
                });
                for (k, c) in class_idx.iter().enumerate() {
                    if !hits[k] {
                        continue;
                    }
                    // Same descent target either way: order is moot.
                    if e.o.p().is_some() && entries[*c].o.p() == e.o.p() {
                        continue;
                    }
                    if first_c.is_none() {
                        first_c = Some(*c);
                    }
                    last_c = Some(*c);
                }
            }

            let Some(first_c) = first_c else {
                put(e.o.clone(), Some(i), i as f64);
                continue;
            };
            let front = first_c as f64 - 0.5;
            let back = last_c.unwrap_or(first_c) as f64 + 0.5;

            if s_lens[i] >= 2 {
                // Already carries its own lookahead: just outrank the class.
                put(e.o.clone(), Some(i), (i as f64).min(front));
                continue;
            }

            let consumed = 1 - e.o.b().map_or(0, |b| b as i64);
            let guards = if consumed == 0 || consumed == 1 {
                self.synth_keyword_guards(
                    prod,
                    &e.o,
                    e.alt.as_deref().expect("checked above"),
                    f.expect("checked above"),
                    consumed,
                )
            } else {
                None
            };
            let Some(guards) = guards else {
                put(e.o.clone(), Some(i), i as f64);
                continue;
            };
            for g in guards {
                put(g, None, (i as f64).min(front));
            }
            put(e.o.clone(), Some(i), (i as f64).max(back));
        }

        placed.sort_by(|a, b| {
            a.rank
                .partial_cmp(&b.rank)
                .unwrap_or(std::cmp::Ordering::Equal)
                .then(a.seq.cmp(&b.seq))
        });
        placed
    }

    /// Specificity ordering among contested class heads: among entries
    /// whose class heads overlap at the character level and whose
    /// descents differ, longer lookahead goes first (maximal munch), then
    /// the longer alternative. Entries are permuted among their own
    /// slots so everything else stays put.
    fn specificity_permute(&self, entries: &mut [Entry]) {
        let class_toks: IndexSet<&String> = self.tokens.regex_tokens.values().collect();
        let n = entries.len();
        let mut s_lens: Vec<usize> = vec![0; n];
        let mut heads: Vec<Option<String>> = vec![None; n];
        for (i, e) in entries.iter().enumerate() {
            let Some(s) = e.o.s().filter(|s| !s.is_empty()) else {
                continue;
            };
            s_lens[i] = 1 + s.matches(' ').count();
            if e.alt.is_none() {
                continue;
            }
            let f = s.split(' ').next().unwrap_or("").to_string();
            if class_toks.contains(&f) {
                heads[i] = Some(f);
            }
        }
        let ranges: Vec<Option<Vec<CharRange>>> = heads
            .iter()
            .map(|h| h.as_deref().and_then(|f| self.contest.token_ranges_of(f)))
            .collect();
        let mut idxs: Vec<usize> = Vec::new();
        for i in 0..n {
            let Some(ri) = &ranges[i] else { continue };
            for (j, rj) in ranges.iter().enumerate() {
                if j == i {
                    continue;
                }
                let Some(rj) = rj else { continue };
                // Same descent target: the order between them is moot.
                // Only when a descent EXISTS, though.
                if entries[i].o.p().is_some() && entries[j].o.p() == entries[i].o.p() {
                    continue;
                }
                if char_ranges_overlap(ri, rj) {
                    idxs.push(i);
                    break;
                }
            }
        }
        if idxs.len() < 2 {
            return;
        }
        // How much the alternative behind an entry can consume in total:
        // the longer alternative is the more specific one.
        let spans: IndexMap<usize, f64> = idxs
            .iter()
            .map(|&i| {
                let span = match &entries[i].alt {
                    None => 0.0,
                    Some(alt) => match seq_token_span(alt, self.grammar, &IndexSet::new()) {
                        Some(n) => n as f64,
                        None => 1e9,
                    },
                };
                (i, span)
            })
            .collect();
        let mut order = idxs.clone();
        order.sort_by(|&a, &b| {
            s_lens[b].cmp(&s_lens[a]).then(
                spans[&b]
                    .partial_cmp(&spans[&a])
                    .unwrap_or(std::cmp::Ordering::Equal),
            )
        });
        let picked: Vec<Entry> = order.iter().map(|&i| entries[i].clone()).collect();
        for (k, slot) in idxs.iter().enumerate() {
            entries[*slot] = picked[k].clone();
        }
    }

    /// True for a repetition helper whose content can start with a token
    /// its FOLLOW also contains.
    fn contested_by_follow(&self, prod: &Production, alt: &[Element]) -> bool {
        if !prod.repeat_helper {
            return false;
        }
        let Some(mine) = first_of_alt(alt, &self.tokens, &self.first_sets, &self.nullable) else {
            return false;
        };
        let empty = IndexSet::new();
        let fol = self.follow_sets.get(&prod.name).unwrap_or(&empty);
        mine.iter().any(|t| {
            fol.iter()
                .any(|f| f == t || self.contest.tokens_overlap(t, f))
        })
    }

    /// True when this alternative's first tokens overlap another
    /// alternative's at the character level with a different descent.
    fn alt_head_contested(&self, idx: usize, all: &[Sequence]) -> bool {
        let Some(mine) = first_of_alt(&all[idx], &self.tokens, &self.first_sets, &self.nullable)
        else {
            return false;
        };
        for (j, other) in all.iter().enumerate() {
            if j == idx || other.is_empty() {
                continue;
            }
            let Some(theirs) = first_of_alt(other, &self.tokens, &self.first_sets, &self.nullable)
            else {
                continue;
            };
            for t in &mine {
                for u in &theirs {
                    if self.contest.tokens_overlap(t, u) {
                        return true;
                    }
                }
            }
        }
        false
    }

    /// Can two alternates be handed the SAME token? Deliberately narrower
    /// than [`alt_head_contested`](Self::alt_head_contested): two
    /// different class tokens can still be handed the same token,
    /// because a class that spans several atoms is a set.
    fn alt_head_shares_token(&self, idx: usize, all: &[Sequence]) -> bool {
        let Some(mine) = first_of_alt(&all[idx], &self.tokens, &self.first_sets, &self.nullable)
        else {
            return false;
        };
        let class_head_toks: IndexSet<&String> = self.tokens.regex_tokens.values().collect();
        for (j, other) in all.iter().enumerate() {
            if j == idx || other.is_empty() {
                continue;
            }
            let Some(theirs) = first_of_alt(other, &self.tokens, &self.first_sets, &self.nullable)
            else {
                continue;
            };
            for t in &mine {
                for u in &theirs {
                    if t == u {
                        return true;
                    }
                    if class_head_toks.contains(t)
                        && class_head_toks.contains(u)
                        && self.contest.tokens_overlap(t, u)
                    {
                        return true;
                    }
                }
            }
        }
        false
    }

    /// Suffix-debt guard for a contested left-recursion tail loop: a
    /// branch that would eat a token an enclosing frame still owes may
    /// only run while the debt is zero. Only the branches whose head
    /// token is contested are guarded, and never the exit peeks or the
    /// bare fallback.
    fn apply_debt_guard(prod: &Production, list: &mut [Entry]) {
        let (Some(guard), Some(owed)) = (&prod.debt_guard, &prod.debt_owed) else {
            return;
        };
        let owed: IndexSet<&String> = owed.iter().collect();
        // Scalar shorthand for `$eq`, the one spelling shape-identical in
        // every runtime.
        let c = json!({ format!("n.{guard}"): 0 });
        for e in list.iter_mut() {
            if e.alt.as_ref().is_none_or(Vec::is_empty) {
                continue;
            }
            // Entries are keyed by the token sequence they peek, so the
            // head token says which branch this is.
            if let Some(s) = e.o.s() {
                let head = s.split(' ').next().unwrap_or("").to_string();
                if !owed.contains(&head) {
                    continue;
                }
            }
            e.o.set("c", c.clone());
        }
    }

    fn emit_production(&mut self, prod: &Production) -> Result<(), EmitError> {
        // This rule stands inside an annotated array: it inherits that
        // array from its parent and pushes into it, instead of building a
        // node.
        let array_elem = self.array_helpers.contains(&prod.name);
        let tag = self.tag.clone();

        for alt in &prod.alts {
            self.validate_refs(alt, &prod.name)?;
        }

        if prod.tail_repeat.is_some() {
            self.emit_tail_repeat(prod);
            return Ok(());
        }

        let all_simple = prod.alts.iter().all(is_single_segment);
        let prod_kind = prod.node_kind;

        // A collecting helper with several alternatives shares ONE close
        // alt here, and the alternatives need not agree. The dispatcher
        // path gives each alternative its own rule, and so its own close.
        let split_alts = array_elem && prod.alts.len() > 1 && !prod.repeat_helper;

        if all_simple && prod.value.is_none() && !split_alts {
            // Every alternative collapses to one tabnas alt. Empty alts
            // are sorted to the end so first-match-wins cannot let them
            // short-circuit non-empty alternatives.
            let mut ordered: Vec<Sequence> = prod
                .alts
                .iter()
                .filter(|a| !a.is_empty())
                .cloned()
                .collect();
            ordered.extend(prod.alts.iter().filter(|a| a.is_empty()).cloned());

            let marks = if prod_kind == NodeKind::User && self.refs.emit_marks {
                Some(assign_marks(&ordered, &self.tokens))
            } else {
                None
            };
            let needs_peek = ordered.len() > 1;
            let mut entries: Vec<Entry> = Vec::new();
            for (idx, alt) in ordered.iter().enumerate() {
                let segs = segmentize(alt, &self.tokens);
                let seg = segs[0].clone();
                let is_ref_only = !alt.is_empty()
                    && alt.iter().all(Element::is_ref)
                    && seg.terms.is_empty()
                    && seg.reference.is_some();
                let mark = marks.as_ref().map(|m| m[idx].clone());

                // Ref-only alternatives have no terminal to discriminate
                // on, so guard them with FIRST-set peeks when the
                // production has more than one alt.
                if needs_peek && is_ref_only {
                    if let Some(first_tokens) =
                        first_of_alt(alt, &self.tokens, &self.first_sets, &self.nullable)
                    {
                        let mut node_fields = AltSpec::new();
                        self.refs
                            .node(&mut node_fields, true, &prod.name, prod_kind, 0);
                        // A contested head cannot be decided by one
                        // token: fan out to K-token prefixes (bounded,
                        // deduped; bail to the 1-token peek if the fan-out
                        // is degenerate).
                        let mut paths: Option<Vec<Vec<String>>> = None;
                        if self.alt_head_contested(idx, &ordered)
                            || self.contested_by_follow(prod, alt)
                        {
                            let pfx: Vec<Vec<String>> = alt_prefixes(
                                alt,
                                self.grammar,
                                &self.tokens,
                                LOOKAHEAD_K,
                                Some(&self.contest.class_sets),
                            )
                            .into_iter()
                            .filter(|p| !p.is_empty())
                            .collect();
                            if !pfx.is_empty() && pfx.len() <= 64 {
                                paths = Some(pfx);
                            }
                        }
                        // `{ s, b, p, n?, a, k?, g }`, with the same
                        // suffix-debt bookkeeping `segment_to_alt` carries.
                        let debt = seg.debt.as_ref().map(debt_to_n);
                        let mut push_entry = |s: String, b: usize| {
                            let mut o = AltSpec::new();
                            o.set("s", s);
                            o.set("b", b);
                            o.set("p", seg.reference.clone().unwrap_or_default());
                            if let Some(n) = &debt {
                                o.set("n", Value::Object(n.clone()));
                            }
                            o.assign_from(&node_fields);
                            o.set("g", tag.as_str());
                            o.m = mark.clone();
                            entries.push(Entry {
                                o,
                                alt: Some(alt.clone()),
                            });
                        };
                        match paths {
                            Some(paths) => {
                                for p in paths {
                                    push_entry(p.join(" "), p.len());
                                }
                            }
                            None => {
                                for tok in &first_tokens {
                                    push_entry(tok.clone(), 1);
                                }
                            }
                        }
                        continue;
                    }
                }
                let mut o = self.segment_to_alt(&seg, true, &prod.name, prod_kind);
                o.m = mark;
                // The terminating alternative of a repetition helper
                // names no token, so the lexer is never asked to produce
                // whatever follows the repetition. Re-issue that
                // alternative once per FOLLOW token, peeking and pushing
                // straight back. The bare alternative stays last.
                if alt.is_empty() && prod.repeat_helper {
                    let fol: Vec<String> = self
                        .follow_sets
                        .get(&prod.name)
                        .map(|s| s.iter().cloned().collect())
                        .unwrap_or_default();
                    for tok in fol {
                        entries.push(Entry {
                            o: peek_copy(&o, tok, 1),
                            alt: None,
                        });
                    }
                    // Contested repetitions additionally get FOLLOW₂
                    // guards, at the FRONT so they outrank the continue
                    // alternatives.
                    let guards: Vec<Entry> = self
                        .pair_exit_guards(prod, &o)
                        .into_iter()
                        .map(|g| Entry { o: g, alt: None })
                        .collect();
                    entries.splice(0..0, guards);
                }

                // `terms… ref` against a sibling that stops on the same
                // head: name the reference's FIRST token in `s` and push
                // it straight back, so the shorter sibling stays
                // reachable. These entries REPLACE the one-token form.
                let ref_peek: Option<Vec<String>> = match &seg.reference {
                    Some(r)
                        if needs_peek
                            && !seg.terms.is_empty()
                            && !self.nullable.contains(r)
                            && self.alt_head_shares_token(idx, &ordered) =>
                    {
                        Some(
                            self.first_sets
                                .get(r)
                                .map(|s| s.iter().cloned().collect())
                                .unwrap_or_default(),
                        )
                    }
                    _ => None,
                };
                if let Some(peek) = ref_peek {
                    if !peek.is_empty() && peek.len() <= 64 {
                        for tok in peek {
                            let mut terms = seg.terms.clone();
                            terms.push(tok);
                            entries.push(Entry {
                                o: peek_copy(&o, terms.join(" "), 1),
                                alt: Some(alt.clone()),
                            });
                        }
                        continue;
                    }
                }

                entries.push(Entry {
                    o,
                    alt: if alt.is_empty() {
                        None
                    } else {
                        Some(alt.clone())
                    },
                });
            }

            Self::apply_debt_guard(prod, &mut entries);
            self.specificity_permute(&mut entries);
            let placed = self.reorder_keyword_shadow(prod, &entries);

            // If any alt has a push, the close state must capture the
            // returned child.
            let mut close: Option<Vec<AltSpec>> = None;
            if prod.alts.iter().any(|alt| alt.iter().any(Element::is_ref)) {
                let mut c = self.capture_close(&prod.name, prod_kind, None);
                if marks.is_some() {
                    c.m = Some("_".into());
                }
                close = Some(vec![c]);
            }
            let mut opens: Vec<AltSpec> = Vec::with_capacity(placed.len());
            for p in placed {
                let mut o = p.o;
                // The value actions are applied to the entries themselves,
                // after the reordering copied the keyword guards off them.
                if array_elem && p.origin.is_some() {
                    use_value_actions(&mut o, &[], None);
                }
                opens.push(o);
            }
            if array_elem {
                // At most one distinct pushed rule is an ELEMENT here.
                let elem: Option<String> = ordered
                    .iter()
                    .filter_map(|alt| {
                        segmentize(alt, &self.tokens)
                            .first()
                            .and_then(|s| s.reference.clone())
                    })
                    .find(|r| !self.array_helpers.contains(r));
                if let Some(close) = &mut close {
                    for c in close.iter_mut() {
                        push_element(c, elem.as_deref(), &self.value_rules);
                    }
                }
            }
            self.rule_spec
                .insert(prod.name.clone(), Some(RuleSpec { open: opens, close }));
            return Ok(());
        }

        if prod.alts.len() == 1 {
            // Single-alt, multi-segment: chain rules directly on the
            // production.
            let origin = origin_of(prod).to_string();
            let nested = self.value_plan.get(&origin).cloned();
            self.emit_chain(
                &prod.name,
                &prod.alts[0],
                prod_kind,
                Some(&origin),
                prod.value.as_ref(),
                nested.as_deref(),
                prod.sp,
                array_elem,
            )?;
            return Ok(());
        }

        // A value annotation names one member per pushing segment, which
        // only has one reading when the production HAS one alternative.
        if prod.value.is_some() {
            return Err(EmitError::new(format!(
                "{}: rule '{}' has a value annotation and {} alternatives. A value \
                 annotation names the parts of ONE alternative; with more than one it \
                 is ambiguous which alternative's parts are named. Split the rule, or \
                 annotate the alternatives' own rules.",
                diag_name(),
                origin_of(prod),
                prod.alts.len()
            )));
        }

        // Multi-alt with at least one multi-segment alternative: emit a
        // dispatcher. Each alt becomes its own chained impl rule
        // (`<prodname>$alt<i>`); the main rule's open peeks the first
        // token and pushes the matching impl rule.
        let mut dispatch_entries: Vec<Entry> = Vec::new();
        let mut empty_alt_seen = false;
        let mut nullable_impls: Vec<(String, AltSpec, Option<String>)> = Vec::new();
        let dispatch_marks = if prod_kind == NodeKind::User && self.refs.emit_marks {
            Some(assign_marks(&prod.alts, &self.tokens))
        } else {
            None
        };
        let origin = origin_of(prod).to_string();

        // The ways out of this choice, for the contest check: a choice with
        // an empty alternative (or one that derives ε) can end on any token
        // that may follow it, so a content head the lexer could also read
        // as a follow token has to look further before it commits.
        let mut exit_paths: Vec<Vec<String>> = Vec::new();
        let any_epsilon = prod.alts.iter().any(|alt| {
            alt.is_empty()
                || first_of_alt(alt, &self.tokens, &self.first_sets, &self.nullable).is_none()
        });
        if any_epsilon {
            if let Some(fol) = self.follow_sets.get(&prod.name) {
                for t in fol {
                    exit_paths.push(vec![t.clone()]);
                }
            }
            if let Some(pairs) = self.follow_pairs.get(&prod.name) {
                for (t, us) in pairs {
                    for u in us {
                        exit_paths.push(vec![t.clone(), u.clone()]);
                    }
                }
            }
        }
        let dispatch = {
            let contest = &self.contest;
            dispatch_prefixes(
                &prod.alts,
                self.grammar,
                &self.tokens,
                LOOKAHEAD_K,
                &|a, b| contest.heads_contest(a, b),
                &exit_paths,
                &contest.class_sets,
            )
        };

        for (i, alt) in prod.alts.iter().enumerate() {
            let impl_name = format!("{}$alt{}", prod.name, i);
            let mark = dispatch_marks.as_ref().map(|m| m[i].clone());
            if alt.is_empty() {
                // Empty alt acts as fallback, handled after the loop.
                empty_alt_seen = true;
                continue;
            }
            // One impl rule per alternative of a multi-segment dispatch:
            // the author wrote one rule with alternatives, not N rules.
            self.record_prov(&impl_name, &origin);

            self.emit_chain(
                &impl_name,
                alt,
                NodeKind::Helper,
                Some(&origin),
                None,
                None,
                None,
                array_elem,
            )?;

            // The dispatcher itself is a user (or helper) rule: it must
            // allocate its own AST node on every dispatch alt.
            let mut init_dispatch_fields = AltSpec::new();
            self.refs
                .node(&mut init_dispatch_fields, true, &prod.name, prod_kind, 0);

            // One dispatch entry per prefix this alternative needs to be
            // told apart from its rivals: its first token where that
            // decides, and deeper prefixes only under a head another
            // alternative (or the way out of the choice) shares. See
            // `dispatch_prefixes`. The entry pushes the alternative's own
            // rule, which parses it whole, so the trees are those of the
            // K-token fan-out this replaced.
            //
            // An alternative that can derive ε has no prefix for that
            // derivation. Remember it: it is re-issued as FOLLOW-guarded
            // entries plus a bare fallback, after every content entry.
            if dispatch[i].nullable {
                nullable_impls.push((
                    impl_name.clone(),
                    init_dispatch_fields.clone(),
                    mark.clone(),
                ));
            }
            let usable: Vec<Vec<String>> = dispatch[i].prefixes.clone();
            // `{ s, b, p, a, k?, g }`.
            let mut push_entry = |s: String, b: usize| {
                let mut o = AltSpec::new();
                o.set("s", s);
                o.set("b", b);
                o.set("p", impl_name.as_str());
                o.assign_from(&init_dispatch_fields);
                o.set("g", tag.as_str());
                o.m = mark.clone();
                dispatch_entries.push(Entry {
                    o,
                    alt: Some(alt.clone()),
                });
            };
            if !usable.is_empty() {
                for p in usable {
                    push_entry(p.join(" "), p.len());
                }
            } else {
                let Some(first_tokens) =
                    first_of_alt(alt, &self.tokens, &self.first_sets, &self.nullable)
                else {
                    return Err(EmitError::at(
                        format!(
                            "{}: rule '{}' alternative {} is nullable but is not the only \
                             empty alt; FIRST set is ambiguous",
                            diag_name(),
                            prod.name,
                            i
                        ),
                        &prod.name,
                        prod.sp,
                    ));
                };
                for tok in &first_tokens {
                    push_entry(tok.clone(), 1);
                }
            }
        }

        // Re-issue each nullable alternative's ε-derivation: FOLLOW peeks
        // first, then one unguarded fallback that pushes the impl with
        // nothing consumed.
        let fol: Vec<String> = self
            .follow_sets
            .get(&prod.name)
            .map(|s| s.iter().cloned().collect())
            .unwrap_or_default();
        for (impl_name, fields, mark) in &nullable_impls {
            for tok in &fol {
                let mut o = AltSpec::new();
                o.set("s", tok.as_str());
                o.set("b", 1usize);
                o.set("p", impl_name.as_str());
                o.assign_from(fields);
                o.set("g", tag.as_str());
                o.m = mark.clone();
                dispatch_entries.push(Entry { o, alt: None });
            }
            let mut o = AltSpec::new();
            o.set("p", impl_name.as_str());
            o.assign_from(fields);
            o.set("g", tag.as_str());
            o.m = mark.clone();
            dispatch_entries.push(Entry { o, alt: None });
        }

        if empty_alt_seen {
            // Fallback: matches any token (or none), pops immediately
            // with an empty tree.
            let mut o = AltSpec::new();
            self.refs.node(&mut o, true, &prod.name, prod_kind, 0);
            o.set("g", tag.as_str());
            if dispatch_marks.is_some() {
                o.m = Some("_".into());
            }
            // Same FOLLOW guard as the single-segment path above.
            if prod.repeat_helper {
                for tok in &fol {
                    dispatch_entries.push(Entry {
                        o: peek_copy(&o, tok.clone(), 1),
                        alt: None,
                    });
                }
                let guards: Vec<Entry> = self
                    .pair_exit_guards(prod, &o)
                    .into_iter()
                    .map(|g| Entry { o: g, alt: None })
                    .collect();
                dispatch_entries.splice(0..0, guards);
            }
            dispatch_entries.push(Entry { o, alt: None });
        }

        // Merge the chosen impl's result up into the dispatcher's node,
        // tagged with the user rule name.
        let mut disp_close = self.capture_close(&prod.name, prod_kind, None);
        if dispatch_marks.is_some() {
            disp_close.m = Some("_".into());
        }
        // The impl rule this dispatches to inherits the array and fills
        // it directly, so the dispatcher allocates and captures nothing.
        if array_elem {
            for e in dispatch_entries.iter_mut() {
                use_value_actions(&mut e.o, &[], None);
            }
            use_value_actions(&mut disp_close, &[], None);
        }
        Self::apply_debt_guard(prod, &mut dispatch_entries);
        self.specificity_permute(&mut dispatch_entries);
        let opens: Vec<AltSpec> = self
            .reorder_keyword_shadow(prod, &dispatch_entries)
            .into_iter()
            .map(|p| p.o)
            .collect();
        self.rule_spec.insert(
            prod.name.clone(),
            Some(RuleSpec {
                open: opens,
                close: Some(vec![disp_close]),
            }),
        );
        Ok(())
    }

    /// Emit a (possibly single-step) chain of rules for one alt under the
    /// given head rule name. Segment 0 goes into `head_name`; later
    /// segments get synthetic `<head_name>$stepN` continuations, always
    /// helpers, which inherit and accumulate into the head's node via
    /// `r:` replacement.
    #[allow(clippy::too_many_arguments)]
    fn emit_chain(
        &mut self,
        head_name: &str,
        alt: &[Element],
        head_kind: NodeKind,
        origin: Option<&str>,
        value: Option<&ValueAnnotation>,
        // One flag per pushing part of `alt`: does that part's own rule
        // build a value, and so nest whole rather than resolve to text?
        nested: Option<&[bool]>,
        sp: Option<SrcSpan>,
        // This chain is a helper INSIDE an annotated array: it inherits
        // that array rather than allocating a node.
        array_elem: bool,
    ) -> Result<(), EmitError> {
        let tag = self.tag.clone();
        let segs = segmentize(alt, &self.tokens);
        let chain_name = |i: usize| -> String {
            if i == 0 {
                head_name.to_string()
            } else {
                format!("{head_name}$step{i}")
            }
        };

        // Value building rides on the segments that PUSH: those are the
        // parts that produce a member.
        let member_segs: Vec<bool> = if value.is_some() {
            segs.iter().map(|s| s.reference.is_some()).collect()
        } else {
            Vec::new()
        };
        let member_at = |i: usize| -> Option<usize> {
            if member_segs.get(i).copied().unwrap_or(false) {
                Some(member_segs[..i].iter().filter(|m| **m).count())
            } else {
                None
            }
        };
        let diag_rule = origin.unwrap_or(head_name);

        if let Some(v) = value {
            // An unknown kind must not fall through as an object.
            if v.kind != "object" && v.kind != "array" {
                return Err(EmitError::at(
                    format!(
                        "{}: rule '{}' has a value annotation of unknown kind '{}'. A rule \
                         builds an 'object' or an 'array'.",
                        diag_name(),
                        diag_rule,
                        v.kind
                    ),
                    diag_rule,
                    sp,
                ));
            }
            let pushes = member_segs.iter().filter(|m| **m).count();
            // One name per pushing segment, or the names line up with
            // the wrong parts.
            if v.kind == "object" {
                let named = v.members.len();
                if named != pushes {
                    return Err(EmitError::at(
                        format!(
                            "{}: rule '{}' names {} member{} but builds {}. A value \
                             annotation names one member per part that produces a value. A \
                             part made only of literals produces none — and note that a \
                             part whose own rule is a single terminal stops being a part \
                             here in two ways: as a LEADING part it is folded into this \
                             rule by left-recursion elimination, and anywhere else it \
                             becomes a named lexer token. Giving that rule a body that is \
                             not a bare terminal (a repetition, a group, or more than one \
                             element) keeps it nameable.",
                            diag_name(),
                            diag_rule,
                            named,
                            if named == 1 { "" } else { "s" },
                            pushes
                        ),
                        diag_rule,
                        sp,
                    ));
                }
            }
            // The same count, checked against the PLAN rather than the
            // names, because an array has no names.
            if let Some(nested) = nested {
                if nested.len() != pushes {
                    return Err(EmitError::at(
                        format!(
                            "{}: rule '{}' has a value annotation for {} part{} but builds \
                             {}. A rewrite pass changed how many parts of this rule produce \
                             a value, so the annotation no longer describes it. A part \
                             whose own rule is a single literal is the usual cause: it \
                             becomes a lexer token here and stops being a part at all. Give \
                             that rule a body that is not a bare terminal.",
                            diag_name(),
                            diag_rule,
                            nested.len(),
                            if nested.len() == 1 { "" } else { "s" },
                            pushes
                        ),
                        diag_rule,
                        sp,
                    ));
                }
            }
        }

        for (i, seg) in segs.iter().enumerate() {
            let name = chain_name(i);
            let kind = if i == 0 { head_kind } else { NodeKind::Helper };
            // Only the head of the chain initialises the node object;
            // later steps inherit and continue to accumulate into it.
            let mut head_alt = self.segment_to_alt(seg, i == 0, &name, kind);
            // Single-alt user rule: the head alt is user-addressable.
            if i == 0 && head_kind == NodeKind::User && self.refs.emit_marks {
                head_alt.m = Some(alt_discriminator(alt, &self.tokens));
            }

            // Step rules exist only because the alternative had more than
            // one segment; nothing in the author's grammar is named after
            // them.
            if i > 0 {
                if let Some(origin) = origin {
                    self.record_prov(&name, origin);
                }
            }

            let is_last = i == segs.len() - 1;
            let mut close: Option<Vec<AltSpec>> = None;
            if !is_last {
                // After the push returns, capture the child's node and
                // replace with the next step rule.
                let next = chain_name(i + 1);
                close = Some(vec![self.capture_close(&name, kind, Some(&next))]);
            } else if seg.reference.is_some() {
                // Last step with a push: capture the final child before
                // popping.
                close = Some(vec![self.capture_close(&name, kind, None)]);
            }

            if array_elem {
                use_value_actions(&mut head_alt, &[], None);
                let elem: Option<String> = seg
                    .reference
                    .as_ref()
                    .filter(|r| !self.array_helpers.contains(*r))
                    .cloned();
                if let Some(close) = &mut close {
                    for c in close.iter_mut() {
                        push_element(c, elem.as_deref(), &self.value_rules);
                    }
                }
            }

            if let Some(v) = value {
                let is_array = v.kind == "array";
                let m = member_at(i);
                let member_name: Option<&String> = m.and_then(|m| v.members.get(m));

                // Open side: the container is allocated once, on the
                // head; every pushing link names the member it fills.
                let mut open_acts: Vec<&str> = Vec::new();
                let mut open_cfg: Map<String, Value> = Map::new();
                if i == 0 {
                    open_acts.push(if is_array { "@array$" } else { "@object$" });
                }
                if !is_array {
                    if let Some(member_name) = member_name {
                        // The key is a CONSTANT: the author named this part.
                        open_acts.push("@key$");
                        open_cfg.insert("key$".into(), json!({ "lit": member_name }));
                    }
                }
                // Unconditional: EVERY link of a value rule must lose its
                // tree builders, not just the ones that gain value
                // builders.
                use_value_actions(&mut head_alt, &open_acts, Some(open_cfg));

                // Close side: a member that builds its OWN value is
                // assigned whole, so it nests; anything else resolves to
                // its source text. A part that is sugar under an array
                // fills the array itself, so this link only sheds its
                // tree builders.
                let fills = is_array
                    && seg
                        .reference
                        .as_ref()
                        .is_some_and(|r| self.array_helpers.contains(r));
                match m {
                    Some(m) if !fills => {
                        let is_nested = nested.is_some_and(|n| n.get(m).copied().unwrap_or(false));
                        let act = if is_array { "@push$" } else { "@setval$" };
                        let cfg = if is_nested {
                            None
                        } else {
                            let mut c = Map::new();
                            c.insert(
                                if is_array { "push$" } else { "setval$" }.into(),
                                json!({ "src": true }),
                            );
                            Some(c)
                        };
                        let close = close.get_or_insert_with(|| vec![AltSpec::with_tag(&tag)]);
                        for c in close.iter_mut() {
                            use_value_actions(c, &[act], cfg.clone());
                        }
                    }
                    _ => {
                        // A link that pushes nothing contributes no
                        // member, but its close still carries a tree
                        // capture for a node this rule no longer has.
                        if let Some(close) = &mut close {
                            for c in close.iter_mut() {
                                use_value_actions(c, &[], None);
                            }
                        }
                    }
                }
            }
            self.rule_spec.insert(
                name,
                Some(RuleSpec {
                    open: vec![head_alt],
                    close,
                }),
            );
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn term(lit: &str) -> Element {
        Element::term(lit)
    }
    fn reference(name: &str) -> Element {
        Element::reference(name)
    }
    fn at(s: usize, e: usize, r: usize, c: usize) -> SrcSpan {
        SrcSpan::at(s, e, r, c)
    }

    // A production is REBUILT by several passes, so a new field is
    // silently lost unless every one of them carries it across. The
    // grammar is shaped so that each rebuilding pass has a spanned
    // production to mangle:
    //
    //   doc   probe dispatch     rewrite_probe_dispatches' rebuild
    //   expr  direct left rec.   eliminate_direct_left_rec's star return
    //   lead  leading ref        substitute_leading_ref
    //   triv  trivial self-ref   eliminate_direct_left_rec's seeds-only return
    //   all                      the working copy, desugar
    //
    // Every one of them must come out the far end still knowing where the
    // author wrote it. Mirrors go/bnf_test.go
    // TestProductionSpansSurviveTheRewritePasses.
    #[test]
    fn production_spans_survive_the_rewrite_passes() {
        let want: IndexMap<&str, SrcSpan> = IndexMap::from([
            ("doc", at(0, 24, 1, 1)),
            ("x", at(24, 40, 2, 1)),
            ("y", at(40, 56, 3, 1)),
            ("expr", at(56, 80, 4, 1)),
            ("lead", at(80, 96, 5, 1)),
            ("triv", at(96, 112, 6, 1)),
        ]);
        let spanned = |name: &str, alts: Vec<Sequence>| Production {
            sp: Some(want[name]),
            ..Production::new(name, alts)
        };
        let opt_group = Element::opt(Element::group(vec![vec![reference("x"), term("@")]]));
        let g = Grammar::new(vec![
            // The optional prefix shares vocabulary with the tail, so the
            // probe rewriter rebuilds `doc`.
            spanned("doc", vec![vec![opt_group, reference("y")]]),
            spanned("x", vec![vec![term("a")], vec![term("b")]]),
            spanned("y", vec![vec![term("a")], vec![term("c")]]),
            spanned(
                "expr",
                vec![
                    vec![reference("expr"), term("+"), reference("x")],
                    vec![reference("x")],
                ],
            ),
            // A leading reference Paull's substitution inlines.
            spanned("lead", vec![vec![reference("expr"), term("!")]]),
            // A trivial self-reference, dropped rather than eliminated.
            spanned("triv", vec![vec![reference("triv")], vec![term("z")]]),
        ]);

        // The rewrite pipeline emit_grammar_spec runs, in its order.
        let start = "doc";
        let mut out = g.clone();
        resolve_prose_terminals(&mut out).expect("prose");
        lift_literal_tokens(&mut out, start);
        normalize_builtin_tokens(&mut out);
        let out = crate::leftrec::eliminate_left_recursion(&out).expect("left recursion");
        let out = rewrite_probe_dispatches(&out).expect("probe");
        let out = left_factor(&out).expect("factor");
        let out = rewrite_tail_repeats(out, start);
        let out = desugar(&out);

        for (name, sp) in &want {
            let p = out
                .find(name)
                .unwrap_or_else(|| panic!("rule {name:?} did not survive the rewrite passes"));
            assert_eq!(
                p.sp,
                Some(*sp),
                "rule {name:?}: a rebuild site dropped its span"
            );
        }
        // The probe rewriter must actually have fired, or `doc` proves
        // nothing about its rebuild site.
        assert!(
            out.productions.iter().any(|p| p.probe_dispatch.is_some()),
            "no probe dispatcher was synthesised"
        );
        // Synthesised productions locate themselves by origin, not by a
        // span the author never wrote.
        for p in &out.productions {
            if p.origin.is_some() {
                assert!(
                    p.sp.is_none(),
                    "synthesised rule {:?} carries a span",
                    p.name
                );
            }
        }
        // The caller's own IR is untouched.
        for p in &g.productions {
            assert_eq!(p.sp, Some(want[p.name.as_str()]));
        }
    }
}
