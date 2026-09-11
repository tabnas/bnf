package bnf

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
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
	if err == nil || !strings.Contains(err.Error(),
		"names 2 members but has 1 part that produces a value") {
		t.Errorf("expected a member-count refusal, got %v", err)
	}
}

// `a = x ":"` is folded into `top` by left-recursion elimination, so the
// member would capture `x` and silently drop the `":"` that belonged to
// `a`. A rule whose body pushes twice would silently become two members.
// Both are wrong VALUES, not errors, so refuse.
func TestValueAnnotationRefusesLostMemberShape(t *testing.T) {
	prods := []*Production{
		{Name: "top",
			Value: &ValueAnnotation{Kind: "object", Members: []string{"a", "b"}},
			Alts:  []Sequence{{refEl("a"), termEl(","), refEl("b")}}},
		{Name: "a", Alts: []Sequence{{refEl("x"), termEl(":")}}},
		{Name: "x", Alts: []Sequence{{digitsEl()}}},
		{Name: "b", Alts: []Sequence{{digitsEl()}}},
	}
	_, err := EmitGrammarSpec(&Grammar{Productions: prods},
		&ConvertOptions{Tag: "tst", Start: "top", Builtins: true})
	if err == nil || !strings.Contains(err.Error(), "folded into this rule") {
		t.Errorf("expected a lost-shape refusal, got %v", err)
	}
}

// Kind is an unrestricted string in the public IR, so anything can reach
// here; an unknown kind must not fall through as an object.
func TestValueAnnotationRefusesUnknownKind(t *testing.T) {
	prods := []*Production{
		{Name: "top", Value: &ValueAnnotation{Kind: "arry"},
			Alts: []Sequence{{refEl("a")}}},
		{Name: "a", Alts: []Sequence{{digitsEl()}}},
	}
	_, err := EmitGrammarSpec(&Grammar{Productions: prods},
		&ConvertOptions{Tag: "tst", Start: "top", Builtins: true})
	if err == nil || !strings.Contains(err.Error(), "unknown kind 'arry'") {
		t.Errorf("expected an unknown-kind refusal, got %v", err)
	}
}

// `top = child` takes the all-simple shortcut, which emitted tree
// builders and returned before the annotation was ever consulted —
// silently handing back an AST instead of the requested value.
func TestValueAnnotationOnASingleReference(t *testing.T) {
	prods := []*Production{
		{Name: "top",
			Value: &ValueAnnotation{Kind: "object", Members: []string{"child"}},
			Alts:  []Sequence{{refEl("child")}}},
		{Name: "child", Alts: []Sequence{{digitsEl()}}},
	}
	got := buildValue(t, prods, "top", "7")
	if !valueEquals(got, map[string]any{"child": "7"}) {
		t.Errorf("got %#v, want {child: 7}", got)
	}
}

