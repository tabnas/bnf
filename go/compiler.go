// Copyright (c) 2026 tabnas, MIT License

package bnf

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

func refsIn(alt Sequence, out map[string]bool) {
	for _, el := range alt {
		switch el.Kind {
		case KindRef:
			out[el.Name] = true
		case KindOpt, KindStar, KindPlus, KindRep:
			refsIn(Sequence{el.Inner}, out)
		case KindGroup:
			for _, a := range el.Alts {
				refsIn(a, out)
			}
		}
	}
}

// cloneGrammar copies a grammar deeply enough that the emit pipeline cannot
// disturb the caller's AST. The passes replace Productions, Alts and the
// individual sequences, but treat elements as immutable (each rewriting walk
// returns fresh elements), so sharing elements is safe.
func cloneGrammar(grammar *Grammar) *Grammar {
	prods := make([]*Production, len(grammar.Productions))
	for i, p := range grammar.Productions {
		alts := make([]Sequence, len(p.Alts))
		for j, a := range p.Alts {
			alts[j] = append(Sequence{}, a...)
		}
		cp := *p
		cp.Alts = alts
		prods[i] = &cp
	}
	return &Grammar{Productions: prods}
}

// ---- left-recursion elimination (Paull's) --------------------------

func eliminateLeftRecursion(grammar *Grammar) *Grammar {
	originalOrder := make([]string, len(grammar.Productions))
	for i, p := range grammar.Productions {
		originalOrder[i] = p.Name
	}

	// Copy productions (shallow-copy alts) before reordering.
	copies := make([]*Production, len(grammar.Productions))
	for i, p := range grammar.Productions {
		alts := make([]Sequence, len(p.Alts))
		for j, a := range p.Alts {
			alts[j] = append(Sequence{}, a...)
		}
		copies[i] = &Production{Name: p.Name, Alts: alts, NodeKind: p.NodeKind}
	}
	prods := topoOrderForPaull(copies)

	// Substitution normally runs for every production, even a cycle-free one.
	// That is pragmatic rather than theoretical: the multi-token altPrefixes
	// used to populate tcol (so the lexer's regex matchers fire with the right
	// tin in nested contexts) are read off the fully inlined shape, and a rule
	// choosing between several alternatives needs that lookahead to dispatch.
	// Scoping substitution to the cyclic SCCs in general therefore has to wait
	// for tcol to be computed from the un-substituted grammar.
	//
	// One case is safe to exempt today, and it is the one that visibly mangles
	// a grammar: a *pure alias*, a production whose single alternative is a
	// single rule reference (`val = add`). Inlining it rewrites `val = add`
	// into `val = NR [ PL add ]` — the alias name is dissolved, so the rule
	// vanishes from the emitted AST and the grammar no longer renders back to
	// the ABNF it was written in. Because such a production has exactly one
	// alternative, it has nothing to dispatch between: it unconditionally
	// pushes its target, needs no lookahead, and so cannot depend on the
	// inlined prefixes. Aliases caught up in a leading-reference cycle are
	// still inlined — that is where Paull's substitution is doing real work
	// (`P = Q`, `Q = P a / b`).
	cyclic := findLeadingRefCycleMembers(prods)
	isExemptAlias := func(p *Production) bool {
		return len(p.Alts) == 1 && len(p.Alts[0]) == 1 &&
			p.Alts[0][0].Kind == KindRef &&
			!cyclic[p.Name] && !cyclic[p.Alts[0][0].Name]
	}

	// Paull's invariant is that after the inner loop no alternative of A_i
	// begins with a ref to any A_j, j < i. A single increasing pass gives that
	// only when every A_j has itself been fully substituted — but the pure-alias
	// exemption above deliberately leaves some A_j un-substituted, so inlining
	// such an alias can (re)introduce a leading ref to an A_k with k < j, which
	// the pass has already walked past. Left in place, a nullable A_k hides the
	// left recursion from eliminateDirectLeftRec and the emitted grammar
	// re-enters A_i at the same source position (`a = b a / "x"`, `b = c`,
	// `c = "y" /`). So re-run the pass until it reaches a fixed point.
	//
	// Termination: each round only fires where a leading ref to an earlier
	// production remains, and earlier productions have already had their own
	// direct left recursion eliminated, so no substitution can reproduce the ref
	// it just consumed. The guard is belt-and-braces against a pathological
	// grammar. Mirrors the TS eliminateLeftRecursion.
	for i := 0; i < len(prods); i++ {
		if !isExemptAlias(prods[i]) {
			guard := len(prods) + 1
			for round := 0; round < guard; round++ {
				changed := false
				for j := 0; j < i; j++ {
					if !hasLeadingRefTo(prods[i], prods[j].Name) {
						continue
					}
					prods[i] = substituteLeadingRef(prods[i], prods[j])
					changed = true
				}
				if !changed {
					break
				}
			}
		}
		prods[i] = eliminateDirectLeftRec(prods[i])
	}

	byName := map[string]*Production{}
	for _, p := range prods {
		byName[p.Name] = p
	}
	ordered := []*Production{}
	for _, name := range originalOrder {
		if p, ok := byName[name]; ok {
			ordered = append(ordered, p)
			delete(byName, name)
		}
	}
	// Any remaining (generated) productions, in stable order.
	remaining := []string{}
	for name := range byName {
		remaining = append(remaining, name)
	}
	sort.Strings(remaining)
	for _, name := range remaining {
		ordered = append(ordered, byName[name])
	}
	return &Grammar{Productions: ordered}
}

