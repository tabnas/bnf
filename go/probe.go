// Copyright (c) 2025-2026 Richard Rodger and other contributors, MIT License

package bnf

// probe.go — the probe-dispatch analyser + rewriter, the Go port of the
// `[X D] Y` ambiguity handling in converter.ts. For an optional group
// whose body ends with a terminal D and is followed by a tail Y whose
// leading vocabulary overlaps X's, we rewrite the rule to a
// probe + phase-retry dispatcher (function-free in builtins mode).

import "strconv"

// isProbeableOpt: element is `[ X D ]` where X is one or more elements
// and D is a terminal literal or a regex terminal. Returns (xSeq, D).
func isProbeableOpt(el *Element) (Sequence, *Element, bool) {
	if el.Kind != KindOpt {
		return nil, nil, false
	}
	inner := el.Inner
	if inner.Kind != KindGroup || len(inner.Alts) != 1 {
		return nil, nil, false
	}
	seq := inner.Alts[0]
	if len(seq) < 2 {
		return nil, nil, false
	}
	last := seq[len(seq)-1]
	if last.Kind != KindTerm && last.Kind != KindRegex {
		return nil, nil, false
	}
	xSeq := append(Sequence{}, seq[:len(seq)-1]...)
	return xSeq, last, true
}

func collectTerminalVocabElements(el *Element, grammar *Grammar,
	out map[string]*Element, visited map[string]bool) {
	switch el.Kind {
	case KindTerm:
		k := termKey(el)
		if _, ok := out[k]; !ok {
			out[k] = el
		}
	case KindRegex:
		k := regexKey(el)
		if _, ok := out[k]; !ok {
			out[k] = el
		}
	case KindToken:
		// Key builtin tokens by their token name (e.g. "#TX").
		if _, ok := out[el.Name]; !ok {
			out[el.Name] = el
		}
	case KindRef:
		if visited[el.Name] {
			return
		}
		visited[el.Name] = true
		prod := findProd(grammar, el.Name)
		if prod == nil {
			return
		}
		for _, alt := range prod.Alts {
			for _, sub := range alt {
				collectTerminalVocabElements(sub, grammar, out, visited)
			}
		}
	case KindOpt, KindStar, KindPlus, KindRep:
		collectTerminalVocabElements(el.Inner, grammar, out, visited)
	case KindGroup:
		for _, alt := range el.Alts {
			for _, sub := range alt {
				collectTerminalVocabElements(sub, grammar, out, visited)
			}
		}
	}
}

func collectSeqVocabElements(seq Sequence, grammar *Grammar) map[string]*Element {
	out := map[string]*Element{}
	visited := map[string]bool{}
	for _, el := range seq {
		collectTerminalVocabElements(el, grammar, out, visited)
	}
	return out
}

func mapsOverlap(a, b map[string]*Element) bool {
	for k := range a {
		if _, ok := b[k]; ok {
			return true
		}
	}
	return false
}

