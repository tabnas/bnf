// Copyright (c) 2025-2026 Richard Rodger and other contributors, MIT License

package tabnasabnf

// probe.go — the probe-dispatch analyser + rewriter, the Go port of the
// `[X D] Y` ambiguity handling in converter.ts. For an optional group
// whose body ends with a terminal D and is followed by a tail Y whose
// leading vocabulary overlaps X's, we rewrite the rule to a
// probe + phase-retry dispatcher (function-free in builtins mode).

import "strconv"

// isProbeableOpt: element is `[ X D ]` where X is one or more elements
// and D is a terminal literal or a regex terminal. Returns (xSeq, D).
func isProbeableOpt(el *abnfElement) (abnfSequence, *abnfElement, bool) {
	if el.Kind != kindOpt {
		return nil, nil, false
	}
	inner := el.Inner
	if inner.Kind != kindGroup || len(inner.Alts) != 1 {
		return nil, nil, false
	}
	seq := inner.Alts[0]
	if len(seq) < 2 {
		return nil, nil, false
	}
	last := seq[len(seq)-1]
	if last.Kind != kindTerm && last.Kind != kindRegex {
		return nil, nil, false
	}
	xSeq := append(abnfSequence{}, seq[:len(seq)-1]...)
	return xSeq, last, true
}

func collectTerminalVocabElements(el *abnfElement, grammar *abnfGrammar,
	out map[string]*abnfElement, visited map[string]bool) {
	switch el.Kind {
	case kindTerm:
		k := termKey(el)
		if _, ok := out[k]; !ok {
			out[k] = el
		}
	case kindRegex:
		k := regexKey(el)
		if _, ok := out[k]; !ok {
			out[k] = el
		}
	case kindToken:
		// Key builtin tokens by their token name (e.g. "#TX").
		if _, ok := out[el.Name]; !ok {
			out[el.Name] = el
		}
	case kindRef:
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
	case kindOpt, kindStar, kindPlus, kindRep:
		collectTerminalVocabElements(el.Inner, grammar, out, visited)
	case kindGroup:
		for _, alt := range el.Alts {
			for _, sub := range alt {
				collectTerminalVocabElements(sub, grammar, out, visited)
			}
		}
	}
}

func collectSeqVocabElements(seq abnfSequence, grammar *abnfGrammar) map[string]*abnfElement {
	out := map[string]*abnfElement{}
	visited := map[string]bool{}
	for _, el := range seq {
		collectTerminalVocabElements(el, grammar, out, visited)
	}
	return out
}

func mapsOverlap(a, b map[string]*abnfElement) bool {
	for k := range a {
		if _, ok := b[k]; ok {
			return true
		}
	}
	return false
}

// rewriteProbeDispatches rewrites every ambiguous `[X D] Y` subsequence
// into a probe-dispatch pattern. Runs before token allocation.
func rewriteProbeDispatches(grammar *abnfGrammar) *abnfGrammar {
	reports := grammar.Ambiguities
	if reports == nil {
		reports = []ambiguityReport{}
	}
	extra := []*abnfProduction{}
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

	rewritten := []*abnfProduction{}

	for _, prod := range grammar.Productions {
		newAlts := []abnfSequence{}
		touched := false
		for altIdx, alt := range prod.Alts {
			resultAlt := abnfSequence{}
			for i := 0; i < len(alt); i++ {
				el := alt[i]
				xSeq, disamb, ok := isProbeableOpt(el)
				if !ok {
					resultAlt = append(resultAlt, el)
					continue
				}
				ySeq := append(abnfSequence{}, alt[i+1:]...)
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
				vocab := map[string]*abnfElement{}
				vocabOrder := []string{}
				addVocab := func(m map[string]*abnfElement, order *[]string) {
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
				if disamb.Kind == kindTerm {
					dKey = termKey(disamb)
				} else if disamb.Kind == kindRegex {
					dKey = regexKey(disamb)
				} else if disamb.Kind == kindToken {
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
				vocabElems := []*abnfElement{}
				for _, k := range vocabOrder {
					if v, ok := vocab[k]; ok {
						vocabElems = append(vocabElems, v)
					}
				}

				extra = append(extra, &abnfProduction{
					Name:        probeName,
					Alts:        []abnfSequence{},
					ProbeHelper: &probeHelperSpec{VocabElements: vocabElems},
					NodeKind:    "helper",
				})
				withAlt := append(append(abnfSequence{}, xSeq...), disamb)
				withAlt = append(withAlt, ySeq...)
				extra = append(extra, &abnfProduction{
					Name: withName, Alts: []abnfSequence{withAlt}, NodeKind: "helper"})
				extra = append(extra, &abnfProduction{
					Name: noName, Alts: []abnfSequence{ySeq}, NodeKind: "helper"})
				extra = append(extra, &abnfProduction{
					Name: dispatchName,
					Alts: []abnfSequence{
						{{Kind: kindRef, Name: withName}},
						{{Kind: kindRef, Name: noName}},
					},
					ProbeDisp: &probeDispatchSpec{
						ProbeRule:     probeName,
						Disambiguator: disamb,
						WithBranch:    withName,
						NoBranch:      noName,
					},
					NodeKind: "helper",
				})

				reports = append(reports, ambiguityReport{
					Rule: prod.Name, AltIdx: altIdx, OptIdx: i,
					Reason: "optional prefix shares vocabulary with tail", Resolved: true,
				})

				resultAlt = append(resultAlt, &abnfElement{Kind: kindRef, Name: dispatchName})
				i = len(alt)
				touched = true
			}
			newAlts = append(newAlts, resultAlt)
		}
		if touched {
			rewritten = append(rewritten, &abnfProduction{
				Name: prod.Name, Alts: newAlts, NodeKind: prod.NodeKind})
		} else {
			rewritten = append(rewritten, prod)
		}
	}

	return &abnfGrammar{
		Productions: append(rewritten, extra...),
		Ambiguities: reports,
	}
}

// orderedVocabKeys returns the keys of a vocab map. To mirror the TS
// Map insertion order (which derives from grammar walk order) we sort
// for determinism; vocab membership, not order, drives correctness.
func orderedVocabKeys(m map[string]*abnfElement) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortStrings(keys)
	return keys
}
