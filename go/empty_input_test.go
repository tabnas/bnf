// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

package bnf

// empty_input_test.go — whether the empty input is in the language.
//
// Mirrors ts/test/empty-input.test.js. The engine short-circuits "" before
// the parse loop starts, so no rule ever sees it and Options.Lex.Empty
// alone decides. Nothing set it, and the engine's default is permissive,
// so every emitted grammar accepted "" — `S = "a"` included. A grammar's
// nullability is a property of the IR rather than of any notation, which
// is why it is answered here and not in a front-end.

import "testing"

func emptyOf(t *testing.T, prods []*Production, opts *ConvertOptions) bool {
	t.Helper()
	spec := emitIR(t, prods, opts)
	if spec.Options == nil || spec.Options.Lex == nil ||
		spec.Options.Lex.Empty == nil {
		t.Fatalf("lex.empty was not set at all")
	}
	return *spec.Options.Lex.Empty
}

func oneAlt(alt Sequence) []*Production {
	return []*Production{{Name: "S", Alts: []Sequence{alt}}}
}

func TestEmptyInputFromTheGrammar(t *testing.T) {
	cases := []struct {
		label string
		alt   Sequence
		empty bool
	}{
		// A terminal consumes.
		{"a literal", Sequence{termEl("a")}, false},
		{"a sequence", Sequence{termEl("a"), termEl("b")}, false},
		{"1*A", Sequence{{Kind: KindPlus, Inner: termEl("a")}}, false},
		{"a class", Sequence{rxEl("[a-z]", "")}, false},
		{"a built-in token", Sequence{{Kind: KindToken, Name: "#TX"}}, false},

		// The engine's own zero-width tokens. Neither is reachable from
		// grammar text (a bareword becomes a token element only if it is in
		// BUILTIN_TOKENS, which holds just TX/NR/ST/VL) but the IR is the
		// shared contract, and calling either consuming rejects a grammar's
		// only string.
		{"#ZZ, end of source", Sequence{{Kind: KindToken, Name: "#ZZ"}}, true},
		{"#AA, the ANY wildcard", Sequence{{Kind: KindToken, Name: "#AA"}}, true},
		{"#SP still consumes", Sequence{{Kind: KindToken, Name: "#SP"}}, false},
		{"a zero-width token does not make its sequence empty",
			Sequence{termEl("a"), {Kind: KindToken, Name: "#ZZ"}}, false},
		{"a group of consuming alternatives", Sequence{{Kind: KindGroup, Alts: []Sequence{
			{termEl("a")}, {termEl("b")}}}}, false},

		// These derive empty.
		{"*A", Sequence{{Kind: KindStar, Inner: termEl("a")}}, true},
		{"[ A ]", Sequence{{Kind: KindOpt, Inner: termEl("a")}}, true},
		{"0*2A", Sequence{{Kind: KindRep, Min: 0, Max: 2, Inner: termEl("a")}}, true},
		{"a group with one empty-deriving branch", Sequence{{Kind: KindGroup, Alts: []Sequence{
			{termEl("a")}, {{Kind: KindOpt, Inner: termEl("b")}}}}}, true},

		// Two kinds can match nothing without looking like it. Reading
		// either as consuming would reject input the grammar admits, which
		// is the worse of the two directions.
		{"an empty literal", Sequence{termEl("")}, true},
		{"a class that can match nothing", Sequence{rxEl("[a-z]*", "")}, true},
		{"an alternation with an empty branch", Sequence{rxEl("a|", "")}, true},
	}

	for _, c := range cases {
		if got := emptyOf(t, oneAlt(c.alt), nil); got != c.empty {
			t.Errorf("%s: lex.empty = %v, want %v", c.label, got, c.empty)
		}
	}
}

// Nullability is a least fixed point over the rules, not a property of one
// production read alone. Each of these needs more than one pass.
func TestEmptyInputFollowsOtherRules(t *testing.T) {
	star := &Element{Kind: KindStar, Inner: termEl("a")}

	through := []*Production{
		{Name: "S", Alts: []Sequence{{refEl("A")}}},
		{Name: "A", Alts: []Sequence{{refEl("B")}}},
		{Name: "B", Alts: []Sequence{{star}}},
	}
	if !emptyOf(t, through, nil) {
		t.Error("nullability through a chain of rules was missed")
	}

	consuming := []*Production{
		{Name: "S", Alts: []Sequence{{refEl("A")}}},
		{Name: "A", Alts: []Sequence{{refEl("B")}}},
		{Name: "B", Alts: []Sequence{{termEl("a")}}},
	}
	if emptyOf(t, consuming, nil) {
		t.Error("a consuming chain was reported as deriving empty")
	}

	// A single pass over the productions in order answers false here.
	later := []*Production{
		{Name: "S", Alts: []Sequence{{refEl("A"), refEl("B")}}},
		{Name: "A", Alts: []Sequence{{{Kind: KindOpt, Inner: termEl("x")}}}},
		{Name: "B", Alts: []Sequence{{{Kind: KindOpt, Inner: termEl("y")}}}},
	}
	if !emptyOf(t, later, nil) {
		t.Error("rules defined after their use were missed")
	}
}

// Every alternative consumes an `a` before reaching the recursion, so the
// rule that reaches itself stays at the bottom of the fixed point.
func TestRecursionIsNotNullability(t *testing.T) {
	prods := []*Production{
		{Name: "S", Alts: []Sequence{{termEl("a"), refEl("S")}, {termEl("a")}}},
	}
	if emptyOf(t, prods, nil) {
		t.Error("a recursive rule that always consumes was reported as nullable")
	}
}

func TestEmptyInputFollowsTheStartRule(t *testing.T) {
	prods := []*Production{
		{Name: "S", Alts: []Sequence{{termEl("a")}}},
		{Name: "T", Alts: []Sequence{{{Kind: KindStar, Inner: termEl("b")}}}},
	}
	if emptyOf(t, prods, &ConvertOptions{Tag: "t"}) {
		t.Error("default start S consumes, but lex.empty was true")
	}
	if !emptyOf(t, prods, &ConvertOptions{Tag: "t", Start: "T"}) {
		t.Error("start T derives empty, but lex.empty was false")
	}
}
