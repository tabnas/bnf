/* Copyright (c) 2026 Richard Rodger and other contributors, MIT License */

package bnf

// The size of what the emitter produces is a contract (tabnas/bnf#71).
// Mirrors ts/test/size.test.js: a choice is dispatched on the first token
// of each alternative, and the lookahead deepens only under a head two
// alternatives share, so the emitted table grows with the number of
// decisions rather than with the product of the tokens that can fill
// four positions. Every count is exact and pinned.

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"

	tabnas "github.com/tabnas/parser/go"
)

func szLit(s string) *Element {
	return &Element{Kind: KindTerm, Literal: s, CaseSensitive: true, HasCaseSens: true}
}

func szKw(n int) *Production {
	alts := []Sequence{}
	for i := 1; i <= n; i++ {
		alts = append(alts, Sequence{szLit(fmt.Sprintf("k%d", i))})
	}
	return &Production{Name: "kw", Alts: alts}
}

// doc = "{" *entry "}" ; entry = kw kw ; kw = "k1" / ... / "kN"
func szStarred(n int) *Grammar {
	return &Grammar{Productions: []*Production{
		{Name: "doc", Alts: []Sequence{{szLit("{"), {Kind: KindStar, Inner: ref("entry")}, szLit("}")}}},
		{Name: "entry", Alts: []Sequence{{ref("kw"), ref("kw")}}},
		szKw(n),
	}}
}

// doc = "{" x "}" ; x = entry entry / "z" ; entry = kw kw ; kw = ...
func szChoice(n int) *Grammar {
	return &Grammar{Productions: []*Production{
		{Name: "doc", Alts: []Sequence{{szLit("{"), ref("x"), szLit("}")}}},
		{Name: "x", Alts: []Sequence{{ref("entry"), ref("entry")}, {szLit("z")}}},
		{Name: "entry", Alts: []Sequence{{ref("kw"), ref("kw")}}},
		szKw(n),
	}}
}

func szOpens(t *testing.T, spec *tabnas.GrammarSpec, name string) int {
	t.Helper()
	r, ok := spec.Rule[name]
	if !ok || r == nil {
		t.Fatalf("no rule %q", name)
	}
	v := reflect.ValueOf(r.Open)
	if v.IsValid() && v.Kind() == reflect.Slice {
		return v.Len()
	}
	t.Fatalf("rule %q: open is %T", name, r.Open)
	return 0
}

func szRule(t *testing.T, spec *tabnas.GrammarSpec, re string) string {
	t.Helper()
	rx := regexp.MustCompile(re)
	for name := range spec.Rule {
		if rx.MatchString(name) {
			return name
		}
	}
	t.Fatalf("no rule matching %s", re)
	return ""
}

func szTotal(t *testing.T, spec *tabnas.GrammarSpec) int {
	t.Helper()
	total := 0
	for name, r := range spec.Rule {
		if r == nil {
			continue
		}
		total += szOpens(t, spec, name)
	}
	return total
}

func szParses(t *testing.T, spec *tabnas.GrammarSpec, src string) bool {
	t.Helper()
	j := tabnas.Make()
	if err := j.Grammar(spec); err != nil {
		t.Fatalf("install: %v", err)
	}
	_, err := j.Parse(src)
	return err == nil
}

func TestSizeRepeatedEntryDispatchesOnHeads(t *testing.T) {
	// The star helper: one entry per keyword head, one FOLLOW peek for
	// the `}` that ends the loop, and the bare fallback.
	for _, n := range []int{2, 8, 26} {
		spec, err := EmitGrammarSpec(szStarred(n), &ConvertOptions{Tag: "rp", Start: "doc", WordKeywords: true})
		if err != nil {
			t.Fatal(err)
		}
		helper := szRule(t, spec, `star_entry$`)
		if got := szOpens(t, spec, helper); got != n+2 {
			t.Errorf("N=%d: %s has %d open alternates, want %d", n, helper, got, n+2)
		}
	}
}

// A leading reference to the class is consumed as its one token where the
// plain compile inlines the class's alternatives, and stays a node where
// the plain compile keeps the reference: the same parse result, node for
// node, with the option on or off.
func TestSizeTokenClassLeavesTheTreeAsTheOptionOffCompileDoes(t *testing.T) {
	off, err := EmitGrammarSpec(szStarred(26), &ConvertOptions{Tag: "rp", Start: "doc", WordKeywords: true})
	if err != nil {
		t.Fatal(err)
	}
	on, err := EmitGrammarSpec(szStarred(26), &ConvertOptions{Tag: "rp", Start: "doc", WordKeywords: true, TokenClasses: true})
	if err != nil {
		t.Fatal(err)
	}
	joff, jon := tabnas.Make(), tabnas.Make()
	if err := joff.Grammar(off); err != nil {
		t.Fatal(err)
	}
	if err := jon.Grammar(on); err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{"{k1 k2 k26 k1}", "{}", "{k3 k3}"} {
		a, err := joff.Parse(src)
		if err != nil {
			t.Fatalf("%s: off: %v", src, err)
		}
		b, err := jon.Parse(src)
		if err != nil {
			t.Fatalf("%s: on: %v", src, err)
		}
		ja, _ := json.Marshal(a)
		jb, _ := json.Marshal(b)
		if string(ja) != string(jb) {
			t.Fatalf("%s: trees differ\n off: %s\n on:  %s", src, ja, jb)
		}
	}
}

func TestSizeUncontestedChoiceDispatchesOnOneToken(t *testing.T) {
	spec, err := EmitGrammarSpec(szChoice(26), &ConvertOptions{Tag: "rp", Start: "doc", WordKeywords: true})
	if err != nil {
		t.Fatal(err)
	}
	// 26 keyword heads for the first alternative, one for "z".
	if got := szOpens(t, spec, "x"); got != 27 {
		t.Errorf("x has %d open alternates, want 27", got)
	}
}

