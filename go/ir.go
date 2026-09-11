// Copyright (c) 2026 tabnas, MIT License

package bnf

import (
	"fmt"
	"sort"
	"strconv"
	"sync"
)

// ---- ABNF AST -------------------------------------------------------
//
// The parsed ABNF grammar is a list of productions, each an alternation
// of sequences of elements. Element kinds mirror the TS AbnfElement
// union; Go uses a single struct tagged by Kind plus optional fields.

// ElemKind is the discriminator for a Element.
type ElemKind string

const (
	KindTerm  ElemKind = "term"
	KindRef   ElemKind = "ref"
	KindRegex ElemKind = "regex"
	KindOpt   ElemKind = "opt"
	KindStar  ElemKind = "star"
	KindPlus  ElemKind = "plus"
	KindRep   ElemKind = "rep"
	KindGroup ElemKind = "group"
	// KindToken is an engine builtin lexer token (e.g. #TX/#NR/#ST/#VL),
	// produced by normalizeBuiltinTokens. Its token name is held in Name and
	// is emitted verbatim into a rule's token sequence (no allocation, unlike
	// a literal term).
	KindToken ElemKind = "token"
	// KindProse is an RFC 5234 prose-val (`<free text>`). Prose is
	// informational: it describes a terminal in English rather than defining
	// one. The converter accepts it only as the entire body of a production
	// naming a builtin lexer token (`NR = <number>`), where it documents the
	// token the lexer already provides; resolveProseTerminals then drops the
	// production so refs resolve to that builtin. Anywhere else there is
	// nothing to compile, and it is an error. Text holds the prose body.
	KindProse ElemKind = "prose"
)

// SrcSpan is where an IR node came from in the front-end's grammar text.
//
//	S  start offset, inclusive
//	E  end offset, exclusive
//	R  row of the start, 1-based (optional)
//	C  column of the start, 1-based (optional)
//
// Offsets and row/column are in the SAME UNITS the front-end's own
// engine tokens use, so a front-end copies `sI`/`rI`/`cI` straight
// across with no arithmetic — the step where an off-by-one would
// otherwise creep in. That does mean the units are runtime-native and
// not identical across ports: Go offsets count BYTES and TypeScript's
// count UTF-16 code units, the same divergence the engine already
// records for token positions. A consumer that needs LSP positions
// converts at the LSP boundary, where the document's encoding is known;
// nothing here can do that conversion correctly, because the IR does
// not hold the source text.
//
// R and C are 1-based, so a zero in either means "not recorded" — there
// is no row 0. The span ITSELF is optional a level up: `Element.Sp` and
// `Production.Sp` are POINTERS, because `SrcSpan{S: 0, E: 0}` is a
// legitimate empty span at the very start of a file and must not read
// as "no span".
//
// Spans are optional everywhere. A front-end that records them gets
// ranged compile errors (see `EmitError.Sp`); one that does not
// compiles to exactly the same grammar. Mirrors the TS `SrcSpan`.
type SrcSpan struct {
	S int
	E int
	R int
	C int
}

// Element is one element of an ABNF sequence (a term, ref, regex, or
// EBNF sugar). Mirrors the TS AbnfElement union.
type Element struct {
	Kind ElemKind

	// Sp is where this element came from in the grammar source
	// (front-end populated, nil when unrecorded). Rewrite passes share
	// element objects by reference — cloneGrammar copies productions and
	// alt slices but not the elements themselves — so a span recorded at
	// parse time survives all the way to the emitter. Elements the
	// compiler synthesises for itself (a group wrapper around
	// left-recursion seeds, say) carry none, which is correct: the author
	// wrote no such group. Mirrors the TS `Element.sp`.
	Sp *SrcSpan

	// term
	Literal       string
	CaseSensitive bool // explicit %s flag (ABNF strings are insensitive by default)
	HasCaseSens   bool // whether CaseSensitive was set explicitly (TS optional flag)
	// TokenName is the preferred lexer token name, set by liftLiteralTokens
	// when this terminal came from a production that names it (`PL = "+"` ->
	// `#PL`). Without it the emitter derives a name from the literal text,
	// which for punctuation degrades to `#T`, `#T1`, …
	TokenName string

	// prose
	Text string

	// NumErr carries a deferred diagnostic from parseNumericValue: an
	// alt-action has no error return, and panicking is no good either
	// because the engine turns a panic into its own `tabnas/internal`
	// wrapper. So the element is built anyway and parseAbnf reports this
	// message once the parse is structurally complete. Unexported: it is an
	// internal signal, not part of the AST.
	NumErr string

	// regex
	Pattern string
	Flags   string

	// ref
	Name string
	// Debt holds the suffix-debt counter mutations to emit on the alt that
	// pushes this reference (`n: {<counter>: 1|0}`). Written by
	// resolveSuffixDebts; see that pass for what the counter means. Nil on
	// every reference in a grammar with no contested tail loop, which is all
	// of them until one is detected. Mirrors the TS `debt` field.
	Debt map[string]int

	// opt / star / plus / rep
	Inner *Element
	Min   int
	Max   int // MaxInfinity for unbounded
	// DebtGuard names the suffix-debt counter guarding a star, set by
	// eliminateDirectLeftRec on the tail loop it generates. desugar carries
	// it onto the helper production the star becomes; resolveSuffixDebts
	// then confirms or drops it. Mirrors the TS `debtGuard` field.
	DebtGuard string

	// group
	Alts []Sequence
}

