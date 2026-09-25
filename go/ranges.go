package bnf

// ranges.go — character coverage of emitted matcher patterns.
//
// The three contested-alternative passes all ask the same question:
// can these two tokens claim the SAME input character? A tokenising
// notation never contests one, so every guard below is inert for ABNF
// and EBNF; a scannerless one (GBNF) contests constantly, and the
// answer decides which alternative may win.
//
// Ported from ts/src/compiler.ts (patternCharRanges, foldCaseRanges,
// normalizeRanges, charRangesOverlap), which is canonical.

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// charRange is an inclusive code-point span.
type charRange struct{ lo, hi rune }

const maxCodePoint = 0x10FFFF

// patternCharRanges returns the character coverage of an emitted
// matcher pattern as sorted code-point ranges, or nil when the pattern
// is not a shape this parser understands — the caller must then treat
// coverage as UNKNOWN and stay conservative, which means emitting no
// guard rather than a wrong one.
//
// It handles exactly what the emitter itself produces: a leading
// character class with \uXXXX / \u{…} / \xXX escapes and ranges,
// `[\s\S]`, negation, or a single (possibly escaped) literal character.
// Trailing content after the first class (`[aA][bB]`, boundary guards)
// is irrelevant: only the FIRST character's coverage decides whether
// two tokens can contest one input position.
// regexHeadAtomEnd is the byte offset where a pattern's first atom or
// class ends, or -1 when the pattern does not begin with one: a group, an
// alternation, a quantifier, `.`, or an escape whose coverage
// patternCharRanges declines to name.
func regexHeadAtomEnd(src string) int {
	if src == "" {
		return -1
	}
	switch c := src[0]; {
	case c == '[':
		i := 1
		if i < len(src) && src[i] == '^' {
			i++
		}
		if i < len(src) && src[i] == ']' {
			i++
		}
		for i < len(src) && src[i] != ']' {
			if src[i] == '\\' {
				i += 2
			} else {
				i++
			}
		}
		if i < len(src) {
			return i + 1
		}
		return -1
	case c == '\\':
		// Exactly the escapes patternCharRanges reads: a head this calls
		// one atom must be one whose coverage is known.
		r := []rune(src)
		n, _, ok := readEscape(r, 0)
		if !ok {
			return -1
		}
		return len(string(r[:n]))
	case strings.IndexByte("(.|)?*+{", c) >= 0:
		return -1
	}
	_, size := utf8.DecodeRuneInString(src)
	return size
}

// readEscape reads the escape at r[at] as one code point: its length in
// runes and the code point, or ok false when it is not one this can name.
// A shorthand class, a control or property escape, a group or back
// reference, and a digit escape all bail rather than guess, and so does a
// hex escape without its full digits (`\u1` is `u` then `1` in the
// JavaScript matcher the canonical runtime emits for, not U+0001) or one
// naming half of a surrogate pair. Unknown coverage keeps every caller
// conservative. Mirrors the TS readEscape, plus RE2's `\x{…}`: the TS side
// never writes that form (JavaScript spells it `\u{…}`), but the Go
// emitter does, for every character class it builds.
func readEscape(r []rune, at int) (int, rune, bool) {
	if at+1 >= len(r) {
		return 0, 0, false
	}
	m := r[at+1]
	if (m == 'u' || m == 'x') && at+2 < len(r) && r[at+2] == '{' {
		e := -1
		for k := at + 3; k < len(r); k++ {
			if r[k] == '}' {
				e = k
				break
			}
		}
		if e < 0 || !hexDigits(r[at+3:e], 1, 6) {
			return 0, 0, false
		}
		cp, _ := strconv.ParseInt(string(r[at+3:e]), 16, 32)
		if cp > maxCodePoint {
			return 0, 0, false
		}
		return e + 1 - at, rune(cp), true
	}
	if m == 'u' || m == 'x' {
		digits := 4
		if m == 'x' {
			digits = 2
		}
		if at+2+digits > len(r) || !hexDigits(r[at+2:at+2+digits], digits, digits) {
			return 0, 0, false
		}
		cp, _ := strconv.ParseInt(string(r[at+2:at+2+digits]), 16, 32)
		if 0xD800 <= cp && cp <= 0xDFFF {
			return 0, 0, false
		}
		return 2 + digits, rune(cp), true
	}
	if strings.ContainsRune("dDwWsSbBnrtfvckpP0123456789", m) {
		return 0, 0, false
	}
	return 2, m, true
}

// hexDigits reports whether r is between min and max hexadecimal digits.
func hexDigits(r []rune, min, max int) bool {
	if len(r) < min || len(r) > max {
		return false
	}
	for _, c := range r {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
			return false
		}
	}
	return true
}

