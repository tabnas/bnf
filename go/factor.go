package bnf

// factor.go — left factoring.
//
// tabnas alternates are first-match-wins: once an alternative's first
// token matches, the engine commits to it. Two alternatives sharing a
// non-trivial prefix (`stmt = ident SP "=" … / ident SP "(" …`) can
// therefore never both be reachable — the first wins the shared prefix
// and fails where the second would have succeeded, and no finite token
// lookahead can separate them, because the shared prefix has unbounded
// token length. The classical fix is mechanical: factor the prefix out
// and defer the choice to a helper that dispatches on the first token
// AFTER the prefix, where the alternatives really differ.
//
//	P = α β1 / α β2   ⇒   P = α P$factN ; P$factN = β1 / β2
//
// Ported from ts/src/compiler.ts (leftFactor, factorOnce, inlineHeadRef
// and their helpers), which is canonical.

import (
	"math"
)

// lookaheadKSpan is the dispatcher's concrete-token lookahead. A shared
// prefix shorter than this is already separated by dispatch and is left
// alone; see factorOnce.
const lookaheadKSpan = 4

// elemEqual reports structural equality of two IR elements.
func elemEqual(a, b *Element) bool {
	if a == nil || b == nil || a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case KindTerm:
		return termKey(a) == termKey(b)
	case KindRef, KindToken:
		return a.Name == b.Name
	case KindRegex:
		return a.Pattern == b.Pattern && a.Flags == b.Flags
	case KindProse:
		// Never equal: prose is unresolved text, and treating two
		// unresolved terminals as the same element would factor across
		// a difference nothing here can see.
		return false
	case KindOpt, KindStar, KindPlus:
		return elemEqual(a.Inner, b.Inner)
	case KindRep:
		return a.Min == b.Min && a.Max == b.Max && elemEqual(a.Inner, b.Inner)
	case KindGroup:
		if len(a.Alts) != len(b.Alts) {
			return false
		}
		for i := range a.Alts {
			if !seqEqual(a.Alts[i], b.Alts[i]) {
				return false
			}
		}
		return true
	}
	return false
}

func seqEqual(a, b Sequence) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !elemEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

// unwrapAlt sees through a single-alternative group: `( a b )` as an
// entire alternative is just `a b`. A multi-alternative group is a real
// alternation and stays opaque.
func unwrapAlt(alt Sequence) Sequence {
	a := alt
	for len(a) == 1 && a[0].Kind == KindGroup && len(a[0].Alts) == 1 {
		a = a[0].Alts[0]
	}
	return a
}

// seqTokenSpan is the most tokens a sequence can span, for deciding
// whether the dispatcher's K-token lookahead can see past it. It runs
// on the RAW IR — left factoring precedes desugaring, so the sugar
// kinds are still here and each has to be counted on its own terms.
// Returns +Inf for anything unbounded (a star/plus, an unbounded rep, a
// cycle) or unknown (prose), which is what makes factoring the only
// remedy.
//
// This must NOT be approximated by asking altPrefixesRaw: that walker
// treats every element it does not recognise as a truncation, and
// before desugar it recognises none of the sugar — so a bounded `("a")`
// or `["-"]` would read as unbounded and get factored, collapsing
// alternatives the dispatcher separates perfectly well (and, with
// marks enabled, silently merging their user-visible marks).
func seqTokenSpan(seq Sequence, grammar *Grammar, visited map[string]bool) float64 {
	total := 0.0
	for _, el := range seq {
		total += elementTokenSpan(el, grammar, visited)
		if float64(lookaheadKSpan) < total {
			return math.Inf(1)
		}
	}
	return total
}