// MaxInfinity stands in for the TS `Infinity` upper bound on repetition.
const MaxInfinity = 1 << 30

type Sequence []*Element

// ProbeDispatchSpec configures a synthesised dispatcher production for
// an ambiguous `[X D] Y` subsequence.
type ProbeDispatchSpec struct {
	ProbeRule     string
	Disambiguator *Element
	WithBranch    string
	NoBranch      string
}

// ProbeHelperSpec carries the vocabulary for a synthesised probe helper.
type ProbeHelperSpec struct {
	VocabElements []*Element
}

// nodeKind controls how a production contributes to the output AST:
//   - "user": emit a tagged node {rule, src, kids}.
//   - "core": RFC 5234 core rules — flatten into the enclosing src.
//   - "helper": synthetic sugar / dispatcher / chain rules — flatten.
//
// Empty is treated as "user".

type Production struct {
	Name        string
	Alts        []Sequence
	Incremental bool
	ProbeDisp   *ProbeDispatchSpec
	ProbeHelper *ProbeHelperSpec
	// TailRepeat is set by rewriteTailRepeats on a production of the
	// shape `X = prefix [ sep X ]` (all-terminal prefix and separator,
	// self-ref last). The opt is removed from Alts (leaving just the
	// prefix) and the separator elements are stashed here; the emitter
	// compiles the production to a same-depth close-phase repeat
	// (`r: X`) instead of the opt→group→push helper chain. Mirrors the
	// TS `tailRepeat` flag.
	TailRepeat *TailRepeatSpec
	// DebtGuard is set by desugar on the star helper generated for a
	// left-recursion tail loop whose greediness contests a suffix of the
	// rule it was derived from, and confirmed by resolveSuffixDebts. Names
	// the counter whose value must be zero for the loop to keep going.
	// Mirrors the TS `debtGuard` production flag.
	DebtGuard string
	// DebtOwed lists the loop's own FIRST tokens that an enclosing suffix
	// can actually compete for, set by resolveSuffixDebts alongside
	// DebtGuard. Only the branches that could eat one of these are guarded:
	// a multi-tail loop (`A = A "y" / A "w" / "x" A "y" / "z"`) owes a `"y"`
	// and nothing else, so blocking its `"w"` branch as well would reject
	// `xzwy`. Mirrors the TS `debtOwed` production flag.
	DebtOwed []string
	NodeKind string // "", "user", "core", "helper"

	// RepeatHelper marks a synthetic production standing in for a
	// repetition (`opt`/`star` and the tails of `plus`/`rep`), and the
	// nullable tail helpers left factoring creates. Their terminating
	// alternative is EMPTY, so it names no token — and the engine only
	// offers a matcher at a position where the active rule names it.
	// The emitter therefore guards that alternative with a FOLLOW-set
	// peek, without which a repetition followed by a character class
	// cannot terminate. See computeFollowSets.
	RepeatHelper bool

	// Origin is the author-written production this one descends from. Set
	// by every pass that SYNTHESISES a production (desugar's sugar
	// helpers, left factoring's `$fact` tails, the probe rewriter's
	// dispatch branches) to the origin of the production being rewritten.
	// EMPTY means the production is itself author-written — so the source
	// rule is always `Origin or Name`, which is what originOf returns.
	//
	// A compiled grammar carries an order of magnitude more rules than the
	// author wrote (a 12-production ABNF grammar emits 118), and every one
	// of the extra names surfaces in rule stacks, hover and completion.
	// Carrying the origin is what lets emitGrammarSpec export the map back
	// out (`spec.Meta["provenance"]`) so a tool can name the user's rule
	// instead of the machinery's. Mirrors the TS `Production.origin`.
	Origin string

	// Sp is where the author wrote this production, when the front-end
	// records it. Element spans locate a term or a reference; this locates
	// the rule as a whole, which is what an outline entry or a
	// go-to-definition on a rule name needs. Synthesised productions carry
	// none — Origin is how they are located, by naming the rule they
	// descend from.
	//
	// A pointer, not a value: see SrcSpan. Nil means "not recorded", which
	// a zero-valued span cannot mean.
	//
	// CAUTION: every pass that REBUILDS a production field by field has to
	// carry this across, exactly as it carries Origin — a `&Production{…}`
	// that forgets it silently drops the span. The passes that copy with
	// `cp := *p` get it for free. Mirrors the TS `Production.sp`.
	Sp *SrcSpan

	// Value is what this production BUILDS, when a front-end says it
	// builds something, rather than the AST node the tree builders
	// produce by default.
	//
	// Notation-neutral by construction: it says WHAT to build, never how
	// the notation spelled it. ABNF carries it in a trailing comment, but
	// nothing here knows that, and a front-end for another notation can
	// set the same field from whatever syntax it likes.
	//
	// CAUTION applies here exactly as it does to Sp above, and it is not
	// hypothetical: the TypeScript side of this shipped with SIX passes
	// silently dropping the annotation, so a rule that declared a value
	// quietly built a tree instead. Mirrors the TS `Production.value`.
	Value *ValueAnnotation
}