// rewriteProbeDispatches rewrites every ambiguous `[X D] Y` subsequence
// into a probe-dispatch pattern. Runs before token allocation.
func rewriteProbeDispatches(grammar *Grammar) *Grammar {
	reports := grammar.Ambiguities
	if reports == nil {
		reports = []AmbiguityReport{}
	}
	extra := []*Production{}
	used := map[string]bool{}
	for _, p := range grammar.Productions {
		used[p.Name] = true
	}
	freshName := func(hint string) string {
		name := hint
		i := 1
		for used[name] {
			name = hint + strconv.Itoa(i)
			i++
		}
		used[name] = true
		return name
	}

	rewritten := []*Production{}

	for _, prod := range grammar.Productions {
		newAlts := []Sequence{}
		touched := false
		for altIdx, alt := range prod.Alts {
			resultAlt := Sequence{}
			for i := 0; i < len(alt); i++ {
				el := alt[i]
				xSeq, disamb, ok := isProbeableOpt(el)
				if !ok {
					resultAlt = append(resultAlt, el)
					continue
				}
				ySeq := append(Sequence{}, alt[i+1:]...)
				if len(ySeq) == 0 {
					resultAlt = append(resultAlt, el)
					continue
				}
				xVocab := collectSeqVocabElements(xSeq, grammar)
				yVocab := collectSeqVocabElements(ySeq, grammar)
				if !mapsOverlap(xVocab, yVocab) {
					resultAlt = append(resultAlt, el)
					continue
				}

				// Joint vocab minus the disambiguator.
				vocab := map[string]*Element{}
				vocabOrder := []string{}
				addVocab := func(m map[string]*Element, order *[]string) {
					for _, k := range orderedVocabKeys(m) {
						if _, exists := vocab[k]; !exists {
							vocab[k] = m[k]
							*order = append(*order, k)
						}
					}
				}
				addVocab(xVocab, &vocabOrder)
				addVocab(yVocab, &vocabOrder)
				var dKey string
				if disamb.Kind == KindTerm {
					dKey = termKey(disamb)
				} else if disamb.Kind == KindRegex {
					dKey = regexKey(disamb)
				} else if disamb.Kind == KindToken {
					dKey = disamb.Name
				}
				if dKey != "" {
					delete(vocab, dKey)
				}

				dispatchName := freshName(prod.Name + "$pd" + strconv.Itoa(i))
				probeName := freshName(dispatchName + "$probe")
				withName := freshName(dispatchName + "$with")
				noName := freshName(dispatchName + "$no")

				// Vocab elements in insertion order, minus disambiguator.
				vocabElems := []*Element{}
				for _, k := range vocabOrder {
					if v, ok := vocab[k]; ok {
						vocabElems = append(vocabElems, v)
					}
				}

				extra = append(extra, &Production{
					Name:        probeName,
					Alts:        []Sequence{},
					ProbeHelper: &ProbeHelperSpec{VocabElements: vocabElems},
					NodeKind:    "helper",
					Origin:      originOf(prod),
				})
				withAlt := append(append(Sequence{}, xSeq...), disamb)
				withAlt = append(withAlt, ySeq...)
				extra = append(extra, &Production{
					Name: withName, Alts: []Sequence{withAlt}, NodeKind: "helper",
					Origin: originOf(prod)})
				extra = append(extra, &Production{
					Name: noName, Alts: []Sequence{ySeq}, NodeKind: "helper",
					Origin: originOf(prod)})
				extra = append(extra, &Production{
					Name: dispatchName,
					Alts: []Sequence{
						{{Kind: KindRef, Name: withName}},
						{{Kind: KindRef, Name: noName}},
					},
					ProbeDisp: &ProbeDispatchSpec{
						ProbeRule:     probeName,
						Disambiguator: disamb,
						WithBranch:    withName,
						NoBranch:      noName,
					},
					NodeKind: "helper",
					Origin:   originOf(prod),
				})

				reports = append(reports, AmbiguityReport{
					Rule: prod.Name, AltIdx: altIdx, OptIdx: i,
					Reason: "optional prefix shares vocabulary with tail", Resolved: true,
				})

				resultAlt = append(resultAlt, &Element{Kind: KindRef, Name: dispatchName})
				i = len(alt)
				touched = true
			}
			newAlts = append(newAlts, resultAlt)
		}
		if touched {
			rewritten = append(rewritten, &Production{
				Name: prod.Name, Alts: newAlts, NodeKind: prod.NodeKind,
				Origin: prod.Origin, Sp: prod.Sp})
		} else {
			rewritten = append(rewritten, prod)
		}
	}

	return &Grammar{
		Productions: append(rewritten, extra...),
		Ambiguities: reports,
	}
}

// orderedVocabKeys returns the keys of a vocab map. To mirror the TS
// Map insertion order (which derives from grammar walk order) we sort
// for determinism; vocab membership, not order, drives correctness.
func orderedVocabKeys(m map[string]*Element) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortStrings(keys)
	return keys
}
