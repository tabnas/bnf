package bnf

// follow.go — FOLLOW and FOLLOW₂ analysis.
//
// Ported from ts/src/compiler.ts (computeFollowSets, computeFollowPairs),
// which is canonical.

// computeFollowSets returns, per production, the tokens that may
// legitimately appear immediately after it.
//
// This exists for one reason: the engine lexes UNDER THE DIRECTION OF
// THE ACTIVE RULE. A matcher-backed token (a character class) is only
// offered at a position where the current rule names it. A generated
// repetition helper ends on an empty alternative, which names nothing,
// so at the moment the loop could terminate the following token is not
// on offer and the lex fails instead. `root ::= sign? [0-9]+` dies one
// character in for exactly this reason.
//
// Naming FOLLOW on that terminating alternative puts those tokens back
// in the rule's token column. The guard alternatives peek and push the
// token straight back (`b: 1`), so they accept nothing extra — they
// only widen what the lexer is willing to produce there.
func computeFollowSets(grammar *Grammar, literals, regexTokens map[string]string,
	firstSets map[string]map[string]bool, nullable map[string]bool, start string,
) map[string]map[string]bool {
	follow := map[string]map[string]bool{}
	for _, p := range grammar.Productions {
		follow[p.Name] = map[string]bool{}
	}
	// End-of-source can follow the start rule.
	if f, ok := follow[start]; ok {
		f["#ZZ"] = true
	}

	for changed := true; changed; {
		changed = false
		for _, prod := range grammar.Productions {
			prodFollow := follow[prod.Name]
			for _, alt := range prod.Alts {
				for i, el := range alt {
					if el.Kind != KindRef {
						continue
					}
					target, ok := follow[el.Name]
					if !ok {
						continue
					}
					add := func(tok string) {
						if !target[tok] {
							target[tok] = true
							changed = true
						}
					}
					rest, restNullable := firstOfSeq(
						alt[i+1:], literals, regexTokens, firstSets, nullable)
					for tok := range rest {
						add(tok)
					}
					// Nothing (or nothing mandatory) follows this
					// reference inside the alternative, so whatever can
					// follow the enclosing production can follow the
					// reference too.
					if restNullable {
						for tok := range prodFollow {
							add(tok)
						}
					}
				}
			}
		}
	}

	return follow
}

// computeFollowPairs returns, per production R, the pairs (t, u) such
// that R may be followed by token t and then token u.
//
// This exists for exactly one decision the 1-token FOLLOW guard cannot
// make: a repetition whose repeated element COVERS a follow token at
// the character level. `ws = *[ \t\n]` followed by the literal "\n" is
// the canonical case — at a newline, continuing the loop and exiting
// are both locally viable, and which is right depends on what comes
// AFTER the newline. The pair (t, u) is what lets the emitter write
// that decision down as an ordered guard.
//
// Deliberately approximate, in ONE direction only: pairs whose t would
// come from inside a following REFERENCE are not collected. Missing a
// pair costs a guard that is never emitted (the grammar behaves as it
// does today); inventing one would emit a guard that accepts input the
// grammar does not describe.
func computeFollowPairs(grammar *Grammar, literals, regexTokens map[string]string,
	firstSets map[string]map[string]bool, nullable map[string]bool,
	follow map[string]map[string]bool,
) map[string]map[string]map[string]bool {
	pairs := map[string]map[string]map[string]bool{}
	for _, p := range grammar.Productions {
		pairs[p.Name] = map[string]map[string]bool{}
	}

	tokOf := func(el *Element) (string, bool) {
		switch el.Kind {
		case KindTerm, KindToken, KindRegex:
			t := tokenForTerminal(el, literals, regexTokens)
			return t, t != ""
		}
		return "", false
	}

	addPair := func(r, t, u string) bool {
		m, ok := pairs[r]
		if !ok {
			return false
		}
		us := m[t]
		if us == nil {
			us = map[string]bool{}
			m[t] = us
		}
		if us[u] {
			return false
		}
		us[u] = true
		return true
	}

	for changed := true; changed; {
		changed = false
		for _, prod := range grammar.Productions {
			prodFollow := follow[prod.Name]
			prodPairs := pairs[prod.Name]
			for _, alt := range prod.Alts {
				for i, el := range alt {
					if el.Kind != KindRef {
						continue
					}
					if _, ok := pairs[el.Name]; !ok {
						continue
					}

					blocked := false
					for j := i + 1; j < len(alt); j++ {
						ej := alt[j]
						if ej.Kind == KindRef {
							// A following reference blocks the walk
							// unless it can vanish; pairs starting
							// inside it are not collected.
							if nullable[ej.Name] {
								continue
							}
							blocked = true
							break
						}
						t, ok := tokOf(ej)
						if !ok {
							blocked = true
							break
						}
						rest, restNullable := firstOfSeq(
							alt[j+1:], literals, regexTokens, firstSets, nullable)
						for u := range rest {
							if addPair(el.Name, t, u) {
								changed = true
							}
						}
						if restNullable {
							for u := range prodFollow {
								if addPair(el.Name, t, u) {
									changed = true
								}
							}
						}
						blocked = true
						break
					}

					if !blocked {
						// Nothing mandatory follows the reference, so
						// the enclosing production's pairs follow it too.
						for t, us := range prodPairs {
							for u := range us {
								if addPair(el.Name, t, u) {
									changed = true
								}
							}
						}
					}
				}
			}
		}
	}

	return pairs
}
