// Copyright (c) 2026 tabnas, MIT License

// Smoke tests for the notation-neutral compiler. The heavy verification
// lives downstream, in the front-ends' suites — this package has no
// notation of its own to exercise it. What is checked here is that the
// surface a front-end actually uses works with no front-end present.
package bnf

import (
	"strings"
	"testing"
)

func ref(name string) *Element { return &Element{Kind: kindRef, Name: name} }
func tok(name string) *Element { return &Element{Kind: kindToken, Name: name} }
func term(lit string) *Element {
	return &Element{Kind: kindTerm, Literal: lit}
}

func TestCompilesMinimalGrammar(t *testing.T) {
	spec, err := EmitGrammarSpec(&Grammar{Productions: []*Production{
		{Name: "val", Alts: []Sequence{{ref("add")}}},
		{Name: "add", Alts: []Sequence{{tok("#NR")}}},
	}}, &ConvertOptions{Tag: "demo"})
	if err != nil {
		t.Fatalf("emit failed: %v", err)
	}
	if spec.Rule["val"] == nil {
		t.Error("expected a val rule")
	}
	if spec.Rule["add"] == nil {
		t.Error("expected an add rule")
	}
}

func TestStampsCallerTag(t *testing.T) {
	// The tag must be the caller's, never a notation this package
	// assumes. Nothing here may know what syntax a grammar came from.
	spec, err := EmitGrammarSpec(&Grammar{Productions: []*Production{
		{Name: "top", Alts: []Sequence{{term("x")}}},
	}}, &ConvertOptions{Tag: "demo"})
	if err != nil {
		t.Fatalf("emit failed: %v", err)
	}
	text := SpecToJSON(spec, 0)
	if !strings.Contains(text, "demo") {
		t.Error("expected the caller's tag on an emitted alt")
	}
	if strings.Contains(text, `"abnf"`) {
		t.Error("must not assume a notation")
	}
}

func TestLiftsSingleLiteralProduction(t *testing.T) {
	spec, err := EmitGrammarSpec(&Grammar{Productions: []*Production{
		{Name: "top", Alts: []Sequence{{ref("PL")}}},
		{Name: "PL", Alts: []Sequence{{term("+")}}},
	}}, &ConvertOptions{Tag: "demo"})
	if err != nil {
		t.Fatalf("emit failed: %v", err)
	}
	got := spec.Options.Fixed.Token["#PL"]
	if got == nil || *got != "+" {
		t.Errorf("expected #PL lifted to \"+\", got %v", got)
	}
}

func TestEliminatesLeftRecursion(t *testing.T) {
	out := EliminateLeftRecursion(&Grammar{Productions: []*Production{
		{Name: "expr", Alts: []Sequence{
			{ref("expr"), term("+"), ref("num")},
			{ref("num")},
		}},
		{Name: "num", Alts: []Sequence{{tok("#NR")}}},
	}})
	for _, p := range out.Productions {
		if p.Name != "expr" {
			continue
		}
		for _, alt := range p.Alts {
			if len(alt) > 0 && alt[0].Kind == kindRef && alt[0].Name == "expr" {
				t.Error("expr must no longer start with itself")
			}
		}
	}
}

func TestDiagnosticsNameTheNotation(t *testing.T) {
	// A front-end's users should see their own notation's name on an
	// error about their own syntax, never "bnf:".
	//
	// NOTE the shape of this test: the Go pipeline *panics* on a grammar
	// it cannot compile, where the TypeScript one throws a catchable
	// error. That is inherited from the pre-extraction code, not
	// introduced here, but it is the wrong contract for a Go library —
	// invalid user input should be an error return. Recorded rather than
	// changed, since the ABNF front-end's suite pins the current
	// behaviour.
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected a purely left-recursive rule to be refused")
		}
		msg, _ := r.(error)
		text := ""
		if msg != nil {
			text = msg.Error()
		} else {
			text, _ = r.(string)
		}
		if !strings.HasPrefix(text, "gbnf: ") {
			t.Errorf("expected the caller's tag to prefix the diagnostic, got %q",
				text)
		}
	}()
	_, _ = EmitGrammarSpec(&Grammar{Productions: []*Production{
		{Name: "A", Alts: []Sequence{{ref("A"), term("x")}}},
	}}, &ConvertOptions{Tag: "gbnf"})
}

func TestEscapeRegexp(t *testing.T) {
	if got := EscapeRegexp("a.b"); !strings.Contains(got, `\.`) {
		t.Errorf("expected the dot escaped, got %q", got)
	}
}

func TestBuiltinTokens(t *testing.T) {
	b := BuiltinTokens()
	for name, want := range map[string]string{
		"NR": "#NR", "TX": "#TX", "ST": "#ST", "VL": "#VL",
	} {
		if b[name] != want {
			t.Errorf("BuiltinTokens()[%q] = %q, want %q", name, b[name], want)
		}
	}
}