// ValueAnnotation is what a production builds when it carries one.
//
// Members names one member per PUSHING segment of the alternative, in
// order — the parts the author named. The names matter because the
// emitter cannot recover them from the chain: a production's leading
// reference is INLINED by the left-recursion pass, so the first member
// pushes a generated helper rather than the rule the author wrote. The
// annotation naming its own parts is what makes this independent of that
// rewrite. Mirrors the TS `ValueAnnotation`.
//
// An array has no member names: every pushing segment is an element.
type ValueAnnotation struct {
	Kind    string // "object" or "array"
	Members []string
}

// originOf is the author-written production a (possibly synthesised)
// production descends from. Synthetic productions carry Origin; an
// author-written one is its own origin. Always read Origin through this —
// a bare `p.Origin` is empty for exactly the productions whose name is
// already the answer. Mirrors the TS `originOf`.
func originOf(prod *Production) string {
	if prod.Origin == "" {
		return prod.Name
	}
	return prod.Origin
}

type TailRepeatSpec struct {
	Sep Sequence
}

func (p *Production) kind() string {
	if p.NodeKind == "" {
		return "user"
	}
	return p.NodeKind
}

type Grammar struct {
	Productions []*Production
	Ambiguities []AmbiguityReport

	// `<remove>` directives. Remove names rules/tokens to drop; ClearAll is
	// `<all> = <remove>`, which wipes the instance first. Mirrors the TS
	// Grammar `remove` / `clearAll` fields.
	Remove   []string
	ClearAll bool
}

type AmbiguityReport struct {
	Rule     string
	AltIdx   int
	OptIdx   int
	Reason   string
	Resolved bool
}

// ConvertOptions controls emission. Each front-end passes its own Tag so
// emitted alts stay attributable to the notation they came from.
type ConvertOptions struct {
	Start    string
	Tag      string
	Builtins bool
	Marks    bool
	// WordKeywords makes a literal ending in a word character match only as a
	// whole word: it is emitted as an anchored regex with a trailing `\b`
	// guard so e.g. `option` does not match inside `optional`. Mirrors the TS
	// `wordKeywords` option (which uses a `(?![A-Za-z0-9_])` lookahead; the Go
	// engine's RE2 has no lookahead, so `\b` — equivalent here — is used).
	WordKeywords bool
	// Provenance emits `Meta["provenance"]` — the map from each generated
	// rule name back to the author-written production it came from (see
	// `Production.Origin`). DEFAULT TRUE, hence the pointer: the names are
	// otherwise unattributable, and every tool that shows a rule name to a
	// human needs it. Point it at false to keep an embedded grammar as
	// small as possible. Mirrors the TS `provenance?: boolean`, which is
	// likewise on unless explicitly `false`.
	Provenance *bool
}

// provenanceOn reports whether the provenance map should be emitted:
// absent means ON, so a caller writing `&ConvertOptions{Tag: "x"}` gets
// the same answer as TypeScript's `{tag: 'x'}`. Only an explicit `false`
// turns it off.
func (o *ConvertOptions) provenanceOn() bool {
	return o == nil || o.Provenance == nil || *o.Provenance
}

// ParseError is raised by the shared compiler for a grammar the IR
// cannot express. Front-ends wrap or restamp it as they see fit.
type ParseError struct {
	Message string
	Line    int
	Column  int
	Cause   error
}

func (e *ParseError) Error() string { return e.Message }
func (e *ParseError) Unwrap() error { return e.Cause }

