// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

//! Character coverage of emitted matcher patterns, and the partition of
//! overlapping character classes into disjoint atoms. Mirrors
//! `patternCharRanges`, `foldCaseRanges`, `normalizeRanges`,
//! `charRangesOverlap`, `singleCodePointRanges`, `partitionRanges`,
//! `classPattern` and `classAnalysis` in `ts/src/compiler.ts`.
//!
//! The three contested-alternative passes all ask the same question: can
//! these two tokens claim the SAME input character? A tokenising notation
//! never contests one, so every guard built on this is inert for ABNF and
//! EBNF; a scannerless one contests constantly, and the answer decides
//! which alternative may win.

use indexmap::{IndexMap, IndexSet};

use crate::ir::{regex_key, Element, Kind};

/// An inclusive code-point span.
pub type CharRange = (u32, u32);

const MAX_CODE_POINT: u32 = 0x10FFFF;

/// Character coverage of an emitted matcher pattern, as code-point
/// ranges, or `None` when the pattern is not a shape this parser
/// understands (the caller must then treat coverage as unknown and stay
/// conservative).
///
/// Handles exactly what the emitter itself produces: a leading character
/// class with `\uXXXX` / `\u{…}` / `\xXX` escapes and ranges, `[\s\S]`,
/// negation, or a single (possibly escaped) literal character. Trailing
/// content after the first class is irrelevant: only the FIRST
/// character's coverage decides whether two tokens can contest one input
/// position.
/// Where a pattern's first atom or class ends, in chars, or `None` when
/// the pattern does not begin with one: a group, an alternation, a
/// quantifier, `.`, or an escape whose coverage `pattern_char_ranges`
/// declines to name.
pub fn regex_head_atom_end(src: &str) -> Option<usize> {
    let chars: Vec<char> = src.chars().collect();
    let c = *chars.first()?;
    if c == '[' {
        let mut i = 1;
        if chars.get(i) == Some(&'^') {
            i += 1;
        }
        if chars.get(i) == Some(&']') {
            i += 1;
        }
        while i < chars.len() && chars[i] != ']' {
            i += if chars[i] == '\\' { 2 } else { 1 };
        }
        return (i < chars.len()).then_some(i + 1);
    }
    if c == '\\' {
        // Exactly the escapes `pattern_char_ranges` reads: a head this
        // calls one atom must be one whose coverage is known.
        return read_escape(&chars, 0).map(|(n, _)| n);
    }
    if "(.|)?*+{".contains(c) {
        return None;
    }
    Some(1)
}

/// The escape at `chars[at]` read as one code point by the `regex` crate's
/// rules, the dialect every matcher this port emits is compiled in
/// (`check_regex` in `src/emit.rs`): its length in chars and the code
/// point, or `None` when it is not one this can name.
///
/// A hex escape is the code point it spells, in each of the crate's forms,
/// `\xHH`, `\uHHHH` and `\UHHHHHHHH` with all their digits and the brace
/// forms `\x{…}`, `\u{…}` and `\U{…}`, and only when that is a Unicode
/// scalar value (the crate refuses a surrogate). `\a` is BEL, U+0007, and
/// escaped ASCII punctuation is the character itself, except `\<` and
/// `\>`. Everything else bails rather than guess: a shorthand or property
/// class, a digit escape, the zero-width `\A`, `\z`, `\b`, `\B`, `\<` and
/// `\>`, and every letter the crate does not define. `\f`, `\n`, `\r`,
/// `\t` and `\v` bail as well although the crate names them, because the
/// canonical TypeScript reader declines them, and where the two dialects
/// agree the two compilers should emit alike; `\u{…}` keeps that reader's
/// one to six digits for the same reason. Unknown coverage keeps the
/// dispatcher conservative.
fn read_escape(chars: &[char], at: usize) -> Option<(usize, u32)> {
    let m = *chars.get(at + 1)?;
    let scalar = |digits: &[char]| -> Option<u32> {
        if digits.is_empty() || !digits.iter().all(|c| c.is_ascii_hexdigit()) {
            return None;
        }
        let cp = u32::from_str_radix(&digits.iter().collect::<String>(), 16).ok()?;
        char::from_u32(cp).map(|_| cp)
    };
    match m {
        'x' | 'u' | 'U' if chars.get(at + 2) == Some(&'{') => {
            let e = (at + 3..chars.len()).find(|k| chars[*k] == '}')?;
            let digits = &chars[at + 3..e];
            if m == 'u' && 6 < digits.len() {
                return None;
            }
            scalar(digits).map(|cp| (e + 1 - at, cp))
        }
        'x' | 'u' | 'U' => {
            let n = match m {
                'x' => 2,
                'u' => 4,
                _ => 8,
            };
            scalar(chars.get(at + 2..at + 2 + n)?).map(|cp| (2 + n, cp))
        }
        'a' => Some((2, 0x07)),
        c if c.is_ascii() && !c.is_ascii_alphanumeric() && c != '<' && c != '>' => {
            Some((2, c as u32))
        }
        _ => None,
    }
}

