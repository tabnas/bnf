// Copyright (c) 2025-2026 Richard Rodger and other contributors, MIT License

package bnf

// emit.go — emitGrammarSpec and friends: turn an ABNF grammar AST into a
// tabnas GrammarSpec. The Go port of the emitter half of converter.ts.
//
// Tree-building actions are emitted either as registered closures
// (builtins=false) or as engine `$`-builtin refs + K config
// (builtins=true). The closures here replicate the engine builtins'
// behaviour so closure-mode and builtin-mode produce the same AST.

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	tabnas "github.com/tabnas/parser/go"
)

// emitGrammarSpec converts an ABNF grammar AST into a tabnas GrammarSpec.
func emitGrammarSpec(grammar *Grammar, opts *ConvertOptions) (*tabnas.GrammarSpec, error) {
	if opts == nil {
		opts = &ConvertOptions{}
	}
	// Diagnostics name the notation the grammar was written in, not this
	// package. The pipeline is synchronous, so a package-level value is
	// safe and avoids threading a prefix through every pass. Mirrors
	// ts/src/compiler.ts.
	if opts.Tag != "" {
		diagPrefix = opts.Tag
	} else {
		diagPrefix = "bnf"
	}
	// Work on a copy: resolveProseTerminals, liftLiteralTokens and
	// normalizeBuiltinTokens all rewrite the grammar in place, so emitting
	// twice from one ParseAbnf result would otherwise give two different
	// specs — the second missing every lifted production, since the first
	// pass had already removed them.
	grammar = cloneGrammar(grammar)

	// Drop informational prose definitions (`NR = <number>`) first, so the
	// names they document fall through to the builtin tokens — and so a
	// leading prose line is never mistaken for the start rule.
	if err := resolveProseTerminals(grammar); err != nil {
		return nil, err
	}

	// Capture the <remove> directives before the rewrite passes below: each
	// returns a fresh grammar carrying only Productions, so anything else on
	// the grammar is dropped at the first reassignment. Mirrors the TS
	// emitter, which snapshots removeNames/clearAll for the same reason.
	removeNames := append([]string{}, grammar.Remove...)
	clearAll := grammar.ClearAll

	start := opts.Start
	if start == "" {
		start = grammar.Productions[0].Name
	}
	tag := opts.Tag
	if tag == "" {
		tag = "abnf"
	}

	// Turn single-literal productions (`PL = "+"`) into named lexer tokens,
	// then resolve bare builtin token names (TX/NR/ST/VL) to token terminals —
	// both before any structural pass sees them as rule references.
	liftedLiterals := liftLiteralTokens(grammar, start)
	normalizeBuiltinTokens(grammar)

	grammar = eliminateLeftRecursion(grammar)
	grammar = rewriteProbeDispatches(grammar)
	// Left factoring runs after the probe rewriter (so `[X D] Y`
	// patterns are recognised in their original alternatives) and
	// before tail-repeat detection and desugaring.
	grammar = leftFactor(grammar)
	grammar = rewriteTailRepeats(grammar, start)
	grammar = desugar(grammar)

	// Token allocation.
	literals := map[string]string{}    // literal-key -> token name
	regexTokens := map[string]string{} // regex key -> token name
	usedNames := map[string]bool{}
	fixedTokens := map[string]*string{}
	matchTokens := map[string]*regexp.Regexp{}
	matchEager := map[string]bool{}
	// matchOrder records the order in which match tokens are first
	// allocated (the grammar walk order). The engine allocates Tins in
	// this order so its deterministic match-token iteration reflects the
	// same precedence as the TS converter's ordered match.token object —
	// crucial when overlapping eager tokens (a range regex vs a single-
	// char case-insensitive literal) both match the same source char.
	var matchOrder []string

	allocTerm := func(el *Element) {
		key := termKey(el)
		if _, ok := literals[key]; ok {
			return
		}
		name := allocTokenName(el.Literal, usedNames, el.TokenName)
		literals[key] = name
		// A word-keyword literal (ending in a word char) needs a trailing `\b`
		// guard so it only matches as a whole word; that forces a regex token
		// even when the literal is case-sensitive. Mirrors TS emitLiteralToken.
		boundary := ""
		if opts.WordKeywords && endsWithWordChar(el.Literal) {
			boundary = `\b`
		}
		if isEffectivelyCaseSensitive(el) && boundary == "" {
			lit := el.Literal
			fixedTokens[name] = &lit
		} else {
			flags := "(?i)"
			if isEffectivelyCaseSensitive(el) {
				flags = ""
			}
			re := regexp.MustCompile(flags + "^" + escapeRegexp(el.Literal) + boundary)
			matchTokens[name] = re
			matchEager[name] = true
			matchOrder = append(matchOrder, name)
		}
	}
	allocRegex := func(el *Element) {
		key := regexKey(el)
		if _, ok := regexTokens[key]; ok {
			return
		}
		name := allocTokenName("rx_"+el.Pattern, usedNames, "")
		regexTokens[key] = name
		matchTokens[name] = goRegex(el.Pattern, el.Flags)
		// The Go engine gates non-eager match tokens by alt position 0
		// only (the TS engine uses a per-position tcol that covers every
		// alt slot). Marking range regexes eager makes them fire at any
		// lookahead position — equivalent coverage; the parser still
		// rejects a token it doesn't expect at the current slot.
		matchEager[name] = true
		matchOrder = append(matchOrder, name)
	}

	// Gather every terminal first. Probe-helper productions store their vocab
	// as elements rather than in Alts, so walk those too. The lifted literals
	// are seeded up front: their productions no longer exist, so an
	// unreferenced one has no element anywhere in Alts.
	terminals := append([]*Element{}, liftedLiterals...)
	for _, prod := range grammar.Productions {
		for _, alt := range prod.Alts {
			terminals = append(terminals, alt...)
		}
		if prod.ProbeHelper != nil {
			terminals = append(terminals, prod.ProbeHelper.VocabElements...)
		}
	}

	// Terminals carrying a lifted production name are allocated first, so the
	// name wins even when the same literal also appears inline in an earlier
	// rule (`PL = "+"` must yield `#PL`, not `#T`, regardless of where the
	// bare `"+"` shows up).
	named := []*Element{}
	for _, el := range terminals {
		if el.Kind == KindTerm && el.TokenName != "" {
			named = append(named, el)
		}
	}
	for _, el := range append(named, terminals...) {
		if el.Kind == KindTerm {
			allocTerm(el)
		} else if el.Kind == KindRegex {
			allocRegex(el)
		}
	}

	knownRules := map[string]bool{}
	for _, p := range grammar.Productions {
		knownRules[p.Name] = true
	}
	firstSets, nullable := computeFirstSets(grammar, literals, regexTokens)
	// Settle the contested left-recursion tail loops flagged during
	// elimination, now that FIRST sets can say whether the competition is
	// real. Runs on the desugared grammar because the loop is a helper
	// production by this point.
	cc := newContestCtx(fixedTokens, matchTokens)
	resolveSuffixDebts(grammar, literals, regexTokens, firstSets, nullable, cc)
	// FOLLOW puts the tokens that may come after a repetition back into
	// its terminating alternative's token column; FOLLOW₂ decides a
	// repetition whose repeated class COVERS one of them. See follow.go.
	followSets := computeFollowSets(
		grammar, literals, regexTokens, firstSets, nullable, start)
	followPairs := computeFollowPairs(
		grammar, literals, regexTokens, firstSets, nullable, followSets)
	refs := newRefRegistry()
	refs.useBuiltins = opts.Builtins
	refs.emitMarks = opts.Marks

	ruleSpec := map[string]*tabnas.GrammarRuleSpec{}
	for _, prod := range grammar.Productions {
		if prod.ProbeHelper != nil {
			emitProbeHelper(prod, tag, ruleSpec, literals, regexTokens)
			continue
		}
		if prod.ProbeDisp != nil {
			emitProbeDispatch(prod, tag, ruleSpec, refs, literals, regexTokens, opts.Builtins)
			continue
		}
		if err := emitProduction(prod, grammar, literals, regexTokens, knownRules,
			tag, ruleSpec, firstSets, nullable, refs,
			followSets, followPairs, cc); err != nil {
			return nil, err
		}
	}

	// __start__ wrapper consumes #ZZ.
	startWrapper := "__start__"
	bubbleClose := refs.bubble()
	bubbleClose["s"] = "#ZZ"
	bubbleClose["g"] = tag
	ruleSpec[startWrapper] = &tabnas.GrammarRuleSpec{
		Open:  []*tabnas.GrammarAltSpec{mapToAlt(map[string]any{"p": start, "g": tag})},
		Close: []*tabnas.GrammarAltSpec{mapToAlt(bubbleClose)},
	}

	opt := &tabnas.Options{
		Fixed: &tabnas.FixedOptions{Token: fixedTokens},
		Rule:  &tabnas.RuleOptions{Start: startWrapper},
	}
	if len(matchTokens) > 0 {
		opt.Match = &tabnas.MatchOptions{
			Token: matchTokens, TokenEager: matchEager, TokenOrder: matchOrder,
		}
	}

	spec := &tabnas.GrammarSpec{
		Ref:     refs.refMap(),
		Options: opt,
		Rule:    ruleSpec,
	}

	// `<remove>` directives. `<all> = <remove>` maps to the engine's Clear,
	// which wipes rules and fixed tokens before the rest of the spec is
	// applied — so a grammar can reset an instance and rebuild it in one
	// pass. A named removal drops both the rule and the fixed token of that
	// name, because ABNF does not distinguish them at the point of use and a
	// removal that matches nothing is a no-op either way. A nil map entry is
	// how the engine spells "remove" for both. Mirrors the TS emitter.
	if clearAll {
		spec.Clear = true
	}
	for _, name := range removeNames {
		ruleSpec[name] = nil
		fixedTokens["#"+name] = nil
	}
	return spec, nil
}