// EmitError is a compile failure that can say WHERE. Every diagnostic
// this compiler raises used to be a bare message whose only structure
// was the `diagName():` prefix, so a caller wanting to underline the
// offending text had nothing to read and had to parse the message.
//
// Sp is populated only when the offending IR node carries a span, which
// means only when the front-end recorded one — so this is a strict
// improvement on every path and a change of behaviour on none. It
// implements `error` and the message text is unchanged, so existing
// error handling and message assertions keep working.
//
// It sits alongside ParseError rather than replacing it, mirroring
// TypeScript: there, five author-facing throw sites became `EmitError`
// and the other eleven stayed a plain `Error`. The same five sites raise
// this here, and the rest still raise *ParseError or a bare
// `fmt.Errorf`.
//
// NOTE one of those five (`eliminateDirectLeftRec`'s purely-left-
// recursive rule) PANICS in Go where TypeScript throws — inherited
// behaviour the ABNF front-end's suite pins. It panics with a
// *EmitError VALUE rather than a string precisely so the span survives
// the panic: a `recover()` that type-asserts gets the span, where one
// that only stringifies gets what it always got.
type EmitError struct {
	Message string
	// Rule is the rule being compiled when the failure was raised.
	Rule string
	// Sp is where in the grammar source, when the IR knew. Nil when the
	// front-end recorded no span for the offending node.
	Sp    *SrcSpan
	Cause error
}

func (e *EmitError) Error() string { return e.Message }
func (e *EmitError) Unwrap() error { return e.Cause }

// diagPrefix names the NOTATION a grammar was written in, not this
// package: a front-end's users should not see "bnf:" on an error about
// their own syntax. EmitGrammarSpec sets it from ConvertOptions.Tag for
// the duration of one emit, holding emitMu. Mirrors `_diagName` in
// ts/src/compiler.ts.
var diagPrefix = "bnf"

// emitMu serialises emitGrammarSpec, which is what makes the package
// state above safe to write per-emit. See the comment at the top of
// emitGrammarSpec for why a lock and not a threaded parameter.
var emitMu sync.Mutex

func diagName() string { return diagPrefix }

