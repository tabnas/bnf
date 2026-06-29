// Copyright (c) 2025-2026 Richard Rodger and other contributors, MIT License

package tabnasabnf

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

func computeFirstSets(grammar *abnfGrammar, literals, regexTokens map[string]string) (map[string]map[string]bool, map[string]bool) {
	firstSets := map[string]map[string]bool{}
	nullable := map[string]bool{}
	for _, p := range grammar.Productions {
		firstSets[p.Name] = map[string]bool{}
	}

	changed := true
	for changed {
		changed = false
		for _, prod := range grammar.Productions {
			first := firstSets[prod.Name]
			for _, alt := range prod.Alts {
				altNullable := true
				for _, el := range alt {
					if el.Kind == kindTerm || el.Kind == kindRegex || el.Kind == kindToken {
						tok := tokenForTerminal(el, literals, regexTokens)
						if !first[tok] {
							first[tok] = true
							changed = true
						}
						altNullable = false
						break
					}
					if el.Kind == kindRef {
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
					panic("abnf: internal — unexpected kind in FIRST: " + string(el.Kind))
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

func tokenForTerminal(el *abnfElement, literals, regexTokens map[string]string) string {
	switch el.Kind {
	case kindTerm:
		return literals[termKey(el)]
	case kindToken:
		return el.Name
	default:
		return regexTokens[regexKey(el)]
	}
}

// firstOfAlt returns the FIRST set for a specific alt, or nil if the alt
// is nullable.
func firstOfAlt(alt abnfSequence, literals, regexTokens map[string]string,
	firstSets map[string]map[string]bool, nullable map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, el := range alt {
		if el.Kind == kindTerm || el.Kind == kindRegex || el.Kind == kindToken {
			out[tokenForTerminal(el, literals, regexTokens)] = true
			return out
		}
		if el.Kind == kindRef {
			for tok := range firstSets[el.Name] {
				out[tok] = true
			}
			if !nullable[el.Name] {
				return out
			}
			continue
		}
		panic("abnf: internal — unexpected kind in firstOfAlt: " + string(el.Kind))
	}
	return nil
}

// ---- k-token prefixes ----------------------------------------------

type prefixPath struct {
	tokens []string
	done   bool
}

func altPrefixesRaw(alt abnfSequence, grammar *abnfGrammar, literals, regexTokens map[string]string,
	maxK int, visited map[string]bool) []prefixPath {
	paths := []prefixPath{{tokens: []string{}, done: false}}

	for _, el := range alt {
		next := []prefixPath{}
		for _, p := range paths {
			if p.done || len(p.tokens) >= maxK {
				next = append(next, p)
				continue
			}
			switch el.Kind {
			case kindTerm:
				next = append(next, prefixPath{
					tokens: appendStr(p.tokens, literals[termKey(el)]), done: false})
			case kindRegex:
				next = append(next, prefixPath{
					tokens: appendStr(p.tokens, regexTokens[regexKey(el)]), done: false})
			case kindToken:
				next = append(next, prefixPath{
					tokens: appendStr(p.tokens, el.Name), done: false})
			case kindRef:
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
				for _, sub := range target.Alts {
					subPaths := altPrefixesRaw(sub, grammar, literals, regexTokens,
						maxK-len(p.tokens), childVisited)
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

func altPrefixes(alt abnfSequence, grammar *abnfGrammar, literals, regexTokens map[string]string, maxK int) [][]string {
	raw := altPrefixesRaw(alt, grammar, literals, regexTokens, maxK, map[string]bool{})
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

func findProd(grammar *abnfGrammar, name string) *abnfProduction {
	for _, p := range grammar.Productions {
		if p.Name == name {
			return p
		}
	}
	return nil
}

// ---- probe-dispatch emitters ---------------------------------------

func emitProbeHelper(prod *abnfProduction, tag string, ruleSpec map[string]*tabnas.GrammarRuleSpec,
	literals, regexTokens map[string]string) {
	elems := prod.ProbeHelper.VocabElements
	opens := []map[string]any{}
	for _, el := range elems {
		var tok string
		if el.Kind == kindTerm {
			tok = literals[termKey(el)]
		} else if el.Kind == kindRegex {
			tok = regexTokens[regexKey(el)]
		} else if el.Kind == kindToken {
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

func emitProbeDispatch(prod *abnfProduction, tag string, ruleSpec map[string]*tabnas.GrammarRuleSpec,
	refs *refRegistry, literals, regexTokens map[string]string, useBuiltins bool) {
	pd := prod.ProbeDisp
	var disambiguatorToken string
	if pd.Disambiguator.Kind == kindTerm {
		disambiguatorToken = literals[termKey(pd.Disambiguator)]
	} else if pd.Disambiguator.Kind == kindRegex {
		disambiguatorToken = regexTokens[regexKey(pd.Disambiguator)]
	} else if pd.Disambiguator.Kind == kindToken {
		disambiguatorToken = pd.Disambiguator.Name
	}
	if disambiguatorToken == "" {
		panic("abnf: probe-dispatch rule '" + prod.Name + "' has unresolvable disambiguator")
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
		r.K["pd_phase"] = 0
		r.K["pd_mark"] = ctx.Mark()
	})
	decide := refs.registerAction(func(r *tabnas.Rule, ctx *tabnas.Context) {
		var peek *tabnas.Token
		if len(ctx.T) > 0 {
			peek = ctx.T[0]
		}
		mark, _ := r.K["pd_mark"].(int)
		_ = ctx.Rewind(mark)
		matched := peek != nil && peek.Name == disambiguatorToken
		if matched {
			r.K["pd_phase"] = 1
		} else {
			r.K["pd_phase"] = 2
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