pub fn pattern_char_ranges(pattern: &str, flags: &str) -> Option<Vec<CharRange>> {
    if pattern == r"[\s\S]" {
        return Some(vec![(0, MAX_CODE_POINT)]);
    }
    let chars: Vec<char> = pattern.chars().collect();
    let mut i = 0usize;

    // One code point at `i`, advancing, or `None` for a construct whose
    // coverage cannot be known.
    fn one(chars: &[char], i: &mut usize) -> Option<u32> {
        let c = *chars.get(*i)?;
        if c != '\\' {
            *i += 1;
            return Some(c as u32);
        }
        let (n, cp) = read_escape(chars, *i)?;
        *i += n;
        Some(cp)
    }

    if chars.first() != Some(&'[') {
        let cp = one(&chars, &mut i)?;
        return Some(vec![(cp, cp)]);
    }

    i = 1;
    let mut neg = false;
    if chars.get(i) == Some(&'^') {
        neg = true;
        i += 1;
    }
    let mut ranges: Vec<CharRange> = Vec::new();
    // Without `u` or `v` the canonical matcher reads a class in UTF-16
    // code units, so an astral character written in one is two members,
    // its lead and trail surrogates: `[😀]` takes the first half of
    // U+1F601 as well. The crate reads the code point, which is kept, and
    // the two units are added beside it, so the contest checks reach from
    // the lead as the canonical compiler's do (`code_unit_reach`) and the
    // three ports emit the same grammar. Mirrors TS `patternCharRanges`
    // (tabnas/bnf#75 review).
    let units = !flags.contains(['u', 'v']);
    let split = |raw: bool, cp: u32, ranges: &mut Vec<CharRange>| {
        if raw && units && cp > 0xFFFF {
            let lead = 0xD800 + ((cp - 0x10000) >> 10);
            let trail = 0xDC00 + ((cp - 0x10000) & 0x3FF);
            ranges.push((lead, lead));
            ranges.push((trail, trail));
        }
    };
    while i < chars.len() && chars[i] != ']' {
        let raw = chars[i] != '\\';
        let lo = one(&chars, &mut i)?;
        split(raw, lo, &mut ranges);
        if chars.get(i) == Some(&'-') && chars.get(i + 1).is_some_and(|c| *c != ']') {
            i += 1;
            let raw = chars.get(i) != Some(&'\\');
            let hi = one(&chars, &mut i)?;
            split(raw, hi, &mut ranges);
            ranges.push((lo, hi));
        } else {
            ranges.push((lo, lo));
        }
    }
    if chars.get(i) != Some(&']') {
        return None;
    }
    if !neg {
        return Some(ranges);
    }

    // Complement over the code-point space.
    ranges.sort_by_key(|r| r.0);
    let mut out = Vec::new();
    let mut next = 0u32;
    for (lo, hi) in ranges {
        if next < lo {
            out.push((next, lo - 1));
        }
        if hi + 1 > next {
            next = hi + 1;
        }
    }
    if next <= MAX_CODE_POINT {
        out.push((next, MAX_CODE_POINT));
    }
    Some(out)
}

/// A matcher's first-character coverage in the code points the contest
/// checks compare. Under `u` or `v` the canonical matcher reads code
/// points, and a lead surrogate it names is one standing alone. Without
/// either it reads UTF-16 code units and takes a lead surrogate wherever
/// it stands, the first half of an astral character included, so it can
/// start on every astral character that surrogate begins. The `regex`
/// crate never meets a surrogate, but this port widens the same coverage,
/// so the three ports emit the same grammar. A literal is matched in code
/// units there too. Mirrors TS `codeUnitReach` (tabnas/bnf#75 review).
pub fn code_unit_reach(r: &[CharRange], flags: &str) -> Vec<CharRange> {
    let mut out = r.to_vec();
    if flags.contains(['u', 'v']) {
        return out;
    }
    for &(lo, hi) in r {
        let (a, b) = (lo.max(0xD800), hi.min(0xDBFF));
        if a <= b {
            out.push((
                0x10000 + ((a - 0xD800) << 10),
                0x10000 + ((b - 0xD800) << 10) + 0x3FF,
            ));
        }
    }
    out
}