// findLeadingRefCycleMembers is a Tarjan-flavoured SCC scan over the
// leading-reference graph: it returns the names of productions that
// participate in at least one cycle (self-loop or longer). Used to scope
// Paull's substitution to only the rules that actually need it. Go port of the
// TS findLeadingRefCycleMembers.
func findLeadingRefCycleMembers(prods []*Production) map[string]bool {
	byName := map[string]*Production{}
	for _, p := range prods {
		byName[p.Name] = p
	}
	leadingRefs := func(p *Production) []string {
		out := []string{}
		for _, alt := range p.Alts {
			if len(alt) == 0 {
				continue
			}
			if first := alt[0]; first.Kind == KindRef {
				if _, ok := byName[first.Name]; ok {
					out = append(out, first.Name)
				}
			}
		}
		return out
	}

	index := 0
	stack := []string{}
	onStack := map[string]bool{}
	indices := map[string]int{}
	lowlinks := map[string]int{}
	cyclic := map[string]bool{}

	var strongConnect func(name string)
	strongConnect = func(name string) {
		indices[name] = index
		lowlinks[name] = index
		index++
		stack = append(stack, name)
		onStack[name] = true

		if prod := byName[name]; prod != nil {
			for _, target := range leadingRefs(prod) {
				if _, seen := indices[target]; !seen {
					strongConnect(target)
					if lowlinks[target] < lowlinks[name] {
						lowlinks[name] = lowlinks[target]
					}
				} else if onStack[target] {
					if indices[target] < lowlinks[name] {
						lowlinks[name] = indices[target]
					}
				}
			}
		}

		if lowlinks[name] == indices[name] {
			// Pop the SCC. More than one member, or one member with a
			// self-loop, means a cycle.
			scc := []string{}
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				delete(onStack, w)
				scc = append(scc, w)
				if w == name {
					break
				}
			}
			isCycle := len(scc) > 1
			if len(scc) == 1 {
				for _, t := range leadingRefs(byName[scc[0]]) {
					if t == scc[0] {
						isCycle = true
					}
				}
			}
			if isCycle {
				for _, n := range scc {
					cyclic[n] = true
				}
			}
		}
	}

	for _, p := range prods {
		if _, seen := indices[p.Name]; !seen {
			strongConnect(p.Name)
		}
	}
	return cyclic
}

// topoOrderForPaull orders over the leading-position reference graph.
func topoOrderForPaull(prods []*Production) []*Production {
	byName := map[string]*Production{}
	for _, p := range prods {
		byName[p.Name] = p
	}
	colour := map[string]int{} // 0 unseen, 1 in-progress, 2 done
	order := []*Production{}
	var visit func(name string)
	visit = func(name string) {
		if colour[name] != 0 {
			return
		}
		colour[name] = 1
		p := byName[name]
		if p != nil {
			for _, alt := range p.Alts {
				if len(alt) > 0 && alt[0].Kind == KindRef {
					if _, ok := byName[alt[0].Name]; ok {
						visit(alt[0].Name)
					}
				}
			}
			colour[name] = 2
			order = append(order, p)
		} else {
			colour[name] = 2
		}
	}
	for _, p := range prods {
		visit(p.Name)
	}
	return order
}

