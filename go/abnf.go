// Copyright (c) 2025-2026 Richard Rodger and other contributors, MIT License

// Package tabnasabnf is an ABNF -> tabnas GrammarSpec compiler for the
// tabnas parsing engine (github.com/tabnas/parser/go). It is a faithful
// Go port of the @tabnas/abnf TypeScript package.
//
// Given a small ABNF dialect it produces a function-free (when
// requested) GrammarSpec that, installed on a tabnas engine, parses
// inputs in that grammar and builds a {rule, src, kids} AST. It also
// emits "pure-data" jsonic (recognition / pure specs) and supports
// user actions.
//
// The package mirrors the TS sources:
//   - converter.ts -> converter.go (AST, parseAbnf, abnfRules, desugar,
//     core rules, left-recursion elimination, probe-dispatch rewriter,
//     FIRST sets, emitGrammarSpec, token allocation, Abnf entry) and
//     parser_abnf.go (the ABNF-for-ABNF parser grammar).
//   - compile.ts -> compile.go (AbnfCompile, ToRecognitionSpec,
//     ToPureSpec, ToJsonic, AttachActions, MarkListing).
//   - abnf.ts -> facade.go (Abnf, ParseAbnf, EmitGrammarSpec,
//     EliminateLeftRecursion, Install — the public facade).
package tabnasabnf

// Version is the current version of the module.
const Version = "0.2.0"

// ---- ABNF AST -------------------------------------------------------
//
// The parsed ABNF grammar is a list of productions, each an alternation
// of sequences of elements. Element kinds mirror the TS AbnfElement
// union; Go uses a single struct tagged by Kind plus optional fields.

// elemKind is the discriminator for a abnfElement.
type elemKind string

const (
	kindTerm  elemKind = "term"
	kindRef   elemKind = "ref"
	kindRegex elemKind = "regex"
	kindOpt   elemKind = "opt"
	kindStar  elemKind = "star"
	kindPlus  elemKind = "plus"
	kindRep   elemKind = "rep"
	kindGroup elemKind = "group"
	// kindToken is an engine builtin lexer token (e.g. #TX/#NR/#ST/#VL),
	// produced by normalizeBuiltinTokens. Its token name is held in Name and
	// is emitted verbatim into a rule's token sequence (no allocation, unlike
	// a literal term).
	kindToken elemKind = "token"
)

// abnfElement is one element of an ABNF sequence (a term, ref, regex, or
// EBNF sugar). Mirrors the TS AbnfElement union.
type abnfElement struct {
	Kind elemKind

	// term
	Literal       string
	CaseSensitive bool // explicit %s flag (ABNF strings are insensitive by default)
	hasCaseSens   bool // whether CaseSensitive was set explicitly (TS optional flag)

	// regex
	Pattern string
	Flags   string

	// ref
	Name string

	// opt / star / plus / rep
	Inner *abnfElement
	Min   int
	Max   int // maxInfinity for unbounded

	// group
	Alts []abnfSequence
}

// maxInfinity stands in for the TS `Infinity` upper bound on repetition.
const maxInfinity = 1 << 30

type abnfSequence []*abnfElement

// probeDispatchSpec configures a synthesised dispatcher production for
// an ambiguous `[X D] Y` subsequence.
type probeDispatchSpec struct {
	ProbeRule     string
	Disambiguator *abnfElement
	WithBranch    string
	NoBranch      string
}

// probeHelperSpec carries the vocabulary for a synthesised probe helper.
type probeHelperSpec struct {
	VocabElements []*abnfElement
}

// nodeKind controls how a production contributes to the output AST:
//   - "user": emit a tagged node {rule, src, kids}.
//   - "core": RFC 5234 core rules — flatten into the enclosing src.
//   - "helper": synthetic sugar / dispatcher / chain rules — flatten.
//
// Empty is treated as "user".

type abnfProduction struct {
	Name        string
	Alts        []abnfSequence
	Incremental bool
	ProbeDisp   *probeDispatchSpec
	ProbeHelper *probeHelperSpec
	NodeKind    string // "", "user", "core", "helper"
}

func (p *abnfProduction) kind() string {
	if p.NodeKind == "" {
		return "user"
	}
	return p.NodeKind
}

type abnfGrammar struct {
	Productions []*abnfProduction
	Ambiguities []ambiguityReport
}

type ambiguityReport struct {
	Rule     string
	AltIdx   int
	OptIdx   int
	Reason   string
	Resolved bool
}
