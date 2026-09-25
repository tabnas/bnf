// Copyright (c) 2025-2026 Richard Rodger and other contributors, MIT License

package bnf

// emit_support.go — map->AltSpec conversion, FIRST-set computation,
// literal-prefix / k-prefix enumeration, and the probe-dispatch emitter.

import (
	tabnas "github.com/tabnas/parser/go"
)

// mapToAlt converts a generic alt-spec map (the shape the TS emitter
// builds) into a typed *GrammarAltSpec. Recognised keys: s, b, p, r, a,
// c, k, u, n, m, g. The `m` key (mark) has no typed field, so it rides
// in U under "_mark" for serialisation/inspection; the engine ignores
// it. (markListing reads it back from there.)
func mapToAlt(m map[string]any) *tabnas.GrammarAltSpec {
	alt := &tabnas.GrammarAltSpec{}
	if v, ok := m["s"]; ok {
		alt.S = v
	}
	if v, ok := m["b"]; ok {
		switch n := v.(type) {
		case int:
			alt.B = n
		case float64:
			alt.B = int(n)
		default:
			alt.B = v
		}
	}
	if v, ok := m["p"].(string); ok {
		alt.P = v
	}
	if v, ok := m["r"].(string); ok {
		alt.R = v
	}
	if v, ok := m["a"]; ok {
		alt.A = v
	}
	if v, ok := m["c"]; ok {
		alt.C = v
	}
	if v, ok := m["k"].(map[string]any); ok {
		alt.K = v
	}
	if v, ok := m["u"].(map[string]any); ok {
		alt.U = v
	}
	if v, ok := m["n"].(map[string]int); ok {
		alt.N = v
	}
	if v, ok := m["g"].(string); ok {
		alt.G = v
	}
	// Mark: stash in U under the conventional key so markListing can
	// recover it and attachActions can match on it.
	if v, ok := m["m"].(string); ok {
		if alt.U == nil {
			alt.U = map[string]any{}
		}
		alt.U["m$"] = v
	}
	return alt
}

func mapsToAlts(ms []map[string]any) []*tabnas.GrammarAltSpec {
	out := make([]*tabnas.GrammarAltSpec, 0, len(ms))
	for _, m := range ms {
		out = append(out, mapToAlt(m))
	}
	return out
}

// ---- FIRST sets ----------------------------------------------------

// computeFirstSets is FIRST(ref) for every production, plus which
// productions are nullable. classSets maps a token-class production to
// the token set it compiles to (ConvertOptions.TokenClasses): FIRST of
// such a production is its set, one name, wherever it is referenced.
func computeFirstSets(grammar *Grammar, literals, regexTokens map[string]string,
	classSets map[string]string) (map[string]map[string]bool, map[string]bool) {
	firstSets := map[string]map[string]bool{}
	nullable := map[string]bool{}
	for _, p := range grammar.Productions {
		firstSets[p.Name] = map[string]bool{}
		if set, ok := classSets[p.Name]; ok {
			firstSets[p.Name][set] = true
		}
	}

	changed := true
	for changed {
		changed = false
		for _, prod := range grammar.Productions {
			if _, ok := classSets[prod.Name]; ok {
				continue
			}
			first := firstSets[prod.Name]
			for _, alt := range prod.Alts {
				altNullable := true
				for _, el := range alt {
					if el.Kind == KindTerm || el.Kind == KindRegex || el.Kind == KindToken {
						tok := tokenForTerminal(el, literals, regexTokens)
						if !first[tok] {
							first[tok] = true
							changed = true
						}
						altNullable = false
						break
					}
					if el.Kind == KindRef {
						refFirst := firstSets[el.Name]
						for tok := range refFirst {
							if !first[tok] {
								first[tok] = true
								changed = true
							}
						}
						if !nullable[el.Name] {
							altNullable = false
							break
						}
						continue
					}
					panic(diagName() + ": internal — unexpected kind in FIRST: " + string(el.Kind))
				}
				if altNullable && !nullable[prod.Name] {
					nullable[prod.Name] = true
					changed = true
				}
			}
		}
	}
	return firstSets, nullable
}