// hasLeadingRefTo reports whether at least one alternative of prod begins with
// a reference to name — i.e. substituteLeadingRef would change it. Go port of
// the TS hasLeadingRefTo.
func hasLeadingRefTo(prod *Production, name string) bool {
	for _, alt := range prod.Alts {
		if len(alt) > 0 && alt[0].Kind == KindRef && alt[0].Name == name {
			return true
		}
	}
	return false
}

func substituteLeadingRef(target, source *Production) *Production {
	newAlts := []Sequence{}
	for _, alt := range target.Alts {
		if len(alt) > 0 && alt[0].Kind == KindRef && alt[0].Name == source.Name {
			tail := append(Sequence{}, alt[1:]...)
			for _, srcAlt := range source.Alts {
				combined := append(append(Sequence{}, srcAlt...), tail...)
				newAlts = append(newAlts, combined)
			}
		} else {
			newAlts = append(newAlts, alt)
		}
	}
	return &Production{Name: target.Name, Alts: newAlts, NodeKind: target.NodeKind}
}

func eliminateDirectLeftRec(prod *Production) *Production {
	recursive := []Sequence{}
	seeds := []Sequence{}
	for _, alt := range prod.Alts {
		if len(alt) > 0 && alt[0].Kind == KindRef && alt[0].Name == prod.Name {
			recursive = append(recursive, append(Sequence{}, alt[1:]...))
		} else {
			seeds = append(seeds, alt)
		}
	}
	nonTrivial := []Sequence{}
	for _, t := range recursive {
		if len(t) > 0 {
			nonTrivial = append(nonTrivial, t)
		}
	}
	if len(nonTrivial) == 0 {
		return &Production{Name: prod.Name, Alts: seeds, NodeKind: prod.NodeKind}
	}
	if len(seeds) == 0 {
		panic(fmt.Sprintf(diagName()+": rule '%s' is purely left-recursive "+
			"(no seed alternative); cannot eliminate", prod.Name))
	}

	var seedElement *Element
	if len(seeds) == 1 && len(seeds[0]) == 1 {
		seedElement = seeds[0][0]
	} else {
		seedElement = &Element{Kind: KindGroup, Alts: seeds}
	}
	var tailInner *Element
	if len(nonTrivial) == 1 && len(nonTrivial[0]) == 1 {
		tailInner = nonTrivial[0][0]
	} else {
		tailInner = &Element{Kind: KindGroup, Alts: nonTrivial}
	}
	return &Production{
		Name:     prod.Name,
		Alts:     []Sequence{{seedElement, {Kind: KindStar, Inner: tailInner}}},
		NodeKind: prod.NodeKind,
	}
}

// ---- desugar -------------------------------------------------------

// rewriteTailRepeats rewrites tail self-references into same-depth
// repeats:
//
//	X = prefix [ sep X ]
//
// compiles naturally to a rule that repeats itself (`r: X`) from its
// close phase — the form a hand-written tabnas grammar uses — rather
// than to an optional-group helper chain that re-pushes X. The repeat
// keeps every iteration at one stack depth with the SAME parent, which
// is what makes r.Parent.Node usable from user actions, and flattens
// the tree. Mirrors the TS `rewriteTailRepeats`; the guards MUST stay
// identical (the alignment TSVs pin the emitted shape cross-engine).
func rewriteTailRepeats(grammar *Grammar, start string) *Grammar {
	isTerminal := func(el *Element) bool {
		return el.Kind == KindTerm || el.Kind == KindToken || el.Kind == KindRegex
	}
	allTerminal := func(seq Sequence) bool {
		for _, el := range seq {
			if !isTerminal(el) {
				return false
			}
		}
		return true
	}

	for _, prod := range grammar.Productions {
		if prod.ProbeDisp != nil || prod.ProbeHelper != nil {
			continue
		}
		if prod.Name == start {
			continue
		}
		if len(prod.Alts) != 1 {
			continue
		}
		alt := prod.Alts[0]
		if len(alt) < 2 {
			continue
		}
		last := alt[len(alt)-1]
		if last.Kind != KindOpt {
			continue
		}

		// Normalize the option body to a sequence: `[ a b ]` parses as
		// opt(group([[a, b]])); `[ a ]` as opt(a).
		var seq Sequence
		if last.Inner.Kind == KindGroup {
			if len(last.Inner.Alts) != 1 {
				continue
			}
			seq = last.Inner.Alts[0]
		} else {
			seq = Sequence{last.Inner}
		}
		if len(seq) < 2 { // need at least one separator + the self-ref
			continue
		}

		tail := seq[len(seq)-1]
		if tail.Kind != KindRef || tail.Name != prod.Name {
			continue
		}
		sep := seq[:len(seq)-1]
		if !allTerminal(sep) {
			continue
		}

		prefix := alt[:len(alt)-1]
		if len(prefix) == 0 || !allTerminal(prefix) {
			continue
		}

		prod.Alts = []Sequence{prefix}
		prod.TailRepeat = &TailRepeatSpec{Sep: sep}
	}
	return grammar
}