// Nesting is decided by POSITION, not by a member name — an array names
// nothing, so a name-based rule could never nest one.
func TestValueAnnotationNestsAnArrayElement(t *testing.T) {
	prods := []*Production{
		{Name: "top", Value: &ValueAnnotation{Kind: "array"},
			Alts: []Sequence{{refEl("plain"), termEl(","), refEl("one")}}},
		{Name: "plain", Alts: []Sequence{{digitsEl()}}},
		{Name: "one",
			Value: &ValueAnnotation{Kind: "object", Members: []string{"p", "q"}},
			Alts:  []Sequence{{refEl("p"), termEl(":"), refEl("q")}}},
		{Name: "p", Alts: []Sequence{{digitsEl()}}},
		{Name: "q", Alts: []Sequence{{digitsEl()}}},
	}
	got := buildValue(t, prods, "top", "9,1:2")
	want := []any{"9", map[string]any{"p": "1", "q": "2"}}
	if !valueEquals(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// ToRecognitionSpec drops the output-building actions. The value builders
// belong in that set for the same reason the tree builders do — a
// recognition grammar must recognise and build nothing.
func TestValueAnnotationStrippedFromRecognitionSpec(t *testing.T) {
	spec := emitValue(t, tripleProds(), "top")
	rec, err := ToRecognitionSpec(spec)
	if err != nil {
		t.Fatalf("recognition: %v", err)
	}
	b, _ := json.Marshal(rec)
	for _, act := range []string{"@object$", "@key$", "@setval$", "@push$", "@array$"} {
		if strings.Contains(string(b), act) {
			t.Errorf("%s must not survive recognition mode", act)
		}
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

// ---- The plan and the emitter must count the SAME parts -------------
//
// planValueAnnotations runs on the authored grammar and hands the
// emitter one nesting flag per member; the emitter walks segments of the
// REWRITTEN alternative. Every test below is a way those two sequences
// came apart, and each one produced a wrong value in silence rather than
// an error. Mirrors ts/test/value-annotation.test.js.

func groupEl(alts ...Sequence) *Element {
	return &Element{Kind: KindGroup, Alts: alts}
}

// The plan counted only `ref` elements, so a leading group was not a
// member to it — and `inner`'s nesting flag landed on the GROUP, pushing
// an internal tree node as element 0.
func TestValueAnnotationNestsByPositionPastAGroup(t *testing.T) {
	prods := []*Production{
		{Name: "top", Value: &ValueAnnotation{Kind: "array"},
			Alts: []Sequence{{groupEl(Sequence{lettersEl()}), termEl(","),
				refEl("inner")}}},
		{Name: "inner",
			Value: &ValueAnnotation{Kind: "object", Members: []string{"y"}},
			Alts:  []Sequence{{refEl("y")}}},
		{Name: "y", Alts: []Sequence{{digitsEl()}}},
	}
	got := buildValue(t, prods, "top", "ab,3")
	want := []any{"ab", map[string]any{"y": "3"}}
	if !valueEquals(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// Same miscount on the object side, where it showed up as the emitter's
// own count check firing with a number the author could not relate to
// what they wrote. The refusal is now at annotation time.
func TestValueAnnotationCountsAGroupAsAMember(t *testing.T) {
	prods := []*Production{
		{Name: "top",
			Value: &ValueAnnotation{Kind: "object", Members: []string{"inner"}},
			Alts: []Sequence{{groupEl(Sequence{lettersEl()}), termEl(","),
				refEl("inner")}}},
		{Name: "inner", Alts: []Sequence{{digitsEl()}}},
	}
	_, err := EmitGrammarSpec(&Grammar{Productions: prods},
		&ConvertOptions{Tag: "tst", Start: "top", Builtins: true})
	if err == nil ||
		!strings.Contains(err.Error(), "names 1 member but has 2 parts that produce a value") {
		t.Errorf("expected a part-count refusal, got %v", err)
	}
}

// liftLiteralTokens turns a single-literal production into a named lexer
// token and deletes the rule. Doing that to an ANNOTATED rule discarded
// its builders with no diagnostic, and removed it from its caller's
// member list at the same time — so the caller's remaining flags shifted
// onto the wrong parts and nested an internal node.
func TestValueAnnotationSurvivesTheLiteralLift(t *testing.T) {
	prods := []*Production{
		{Name: "top", Value: &ValueAnnotation{Kind: "array"},
			Alts: []Sequence{{refEl("n"), refEl("s"), refEl("m")}}},
		{Name: "n", Alts: []Sequence{{digitsEl()}}},
		{Name: "s", Value: &ValueAnnotation{Kind: "object"},
			Alts: []Sequence{{termEl("+")}}},
		{Name: "m", Alts: []Sequence{{digitsEl()}}},
	}
	got := buildValue(t, prods, "top", "12+34")
	want := []any{"12", map[string]any{}, "34"}
	if !valueEquals(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// The array side had nothing checking the plan against the segments: an
// object at least compared its NAMES. A member whose own rule is one
// literal becomes a lexer token, so it stops pushing — and every later
// flag then sits one place too early.
func TestValueAnnotationRefusesAChangedPartCount(t *testing.T) {
	prods := []*Production{
		{Name: "top", Value: &ValueAnnotation{Kind: "array"},
			Alts: []Sequence{{refEl("n"), refEl("s"), refEl("m")}}},
		{Name: "n", Alts: []Sequence{{digitsEl()}}},
		{Name: "s", Alts: []Sequence{{termEl("+")}}},
		{Name: "m", Alts: []Sequence{{digitsEl()}}},
	}
	_, err := EmitGrammarSpec(&Grammar{Productions: prods},
		&ConvertOptions{Tag: "tst", Start: "top", Builtins: true})
	if err == nil ||
		!strings.Contains(err.Error(), "annotation for 3 parts but builds 2") {
		t.Errorf("expected a plan-length refusal, got %v", err)
	}
}

// ---- The leading fold, followed the whole way ----------------------

// `a = b` is a single part, so the check passed — but the pass inlines
// `a` into `top` and then `b` into that, so what landed was `b`'s
// two-part body and the member silently lost its ':'.
func TestValueAnnotationFollowsAnAliasChain(t *testing.T) {
	prods := []*Production{
		{Name: "top",
			Value: &ValueAnnotation{Kind: "object", Members: []string{"a", "c"}},
			Alts:  []Sequence{{refEl("a"), termEl(","), refEl("c")}}},
		{Name: "a", Alts: []Sequence{{refEl("b")}}},
		{Name: "b", Alts: []Sequence{{digitsEl(), termEl(":")}}},
		{Name: "c", Alts: []Sequence{{digitsEl()}}},
	}
	_, err := EmitGrammarSpec(&Grammar{Productions: prods},
		&ConvertOptions{Tag: "tst", Start: "top", Builtins: true})
	if err == nil || !strings.Contains(err.Error(), "not a single part") {
		t.Errorf("expected a fold refusal, got %v", err)
	}
}

// Inlining erases that rule's builders, so the member held an internal
// tree node where the author had asked for the object `a` is annotated
// to build.
func TestValueAnnotationRefusesAnAnnotatedLeadingMember(t *testing.T) {
	prods := []*Production{
		{Name: "top",
			Value: &ValueAnnotation{Kind: "object", Members: []string{"a", "c"}},
			Alts:  []Sequence{{refEl("a"), termEl(","), refEl("c")}}},
		{Name: "a",
			Value: &ValueAnnotation{Kind: "object", Members: []string{"x"}},
			Alts:  []Sequence{{refEl("x")}}},
		{Name: "x", Alts: []Sequence{{digitsEl()}}},
		{Name: "c", Alts: []Sequence{{digitsEl()}}},
	}
	_, err := EmitGrammarSpec(&Grammar{Productions: prods},
		&ConvertOptions{Tag: "tst", Start: "top", Builtins: true})
	if err == nil ||
		!strings.Contains(err.Error(), "erases the value 'a' is annotated to build") {
		t.Errorf("expected an erased-builder refusal, got %v", err)
	}
}

// The escape hatch the diagnostic above offers has to actually work: a
// literal in front means the reference is no longer leading, so nothing
// is inlined and the member nests whole.
func TestValueAnnotationNestsAGuardedLeadingMember(t *testing.T) {
	prods := []*Production{
		{Name: "top",
			Value: &ValueAnnotation{Kind: "object", Members: []string{"a", "c"}},
			Alts: []Sequence{{termEl("v"), refEl("a"), termEl(","),
				refEl("c")}}},
		{Name: "a",
			Value: &ValueAnnotation{Kind: "object", Members: []string{"x"}},
			Alts:  []Sequence{{refEl("x")}}},
		{Name: "x", Alts: []Sequence{{digitsEl()}}},
		{Name: "c", Alts: []Sequence{{digitsEl()}}},
	}
	got := buildValue(t, prods, "top", "v1,2")
	want := map[string]any{"a": map[string]any{"x": "1"}, "c": "2"}
	if !valueEquals(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// Chasing the chain has to have a stop. `a = b`, `b = a` is left
// recursion reached through aliases; the fold check must refuse rather
// than loop.
func TestValueAnnotationTerminatesOnAnAliasCycle(t *testing.T) {
	prods := []*Production{
		{Name: "top",
			Value: &ValueAnnotation{Kind: "object", Members: []string{"a"}},
			Alts:  []Sequence{{refEl("a")}}},
		{Name: "a", Alts: []Sequence{{refEl("b")}}},
		{Name: "b", Alts: []Sequence{{refEl("a")}}},
	}
	_, err := EmitGrammarSpec(&Grammar{Productions: prods},
		&ConvertOptions{Tag: "tst", Start: "top", Builtins: true})
	if err == nil || !strings.Contains(err.Error(), "not a single part") {
		t.Errorf("expected a fold refusal, got %v", err)
	}
}

// ---- `members` is data, not a type promise -------------------------

// Go's Members is []string, so it cannot hold the nil the TypeScript IR
// can — but "" reaches here from either, and the two ports DISAGREED
// about it: TS built the key "", Go skipped the @key$ entirely and let
// @setval$ write into whatever key the previous part had left behind.
func TestValueAnnotationRefusesAnEmptyMemberName(t *testing.T) {
	prods := []*Production{
		{Name: "top",
			Value: &ValueAnnotation{Kind: "object", Members: []string{"", "b"}},
			Alts:  []Sequence{{refEl("a"), refEl("b")}}},
		{Name: "a", Alts: []Sequence{{lettersEl()}}},
		{Name: "b", Alts: []Sequence{{digitsEl()}}},
	}
	_, err := EmitGrammarSpec(&Grammar{Productions: prods},
		&ConvertOptions{Tag: "tst", Start: "top", Builtins: true})
	if err == nil || !strings.Contains(err.Error(), "is not a name") {
		t.Errorf("expected an empty-name refusal, got %v", err)
	}
}

// An array's parts are positional. Ignoring the names hid the real
// mistake, which is that the author meant `object`.
func TestValueAnnotationRefusesANamedArray(t *testing.T) {
	prods := []*Production{
		{Name: "top",
			Value: &ValueAnnotation{Kind: "array", Members: []string{"a"}},
			Alts:  []Sequence{{refEl("a")}}},
		{Name: "a", Alts: []Sequence{{digitsEl()}}},
	}
	_, err := EmitGrammarSpec(&Grammar{Productions: prods},
		&ConvertOptions{Tag: "tst", Start: "top", Builtins: true})
	if err == nil ||
		!strings.Contains(err.Error(), "positional and are not named") {
		t.Errorf("expected a named-array refusal, got %v", err)
	}
}

// ---- Diagnostics ---------------------------------------------------

// The plan runs before any rewrite — and in the TypeScript port it used
// to run before the diagnostic prefix was set, so the first bad grammar
// in a process reported `bnf:` and every later one inherited the
// PREVIOUS conversion's tag. Pinned here too so the ports cannot drift.
func TestValueAnnotationDiagnosticNamesTheNotation(t *testing.T) {
	for _, tag := range []string{"gbnf", "ebnf"} {
		prods := []*Production{
			{Name: "top", Value: &ValueAnnotation{Kind: "nope"},
				Alts: []Sequence{{digitsEl()}}},
		}
		_, err := EmitGrammarSpec(&Grammar{Productions: prods},
			&ConvertOptions{Tag: tag, Start: "top", Builtins: true})
		if err == nil || !strings.HasPrefix(err.Error(), tag+": ") {
			t.Errorf("tag %s: got %v", tag, err)
		}
	}
}

// Every other diagnostic in this compiler can say WHERE. These are the
// ones an author is most likely to hit, and they were the ones with
// nothing to underline.
func TestValueAnnotationDiagnosticCarriesTheSpan(t *testing.T) {
	prods := []*Production{
		{Name: "top", Value: &ValueAnnotation{Kind: "nope"},
			Sp: &SrcSpan{S: 12, E: 20}, Alts: []Sequence{{digitsEl()}}},
	}
	_, err := EmitGrammarSpec(&Grammar{Productions: prods},
		&ConvertOptions{Tag: "tst", Start: "top", Builtins: true})
	var ee *EmitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected an EmitError, got %v", err)
	}
	if ee.Rule != "top" {
		t.Errorf("rule: got %q, want top", ee.Rule)
	}
	if ee.Sp == nil || ee.Sp.S != 12 || ee.Sp.E != 20 {
		t.Errorf("span: got %#v, want S=12 E=20", ee.Sp)
	}
}

// Two conversions at once must not read each other's plan. The flags
// lived in a package-level map, so the second call's planValueAnnotations
// replaced the first's mid-emit — and `-race` is what says so.
func TestValueAnnotationConcurrentEmitsDoNotShareAPlan(t *testing.T) {
	nested := []*Production{
		{Name: "top", Value: &ValueAnnotation{Kind: "array"},
			Alts: []Sequence{{termEl("<"), refEl("one"), termEl(">")}}},
		{Name: "one",
			Value: &ValueAnnotation{Kind: "object", Members: []string{"p"}},
			Alts:  []Sequence{{refEl("p")}}},
		{Name: "p", Alts: []Sequence{{digitsEl()}}},
	}
	flat := []*Production{
		{Name: "top", Value: &ValueAnnotation{Kind: "array"},
			Alts: []Sequence{{termEl("<"), refEl("one"), termEl(">")}}},
		{Name: "one", Alts: []Sequence{{digitsEl()}}},
	}
	var wg sync.WaitGroup
	errs := make(chan string, 200)
	for i := 0; i < 100; i++ {
		for _, c := range []struct {
			prods []*Production
			want  any
		}{{nested, []any{map[string]any{"p": "4"}}}, {flat, []any{"4"}}} {
			wg.Add(1)
			go func(prods []*Production, want any) {
				defer wg.Done()
				spec, err := EmitGrammarSpec(&Grammar{Productions: prods},
					&ConvertOptions{Tag: "tst", Start: "top", Builtins: true})
				if err != nil {
					errs <- "emit: " + err.Error()
					return
				}
				j := tabnas.Make()
				if err := j.Grammar(spec); err != nil {
					errs <- "install: " + err.Error()
					return
				}
				out, err := j.Parse("<4>")
				if err != nil {
					errs <- "parse: " + err.Error()
					return
				}
				if got := tabnas.UnwrapUndefined(out); !valueEquals(got, want) {
					errs <- fmt.Sprintf("got %#v, want %#v", got, want)
				}
			}(c.prods, c.want)
		}
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// The user-visible half of the race above, asserted without needing the
// detector: two conversions running at once must each report their OWN
// notation. `diagPrefix` is package state, so the loser used to describe
// its grammar with the winner's tag — "gbnf: rule 'top' ..." on an error
// about a rule an ABNF author wrote.
func TestConcurrentEmitsKeepTheirOwnDiagPrefix(t *testing.T) {
	bad := func() []*Production {
		return []*Production{
			{Name: "top", Value: &ValueAnnotation{Kind: "nope"},
				Alts: []Sequence{{digitsEl()}}},
		}
	}
	var wg sync.WaitGroup
	errs := make(chan string, 400)
	for i := 0; i < 200; i++ {
		for _, tag := range []string{"gbnf", "ebnf"} {
			wg.Add(1)
			go func(tag string) {
				defer wg.Done()
				_, err := EmitGrammarSpec(&Grammar{Productions: bad()},
					&ConvertOptions{Tag: tag, Start: "top", Builtins: true})
				if err == nil {
					errs <- "expected a refusal for tag " + tag
					return
				}
				if !strings.HasPrefix(err.Error(), tag+": ") {
					errs <- fmt.Sprintf("tag %s got %q", tag, err.Error())
				}
			}(tag)
		}
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// An UNANNOTATED caller erases an annotated rule just as thoroughly, and
// nothing was looking at it: the planner only ever walked productions
// that named members. `top = leaf ","` with an annotated `leaf` returned
// an ordinary AST — `{rule:"top",src:"x,",kids:[]}` — with the requested
// value nowhere in it.
func TestValueAnnotationRefusesAnUnannotatedLeadingCaller(t *testing.T) {
	prods := []*Production{
		{Name: "top", Alts: []Sequence{{refEl("leaf"), termEl(",")}}},
		{Name: "leaf",
			Value: &ValueAnnotation{Kind: "object", Members: []string{"d"}},
			Alts:  []Sequence{{refEl("d")}}},
		{Name: "d", Alts: []Sequence{{digitsEl()}}},
	}
	_, err := EmitGrammarSpec(&Grammar{Productions: prods},
		&ConvertOptions{Tag: "tst", Start: "top", Builtins: true})
	if err == nil ||
		!strings.Contains(err.Error(), "erases the value 'leaf' is annotated to build") {
		t.Errorf("expected an erased-builder refusal, got %v", err)
	}
}

// A pure alias is the one caller Paull's pass does NOT substitute into,
// so `top = child` keeps its reference and an annotated `child` nests
// whole. Refusing it was a refusal of a shape that works — and a
// one-member wrapper is the most natural way to reach for it.
func TestValueAnnotationNestsThroughAnAnnotatedAlias(t *testing.T) {
	prods := []*Production{
		{Name: "top",
			Value: &ValueAnnotation{Kind: "object", Members: []string{"child"}},
			Alts:  []Sequence{{refEl("child")}}},
		{Name: "child",
			Value: &ValueAnnotation{Kind: "object", Members: []string{"d"}},
			Alts:  []Sequence{{refEl("d")}}},
		{Name: "d", Alts: []Sequence{{digitsEl()}}},
	}
	got := buildValue(t, prods, "top", "7")
	want := map[string]any{"child": map[string]any{"d": "7"}}
	if !valueEquals(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// The tail-repeat refusal is ranged like the rest: the rewrite mutates
// the production in place and keeps its span.
func TestValueAnnotationTailRepeatRefusalCarriesTheSpan(t *testing.T) {
	prods := []*Production{
		{Name: "top", Alts: []Sequence{{refEl("add")}}},
		{Name: "add",
			Value: &ValueAnnotation{Kind: "array"},
			Sp:    &SrcSpan{S: 5, E: 25},
			// `add = [0-9]+ [ "+" add ]` — the prefix and the separator have
			// to be BARE terminals for this to be read as a tail repeat; a
			// `1*DIGIT` repetition is not one.
			Alts: []Sequence{{rxEl("[0-9]+", ""),
				&Element{Kind: KindOpt, Inner: groupEl(Sequence{termEl("+"), refEl("add")})}}}},
	}
	_, err := EmitGrammarSpec(&Grammar{Productions: prods},
		&ConvertOptions{Tag: "tst", Start: "top", Builtins: true})
	var ee *EmitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected an EmitError, got %v", err)
	}
	if !strings.Contains(ee.Message, "same-depth repeat") {
		t.Fatalf("expected the tail-repeat refusal, got %q", ee.Message)
	}
	if ee.Sp == nil || ee.Sp.S != 5 || ee.Sp.E != 25 {
		t.Errorf("span: got %#v, want S=5 E=25", ee.Sp)
	}
}

// A composed `a` must stay FLAT when a user action or slot is attached
// to it. The builders were stored as []string, which appendAction's type
// switch did not recognise, so attaching produced the nested
// [["@object$","@key$"], ref] where TypeScript produces the flat three —
// and the engine cannot resolve a list inside a list. This is the first
// place in the emitter to compose actions at all, so nothing had
// exercised it.
func TestValueAnnotationComposedActionStaysFlat(t *testing.T) {
	prods := []*Production{
		{Name: "top",
			Value: &ValueAnnotation{Kind: "object", Members: []string{"a", "b"}},
			Alts:  []Sequence{{refEl("a"), termEl("."), refEl("b")}}},
		{Name: "a", Alts: []Sequence{{digitsEl()}}},
		{Name: "b", Alts: []Sequence{{digitsEl()}}},
	}
	spec, err := EmitGrammarSpec(&Grammar{Productions: prods},
		&ConvertOptions{Tag: "tst", Start: "top", Builtins: true, Marks: true})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	open, ok := spec.Rule["top"].Open.([]*tabnas.GrammarAltSpec)
	if !ok || len(open) == 0 {
		t.Fatalf("top open is %T", spec.Rule["top"].Open)
	}
	mark, _ := open[0].U["m$"].(string)
	ref := "@top:o:" + mark
	if err := AttachActionSlots(spec, []string{ref}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	got, _ := json.Marshal(open[0].A)
	want := `["@object$","@key$","` + ref + `"]`
	if string(got) != want {
		t.Errorf("composed action: got %s, want %s", got, want)
	}
}

// appendAction itself must flatten a []string, not only a []any: a
// composed `a` reaches it from a hand-written spec or a JSON round-trip
// as readily as from this package.
func TestAppendActionFlattensAStringSlice(t *testing.T) {
	got, _ := json.Marshal(appendAction([]string{"x", "y"}, "z"))
	if string(got) != `["x","y","z"]` {
		t.Errorf("got %s, want [x y z]", got)
	}
}
