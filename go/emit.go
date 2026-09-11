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

// Recovery sync groups (@tabnas/parser `parse.recover.syncGroups`, whose
// shipped default is exactly ['close','comma','end']). A close alternate
// tagged with one of these offers its LEADING token as a resynchronisation
// point after a syntax error.
//
// Two rules govern how these are stamped, and both are the engine's, not
// preferences:
//
//  1. Only a CLOSE alternate that NAMES A TOKEN can be a sync point. An
//     open alternate contributes nothing, and a close alternate with no
//     `s` is skipped before its tags are even read. Exactly two of this
//     emitter's alternates qualify: the `__start__` wrapper's `#ZZ`, and
//     a tail repeat's separator continuation. Everything else it emits
//     closes by capturing a child, naming no token.
//
//  2. Tag ALL of them or NONE. The engine falls back to "every close
//     alternate's leading token on the stack" only while the tagged set
//     is EMPTY — and that test is over the whole live rule stack, not
//     per rule. So one tagged alternate anywhere switches the fallback
//     off for every rule below it too. Tagging half of them would
//     therefore silently DELETE the other half's sync points. Since the
//     two sites below are the complete set, tagging both is exactly
//     equivalent to the fallback for a grammar parsed on its own — and
//     strictly better when it is composed with an already-tagged
//     grammar, where the host's tags would otherwise disable the
//     fallback these rules were relying on.
//
// Emitted as a comma-separated string because that is the form BOTH
// runtimes accept (`g` as an array is TypeScript-only), with no spaces
// around the comma (the TS grammar builder rejects a padded tag).
// Mirrors the TS `syncG`.
func syncG(tag, group string) string { return tag + "," + group }

