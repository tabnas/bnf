package bnf

// contested.go — deciding alternatives a SCANNERLESS grammar contests
// at the character level.
//
// The engine lexes under the direction of the active rule, and a
// character class and a literal can both claim the same character. Two
// passes here write the decision down where the grammar cannot:
//
//   - pairExitGuards: FOLLOW₂ exit guards on a contested repetition.
//   - reorderKeywordShadow (+ synthKeywordGuards): a literal-keyword
//     alternative contested by a character-class alternative.
//
// Both lean on the engine's negotiated lexing (Options.Lex.Relex,
// parser 0.8.5). Without it the class matcher wins the first cut and
// the guards never match, which is exactly what keeps them INERT for
// tokenising notations — every ABNF and EBNF grammar in the shared
// fixtures behaves identically with them present.
//
// Ported from ts/src/compiler.ts, which is canonical.

import (
	"math"
	"regexp"
	"sort"
	"strings"
)

// contestCtx answers "can these two tokens claim the same character?"
// with memoisation on both the token and the PAIR. The contest checks
// below are quadratic in dispatch entries while the distinct token
// pairs behind them are few — a grammar with hundreds of entries per
// rule asks the same handful of questions over and over, and without
// the caches a 332-rule grammar took a minute in the TS port.
type contestCtx struct {
	fixedTokens map[string]*string        // token name -> literal
	matchTokens map[string]*regexp.Regexp // token name -> matcher

	rangeCache   map[string][]charRange
	rangeKnown   map[string]bool
	overlapCache map[string]bool
}

func newContestCtx(fixedTokens map[string]*string,
	matchTokens map[string]*regexp.Regexp) *contestCtx {
	return &contestCtx{
		fixedTokens:  fixedTokens,
		matchTokens:  matchTokens,
		rangeCache:   map[string][]charRange{},
		rangeKnown:   map[string]bool{},
		overlapCache: map[string]bool{},
	}
}

// tokenRangesOf is the character coverage of a token, or nil when it
// cannot be established — the caller then treats the pair as NOT
// contested, which emits no guard.
//
// A fixed token covers its literal's first code point; a match token
// covers whatever its leading character class covers.
func (c *contestCtx) tokenRangesOf(tok string) []charRange {
	if known, seen := c.rangeKnown[tok]; seen {
		if !known {
			return nil
		}
		return c.rangeCache[tok]
	}

	var r []charRange
	if lit, ok := c.fixedTokens[tok]; ok && lit != nil && *lit != "" {
		cp := []rune(*lit)[0]
		r = []charRange{{cp, cp}}
	} else if re, ok := c.matchTokens[tok]; ok && re != nil {
		src := re.String()
		// Strip the emitter's own inline case flag and `^` anchor (and
		// grouping) so the parser sees the pattern as written.
		fold := false
		if strings.HasPrefix(src, "(?i)") {
			fold = true
			src = src[4:]
		}
		src = strings.TrimPrefix(src, "^")
		if strings.HasPrefix(src, "(?:") && strings.HasSuffix(src, ")") {
			src = src[3 : len(src)-1]
		}
		r = patternCharRanges(src)
		// A case-insensitive matcher covers both cases of every letter
		// it names, and the pattern text only spells one of them. ABNF
		// literals are case-insensitive by default, so without this an
		// unquoted `"GET"` reads as covering `G` alone — no contest is
		// detected against a lowercase identifier class, no guards are
		// emitted, and a valid sentence is rejected. Keeps this in step
		// with firstCharRangesOfElement, which folds case already.
		if r != nil && fold {
			r = foldCaseRanges(r)
		}
	}

	if r == nil {
		c.rangeKnown[tok] = false
		return nil
	}
	r = normalizeRanges(r)
	c.rangeCache[tok] = r
	c.rangeKnown[tok] = true
	return r
}

// tokensOverlap reports whether two tokens' coverages intersect.
func (c *contestCtx) tokensOverlap(a, b string) bool {
	// "\x00" as an ESCAPE, never a literal NUL: one in a .go file
	// makes it binary to grep, which then silently finds nothing in
	// it. (The TS side had exactly that, and it hid this whole
	// machinery from the search that scoped this port.)
	key := a + "\x00" + b
	if b < a {
		key = b + "\x00" + a
	}
	if hit, ok := c.overlapCache[key]; ok {
		return hit
	}
	ra := c.tokenRangesOf(a)
	rb := c.tokenRangesOf(b)
	hit := ra != nil && rb != nil && charRangesOverlap(ra, rb)
	c.overlapCache[key] = hit
	return hit
}

// dispatchEntry is one emitted open alternative together with the IR
// alternative it came from (nil for a synthesized guard or a FOLLOW
// re-issue, which have no alternative of their own).
type dispatchEntry struct {
	o   map[string]any
	alt Sequence
}

