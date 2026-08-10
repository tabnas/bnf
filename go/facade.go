// Copyright (c) 2026 tabnas, MIT License

package bnf

import tabnas "github.com/tabnas/parser/go"

// EmitGrammarSpec compiles a grammar IR into a tabnas GrammarSpec. This
// is the whole public surface a front-end needs: parse your notation
// into a *Grammar, then call this.
//
// Opts.Tag should be the notation's own name ("abnf", "gbnf", "ebnf"),
// so emitted alts stay attributable and diagnostics name the syntax the
// user actually wrote.
func EmitGrammarSpec(
	grammar *Grammar, opts *ConvertOptions) (*tabnas.GrammarSpec, error) {
	return emitGrammarSpec(grammar, opts)
}

// EliminateLeftRecursion runs that pass alone, for a front-end that
// wants to inspect or test the rewritten IR.
func EliminateLeftRecursion(grammar *Grammar) *Grammar {
	return eliminateLeftRecursion(grammar)
}

// RefsIn collects the rule references in a sequence.
func RefsIn(alt Sequence, out map[string]bool) { refsIn(alt, out) }

// EscapeRegexp quotes the regex metacharacters in a literal.
func EscapeRegexp(s string) string { return escapeRegexp(s) }

// TermKey is the identity of a terminal for token allocation.
func TermKey(el *Element) string { return termKey(el) }

// IsEffectivelyCaseSensitive reports whether a literal's case actually
// matters — a literal with no ASCII letter is insensitive either way.
func IsEffectivelyCaseSensitive(el *Element) bool {
	return isEffectivelyCaseSensitive(el)
}

// BuiltinTokens maps a bareword name to the engine's lexer token.
func BuiltinTokens() map[string]string {
	out := make(map[string]string, len(builtinTokens))
	for k, v := range builtinTokens {
		out[k] = v
	}
	return out
}

// IsProseName reports whether a prose text is a compiler directive.
func IsProseName(name string) bool { return isProseName(name) }