func tokenForTerminal(el *Element, literals, regexTokens map[string]string) string {
	switch el.Kind {
	case KindTerm:
		return literals[termKey(el)]
	case KindToken:
		return el.Name
	default:
		return regexTokens[regexKey(el)]
	}
}

// firstOfAlt returns the FIRST set for a specific alt, or nil if the alt
// is nullable.
func firstOfAlt(alt Sequence, literals, regexTokens map[string]string,
	firstSets map[string]map[string]bool, nullable map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, el := range alt {
		if el.Kind == KindTerm || el.Kind == KindRegex || el.Kind == KindToken {
			out[tokenForTerminal(el, literals, regexTokens)] = true
			return out
		}
		if el.Kind == KindRef {
			for tok := range firstSets[el.Name] {
				out[tok] = true
			}
			if !nullable[el.Name] {
				return out
			}
			continue
		}
		panic(diagName() + ": internal — unexpected kind in firstOfAlt: " + string(el.Kind))
	}
	return nil
}

// firstOfSeq is FIRST of a sequence, reporting nullability separately
// rather than collapsing it to nil the way firstOfAlt does — the caller
// needs both halves of the answer. Mirrors the TS `firstOfSeq`.
func firstOfSeq(seq Sequence, literals, regexTokens map[string]string,
	firstSets map[string]map[string]bool, nullable map[string]bool,
) (map[string]bool, bool) {
	out := map[string]bool{}
	for _, el := range seq {
		if el.Kind == KindTerm || el.Kind == KindRegex || el.Kind == KindToken {
			out[tokenForTerminal(el, literals, regexTokens)] = true
			return out, false
		}
		if el.Kind == KindRef {
			for tok := range firstSets[el.Name] {
				out[tok] = true
			}
			if !nullable[el.Name] {
				return out, false
			}
			continue
		}
		panic(diagName() + ": internal — unexpected kind in firstOfSeq: " + string(el.Kind))
	}
	return out, true
}