// altHeadContested reports whether this alternative's first tokens
// overlap another alternative's at the character level — the condition
// under which a 1-token FIRST peek cannot pick the right alternative
// and K-token prefixes are worth their weight.
func altHeadContested(alt Sequence, all []Sequence, literals, regexTokens map[string]string,
	firstSets map[string]map[string]bool, nullable map[string]bool, cc *contestCtx) bool {

	mine := firstOfAlt(alt, literals, regexTokens, firstSets, nullable)
	if mine == nil {
		return false
	}
	for _, other := range all {
		if len(other) == 0 || seqEqual(other, alt) {
			continue
		}
		theirs := firstOfAlt(other, literals, regexTokens, firstSets, nullable)
		if theirs == nil {
			continue
		}
		for t := range mine {
			for u := range theirs {
				if cc.tokensOverlap(t, u) {
					return true
				}
			}
		}
	}
	return false
}

// contestedByFollow reports a repetition helper whose content can start
// with a token its FOLLOW also contains (`( "," space b-kv )? ( ","
// space c-kv )?` — both sides open with the comma). One token can never
// decide continue-vs-exit there; K-token prefixes on the continue side
// let a failed deep match fall through to the exit peeks instead of
// committing.
func contestedByFollow(prod *Production, alt Sequence, literals, regexTokens map[string]string,
	firstSets map[string]map[string]bool, nullable map[string]bool,
	followSets map[string]map[string]bool, cc *contestCtx) bool {

	if !prod.RepeatHelper {
		return false
	}
	mine := firstOfAlt(alt, literals, regexTokens, firstSets, nullable)
	if mine == nil {
		return false
	}
	for t := range mine {
		for f := range followSets[prod.Name] {
			if f == t || cc.tokensOverlap(t, f) {
				return true
			}
		}
	}
	return false
}

// pairExitGuards builds the FOLLOW₂ exit guards for a CONTESTED
// repetition — one whose repeated element covers a follow token at the
// character level, so that at that character both continuing the loop
// and exiting are locally viable (`ws = *[ \t\n]` before the literal
// "\n").
//
// The 2-token guard writes the decision down: exit exactly when the
// follow token is followed by something only the exit path can accept.
// Ordered BEFORE the continue alternatives; under negotiated lexing the
// guard can re-cut the character to the follow token's identity, and a
// failed guard leaves the loop's own alternatives to re-cut it back.
func pairExitGuards(prod *Production, baseO map[string]any,
	followPairs map[string]map[string]map[string]bool,
	firstSets map[string]map[string]bool, cc *contestCtx) []map[string]any {

	if !prod.RepeatHelper {
		return nil
	}
	pairs := followPairs[prod.Name]
	if len(pairs) == 0 {
		return nil
	}
	contFirst := firstSets[prod.Name]

	out := []map[string]any{}
	seen := map[string]bool{}
	for _, t := range sortedKeysOfPairs(pairs) {
		us := pairs[t]
		if len(us) == 0 {
			continue
		}
		if cc.tokenRangesOf(t) == nil {
			continue
		}
		contested := false
		for _, f := range sortedKeys(contFirst) {
			if f == t {
				continue
			}
			if cc.tokensOverlap(t, f) {
				contested = true
				break
			}
		}
		if !contested {
			continue
		}
		for _, u := range sortedKeys(us) {
			s := t + " " + u
			if seen[s] {
				continue
			}
			seen[s] = true
			g := copyMap(baseO)
			g["s"] = s
			g["b"] = 2
			out = append(out, g)
		}
	}
	return out
}

// synthKeywordGuards builds the 2-token guards for one literal-headed
// dispatch entry: the keyword plus a token only the keyword alternative
// can follow it with. Returns nil when no guard can be justified — and
// then no reordering happens either, which leaves the entry exactly
// where the grammar put it.
func synthKeywordGuards(prod *Production, o map[string]any, alt Sequence, f string,
	consumed int, grammar *Grammar, literals, regexTokens map[string]string,
	followSets map[string]map[string]bool) []map[string]any {

	paths := altPrefixesRaw(alt, grammar, literals, regexTokens, 2, map[string]bool{})
	seconds := map[string]bool{}
	for _, path := range paths {
		p := path.tokens
		if len(p) == 0 || p[0] != f {
			continue
		}
		if len(p) >= 2 {
			seconds[p[1]] = true
			continue
		}
		// The literal can end the alternative (or the prefix was cut
		// short by a cycle): the second token is whatever may follow the
		// production. An unknown FOLLOW means no guard — and then no
		// reordering either.
		fol := followSets[prod.Name]
		if len(fol) == 0 {
			return nil
		}
		for t := range fol {
			seconds[t] = true
		}
	}
	if len(seconds) == 0 || len(seconds) > 16 {
		return nil
	}
	out := []map[string]any{}
	for _, u := range sortedKeys(seconds) {
		g := copyMap(o)
		g["s"] = f + " " + u
		g["b"] = 2 - consumed
		out = append(out, g)
	}
	return out
}