/// The part of sorted, merged ranges that names a lead surrogate, U+D800
/// to U+DBFF, still sorted and merged.
fn lead_surrogates(r: &[CharRange]) -> Vec<CharRange> {
    r.iter()
        .filter(|&&(lo, hi)| lo <= 0xDBFF && 0xD800 <= hi)
        .map(|&(lo, hi)| (lo.max(0xD800), hi.min(0xDBFF)))
        .collect()
}

/// Widen character ranges to cover both cases of every ASCII letter in
/// them, for matchers carrying the `i` flag. ASCII only: both sides of
/// the contest this feeds are ASCII in every notation this compiler
/// targets, and a non-ASCII letter keeping its own case is the
/// conservative answer.
pub fn fold_case_ranges(ranges: &[CharRange]) -> Vec<CharRange> {
    let mut out = ranges.to_vec();
    const A: u32 = 0x41;
    const Z: u32 = 0x5a;
    const LA: u32 = 0x61;
    const LZ: u32 = 0x7a;
    const DELTA: u32 = LA - A;
    for &(lo, hi) in ranges {
        let (u_lo, u_hi) = (lo.max(A), hi.min(Z));
        if u_lo <= u_hi {
            out.push((u_lo + DELTA, u_hi + DELTA));
        }
        let (l_lo, l_hi) = (lo.max(LA), hi.min(LZ));
        if l_lo <= l_hi {
            out.push((l_lo - DELTA, l_hi - DELTA));
        }
    }
    out
}

/// Sort by low bound and merge touching or overlapping spans, so the
/// overlap test can sweep both sides once.
pub fn normalize_ranges(r: &[CharRange]) -> Vec<CharRange> {
    if r.len() < 2 {
        return r.to_vec();
    }
    let mut sorted = r.to_vec();
    sorted.sort_by(|x, y| x.0.cmp(&y.0).then(x.1.cmp(&y.1)));
    let mut out: Vec<CharRange> = vec![sorted[0]];
    for cur in sorted.into_iter().skip(1) {
        let last = out.last_mut().expect("one range is always present");
        if cur.0 <= last.1 + 1 {
            if last.1 < cur.1 {
                last.1 = cur.1;
            }
        } else {
            out.push(cur);
        }
    }
    out
}

/// Do two coverages share a character? Both sides are sorted and merged,
/// so one linear sweep decides it.
pub fn char_ranges_overlap(a: &[CharRange], b: &[CharRange]) -> bool {
    let (mut i, mut j) = (0, 0);
    while i < a.len() && j < b.len() {
        let (ai, bj) = (a[i], b[j]);
        if ai.0 <= bj.1 && bj.0 <= ai.1 {
            return true;
        }
        if ai.1 < bj.1 {
            i += 1;
        } else {
            j += 1;
        }
    }
    false
}

/// The coverage of a class that provably matches EXACTLY ONE code point,
/// or `None` when the pattern could match more (or less) than that.
///
/// Stricter than [`pattern_char_ranges`] on purpose: partitioning
/// REPLACES a class's matcher with one-character atom matchers, so a
/// pattern whose first-character coverage is only part of what it
/// matches would lose the rest. A case-insensitive class is refused
/// outright rather than folded, because the fold covers ASCII only.
pub fn single_code_point_ranges(pattern: &str, flags: &str) -> Option<Vec<CharRange>> {
    single_code_point_coverage(pattern, flags).filter(|r| !code_unit_matcher_past_bmp(r, flags))
}

