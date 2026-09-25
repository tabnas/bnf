/* Copyright (c) 2026 Richard Rodger, MIT License */

package bnf

// Whether two dispatch heads CONTEST (can the lexer hand the same input
// to both) decides how deep the dispatcher looks. An answer of "no" that
// is wrong emits one-token entries for a decision that needs two, and the
// first branch commits on the first token and rejects input the other
// branch accepts. Mirrors ts/test/contest.test.js.

import (
	"reflect"
	"strings"
	"testing"

	tabnas "github.com/tabnas/parser/go"
)

func ctLit(s string) *Element {
	return &Element{Kind: KindTerm, Literal: s, CaseSensitive: true, HasCaseSens: true}
}
func ctILit(s string) *Element { return &Element{Kind: KindTerm, Literal: s} }
func ctRef(n string) *Element  { return &Element{Kind: KindRef, Name: n} }

// doc = x ; x = <a> t u / <b> u t ; t = "." "." ; u = "," ","
func ctGrammar(a, b *Element) *Grammar {
	return &Grammar{Productions: []*Production{
		{Name: "doc", Alts: []Sequence{{ctRef("x")}}},
		{Name: "x", Alts: []Sequence{{a, ctRef("t"), ctRef("u")}, {b, ctRef("u"), ctRef("t")}}},
		{Name: "t", Alts: []Sequence{{ctLit("."), ctLit(".")}}},
		{Name: "u", Alts: []Sequence{{ctLit(","), ctLit(",")}}},
	}}
}

// ctDepths is the number of lookahead tokens each open alternate of x
// names. Open is `any`; its elements carry the alternate's `s` either as
// a map entry or as a struct field, depending on the emitter's form.
func ctDepths(t *testing.T, spec *tabnas.GrammarSpec) []int {
	t.Helper()
	rule, ok := spec.Rule["x"]
	if !ok || rule == nil {
		t.Fatal("no rule x")
	}
	v := reflect.ValueOf(rule.Open)
	if !v.IsValid() || v.Kind() != reflect.Slice {
		t.Fatalf("open is %T", rule.Open)
	}
	var out []int
	for i := 0; i < v.Len(); i++ {
		el := v.Index(i)
		for el.Kind() == reflect.Interface || el.Kind() == reflect.Ptr {
			el = el.Elem()
		}
		var s string
		switch el.Kind() {
		case reflect.Map:
			s, _ = el.MapIndex(reflect.ValueOf("s")).Interface().(string)
		case reflect.Struct:
			f := el.FieldByName("S")
			if f.IsValid() {
				s, _ = f.Interface().(string)
			}
		default:
			t.Fatalf("alternate %d is %s", i, el.Kind())
		}
		out = append(out, len(strings.Fields(s)))
	}
	return out
}

func ctParses(t *testing.T, spec *tabnas.GrammarSpec, src string, relex bool) bool {
	t.Helper()
	j := tabnas.Make(tabnas.Options{Lex: &tabnas.LexOptions{Relex: tabnas.Bool(relex)}})
	if err := j.Grammar(spec); err != nil {
		t.Fatal(err)
	}
	_, err := j.Parse(src)
	return err == nil
}

func TestContestWordKeywordAgainstPunctuationContinuation(t *testing.T) {
	spec, err := EmitGrammarSpec(ctGrammar(ctLit("a"), ctLit("a-b")), &ConvertOptions{Tag: "ct", Start: "doc", WordKeywords: true})
	if err != nil {
		t.Fatal(err)
	}
	if d := ctDepths(t, spec); d[0] != 2 || d[1] != 2 {
		t.Fatalf("depths %v, want [2 2]", d)
	}
	for _, src := range []string{"a-b,,..", "a..,,"} {
		if !ctParses(t, spec, src, true) {
			t.Errorf("%q should parse", src)
		}
	}
	word, err := EmitGrammarSpec(ctGrammar(ctLit("a"), ctLit("ab")), &ConvertOptions{Tag: "ct", Start: "doc", WordKeywords: true})
	if err != nil {
		t.Fatal(err)
	}
	if d := ctDepths(t, word); d[0] != 1 || d[1] != 1 {
		t.Fatalf("depths %v, want [1 1]: a word continuation keeps the guard", d)
	}
}

func TestContestCaseInsensitiveLiteralsFoldAsTheirMatchersDo(t *testing.T) {
	// A literal with no ASCII letter compiles to an exact fixed token, so
	// Σ and ς never meet; with ASCII letters the matcher folds case.
	spec, err := EmitGrammarSpec(ctGrammar(ctILit("Σ"), ctILit("ς")), &ConvertOptions{Tag: "ct", Start: "doc"})
	if err != nil {
		t.Fatal(err)
	}
	if d := ctDepths(t, spec); d[0] != 1 || d[1] != 1 {
		t.Fatalf("depths %v, want [1 1]", d)
	}
	for _, src := range []string{"ς,,..", "Σ..,,"} {
		if !ctParses(t, spec, src, false) {
			t.Errorf("%q should parse", src)
		}
	}
	ascii, err := EmitGrammarSpec(ctGrammar(ctILit("a"), ctILit("A-b")), &ConvertOptions{Tag: "ct", Start: "doc"})
	if err != nil {
		t.Fatal(err)
	}
	if d := ctDepths(t, ascii); d[0] != 2 || d[1] != 2 {
		t.Fatalf("depths %v, want [2 2]", d)
	}
	for _, src := range []string{"A-B,,..", "A..,,"} {
		if !ctParses(t, ascii, src, true) {
			t.Errorf("%q should parse", src)
		}
	}
}

func TestContestRegexHeadThatIsNotOneAtom(t *testing.T) {
	spec, err := EmitGrammarSpec(ctGrammar(&Element{Kind: KindRegex, Pattern: "a|b"}, ctLit("b")), &ConvertOptions{Tag: "ct", Start: "doc"})
	if err != nil {
		t.Fatal(err)
	}
	if d := ctDepths(t, spec); d[0] != 2 || d[1] != 2 {
		t.Fatalf("depths %v, want [2 2]", d)
	}
	for _, src := range []string{"b,,..", "a..,,"} {
		if !ctParses(t, spec, src, true) {
			t.Errorf("%q should parse", src)
		}
	}
}

func TestContestAnyTokenMeetsEveryHead(t *testing.T) {
	spec, err := EmitGrammarSpec(ctGrammar(&Element{Kind: KindToken, Name: "#AA"}, ctLit("b")), &ConvertOptions{Tag: "ct", Start: "doc"})
	if err != nil {
		t.Fatal(err)
	}
	if d := ctDepths(t, spec); d[0] != 2 || d[1] != 2 {
		t.Fatalf("depths %v, want [2 2]", d)
	}
	for _, src := range []string{"b,,..", "z..,,"} {
		if !ctParses(t, spec, src, false) {
			t.Errorf("%q should parse", src)
		}
	}
}