// ---- suffix debt: contested left-recursion tail loops ---------------
//
// eliminateDirectLeftRec rewrites `A = ["x"] A "y" / "z"` into
//
//	A = ( "x" A "y" | "z" ) "y"*
//
// which is a correct CFG and a broken parser. Parsing `xzy` needs the
// inner A's tail loop to match ZERO `"y"`s, so the enclosing
// `"x" A "y"` has one left to consume; the loop is greedy, eats it, and
// the outer alternative starves. Widening the loop's lookahead cannot
// help, because the two cases it must separate are indistinguishable
// through any token window:
//
//	input | remaining at the decision | required
//	------|--------------------------|-------------------------------
//	xzy   | #Y #ZZ                   | exit — `"x" A "y"` owes a #Y
//	zy    | #Y #ZZ                   | continue — nothing owes a #Y
//
// Same rule, same tokens, opposite answers. What differs is how many
// enclosing frames have committed to consuming a `"y"` once the current
// subtree returns — stack depth, not a token window. The engine already
// propagates exactly that kind of state: `n` counters flow from a rule
// to every rule it pushes, and never back up.
//
// So count the debt. For each contested loop, on every push that can
// reach the recursive rule:
//
//	suffix after the push          | counter
//	-------------------------------|-------------------------------------
//	empty, or can derive ε         | inherited (the push is in tail
//	                               | position; the ancestor's debt stands)
//	mandatory, FIRST hits the loop | +1 — this frame owes the loop's token
//	mandatory, FIRST disjoint      | 0  — a barrier; the frame re-anchors
//
// and guard the loop branches that could eat what is owed with
// `n.<counter> == 0`. The barrier reset is not an optimisation: in
// `A = ["x"] A "y" / "(" A ")" / "z"` the paren alternative owes a
// `")"`, not a `"y"`, so an A pushed from there must start clean or
// `x(zy)y` cannot parse.
//
// "Could eat what is owed" is per branch, not per loop. A loop built from
// several tails repeats several tokens, and only the ones an enclosing
// suffix competes for may be blocked — guarding the whole helper makes
// `A = A "y" / A "w" / "x" A "y" / "z"` reject `xzwy`, where the inner A
// has to consume the `w` before yielding the `y`. The contested tokens are
// recorded in DebtOwed for the emitter.
//
// Competition is decided here by token identity. The TS port additionally
// compares character coverage, so a fixed `"a"` token and a `[a-z]` match
// token read as competing; that rests on the contested-alternative
// machinery this port does not have. See doc/differences.md.
//
// The counter needs no explicit decrement. `n` is copied down at push
// time and a parent's own counters are untouched by what its children
// do, so unwinding out of a frame restores that frame's debt by
// construction.
//
// Nothing is emitted unless some push actually owes the loop's token, so
// a grammar whose loop was never contested compiles unchanged.
// Mirrors the TS `resolveSuffixDebts`. Issue #6.
func resolveSuffixDebts(grammar *Grammar, literals, regexTokens map[string]string,
	firstSets map[string]map[string]bool, nullable map[string]bool, cc *contestCtx) {

	guarded := []*Production{}
	for _, p := range grammar.Productions {
		if p.DebtGuard != "" {
			guarded = append(guarded, p)
		}
	}
	if len(guarded) == 0 {
		return
	}

	type site struct {
		alt   Sequence
		i     int
		delta int
	}

	for _, loop := range guarded {
		counter := loop.DebtGuard

		// The recursive rule is whatever references the loop helper.
		// eliminateDirectLeftRec emits one such reference and desugar mints
		// the helper, so there is exactly one candidate.
		var owner *Production
		for _, p := range grammar.Productions {
			if p == loop {
				continue
			}
			for _, alt := range p.Alts {
				for _, el := range alt {
					if el.Kind == KindRef && el.Name == loop.Name {
						owner = p
					}
				}
			}
			if owner != nil {
				break
			}
		}

		// FIRST of the helper is FIRST of what it repeats: its other
		// alternative is empty.
		loopFirst := firstSets[loop.Name]
		if owner == nil || len(loopFirst) == 0 {
			loop.DebtGuard = ""
			continue
		}

		// Only a push into something that can still reach the recursive rule
		// can end up inside the contested loop; everything else is left alone.
		carries := refCallersOf(grammar, owner.Name)

		pending := []site{}
		// The loop's own tokens that some enclosing suffix competes for.
		owed := map[string]bool{}
		for _, prod := range grammar.Productions {
			for _, alt := range prod.Alts {
				for i, el := range alt {
					if el.Kind != KindRef || !carries[el.Name] {
						continue
					}
					suffix := alt[i+1:]
					if len(suffix) == 0 {
						continue
					}
					toks, sufNullable := firstOfSeq(
						suffix, literals, regexTokens, firstSets, nullable)
					// A suffix that can vanish commits the frame to nothing, so
					// the push stays in tail position and inherits.
					if sufNullable {
						continue
					}
					// Collect every loop token this suffix competes for, rather
					// than stopping at the first: they are exactly the branches
					// the emitter may block, and the rest must stay open.
					// Identity is not enough: a fixed `"a"` token and a
					// `[a-z]` match token are different tokens that claim
					// the same character, and under negotiated lexing the
					// suffix really can take what the loop wants. Mirrors
					// the TS test, which has always compared coverage.
					hits := false
					for u := range toks {
						for t := range loopFirst {
							if t == u || cc.tokensOverlap(t, u) {
								owed[t] = true
								hits = true
							}
						}
					}
					delta := 0
					if hits {
						delta = 1
					}
					pending = append(pending, site{alt: alt, i: i, delta: delta})
				}
			}
		}

		if len(owed) == 0 {
			// Nothing anywhere competes with this loop — the shape matched
			// syntactically but the tokens never collide. Leave the grammar
			// exactly as it was.
			loop.DebtGuard = ""
			continue
		}
		loop.DebtOwed = sortedKeys(owed)

		for _, s := range pending {
			el := s.alt[s.i]
			// Replace rather than mutate. Elements are shared between
			// alternatives and with the caller's AST (cloneGrammar copies the
			// sequences, not the elements), so writing through this reference
			// would annotate occurrences this pass never inspected.
			cp := *el
			cp.Debt = map[string]int{}
			for k, v := range el.Debt {
				cp.Debt[k] = v
			}
			cp.Debt[counter] = s.delta
			s.alt[s.i] = &cp
		}
	}
}