// planValueAnnotations validates every value annotation against the
// AUTHORED grammar and works out which members nest — both BEFORE any
// rewrite runs.
//
// The rewrites are exactly why this cannot wait for emit time. Paull's
// pass INLINES a leading reference, so by then the first member's rule is
// gone and the segments no longer correspond to the parts the author
// named: a member whose rule carried a trailing literal loses it, and a
// member whose rule pushed twice silently becomes two members. The shape
// the AUTHOR wrote is the only place those are still visible.
//
// Nesting is decided by POSITION, not by looking a member name up as a
// rule name. An array names nothing at all, so a name-based rule could
// never nest an array element. Mirrors the TS `planValueAnnotations`.
func planValueAnnotations(grammar *Grammar) (valuePlan, error) {
	byName := map[string]*Production{}
	for _, p := range grammar.Productions {
		byName[p.Name] = p
	}
	plan := valuePlan{
		plan:    map[string][]bool{},
		collect: map[string][]bool{},
	}

	// Nothing annotated means nothing to predict, and this is the common
	// case by a wide margin — every grammar in the conformance corpus.
	anyValue := false
	for _, p := range grammar.Productions {
		if p.Value != nil {
			anyValue = true
			break
		}
	}
	if !anyValue {
		return plan, nil
	}

	// Which productions keep their leading reference. Computed on the
	// AUTHORED grammar for the same reason everything else here is.
	cyclic := findLeadingRefCycleMembers(grammar.Productions)

	// Inlining an annotated rule erases the builders it was going to run,
	// and that happens wherever the rule is a LEADING reference — not only
	// where the caller is itself annotated. `top = leaf ","` with an
	// annotated `leaf` silently handed back an ordinary AST, because
	// nothing looked at `top` at all: it names no members, so the loop
	// below skipped it.
	//
	// Every alternative, not just the first: Paull's pass substitutes into
	// any alternative whose leading element is a reference.
	for _, prod := range grammar.Productions {
		if exemptAlias(prod, cyclic) {
			continue
		}
		for _, alt := range prod.Alts {
			if len(alt) == 0 || alt[0].Kind != KindRef {
				continue
			}
			_, hit := resolveLeadingFold(alt[0].Name, byName)
			if hit == "" {
				continue
			}
			return valuePlan{}, &EmitError{Rule: prod.Name, Sp: prod.Sp, Message: fmt.Sprintf(
				diagName()+": rule '%s' begins with '%s', and '%s' is folded "+
					"into '%s' by left-recursion elimination — which erases the "+
					"value '%s' is annotated to build, so nothing would produce "+
					"it. Put a literal before '%s', or remove the annotation on "+
					"'%s'.",
				prod.Name, alt[0].Name, hit, prod.Name, hit, alt[0].Name, hit)}
		}
	}

	for _, prod := range grammar.Productions {
		v := prod.Value
		if v == nil {
			continue
		}
		// Sp when the front-end recorded one: a caller that wants to
		// underline the offending rule needs the span, and these refusals
		// are the ones an AUTHOR is most likely to hit — they are about
		// what they wrote, not about anything the compiler derived.
		sp := prod.Sp
		if v.Kind != "object" && v.Kind != "array" {
			return valuePlan{}, &EmitError{Rule: prod.Name, Sp: sp, Message: fmt.Sprintf(
				diagName()+": rule '%s' has a value annotation of unknown kind "+
					"'%s'. A rule builds an 'object' or an 'array'.",
				prod.Name, v.Kind)}
		}
		if len(prod.Alts) != 1 {
			return valuePlan{}, &EmitError{Rule: prod.Name, Sp: sp, Message: fmt.Sprintf(
				diagName()+": rule '%s' has a value annotation and %d "+
					"alternatives. A value annotation names the parts of ONE "+
					"alternative; with more than one it is ambiguous which "+
					"alternative's parts are named. Split the rule, or annotate "+
					"the alternatives' own rules.", prod.Name, len(prod.Alts))}
		}

		// An empty member name is not a name. Go's Members is []string, so
		// it cannot hold the nil the TypeScript IR can — but "" reaches
		// here from either, and the two ports DISAGREED about it: TS built
		// the key "", Go skipped the @key$ entirely and let @setval$ write
		// into whatever key the previous part had left in the slot.
		seenMember := map[string]bool{}
		for _, m := range v.Members {
			if m == "" {
				return valuePlan{}, &EmitError{Rule: prod.Name, Sp: sp, Message: fmt.Sprintf(
					diagName()+": rule '%s' has a value annotation naming a "+
						"member that is not a name (\"\"). Every member of an "+
						"object is named by a non-empty string.", prod.Name)}
			}
			// Each member is a separate KEY. Two parts named the same thing
			// both write to it, so the second silently overwrites the first
			// and that part's match is simply absent from the result.
			if seenMember[m] {
				return valuePlan{}, &EmitError{Rule: prod.Name, Sp: sp, Message: fmt.Sprintf(
					diagName()+": rule '%s' names the member '%s' twice. Each "+
						"member is a separate key, so the second part would "+
						"overwrite the first. Give them different names.",
					prod.Name, m)}
			}
			seenMember[m] = true
			// srcField is how a parse-tree node is told apart from a value:
			// the close-phase capture asks whether the returned child has
			// one. So a value that HAS such a member is taken for a node —
			// its text is folded into the parent's src and the object itself
			// is never added as a kid, which loses it outright wherever a
			// tree rule captures it. Measured, not assumed: "rule" and
			// "kids" as member names are captured correctly and stay
			// allowed.
			if m == srcField {
				return valuePlan{}, &EmitError{Rule: prod.Name, Sp: sp, Message: fmt.Sprintf(
					diagName()+": rule '%s' names a member '%s'. That name is "+
						"how a value is told apart from a parse-tree node, so a "+
						"value carrying it is mistaken for a node and dropped "+
						"wherever an ordinary rule captures this one. Name the "+
						"member something else.", prod.Name, srcField)}
			}
		}

		alt := prod.Alts[0]

		// resolveProseTerminals drops an informational prose definition
		// (`NR = <number>`) outright, so references to it become the
		// built-in token and this rule is gone before anything could build
		// its value. Annotating one is a mistake with no reading, and it
		// was silently discarded.
		if len(alt) == 1 && alt[0].Kind == KindProse {
			return valuePlan{}, &EmitError{Rule: prod.Name, Sp: sp, Message: fmt.Sprintf(
				diagName()+": rule '%s' has a value annotation, but its body "+
					"is prose ('<%s>'), which describes a built-in token rather "+
					"than defining a rule — the rule is dropped, so nothing "+
					"would build the value. Remove the annotation.",
				prod.Name, alt[0].Text)}
		}

		// Every part that PUSHES is a member, not every part that is a
		// reference. A group or a repetition becomes a reference to a
		// generated helper before the emitter sees it, so it pushes exactly
		// like a written reference does — and counting only references left
		// the flags below indexed by a different sequence from the one the
		// emitter walks. `(plain) inner` then applied `inner`'s flag to the
		// GROUP, pushing an internal tree node into the value.
		var parts []*Element
		for _, el := range alt {
			if pushesValue(el) {
				parts = append(parts, el)
			}
		}

		if v.Kind == "array" {
			// An array's parts are positional; there is nothing for a name
			// to attach to. Silently ignoring the names hid the real
			// mistake, which is that the author meant `object`.
			if len(v.Members) > 0 {
				plural := "s"
				if len(v.Members) == 1 {
					plural = ""
				}
				return valuePlan{}, &EmitError{Rule: prod.Name, Sp: sp, Message: fmt.Sprintf(
					diagName()+": rule '%s' builds an array and names %d "+
						"member%s. An array's parts are positional and are not "+
						"named; annotate it as an object to name them.",
					prod.Name, len(v.Members), plural)}
			}
		} else {
			named := len(v.Members)
			if named != len(parts) {
				plural, verb := "s", "s that produce"
				if named == 1 {
					plural = ""
				}
				if len(parts) == 1 {
					verb = " that produces"
				}
				return valuePlan{}, &EmitError{Rule: prod.Name, Sp: sp, Message: fmt.Sprintf(
					diagName()+": rule '%s' names %d member%s but has %d part%s "+
						"a value. A value annotation names one member per part "+
						"that produces a value; a literal produces no value "+
						"and is not a member.",
					prod.Name, named, plural, len(parts), verb)}
			}
		}

		// The LEADING reference is folded into this rule by left-recursion
		// elimination. Its rule has to reduce to exactly one part, or the
		// boundary the author drew is lost — a trailing literal disappears
		// from the member, and a second reference silently becomes a second
		// member. Both are wrong VALUES rather than errors, so refuse.
		//
		// Unless this rule is a pure alias, which that pass does not
		// substitute into at all: `top = child` keeps its reference, so the
		// chain allocates `top`'s container and assigns `child`'s value
		// whole. Refusing that was a refusal of a shape that works.
		//
		// The annotated half of this — a leading member whose own rule
		// builds a value — is caught by the scan above, which asks the same
		// question of every production rather than only of this one. Only
		// the shape check is left here.
		if len(alt) > 0 && alt[0].Kind == KindRef && !exemptAlias(prod, cyclic) {
			if ok, _ := resolveLeadingFold(alt[0].Name, byName); !ok {
				return valuePlan{}, &EmitError{Rule: prod.Name, Sp: sp, Message: fmt.Sprintf(
					diagName()+": rule '%s' names '%s' as its first member, but "+
						"'%s' is folded into this rule by left-recursion "+
						"elimination and its body is not a single part, so the "+
						"member would not cover what the author wrote. Give '%s' "+
						"a body that is one part (a reference, a repetition or a "+
						"group), or put a literal before it.",
					prod.Name, alt[0].Name, alt[0].Name, alt[0].Name)}
			}
		}

		flags := make([]bool, len(parts))
		for i, el := range parts {
			if el.Kind != KindRef {
				continue
			}
			if p, ok := byName[el.Name]; ok && p.Value != nil {
				flags[i] = true
			}
		}

		// A member that is not itself annotated resolves to the source text
		// its tree builders accumulated — which a rule that builds a value
		// does not contribute to. Refuse rather than hand back the hole:
		// see reachesAnnotated.
		for i, el := range parts {
			if flags[i] {
				continue
			}
			hit := reachesAnnotated(el, byName, map[string]bool{})
			if hit == "" {
				continue
			}
			// A repetition under an array is COLLECTED, not taken as
			// text, so the question below has to be asked again of each
			// thing its helper pushes rather than of the run as a whole.
			// Reaching this rule ITSELF is still refused — that nests the
			// rule's own array inside itself, which is a shape nobody
			// writing a list means.
			if v.Kind == "array" && collectsValues(el) && hit != prod.Name {
				hit = hiddenAnnotated(el, byName)
				if hit == "" {
					continue
				}
			}
			which := fmt.Sprintf("element %d", i+1)
			if v.Kind != "array" {
				which = fmt.Sprintf("member '%s'", v.Members[i])
			}
			// Reaching ITSELF is the recursive case, and `add = 1*DIGIT
			// [ "+" add ]` is how it is usually written — worth its own
			// wording, since "make that part 'add' itself" is nonsense
			// advice for a rule that already is.
			if hit == prod.Name {
				return valuePlan{}, &EmitError{Rule: prod.Name, Sp: sp, Message: fmt.Sprintf(
					diagName()+": rule '%s' takes %s as source text, but that "+
						"part reaches '%s' itself, which builds a value — a rule "+
						"that builds a value contributes no text to the part "+
						"above it, so a recursive rule cannot take its own "+
						"repetition as text. Annotate the rule the recursion "+
						"pushes instead.",
					prod.Name, which, prod.Name)}
			}
			return valuePlan{}, &EmitError{Rule: prod.Name, Sp: sp, Message: fmt.Sprintf(
				diagName()+": rule '%s' takes %s as source text, but '%s' is "+
					"reached from it and builds a value of its own — a rule "+
					"that builds a value contributes no text to the part above "+
					"it, so the member would be missing '%s's match, or empty. "+
					"Make that part '%s' itself so it nests, or remove the "+
					"annotation on '%s'.",
				prod.Name, which, hit, hit, hit, hit)}
		}

		plan.plan[prod.Name] = flags
		// Which parts are a REPETITION the author wrote here, and so
		// collect into the array rather than becoming one element of it.
		// Recorded on the AUTHORED shape, for the same reason everything
		// else in this pass is: by emit time Paull's substitution has
		// inlined the leading member's own body, whose helpers look
		// exactly like a repetition written at this level. They are not;
		// that member is one element, and collecting it loses it.
		if v.Kind == "array" {
			sugar := make([]bool, len(parts))
			for i, el := range parts {
				sugar[i] = collectsValues(el)
			}
			plan.collect[prod.Name] = sugar
		}
	}
	return plan, nil
}