// reorderKeywordShadow places literal-keyword entries so a character
// class cannot shadow them, and so they cannot steal from it either.
//
// A dispatch list built in grammar order puts a character-class
// alternative (`identifier`) ahead of literal-keyword alternatives
// (`"while" …`) whenever the grammar listed them that way — and a
// scannerless lexer cuts `w` as the class token first, so the class
// alternative wins the dispatch and the keyword alternative is
// unreachable (`while(…)` dies inside `identifier ws …`). The
// symmetric problem when the literal comes FIRST: under negotiated
// lexing it re-cuts `intx` to `int` and steals the identifier.
//
// So every literal-headed entry contested by a class-headed entry gets
// 2-token guards placed ahead of the first contesting class entry,
// while its 1-token original drops BEHIND the class entries so it can
// no longer steal; entries that already carry multi-token prefixes
// simply move ahead.
func reorderKeywordShadow(prod *Production, entries []dispatchEntry, grammar *Grammar,
	literals, regexTokens map[string]string, followSets map[string]map[string]bool,
	cc *contestCtx) []map[string]any {

	litToks := map[string]bool{}
	for _, t := range literals {
		litToks[t] = true
	}
	classToks := map[string]bool{}
	for _, t := range regexTokens {
		classToks[t] = true
	}

	// Head token and lookahead length, resolved ONCE per entry. A
	// dispatch list can hold hundreds of entries whose `s` is a
	// four-token prefix, and the loop below is quadratic in them —
	// splitting those strings per comparison dominated compile time.
	n := len(entries)
	heads := make([]string, n)
	sLens := make([]int, n)
	for i, e := range entries {
		s, _ := e.o["s"].(string)
		if s == "" {
			continue
		}
		if sp := strings.IndexByte(s, ' '); sp >= 0 {
			heads[i] = s[:sp]
		} else {
			heads[i] = s
		}
		sLens[i] = 1 + strings.Count(s, " ")
	}

	// Class-headed entries, with their character coverage resolved once.
	classIdx := []int{}
	classRanges := [][]charRange{}
	for i := 0; i < n; i++ {
		f := heads[i]
		if f == "" || !classToks[f] {
			continue
		}
		r := cc.tokenRangesOf(f)
		if r == nil {
			continue
		}
		classIdx = append(classIdx, i)
		classRanges = append(classRanges, r)
	}
	if len(classIdx) == 0 {
		out := make([]map[string]any, n)
		for i, e := range entries {
			out[i] = e.o
		}
		return out
	}

	type placed struct {
		o    map[string]any
		rank float64
		seq  int
	}
	out := []placed{}
	seq := 0
	put := func(o map[string]any, rank float64) {
		out = append(out, placed{o: o, rank: rank, seq: seq})
		seq++
	}

	// Which class entries a given literal head contests, decided once
	// per distinct head token. Entries repeat their head heavily (one
	// per lookahead prefix of the same alternative) and the scan is
	// quadratic, so without this the coverage test runs on every pair.
	contestsByHead := map[string][]bool{}

	for i := 0; i < n; i++ {
		e := entries[i]
		f := heads[i]
		var fr []charRange
		if f != "" && litToks[f] {
			fr = cc.tokenRangesOf(f)
		}

		// First and last contesting class entry, in one pass.
		firstC, lastC := -1, -1
		if fr != nil && e.alt != nil {
			hits, ok := contestsByHead[f]
			if !ok {
				hits = make([]bool, len(classIdx))
				for k := range classIdx {
					hits[k] = charRangesOverlap(fr, classRanges[k])
				}
				contestsByHead[f] = hits
			}
			for k, c := range classIdx {
				if !hits[k] {
					continue
				}
				// Same descent target either way — order is moot.
				if p, ok := e.o["p"]; ok && p != nil && entries[c].o["p"] == p {
					continue
				}
				if firstC == -1 {
					firstC = c
				}
				lastC = c
			}
		}

		if firstC == -1 {
			put(e.o, float64(i))
			continue
		}
		front := float64(firstC) - 0.5
		back := float64(lastC) + 0.5

		if sLens[i] >= 2 {
			// Already carries its own lookahead — just outrank the class.
			put(e.o, minFloat(float64(i), front))
			continue
		}

		consumed := 1
		if b, ok := e.o["b"].(int); ok {
			consumed = 1 - b
		}
		var guards []map[string]any
		if consumed == 0 || consumed == 1 {
			guards = synthKeywordGuards(prod, e.o, e.alt, f, consumed,
				grammar, literals, regexTokens, followSets)
		}
		if guards == nil {
			put(e.o, float64(i))
			continue
		}
		for _, g := range guards {
			put(g, minFloat(float64(i), front))
		}
		put(e.o, maxFloat(float64(i), back))
	}

	// By rank, then by insertion order — the seq tiebreak is what makes
	// equal ranks keep the order the grammar gave them.
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].rank != out[b].rank {
			return out[a].rank < out[b].rank
		}
		return out[a].seq < out[b].seq
	})
	res := make([]map[string]any, len(out))
	for i, p := range out {
		res[i] = p.o
	}
	return res
}