// refCallersOf returns the names of the productions from which `target`
// can be reached through rule references, `target` itself included. A
// backward walk: the forward closure would cost a traversal per
// production, and only this one node's ancestry is ever asked for.
// Mirrors the TS `refCallersOf`.
func refCallersOf(grammar *Grammar, target string) map[string]bool {
	rev := map[string][]string{}
	for _, p := range grammar.Productions {
		out := map[string]bool{}
		for _, alt := range p.Alts {
			refsIn(alt, out)
		}
		// A dispatcher's branches live outside Alts, but a push into one is
		// still a push.
		if p.ProbeDisp != nil {
			out[p.ProbeDisp.ProbeRule] = true
			out[p.ProbeDisp.WithBranch] = true
			out[p.ProbeDisp.NoBranch] = true
		}
		for to := range out {
			rev[to] = append(rev[to], p.Name)
		}
	}

	seen := map[string]bool{target: true}
	queue := []string{target}
	for len(queue) > 0 {
		n := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		for _, from := range rev[n] {
			if seen[from] {
				continue
			}
			seen[from] = true
			queue = append(queue, from)
		}
	}
	return seen
}

// ---- k-token prefixes ----------------------------------------------

type prefixPath struct {
	tokens []string
	done   bool
}

// tokenClassNames is the token classes of a grammar
// (ConvertOptions.TokenClasses): every alternative one literal or engine
// token, at least two of them. A character class (regex) is not a
// member: those are laid over the partition the class analysis builds,
// and a set over them is that machinery's to mint. Mirrors the TS
// tokenClassNames.
func tokenClassNames(grammar *Grammar) map[string]bool {
	out := map[string]bool{}
	for _, prod := range grammar.Productions {
		if prod.ProbeHelper != nil || prod.ProbeDisp != nil || prod.TailRepeat != nil {
			continue
		}
		if prod.Value != nil || len(prod.Alts) < 2 {
			continue
		}
		// The class's set is named after it (#ident), and a reference the
		// substitution pass consumes as that token has to resolve to the
		// set: a production named like an engine token (TX, ZZ) cannot
		// take its own name, so it is not a class.
		if isEngineOwnedToken("#" + prod.Name) {
			continue
		}
		all := true
		for _, alt := range prod.Alts {
			if len(alt) != 1 || (alt[0].Kind != KindTerm && alt[0].Kind != KindToken) {
				all = false
				break
			}
		}
		if all {
			out[prod.Name] = true
		}
	}
	return out
}

// altPrefixesRaw enumerates the concrete token-sequence prefixes an
// alternative can start with, each at most maxK tokens long. headFilter,
// when non-nil, keeps only the paths whose FIRST token it admits: the
// dispatcher deepens the lookahead under a contested head alone, and
// enumerating every path to depth K only to discard the rest is the cost
// tabnas/bnf#71 measured in seconds. classSets maps a token-class
// production to its set: a reference to one is one token, its set, rather
// than one path per member. Mirrors the TS altPrefixesRaw.
func altPrefixesRaw(alt Sequence, grammar *Grammar, literals, regexTokens map[string]string,
	maxK int, visited map[string]bool, headFilter func(string) bool,
	classSets map[string]string) []prefixPath {
	paths := []prefixPath{{tokens: []string{}, done: false}}

	// A path that already carries a token has had its head admitted; only
	// a path still at length zero can be pruned by what it gains next.
	admit := func(before prefixPath, tokens []string) bool {
		return headFilter == nil || 0 < len(before.tokens) || 0 == len(tokens) ||
			headFilter(tokens[0])
	}

	for _, el := range alt {
		next := []prefixPath{}
		for _, p := range paths {
			if p.done || len(p.tokens) >= maxK {
				next = append(next, p)
				continue
			}
			switch el.Kind {
			case KindTerm:
				tokens := appendStr(p.tokens, literals[termKey(el)])
				if admit(p, tokens) {
					next = append(next, prefixPath{tokens: tokens, done: false})
				}
			case KindRegex:
				tokens := appendStr(p.tokens, regexTokens[regexKey(el)])
				if admit(p, tokens) {
					next = append(next, prefixPath{tokens: tokens, done: false})
				}
			case KindToken:
				tokens := appendStr(p.tokens, el.Name)
				if admit(p, tokens) {
					next = append(next, prefixPath{tokens: tokens, done: false})
				}
			case KindRef:
				if set, ok := classSets[el.Name]; ok {
					tokens := appendStr(p.tokens, set)
					if admit(p, tokens) {
						next = append(next, prefixPath{tokens: tokens, done: false})
					}
					continue
				}
				if visited[el.Name] {
					next = append(next, prefixPath{tokens: p.tokens, done: true})
					continue
				}
				childVisited := copyBoolSet(visited)
				childVisited[el.Name] = true
				target := findProd(grammar, el.Name)
				if target == nil || len(target.Alts) == 0 {
					next = append(next, prefixPath{tokens: p.tokens, done: true})
					continue
				}
				// The head of the outer path is the head of the sub-path
				// while nothing has been consumed yet.
				var subFilter func(string) bool
				if 0 == len(p.tokens) {
					subFilter = headFilter
				}
				for _, sub := range target.Alts {
					subPaths := altPrefixesRaw(sub, grammar, literals, regexTokens,
						maxK-len(p.tokens), childVisited, subFilter, classSets)
					for _, sp := range subPaths {
						next = append(next, prefixPath{
							tokens: appendStrs(p.tokens, sp.tokens), done: sp.done})
					}
				}
			default:
				next = append(next, prefixPath{tokens: p.tokens, done: true})
			}
		}
		paths = next
		allDone := true
		for _, p := range paths {
			if !p.done && len(p.tokens) < maxK {
				allDone = false
				break
			}
		}
		if allDone {
			break
		}
	}
	return paths
}