// valuePlan is what planValueAnnotations predicts, per annotated
// production: one flag per part that produces a value. `plan` says
// whether that part nests (its own rule builds a value); `collect` —
// arrays only — says whether it is a repetition, and so contributes its
// own parts as elements instead of being one. Mirrors the TS
// `ValuePlan`.
type valuePlan struct {
	plan    map[string][]bool
	collect map[string][]bool
}

// srcField is the field a parse-tree node carries its matched text in,
// and the one captureChildFields tests for to tell a node from anything
// else. A value annotation may not name a member this, or the two become
// indistinguishable — see the refusal in planValueAnnotations. Mirrors
// the TS `SRC_FIELD`.
const srcField = "src"

// reachesAnnotated returns the first rule that BUILDS A VALUE reachable
// from this element, or "". Walks sugar and follows rule references
// transitively, stopping at the first hit.
//
// This is asked about a member that is NOT itself annotated, whose value
// is therefore the source text its tree builders accumulated. A rule
// that builds a value does not contribute text to the node above it —
// its object is a child, not a span — so an ancestor's src comes out
// missing that rule's match. In a tree that is lossy; in a MEMBER it is
// the whole value, so `top = "<" (inner) ">"` as an array built [""]
// where the annotated inner matched, and `"[" inner "]"` built "[]".
// Nothing is between the two to notice, which is why this is refused
// rather than corrected. Mirrors the TS `reachesAnnotated`.
func reachesAnnotated(
	el *Element, byName map[string]*Production, seen map[string]bool,
) string {
	switch el.Kind {
	case KindRef:
		target, ok := byName[el.Name]
		if !ok {
			return ""
		}
		if target.Value != nil {
			return target.Name
		}
		if seen[target.Name] {
			return ""
		}
		seen[target.Name] = true
		for _, alt := range target.Alts {
			for _, inner := range alt {
				if hit := reachesAnnotated(inner, byName, seen); hit != "" {
					return hit
				}
			}
		}
		return ""
	case KindOpt, KindStar, KindPlus, KindRep:
		if el.Inner == nil {
			return ""
		}
		return reachesAnnotated(el.Inner, byName, seen)
	case KindGroup:
		for _, alt := range el.Alts {
			for _, inner := range alt {
				if hit := reachesAnnotated(inner, byName, seen); hit != "" {
					return hit
				}
			}
		}
		return ""
	}
	return ""
}