/// A class the canonical matcher reads in UTF-16 code units whose
/// code-point coverage reaches past U+FFFF: a negation, `[\s\S]` or an
/// astral literal written without `u` or `v`. JavaScript's matcher takes
/// such a class one code unit at a time, and laid over the partition it
/// would be matched by atoms compiled with `u`, which take an astral
/// character whole, so whether a grammar accepted an emoji turned on
/// whether another class overlapped this one. The `regex` crate reads
/// code points whatever the flags say, but this port leaves the same
/// classes out, so the three ports emit the same grammar: the class keeps
/// its own matcher and gives up only the partition's answer for the
/// characters it shares with an overlapping class.
fn code_unit_matcher_past_bmp(r: &[CharRange], flags: &str) -> bool {
    !flags.contains(['u', 'v']) && r.iter().any(|&(_, hi)| hi > 0xFFFF)
}

fn single_code_point_coverage(pattern: &str, flags: &str) -> Option<Vec<CharRange>> {
    if flags.contains('i') {
        return None;
    }
    if pattern == r"[\s\S]" {
        return pattern_char_ranges(pattern, flags);
    }
    let chars: Vec<char> = pattern.chars().collect();
    if chars.first() == Some(&'[') {
        // The class must BE the pattern: find its closing bracket,
        // honouring backslash escapes, and require it to be the last
        // character. That rejects `[a-z]+`, `[aA][bB]` and `[a]|b` alike.
        let mut i = 1;
        if chars.get(i) == Some(&'^') {
            i += 1;
        }
        while i < chars.len() {
            if chars[i] == '\\' {
                i += 2;
                continue;
            }
            if chars[i] == ']' {
                break;
            }
            i += 1;
        }
        if i != chars.len() - 1 {
            return None;
        }
        return pattern_char_ranges(pattern, flags);
    }

    // A bare single code point, possibly escaped: `a`, `\.`, `A`,
    // `\u{1F600}`. Anything longer is a sequence, an alternation or a
    // quantified atom, none of which this may touch.
    if !is_single_code_point_pattern(&chars) {
        return None;
    }
    pattern_char_ranges(pattern, flags)
}

/// One unescaped character that is no regex syntax, or one escape that
/// `read_escape` names in full: the TypeScript port's
/// `^(?:\\u\{[0-9A-Fa-f]{1,6}\}|\\u[0-9A-Fa-f]{4}|\\x[0-9A-Fa-f]{2}|\\[^ux]|[^\\[\]()|*+?{}^$.])$`,
/// with the escapes read in this port's dialect.
fn is_single_code_point_pattern(chars: &[char]) -> bool {
    // TypeScript tests this with a regular expression over UTF-16 code
    // units, so an astral character is two units there and never a single
    // code point; a BMP check keeps the two ports agreeing.
    let bmp = |c: &char| (*c as u32) <= 0xFFFF;
    match chars {
        [c] => {
            bmp(c)
                && !matches!(
                    c,
                    '\\' | '['
                        | ']'
                        | '('
                        | ')'
                        | '|'
                        | '*'
                        | '+'
                        | '?'
                        | '{'
                        | '}'
                        | '^'
                        | '$'
                        | '.'
                )
        }
        ['\\', ..] => read_escape(chars, 0).is_some_and(|(n, _)| n == chars.len()),
        _ => false,
    }
}

/// Split a collection of character coverages into ATOMS: the coarsest
/// set of pairwise-disjoint spans such that every input coverage is an
/// exact union of them.
///
/// The lexer produces ONE token per position, picking it by running the
/// matchers the rule expects in tin order, first match wins. When two
/// class tokens both cover a character, whichever was allocated first
/// always wins and every alternative keyed on the other one is
/// unreachable. Disjoint atoms remove the choice: no character matches
/// two atoms, so there is nothing for allocation order to decide.
///
/// Endpoint sweep: every coverage boundary starts a new atom. Returns
/// sorted, disjoint spans covering exactly the union of the inputs.
pub fn partition_ranges(coverages: &[Vec<CharRange>]) -> Vec<CharRange> {
    let mut cuts: IndexSet<u32> = IndexSet::new();
    for ranges in coverages {
        for &(lo, hi) in ranges {
            cuts.insert(lo);
            cuts.insert(hi + 1);
        }
    }
    let mut points: Vec<u32> = cuts.into_iter().collect();
    points.sort_unstable();
    let mut out = Vec::new();
    for w in points.windows(2) {
        let (lo, hi) = (w[0], w[1] - 1);
        if hi < lo {
            continue;
        }
        // Keep only spans some coverage actually contains: the gaps
        // between classes are not atoms.
        if coverages
            .iter()
            .any(|rs| rs.iter().any(|&(a, b)| a <= lo && hi <= b))
        {
            out.push((lo, hi));
        }
    }
    out
}

