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
		!strings.Contains(err.Error(), "item can match the empty string") {
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
