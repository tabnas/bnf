/* Copyright (c) 2026 Richard Rodger, MIT License */

package bnf

// TokenClasses compiles a production whose alternatives are all single
// literals or engine tokens to one engine token set. What a grammar
// accepts must not depend on the option: each case here is one where it
// once did (tabnas/bnf#74 review). Mirrors ts/test/token-class.test.js.

import (
	"reflect"
	"sort"
	"testing"

	tabnas "github.com/tabnas/parser/go"
)

// ctSeqs is the `s` of each open alternate of a rule. Open is `any`; its
// elements carry `s` as a map entry or a struct field, depending on the
// emitter's form.
func ctSeqs(t *testing.T, spec *tabnas.GrammarSpec, name string) []string {
	t.Helper()
	rule, ok := spec.Rule[name]
	if !ok || rule == nil {
		t.Fatalf("no rule %s", name)
	}
	v := reflect.ValueOf(rule.Open)
	if !v.IsValid() || v.Kind() != reflect.Slice {
		t.Fatalf("open is %T", rule.Open)
	}
	out := []string{}
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
			if f := el.FieldByName("S"); f.IsValid() {
				s, _ = f.Interface().(string)
			}
		default:
			t.Fatalf("alternate %d is %s", i, el.Kind())
		}
		out = append(out, s)
	}
	return out
}

func tcEmit(t *testing.T, prods []*Production, tokenClasses bool) *tabnas.GrammarSpec {
	t.Helper()
	spec, err := EmitGrammarSpec(&Grammar{Productions: prods}, &ConvertOptions{Tag: "tc", Start: "doc", TokenClasses: tokenClasses})
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func tcSets(spec *tabnas.GrammarSpec) []string {
	out := []string{}
	if spec.Options != nil {
		for name := range spec.Options.TokenSet {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func tcTok(name string) *Element { return &Element{Kind: KindToken, Name: name} }

func TestTokenClassSetHeadTakesTheKeywordShadowOrderItsMembersHad(t *testing.T) {
	// doc = x ; x = C / [a-z] D ; C = "a" / "b" ; D = [0-9]
	// With the option off C's literals are heads of x, each guarded ahead
	// of the character class that shadows it. The set that stands for
	// them is ordered the same way, or `a1` commits to C at `a` and fails
	// at `1`.
	g := func() []*Production {
		return []*Production{
			{Name: "doc", Alts: []Sequence{{ctRef("x")}}},
			{Name: "x", Alts: []Sequence{{ctRef("C")}, {&Element{Kind: KindRegex, Pattern: "[a-z]"}, ctRef("D")}}},
			{Name: "C", Alts: []Sequence{{ctLit("a")}, {ctLit("b")}}},
			{Name: "D", Alts: []Sequence{{&Element{Kind: KindRegex, Pattern: "[0-9]"}}}},
		}
	}
	off := tcEmit(t, g(), false)
	on := tcEmit(t, g(), true)
	if got, want := ctSeqs(t, on, "x"), []string{"#C #ZZ", "#RX__A_Z", "#C"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("x: %q, want %q", got, want)
	}
	for _, src := range []string{"a1", "a", "b", "c1", "b1"} {
		if !ctParses(t, off, src, true) {
			t.Errorf("off: %q should parse", src)
		}
		if !ctParses(t, on, src, true) {
			t.Errorf("on: %q should parse", src)
		}
	}
}

func TestTokenClassWithASetAmongItsMembersStaysAProduction(t *testing.T) {
	// C = #C / "a" names its own set; C = #D / "a" beside D = #C / "b"
	// names another class's. A set of sets is one the engine cannot
	// resolve, and expanding it never ended.
	for _, classes := range [][]*Production{
		{{Name: "C", Alts: []Sequence{{tcTok("#C")}, {ctLit("a")}}}},
		{
			{Name: "C", Alts: []Sequence{{tcTok("#D")}, {ctLit("a")}}},
			{Name: "D", Alts: []Sequence{{tcTok("#C")}, {ctLit("b")}}},
		},
	} {
		g := func() []*Production {
			out := []*Production{
				{Name: "doc", Alts: []Sequence{{ctRef("x")}}},
				{Name: "x", Alts: []Sequence{{ctRef("C"), ctRef("t"), ctRef("u")}, {ctLit("z"), ctRef("u"), ctRef("t")}}},
				{Name: "t", Alts: []Sequence{{ctLit("."), ctLit(".")}}},
				{Name: "u", Alts: []Sequence{{ctLit(","), ctLit(",")}}},
			}
			for _, c := range classes {
				cp := *c
				out = append(out, &cp)
			}
			return out
		}
		on := tcEmit(t, g(), true)
		if sets := tcSets(on); len(sets) != 0 {
			t.Fatalf("token sets %v, want none", sets)
		}
		if got, want := ctSeqs(t, on, "x"), ctSeqs(t, tcEmit(t, g(), false), "x"); !reflect.DeepEqual(got, want) {
			t.Fatalf("x: %q, want the option-off %q", got, want)
		}
		for _, src := range []string{"a..,,", "z,,.."} {
			if !ctParses(t, on, src, false) {
				t.Errorf("%q should parse", src)
			}
		}
	}
}

func TestTokenClassEmptyProductionNameIsNotAClass(t *testing.T) {
	// doc = x ; x = <""> "!" ; <""> = "a" / "b". The set would be named
	// `#`, which names nothing, and so would the token standing for the
	// reference.
	g := func() []*Production {
		return []*Production{
			{Name: "doc", Alts: []Sequence{{ctRef("x")}}},
			{Name: "x", Alts: []Sequence{{ctRef(""), ctLit("!")}}},
			{Name: "", Alts: []Sequence{{ctLit("a")}, {ctLit("b")}}},
		}
	}
	on := tcEmit(t, g(), true)
	if sets := tcSets(on); len(sets) != 0 {
		t.Fatalf("token sets %v, want none", sets)
	}
	if got, want := ctSeqs(t, on, "x"), ctSeqs(t, tcEmit(t, g(), false), "x"); !reflect.DeepEqual(got, want) {
		t.Fatalf("x: %q, want the option-off %q", got, want)
	}
	for _, src := range []string{"a!", "b!"} {
		if !ctParses(t, on, src, false) {
			t.Errorf("%q should parse", src)
		}
	}
}