func patternCharRanges(pattern string) []charRange {
	if pattern == `[\s\S]` {
		return []charRange{{0, maxCodePoint}}
	}

	r := []rune(pattern)
	i := 0

	// one reads a single code point at i, advancing i, or reports
	// failure for a construct whose coverage cannot be known.
	one := func() (rune, bool) {
		if i >= len(r) {
			return 0, false
		}
		c := r[i]
		if c != '\\' {
			i++
			return c, true
		}
		n, cp, ok := readEscape(r, i)
		if !ok {
			return 0, false
		}
		i += n
		return cp, true
	}

	if len(r) == 0 {
		return nil
	}

	if r[0] != '[' {
		cp, ok := one()
		if !ok {
			return nil
		}
		return []charRange{{cp, cp}}
	}

	i = 1
	neg := false
	if i < len(r) && r[i] == '^' {
		neg = true
		i++
	}

	out := []charRange{}
	for i < len(r) && r[i] != ']' {
		lo, ok := one()
		if !ok {
			return nil
		}
		if i < len(r) && r[i] == '-' && i+1 < len(r) && r[i+1] != ']' {
			i++
			hi, ok := one()
			if !ok {
				return nil
			}
			out = append(out, charRange{lo, hi})
			continue
		}
		out = append(out, charRange{lo, lo})
	}
	if i >= len(r) || r[i] != ']' {
		return nil
	}

	if !neg {
		return out
	}

	// Complement over the code-point space.
	sort.Slice(out, func(a, b int) bool { return out[a].lo < out[b].lo })
	comp := []charRange{}
	next := rune(0)
	for _, cr := range out {
		if next < cr.lo {
			comp = append(comp, charRange{next, cr.lo - 1})
		}
		if cr.hi+1 > next {
			next = cr.hi + 1
		}
	}
	if next <= maxCodePoint {
		comp = append(comp, charRange{next, maxCodePoint})
	}
	return comp
}

// foldCaseRanges widens ranges to cover both cases of every ASCII
// letter in them, for matchers carrying the `i` flag.
//
// ASCII only: these ranges feed contest detection between a keyword and
// a character class, and both sides of that contest are ASCII in every
// notation this compiler targets. A non-ASCII letter simply keeps its
// own case, which is the conservative answer — a missed contest emits
// no guard.
func foldCaseRanges(ranges []charRange) []charRange {
	const (
		upA   = rune(0x41)
		upZ   = rune(0x5a)
		loA   = rune(0x61)
		loZ   = rune(0x7a)
		delta = loA - upA
	)
	out := append([]charRange{}, ranges...)
	for _, cr := range ranges {
		uLo, uHi := maxRune(cr.lo, upA), minRune(cr.hi, upZ)
		if uLo <= uHi {
			out = append(out, charRange{uLo + delta, uHi + delta})
		}
		lLo, lHi := maxRune(cr.lo, loA), minRune(cr.hi, loZ)
		if lLo <= lHi {
			out = append(out, charRange{lLo - delta, lHi - delta})
		}
	}
	return out
}

// normalizeRanges sorts by low bound and merges touching/overlapping
// spans, so charRangesOverlap can sweep both sides once instead of
// comparing every pair. Callers cache the result per token, so each
// token pays for this at most once.
func normalizeRanges(r []charRange) []charRange {
	if len(r) < 2 {
		return r
	}
	sorted := append([]charRange{}, r...)
	sort.Slice(sorted, func(a, b int) bool {
		if sorted[a].lo != sorted[b].lo {
			return sorted[a].lo < sorted[b].lo
		}
		return sorted[a].hi < sorted[b].hi
	})
	out := []charRange{sorted[0]}
	for _, cur := range sorted[1:] {
		last := &out[len(out)-1]
		if cur.lo <= last.hi+1 {
			if last.hi < cur.hi {
				last.hi = cur.hi
			}
			continue
		}
		out = append(out, cur)
	}
	return out
}

// charRangesOverlap reports whether two coverages share a character.
// Both sides arrive sorted and merged, so one linear sweep decides it.
// This runs inside the quadratic contest loops, where a pairwise scan
// is what made a 332-rule grammar take a minute in the TS port.
func charRangesOverlap(a, b []charRange) bool {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i].lo <= b[j].hi && b[j].lo <= a[i].hi {
			return true
		}
		if a[i].hi < b[j].hi {
			i++
		} else {
			j++
		}
	}
	return false
}

func minRune(a, b rune) rune {
	if a < b {
		return a
	}
	return b
}

func maxRune(a, b rune) rune {
	if a > b {
		return a
	}
	return b
}

