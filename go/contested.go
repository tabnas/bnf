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
	"unicode/utf8"
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
	// codePoints names the match tokens the canonical compiler compiles
	// with `u` or `v`, which read code points; every other token reads
	// UTF-16 code units there (codeUnitReach).
	codePoints map[string]bool
	// setRanges is the coverage of a class that became a token SET over
	// atoms, so it appears in neither token table. Without it the
	// contest checks read "we do not know what this covers" and go
	// conservative for every overlapping class in the grammar — which is
	// precisely the set of classes the guards exist for.
	setRanges map[string][]charRange

	// The token classes of ConvertOptions.TokenClasses: classSets maps a
	// production to its set, classMembers the set to its tokens.
	classSets    map[string]string
	classMembers map[string][]string
	// Every token set the emitted spec declares (class-partition sets and
	// token classes alike), for the heads predicate.
	tokenSets map[string][]string
	// Token name -> the literal it was allocated for, for the heads
	// predicate's prefix test.
	literalByToken map[string]contestLiteral
	wordKeywords   bool

	rangeCache   map[string][]charRange
	rangeKnown   map[string]bool
	overlapCache map[string]bool
	contestCache map[string]bool
}

type contestLiteral struct {
	literal   string
	sensitive bool
}

func newContestCtx(fixedTokens map[string]*string,
	matchTokens map[string]*regexp.Regexp,
	setRanges map[string][]charRange) *contestCtx {
	return &contestCtx{
		fixedTokens:    fixedTokens,
		matchTokens:    matchTokens,
		setRanges:      setRanges,
		classSets:      map[string]string{},
		classMembers:   map[string][]string{},
		tokenSets:      map[string][]string{},
		literalByToken: map[string]contestLiteral{},
		rangeCache:     map[string][]charRange{},
		rangeKnown:     map[string]bool{},
		overlapCache:   map[string]bool{},
		contestCache:   map[string]bool{},
	}
}

// headsContest answers "can the lexer hand the same input to two dispatch
// heads, so that a choice between them on one token is no choice?" The
// same token can; a literal can meet a literal it is a prefix of, or
// that is a prefix of it, case-folded when either is insensitive — except
// two whole-word keywords under WordKeywords, whose boundary guard keeps
// `option` off `optional`; a token set meets whatever one of its members
// meets; a character class, or an atom of one, meets what its coverage
// overlaps; an engine token meets itself and what its matcher can take
// when the parser asks (engineTokenMeets). Narrower than
// tokensOverlap on purpose: that is the lexer's question, whether two
// heads can claim one CHARACTER. Mirrors the TS headsContest.
func (c *contestCtx) headsContest(a, b string) bool {
	if a == b {
		return true
	}
	// The engine's ANY token takes every token, so it meets every head.
	if a == "#AA" || b == "#AA" {
		return true
	}
	key := a + "\x00" + b
	if b < a {
		key = b + "\x00" + a
	}
	if hit, ok := c.contestCache[key]; ok {
		return hit
	}
	out := false
	ma, aSet := c.tokenSets[strings.TrimPrefix(a, "#")]
	mb, bSet := c.tokenSets[strings.TrimPrefix(b, "#")]
	la, aLit := c.literalByToken[a]
	lb, bLit := c.literalByToken[b]
	if aSet || bSet {
		// A set meets what any member meets. The members are never sets
		// (tokenClassNames), and the provisional answer, the conservative
		// one, would end the expansion if one were.
		c.contestCache[key] = true
		xs := []string{a}
		if aSet {
			xs = ma
		}
		ys := []string{b}
		if bSet {
			ys = mb
		}
	outer:
		for _, x := range xs {
			for _, y := range ys {
				if c.headsContest(x, y) {
					out = true
					break outer
				}
			}
		}
	} else if aLit && bLit {
		x, y := la.literal, lb.literal
		short, long := x, y
		if utf8.RuneCountInString(y) < utf8.RuneCountInString(x) {
			short, long = y, x
		}
		isPrefix := strings.HasPrefix(long, short)
		if !(la.sensitive && lb.sensitive) {
			isPrefix = foldedPrefix(short, long)
		}
		if isPrefix {
			// Two whole-word keywords under WordKeywords: the shorter's
			// boundary guard refuses the longer wherever the longer goes
			// on with a word character (`option` off `optional`), and
			// admits it wherever it goes on with anything else (`a` on
			// `a-b`).
			out = true
			if c.isWordLiteral(x) && c.isWordLiteral(y) {
				lr, sr := []rune(long), []rune(short)
				if len(sr) < len(lr) && isWordRune(lr[len(sr)]) {
					out = false
				}
			}
		}
	} else {
		// A character class, or an atom of one, against anything: by
		// coverage, when the coverage is exact. An engine token has no
		// coverage of its own, and meets what its matcher can take
		// (engineTokenMeets).
		out = !c.regexHeadIsExact(a) || !c.regexHeadIsExact(b) || c.tokensOverlap(a, b) ||
			c.engineTokenMeets(a, b) || c.engineTokenMeets(b, a)
	}
	c.contestCache[key] = out
	return out
}