func TestSizeTokenClassIsOneLookaheadToken(t *testing.T) {
	spec, err := EmitGrammarSpec(szStarred(26), &ConvertOptions{Tag: "rp", Start: "doc", WordKeywords: true, TokenClasses: true})
	if err != nil {
		t.Fatal(err)
	}
	// The class is a set; the helper peeks it once. The class rule
	// itself keeps its 26 alternates, one per member: it is the rule
	// that builds the node.
	if len(spec.Options.TokenSet) != 1 || len(spec.Options.TokenSet["kw"]) != 26 {
		t.Fatalf("token sets: %v", spec.Options.TokenSet)
	}
	helper := szRule(t, spec, `star_entry$`)
	if got := szOpens(t, spec, helper); got != 3 {
		t.Errorf("%s has %d open alternates, want 3", helper, got)
	}
	if got := szOpens(t, spec, "kw"); got != 26 {
		t.Errorf("kw has %d open alternates, want 26", got)
	}
	x, err := EmitGrammarSpec(szChoice(26), &ConvertOptions{Tag: "rp", Start: "doc", WordKeywords: true, TokenClasses: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := szOpens(t, x, "x"); got != 2 {
		t.Errorf("x has %d open alternates, want 2", got)
	}
	// The whole grammar stays small, and installs and parses.
	if total := szTotal(t, spec); total >= 40 {
		t.Errorf("%d open alternates", total)
	}
	j := tabnas.Make()
	if err := j.Grammar(spec); err != nil {
		t.Fatal(err)
	}
	out, err := j.Parse("{k1 k2 k26 k1}")
	if err != nil {
		t.Fatal(err)
	}
	// The class keeps its node: it is a rule of its own, not inlined.
	if !strings.Contains(fmt.Sprintf("%v", out), "kw") {
		t.Errorf("no kw node in %v", out)
	}
	if _, err := j.Parse("{k1}"); err == nil {
		t.Error("{k1} should be refused")
	}
}

func TestSizeEmittedGrammarInstallsAndParsesAtN26(t *testing.T) {
	spec, err := EmitGrammarSpec(szStarred(26), &ConvertOptions{Tag: "rp", Start: "doc", WordKeywords: true})
	if err != nil {
		t.Fatal(err)
	}
	if !szParses(t, spec, "{k1 k2 k2 k1}") {
		t.Error("{k1 k2 k2 k1} should parse")
	}
	if szParses(t, spec, "{k1}") {
		t.Error("{k1} should be refused")
	}
}

func TestSizeContestedHeadDeepensOnlyAsFarAsNeeded(t *testing.T) {
	// Two alternatives share `a`; they part at the second token, so the
	// dispatcher peeks two tokens under `a` and one under `z`.
	spec, err := EmitGrammarSpec(&Grammar{Productions: []*Production{
		{Name: "doc", Alts: []Sequence{{ref("x")}}},
		{Name: "x", Alts: []Sequence{
			{szLit("a"), ref("t"), ref("u")},
			{szLit("a"), ref("u"), ref("t")},
			{szLit("z"), ref("t"), ref("u")},
		}},
		{Name: "t", Alts: []Sequence{{szLit("."), szLit(".")}}},
		{Name: "u", Alts: []Sequence{{szLit(","), szLit(",")}}},
	}}, &ConvertOptions{Tag: "rp", Start: "doc"})
	if err != nil {
		t.Fatal(err)
	}
	opens := reflect.ValueOf(spec.Rule["x"].Open)
	got := []string{}
	for i := 0; i < opens.Len(); i++ {
		o := reflect.Indirect(opens.Index(i))
		got = append(got, fmt.Sprintf("%v|%v", o.FieldByName("S"), o.FieldByName("B")))
	}
	want := []string{"#A #T|2", "#A #T1|2", "#Z|1"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("x dispatch: %v, want %v", got, want)
	}
	for _, src := range []string{"a..,,", "a,,..", "z..,,"} {
		if !szParses(t, spec, src) {
			t.Errorf("%q should parse", src)
		}
	}
	if szParses(t, spec, "a.,.,") {
		t.Error("a.,., should be refused")
	}
}

func TestSizeRepetitionContestedByFollowLooksAcrossTheBoundary(t *testing.T) {
	// start = *( "a" t ) "a" "b" with t = "x" "x": at an `a` the loop may
	// continue (`a x`) or exit (`a b`). The exit is an open path, so the
	// continue entries keep the full window here, as they always did.
	spec, err := EmitGrammarSpec(&Grammar{Productions: []*Production{
		{Name: "start", Alts: []Sequence{{
			{Kind: KindStar, Inner: groupEl(Sequence{szLit("a"), ref("t")})},
			szLit("a"), szLit("b"),
		}}},
		{Name: "t", Alts: []Sequence{{szLit("x"), szLit("x")}}},
	}}, &ConvertOptions{Tag: "rp", Start: "start"})
	if err != nil {
		t.Fatal(err)
	}
	helper := szRule(t, spec, `star.*group$`)
	opens := reflect.ValueOf(spec.Rule[helper].Open)
	first := reflect.Indirect(opens.Index(0))
	if s := fmt.Sprint(first.FieldByName("S")); !strings.HasPrefix(s, "#A #X") {
		t.Errorf("first continue entry peeks %q", s)
	}
	for _, src := range []string{"ab", "axxab", "axxaxxab"} {
		if !szParses(t, spec, src) {
			t.Errorf("%q should parse", src)
		}
	}
	for _, src := range []string{"b", "axx"} {
		if szParses(t, spec, src) {
			t.Errorf("%q should be refused", src)
		}
	}
}
