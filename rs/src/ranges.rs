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
pub fn pattern_char_ranges(pattern: &str) -> Option<Vec<CharRange>> {
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
        let m = *chars.get(*i + 1)?;
        if m == 'u' {
            if chars.get(*i + 2) == Some(&'{') {
                let e = (*i + 3..chars.len()).find(|k| chars[*k] == '}')?;
                let hex: String = chars[*i + 3..e].iter().collect();
                let cp = parse_hex(&hex)?;
                *i = e + 1;
                return Some(cp);
            }
            let hex: String = chars.iter().skip(*i + 2).take(4).collect();
            let cp = parse_hex(&hex)?;
            *i += 6;
            return Some(cp);
        }
        if m == 'x' {
            let hex: String = chars.iter().skip(*i + 2).take(2).collect();
            let cp = parse_hex(&hex)?;
            *i += 4;
            return Some(cp);
        }
        if "dDwWsSbB0nrtfv".contains(m) {
            // Shorthand classes and control escapes: bail rather than
            // guess. Unknown coverage keeps the caller conservative.
            return None;
        }
        *i += 2;
        Some(m as u32)
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
    while i < chars.len() && chars[i] != ']' {
        let lo = one(&chars, &mut i)?;
        if chars.get(i) == Some(&'-') && chars.get(i + 1).is_some_and(|c| *c != ']') {
            i += 1;
            let hi = one(&chars, &mut i)?;
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

/// JavaScript's `parseInt(s, 16)`: a leading hexadecimal run, or NaN.
fn parse_hex(s: &str) -> Option<u32> {
    let digits: String = s.chars().take_while(|c| c.is_ascii_hexdigit()).collect();
    if digits.is_empty() {
        return None;
    }
    u32::from_str_radix(&digits, 16).ok()
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
    if flags.contains('i') {
        return None;
    }
    if pattern == r"[\s\S]" {
        return pattern_char_ranges(pattern);
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
        return pattern_char_ranges(pattern);
    }

    // A bare single code point, possibly escaped: `a`, `\.`, `A`,
    // `\u{1F600}`. Anything longer is a sequence, an alternation or a
    // quantified atom, none of which this may touch.
    if !is_single_code_point_pattern(&chars) {
        return None;
    }
    pattern_char_ranges(pattern)
}

/// `^(?:\\u\{[0-9A-Fa-f]{1,6}\}|\\u[0-9A-Fa-f]{4}|\\x[0-9A-Fa-f]{2}|\\[^ux]|[^\\[\]()|*+?{}^$.])$`
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
        ['\\', m] => bmp(m) && !matches!(m, 'u' | 'x'),
        ['\\', 'x', a, b] => a.is_ascii_hexdigit() && b.is_ascii_hexdigit(),
        ['\\', 'u', a, b, c, d] => [a, b, c, d].iter().all(|h| h.is_ascii_hexdigit()),
        ['\\', 'u', '{', rest @ .., '}'] => {
            !rest.is_empty() && rest.len() <= 6 && rest.iter().all(|h| h.is_ascii_hexdigit())
        }
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
/// matchers. Returns the pattern and whether it needs the `u` flag
/// (astral bounds).
pub fn class_pattern(lo: u32, hi: u32) -> (String, bool) {
    let astral = lo > 0xffff || hi > 0xffff;
    let esc = |cp: u32| {
        if astral {
            format!("\\u{{{:X}}}", cp)
        } else {
            format!("\\u{:04X}", cp)
        }
    };
    (format!("[{}-{}]", esc(lo), esc(hi)), astral)
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
            coverage.insert(key, normalize_ranges(&r));
        }
    }

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
            class_pattern(b'0' as u32, b'9' as u32),
            ("[\\u0030-\\u0039]".to_string(), false)
        );
        assert_eq!(
            class_pattern(0x1F600, 0x1F64F),
            (r"[\u{1F600}-\u{1F64F}]".to_string(), true)
        );
    }

    #[test]
    fn pattern_char_ranges_reads_the_emitter_shapes() {
        assert_eq!(pattern_char_ranges("[a-c]"), Some(vec![(97, 99)]));
        assert_eq!(pattern_char_ranges(r"A"), Some(vec![(65, 65)]));
        assert_eq!(
            pattern_char_ranges(r"[\x41-\x43x]"),
            Some(vec![(65, 67), (120, 120)])
        );
        assert_eq!(
            pattern_char_ranges(r"[\s\S]"),
            Some(vec![(0, MAX_CODE_POINT)])
        );
        assert_eq!(pattern_char_ranges(r"\d"), None);
        assert_eq!(
            pattern_char_ranges("[^a]"),
            Some(vec![(0, 96), (98, MAX_CODE_POINT)])
        );
    }
}