func desugar(grammar *Grammar) *Grammar {
	extra := []*Production{}
	used := map[string]bool{}
	for _, p := range grammar.Productions {
		used[p.Name] = true
	}
	freshName := func(hint string) string {
		i := len(extra)
		var name string
		for {
			i++
			name = fmt.Sprintf("_gen%d_%s", i, hint)
			if !used[name] {
				break
			}
		}
		used[name] = true
		return name
	}

	var desugarElement func(el *Element) *Element
	desugarAlt := func(alt Sequence) Sequence {
		out := make(Sequence, len(alt))
		for i, el := range alt {
			out[i] = desugarElement(el)
		}
		return out
	}
	desugarElement = func(el *Element) *Element {
		switch el.Kind {
		case KindTerm, KindRef, KindRegex, KindToken:
			return el
		case KindProse:
			// Unreachable: resolveProseTerminals drops every prose element
			// (or errors) before desugaring runs.
			panic(fmt.Sprintf(diagName()+": internal: unresolved prose terminal '<%s>'", el.Text))
		case KindGroup:
			innerAlts := make([]Sequence, len(el.Alts))
			for i, a := range el.Alts {
				innerAlts[i] = desugarAlt(a)
			}
			name := freshName("group")
			extra = append(extra, &Production{Name: name, Alts: innerAlts, NodeKind: "helper"})
			return &Element{Kind: KindRef, Name: name}
		}

		inner := desugarElement(el.Inner)
		// Name the generated helper after what it repeats. A literal lifted
		// from a named production (`PL = "+"`) carries that name, and a
		// builtin token carries its own, so `*PL` still yields
		// `_genN_star_PL` rather than an anonymous `_genN_star_term`.
		hint := "x"
		if inner.Kind == KindRef {
			hint = inner.Name
		} else if inner.Kind == KindTerm {
			hint = "term"
			if inner.TokenName != "" {
				hint = inner.TokenName
			}
		} else if inner.Kind == KindToken {
			hint = strings.TrimPrefix(inner.Name, "#")
		}

		switch el.Kind {
		case KindOpt:
			name := freshName("opt_" + hint)
			extra = append(extra, &Production{
				Name: name, Alts: []Sequence{{inner}, {}}, NodeKind: "helper"})
			return &Element{Kind: KindRef, Name: name}
		case KindStar:
			name := freshName("star_" + hint)
			selfRef := &Element{Kind: KindRef, Name: name}
			extra = append(extra, &Production{
				Name: name, Alts: []Sequence{{inner, selfRef}, {}}, NodeKind: "helper"})
			return &Element{Kind: KindRef, Name: name}
		case KindPlus:
			tailName := freshName("star_" + hint)
			plusName := freshName("plus_" + hint)
			tailRef := &Element{Kind: KindRef, Name: tailName}
			extra = append(extra, &Production{
				Name: tailName, Alts: []Sequence{{inner, tailRef}, {}}, NodeKind: "helper"})
			extra = append(extra, &Production{
				Name: plusName, Alts: []Sequence{{inner, tailRef}}, NodeKind: "helper"})
			return &Element{Kind: KindRef, Name: plusName}
		}

		// KindRep — bounded m*n.
		min, max := el.Min, el.Max
		repName := freshName("rep_" + hint)
		repAlt := Sequence{}
		for i := 0; i < min; i++ {
			repAlt = append(repAlt, inner)
		}
		if max == MaxInfinity {
			tailStarName := freshName("star_" + hint)
			tailStarRef := &Element{Kind: KindRef, Name: tailStarName}
			extra = append(extra, &Production{
				Name: tailStarName, Alts: []Sequence{{inner, tailStarRef}, {}}, NodeKind: "helper"})
			repAlt = append(repAlt, tailStarRef)
		} else {
			// Nest (max - min) optionals: [A [A [A ...]]].
			//
			// Built bottom-up as an explicit chain of helper productions
			// rather than as one deeply-nested inline element tree. The shape
			// (and every generated name) is identical either way, but handing
			// a nested tree to desugarAlt costs a stack frame per repetition,
			// and real ABNF carries big bounds — RFC 5322's
			// `body = (*(*998text CRLF) *998text)`. Mirrors the TS desugar.
			//
			// Each level is exactly what desugarElement would have emitted for
			// opt(group([[inner, <previous level>]])): the group helper first,
			// then the optional wrapping a reference to it. Pushing in that
			// order keeps freshName's numbering unchanged.
			var nestedRef *Element
			for i := 0; i < max-min; i++ {
				seq := Sequence{inner}
				if nestedRef != nil {
					seq = Sequence{inner, nestedRef}
				}
				groupName := freshName("group")
				extra = append(extra, &Production{
					Name: groupName, Alts: []Sequence{seq}, NodeKind: "helper"})
				groupRef := &Element{Kind: KindRef, Name: groupName}
				optName := freshName("opt_" + groupName)
				extra = append(extra, &Production{
					Name: optName,
					Alts: []Sequence{{groupRef}, {}}, NodeKind: "helper"})
				nestedRef = &Element{Kind: KindRef, Name: optName}
			}
			if nestedRef != nil {
				repAlt = append(repAlt, nestedRef)
			}
		}
		extra = append(extra, &Production{
			Name: repName, Alts: []Sequence{desugarAlt(repAlt)}, NodeKind: "helper"})
		return &Element{Kind: KindRef, Name: repName}
	}

	rewritten := []*Production{}
	for _, p := range grammar.Productions {
		out := &Production{Name: p.Name, NodeKind: p.NodeKind}
		alts := make([]Sequence, len(p.Alts))
		for i, a := range p.Alts {
			alts[i] = desugarAlt(a)
		}
		out.Alts = alts
		if p.ProbeDisp != nil {
			out.ProbeDisp = p.ProbeDisp
		}
		if p.ProbeHelper != nil {
			out.ProbeHelper = p.ProbeHelper
		}
		if p.TailRepeat != nil {
			out.TailRepeat = p.TailRepeat
		}
		rewritten = append(rewritten, out)
	}
	return &Grammar{Productions: append(rewritten, extra...)}
}