func elementTokenSpan(el *Element, grammar *Grammar, visited map[string]bool) float64 {
	switch el.Kind {
	case KindTerm, KindRegex, KindToken:
		return 1
	case KindProse:
		return math.Inf(1)
	case KindOpt:
		// Absent contributes 0, present contributes the inner span; the
		// MOST it can span is the inner span.
		return elementTokenSpan(el.Inner, grammar, visited)
	case KindStar, KindPlus:
		return math.Inf(1)
	case KindRep:
		if el.Max == MaxInfinity {
			return math.Inf(1)
		}
		return float64(el.Max) * elementTokenSpan(el.Inner, grammar, visited)
	case KindGroup:
		most := 0.0
		for _, alt := range el.Alts {
			if n := seqTokenSpan(alt, grammar, visited); most < n {
				most = n
			}
		}
		return most
	case KindRef:
		if visited[el.Name] {
			return math.Inf(1)
		}
		target := findProd(grammar, el.Name)
		if target == nil || len(target.Alts) == 0 {
			return math.Inf(1)
		}
		sub := map[string]bool{}
		for k := range visited {
			sub[k] = true
		}
		sub[el.Name] = true
		most := 0.0
		for _, alt := range target.Alts {
			if n := seqTokenSpan(alt, grammar, sub); most < n {
				most = n
			}
		}
		return most
	}
	return math.Inf(1)
}

// firstCharRangesOfElement is the first-character coverage of an
// element, from the raw IR (token allocation has not happened when left
// factoring runs). Returns nil when coverage cannot be established — a
// nullable element leaks its FOLLOW, a cycle or unparseable regex is
// unknown — and the caller treats nil as "not provably disjoint".
func firstCharRangesOfElement(el *Element, grammar *Grammar, visited map[string]bool) []charRange {
	switch el.Kind {
	case KindTerm:
		r := []rune(el.Literal)
		if len(r) == 0 {
			return nil
		}
		cp := r[0]
		if !isEffectivelyCaseSensitive(el) {
			lo := toLowerRune(cp)
			up := toUpperRune(cp)
			if lo == up {
				return []charRange{{cp, cp}}
			}
			return []charRange{{lo, lo}, {up, up}}
		}
		return []charRange{{cp, cp}}
	case KindRegex:
		return patternCharRanges(el.Pattern)
	case KindRef:
		if visited[el.Name] {
			return nil
		}
		target := findProd(grammar, el.Name)
		if target == nil || len(target.Alts) == 0 {
			return nil
		}
		sub := map[string]bool{}
		for k := range visited {
			sub[k] = true
		}
		sub[el.Name] = true
		out := []charRange{}
		for _, alt := range target.Alts {
			if len(alt) == 0 {
				return nil
			}
			r := firstCharRangesOfElement(alt[0], grammar, sub)
			if r == nil {
				return nil
			}
			out = append(out, r...)
		}
		return out
	case KindGroup:
		out := []charRange{}
		for _, alt := range el.Alts {
			if len(alt) == 0 {
				return nil
			}
			r := firstCharRangesOfElement(alt[0], grammar, visited)
			if r == nil {
				return nil
			}
			out = append(out, r...)
		}
		return out
	case KindPlus:
		return firstCharRangesOfElement(el.Inner, grammar, visited)
	case KindRep:
		if 0 < el.Min {
			return firstCharRangesOfElement(el.Inner, grammar, visited)
		}
		return nil
	}
	// opt, star, token, prose: no established first-character coverage.
	return nil
}

func toLowerRune(r rune) rune {
	if 'A' <= r && r <= 'Z' {
		return r + 32
	}
	return r
}

func toUpperRune(r rune) rune {
	if 'a' <= r && r <= 'z' {
		return r - 32
	}
	return r
}