func altPrefixes(alt Sequence, grammar *Grammar, literals, regexTokens map[string]string, maxK int,
	classSets map[string]string) [][]string {
	raw := altPrefixesRaw(alt, grammar, literals, regexTokens, maxK, map[string]bool{}, nil, classSets)
	seen := map[string]bool{}
	out := [][]string{}
	for _, p := range raw {
		key := joinSpace(p.tokens)
		if !seen[key] {
			seen[key] = true
			out = append(out, p.tokens)
		}
	}
	return out
}

// dispatchPrefixes is one entry per alternative of a choice: the
// prefixes to dispatch it on, using the least lookahead each decision
// needs, and whether the alternative can derive ε. Mirrors the TS
// dispatchPrefixes (tabnas/bnf#71).
//
// Every alternative is dispatched on its first tokens alone, and the
// lookahead deepens only under a head two alternatives share, one token
// at a time, until the paths through that head no longer collide, or the
// window (maxK) runs out and the paths are emitted whole, as they always
// were. "Collide" is contest(a, b): the same token, a pair the lexer can
// hand to either alternative, or a token set meeting one of its members.
// Two paths collide up to depth d when their tokens collide at every
// position below d that both have; a path that ends sooner is open, so
// it keeps colliding for as long as it lasts, and the longer path is then
// emitted whole. exitPaths are the ways OUT of a choice that has an empty
// alternative: each FOLLOW token as an open one-token path, and each
// FOLLOW₂ pair.
type dispatchPrefixSet struct {
	prefixes [][]string
	nullable bool
}

