/* Copyright (c) 2026 Richard Rodger and other contributors, MIT License */

package bnf

import (
	"strings"
	"testing"
)

func safetyLit(source string) *Element {
	return &Element{Kind: KindTerm, Literal: source, CaseSensitive: true, HasCaseSens: true}
}

func safetyRep(min, max int, inner *Element) *Element {
	return &Element{Kind: KindRep, Min: min, Max: max, Inner: inner}
}

func safetyEmit(productions ...*Production) (*Grammar, *ConvertOptions) {
	return &Grammar{Productions: productions}, &ConvertOptions{Tag: "safe", Start: "top"}
}

func TestRepetitionSafetyRefusesNullableUnboundedItem(t *testing.T) {
	sp := &SrcSpan{S: 4, E: 18, R: 1, C: 5}
	grammar, opts := safetyEmit(&Production{
		Name: "top", Sp: sp,
		Alts: []Sequence{{{
			Kind: KindStar,
			Inner: &Element{Kind: KindGroup, Alts: []Sequence{
				{safetyLit("a"), safetyLit("b")}, {},
			}},
		}}},
	})
	_, err := EmitGrammarSpec(grammar, opts)
	if err == nil {
		t.Fatal("nullable star compiled")
	}
	ee, ok := err.(*EmitError)
	if !ok || ee.Rule != "top" || ee.Sp != sp {
		t.Fatalf("error = %#v, want ranged EmitError for top", err)
	}
	if !strings.Contains(err.Error(), "rule 'top'") ||
		!strings.Contains(err.Error(), "without consuming input") {
		t.Fatalf("error = %q", err)
	}
}

func TestRepetitionSafetyFindsNullableReference(t *testing.T) {
	grammar, opts := safetyEmit(
		&Production{Name: "top", Alts: []Sequence{{{
			Kind: KindPlus, Inner: &Element{Kind: KindRef, Name: "empty"},
		}}}},
		&Production{Name: "empty", Alts: []Sequence{{}}},
	)
	_, err := EmitGrammarSpec(grammar, opts)
	if err == nil || !strings.Contains(err.Error(), "unbounded repetition") {
		t.Fatalf("error = %v", err)
	}
}

func TestRepetitionSafetyRefusesContextDependentZeroWidthRegex(t *testing.T) {
	grammar, opts := safetyEmit(&Production{
		Name: "top",
		Alts: []Sequence{{{
			Kind: KindStar, Inner: &Element{Kind: KindRegex, Pattern: `\b`},
		}}},
	})
	_, err := EmitGrammarSpec(grammar, opts)
	if err == nil || !strings.Contains(err.Error(), "unbounded repetition") {
		t.Fatalf("error = %v", err)
	}
}

func TestRepetitionSafetyAllowsConsumingBoundaryRegex(t *testing.T) {
	grammar, opts := safetyEmit(&Production{
		Name: "top",
		Alts: []Sequence{{{
			Kind: KindStar, Inner: &Element{Kind: KindRegex, Pattern: `\b[a-z]+`},
		}}},
	})
	if _, err := EmitGrammarSpec(grammar, opts); err != nil {
		t.Fatal(err)
	}
}

func TestRepetitionSafetyValidatesTailRepeatProgress(t *testing.T) {
	boundary := func() *Element { return &Element{Kind: KindRegex, Pattern: `\b`} }
	grammar, opts := safetyEmit(
		&Production{Name: "top", Alts: []Sequence{{{Kind: KindRef, Name: "X"}}}},
		&Production{Name: "X", Alts: []Sequence{{
			boundary(),
			{Kind: KindOpt, Inner: &Element{Kind: KindGroup, Alts: []Sequence{{
				boundary(), {Kind: KindRef, Name: "X"},
			}}}},
		}}},
	)
	_, err := EmitGrammarSpec(grammar, opts)
	if err == nil || !strings.Contains(err.Error(), "unbounded tail repetition") {
		t.Fatalf("error = %v", err)
	}
}

func TestRepetitionSafetyValidatesProbeHelperProgress(t *testing.T) {
	nullable := func() *Element { return &Element{Kind: KindRegex, Pattern: `[a-z]*`} }
	grammar, opts := safetyEmit(&Production{
		Name: "top",
		Alts: []Sequence{{
			{Kind: KindOpt, Inner: &Element{Kind: KindGroup, Alts: []Sequence{{
				nullable(), safetyLit("!"),
			}}}},
			nullable(),
		}},
	})
	_, err := EmitGrammarSpec(grammar, opts)
	if err == nil || !strings.Contains(err.Error(), "probe helper matcher") ||
		!strings.Contains(err.Error(), "without consuming input") {
		t.Fatalf("error = %v", err)
	}
}

