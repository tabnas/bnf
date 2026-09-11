package bnf

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	tabnas "github.com/tabnas/parser/go"
)

// Value annotations, driven through the IR rather than a notation.
// Mirrors ts/test/value-annotation.test.js.
//
// A front-end says WHAT a rule builds; nothing here knows how the
// notation spelled it. ABNF carries it in a trailing comment, but this
// compiler must not know that — so these drive `Production.Value`
// directly, which is also the only way to reach the cases no front-end
// has syntax for yet.

func refEl(name string) *Element { return &Element{Kind: KindRef, Name: name} }

// valueEquals compares a built value against a plain Go want-shape.
// Objects come back as the engine's insertion-ordered map, so both sides
// go through JSON rather than reflect.DeepEqual — this asserts the
// VALUE, and key order is the engine's own contract, pinned there.
func valueEquals(got, want any) bool {
	// Round-trip BOTH sides through JSON into plain Go values before
	// comparing: objects come back as the engine's insertion-ordered map,
	// so a string comparison would be asserting key ORDER, which is the
	// engine's own contract and pinned there, not here.
	norm := func(v any) (any, error) {
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		var out any
		return out, json.Unmarshal(b, &out)
	}
	g, err1 := norm(got)
	w, err2 := norm(want)
	return err1 == nil && err2 == nil && reflect.DeepEqual(g, w)
}

// altActionsOf renders a rule's emitted alts so a test can assert which
// ACTIONS survived, without depending on the alt struct's shape.
func altActionsOf(t *testing.T, spec *tabnas.GrammarSpec, rule string) string {
	t.Helper()
	rs := spec.Rule[rule]
	if rs == nil {
		t.Fatalf("no rule %q in emitted spec", rule)
	}
	b, err := json.Marshal(map[string]any{"open": rs.Open, "close": rs.Close})
	if err != nil {
		t.Fatalf("marshal rule %q: %v", rule, err)
	}
	return string(b)
}

// digitsEl is `1*DIGIT`, not a bare `[0-9]+` terminal. The difference
// decides whether a LEADING member survives: left-recursion elimination
// folds a leading reference into this rule, and a rule whose whole body
// is one terminal becomes literals here — which stops it being a member
// at all. A repetition becomes a reference to a generated helper, so it
// still pushes. Real ABNF writes `1*DIGIT`, so that is what these use.
func digitsEl() *Element {
	return &Element{Kind: KindPlus, Inner: rxEl("[0-9]", "")}
}

func lettersEl() *Element {
	return &Element{Kind: KindPlus, Inner: rxEl("[a-z]", "")}
}

func emitValue(t *testing.T, prods []*Production, start string) *tabnas.GrammarSpec {
	t.Helper()
	spec, err := EmitGrammarSpec(&Grammar{Productions: prods},
		&ConvertOptions{Tag: "tst", Start: start, Builtins: true})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	return spec
}

func buildValue(t *testing.T, prods []*Production, start, src string) any {
	t.Helper()
	j := tabnas.Make()
	if err := j.Grammar(emitValue(t, prods, start)); err != nil {
		t.Fatalf("install: %v", err)
	}
	out, err := j.Parse(src)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	return tabnas.UnwrapUndefined(out)
}

// `top = a "." b "." c` with the parts named. The FIRST member is the one
// that matters: a production's leading reference is inlined by the
// left-recursion pass, so `top` pushes a generated helper rather than
// `a`. The annotation naming its own parts is what survives that.
func tripleProds() []*Production {
	return []*Production{
		{Name: "top",
			Value: &ValueAnnotation{Kind: "object", Members: []string{"a", "b", "c"}},
			Alts:  []Sequence{{refEl("a"), termEl("."), refEl("b"), termEl("."), refEl("c")}}},
		{Name: "a", Alts: []Sequence{{digitsEl()}}},
		{Name: "b", Alts: []Sequence{{digitsEl()}}},
		{Name: "c", Alts: []Sequence{{digitsEl()}}},
	}
}

