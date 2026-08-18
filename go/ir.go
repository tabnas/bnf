// Copyright (c) 2026 tabnas, MIT License

package bnf

import (
	"sort"
	"strconv"
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

// Element is one element of an ABNF sequence (a term, ref, regex, or
// EBNF sugar). Mirrors the TS AbnfElement union.
type Element struct {
	Kind ElemKind

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

// diagPrefix names the NOTATION a grammar was written in, not this
// package: a front-end's users should not see "bnf:" on an error about
// their own syntax. EmitGrammarSpec sets it from ConvertOptions.Tag for
// the duration of one emit. Mirrors `_diagName` in ts/src/compiler.ts.
var diagPrefix = "bnf"

func diagName() string { return diagPrefix }

func intToStr(n int) string { return strconv.Itoa(n) }

func sortStrings(s []string) { sort.Strings(s) }
