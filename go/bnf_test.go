// Copyright (c) 2026 tabnas, MIT License

// Smoke tests for the notation-neutral compiler. The heavy verification
// lives downstream, in the front-ends' suites — this package has no
// notation of its own to exercise it. What is checked here is that the
// surface a front-end actually uses works with no front-end present.
package bnf

import (
	"sort"
	"strings"
	"testing"

	tabnas "github.com/tabnas/parser/go"
)

func ref(name string) *Element { return &Element{Kind: KindRef, Name: name} }
func tok(name string) *Element { return &Element{Kind: KindToken, Name: name} }
func term(lit string) *Element {
	return &Element{Kind: KindTerm, Literal: lit}
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
			if len(alt) > 0 && alt[0].Kind == KindRef && alt[0].Name == "expr" {
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

// ---- suffix debt (issue #6) ----------------------------------------
//
// Left-recursion elimination turns `A = ["x"] A "y" / "z"` into
// `A = ( "x" A "y" | "z" ) "y"*`. That is a correct CFG and a broken
// parser: the tail loop is greedy, so the inner A eats the `"y"` the
// enclosing `"x" A "y"` still owes and the outer alternative starves. No
// lookahead settles it — the repeated token and the follow token are the
// same token, and the answer depends on enclosing stack depth.

func optOf(inner *Element) *Element { return &Element{Kind: KindOpt, Inner: inner} }

func sensTerm(lit string) *Element {
	return &Element{Kind: KindTerm, Literal: lit, CaseSensitive: true, HasCaseSens: true}
}

// altsOf flattens every alt of every rule: the counter machinery is
// spread across a dispatcher and its `$altN` chain rules.
func altsOf(spec *tabnas.GrammarSpec) []*tabnas.GrammarAltSpec {
	out := []*tabnas.GrammarAltSpec{}
	for _, rs := range spec.Rule {
		if rs == nil {
			continue
		}
		out = append(out, altListOf(rs.Open)...)
		out = append(out, altListOf(rs.Close)...)
	}
	return out
}

func emitOrFail(t *testing.T, g *Grammar) *tabnas.GrammarSpec {
	t.Helper()
	spec, err := EmitGrammarSpec(g, &ConvertOptions{Tag: "demo"})
	if err != nil {
		t.Fatalf("emit failed: %v", err)
	}
	return spec
}

func hiddenLeftRec(alts ...Sequence) *Grammar {
	return &Grammar{Productions: []*Production{{Name: "A", Alts: alts}}}
}

func TestSuffixDebtGuardsTheContestedLoop(t *testing.T) {
	spec := emitOrFail(t, hiddenLeftRec(
		Sequence{optOf(sensTerm("x")), ref("A"), sensTerm("y")},
		Sequence{sensTerm("z")}))

	// The alternative that pushes the inner A increments the counter…
	counter := ""
	pushes := 0
	for _, a := range altsOf(spec) {
		if len(a.N) == 0 {
			continue
		}
		pushes++
		for name, delta := range a.N {
			counter = name
			if delta != 1 {
				t.Errorf("expected the push to add one debt, got %d", delta)
			}
		}
		if a.P != "A" {
			t.Errorf("the debt must ride on the push of A, got %q", a.P)
		}
	}
	if pushes != 1 {
		t.Fatalf("expected exactly one debt-carrying push, got %d", pushes)
	}

	// …and the tail loop's continue alternative refuses to run while any
	// debt is outstanding.
	guards := 0
	for _, a := range altsOf(spec) {
		cd, ok := a.C.(map[string]any)
		if !ok {
			continue
		}
		guards++
		if got := cd["n."+counter]; got != 0 {
			t.Errorf("expected the guard to test %s == 0, got %#v", counter, cd)
		}
	}
	if guards != 1 {
		t.Fatalf("expected exactly one guarded alternative, got %d", guards)
	}

	// The loop's exits stay unguarded, so it yields rather than fails.
	for name, rs := range spec.Rule {
		if rs == nil || !strings.Contains(name, "_star_") || strings.Contains(name, "$") {
			continue
		}
		for i, a := range altListOf(rs.Open) {
			if i > 0 && a.C != nil {
				t.Errorf("%s: exit alternative %d must stay unconditional", name, i)
			}
		}
	}
}

func TestSuffixDebtResetsAcrossAReanchoringAlternative(t *testing.T) {
	// `"(" A ")"` owes a `")"`, not a `"y"`, so the A it pushes must start
	// from a clean slate or `x(zy)y` cannot parse.
	spec := emitOrFail(t, hiddenLeftRec(
		Sequence{optOf(sensTerm("x")), ref("A"), sensTerm("y")},
		Sequence{sensTerm("("), ref("A"), sensTerm(")")},
		Sequence{sensTerm("z")}))

	deltas := []int{}
	for _, a := range altsOf(spec) {
		for _, d := range a.N {
			deltas = append(deltas, d)
		}
	}
	sort.Ints(deltas)
	if len(deltas) != 2 || deltas[0] != 0 || deltas[1] != 1 {
		t.Errorf("expected one increment and one barrier reset, got %v", deltas)
	}
}

func TestSuffixDebtLeavesUncontestedGrammarsAlone(t *testing.T) {
	cases := map[string]*Grammar{
		// The loop repeats `"w"`; the only suffix after a self-reference is
		// `")"`, which never competes with it. A guard here would stop `(z)w`
		// parsing.
		"disjoint suffix": hiddenLeftRec(
			Sequence{ref("A"), sensTerm("w")},
			Sequence{sensTerm("("), ref("A"), sensTerm(")")},
			Sequence{sensTerm("z")}),
		// A nullable suffix commits the enclosing frame to nothing, so the
		// loop stays greedy.
		"nullable suffix": hiddenLeftRec(
			Sequence{optOf(sensTerm("x")), ref("A"), optOf(sensTerm("y"))},
			Sequence{sensTerm("z")}),
		"plain direct left recursion": hiddenLeftRec(
			Sequence{ref("A"), sensTerm("y")},
			Sequence{sensTerm("z")}),
	}
	for name, g := range cases {
		for _, a := range altsOf(emitOrFail(t, g)) {
			if len(a.N) > 0 || a.C != nil {
				t.Errorf("%s: must compile exactly as before, got n=%v c=%v",
					name, a.N, a.C)
			}
		}
	}
}

func TestSuffixDebtAllocatesACounterPerContestedRule(t *testing.T) {
	spec := emitOrFail(t, &Grammar{Productions: []*Production{
		{Name: "B", Alts: []Sequence{
			{optOf(sensTerm("p")), ref("B"), sensTerm("q")},
			{ref("A")},
		}},
		{Name: "A", Alts: []Sequence{
			{optOf(sensTerm("x")), ref("A"), sensTerm("y")},
			{sensTerm("z")},
		}},
	}})

	counters := map[string]bool{}
	for _, a := range altsOf(spec) {
		if cd, ok := a.C.(map[string]any); ok {
			for path := range cd {
				counters[path] = true
			}
		}
	}
	if len(counters) != 2 {
		t.Errorf("expected one counter per contested rule, got %v", counters)
	}
}

// Hidden left recursion has to become direct left recursion before the
// tail loop exists at all: without the split the generated optional
// helper takes its empty branch and exposes A again at the same source
// position.
func TestExpandsNullableLeftPrefixes(t *testing.T) {
	out := EliminateLeftRecursion(hiddenLeftRec(
		Sequence{optOf(sensTerm("x")), ref("A"), sensTerm("y")},
		Sequence{sensTerm("z")}))

	var a *Production
	for _, p := range out.Productions {
		if p.Name == "A" {
			a = p
		}
	}
	if a == nil {
		t.Fatal("expected rule A to survive")
	}
	for _, alt := range a.Alts {
		if len(alt) > 0 && alt[0].Kind == KindRef && alt[0].Name == "A" {
			t.Error("A still re-enters itself immediately")
		}
		if len(alt) > 1 && alt[0].Kind == KindOpt &&
			alt[1].Kind == KindRef && alt[1].Name == "A" {
			t.Error("A still re-enters itself behind an opt")
		}
	}
}

func TestSuffixDebtGuardsOnlyTheContestedBranches(t *testing.T) {
	// The loop repeats `"y"` and `"w"`; the suffix owes a `"y"`. Blocking
	// the `"w"` branch too would reject `xzwy`, where the inner A has to
	// consume the `w` before yielding the `y`.
	spec := emitOrFail(t, hiddenLeftRec(
		Sequence{ref("A"), sensTerm("y")},
		Sequence{ref("A"), sensTerm("w")},
		Sequence{sensTerm("x"), ref("A"), sensTerm("y")},
		Sequence{sensTerm("z")}))

	tokenOf := func(lit string) string {
		for name, v := range spec.Options.Fixed.Token {
			if v != nil && *v == lit {
				return name
			}
		}
		t.Fatalf("no token for %q", lit)
		return ""
	}

	// Continue alternatives fan out to k-token prefixes, so group them by
	// the head token that decides which branch they are.
	guarded, open := map[string]bool{}, map[string]bool{}
	for name, rs := range spec.Rule {
		if rs == nil || !strings.Contains(name, "_star_") || strings.Contains(name, "$") {
			continue
		}
		for _, a := range altListOf(rs.Open) {
			s, ok := a.S.(string)
			if !ok || a.P == "" {
				continue
			}
			head := s
			if i := strings.IndexByte(s, ' '); i >= 0 {
				head = s[:i]
			}
			if a.C != nil {
				guarded[head] = true
			} else {
				open[head] = true
			}
		}
	}
	if want := []string{tokenOf("y")}; !sameStrings(sortedKeys(guarded), want) {
		t.Errorf("guarded heads = %v, want %v", sortedKeys(guarded), want)
	}
	if want := []string{tokenOf("w")}; !sameStrings(sortedKeys(open), want) {
		t.Errorf("unguarded heads = %v, want %v", sortedKeys(open), want)
	}
}

func TestSuffixDebtSeesASelfReferenceInsideAGroup(t *testing.T) {
	// The detector runs before desugar, where the recursive call is still
	// inside an IR group. Reading only the top level of each seed missed
	// this shape entirely.
	spec := emitOrFail(t, hiddenLeftRec(
		Sequence{ref("A"), sensTerm("y")},
		Sequence{&Element{Kind: KindGroup, Alts: []Sequence{
			{sensTerm("x"), ref("A"), sensTerm("y")},
			{sensTerm("z")},
		}}}))

	guards := 0
	for _, a := range altsOf(spec) {
		if a.C != nil {
			guards++
		}
	}
	if guards != 1 {
		t.Errorf("expected the grouped seed to allocate a guard, got %d", guards)
	}
}

func TestSuffixDebtCounterNameMatchesTypeScript(t *testing.T) {
	// TypeScript sanitises with a Unicode-aware regular expression, so an
	// astral rule name reduces to one underscore per code point in both
	// runtimes rather than one per UTF-16 surrogate half.
	for name, want := range map[string]string{
		"a-b":          "debt_a_b",
		"\U0001F600":   "debt__",
		"x\U0001F600y": "debt_x_y",
	} {
		spec := emitOrFail(t, &Grammar{Productions: []*Production{{Name: name, Alts: []Sequence{
			{optOf(sensTerm("x")), ref(name), sensTerm("y")},
			{sensTerm("z")},
		}}}})
		got := ""
		for _, a := range altsOf(spec) {
			for counter := range a.N {
				got = counter
			}
		}
		if got != want {
			t.Errorf("rule %q: counter = %q, want %q", name, got, want)
		}
	}
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A tail repeat's separator is moved out of Alts and stashed on
// TailRepeat, so token allocation — which walks Alts — never saw it. A
// separator whose literal appears nowhere else in the grammar therefore
// got no token, the emitted separator alternate came out as `s: ""`, and
// the repeat could never match: the grammar silently accepted a single
// element instead of a list. Mirrored by ts/test/bnf.test.js.
func TestTailRepeatSeparatorGetsAToken(t *testing.T) {
	// `list = DIGIT [ "," list ]`, with the comma used NOWHERE else, is
	// the isolating case. Real grammars usually reuse the separator
	// literal in another rule and pick up that rule's token by accident,
	// which is how this survived.
	spec, err := EmitGrammarSpec(&Grammar{Productions: []*Production{
		{Name: "doc", Alts: []Sequence{{ref("list")}}},
		{Name: "list", Alts: []Sequence{{
			{Kind: KindRegex, Pattern: "[0-9]"},
			{Kind: KindOpt, Inner: &Element{Kind: KindGroup, Alts: []Sequence{
				{term(","), ref("list")},
			}}},
		}}},
	}}, &ConvertOptions{Tag: "demo", Start: "doc"})
	if err != nil {
		t.Fatalf("emit failed: %v", err)
	}

	alts, ok := spec.Rule["list"].Close.([]*tabnas.GrammarAltSpec)
	if !ok || 0 == len(alts) {
		t.Fatalf("list has no close alternates: %#v", spec.Rule["list"])
	}
	sep, _ := alts[0].S.(string)
	if "" == sep {
		t.Fatalf("separator alternate names no token (s: %#v) — the comma "+
			"was never allocated one, so the repeat can never match", alts[0].S)
	}
	if !strings.HasPrefix(sep, "#") {
		t.Fatalf("separator alternate s = %q, want a #token name", sep)
	}
}
