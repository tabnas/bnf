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
	"sort"
	"strconv"
	"strings"
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
		if i+1 >= len(r) {
			return 0, false
		}
		m := r[i+1]
		switch {
		case m == 'u' && i+2 < len(r) && r[i+2] == '{':
			e := -1
			for k := i + 3; k < len(r); k++ {
				if r[k] == '}' {
					e = k
					break
				}
			}
			if e < 0 {
				return 0, false
			}
			cp, err := strconv.ParseInt(string(r[i+3:e]), 16, 32)
			if err != nil {
				return 0, false
			}
			i = e + 1
			return rune(cp), true
		case m == 'u':
			if i+6 > len(r) {
				return 0, false
			}
			cp, err := strconv.ParseInt(string(r[i+2:i+6]), 16, 32)
			if err != nil {
				return 0, false
			}
			i += 6
			return rune(cp), true
		case m == 'x' && i+2 < len(r) && r[i+2] == '{':
			// RE2's brace form. The TS side never writes this — JS spells
			// the same thing `\u{…}` — but the GO emitter does, for every
			// character class it builds. Reading only `\xHH` here made
			// every Go-emitted class's coverage UNKNOWN, which silently
			// switched off every contest check downstream: no guards, and
			// a valid sentence rejected.
			e := -1
			for k := i + 3; k < len(r); k++ {
				if r[k] == '}' {
					e = k
					break
				}
			}
			if e < 0 {
				return 0, false
			}
			cp, err := strconv.ParseInt(string(r[i+3:e]), 16, 32)
			if err != nil {
				return 0, false
			}
			i = e + 1
			return rune(cp), true
		case m == 'x':
			if i+4 > len(r) {
				return 0, false
			}
			cp, err := strconv.ParseInt(string(r[i+2:i+4]), 16, 32)
			if err != nil {
				return 0, false
			}
			i += 4
			return rune(cp), true
		case strings.ContainsRune(`dDwWsSbB0nrtfv`, m):
			// Shorthand classes and control escapes: bail rather than
			// guess — unknown coverage keeps the caller conservative.
			return 0, false
		}
		i += 2
		return m, true
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