// singleCodePointRanges returns the coverage of a class that provably
// matches EXACTLY ONE code point, or nil when the pattern could match
// more (or less) than that.
//
// Stricter than patternCharRanges on purpose, and the two must not be
// confused. That one answers "what can this pattern's FIRST character
// be?" — the right question for contest detection, which is what it was
// written for, and it deliberately ignores everything after the first
// class. Partitioning asks a different question: it REPLACES a class's
// matcher with one-character atom matchers, so a pattern whose first
// character coverage is only part of what it matches loses the rest.
//
// Measured, on emitGrammarSpec directly:
//
//	`a|bc` beside `[a]`      → the `a|bc` matcher vanished; `bc` rejected
//	`[a-z]+` beside `[a-c]`  → the `+` lost; matched one char, not a run
//	`[aA][bB]` beside `[a]`  → the `[bB]` lost
//
// A case-insensitive class is refused outright rather than folded:
// foldCaseRanges folds ASCII A-Z/a-z and nothing else, so the atoms it
// would produce for `[é]/i` cover `é` but not `É` — the matcher says one
// thing and the ranges another. Refusing costs nothing real (`%x` ranges
// are case-sensitive by construction, and a case-insensitive literal is
// a term, not a regex), and a class left out of the partition simply
// keeps the single token it has always had.
func singleCodePointRanges(pattern, flags string) []charRange {
	if strings.Contains(flags, "i") {
		return nil
	}
	if pattern == `[\s\S]` {
		return patternCharRanges(pattern)
	}

	if strings.HasPrefix(pattern, "[") {
		// The class must BE the pattern: find its closing bracket,
		// honouring backslash escapes, and require it to be the last
		// character. That rejects `[a-z]+`, `[aA][bB]` and `[a]|b` alike.
		i := 1
		if i < len(pattern) && pattern[i] == '^' {
			i++
		}
		for ; i < len(pattern); i++ {
			if pattern[i] == '\\' {
				i++
				continue
			}
			if pattern[i] == ']' {
				break
			}
		}
		if i != len(pattern)-1 {
			return nil
		}
		return patternCharRanges(pattern)
	}

	// A bare single code point, possibly escaped: `a`, `\.`, `\x{41}`,
	// `\u0041`. Anything longer is a sequence, an alternation or a
	// quantified atom, none of which this may touch.
	if !singleCodePointRe.MatchString(pattern) {
		return nil
	}
	return patternCharRanges(pattern)
}

var singleCodePointRe = regexp.MustCompile(
	`^(?:\\x\{[0-9A-Fa-f]{1,6}\}|\\u[0-9A-Fa-f]{4}|\\x[0-9A-Fa-f]{2}|\\[^ux]|[^\\\[\]()|*+?{}^$.])$`)

// partitionRanges splits a collection of character coverages into
// ATOMS: the coarsest set of pairwise-disjoint spans such that every
// input coverage is an exact union of them.
//
// This is what makes overlapping character classes work at all. The
// lexer produces ONE token per position, and it picks it by running the
// matchers the rule expects in allocation order, first match wins. So
// when two class tokens both cover a character, whichever was allocated
// first always wins and every alternative keyed on the other one is
// unreachable — `dec-octet = DIGIT / %x31-39 DIGIT` accepted `0` and `9`
// and rejected every two-digit octet, because `%x31-39` never fired.
// Which alternative dies depends only on the order the classes happen to
// be allocated in, which in turn depends on the order the productions
// are visited: RFC 3986 worked by luck, and swapping its two dec-octet
// alternatives broke it.
//
// Disjoint atoms remove the choice. No character matches two atoms, so
// there is nothing for allocation order to decide, and a class that
// spans several atoms is expressed as a token SET over them — which the
// engine already resolves to a tin list when it norms an alternate.
//
// Endpoint sweep: every coverage boundary starts a new atom. Returns
// sorted, disjoint spans covering exactly the union of the inputs.
func partitionRanges(coverages [][]charRange) []charRange {
	// Cut points: the low bound of every span, and one past every high
	// bound. Between consecutive cut points, membership cannot change.
	seen := map[rune]bool{}
	var points []rune
	add := func(p rune) {
		if !seen[p] {
			seen[p] = true
			points = append(points, p)
		}
	}
	for _, ranges := range coverages {
		for _, r := range ranges {
			add(r.lo)
			add(r.hi + 1)
		}
	}
	sort.Slice(points, func(i, j int) bool { return points[i] < points[j] })

	var out []charRange
	for i := 0; i+1 < len(points); i++ {
		lo, hi := points[i], points[i+1]-1
		if hi < lo {
			continue
		}
		// Keep only spans some coverage actually contains — the gaps
		// between classes are not atoms.
		for _, ranges := range coverages {
			covered := false
			for _, r := range ranges {
				if r.lo <= lo && hi <= r.hi {
					covered = true
					break
				}
			}
			if covered {
				out = append(out, charRange{lo, hi})
				break
			}
		}
	}
	return out
}

// classPattern is a regex character class matching exactly one span.
// Used for the atom matchers, whose spans come from classes the grammar
// already wrote, so the only escaping that matters is making the bounds
// unambiguous. Go's regexp is always Unicode-aware, so there is no
// astral special case as there is in the TypeScript port.
func classPattern(lo, hi rune) string {
	// `\x{%04x}`, matching the escape the ABNF front-end already emits
	// for a `%x` range on this side, so an atom's token name reads like
	// every other class token in a Go-emitted spec. (The TypeScript port
	// spells the same span `\u0030`; the two runtimes have always named
	// class tokens differently, because their regex dialects differ.)
	esc := func(cp rune) string {
		return fmt.Sprintf(`\x{%04x}`, cp)
	}
	return "[" + esc(lo) + "-" + esc(hi) + "]"
}