// leftFactor factors alternatives that share a leading element prefix.
//
// Only CONSECUTIVE alternatives merge: folding a later alternative over
// an intervening one would promote it in first-match order, which is
// observable whenever the intervening alternative overlaps. The helper
// is a transparent 'helper' node, so factoring never changes the
// emitted tree; a helper whose tails include the empty sequence is
// flagged RepeatHelper so its empty alternative gets the same FOLLOW
// guards a repetition's terminator does.
func leftFactor(grammar *Grammar) *Grammar {
	used := map[string]bool{}
	for _, p := range grammar.Productions {
		used[p.Name] = true
	}

	freshName := func(base string) string {
		for i := 0; ; i++ {
			name := base + "$fact" + itoa(i)
			if !used[name] {
				used[name] = true
				return name
			}
		}
	}

	out := []*Production{}
	queue := append([]*Production{}, grammar.Productions...)

	for len(queue) > 0 {
		prod := queue[0]
		queue = queue[1:]

		if prod.ProbeDisp != nil || prod.ProbeHelper != nil || prod.TailRepeat != nil {
			out = append(out, prod)
			continue
		}

		alts := prod.Alts
		for {
			next, more := factorOnce(
				prod.Name, originOf(prod), alts, freshName, &queue, grammar)
			if !more {
				break
			}
			alts = next
		}

		if sameAlts(alts, prod.Alts) {
			out = append(out, prod)
			continue
		}
		cp := *prod
		cp.Alts = alts
		out = append(out, &cp)
	}

	return &Grammar{Productions: out}
}

func sameAlts(a, b []Sequence) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !seqEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