/// A regex character class matching exactly one span, for the atom
/// matchers. Returns the pattern and whether it is compiled with `u`: an
/// astral span is, and so is a span naming a lead surrogate when the
/// classes it stands for read code points (`code_points`), since without
/// `u` the canonical matcher would take the first half of an astral
/// character where they take only a lead surrogate standing alone. An
/// atom compiled with `u` spells its bounds in braces, so its name says
/// which reading it has. Mirrors TS `classPattern` (tabnas/bnf#75
/// review).
pub fn class_pattern(lo: u32, hi: u32, code_points: bool) -> (String, bool) {
    let unicode = hi > 0xffff || (code_points && lo <= 0xDBFF && 0xD800 <= hi);
    let esc = |cp: u32| {
        if unicode {
            format!("\\u{{{:X}}}", cp)
        } else {
            format!("\\u{:04X}", cp)
        }
    };
    (format!("[{}-{}]", esc(lo), esc(hi)), unicode)
}

/// What every character class in the grammar covers, which of them
/// contest a position with another, and the shared partition the
/// contested ones are laid over.
///
/// A class whose coverage overlaps no other class keeps exactly what it
/// had before this existed: one match token, named after its pattern. A
/// class whose pattern is not a plain single-code-point coverage cannot
/// be partitioned, so it never counts as contested and is left alone.
pub struct ClassAnalysis {
    pub coverage: IndexMap<String, Vec<CharRange>>,
    pub contested: IndexSet<String>,
    pub atoms: Vec<CharRange>,
    /// Atom span -> the token minted for it, filled in as classes are
    /// allocated so an atom lands at the position of the first class
    /// that needs it.
    pub atom_tokens: IndexMap<String, String>,
}