// goRegex translates a JS-flavoured regex source + flags into a Go
// regexp. The patterns the converter emits are simple char classes
// (`[\x{0030}-\x{0039}]`) so no heavy translation is needed; the `i`
// flag maps to the (?i) inline group.
func goRegex(pattern, flags string) *regexp.Regexp {
	src := "^" + pattern
	if strings.Contains(flags, "i") {
		src = "(?i)" + src
	}
	return regexp.MustCompile(src)
}

// ---- segments ------------------------------------------------------

type segment struct {
	terms []string
	ref   string
	// debt holds the counter mutations the pushing alt carries, from the
	// reference's Debt annotation. See resolveSuffixDebts.
	debt map[string]int
}

func segmentize(alt Sequence, literals, regexTokens map[string]string) []segment {
	segs := []segment{}
	current := segment{}
	for _, el := range alt {
		switch el.Kind {
		case KindTerm:
			current.terms = append(current.terms, literals[termKey(el)])
		case KindRegex:
			current.terms = append(current.terms, regexTokens[regexKey(el)])
		case KindToken:
			current.terms = append(current.terms, el.Name)
		case KindRef:
			current.ref = el.Name
			current.debt = el.Debt
			segs = append(segs, current)
			current = segment{}
		default:
			panic(fmt.Sprintf(diagName()+": internal — unexpected element kind '%s' in emitter", el.Kind))
		}
	}
	if len(current.terms) > 0 || len(segs) == 0 {
		segs = append(segs, current)
	}
	return segs
}