// sortedKeysOfPairs orders the outer keys of a FOLLOW₂ pair map, so
// guard emission is deterministic across runs.
func sortedKeysOfPairs(m map[string]map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// specificityPermute orders contested class heads by specificity.
//
// llama.cpp's schema converter loves `[0-9] | [1] [0-9] | [2] [0-3]` (a
// bounded integer): the 1-token alternative is listed first and,
// matching any digit, shadows the 2-token ones — `23` dies after `2`.
// Among entries whose class heads overlap at the character level and
// whose descents differ, longer lookahead goes first (maximal munch):
// the longer entry only matches where its full prefix does, and a
// failed longer entry still falls through to the shorter one. Without
// negotiated lexing a token's single identity picks the same entry in
// either order, so tokenising notations are unaffected.
//
// Entries are permuted among their OWN slots, so everything else stays
// exactly where it was.
//
// Ported from ts/src/compiler.ts (specificityPermute), which is
// canonical.
func specificityPermute(entries []dispatchEntry, cc *contestCtx,
	grammar *Grammar, regexTokens map[string]string) {

	classToks := map[string]bool{}
	for _, t := range regexTokens {
		classToks[t] = true
	}

	// Head token and lookahead length once per entry — the loop below is
	// quadratic, and re-splitting multi-token `s` strings inside it is
	// what made large grammars slow.
	n := len(entries)
	sLens := make([]int, n)
	heads := make([]string, n)
	for i := range entries {
		s, ok := entries[i].o["s"].(string)
		if !ok || s == "" {
			continue
		}
		sLens[i] = 1 + strings.Count(s, " ")
		if entries[i].alt == nil {
			continue
		}
		f := s
		if sp := strings.Index(s, " "); sp != -1 {
			f = s[:sp]
		}
		if classToks[f] {
			heads[i] = f
		}
	}

	// Coverage per candidate head, resolved once.
	ranges := make([][]charRange, n)
	for i := range entries {
		if heads[i] != "" {
			ranges[i] = cc.tokenRangesOf(heads[i])
		}
	}

	idxs := []int{}
	for i := range entries {
		if ranges[i] == nil {
			continue
		}
		for j := range entries {
			if j == i || ranges[j] == nil {
				continue
			}
			// Same descent target: the order between them is moot. Only
			// when a descent EXISTS, though — terminal-only alternatives
			// all carry no `p`, and reading those as "same target"
			// excludes the whole rule from the permutation, so
			// `[0-9] / [2] [0-3]` keeps its 1-token entry first and
			// misparses `23`.
			pi, iHasP := entries[i].o["p"]
			pj, jHasP := entries[j].o["p"]
			if iHasP && jHasP && pi == pj {
				continue
			}
			if charRangesOverlap(ranges[i], ranges[j]) {
				idxs = append(idxs, i)
				break
			}
		}
	}
	if len(idxs) < 2 {
		return
	}

	// How much the alternative behind an entry can consume in total.
	// Two contested entries can carry the SAME lookahead length when the
	// prefix walk was truncated by a descent (`[0-9]` beside `[1-9]
	// [0-9]{0,15}`, both fanning out to one token), and then lookahead
	// alone cannot rank them. The longer alternative is the more
	// specific one, so it goes first — maximal munch again, one level
	// up. Computed once per contested entry, not inside the comparator.
	spans := map[int]float64{}
	for _, i := range idxs {
		if entries[i].alt == nil {
			spans[i] = 0
			continue
		}
		v := seqTokenSpan(entries[i].alt, grammar, map[string]bool{})
		if math.IsInf(v, 0) {
			v = 1e9
		}
		spans[i] = v
	}

	order := append([]int{}, idxs...)
	sort.SliceStable(order, func(a, b int) bool {
		ia, ib := order[a], order[b]
		if sLens[ia] != sLens[ib] {
			return sLens[ib] < sLens[ia]
		}
		return spans[ib] < spans[ia]
	})
	picked := make([]dispatchEntry, len(order))
	for k, i := range order {
		picked[k] = entries[i]
	}
	for k, slot := range idxs {
		entries[slot] = picked[k]
	}
}
