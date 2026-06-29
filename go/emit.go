// Copyright (c) 2025-2026 Richard Rodger and other contributors, MIT License

package tabnasabnf

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
func emitGrammarSpec(grammar *abnfGrammar, opts *AbnfConvertOptions) (*tabnas.GrammarSpec, error) {
	if opts == nil {
		opts = &AbnfConvertOptions{}
	}
	start := opts.Start
	if start == "" {
		start = grammar.Productions[0].Name
	}
	tag := opts.Tag
	if tag == "" {
		tag = "abnf"
	}

	// Resolve bare builtin token names (TX/NR/ST/VL) to token terminals before
	// any structural pass sees them as rule references.
	normalizeBuiltinTokens(grammar)

	grammar = eliminateLeftRecursion(grammar)
	grammar = rewriteProbeDispatches(grammar)
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

	allocTerm := func(el *abnfElement) {
		key := termKey(el)
		if _, ok := literals[key]; ok {
			return
		}
		name := allocTokenName(el.Literal, usedNames)
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
	allocRegex := func(el *abnfElement) {
		key := regexKey(el)
		if _, ok := regexTokens[key]; ok {
			return
		}
		name := allocTokenName("rx_"+el.Pattern, usedNames)
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

	for _, prod := range grammar.Productions {
		for _, alt := range prod.Alts {
			for _, el := range alt {
				if el.Kind == kindTerm {
					allocTerm(el)
				} else if el.Kind == kindRegex {
					allocRegex(el)
				}
			}
		}
		if prod.ProbeHelper != nil {
			for _, el := range prod.ProbeHelper.VocabElements {
				if el.Kind == kindTerm {
					allocTerm(el)
				} else if el.Kind == kindRegex {
					allocRegex(el)
				}
			}
		}
	}

	knownRules := map[string]bool{}
	for _, p := range grammar.Productions {
		knownRules[p.Name] = true
	}
	firstSets, nullable := computeFirstSets(grammar, literals, regexTokens)

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
			tag, ruleSpec, firstSets, nullable, refs); err != nil {
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
}

func segmentize(alt abnfSequence, literals, regexTokens map[string]string) []segment {
	segs := []segment{}
	current := segment{}
	for _, el := range alt {
		switch el.Kind {
		case kindTerm:
			current.terms = append(current.terms, literals[termKey(el)])
		case kindRegex:
			current.terms = append(current.terms, regexTokens[regexKey(el)])
		case kindToken:
			current.terms = append(current.terms, el.Name)
		case kindRef:
			current.ref = el.Name
			segs = append(segs, current)
			current = segment{}
		default:
			panic(fmt.Sprintf("abnf: internal — unexpected element kind '%s' in emitter", el.Kind))
		}
	}
	if len(current.terms) > 0 || len(segs) == 0 {
		segs = append(segs, current)
	}
	return segs
}

func isSingleSegment(alt abnfSequence) bool {
	sawRef := false
	for _, el := range alt {
		switch el.Kind {
		case kindRef:
			if sawRef {
				return false
			}
			sawRef = true
		case kindTerm, kindRegex, kindToken:
			if sawRef {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func validateRefs(alt abnfSequence, knownRules map[string]bool, ruleName string) error {
	for _, el := range alt {
		if el.Kind == kindRef && !knownRules[el.Name] {
			return fmt.Errorf("abnf: rule '%s' references unknown rule '%s'", ruleName, el.Name)
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

func altDiscriminator(alt abnfSequence, literals, regexTokens map[string]string) string {
	if len(alt) == 0 {
		return "_"
	}
	el := alt[0]
	switch el.Kind {
	case kindTerm:
		s := strings.TrimPrefix(literals[termKey(el)], "#")
		if s == "" {
			return "_"
		}
		return s
	case kindRegex:
		s := strings.TrimPrefix(regexTokens[regexKey(el)], "#")
		if s == "" {
			return "_"
		}
		return s
	case kindToken:
		s := strings.TrimPrefix(el.Name, "#")
		if s == "" {
			return "_"
		}
		return s
	case kindRef:
		return el.Name
	}
	return "_"
}

// markTable holds mark assignments keyed by alt index within a rule.
type markTable struct {
	byIndex map[int]string
}

func buildMarks(alts []abnfSequence, literals, regexTokens map[string]string) *markTable {
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

func emitProduction(prod *abnfProduction, grammar *abnfGrammar, literals, regexTokens map[string]string,
	knownRules map[string]bool, tag string, ruleSpec map[string]*tabnas.GrammarRuleSpec,
	firstSets map[string]map[string]bool, nullable map[string]bool, refs *refRegistry) error {

	for _, alt := range prod.Alts {
		if err := validateRefs(alt, knownRules, prod.Name); err != nil {
			return err
		}
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
		ordered := []abnfSequence{}
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
		opens := []map[string]any{}
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
					for _, tok := range sortedKeys(firstTokens) {
						o := map[string]any{
							"s": tok, "b": 1, "p": seg.ref, "g": tag,
						}
						merge(o, refs.node(map[string]any{
							"init": true, "rule": prod.Name, "kind": prodKind, "nterms": 0,
						}))
						if mark != "" {
							o["m"] = mark
						}
						opens = append(opens, o)
					}
					continue
				}
			}
			o := segmentToAlt(seg, tag, refs, true, prod.Name, prodKind)
			if mark != "" {
				o["m"] = mark
			}
			opens = append(opens, o)
		}

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
	dispatchOpen := []map[string]any{}
	emptyAltSeen := false
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
				dispatchOpen = append(dispatchOpen, o)
			}
		} else {
			firstTokens := firstOfAlt(alt, literals, regexTokens, firstSets, nullable)
			if firstTokens == nil {
				return fmt.Errorf("abnf: rule '%s' alternative %d is nullable "+
					"but is not the only empty alt; FIRST set is ambiguous", prod.Name, i)
			}
			for _, tok := range sortedKeys(firstTokens) {
				o := map[string]any{"s": tok, "b": 1, "p": implName, "g": tag}
				merge(o, copyMap(initDispatchFields))
				if mark != "" {
					o["m"] = mark
				}
				dispatchOpen = append(dispatchOpen, o)
			}
		}
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
		dispatchOpen = append(dispatchOpen, o)
	}

	dispClose := captureChildFields(refs, prod.Name, prodKind)
	dispClose["g"] = tag
	if dispatchMarks != nil {
		dispClose["m"] = "_"
	}
	ruleSpec[prod.Name] = &tabnas.GrammarRuleSpec{
		Open:  mapsToAlts(dispatchOpen),
		Close: mapsToAlts([]map[string]any{dispClose}),
	}
	return nil
}

// emitChain emits a (possibly single-step) chain of rules for one alt.
func emitChain(headName string, alt abnfSequence, literals, regexTokens map[string]string,
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

func allRefs(alt abnfSequence) bool {
	for _, el := range alt {
		if el.Kind != kindRef {
			return false
		}
	}
	return true
}

func anyHasRef(alts []abnfSequence) bool {
	for _, alt := range alts {
		for _, el := range alt {
			if el.Kind == kindRef {
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