pub fn class_analysis(terminals: &[Element]) -> ClassAnalysis {
    let mut coverage: IndexMap<String, Vec<CharRange>> = IndexMap::new();
    // The classes the canonical matcher reads in UTF-16 code units: no
    // `u` or `v`.
    let mut code_units: IndexSet<String> = IndexSet::new();
    for el in terminals {
        let Kind::Regex { pattern, flags } = &el.kind else {
            continue;
        };
        let key = regex_key(pattern, flags);
        if coverage.contains_key(&key) {
            continue;
        }
        // Only classes that provably match exactly one code point take
        // part: partitioning replaces a class's matcher with
        // one-character atoms, so anything that could match more would
        // lose the rest.
        if let Some(r) = single_code_point_ranges(pattern, flags) {
            if !flags.contains(['u', 'v']) {
                code_units.insert(key.clone());
            }
            coverage.insert(key, normalize_ranges(&r));
        }
    }

    // A lead surrogate means one thing to a class read in code points, a
    // lead surrogate standing alone, and another to a class read in code
    // units, which also takes it as the first half of every astral
    // character it begins. No one atom can be both, so a class read in
    // code units that names a lead surrogate some class read in code
    // points names as well is left out, and keeps its own matcher. The
    // `regex` crate reads code points whatever the flags say, but this
    // port leaves the same classes out, so the three ports emit the same
    // grammar. Mirrors TS `classAnalysis` (tabnas/bnf#75 review).
    let point_leads = normalize_ranges(
        &coverage
            .iter()
            .filter(|(key, _)| !code_units.contains(*key))
            .flat_map(|(_, r)| lead_surrogates(r))
            .collect::<Vec<_>>(),
    );
    coverage.retain(|key, r| {
        !(code_units.contains(key) && char_ranges_overlap(&lead_surrogates(r), &point_leads))
    });

    let mut contested = IndexSet::new();
    for (key, mine) in &coverage {
        for (other, theirs) in &coverage {
            if other != key && char_ranges_overlap(mine, theirs) {
                contested.insert(key.clone());
                break;
            }
        }
    }

    let atoms = if contested.is_empty() {
        Vec::new()
    } else {
        let coverages: Vec<Vec<CharRange>> =
            contested.iter().map(|k| coverage[k].clone()).collect();
        partition_ranges(&coverages)
    };

    ClassAnalysis {
        coverage,
        contested,
        atoms,
        atom_tokens: IndexMap::new(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    // partition_ranges is the core of the overlapping-class fix, and the
    // one piece of it with a property worth asserting directly: the spans
    // it returns must be pairwise disjoint, and every input coverage must
    // be an exact union of them. Mirrors go/partition_test.go.
    #[test]
    fn partition_ranges_is_disjoint_and_exact() {
        type Case = (&'static str, Vec<Vec<CharRange>>, Vec<CharRange>);
        let cases: Vec<Case> = vec![
            // The reported case: %x30-39 against %x31-39.
            (
                "containment",
                vec![
                    vec![(b'0' as u32, b'9' as u32)],
                    vec![(b'1' as u32, b'9' as u32)],
                ],
                vec![(b'0' as u32, b'0' as u32), (b'1' as u32, b'9' as u32)],
            ),
            // Neither contains the other, the case a containment-only fix
            // would miss.
            (
                "partial overlap",
                vec![
                    vec![(b'0' as u32, b'5' as u32)],
                    vec![(b'3' as u32, b'9' as u32)],
                ],
                vec![
                    (b'0' as u32, b'2' as u32),
                    (b'3' as u32, b'5' as u32),
                    (b'6' as u32, b'9' as u32),
                ],
            ),
            // Gaps between coverages are not atoms.
            (
                "disjoint inputs stay whole",
                vec![
                    vec![(b'a' as u32, b'c' as u32)],
                    vec![(b'x' as u32, b'z' as u32)],
                ],
                vec![(b'a' as u32, b'c' as u32), (b'x' as u32, b'z' as u32)],
            ),
            (
                "multi-span coverage",
                vec![
                    vec![(b'A' as u32, b'Z' as u32), (b'a' as u32, b'z' as u32)],
                    vec![(b'A' as u32, b'F' as u32)],
                ],
                vec![
                    (b'A' as u32, b'F' as u32),
                    (b'G' as u32, b'Z' as u32),
                    (b'a' as u32, b'z' as u32),
                ],
            ),
            (
                "identical coverages collapse",
                vec![
                    vec![(b'0' as u32, b'9' as u32)],
                    vec![(b'0' as u32, b'9' as u32)],
                ],
                vec![(b'0' as u32, b'9' as u32)],
            ),
            ("no input", vec![], vec![]),
        ];
        for (name, input, want) in cases {
            let got = partition_ranges(&input);
            assert_eq!(got, want, "{name}");
            // Disjoint, and in order.
            for w in got.windows(2) {
                assert!(w[0].1 < w[1].0, "{name}: atoms overlap: {got:?}");
            }
            // Every input coverage is an exact union of atoms: each of its
            // characters is in exactly one atom, and no atom straddles its
            // boundary.
            for coverage in &input {
                for &(lo, hi) in coverage {
                    for cp in lo..=hi {
                        let mut n = 0;
                        for a in &got {
                            if a.0 <= cp && cp <= a.1 {
                                n += 1;
                                assert!(
                                    lo <= a.0 && a.1 <= hi,
                                    "{name}: atom {a:?} straddles coverage {:?}",
                                    (lo, hi)
                                );
                            }
                        }
                        assert_eq!(n, 1, "{name}: {cp} is in {n} atoms, want 1");
                    }
                }
            }
        }
    }

    // The escape the canonical compiler uses for an atom's span, so an
    // atom's token name reads like every other class token.
    #[test]
    fn class_pattern_spells_the_span_as_the_canonical_compiler_does() {
        assert_eq!(
            class_pattern(b'0' as u32, b'9' as u32, false),
            ("[\\u0030-\\u0039]".to_string(), false)
        );
        assert_eq!(
            class_pattern(0x1F600, 0x1F64F, false),
            (r"[\u{1F600}-\u{1F64F}]".to_string(), true)
        );
        // A span naming a lead surrogate is compiled in the reading of the
        // classes it stands for; a trail surrogate reads alike either way.
        assert_eq!(
            class_pattern(0xD000, 0xD800, true),
            (r"[\u{D000}-\u{D800}]".to_string(), true)
        );
        assert_eq!(
            class_pattern(0xD000, 0xD800, false),
            ("[\\uD000-\\uD800]".to_string(), false)
        );
        assert_eq!(
            class_pattern(0xDC00, 0xDFFF, true),
            ("[\\uDC00-\\uDFFF]".to_string(), false)
        );
    }

    #[test]
    fn an_escape_is_one_code_point_only_when_it_can_be_read() {
        // The head scanner and the coverage reader take an escape the same
        // way. Mirrors TestContestEscapeWhoseCodePointCannotBeReadIsNotExact
        // in go/contest_test.go.
        for p in [
            r"\u1",
            r"\x1",
            r"\u12",
            r"\uD83D",
            r"\u{}",
            r"\u{4g}",
            r"\u{110000}",
            r"\cA",
            r"\p{L}",
            r"\PL",
            r"\k<a>",
            r"\1",
            r"\0",
            r"\d",
        ] {
            assert_eq!(regex_head_atom_end(p), None, "{p}");
            assert_eq!(pattern_char_ranges(p, ""), None, "{p}");
        }
        for p in [r"[\p{L}]", r"[a\1]", r"[\u1]"] {
            assert_eq!(pattern_char_ranges(p, ""), None, "{p}");
        }
        for (p, end, cp) in [
            (r"\u0041", 6, 0x41),
            (r"\x41", 4, 0x41),
            (r"\u{1F600}", 9, 0x1F600),
            (r"\.", 2, 0x2E),
        ] {
            assert_eq!(regex_head_atom_end(p), Some(end), "{p}");
            assert_eq!(pattern_char_ranges(p, ""), Some(vec![(cp, cp)]), "{p}");
        }
    }

    #[test]
    fn an_escape_is_read_as_the_regex_crate_reads_it() {
        // The dialect the matchers compile in, not JavaScript's. Mirrors
        // TestContestEscapeIsReadAsRE2ReadsIt in go/contest_test.go.
        for (p, end, cp) in [
            (r"\a", 2, 0x07),
            (r"\U00000041", 10, 0x41),
            (r"\U{1F600}", 9, 0x1F600),
            (r"\x{41}", 6, 0x41),
            (r"\x{0000000041}", 14, 0x41),
            (r"\-", 2, 0x2D),
            (r"\#", 2, 0x23),
        ] {
            assert_eq!(regex_head_atom_end(p), Some(end), "{p}");
            assert_eq!(pattern_char_ranges(p, ""), Some(vec![(cp, cp)]), "{p}");
        }
        assert_eq!(
            pattern_char_ranges(r"[\a-\x{0D}]", ""),
            Some(vec![(0x07, 0x0D)])
        );
        // Zero-width assertions, letters the crate does not define, and
        // hex escapes naming no scalar value.
        for p in [
            r"\A",
            r"\z",
            r"\b",
            r"\B",
            r"\<",
            r"\>",
            r"\e",
            r"\Q",
            r"\U0000D800",
            r"\U00110000",
            r"\x{D800}",
            r"\u{D800}",
            r"\U{110000}",
        ] {
            assert_eq!(regex_head_atom_end(p), None, "{p}");
            assert_eq!(pattern_char_ranges(p, ""), None, "{p}");
        }
        // The partition sees the same reading.
        assert_eq!(
            single_code_point_ranges(r"\U00000041", ""),
            Some(vec![(0x41, 0x41)])
        );
        assert_eq!(single_code_point_ranges(r"\a", ""), Some(vec![(7, 7)]));
        assert_eq!(single_code_point_ranges(r"\A", ""), None);
    }

    #[test]
    fn pattern_char_ranges_reads_the_emitter_shapes() {
        assert_eq!(pattern_char_ranges("[a-c]", ""), Some(vec![(97, 99)]));
        // In code units an astral character in a class is also its two
        // surrogates; under `u` it is the code point alone.
        assert_eq!(
            pattern_char_ranges("[\u{1F600}]", ""),
            Some(vec![(0xD83D, 0xD83D), (0xDE00, 0xDE00), (0x1F600, 0x1F600)])
        );
        assert_eq!(
            pattern_char_ranges("[\u{1F600}]", "u"),
            Some(vec![(0x1F600, 0x1F600)])
        );
        assert_eq!(pattern_char_ranges(r"A", ""), Some(vec![(65, 65)]));
        assert_eq!(
            pattern_char_ranges(r"[\x41-\x43x]", ""),
            Some(vec![(65, 67), (120, 120)])
        );
        assert_eq!(
            pattern_char_ranges(r"[\s\S]", ""),
            Some(vec![(0, MAX_CODE_POINT)])
        );
        assert_eq!(pattern_char_ranges(r"\d", ""), None);
        assert_eq!(
            pattern_char_ranges("[^a]", ""),
            Some(vec![(0, 96), (98, MAX_CODE_POINT)])
        );
    }
}