// exemptAlias reports whether eliminateLeftRecursion will leave this
// production's leading reference ALONE. Paull's pass exempts a *pure
// alias* — one alternative that is one reference — from being
// substituted into, unless it or its target is caught in a
// leading-reference cycle, where the substitution is doing real work.
// See that pass for the full reasoning.
//
// Shared with planValueAnnotations because the annotation checks are a
// prediction of exactly this: whether a leading reference survives. Two
// copies of the condition would be two predictions, and the one here
// would be the wrong one. Mirrors the TS `exemptAlias`.
func exemptAlias(p *Production, cyclic map[string]bool) bool {
	if len(p.Alts) != 1 || len(p.Alts[0]) != 1 {
		return false
	}
	el := p.Alts[0][0]
	if el.Kind != KindRef {
		return false
	}
	return !cyclic[p.Name] && !cyclic[el.Name]
}

// pushesValue reports whether an element produces a value when it is
// matched. A reference, a repetition and a group all become a push of one
// child; a terminal in any of its spellings consumes input and pushes
// nothing. This is the one definition of "is a member" — the planner
// counts parts with it and resolveLeadingFold asks it about a body of one
// element, so the two cannot drift apart. Mirrors the TS `pushesValue`.
func pushesValue(el *Element) bool {
	switch el.Kind {
	case KindTerm, KindRegex, KindToken, KindProse:
		return false
	}
	return true
}