func dispatchPrefixes(alts []Sequence, grammar *Grammar, literals, regexTokens map[string]string,
	maxK int, contest func(a, b string) bool, exitPaths [][]string,
	classSets map[string]string) []dispatchPrefixSet {
	n := len(alts)
	one := make([][]prefixPath, n)
	nullable := make([]bool, n)
	heads := make([][]string, n)
	for i, alt := range alts {
		if len(alt) == 0 {
			continue
		}
		one[i] = altPrefixesRaw(alt, grammar, literals, regexTokens, 1, map[string]bool{}, nil, classSets)
		seen := map[string]bool{}
		for _, p := range one[i] {
			if len(p.tokens) == 0 && !p.done {
				nullable[i] = true
			}
			if len(p.tokens) != 1 || seen[p.tokens[0]] {
				continue
			}
			seen[p.tokens[0]] = true
			heads[i] = append(heads[i], p.tokens[0])
		}
	}

	// A head is contested when another alternative, or an exit, has a
	// head the lexer could hand to either.
	contested := make([]map[string]bool, n)
	for i := range alts {
		contested[i] = map[string]bool{}
		for _, h := range heads[i] {
			hit := false
			for j := 0; j < n && !hit; j++ {
				if j == i {
					continue
				}
				for _, o := range heads[j] {
					if contest(h, o) {
						hit = true
						break
					}
				}
			}
			for e := 0; e < len(exitPaths) && !hit; e++ {
				if contest(h, exitPaths[e][0]) {
					hit = true
				}
			}
			if hit {
				contested[i][h] = true
			}
		}
	}

	// Deep paths, only under the contested heads.
	deep := make([][]prefixPath, n)
	for i, alt := range alts {
		if len(contested[i]) == 0 {
			continue
		}
		c := contested[i]
		for _, p := range altPrefixesRaw(alt, grammar, literals, regexTokens, maxK, map[string]bool{},
			func(t string) bool { return c[t] }, classSets) {
			if 0 < len(p.tokens) {
				deep[i] = append(deep[i], p)
			}
		}
	}

	// The depth at which p stops colliding with every rival: the position
	// after the first one where they differ, over all rivals; or the whole
	// path when some rival never differs within what both have.
	depthOf := func(p prefixPath, i int) int {
		d := 1
		against := func(q []string) bool {
			m := len(p.tokens)
			if len(q) < m {
				m = len(q)
			}
			k := 0
			for k < m && contest(p.tokens[k], q[k]) {
				k++
			}
			if k == m {
				return false
			}
			if k+1 > d {
				d = k + 1
			}
			return true
		}
		for j := 0; j < n; j++ {
			if j == i {
				continue
			}
			for _, q := range deep[j] {
				if !against(q.tokens) {
					return len(p.tokens)
				}
			}
		}
		for _, e := range exitPaths {
			if !against(e) {
				return len(p.tokens)
			}
		}
		return d
	}

	out := make([]dispatchPrefixSet, n)
	for i := range alts {
		seen := map[string]bool{}
		prefixes := [][]string{}
		push := func(tokens []string) {
			key := joinSpace(tokens)
			if seen[key] {
				return
			}
			seen[key] = true
			prefixes = append(prefixes, tokens)
		}
		for _, h := range heads[i] {
			if !contested[i][h] {
				push([]string{h})
				continue
			}
			for _, p := range deep[i] {
				if p.tokens[0] != h {
					continue
				}
				push(p.tokens[:depthOf(p, i)])
			}
		}
		out[i] = dispatchPrefixSet{prefixes: prefixes, nullable: nullable[i]}
	}
	return out
}

func findProd(grammar *Grammar, name string) *Production {
	for _, p := range grammar.Productions {
		if p.Name == name {
			return p
		}
	}
	return nil
}

// ---- probe-dispatch emitters ---------------------------------------

func emitProbeHelper(prod *Production, tag string, ruleSpec map[string]*tabnas.GrammarRuleSpec,
	literals, regexTokens map[string]string) {
	elems := prod.ProbeHelper.VocabElements
	opens := []map[string]any{}
	for _, el := range elems {
		var tok string
		if el.Kind == KindTerm {
			tok = literals[termKey(el)]
		} else if el.Kind == KindRegex {
			tok = regexTokens[regexKey(el)]
		} else if el.Kind == KindToken {
			tok = el.Name
		}
		if tok != "" {
			opens = append(opens, map[string]any{"s": tok, "r": prod.Name, "g": tag})
		}
	}
	// Empty fallback — pops without consuming anything. Must be last.
	opens = append(opens, map[string]any{"g": tag})
	ruleSpec[prod.Name] = &tabnas.GrammarRuleSpec{Open: mapsToAlts(opens)}
}