// ---- key / case-sensitivity helpers --------------------------------

var letterRe = regexp.MustCompile(`[A-Za-z]`)

func isEffectivelyCaseSensitive(el *Element) bool {
	if el.HasCaseSens && el.CaseSensitive {
		return true
	}
	return !letterRe.MatchString(el.Literal)
}

func termKey(el *Element) string {
	prefix := "ci:"
	if isEffectivelyCaseSensitive(el) {
		prefix = "cs:"
	}
	return prefix + el.Literal
}

func regexKey(el *Element) string {
	return "/" + el.Pattern + "/" + el.Flags
}

var nonIdentRe = regexp.MustCompile(`[^A-Za-z0-9]`)
var trimUnderscore = regexp.MustCompile(`^_+|_+$`)

func allocTokenName(literal string, used map[string]bool, preferred string) string {
	// A literal lifted from a named production (`PL = "+"`) keeps that name,
	// so the emitted grammar reads `PL` rather than `T`.
	if preferred != "" {
		want := "#" + preferred
		if !used[want] {
			used[want] = true
			return want
		}
	}
	base := nonIdentRe.ReplaceAllString(literal, "_")
	base = strings.ToUpper(base)
	base = trimUnderscore.ReplaceAllString(base, "")
	candidate := "#T"
	if len(base) > 0 {
		candidate = "#" + base
	}
	if !used[candidate] {
		used[candidate] = true
		return candidate
	}
	i := 1
	for used[candidate+strconv.Itoa(i)] {
		i++
	}
	chosen := candidate + strconv.Itoa(i)
	used[chosen] = true
	return chosen
}

// escapeRegexp mirrors the TS escapeRegExp for case-insensitive literal
// match tokens.
var escapeRe = regexp.MustCompile(`[\\^$.*+?()[\]{}|]`)

func escapeRegexp(s string) string {
	return escapeRe.ReplaceAllString(s, `\$0`)
}

// builtinTokens maps a bareword reference name to the engine builtin lexer
// token it resolves to (unless a rule of the same name is defined). Mirrors
// the TS BUILTIN_TOKENS: TX bareword/identifier, NR number, ST quoted string,
// VL keyword value (true/false/null).
var builtinTokens = map[string]string{
	"TX": "#TX",
	"NR": "#NR",
	"ST": "#ST",
	"VL": "#VL",
}