func TestValueAnnotationBuildsObject(t *testing.T) {
	got := buildValue(t, tripleProds(), "top", "1.2.30")
	want := map[string]any{"a": "1", "b": "2", "c": "30"}
	if !valueEquals(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// Not a restatement of the test above: it asserts the CAUSE. `top`'s
// first segment pushes a generated helper, not `a` — so the key can only
// have come from the annotation, never from the chain.
func TestValueAnnotationNamesInlinedFirstMember(t *testing.T) {
	spec := emitValue(t, tripleProds(), "top")
	open, ok := spec.Rule["top"].Open.([]*tabnas.GrammarAltSpec)
	if !ok || len(open) == 0 {
		t.Fatalf("top open is %T", spec.Rule["top"].Open)
	}
	if open[0].P == "a" {
		t.Fatal("this test is pointless if the leading ref survives — " +
			"the inlining it guards against has stopped happening")
	}
	key, _ := open[0].K["key$"].(map[string]any)
	if key == nil || key["lit"] != "a" {
		t.Errorf("first member key: got %#v, want lit=a", open[0].K["key$"])
	}
}

// `inner` is annotated, so it is assigned WHOLE — omitting `src` is what
// makes nesting work, rather than any special case. `name` is not, so it
// still resolves to its text.
func TestValueAnnotationNestsAnAnnotatedMember(t *testing.T) {
	prods := []*Production{
		{Name: "top",
			Value: &ValueAnnotation{Kind: "object", Members: []string{"name", "inner"}},
			Alts:  []Sequence{{refEl("name"), termEl("="), refEl("inner")}}},
		{Name: "name", Alts: []Sequence{{lettersEl()}}},
		{Name: "inner",
			Value: &ValueAnnotation{Kind: "object", Members: []string{"maj", "min"}},
			Alts:  []Sequence{{refEl("maj"), termEl("."), refEl("min")}}},
		{Name: "maj", Alts: []Sequence{{digitsEl()}}},
		{Name: "min", Alts: []Sequence{{digitsEl()}}},
	}
	got := buildValue(t, prods, "top", "ab=1.2")
	want := map[string]any{
		"name":  "ab",
		"inner": map[string]any{"maj": "1", "min": "2"},
	}
	if !valueEquals(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}

	// The nested member's close carries NO src; a scalar one does.
	spec := emitValue(t, prods, "top")
	closes, _ := spec.Rule["top$step1"].Close.([]*tabnas.GrammarAltSpec)
	if len(closes) == 0 {
		t.Fatal("no close on top$step1")
	}
	if _, has := closes[0].K["setval$"]; has {
		t.Error("a nested member must be assigned whole, not flattened to text")
	}
}

func TestValueAnnotationBuildsArray(t *testing.T) {
	prods := []*Production{
		{Name: "top", Value: &ValueAnnotation{Kind: "array"},
			Alts: []Sequence{{refEl("a"), termEl(","), refEl("b")}}},
		{Name: "a", Alts: []Sequence{{digitsEl()}}},
		{Name: "b", Alts: []Sequence{{digitsEl()}}},
	}
	got := buildValue(t, prods, "top", "1,2")
	if !valueEquals(got, []any{"1", "2"}) {
		t.Errorf("got %#v, want [1 2] as strings", got)
	}
}

// The tree builders and the value builders cannot both run: both own
// r.Node. Appending @object$ after @node$ clobbers the node just
// allocated, and the matching @capture$ then finds no kids and throws.
func TestValueAnnotationDropsTreeBuilders(t *testing.T) {
	spec := emitValue(t, tripleProds(), "top")
	top := altActionsOf(t, spec, "top")
	for _, bad := range []string{"@node$", "@capture$"} {
		if strings.Contains(top, bad) {
			t.Errorf("%s must not survive on a value rule: %s", bad, top)
		}
	}
	// ...but the MEMBERS keep theirs: src reads the node.src they build.
	if member := altActionsOf(t, spec, "b"); !strings.Contains(member, "@node$") {
		t.Errorf("a member still needs its tree builders to accumulate src: %s", member)
	}
}

func TestValueAnnotationRefusesWrongMemberCount(t *testing.T) {
	// A part made only of literals pushes nothing and produces no value,
	// so the names would land on the wrong members.
	prods := []*Production{
		{Name: "top",
			Value: &ValueAnnotation{Kind: "object", Members: []string{"a", "b"}},
			Alts:  []Sequence{{refEl("a"), termEl("x")}}},
		{Name: "a", Alts: []Sequence{{digitsEl()}}},
	}
	_, err := EmitGrammarSpec(&Grammar{Productions: prods},
		&ConvertOptions{Tag: "tst", Start: "top", Builtins: true})
	if err == nil || !strings.Contains(err.Error(), "names 2 members but builds 1") {
		t.Errorf("expected a member-count refusal, got %v", err)
	}
}

func TestValueAnnotationRefusesAlternatives(t *testing.T) {
	// Distinct leading refs, so left factoring cannot merge these into one
	// alternative behind the guard's back.
	prods := []*Production{
		{Name: "top",
			Value: &ValueAnnotation{Kind: "object", Members: []string{"a"}},
			Alts: []Sequence{
				{refEl("a"), termEl("x")},
				{refEl("b"), termEl("y")},
			}},
		{Name: "a", Alts: []Sequence{{digitsEl()}}},
		{Name: "b", Alts: []Sequence{{lettersEl()}}},
	}
	_, err := EmitGrammarSpec(&Grammar{Productions: prods},
		&ConvertOptions{Tag: "tst", Start: "top", Builtins: true})
	if err == nil || !strings.Contains(err.Error(), "alternatives") {
		t.Errorf("expected an alternatives refusal, got %v", err)
	}
}