// foldedPrefix reports whether one case-insensitive literal is a prefix
// of the other under the matcher's own folding. Lowercase alone is not
// that relation: (?i)Σ takes ς and (?i)ς takes Σ, while lowercase maps Σ
// to σ and leaves ς alone. Folding both ways over-approximates the
// matcher (a contest declared where the matcher would not meet) and
// never under-approximates it, which is the safe direction here.
func foldedPrefix(short, long string) bool {
	return strings.HasPrefix(strings.ToLower(long), strings.ToLower(short)) ||
		strings.HasPrefix(strings.ToUpper(long), strings.ToUpper(short))
}

func isWordRune(r rune) bool {
	return r == '_' || ('0' <= r && r <= '9') || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z')
}

// regexHeadIsExact reports whether a regex-backed head's first character
// can be read off its pattern: one atom or one class, alone or repeated
// with `+`, whose coverage patternCharRanges can name. Anything else
// (`a|b` begins with b too, `a?b` with b, `.` with anything, `\d` with a
// digit and `\n` with a character patternCharRanges declines to name)
// may meet any head, and the dispatcher must treat it so. Mirrors the TS
// regexHeadIsExact.
func (c *contestCtx) regexHeadIsExact(tok string) bool {
	re, ok := c.matchTokens[tok]
	if !ok || re == nil {
		return true
	}
	src := re.String()
	// A case-insensitive matcher folds case, and the coverage is folded
	// for ASCII only (foldCaseRanges): a head that reaches beyond ASCII
	// meets whatever Unicode folding lets it ((?i)[Σ] takes ς), which the
	// coverage does not say. That holds for a case-insensitive literal as
	// much as for a pattern.
	if strings.HasPrefix(src, "(?i)") {
		r := c.tokenRangesOf(tok)
		if r == nil {
			return false
		}
		for _, span := range r {
			if span.hi > 0x7F {
				return false
			}
		}
	}
	// A literal, fixed or guarded (`^option\b`), covers its first
	// character exactly whatever follows it in the matcher.
	if _, isLit := c.literalByToken[tok]; isLit {
		return true
	}
	src = strings.TrimPrefix(src, "(?i)")
	src = strings.TrimPrefix(src, "^")
	if strings.HasPrefix(src, "(?:") && strings.HasSuffix(src, ")") {
		src = src[3 : len(src)-1]
	}
	end := regexHeadAtomEnd(src)
	if end < 0 {
		return false
	}
	rest := src[end:]
	return (rest == "" || rest == "+") && c.tokenRangesOf(tok) != nil
}

// literalHeadRangesOf is what a literal head covers: a literal its first
// character; a token class's set what its literal members cover, since
// an engine token among them (#TX) meets no character class here, as it
// would not as a head of its own. Mirrors the TS literalHeadRangesOf.
func (c *contestCtx) literalHeadRangesOf(tok string) []charRange {
	members, isClass := c.classMembers[tok]
	if !isClass {
		return c.tokenRangesOf(tok)
	}
	var out []charRange
	for _, m := range members {
		if r := c.tokenRangesOf(m); r != nil {
			out = append(out, r...)
		}
	}
	return out
}