// builtinTokenOrder is builtinTokens in declaration order. Go map iteration is
// randomised, so anything user-visible that lists these names — currently the
// prose-val error message — must use this to stay byte-identical to the TS
// implementation.
var builtinTokenOrder = []string{"TX", "NR", "ST", "VL"}

// reservedTokenNames are the token names the engine itself owns: the standard
// lexer tokens plus the fixed tokens the default config installs. A lifted
// literal must never claim one of these — binding `#TX` to a literal would
// displace the lexer's text matcher rather than add a token — so a production
// so named stays an ordinary rule. Go port of the TS RESERVED_TOKEN_NAMES.
var reservedTokenNames = map[string]bool{
	"BD": true, "ZZ": true, "UK": true, "AA": true, "SP": true, "LN": true,
	"CM": true, "NR": true, "ST": true, "TX": true, "VL": true,
	"OB": true, "CB": true, "OS": true, "CS": true, "CL": true, "CA": true,
}

// resolveProseTerminals resolves RFC 5234 prose-val terminals (`<free text>`).
//
// Prose is informational by definition — RFC 5234 §4 calls it a "last resort"
// escape hatch for describing a terminal in English when no formal notation
// will do. There is nothing to compile, so the converter accepts it in exactly
// one position: as the *entire* body of a production naming a builtin lexer
// token.
//
//	NR = <number>
//
// That line documents, in the grammar text, that `NR` is the engine's number
// token. The lexer already supplies it, so the production is dropped here and
// every `NR` reference falls through to normalizeBuiltinTokens, which binds it
// to `#NR`. This is what lets a grammar state its terminals explicitly and
// still round-trip.
//
// Prose anywhere else has no definition behind it, so it is an error rather
// than a silently-ignored rule. Go port of the TS resolveProseTerminals.
// removeProse is the one prose directive the compiler acts on. Matched
// case-insensitively after trimming, so `<remove>`, `<Remove>` and
// `< remove >` all work.
const removeProse = "remove"

// removeAll is the one prose *name*: `<all> = <remove>` clears the whole
// grammar.
const removeAll = "all"

// isProseName reports whether a production name came from a prose token. Such
// a name keeps its angle brackets, which no ordinary rulename can contain — so
// `<all>` and a production actually called `all` stay distinct.
func isProseName(name string) bool {
	return strings.HasPrefix(name, "<") && strings.HasSuffix(name, ">")
}

