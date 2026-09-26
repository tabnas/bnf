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
// character class with \x{…} / \xXX escapes and ranges, `[\s\S]`,
// negation, or a single (possibly escaped) literal character, each escape
// read as RE2 reads it (readEscape).
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

// readEscape reads the escape at r[at] as one code point by RE2's rules,
// the dialect of Go's regexp, which compiles every matcher this port
// emits: its length in runes and the code point, or ok false when it is
// not one this can name.
//
// A hex escape is the code point it spells, in RE2's two forms: `\xHH`
// with both digits, and `\x{…}`, which the Go emitter writes for every
// character class it builds. `\a` is BEL, U+0007, and escaped ASCII
// punctuation is the character itself. Everything else bails rather than
// guess: a shorthand or property class, an octal or digit escape, the
// zero-width `\A`, `\z`, `\b` and `\B`, the quoting `\Q…\E`, and every
// letter RE2 does not define, JavaScript's `\u` and `\c` among them (Go's
// regexp refuses those, so such a matcher never compiles). `\f`, `\n`,
// `\r`, `\t` and `\v` bail as well although RE2 names them, because the
// canonical TypeScript reader declines them, and where the two dialects
// agree the two runtimes should decide alike. Unknown coverage keeps the
// dispatcher conservative.
func readEscape(r []rune, at int) (int, rune, bool) {
	if at+1 >= len(r) {
		return 0, 0, false
	}
	switch m := r[at+1]; {
	case m == 'x' && at+2 < len(r) && r[at+2] == '{':
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
	case m == 'x':
		if at+4 > len(r) || !hexDigits(r[at+2:at+4], 2, 2) {
			return 0, 0, false
		}
		cp, _ := strconv.ParseInt(string(r[at+2:at+4]), 16, 32)
		return 4, rune(cp), true
	case m == 'a':
		return 2, 0x07, true
	case m < utf8.RuneSelf && !('0' <= m && m <= '9' || 'a' <= m && m <= 'z' || 'A' <= m && m <= 'Z'):
		return 2, m, true
	}
	return 0, 0, false
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

func patternCharRanges(pattern, flags string) []charRange {
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
	// Without `u` or `v` the canonical matcher reads a class in UTF-16 code
	// units, so an astral character written in one is two members, its
	// lead and trail surrogates: `[😀]` takes the first half of U+1F601 as
	// well. RE2 reads the code point, which is kept, and the two units are
	// added beside it, so the contest checks reach from the lead as the
	// canonical compiler's do (codeUnitReach) and the three ports emit the
	// same grammar. Mirrors TS patternCharRanges (tabnas/bnf#75 review).
	units := !strings.ContainsAny(flags, "uv")
	split := func(raw bool, cp rune) {
		if raw && units && cp > 0xFFFF {
			lead := 0xD800 + (cp-0x10000)>>10
			trail := 0xDC00 + (cp-0x10000)&0x3FF
			out = append(out, charRange{lead, lead}, charRange{trail, trail})
		}
	}
	for i < len(r) && r[i] != ']' {
		raw := r[i] != '\\'
		lo, ok := one()
		if !ok {
			return nil
		}
		split(raw, lo)
		if i < len(r) && r[i] == '-' && i+1 < len(r) && r[i+1] != ']' {
			i++
			raw := r[i] != '\\'
			hi, ok := one()
			if !ok {
				return nil
			}
			split(raw, hi)
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

// codeUnitReach is a matcher's first-character coverage in the code
// points the contest checks compare. Under `u` or `v` the canonical
// matcher reads code points, and a lead surrogate it names is one
// standing alone. Without either it reads UTF-16 code units and takes a
// lead surrogate wherever it stands, the first half of an astral
// character included, so it can start on every astral character that
// surrogate begins. RE2 never meets a surrogate in UTF-8 text, but this
// port widens the same coverage, so the three ports emit the same
// grammar. A literal is matched in code units there too. Mirrors TS
// codeUnitReach (tabnas/bnf#75 review).
func codeUnitReach(r []charRange, flags string) []charRange {
	if strings.ContainsAny(flags, "uv") {
		return r
	}
	out := append([]charRange{}, r...)
	for _, span := range r {
		a, b := max(span.lo, 0xD800), min(span.hi, 0xDBFF)
		if a <= b {
			out = append(out, charRange{0x10000 + (a-0xD800)<<10, 0x10000 + (b-0xD800)<<10 + 0x3FF})
		}
	}
	return out
}

// leadSurrogates is the part of sorted, merged ranges that names a lead
// surrogate, U+D800 to U+DBFF, still sorted and merged.
func leadSurrogates(r []charRange) []charRange {
	var out []charRange
	for _, span := range r {
		if span.lo <= 0xDBFF && 0xD800 <= span.hi {
			out = append(out, charRange{max(span.lo, 0xD800), min(span.hi, 0xDBFF)})
		}
	}
	return out
}

// atomReadsCodePoints reports whether the canonical compiler compiles
// an atom with `u`: an astral one always, and one naming a lead
// surrogate when the classes it stands for read code points. RE2 has no
// such flag, but the contest checks read coverage by it (codeUnitReach).
// Mirrors TS classPattern.
func atomReadsCodePoints(lo, hi rune, codePoints bool) bool {
	return hi > 0xFFFF || codePoints && lo <= 0xDBFF && 0xD800 <= hi
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
	r := singleCodePointCoverage(pattern, flags)
	if r != nil && codeUnitMatcherPastBmp(r, flags) {
		return nil
	}
	return r
}

// codeUnitMatcherPastBmp reports a class the canonical matcher reads in
// UTF-16 code units whose code-point coverage reaches past U+FFFF: a
// negation, `[\s\S]` or an astral literal written without `u` or `v`.
// JavaScript's matcher takes such a class one code unit at a time, and
// laid over the partition it would be matched by atoms compiled with
// `u`, which take an astral character whole, so whether a grammar
// accepted an emoji turned on whether another class overlapped this one.
// RE2 reads code points whatever the flags say, but this port leaves the
// same classes out, so the three ports emit the same grammar: the class
// keeps its own matcher and gives up only the partition's answer for the
// characters it shares with an overlapping class.
func codeUnitMatcherPastBmp(r []charRange, flags string) bool {
	if strings.ContainsAny(flags, "uv") {
		return false
	}
	for _, span := range r {
		if span.hi > 0xFFFF {
			return true
		}
	}
	return false
}

func singleCodePointCoverage(pattern, flags string) []charRange {
	if strings.Contains(flags, "i") {
		return nil
	}
	if pattern == `[\s\S]` {
		return patternCharRanges(pattern, flags)
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
		return patternCharRanges(pattern, flags)
	}

	// A bare single code point, possibly escaped: `a`, `\.`, `\x41`,
	// `\x{41}`, `\a`. Anything longer is a sequence, an alternation or a
	// quantified atom, none of which this may touch.
	if !singleCodePointRe.MatchString(pattern) {
		return nil
	}
	return patternCharRanges(pattern, flags)
}

var singleCodePointRe = regexp.MustCompile(
	`^(?:\\x\{[0-9A-Fa-f]{1,6}\}|\\x[0-9A-Fa-f]{2}|\\[^ux]|[^\\\[\]()|*+?{}^$.])$`)

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