// emitGrammarSpec converts an ABNF grammar AST into a tabnas GrammarSpec.
func emitGrammarSpec(grammar *Grammar, opts *ConvertOptions) (spec *tabnas.GrammarSpec, err error) {
	// One emit at a time. `diagPrefix` below is package state, written at
	// the start of every emit and read by forty diagnostics across seven
	// files — so two concurrent conversions raced, and the loser reported
	// the WINNER's notation on an error about its own grammar ("gbnf: rule
	// 'x' ..." for a rule the ABNF author wrote). The race detector calls
	// it on the write; the wrong prefix is what a user would see.
	//
	// A lock rather than a threaded parameter because the alternative is a
	// prefix argument on twenty functions across passes this change has no
	// other business in, and because this is a once-per-grammar-install
	// call: serialising it costs nothing anyone can measure. Not reentrant,
	// and it does not need to be — `emitGrammarSpec` is called only from
	// the facade, never from inside itself.
	//
	// TypeScript needs none of this: its pipeline is synchronous and
	// single-threaded, so the module-scoped `_diagName` there is safe for
	// exactly the reason this comment says it is not safe here. Recorded in
	// doc/differences.md.
	emitMu.Lock()
	defer emitMu.Unlock()

	// A grammar this compiler cannot compile is INVALID USER INPUT, and the
	// contract for that in a Go library is an error return. TS throws a
	// catchable EmitError; the port panicked, so `A = A "y"` took the process
	// down where TypeScript handed you an error object.
	//
	// Three places in this fleet had already reached that conclusion and
	// deferred it — bnf's own test ("the wrong contract for a Go library —
	// invalid user input should be an error return. Recorded rather than
	// changed, since the ABNF front-end's suite pins the current behaviour"),
	// gbnf's emitSafely, and abnf's leftrec test. This closes it at the
	// source rather than in each front-end.
	//
	// ONLY *EmitError is converted. The other panics in this package say
	// "internal — unexpected kind", and those are compiler BUGS, not user
	// input: a bug that returns an error looks like a rejected grammar, and
	// the caller gets a diagnostic about their input for a fault that is
	// ours. Those keep panicking, exactly as an unexpected throw would in TS.
	defer func() {
		if r := recover(); nil != r {
			ee, ok := r.(*EmitError)
			if !ok {
				panic(r)
			}
			spec, err = nil, ee
		}
	}()
	if opts == nil {
		opts = &ConvertOptions{}
	}
	// Diagnostics name the notation the grammar was written in, not this
	// package. Package-level, under `emitMu` above, which is what makes it
	// safe; see that comment. Set BEFORE any pass that can raise a
	// diagnostic, planValueAnnotations included. Mirrors ts/src/compiler.ts.
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

	// Before ANY rewrite: annotations describe the grammar the AUTHOR
	// wrote, and the passes below are what make that shape unrecoverable.
	valuePlan, perr := planValueAnnotations(grammar)
	if perr != nil {
		return nil, perr
	}

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
		// A grammar carrying only removals has no production to take a start
		// rule from. That shape became REACHABLE when cloneGrammar began
		// preserving Remove/ClearAll — before, they were dropped on the clone
		// and resolveProseTerminals rejected the grammar as ruleless first, so
		// this index was never hit. Report it rather than panicking with
		// "index out of range".
		if 0 == len(grammar.Productions) {
			return nil, &EmitError{Message: diagPrefix +
				": grammar has no productions to start from" +
				" (a removal-only grammar needs an explicit start rule)"}
		}
		start = grammar.Productions[0].Name
	}
	tag := opts.Tag
	if tag == "" {
		// "bnf", this package's own name — NOT a notation.
		//
		// This defaulted to "abnf", which is a notation, and AGENTS.md rule
		// 3 says in as many words: "Do not hard-code a notation's tag."
		// Nothing here knows what syntax a grammar was written in, so a
		// caller who omits the tag was getting an emitted `g:"abnf"` that
		// asserted one. TypeScript has always defaulted to 'bnf'
		// (ts/src/compiler.ts, `opts?.tag ?? 'bnf'`).
		//
		// Zero blast radius on the front-ends, measured rather than
		// assumed: abnf, gbnf and ebnf each set their own tag before
		// calling in (abnf/go/bnf_alias.go:88, gbnf/go/facade.go:71,
		// ebnf/go/facade.go:61), so none of them reaches this default. Only
		// a direct caller of this package does, and it was the one being
		// told a falsehood.
		tag = "bnf"
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
	// Which character classes contest a position with another, and the
	// shared partition they are laid over. Filled in below, once every
	// terminal is in view: whether a class becomes one token or a set
	// over atoms depends on what the OTHER classes cover.
	var classes *classAnalysis
	// Named token groups, one per class that spans more than one atom,
	// and the character coverage of each.
	tokenSets := map[string][]string{}
	setRanges := map[string][]charRange{}

	allocRegex := func(el *Element) {
		key := regexKey(el)
		if _, ok := regexTokens[key]; ok {
			return
		}
		name := allocTokenName("rx_"+el.Pattern, usedNames, "")
		regexTokens[key] = name

		// The Go engine gates non-eager match tokens by alt position 0
		// only (the TS engine uses a per-position tcol that covers every
		// alt slot). Marking range regexes eager makes them fire at any
		// lookahead position — equivalent coverage; the parser still
		// rejects a token it doesn't expect at the current slot.
		emit := func(n, pattern, flags string) {
			matchTokens[n] = goRegex(pattern, flags)
			matchEager[n] = true
			matchOrder = append(matchOrder, n)
		}

		if classes == nil || !classes.contested[key] {
			emit(name, el.Pattern, el.Flags)
			return
		}

		// Contested: lay the class over the shared partition. Each atom
		// it covers gets its own match token (minted here if this is the
		// first class to need it, so atoms land in the same allocation
		// order the classes would have had), and the class becomes a
		// token SET over them, under the name it would have had anyway.
		// Downstream, regexTokens still maps a class to a single name —
		// the engine resolves a set name to its tin list when it norms
		// an alternate — so nothing that reads regexTokens has to know
		// the difference, and names derived from it do not move.
		mine := classes.coverage[key]
		var members []string
		for _, span := range classes.atoms {
			covered := false
			for _, r := range mine {
				if r.lo <= span.lo && span.hi <= r.hi {
					covered = true
					break
				}
			}
			if !covered {
				continue
			}
			spanKey := fmt.Sprintf("%d-%d", span.lo, span.hi)
			atom, ok := classes.atomTokens[spanKey]
			if !ok {
				pattern := classPattern(span.lo, span.hi)
				// `rxa_`, not `rx_`: an atom is synthetic, and a name
				// minted from `rx_` collides with the natural name of any
				// class spelling the same span. It did — `%x31-39`'s atom
				// took that name first, so the class itself was pushed to a
				// suffixed one and every name derived from it moved, which
				// is the instability the one-member set below exists to
				// prevent.
				atom = allocTokenName("rxa_"+pattern, usedNames, "")
				emit(atom, pattern, "")
				classes.atomTokens[spanKey] = atom
			}
			members = append(members, atom)
		}

		// A one-member set rather than pointing regexTokens straight at
		// the atom. Redirecting looked tidier and silently renamed
		// things: marks come from altDiscriminator, which reads the token
		// name out of regexTokens, so `[123456789]` took the canonical
		// `[1-9]` atom's name and its `m` — and any `@rule:o:mark` user
		// action attached to it — changed the moment some OTHER
		// production in the grammar mentioned an overlapping `[0-9]`. The
		// class keeps its own name here whatever the partition does
		// underneath it.
		//
		// Keyed WITHOUT the leading `#`. Both engines look a set name
		// up with the `#` stripped (Go `hasTokenSet` trims it outright,
		// TS `findTokenSet` falls back to it), and only the bare key is
		// found by both — a `#`-keyed set is invisible to this engine,
		// which resolved the name to nothing and left every alternate
		// keyed on the class unmatchable.
		tokenSets[strings.TrimPrefix(name, "#")] = members
		setRanges[name] = mine
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
		// A tail repeat's separator is REMOVED from Alts by rewriteTailRepeats
		// and stashed here, so walking Alts alone misses it. Every other
		// terminal in the grammar is reachable from Alts, which is why the
		// omission survived: a separator normally shares its literal with some
		// other rule and picks up that rule's token. When it does not —
		// `list = %x30-39 [ "," list ]`, where the comma appears nowhere else —
		// no token is allocated, the emitted separator alternate comes out as
		// `s: ""`, and the repeat can never match. The grammar then silently
		// accepts one element instead of a list.
		if prod.TailRepeat != nil {
			terminals = append(terminals, prod.TailRepeat.Sep...)
		}
	}

	// Terminals carrying a lifted production name are allocated first, so the
	// name wins even when the same literal also appears inline in an earlier
	// rule (`PL = "+"` must yield `#PL`, not `#T`, regardless of where the
	// bare `"+"` shows up).
	classes = newClassAnalysis(terminals)

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
	cc := newContestCtx(fixedTokens, matchTokens, setRanges)
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

	// Synthetic-rule provenance, accumulated as rules are emitted (see
	// `Production.Origin`). Recorded here rather than derived from the
	// emitted names afterwards: the names compose
	// (`_gen6_star__gen5_group$alt0$step1`), and a front-end's notation may
	// allow `$` in a rule name, so parsing a name back into its parts would
	// be guesswork. Each minting site knows the answer; it just has to say.
	// Nil (not merely empty) when the caller turned provenance off, which
	// is what every recording site tests.
	var prov map[string]string
	if opts.provenanceOn() {
		prov = map[string]string{}
	}

	ruleSpec := map[string]*tabnas.GrammarRuleSpec{}
	for _, prod := range grammar.Productions {
		// Productions synthesised by the rewrite passes (sugar helpers,
		// factored tails, probe branches) are emitted under their own names.
		if prov != nil && originOf(prod) != prod.Name {
			prov[prod.Name] = originOf(prod)
		}
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
			followSets, followPairs, cc, valuePlan, prov); err != nil {
			return nil, err
		}
	}

	// __start__ wrapper consumes #ZZ.
	//
	// The IR reserves no names, so a grammar is free to contain a
	// production actually called that. Assigning unconditionally would
	// overwrite the author's rule — and if it were also the start rule,
	// the wrapper would push itself forever. Fall back to a numbered
	// variant, matching TypeScript.
	startWrapper := "__start__"
	if knownRules[startWrapper] {
		for n := 2; ; n++ {
			cand := fmt.Sprintf("__start%d__", n)
			if !knownRules[cand] {
				startWrapper = cand
				break
			}
		}
	}
	// The wrapper stands in for the start rule, so that is what it is
	// named after: a rule stack reading `__start__` helps nobody. This
	// has to come AFTER the collision check — recording a name the
	// author wrote would claim their rule was generated.
	if prov != nil {
		prov[startWrapper] = start
	}
	bubbleClose := refs.bubble()
	bubbleClose["s"] = "#ZZ"
	// End of source: the one anchor every grammar has, and the last
	// resort for a parse that cannot resynchronise anywhere else.
	bubbleClose["g"] = syncG(tag, "end")
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
	// One group per character class that spans several atoms of the
	// partition. The engine resolves a set name to its tin list while
	// norming an alternate, so an `s` position naming the set matches any
	// atom in it.
	if len(tokenSets) > 0 {
		opt.TokenSet = tokenSets
	}

	spec = &tabnas.GrammarSpec{
		Ref:     refs.refMap(),
		Options: opt,
		Rule:    ruleSpec,
	}

	// Engine-ignored tool metadata (tabnas.GrammarSpec.Meta): the map from
	// each generated rule name to the author-written production it came
	// from. Built over sorted keys, so a serialised grammar is byte-stable
	// across runs and a committed fixture diffs cleanly. Values are held as
	// `any` because that is the shape the serialiser and the cross-runtime
	// JSON door both read.
	if prov != nil && 0 < len(prov) {
		names := make([]string, 0, len(prov))
		for name := range prov {
			names = append(names, name)
		}
		sortStrings(names)
		provenance := make(map[string]any, len(names))
		for _, name := range names {
			provenance[name] = prov[name]
		}
		spec.Meta = map[string]any{"provenance": provenance}
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
			return &EmitError{
				Message: fmt.Sprintf(
					diagName()+": rule '%s' references unknown rule '%s'", ruleName, el.Name),
				Rule: ruleName,
				Sp:   el.Sp,
			}
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
		// The separator continuation of a repetition — the `,` of a comma
		// list, whatever the grammar spells it as. Recovering here drops one
		// bad item and keeps the rest of the list, which is the single most
		// useful resync point a list grammar has. Only the separator's FIRST
		// token becomes the sync point; a multi-token separator syncs on its
		// leading token.
		"g": syncG(tag, "comma"),
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
	followPairs map[string]map[string]map[string]bool, cc *contestCtx,
	valuePlan map[string][]bool, prov map[string]string) error {

	// The token names that came from character classes, for
	// altHeadSharesToken: only between two of these does character
	// overlap mean they can be handed the same token.
	classHeadToks := map[string]bool{}
	for _, t := range regexTokens {
		classHeadToks[t] = true
	}

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
		// A tail repeat is rewritten into a same-depth close-phase loop, so
		// the parts the annotation named are no longer separate pushes to
		// hang members on. Refuse rather than emit a differently-shaped
		// value: this path used to return the AST silently.
		if prod.Value != nil {
			return &EmitError{Rule: originOf(prod), Message: fmt.Sprintf(
				diagName()+": rule '%s' has a value annotation, but it compiles "+
					"to a same-depth repeat, which has no separate parts to "+
					"name. Annotate the rule the repeat pushes instead.",
				originOf(prod))}
		}
		emitTailRepeat(prod, literals, regexTokens, tag, ruleSpec, refs)
		return nil
	}

	allSimple := prod.Value == nil
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

			// `terms… ref` against a sibling that stops on the same head.
			//
			// The alternate as written consumes its own terminals and
			// pushes the reference, so it is chosen on its FIRST token
			// alone — and a sibling that wants to stop there can never be
			// reached, because nothing backtracks once the push has
			// happened. RFC 3986's `dec-octet = DIGIT / %x31-39 DIGIT /
			// …` is the case: `1.2.3.4` entered the two-digit alternate
			// on the `1` and then demanded a second digit of the `.`.
			//
			// The reference is non-nullable here, so the alternate
			// genuinely requires one of its FIRST tokens next. Naming
			// that token in `s` and pushing it straight back (`b: 1`)
			// states the requirement without consuming it, and leaves the
			// shorter sibling reachable. These entries REPLACE the
			// one-token form rather than joining it: keeping both would
			// restore exactly the premature commit, since the bare
			// alternate matches everything the peeked ones do.
			if needsPeek && seg.ref != "" && len(seg.terms) > 0 &&
				!nullable[seg.ref] &&
				altHeadSharesToken(alt, ordered, literals, regexTokens,
					firstSets, nullable, classHeadToks, cc) {
				peek := sortedKeys(firstSets[seg.ref])
				if len(peek) > 0 && len(peek) <= 64 {
					for _, tok := range peek {
						g := copyMap(o)
						g["s"] = strings.Join(append(append([]string{}, seg.terms...), tok), " ")
						g["b"] = 1
						entries = append(entries, dispatchEntry{o: g, alt: alt})
					}
					continue
				}
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
		// Single-alt, multi-segment: chain rules directly on the production.
		return emitChain(prod.Name, prod.Alts[0], literals, regexTokens, tag,
			ruleSpec, refs, prodKind, prov, originOf(prod),
			prod.Value, valuePlan[originOf(prod)])
	}

	// A value annotation names one member per pushing segment, which only
	// has one reading when the production HAS one alternative. Two
	// alternatives push different things in different orders, so the same
	// list of names would mean something different down each — and
	// silently building a different shape depending on which alternative
	// matched is worse than refusing.
	if prod.Value != nil {
		return &EmitError{
			Message: fmt.Sprintf(
				diagName()+": rule '%s' has a value annotation and %d alternatives. A "+
					"value annotation names the parts of ONE alternative; with more "+
					"than one it is ambiguous which alternative's parts are named. "+
					"Split the rule, or annotate the alternatives' own rules.",
				originOf(prod), len(prod.Alts)),
			Rule: originOf(prod),
		}
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

		// One impl rule per alternative of a multi-segment dispatch: the
		// author wrote one rule with alternatives, not N rules. Recorded
		// here, beside the emission, because an EMPTY alternative continues
		// above without emitting anything — claiming a rule that does not
		// exist is worse than omitting one that does.
		if prov != nil {
			prov[implName] = originOf(prod)
		}

		if err := emitChain(implName, alt, literals, regexTokens, tag, ruleSpec,
			refs, "helper", prov, originOf(prod), nil, nil); err != nil {
			return err
		}

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
				return &EmitError{
					Message: fmt.Sprintf(
						diagName()+": rule '%s' alternative %d is nullable "+
							"but is not the only empty alt; FIRST set is ambiguous",
						prod.Name, i),
					Rule: prod.Name,
					Sp:   prod.Sp,
				}
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
// prov / origin are the provenance map and the author-written rule the
// chain belongs to; both nil/empty when the caller has no attribution to
// give (provenance turned off).
func emitChain(headName string, alt Sequence, literals, regexTokens map[string]string,
	tag string, ruleSpec map[string]*tabnas.GrammarRuleSpec, refs *refRegistry,
	headKind string, prov map[string]string, origin string,
	// nested is one flag per pushing part of alt, from
	// planValueAnnotations: does that part's own rule build a value, and
	// so nest whole rather than resolve to its source text?
	//
	// An argument rather than package state, which is where it started:
	// there, two concurrent EmitGrammarSpec calls overwrote each other's
	// flags mid-emit and built each other's values. emitMu would now cover
	// that as it covers diagPrefix, but this table is read in exactly ONE
	// place, so passing it costs two arguments and leaves nothing shared —
	// which is the better answer wherever it is affordable. It is not
	// affordable for diagPrefix; see the comment on emitMu.
	value *ValueAnnotation, nested []bool) error {

	segs := segmentize(alt, literals, regexTokens)
	chainName := func(i int) string {
		if i == 0 {
			return headName
		}
		return fmt.Sprintf("%s$step%d", headName, i)
	}

	// Value building rides on the segments that PUSH: those are the parts
	// that produce a member. A segment of bare terminals (a separator, a
	// trailing literal) consumes input and contributes no value.
	memberSegs := make([]bool, len(segs))
	if value != nil {
		for i, sg := range segs {
			memberSegs[i] = sg.ref != ""
		}
	}
	memberAt := func(i int) int {
		if !memberSegs[i] {
			return -1
		}
		n := 0
		for j := 0; j < i; j++ {
			if memberSegs[j] {
				n++
			}
		}
		return n
	}

	diagRule := origin
	if diagRule == "" {
		diagRule = headName
	}

	// An unknown kind must not fall through as an object. Kind is an
	// unrestricted string in the public IR, so a typo like "arry" would
	// otherwise skip the member-count check below (which asks for exactly
	// "object") and emit object actions with no keys.
	if value != nil && value.Kind != "object" && value.Kind != "array" {
		return &EmitError{
			Message: fmt.Sprintf(
				diagName()+": rule '%s' has a value annotation of unknown kind "+
					"'%s'. A rule builds an 'object' or an 'array'.",
				diagRule, value.Kind),
			Rule: diagRule,
		}
	}

	// One name per pushing segment, or the names line up with the wrong
	// parts. The rewrite passes are why this is checked here and not at
	// annotation time: inlining a leading reference can change how many
	// segments push (a member whose rule is all terminals stops being a
	// push at all), and left-recursion elimination restructures the
	// alternative wholesale. Either way the author's names would land on
	// the wrong members, and silently building a differently-shaped value
	// is worse than refusing.
	if value != nil && value.Kind == "object" {
		pushes := 0
		for _, m := range memberSegs {
			if m {
				pushes++
			}
		}
		named := len(value.Members)
		if named != pushes {
			plural := "s"
			if named == 1 {
				plural = ""
			}
			return &EmitError{
				Message: fmt.Sprintf(
					diagName()+": rule '%s' names %d member%s but builds %d. A value "+
						"annotation names one member per part that produces a value. A "+
						"part made only of literals produces none — and note that a "+
						"part whose own rule is a single terminal stops being a part "+
						"here in two ways: as a LEADING part it is folded into this "+
						"rule by left-recursion elimination, and anywhere else it "+
						"becomes a named lexer token. Giving that rule a body that is "+
						"not a bare terminal (a repetition, a group, or more than one "+
						"element) keeps it nameable.",
					diagRule, named, plural, pushes),
				Rule: diagRule,
			}
		}
	}

	// The same count, checked against the PLAN rather than the names —
	// because an array has no names, and so had nothing checking it at
	// all. nested is indexed by member position, so a plan that is a
	// different length from the segments is a plan whose flags are on the
	// wrong members: an array whose second element's rule was lifted to a
	// terminal pushed the THIRD element's flag onto it, which nested an
	// internal tree node into the value instead of its source text.
	// Silent, and visible only in the output.
	if value != nil && nested != nil {
		pushes := 0
		for _, m := range memberSegs {
			if m {
				pushes++
			}
		}
		if len(nested) != pushes {
			plural := "s"
			if len(nested) == 1 {
				plural = ""
			}
			return &EmitError{
				Message: fmt.Sprintf(
					diagName()+": rule '%s' has a value annotation for %d part%s "+
						"but builds %d. A rewrite pass changed how many parts of "+
						"this rule produce a value, so the annotation no longer "+
						"describes it. A part whose own rule is a single literal is "+
						"the usual cause: it becomes a lexer token here and stops "+
						"being a part at all. Give that rule a body that is not a "+
						"bare terminal.",
					diagRule, len(nested), plural, pushes),
				Rule: diagRule,
			}
		}
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

		// Step rules exist only because the alternative had more than one
		// segment; nothing in the author's grammar is named after them.
		if 0 < i && prov != nil && origin != "" {
			prov[name] = origin
		}

		isLast := i == len(segs)-1
		var closeMaps []map[string]any
		if !isLast {
			close := map[string]any{"r": chainName(i + 1), "g": tag}
			merge(close, captureChildFields(refs, name, kind))
			closeMaps = []map[string]any{close}
		} else if seg.ref != "" {
			close := captureChildFields(refs, name, kind)
			close["g"] = tag
			closeMaps = []map[string]any{close}
		}

		if value != nil {
			isArray := value.Kind == "array"
			m := memberAt(i)
			memberName := ""
			if 0 <= m && m < len(value.Members) {
				memberName = value.Members[m]
			}

			// Open side: the container is allocated once, on the head,
			// before anything goes into it; every pushing link names the
			// member it is about to fill.
			openActs := []string{}
			openCfg := map[string]any{}
			if i == 0 {
				if isArray {
					openActs = append(openActs, "@array$")
				} else {
					openActs = append(openActs, "@object$")
				}
			}
			if !isArray && memberName != "" {
				// The key is a CONSTANT: the author named this part, and no
				// token in the input carries that text.
				openActs = append(openActs, "@key$")
				openCfg["key$"] = map[string]any{"lit": memberName}
			}
			// Unconditional, even when this link gains no action of its
			// own: EVERY link of a value rule must lose its tree builders,
			// not just the ones that gain value builders. An array's steps
			// gain nothing on the open side (there are no member names to
			// set), and leaving their @node$ in place had it accumulate the
			// separator's text into a `src` property on the ARRAY.
			useValueActions(headAlt, openActs, openCfg)

			// Close side: a member that builds its OWN value is assigned
			// whole, so it nests; anything else resolves to the source text
			// its tree builders accumulated. Omitting `src` IS the nesting
			// case — see @tabnas/parser doc/value-builtins.md, v5.
			// Nesting is decided from the PUSHED RULE, not the member name.
			// An array names nothing, so keying off the name meant an array
			// could never nest at all: an element whose own rule builds a
			// value was flattened to its source text instead of pushed
			// whole. The name is still consulted for an object, because a
			// leading member's rule is inlined away and only the annotation
			// still knows what it was.
			if 0 <= m {
				isNested := m < len(nested) && nested[m]
				act, cfgKey := "@setval$", "setval$"
				if isArray {
					act, cfgKey = "@push$", "push$"
				}
				var cfg map[string]any
				if !isNested {
					cfg = map[string]any{cfgKey: map[string]any{"src": true}}
				}
				if closeMaps == nil {
					closeMaps = []map[string]any{{"g": tag}}
				}
				for _, c := range closeMaps {
					useValueActions(c, []string{act}, cfg)
				}
			} else {
				// A link that pushes nothing (a bare separator or trailing
				// literal) contributes no member — but its close still
				// carries a tree @capture$ for a node this rule no longer
				// has.
				for _, c := range closeMaps {
					useValueActions(c, nil, nil)
				}
			}
		}

		rs.Open = mapsToAlts([]map[string]any{headAlt})
		if closeMaps != nil {
			rs.Close = mapsToAlts(closeMaps)
		}
		ruleSpec[name] = rs
	}
	return nil
}

// useValueActions installs the value builders on an alt of a rule that
// BUILDS A VALUE, dropping the tree builders it was emitted with.
//
// They cannot compose: both own `r.node`. Appending @object$ after
// @node$ clobbers the AST node that was just allocated, and the
// @capture$ on the matching close then finds no `kids` to push into and
// dies. On a rule that builds a value the value builders win outright,
// which is also what the author asked for — the tree node is exactly the
// thing they said they did not want.
//
// The MEMBERS keep their tree builders: `src` reads the node.src those
// accumulate, and members are separate rules, so nothing here touches
// them. Mirrors the TS `useValueActions`.
func useValueActions(spec map[string]any, actions []string, cfg map[string]any) {
	if len(actions) == 1 {
		spec["a"] = actions[0]
	} else if len(actions) > 1 {
		spec["a"] = actions
	} else {
		delete(spec, "a")
	}
	if k, ok := spec["k"].(map[string]any); ok {
		// Builtins mode names its tree config; closure mode carries none.
		delete(k, "node$")
		delete(k, "capture$")
		for name, v := range cfg {
			k[name] = v
		}
		if len(k) == 0 {
			delete(spec, "k")
		}
	} else if len(cfg) > 0 {
		spec["k"] = cfg
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