func resolveProseTerminals(grammar *Grammar) error {
	kept := []*Production{}

	var findStray func(el *Element) *Element
	findStray = func(el *Element) *Element {
		switch el.Kind {
		case KindProse:
			return el
		case KindOpt, KindStar, KindPlus, KindRep:
			return findStray(el.Inner)
		case KindGroup:
			for _, alt := range el.Alts {
				for _, inner := range alt {
					if hit := findStray(inner); hit != nil {
						return hit
					}
				}
			}
		}
		return nil
	}

	for _, prod := range grammar.Productions {
		onlyProse := len(prod.Alts) == 1 && len(prod.Alts[0]) == 1 &&
			prod.Alts[0][0].Kind == KindProse

		// A prose name is only ever the removal directive. Checked here as
		// well as in the prose-body branch below, because `<all> = "x"` has a
		// *literal* body and would otherwise fall through and be lifted into a
		// token literally named `#<all>`.
		if isProseName(prod.Name) && !onlyProse {
			return &ParseError{Message: fmt.Sprintf(
				diagName()+": '%s' is prose, and prose is only valid as a production "+
					"name for the removal directive '<all> = <remove>'.", prod.Name)}
		}

		if onlyProse {
			text := prod.Alts[0][0].Text

			// `<remove>` — the one prose form that *does* compile to something.
			// Prose is otherwise informational, which is exactly why it is the
			// right place for a directive: it cannot collide with a real
			// terminal, and RFC 5234 already says a tool may interpret it.
			//
			//	name = <remove>   drop that rule and that fixed token
			//	<all> = <remove>  drop everything — a fresh empty grammar
			if removeProse == strings.ToLower(strings.TrimSpace(text)) {
				if isProseName(prod.Name) {
					target := strings.ToLower(strings.TrimSpace(
						prod.Name[1 : len(prod.Name)-1]))
					if removeAll != target {
						return &ParseError{Message: fmt.Sprintf(
							diagName()+": '<%s>' is not a removal target. The only prose "+
								"name is '<all>', as in '<all> = <remove>', which clears "+
								"the whole grammar. To remove one rule or token, name it "+
								"directly: '%s = <remove>'.", target, target)}
					}
					grammar.ClearAll = true
				} else {
					grammar.Remove = append(grammar.Remove, prod.Name)
				}
				continue
			}

			if isProseName(prod.Name) {
				return &ParseError{Message: fmt.Sprintf(
					diagName()+": '%s' is prose, and prose is only valid as a production "+
						"name for the removal directive '<all> = <remove>'.", prod.Name)}
			}

			if _, ok := builtinTokens[prod.Name]; ok {
				continue // informational — the lexer defines it
			}
			// builtinTokenOrder, not a sorted map walk: the message must
			// match the TS one byte for byte (ts/test/grammar/parity.json).
			names := builtinTokenOrder
			return &ParseError{Message: fmt.Sprintf(
				diagName()+": rule '%s' is defined only by prose ('<%s>'), which describes "+
					"a terminal but does not define one. Prose is allowed only for "+
					"built-in lexer tokens (%s).",
				prod.Name, prod.Alts[0][0].Text, strings.Join(names, ", "))}
		}

		// Any surviving prose is embedded in a larger expression, where it
		// cannot be given a meaning. Nested groups and repetitions are
		// searched too, so `x = ( <foo> / "a" )` reports the same clear
		// error as a top-level `x = "a" <foo>`.
		for _, alt := range prod.Alts {
			for _, el := range alt {
				if stray := findStray(el); stray != nil {
					return &ParseError{Message: fmt.Sprintf(
						diagName()+": rule '%s' uses prose ('<%s>') inside an expression; "+
							"prose may only stand alone as the whole definition of a "+
							"built-in lexer token.", prod.Name, stray.Text)}
				}
			}
		}
		kept = append(kept, prod)
	}

	// A grammar that is nothing but `<remove>` directives is legitimate: it
	// prunes an instance without adding anything back.
	if len(kept) == 0 && !grammar.ClearAll && len(grammar.Remove) == 0 {
		return &ParseError{Message: diagName() + ": grammar defines no rules — " +
			"only informational prose terminals."}
	}

	grammar.Productions = kept
	return nil
}