// Three of the engine's own matchers can take text another head claims,
// and under negotiated lexing the parser asks them to: relex runs only the
// matchers that can produce the token an alternative wants. The number
// matcher takes a leading digit, sign or point, the string matcher a
// leading quote, and the text matcher any text that no fixed token claims,
// a number's or a string's included; a fixed literal it defers to.
// Measured against the four-token dispatch this replaced, these are
// exactly the pairs it kept apart that a one-token dispatch would not. #VL
// is not among them: an emitted grammar lexes no values, so nothing is
// ever cut as one. Mirrors the TS engineTokenMeets.
var (
	numberStart = []charRange{{0x2B, 0x2B}, {0x2D, 0x2E}, {0x30, 0x39}}
	quoteStart  = []charRange{{0x22, 0x22}, {0x27, 0x27}, {0x60, 0x60}}
)

func (c *contestCtx) engineTokenMeets(eng, other string) bool {
	if eng == "#TX" {
		if other == "#NR" || other == "#ST" {
			return true
		}
		re, ok := c.matchTokens[other]
		return ok && re != nil
	}
	var starts []charRange
	switch eng {
	case "#NR":
		starts = numberStart
	case "#ST":
		starts = quoteStart
	default:
		return false
	}
	r := c.tokenRangesOf(other)
	return r != nil && charRangesOverlap(starts, r)
}