func emitProbeDispatch(prod *Production, tag string, ruleSpec map[string]*tabnas.GrammarRuleSpec,
	refs *refRegistry, literals, regexTokens map[string]string, useBuiltins bool) {
	pd := prod.ProbeDisp
	var disambiguatorToken string
	if pd.Disambiguator.Kind == KindTerm {
		disambiguatorToken = literals[termKey(pd.Disambiguator)]
	} else if pd.Disambiguator.Kind == KindRegex {
		disambiguatorToken = regexTokens[regexKey(pd.Disambiguator)]
	} else if pd.Disambiguator.Kind == KindToken {
		disambiguatorToken = pd.Disambiguator.Name
	}
	if disambiguatorToken == "" {
		panic(diagName() + ": probe-dispatch rule '" + prod.Name + "' has unresolvable disambiguator")
	}

	bubbleFields := refs.bubble()

	if useBuiltins {
		open := []map[string]any{
			{"c": "@probePhase0$", "a": "@probeInit$", "p": pd.ProbeRule,
				"k": map[string]any{"pd_d": disambiguatorToken}, "g": tag},
			{"c": "@probePhase1$", "p": pd.WithBranch, "g": tag},
			{"c": "@probePhase2$", "p": pd.NoBranch, "g": tag},
		}
		close0 := map[string]any{"c": "@probePhase0$", "a": "@probeDecide$", "r": prod.Name, "g": tag}
		close1 := copyMap(bubbleFields)
		close1["g"] = tag
		ruleSpec[prod.Name] = &tabnas.GrammarRuleSpec{
			Open:  mapsToAlts(open),
			Close: mapsToAlts([]map[string]any{close0, close1}),
		}
		return
	}

	// Closure mode.
	initMark := refs.registerAction(func(r *tabnas.Rule, ctx *tabnas.Context) {
		k := r.EnsureK()
		k["pd_phase"] = 0
		k["pd_mark"] = ctx.Mark()
	})
	decide := refs.registerAction(func(r *tabnas.Rule, ctx *tabnas.Context) {
		var peek *tabnas.Token
		if len(ctx.T) > 0 {
			peek = ctx.T[0]
		}
		mark, _ := r.K["pd_mark"].(int)
		_ = ctx.Rewind(mark)
		matched := peek != nil && peek.Name == disambiguatorToken
		k := r.EnsureK()
		if matched {
			k["pd_phase"] = 1
		} else {
			k["pd_phase"] = 2
		}
	})
	phase0 := refs.registerCond(func(r *tabnas.Rule, _ *tabnas.Context) bool {
		return cfgPhase(r.K["pd_phase"]) == 0
	})
	phase1 := refs.registerCond(func(r *tabnas.Rule, _ *tabnas.Context) bool {
		return cfgPhase(r.K["pd_phase"]) == 1
	})
	phase2 := refs.registerCond(func(r *tabnas.Rule, _ *tabnas.Context) bool {
		return cfgPhase(r.K["pd_phase"]) == 2
	})

	open := []map[string]any{
		{"c": string(phase0), "a": string(initMark), "p": pd.ProbeRule, "g": tag},
		{"c": string(phase1), "p": pd.WithBranch, "g": tag},
		{"c": string(phase2), "p": pd.NoBranch, "g": tag},
	}
	close0 := map[string]any{"c": string(phase0), "a": string(decide), "r": prod.Name, "g": tag}
	close1 := copyMap(bubbleFields)
	close1["g"] = tag
	ruleSpec[prod.Name] = &tabnas.GrammarRuleSpec{
		Open:  mapsToAlts(open),
		Close: mapsToAlts([]map[string]any{close0, close1}),
	}
}

func (rr *refRegistry) registerCond(fn tabnas.AltCond) tabnas.FuncRef {
	name := tabnas.FuncRef("@abnf_a" + itoa(rr.counter))
	rr.counter++
	rr.refs[name] = fn
	return name
}

func cfgPhase(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}

// ---- tiny utils ----------------------------------------------------

func appendStr(s []string, x string) []string {
	out := make([]string, len(s)+1)
	copy(out, s)
	out[len(s)] = x
	return out
}
func appendStrs(a, b []string) []string {
	out := make([]string, 0, len(a)+len(b))
	out = append(out, a...)
	out = append(out, b...)
	return out
}
func copyBoolSet(m map[string]bool) map[string]bool {
	out := make(map[string]bool, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
func joinSpace(s []string) string {
	out := ""
	for i, x := range s {
		if i > 0 {
			out += " "
		}
		out += x
	}
	return out
}
func itoa(n int) string {
	return intToStr(n)
}

// nullableImpl records an alternative that can derive ε, so the
// dispatcher can re-issue it after every content entry. See the
// nullableImpls loop in emitProduction.
type nullableImpl struct {
	implName string
	fields   map[string]any
	mark     string
}