// factorOnce performs one factoring step over one production's
// alternatives: find the first consecutive run sharing a leading
// element, replace it with a single factored alternative, and queue the
// tail helper (so it is itself factored in turn). Reports false when
// nothing shares a prefix.
//
// Only prefixes the dispatcher cannot see past are factored: the
// emitter separates competing alternatives with up to lookaheadK
// concrete-token prefixes, so a short bounded shared prefix (`"a" X /
// "a" Y`) is already handled — and left alone, which also preserves the
// per-alternative identity that collision marks and tree tests depend
// on. A prefix that can span lookaheadK tokens or has unbounded token
// length (`identifier ws "=" … / identifier ws "(" …`) is beyond any
// finite lookahead, and factoring is the only fix.
func factorOnce(prodName, prodOrigin string, alts []Sequence,
	freshName func(string) string,
	queue *[]*Production, grammar *Grammar) ([]Sequence, bool) {

	views := make([]Sequence, len(alts))
	for i, a := range alts {
		views[i] = unwrapAlt(a)
	}

	for i := 0; i < len(alts)-1; i++ {
		if len(views[i]) == 0 {
			continue
		}
		headEl := views[i][0]

		// Gather run members. A later alternative joins the run when its
		// first element equals the head — directly, or after inlining a
		// single-alternative rule it starts with (`funcCall` in `factor
		// = identifier / … / funcCall`, where `funcCall = identifier "("
		// …`). Alternatives BETWEEN members are skipped over only when
		// their first characters are provably disjoint from the head's,
		// so promoting a member across them cannot change which
		// alternative wins any input.
		members := []int{i}
		memberViews := map[int]Sequence{i: views[i]}
		var headRanges []charRange
		headRangesDone := false

		for j := i + 1; j < len(alts); j++ {
			v := views[j]
			if len(v) > 0 && elemEqual(headEl, v[0]) {
				members = append(members, j)
				memberViews[j] = v
				continue
			}
			if len(v) > 0 {
				if inlined := inlineHeadRef(v, headEl, grammar); inlined != nil {
					members = append(members, j)
					memberViews[j] = inlined
					continue
				}
			}
			// Not a member — skippable only if provably disjoint.
			if !headRangesDone {
				headRanges = normalizeRanges(
					firstCharRangesOfElement(headEl, grammar, map[string]bool{}))
				headRangesDone = true
			}
			if headRanges == nil || len(v) == 0 {
				break
			}
			r := normalizeRanges(firstCharRangesOfElement(v[0], grammar, map[string]bool{}))
			if r == nil || charRangesOverlap(headRanges, r) {
				break
			}
		}
		if len(members) < 2 {
			continue
		}

		run := make([]Sequence, len(members))
		for k, m := range members {
			run[k] = memberViews[m]
		}

		// Longest element-wise common prefix of the run.
		plen := 1
		for {
			done := false
			for _, v := range run {
				if len(v) <= plen || !elemEqual(run[0][plen], v[plen]) {
					done = true
					break
				}
			}
			if done {
				break
			}
			plen++
		}

		prefix := append(Sequence{}, run[0][:plen]...)
		if float64(lookaheadKSpan) >= seqTokenSpan(prefix, grammar, map[string]bool{}) {
			// Dispatch lookahead already separates these — leave them be.
			continue
		}

		// Structurally duplicate tails collapse — a duplicated
		// alternative can never win over its first copy under
		// first-match-wins.
		tails := []Sequence{}
		for _, v := range run {
			tail := append(Sequence{}, v[plen:]...)
			dup := false
			for _, t := range tails {
				if seqEqual(t, tail) {
					dup = true
					break
				}
			}
			if !dup {
				tails = append(tails, tail)
			}
		}
		// Empty tail LAST: it matches anything, so the longer
		// continuations must be offered first. A stable partition, not a
		// sort — the relative order of the non-empty tails is the
		// grammar's own and is observable.
		ordered := []Sequence{}
		for _, t := range tails {
			if len(t) > 0 {
				ordered = append(ordered, t)
			}
		}
		hasEmpty := false
		for _, t := range tails {
			if len(t) == 0 {
				hasEmpty = true
			}
		}
		if hasEmpty {
			ordered = append(ordered, Sequence{})
		}
		tails = ordered

		var factored Sequence
		if len(tails) == 1 {
			// All run members were structurally identical.
			factored = append(append(Sequence{}, prefix...), tails[0]...)
		} else {
			helper := freshName(prodName)
			helperProd := &Production{
				Name:     helper,
				Alts:     tails,
				NodeKind: "helper",
				Origin:   prodOrigin,
			}
			if hasEmpty {
				helperProd.RepeatHelper = true
			}
			*queue = append(*queue, helperProd)
			factored = append(append(Sequence{}, prefix...),
				&Element{Kind: KindRef, Name: helper})
		}

		// Preserve the enclosing shape: when every run member arrived
		// wrapped in its own single-alt group, keep the factored
		// alternative wrapped too, so a production whose alternatives
		// were all simple group refs stays that way for the emitter.
		wrapped := true
		for _, m := range members {
			if len(alts[m]) != 1 || alts[m][0].Kind != KindGroup {
				wrapped = false
				break
			}
		}
		replacement := factored
		if wrapped {
			replacement = Sequence{{Kind: KindGroup, Alts: []Sequence{factored}}}
		}

		removed := map[int]bool{}
		for _, m := range members[1:] {
			removed[m] = true
		}
		out := []Sequence{}
		for k := 0; k < len(alts); k++ {
			if removed[k] {
				continue
			}
			if k == i {
				out = append(out, replacement)
				continue
			}
			out = append(out, alts[k])
		}
		return out, true
	}

	return nil, false
}

// inlineHeadRef: if `v` starts with a ref to a single-alternative
// production whose body's first element equals headEl, return `v` with
// the ref replaced by that body. One level deep, and never through the
// synthetic production kinds. Returns nil when the shape does not apply.
func inlineHeadRef(v Sequence, headEl *Element, grammar *Grammar) Sequence {
	h := v[0]
	if h.Kind != KindRef {
		return nil
	}
	target := findProd(grammar, h.Name)
	if target == nil || len(target.Alts) != 1 {
		return nil
	}
	if target.ProbeDisp != nil || target.ProbeHelper != nil || target.TailRepeat != nil {
		return nil
	}
	body := unwrapAlt(target.Alts[0])
	if len(body) == 0 || !elemEqual(body[0], headEl) {
		return nil
	}
	return append(append(Sequence{}, body...), v[1:]...)
}