// liftLiteralTokens lifts single-literal productions into *named lexer
// tokens*.
//
// A production whose whole body is one string literal is a lexical
// definition, not a syntactic rule:
//
//	PL = "+"
//
// Compiled as a rule it would produce a token named after the literal text —
// and since `+` has no word characters to name it after, that degrades to
// `#T` / `#T1` / … — plus a one-token `PL` rule wrapping it, so a grammar
// rendered back to ABNF reads `PL = T` with a separate `T = "+"`. Lifting
// instead binds the literal to `#PL` directly and drops the rule, which is
// exactly how the same grammar is written by hand against the engine
// (`Fixed: {Token: {"#PL": "+"}}`).
//
// The start rule is never lifted: it has to stay a rule for the grammar to
// have an entry point. Multi-alternative productions (`sign = "+" / "-"`) are
// real choices and are left alone, as are names the engine already owns (see
// reservedTokenNames) and the RFC 5234 core rules.
//
// Returns every lifted definition, referenced or not: the production is gone
// from the grammar, so an unreferenced one would otherwise leave no element
// behind for the emitter to allocate a token from, and the user's declaration
// would vanish silently. Go port of the TS liftLiteralTokens.
func liftLiteralTokens(grammar *Grammar, start string) []*Element {
	type liftedLit struct {
		literal       string
		caseSensitive bool
		HasCaseSens   bool
	}
	lifted := map[string]liftedLit{}
	order := []string{}

	for _, prod := range grammar.Productions {
		if prod.Name == start || reservedTokenNames[prod.Name] ||
			prod.NodeKind == "core" {
			continue
		}
		if len(prod.Alts) != 1 || len(prod.Alts[0]) != 1 {
			continue
		}
		el := prod.Alts[0][0]
		if el.Kind != KindTerm {
			continue
		}
		// `path-empty = ""` (RFC 3986) matches the empty string — a rule that
		// derives epsilon, not a token the lexer could ever emit.
		if el.Literal == "" {
			continue
		}
		lifted[prod.Name] = liftedLit{el.Literal, el.CaseSensitive, el.HasCaseSens}
		order = append(order, prod.Name)
	}

	// The engine keys its fixed tokens by literal (fixed.token is inverted to
	// src -> tin), so one literal is one token — two names for the same
	// literal cannot both become tokens. When `A = "+"` and `B = "+"` both
	// claim `+`, lifting either would silently drop the other, so neither is
	// lifted and both stay ordinary rules.
	byLiteral := map[string][]string{}
	for _, name := range order {
		lit := lifted[name]
		key := termKey(&Element{Kind: KindTerm, Literal: lit.literal,
			CaseSensitive: lit.caseSensitive, HasCaseSens: lit.HasCaseSens})
		byLiteral[key] = append(byLiteral[key], name)
	}
	for _, names := range byLiteral {
		if len(names) > 1 {
			for _, n := range names {
				delete(lifted, n)
			}
		}
	}
	if len(lifted) == 0 {
		return nil
	}

	mk := func(name string, lit liftedLit) *Element {
		return &Element{Kind: KindTerm, Literal: lit.literal,
			CaseSensitive: lit.caseSensitive, HasCaseSens: lit.HasCaseSens,
			TokenName: name}
	}

	var walk func(el *Element) *Element
	walk = func(el *Element) *Element {
		switch el.Kind {
		case KindRef:
			if lit, ok := lifted[el.Name]; ok {
				return mk(el.Name, lit)
			}
			return el
		case KindOpt, KindStar, KindPlus, KindRep:
			cp := *el
			cp.Inner = walk(el.Inner)
			return &cp
		case KindGroup:
			alts := make([]Sequence, len(el.Alts))
			for i, a := range el.Alts {
				na := make(Sequence, len(a))
				for j, e := range a {
					na[j] = walk(e)
				}
				alts[i] = na
			}
			return &Element{Kind: KindGroup, Alts: alts}
		}
		return el
	}

	out := []*Production{}
	for _, p := range grammar.Productions {
		if _, ok := lifted[p.Name]; ok {
			continue
		}
		alts := make([]Sequence, len(p.Alts))
		for i, a := range p.Alts {
			na := make(Sequence, len(a))
			for j, e := range a {
				na[j] = walk(e)
			}
			alts[i] = na
		}
		cp := *p
		cp.Alts = alts
		out = append(out, &cp)
	}
	grammar.Productions = out

	// Stable order (declaration order) so token allocation is deterministic.
	liftedEls := []*Element{}
	for _, name := range order {
		if lit, ok := lifted[name]; ok {
			liftedEls = append(liftedEls, mk(name, lit))
		}
	}
	return liftedEls
}

// normalizeBuiltinTokens rewrites every bareword reference whose name is a
// builtin token AND is not a defined production into a KindToken element. Run
// before any other pass so the rest of the pipeline treats these as ordinary
// terminals. Go port of the TS normalizeBuiltinTokens.
func normalizeBuiltinTokens(grammar *Grammar) {
	defined := map[string]bool{}
	for _, p := range grammar.Productions {
		defined[p.Name] = true
	}
	var walk func(el *Element) *Element
	walk = func(el *Element) *Element {
		switch el.Kind {
		case KindRef:
			if tok, ok := builtinTokens[el.Name]; ok && !defined[el.Name] {
				return &Element{Kind: KindToken, Name: tok}
			}
			return el
		case KindOpt, KindStar, KindPlus, KindRep:
			cp := *el
			cp.Inner = walk(el.Inner)
			return &cp
		case KindGroup:
			alts := make([]Sequence, len(el.Alts))
			for i, a := range el.Alts {
				na := make(Sequence, len(a))
				for j, e := range a {
					na[j] = walk(e)
				}
				alts[i] = na
			}
			return &Element{Kind: KindGroup, Alts: alts}
		}
		return el
	}
	for _, prod := range grammar.Productions {
		for ai, alt := range prod.Alts {
			na := make(Sequence, len(alt))
			for j, e := range alt {
				na[j] = walk(e)
			}
			prod.Alts[ai] = na
		}
	}
}

// endsWithWordChar reports whether s ends in [A-Za-z0-9_] — the test the
// wordKeywords option uses to decide a literal needs a `\b` boundary guard.
func endsWithWordChar(s string) bool {
	if s == "" {
		return false
	}
	c := s[len(s)-1]
	return c == '_' ||
		('0' <= c && c <= '9') ||
		('A' <= c && c <= 'Z') ||
		('a' <= c && c <= 'z')
}