func isSingleSegment(alt Sequence) bool {
	sawRef := false
	for _, el := range alt {
		switch el.Kind {
		case KindRef:
			if sawRef {
				return false
			}
			sawRef = true
		case KindTerm, KindRegex, KindToken:
			if sawRef {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func validateRefs(alt Sequence, knownRules map[string]bool, ruleName string) error {
	for _, el := range alt {
		if el.Kind == KindRef && !knownRules[el.Name] {
			return fmt.Errorf(diagName()+": rule '%s' references unknown rule '%s'", ruleName, el.Name)
		}
	}
	return nil
}

// ---- RefRegistry ---------------------------------------------------

// refRegistry allocates unique @-prefixed FuncRef names for inline
// action closures, OR emits engine `$`-builtin refs + K config.
type refRegistry struct {
	refs        map[tabnas.FuncRef]any
	counter     int
	useBuiltins bool
	emitMarks   bool
}

func newRefRegistry() *refRegistry {
	return &refRegistry{refs: map[tabnas.FuncRef]any{}}
}

func (rr *refRegistry) refMap() map[tabnas.FuncRef]any { return rr.refs }

func (rr *refRegistry) registerAction(fn tabnas.AltAction) tabnas.FuncRef {
	name := tabnas.FuncRef("@abnf_a" + strconv.Itoa(rr.counter))
	rr.counter++
	rr.refs[name] = fn
	return name
}

// node returns alt-spec fields for tree-node init/accumulate.
func (rr *refRegistry) node(cfg map[string]any) map[string]any {
	if rr.useBuiltins {
		return map[string]any{"a": "@node$", "k": map[string]any{"node$": cfg}}
	}
	init, _ := cfg["init"].(bool)
	rule, _ := cfg["rule"].(string)
	kind, _ := cfg["kind"].(string)
	nterms, _ := cfg["nterms"].(int)
	ref := rr.registerAction(func(r *tabnas.Rule, _ *tabnas.Context) {
		if init {
			r.Node = mkAstNode(rule, kind)
		}
		n, _ := r.Node.(map[string]any)
		if n == nil {
			return
		}
		src, _ := n["src"].(string)
		for i := 0; i < nterms && i < len(r.O); i++ {
			src += r.O[i].Src
		}
		n["src"] = src
	})
	return map[string]any{"a": string(ref)}
}

// capture returns alt-spec fields for merging a returned child node.
func (rr *refRegistry) capture(cfg map[string]any) map[string]any {
	if rr.useBuiltins {
		return map[string]any{"a": "@capture$", "k": map[string]any{"capture$": cfg}}
	}
	rule, _ := cfg["rule"].(string)
	kind, _ := cfg["kind"].(string)
	ref := rr.registerAction(func(r *tabnas.Rule, _ *tabnas.Context) {
		if r.Node == nil {
			r.Node = mkAstNode(rule, kind)
		}
		n, _ := r.Node.(map[string]any)
		if n == nil || r.Child == nil {
			return
		}
		c := r.Child.Node
		if c == nil || c == tabnas.Undefined {
			return
		}
		cm, ok := c.(map[string]any)
		if !ok {
			n["kids"] = append(asAnyKids(n["kids"]), c)
			return
		}
		if _, hasSrc := cm["src"]; !hasSrc {
			n["kids"] = append(asAnyKids(n["kids"]), c)
			return
		}
		if sameMap(cm, n) {
			return
		}
		ns, _ := n["src"].(string)
		cs, _ := cm["src"].(string)
		n["src"] = ns + cs
		if rv, ok := cm["rule"]; ok && rv != nil && rv != "" {
			n["kids"] = append(asAnyKids(n["kids"]), cm)
		} else if ck, ok := cm["kids"].([]any); ok {
			n["kids"] = append(asAnyKids(n["kids"]), ck...)
		}
	})
	return map[string]any{"a": string(ref)}
}

// bubble returns alt-spec fields that lift the committed child's node.
func (rr *refRegistry) bubble() map[string]any {
	if rr.useBuiltins {
		return map[string]any{"a": "@bubble$"}
	}
	ref := rr.registerAction(func(r *tabnas.Rule, _ *tabnas.Context) {
		if r.Child != nil && r.Child.Node != tabnas.Undefined {
			r.Node = r.Child.Node
		}
	})
	return map[string]any{"a": string(ref)}
}

// fold returns alt-spec fields for a tail-repeat iteration delivering
// its node to the parent (closure-mode twin of the engine's `@fold$`
// builtin — the two MUST stay behaviourally identical; the fixture
// suite pins this).
func (rr *refRegistry) fold(cN int) map[string]any {
	if rr.useBuiltins {
		cfg := map[string]any{}
		if cN > 0 {
			cfg["cN"] = cN
		}
		return map[string]any{"a": "@fold$", "k": map[string]any{"fold$": cfg}}
	}
	ref := rr.registerAction(func(r *tabnas.Rule, _ *tabnas.Context) {
		if r.Parent == nil {
			return
		}
		p, _ := r.Parent.Node.(map[string]any)
		if p == nil {
			return
		}
		if _, hasSrc := p["src"]; !hasSrc {
			return
		}
		if own, ok := r.Node.(map[string]any); ok && own != nil && !sameMap(own, p) {
			if _, hasSrc := own["src"]; hasSrc {
				ps, _ := p["src"].(string)
				os, _ := own["src"].(string)
				p["src"] = ps + os
				if rv, ok := own["rule"]; ok && rv != nil && rv != "" {
					p["kids"] = append(asAnyKids(p["kids"]), own)
				} else if ok2, okk := own["kids"].([]any); okk {
					p["kids"] = append(asAnyKids(p["kids"]), ok2...)
				}
			}
		}
		for i := 0; i < cN && i < len(r.C); i++ {
			if r.C[i] != nil {
				ps, _ := p["src"].(string)
				p["src"] = ps + r.C[i].Src
			}
		}
		r.Node = tabnas.Undefined
	})
	return map[string]any{"a": string(ref)}
}

// ---- AST node helpers ----------------------------------------------

func mkAstNode(ruleName, nodeKind string) map[string]any {
	if nodeKind == "user" {
		return map[string]any{"rule": ruleName, "src": "", "kids": []any{}}
	}
	return map[string]any{"src": "", "kids": []any{}}
}

func asAnyKids(v any) []any {
	if s, ok := v.([]any); ok {
		return s
	}
	return []any{}
}

func sameMap(a, b map[string]any) bool {
	// Two non-nil maps are the same underlying object only if pointer-equal.
	// Go maps are reference types; compare via fmt pointer.
	return fmt.Sprintf("%p", a) == fmt.Sprintf("%p", b)
}

// ---- marks ---------------------------------------------------------

func altDiscriminator(alt Sequence, literals, regexTokens map[string]string) string {
	if len(alt) == 0 {
		return "_"
	}
	el := alt[0]
	switch el.Kind {
	case KindTerm:
		s := strings.TrimPrefix(literals[termKey(el)], "#")
		if s == "" {
			return "_"
		}
		return s
	case KindRegex:
		s := strings.TrimPrefix(regexTokens[regexKey(el)], "#")
		if s == "" {
			return "_"
		}
		return s
	case KindToken:
		s := strings.TrimPrefix(el.Name, "#")
		if s == "" {
			return "_"
		}
		return s
	case KindRef:
		return el.Name
	}
	return "_"
}

// markTable holds mark assignments keyed by alt index within a rule.
type markTable struct {
	byIndex map[int]string
}

func buildMarks(alts []Sequence, literals, regexTokens map[string]string) *markTable {
	mt := &markTable{byIndex: map[int]string{}}
	seen := map[string]int{}
	for i, alt := range alts {
		base := altDiscriminator(alt, literals, regexTokens)
		n := seen[base] + 1
		seen[base] = n
		if n == 1 {
			mt.byIndex[i] = base
		} else {
			mt.byIndex[i] = fmt.Sprintf("%s~%d", base, n)
		}
	}
	return mt
}

// ---- segmentToAlt --------------------------------------------------

func segmentToAlt(seg segment, tag string, refs *refRegistry, initNode bool, ruleName, nodeKind string) map[string]any {
	spec := map[string]any{"g": tag}
	if len(seg.terms) > 0 {
		spec["s"] = strings.Join(seg.terms, " ")
	}
	if seg.ref != "" {
		spec["p"] = seg.ref
	}
	// Suffix-debt bookkeeping rides on the alt that does the push, so the
	// child inherits the updated counter: the engine applies `n` before it
	// copies counters into the pushed rule.
	if len(seg.debt) > 0 {
		n := map[string]int{}
		for k, v := range seg.debt {
			n[k] = v
		}
		spec["n"] = n
	}
	nterms := len(seg.terms)
	if nterms > 0 || initNode {
		merge(spec, refs.node(map[string]any{
			"init": initNode, "rule": ruleName, "kind": nodeKind, "nterms": nterms,
		}))
	}
	return spec
}

func captureChildFields(refs *refRegistry, ruleName, nodeKind string) map[string]any {
	return refs.capture(map[string]any{"rule": ruleName, "kind": nodeKind})
}

// ---- emitProduction ------------------------------------------------

// emitTailRepeat emits a production marked by rewriteTailRepeats:
//
//	open:  [ { s: prefix,  node$ init } ]
//	close: [ { s: sep, r: SELF, fold$ cN } , { fold$ } ]
//
// The same shape a hand-written tabnas grammar uses for `X = a [ b X ]`.
// Mirrors the TS emitTailRepeat; the alignment TSVs pin the shape.
func emitTailRepeat(prod *Production, literals, regexTokens map[string]string,
	tag string, ruleSpec map[string]*tabnas.GrammarRuleSpec, refs *refRegistry) {

	prodKind := prod.kind()
	prefixAlt := prod.Alts[0]
	sep := prod.TailRepeat.Sep

	// All-terminal sequences (guaranteed by the rewrite's guards), so
	// each segmentizes to exactly one ref-free segment.
	prefixSeg := segmentize(prefixAlt, literals, regexTokens)[0]
	sepSeg := segmentize(sep, literals, regexTokens)[0]

	var marks *markTable
	if prodKind == "user" && refs.emitMarks {
		marks = buildMarks([]Sequence{prefixAlt, sep}, literals, regexTokens)
	}

	open := segmentToAlt(prefixSeg, tag, refs, true, prod.Name, prodKind)
	if marks != nil {
		open["m"] = marks.byIndex[0]
	}

	repeat := map[string]any{
		"s": strings.Join(sepSeg.terms, " "),
		"r": prod.Name,
		"g": tag,
	}
	for k, v := range refs.fold(len(sepSeg.terms)) {
		repeat[k] = v
	}
	if marks != nil {
		repeat["m"] = marks.byIndex[1]
	}

	end := map[string]any{"g": tag}
	for k, v := range refs.fold(0) {
		end[k] = v
	}
	if marks != nil {
		end["m"] = "_"
	}

	ruleSpec[prod.Name] = &tabnas.GrammarRuleSpec{
		Open:  []*tabnas.GrammarAltSpec{mapToAlt(open)},
		Close: []*tabnas.GrammarAltSpec{mapToAlt(repeat), mapToAlt(end)},
	}
}

func emitProduction(prod *Production, grammar *Grammar, literals, regexTokens map[string]string,
	knownRules map[string]bool, tag string, ruleSpec map[string]*tabnas.GrammarRuleSpec,
	firstSets map[string]map[string]bool, nullable map[string]bool, refs *refRegistry,
	followSets map[string]map[string]bool,
	followPairs map[string]map[string]map[string]bool, cc *contestCtx) error {

	for _, alt := range prod.Alts {
		if err := validateRefs(alt, knownRules, prod.Name); err != nil {
			return err
		}
	}

	// Suffix-debt guard for a contested left-recursion tail loop: a branch
	// that would eat a token an enclosing frame still owes may only run while
	// the debt is zero. Applies to the continue alternatives and never to the
	// empty fallback, which is what lets the loop yield rather than fail.
	//
	// Only the branches whose head token is contested are guarded. A loop
	// built from several tails repeats several tokens, and the ones the suffix
	// does not compete for must stay open at any debt — otherwise
	// `A = A "y" / A "w" / "x" A "y" / "z"` rejects `xzwy`, where the inner A
	// must consume the `w` before yielding the `y`.
	//
	// The value is the scalar `$eq` shorthand, which both runtimes accept.
	// See resolveSuffixDebts.
	owed := map[string]bool{}
	for _, t := range prod.DebtOwed {
		owed[t] = true
	}
	debtGuard := func(o map[string]any) map[string]any {
		if prod.DebtGuard == "" || len(owed) == 0 {
			return o
		}
		// Entries are keyed by the token sequence they peek, so the head token
		// says which branch this is. A continue alternative always names one;
		// if it somehow does not, guard it — that is the direction that keeps
		// the loop from starving its parent.
		if s, ok := o["s"].(string); ok {
			head := s
			if i := strings.IndexByte(s, ' '); i >= 0 {
				head = s[:i]
			}
			if !owed[head] {
				return o
			}
		}
		o["c"] = map[string]any{"n." + prod.DebtGuard: 0}
		return o
	}

	if prod.TailRepeat != nil {
		emitTailRepeat(prod, literals, regexTokens, tag, ruleSpec, refs)
		return nil
	}

	allSimple := true
	for _, alt := range prod.Alts {
		if !isSingleSegment(alt) {
			allSimple = false
			break
		}
	}

	prodKind := prod.kind()

	if allSimple {
		// Order non-empty alts first, empty alts last (stable).
		ordered := []Sequence{}
		for _, alt := range prod.Alts {
			if len(alt) > 0 {
				ordered = append(ordered, alt)
			}
		}
		for _, alt := range prod.Alts {
			if len(alt) == 0 {
				ordered = append(ordered, alt)
			}
		}

		var marks *markTable
		if prodKind == "user" && refs.emitMarks {
			marks = buildMarks(ordered, literals, regexTokens)
		}
		needsPeek := len(ordered) > 1
		entries := []dispatchEntry{}
		for idx, alt := range ordered {
			segs := segmentize(alt, literals, regexTokens)
			seg := segs[0]
			isRefOnly := len(alt) >= 1 && allRefs(alt) && len(seg.terms) == 0 && seg.ref != ""
			mark := ""
			if marks != nil {
				mark = marks.byIndex[idx]
			}
			if needsPeek && isRefOnly {
				firstTokens := firstOfAlt(alt, literals, regexTokens, firstSets, nullable)
				if firstTokens != nil {
					// A CONTESTED head cannot be decided by one token —
					// fan out to K-token prefixes (bounded and deduped;
					// fall back to the 1-token peek if the fan-out is
					// degenerate) so the ordering has lookahead to work
					// with.
					var paths [][]string
					if altHeadContested(alt, ordered, literals, regexTokens,
						firstSets, nullable, cc) ||
						contestedByFollow(prod, alt, literals, regexTokens,
							firstSets, nullable, followSets, cc) {
						pfx := [][]string{}
						for _, p := range altPrefixes(alt, grammar, literals, regexTokens, lookaheadKSpan) {
							if len(p) > 0 {
								pfx = append(pfx, p)
							}
						}
						if 0 < len(pfx) && len(pfx) <= 64 {
							paths = pfx
						}
					}

					if paths != nil {
						for _, p := range paths {
							o := map[string]any{
								"s": strings.Join(p, " "), "b": len(p),
								"p": seg.ref, "g": tag,
							}
							merge(o, refs.node(map[string]any{
								"init": true, "rule": prod.Name, "kind": prodKind, "nterms": 0,
							}))
							if mark != "" {
								o["m"] = mark
							}
							entries = append(entries, dispatchEntry{o: o, alt: alt})
						}
						continue
					}

					for _, tok := range sortedKeys(firstTokens) {
						o := map[string]any{
							"s": tok, "b": 1, "p": seg.ref, "g": tag,
						}
						// This path builds the push alt by hand rather than
						// through segmentToAlt, so it has to carry the same
						// suffix-debt bookkeeping.
						if len(seg.debt) > 0 {
							n := map[string]int{}
							for k, v := range seg.debt {
								n[k] = v
							}
							o["n"] = n
						}
						merge(o, refs.node(map[string]any{
							"init": true, "rule": prod.Name, "kind": prodKind, "nterms": 0,
						}))
						if mark != "" {
							o["m"] = mark
						}
						entries = append(entries, dispatchEntry{o: debtGuard(o), alt: alt})
					}
					continue
				}
			}
			o := segmentToAlt(seg, tag, refs, true, prod.Name, prodKind)
			if mark != "" {
				o["m"] = mark
			}
			if len(alt) > 0 {
				o = debtGuard(o)
			}

			// The terminating alternative of a repetition helper names no
			// token, so the lexer is never asked to produce whatever
			// follows the repetition. Re-issue that alternative once per
			// FOLLOW token, peeking and pushing straight back (`b: 1`) so
			// the token column widens without anything extra being
			// consumed. The bare alternative stays last as the fallback.
			if len(alt) == 0 && prod.RepeatHelper {
				for _, tok := range sortedKeys(followSets[prod.Name]) {
					g := copyMap(o)
					g["s"] = tok
					g["b"] = 1
					entries = append(entries, dispatchEntry{o: g})
				}
				// Contested repetitions additionally get FOLLOW₂ guards,
				// at the FRONT so they outrank the continue alternatives.
				guards := pairExitGuards(prod, o, followPairs, firstSets, cc)
				front := make([]dispatchEntry, 0, len(guards)+len(entries))
				for _, g := range guards {
					front = append(front, dispatchEntry{o: g})
				}
				entries = append(front, entries...)
			}

			var srcAlt Sequence
			if len(alt) > 0 {
				srcAlt = alt
			}
			entries = append(entries, dispatchEntry{o: o, alt: srcAlt})
		}

		specificityPermute(entries, cc, grammar, regexTokens)
		opens := reorderKeywordShadow(prod, entries, grammar,
			literals, regexTokens, followSets, cc)
		rs := &tabnas.GrammarRuleSpec{Open: mapsToAlts(opens)}
		if anyHasRef(prod.Alts) {
			close := captureChildFields(refs, prod.Name, prodKind)
			close["g"] = tag
			if marks != nil {
				close["m"] = "_"
			}
			rs.Close = mapsToAlts([]map[string]any{close})
		}
		ruleSpec[prod.Name] = rs
		return nil
	}

	if len(prod.Alts) == 1 {
		emitChain(prod.Name, prod.Alts[0], literals, regexTokens, tag, ruleSpec, refs, prodKind)
		return nil
	}

	// Multi-alt with at least one multi-segment alt: dispatcher.
	dispatchEntries := []dispatchEntry{}
	emptyAltSeen := false
	var nullableImpls []nullableImpl
	var dispatchMarks *markTable
	if prodKind == "user" && refs.emitMarks {
		dispatchMarks = buildMarks(prod.Alts, literals, regexTokens)
	}

	for i, alt := range prod.Alts {
		implName := fmt.Sprintf("%s$alt%d", prod.Name, i)
		mark := ""
		if dispatchMarks != nil {
			mark = dispatchMarks.byIndex[i]
		}
		if len(alt) == 0 {
			emptyAltSeen = true
			continue
		}
		emitChain(implName, alt, literals, regexTokens, tag, ruleSpec, refs, "helper")

		dispatchKind := prodKind
		initDispatchFields := refs.node(map[string]any{
			"init": true, "rule": prod.Name, "kind": dispatchKind, "nterms": 0,
		})

		const lookaheadK = 4
		// An alternative that can derive ε — every element nullable, a
		// complete zero-token path rather than a cycle truncation — loses
		// that derivation in the `usable` filter below, because a
		// zero-token prefix names no token to dispatch on. Remember it:
		// after the loop it is re-issued as FOLLOW-guarded entries plus a
		// bare fallback. Without this, `expression ::= term (("+"|"-")
		// term)*` reaches the `;` that ends the statement with nothing in
		// the token column that can lex it, and a valid C program is
		// rejected one character from the end.
		for _, p := range altPrefixesRaw(
			alt, grammar, literals, regexTokens, lookaheadK, map[string]bool{}) {
			if len(p.tokens) == 0 && !p.done {
				nullableImpls = append(nullableImpls, nullableImpl{
					implName: implName, fields: initDispatchFields, mark: mark,
				})
				break
			}
		}
		prefixes := altPrefixes(alt, grammar, literals, regexTokens, lookaheadK)
		usable := [][]string{}
		for _, p := range prefixes {
			if len(p) > 0 {
				usable = append(usable, p)
			}
		}
		if len(usable) > 0 {
			for _, p := range usable {
				o := map[string]any{"s": strings.Join(p, " "), "b": len(p), "p": implName, "g": tag}
				merge(o, copyMap(initDispatchFields))
				if mark != "" {
					o["m"] = mark
				}
				dispatchEntries = append(dispatchEntries, dispatchEntry{o: debtGuard(o), alt: alt})
			}
		} else {
			firstTokens := firstOfAlt(alt, literals, regexTokens, firstSets, nullable)
			if firstTokens == nil {
				return fmt.Errorf(diagName()+": rule '%s' alternative %d is nullable "+
					"but is not the only empty alt; FIRST set is ambiguous", prod.Name, i)
			}
			for _, tok := range sortedKeys(firstTokens) {
				o := map[string]any{"s": tok, "b": 1, "p": implName, "g": tag}
				merge(o, copyMap(initDispatchFields))
				if mark != "" {
					o["m"] = mark
				}
				dispatchEntries = append(dispatchEntries, dispatchEntry{o: debtGuard(o), alt: alt})
			}
		}
	}

	// Re-issue each nullable alternative's ε-derivation: FOLLOW peeks
	// first (naming the follow token is what makes the lexer offer it at
	// this position), then one unguarded fallback that pushes the impl
	// with nothing consumed. Everything here ranks after all content
	// entries, so an ε-derivation never preempts a real match.
	for _, n := range nullableImpls {
		for _, tok := range sortedKeys(followSets[prod.Name]) {
			o := map[string]any{"s": tok, "b": 1, "p": n.implName, "g": tag}
			merge(o, copyMap(n.fields))
			if n.mark != "" {
				o["m"] = n.mark
			}
			dispatchEntries = append(dispatchEntries, dispatchEntry{o: o})
		}
		o := map[string]any{"p": n.implName, "g": tag}
		merge(o, copyMap(n.fields))
		if n.mark != "" {
			o["m"] = n.mark
		}
		dispatchEntries = append(dispatchEntries, dispatchEntry{o: o})
	}

	if emptyAltSeen {
		fallbackKind := prodKind
		o := map[string]any{"g": tag}
		merge(o, refs.node(map[string]any{
			"init": true, "rule": prod.Name, "kind": fallbackKind, "nterms": 0,
		}))
		if dispatchMarks != nil {
			o["m"] = "_"
		}
		// Same FOLLOW guards as the single-segment path above.
		if prod.RepeatHelper {
			for _, tok := range sortedKeys(followSets[prod.Name]) {
				g := copyMap(o)
				g["s"] = tok
				g["b"] = 1
				dispatchEntries = append(dispatchEntries, dispatchEntry{o: g})
			}
			guards := pairExitGuards(prod, o, followPairs, firstSets, cc)
			front := make([]dispatchEntry, 0, len(guards)+len(dispatchEntries))
			for _, g := range guards {
				front = append(front, dispatchEntry{o: g})
			}
			dispatchEntries = append(front, dispatchEntries...)
		}
		dispatchEntries = append(dispatchEntries, dispatchEntry{o: o})
	}

	dispClose := captureChildFields(refs, prod.Name, prodKind)
	dispClose["g"] = tag
	if dispatchMarks != nil {
		dispClose["m"] = "_"
	}
	specificityPermute(dispatchEntries, cc, grammar, regexTokens)
	ruleSpec[prod.Name] = &tabnas.GrammarRuleSpec{
		Open: mapsToAlts(reorderKeywordShadow(prod, dispatchEntries, grammar,
			literals, regexTokens, followSets, cc)),
		Close: mapsToAlts([]map[string]any{dispClose}),
	}
	return nil
}

// emitChain emits a (possibly single-step) chain of rules for one alt.
func emitChain(headName string, alt Sequence, literals, regexTokens map[string]string,
	tag string, ruleSpec map[string]*tabnas.GrammarRuleSpec, refs *refRegistry, headKind string) {

	segs := segmentize(alt, literals, regexTokens)
	chainName := func(i int) string {
		if i == 0 {
			return headName
		}
		return fmt.Sprintf("%s$step%d", headName, i)
	}

	for i := 0; i < len(segs); i++ {
		name := chainName(i)
		seg := segs[i]
		kind := "helper"
		if i == 0 {
			kind = headKind
		}
		headAlt := segmentToAlt(seg, tag, refs, i == 0, name, kind)
		if i == 0 && headKind == "user" && refs.emitMarks {
			headAlt["m"] = altDiscriminator(alt, literals, regexTokens)
		}
		rs := &tabnas.GrammarRuleSpec{Open: mapsToAlts([]map[string]any{headAlt})}

		isLast := i == len(segs)-1
		if !isLast {
			close := map[string]any{"r": chainName(i + 1), "g": tag}
			merge(close, captureChildFields(refs, name, kind))
			rs.Close = mapsToAlts([]map[string]any{close})
		} else if seg.ref != "" {
			close := captureChildFields(refs, name, kind)
			close["g"] = tag
			rs.Close = mapsToAlts([]map[string]any{close})
		}
		ruleSpec[name] = rs
	}
}

// ---- helpers -------------------------------------------------------

func allRefs(alt Sequence) bool {
	for _, el := range alt {
		if el.Kind != KindRef {
			return false
		}
	}
	return true
}

func anyHasRef(alts []Sequence) bool {
	for _, alt := range alts {
		for _, el := range alt {
			if el.Kind == KindRef {
				return true
			}
		}
	}
	return false
}

func merge(dst, src map[string]any) {
	for k, v := range src {
		dst[k] = v
	}
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