// hiddenAnnotated returns the first value-building rule that a
// COLLECTING part would still take as text.
//
// Collecting changes what the refusal has to ask. A repetition no longer
// resolves to its run's text — it pushes what is inside it — so an
// annotated rule its helper pushes DIRECTLY nests as an element and
// nothing goes missing. One reached through an UNANNOTATED wrapper is a
// different matter: that wrapper is an ordinary rule, pushed as its own
// text, and the text is still missing the annotated rule's match. So the
// walk descends through collecting sugar and then asks the ordinary
// question of each reference it arrives at.
//
// `top = *mid   ; @array` with `mid = "[" inner "]"` and an annotated
// `inner` is the case: it built `["[]","[]"]` — two elements, both
// missing the value — when the refusal was skipped for the whole
// repetition rather than for what the repetition pushes. Mirrors the TS
// `hiddenAnnotated`.
func hiddenAnnotated(el *Element, byName map[string]*Production) string {
	switch el.Kind {
	case KindRef:
		target := byName[el.Name]
		// Annotated: the helper pushes it whole, so it nests.
		if target == nil || target.Value != nil {
			return ""
		}
		return reachesAnnotated(el, byName, map[string]bool{})
	case KindOpt, KindStar, KindPlus, KindRep:
		return hiddenAnnotated(el.Inner, byName)
	case KindGroup:
		for _, alt := range el.Alts {
			for _, inner := range alt {
				if hit := hiddenAnnotated(inner, byName); hit != "" {
					return hit
				}
			}
		}
	}
	return ""
}

// collectsValues reports whether a part COLLECTS into an enclosing
// `; @array` — contributes the parts inside it as elements, in order —
// rather than being one element itself.
//
// A repetition does: it is the only way to write a list, and taking the
// whole run as one element is what made `; @array` unable to build one.
// A bare GROUP does not, and the difference is not arbitrary. A group is
// how an author writes ONE element out of several pieces: `( "[" p "]" )`
// means the element `[7]`, and a group holding only terminals is an
// element with no reference in it at all — collecting either yields fewer
// elements than the author wrote, silently. A group reached THROUGH a
// repetition does collect, because there it is the repeated item rather
// than an element: `*( "," item )` is a list of `item`, not a list of
// runs. Mirrors the TS `collectsValues`.
func collectsValues(el *Element) bool {
	switch el.Kind {
	case KindStar, KindPlus, KindOpt, KindRep:
		return true
	}
	return false
}

// resolveLeadingFold follows a leading reference the way left-recursion
// elimination will.
//
// The pass inlines a leading reference into its caller — and then inlines
// the leading reference of THAT body in turn, so an alias chain collapses
// all the way down. Asking only about the first rule therefore answered
// the wrong question: `top = a "," c` with `a = b` and `b = x ":"` passed,
// because `a`'s body is a single reference, and then quietly lost the
// `":"` from the member, because what actually landed in `top` was `b`'s
// two-part body.
//
// Two answers, because they need different diagnostics. The name returned
// is the first rule in the chain that builds a value of its own: inlining
// erases its builder, so the member would hold an internal node instead
// of the value the author annotated for. The bool is whether the chain
// ends in a body of exactly one pushing part. Mirrors the TS
// `resolveLeadingFold`.
func resolveLeadingFold(
	name string, byName map[string]*Production,
) (bool, string) {
	seen := map[string]bool{}
	prod, ok := byName[name]
	for ok {
		// A cycle is left recursion reached through aliases. The pass that
		// rewrites it is the one whose output this is predicting, so stop
		// rather than guess; the member-count check downstream still holds.
		if seen[prod.Name] {
			return false, ""
		}
		seen[prod.Name] = true
		if prod.Value != nil {
			return false, prod.Name
		}
		if len(prod.Alts) != 1 || len(prod.Alts[0]) != 1 {
			return false, ""
		}
		el := prod.Alts[0][0]
		if !pushesValue(el) {
			return false, ""
		}
		// A group or repetition is opaque to the fold: it becomes one
		// helper reference, which is one part, and nothing inside it is
		// inlined.
		if el.Kind != KindRef {
			return true, ""
		}
		prod, ok = byName[el.Name]
	}
	// An undefined rule is not this check's to refuse — the reference
	// resolution pass reports it, with a better message.
	return true, ""
}

func intToStr(n int) string { return strconv.Itoa(n) }

func sortStrings(s []string) { sort.Strings(s) }