func TestRepetitionSafetyAttributesCopiedLoopToSource(t *testing.T) {
	sp := &SrcSpan{S: 20, E: 21, R: 2, C: 1}
	grammar, opts := safetyEmit(
		&Production{Name: "top", Alts: []Sequence{{{Kind: KindRef, Name: "A"}}}},
		&Production{Name: "A", Sp: sp, Alts: []Sequence{
			{{Kind: KindOpt, Inner: safetyLit("x")},
				{Kind: KindRef, Name: "A"},
				{Kind: KindOpt, Inner: safetyLit("y")}},
			{safetyLit("z")},
		}},
	)
	_, err := EmitGrammarSpec(grammar, opts)
	ee, ok := err.(*EmitError)
	if !ok || ee.Rule != "A" || ee.Sp != sp {
		t.Fatalf("error = %#v, want ranged EmitError for A", err)
	}
}

func TestRepetitionSafetyAttributesNullableGroupedTailToItsSource(t *testing.T) {
	csp := &SrcSpan{S: 40, E: 41, R: 4, C: 1}
	grammar, opts := safetyEmit(
		&Production{Name: "top", Alts: []Sequence{{{Kind: KindRef, Name: "A"}}}},
		&Production{Name: "A", Alts: []Sequence{
			{{Kind: KindRef, Name: "B"}},
			{{Kind: KindRef, Name: "C"}},
			{safetyLit("z")},
		}},
		&Production{Name: "B", Alts: []Sequence{{
			{Kind: KindRef, Name: "A"}, safetyLit("x"),
		}}},
		&Production{Name: "C", Sp: csp, Alts: []Sequence{{
			{Kind: KindRef, Name: "A"}, {Kind: KindOpt, Inner: safetyLit("y")},
		}}},
	)
	_, err := EmitGrammarSpec(grammar, opts)
	ee, ok := err.(*EmitError)
	if !ok || ee.Rule != "C" || ee.Sp != csp {
		t.Fatalf("error = %#v, want ranged EmitError for C", err)
	}
}

func TestRepetitionSafetyRejectsInvalidBounds(t *testing.T) {
	for _, bounds := range [][2]int{{-2, -2}, {3, 2}} {
		grammar, opts := safetyEmit(&Production{
			Name: "top",
			Alts: []Sequence{{safetyRep(bounds[0], bounds[1], safetyLit("a"))}},
		})
		_, err := EmitGrammarSpec(grammar, opts)
		if err == nil || !strings.Contains(err.Error(), "invalid repetition bounds") {
			t.Fatalf("bounds %v: error = %v", bounds, err)
		}
	}
}

func TestRepetitionSafetyRefusesNumericExpansion(t *testing.T) {
	if MaxRepeatExpansion != 8192 {
		t.Fatalf("MaxRepeatExpansion = %d", MaxRepeatExpansion)
	}
	grammar, opts := safetyEmit(&Production{
		Name: "top", Alts: []Sequence{{safetyRep(1, 5000, safetyLit("a"))}},
	})
	_, err := EmitGrammarSpec(grammar, opts)
	if err == nil || !strings.Contains(err.Error(), "limit of 8192") ||
		!strings.Contains(err.Error(), "'1*5000'") {
		t.Fatalf("error = %v", err)
	}
}

func TestRepetitionSafetyCountsUnboundedMandatoryPrefix(t *testing.T) {
	grammar, opts := safetyEmit(&Production{
		Name: "top",
		Alts: []Sequence{{safetyRep(
			MaxRepeatExpansion+1, MaxInfinity, safetyLit("a"),
		)}},
	})
	_, err := EmitGrammarSpec(grammar, opts)
	if err == nil || !strings.Contains(err.Error(), "repetition expansion limit") {
		t.Fatalf("error = %v", err)
	}
}

func TestRepetitionSafetyCountsNestedExpansionsTogether(t *testing.T) {
	grammar, opts := safetyEmit(&Production{
		Name: "top",
		Alts: []Sequence{{safetyRep(0, 2200,
			safetyRep(0, 2200, safetyLit("a")))}},
	})
	_, err := EmitGrammarSpec(grammar, opts)
	if err == nil || !strings.Contains(err.Error(), "repetition expansion limit") {
		t.Fatalf("error = %v", err)
	}
}

func TestRepetitionSafetyPreservesInvalidRegexDiagnostic(t *testing.T) {
	grammar, opts := safetyEmit(&Production{
		Name: "top",
		Alts: []Sequence{{{
			Kind: KindStar,
			Inner: &Element{Kind: KindGroup, Alts: []Sequence{
				{safetyLit("a")},
				{{Kind: KindRegex, Pattern: "[z-a]"}},
			}},
		}}},
	})
	_, err := EmitGrammarSpec(grammar, opts)
	if err == nil || !strings.Contains(err.Error(), "invalid regular expression") {
		t.Fatalf("error = %v", err)
	}
}

func TestRepetitionSafetyAllowsBoundedNullableItem(t *testing.T) {
	grammar, opts := safetyEmit(&Production{
		Name: "top",
		Alts: []Sequence{{safetyRep(0, 2, &Element{
			Kind: KindGroup, Alts: []Sequence{{safetyLit("a")}, {}},
		})}},
	})
	if _, err := EmitGrammarSpec(grammar, opts); err != nil {
		t.Fatal(err)
	}
}