func (c *contestCtx) isWordLiteral(lit string) bool {
	return c.wordKeywords && endsWithWordChar(lit)
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

	if spanned, ok := c.setRanges[tok]; ok && spanned != nil {
		r := normalizeRanges(spanned)
		c.rangeCache[tok] = r
		c.rangeKnown[tok] = true
		return r
	}

	var r []charRange
	if lit, ok := c.fixedTokens[tok]; ok && lit != nil && *lit != "" {
		cp := []rune(*lit)[0]
		r = codeUnitReach([]charRange{{cp, cp}}, "")
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
		if r != nil && !c.codePoints[tok] {
			r = codeUnitReach(r, "")
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
	hit := false
	ma, aClass := c.classMembers[a]
	mb, bClass := c.classMembers[b]
	if aClass || bClass {
		// A token class meets what any member meets, the same token
		// included; coverage alone would miss an engine token in it.
		// tokenClassNames keeps a set out of every set's members; were one
		// to get in, this provisional answer, the conservative one, is
		// what ends the expansion when it comes back to the same pair.
		c.overlapCache[key] = true
		xs := []string{a}
		if aClass {
			xs = ma
		}
		ys := []string{b}
		if bClass {
			ys = mb
		}
	outer:
		for _, x := range xs {
			for _, y := range ys {
				if x == y || c.tokensOverlap(x, y) {
					hit = true
					break outer
				}
			}
		}
	} else {
		ra := c.tokenRangesOf(a)
		rb := c.tokenRangesOf(b)
		hit = ra != nil && rb != nil && charRangesOverlap(ra, rb)
	}
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
	followSets map[string]map[string]bool, classSets map[string]string) []map[string]any {

	paths := altPrefixesRaw(alt, grammar, literals, regexTokens, 2, map[string]bool{}, nil, classSets)
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

	// A token class's set is a literal head here: its members are
	// literals and engine tokens, never a character class
	// (tokenClassNames), and with the option off those members are
	// literal heads this ordering places, each one.
	litToks := map[string]bool{}
	for _, t := range literals {
		litToks[t] = true
	}
	for _, t := range cc.classSets {
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
			fr = cc.literalHeadRangesOf(f)
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
				grammar, literals, regexTokens, followSets, cc.classSets)
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

// classAnalysis records what every character class in the grammar
// covers, which of them contest a position with another, and the shared
// partition the contested ones are laid over.
//
// A class whose coverage overlaps no other class keeps exactly what it
// had before this existed: one match token, named after its pattern.
// That is the common case — of the 75 .abnf files in the conformance
// corpus, 44 have no overlapping classes at all — and keeping it
// byte-identical is what stops token tables and the generated rule names
// derived from them moving for grammars that never had the problem.
//
// A class whose pattern is not a plain character coverage
// (patternCharRanges returns nil) cannot be partitioned, so it never
// counts as contested and is left alone.
type classAnalysis struct {
	coverage  map[string][]charRange
	contested map[string]bool
	atoms     []charRange
	// atomTokens maps an atom span to the token minted for it, filled in
	// as classes are allocated so an atom lands at the position of the
	// first class that needs it.
	atomTokens map[string]string
}

func newClassAnalysis(terminals []*Element) *classAnalysis {
	coverage := map[string][]charRange{}
	var order []string
	// The classes the canonical matcher reads in UTF-16 code units: no
	// `u` or `v`.
	codeUnits := map[string]bool{}
	for _, el := range terminals {
		if el.Kind != KindRegex {
			continue
		}
		key := regexKey(el)
		if _, seen := coverage[key]; seen {
			continue
		}
		// Only classes that provably match exactly one code point take
		// part: partitioning replaces a class's matcher with
		// one-character atoms, so anything that could match more would
		// lose the rest. A class left out contributes no coverage, and so
		// neither contests nor is contested — it keeps the single token it
		// has always had.
		r := singleCodePointRanges(el.Pattern, el.Flags)
		if r == nil {
			continue
		}
		coverage[key] = normalizeRanges(r)
		order = append(order, key)
		if !strings.ContainsAny(el.Flags, "uv") {
			codeUnits[key] = true
		}
	}

	// A lead surrogate means one thing to a class read in code points, a
	// lead surrogate standing alone, and another to a class read in code
	// units, which also takes it as the first half of every astral
	// character it begins. No one atom can be both, so a class read in
	// code units that names a lead surrogate some class read in code
	// points names as well is left out, and keeps its own matcher. RE2
	// reads code points whatever the flags say, but this port leaves the
	// same classes out, so the three ports emit the same grammar. Mirrors
	// TS classAnalysis (tabnas/bnf#75 review).
	var pointLeads []charRange
	for _, key := range order {
		if !codeUnits[key] {
			pointLeads = append(pointLeads, leadSurrogates(coverage[key])...)
		}
	}
	pointLeads = normalizeRanges(pointLeads)
	kept := order[:0:0]
	for _, key := range order {
		if codeUnits[key] && charRangesOverlap(leadSurrogates(coverage[key]), pointLeads) {
			delete(coverage, key)
			continue
		}
		kept = append(kept, key)
	}
	order = kept

	contested := map[string]bool{}
	for _, key := range order {
		for _, other := range order {
			if other != key && charRangesOverlap(coverage[key], coverage[other]) {
				contested[key] = true
				break
			}
		}
	}

	var atoms []charRange
	if len(contested) > 0 {
		var coverages [][]charRange
		// Walked in allocation order, not map order, so the partition is
		// identical between runs.
		for _, key := range order {
			if contested[key] {
				coverages = append(coverages, coverage[key])
			}
		}
		atoms = partitionRanges(coverages)
	}

	return &classAnalysis{
		coverage:   coverage,
		contested:  contested,
		atoms:      atoms,
		atomTokens: map[string]string{},
	}
}

// altHeadSharesToken reports whether two alternates can be handed the
// SAME token, so that choosing between them on one token is not a
// choice at all.
//
// Deliberately narrower than altHeadContested, which asks whether two
// heads can claim the same CHARACTER — the right question for the
// lexer, and the wrong one here. `greeting = "hello" name / "hi" name`
// has two heads sharing an `h`, but `#HELLO` and `#HI` are distinct
// tokens and the dispatch was never in doubt; asking the character
// question doubled that rule's alternates for nothing.
//
// Two DIFFERENT class tokens can still be handed the same token,
// because a class that spans several atoms is a set: `%x30-39` and
// `%x31-39` share three of the four atoms they are built from. That is
// the case this exists for, so overlap still counts when both sides are
// classes.
func altHeadSharesToken(alt Sequence, all []Sequence,
	literals, regexTokens map[string]string,
	firstSets map[string]map[string]bool, nullable map[string]bool,
	classToks map[string]bool, cc *contestCtx) bool {

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
				if t == u {
					return true
				}
				if classToks[t] && classToks[u] && cc.tokensOverlap(t, u) {
					return true
				}
			}
		}
	}
	return false
}
