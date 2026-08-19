// Copyright (c) 2026 tabnas, MIT License

// Smoke tests for the notation-neutral compiler. The heavy verification
// lives downstream, in the front-ends' suites — this package has no
// notation of its own to exercise it. What is checked here is that the
// surface a front-end actually uses works with no front-end present.
package bnf

import (
	"encoding/json"
	"errors"
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

// ---- provenance ----------------------------------------------------
//
// `Meta["provenance"]` maps every rule the compiler MINTED back to the
// author-written production it came from. A compiled grammar carries an
// order of magnitude more rules than the author wrote, and all of them
// surface in rule stacks, hover and completion, so a tool that cannot
// resolve a generated name has nothing useful to show. Mirrored by
// ts/test/bnf.test.js.

// `doc` repeats `item`. The star helper is generated FOR doc, and the
// only rule name embedded in its own generated name is `item` — the rule
// being repeated. Attributing it to `item` is the mistake the map exists
// to prevent, and the mistake any name-parsing implementation would
// make.
func repeatGrammar() *Grammar {
	return &Grammar{Productions: []*Production{
		{Name: "doc", Alts: []Sequence{
			{ref("item"), {Kind: KindStar, Inner: ref("item")}},
		}},
		{Name: "item", Alts: []Sequence{{term("a")}, {term("b")}}},
	}}
}

func provenanceOf(t *testing.T, spec *tabnas.GrammarSpec) map[string]string {
	t.Helper()
	if spec.Meta == nil {
		t.Fatal("spec carries no meta")
	}
	raw, ok := spec.Meta["provenance"].(map[string]any)
	if !ok {
		t.Fatalf("meta.provenance missing or wrong shape: %#v", spec.Meta)
	}
	out := map[string]string{}
	for name, origin := range raw {
		s, ok := origin.(string)
		if !ok {
			t.Fatalf("provenance[%q] = %#v, want a rule name", name, origin)
		}
		out[name] = s
	}
	return out
}

func boolPtr(b bool) *bool { return &b }

func TestProvenanceAttributesAHelperToItsEnclosingRule(t *testing.T) {
	spec := emitOrFail(t, repeatGrammar())
	prov := provenanceOf(t, spec)

	star := ""
	for name := range spec.Rule {
		if strings.HasPrefix(name, "_gen") && strings.Contains(name, "star_item") &&
			!strings.Contains(name, "$") {
			star = name
		}
	}
	if star == "" {
		t.Fatal("expected a generated star helper named after item")
	}
	if got := prov[star]; got != "doc" {
		t.Errorf("%s belongs to doc, which repeats item — not to %q itself",
			star, got)
	}
}

func TestProvenanceAttributesEveryGeneratedRuleAndOnlyToAuthoredOnes(t *testing.T) {
	grammar := repeatGrammar()
	authored := map[string]bool{}
	for _, p := range grammar.Productions {
		authored[p.Name] = true
	}
	spec := emitOrFail(t, grammar)
	prov := provenanceOf(t, spec)

	for name := range spec.Rule {
		if authored[name] {
			if _, listed := prov[name]; listed {
				t.Errorf("%s is author-written and must not be listed", name)
			}
			continue
		}
		origin, listed := prov[name]
		if !listed {
			t.Errorf("generated rule %s has no provenance", name)
			continue
		}
		if !authored[origin] {
			t.Errorf("%s resolves to %s, which the author never wrote", name, origin)
		}
	}

	// An entry naming a rule that was never emitted is a phantom: the
	// empty alternative of a repetition helper allocates a `$altN` name
	// and then continues without emitting anything.
	for name := range prov {
		if _, emitted := spec.Rule[name]; !emitted {
			t.Errorf("%s has provenance but was not emitted", name)
		}
	}
}

func TestProvenanceNamesTheStartWrapperAfterTheStartRule(t *testing.T) {
	spec := emitOrFail(t, repeatGrammar())
	if got := provenanceOf(t, spec)["__start__"]; got != "doc" {
		t.Errorf("__start__ provenance = %q, want the start rule doc", got)
	}
}

func TestProvenanceSurvivesCompilation(t *testing.T) {
	// The consumer that needs provenance most loads COMPILED grammars,
	// and every shape rebuilds the spec from {options, rule} alone.
	spec, err := EmitGrammarSpec(repeatGrammar(),
		&ConvertOptions{Tag: "demo", Start: "doc", Builtins: true})
	if err != nil {
		t.Fatalf("emit failed: %v", err)
	}

	shapes := map[string]func(*tabnas.GrammarSpec) (map[string]any, error){
		"ToPureSpec":        ToPureSpec,
		"ToRecognitionSpec": ToRecognitionSpec,
	}
	for name, shape := range shapes {
		out, err := shape(spec)
		if err != nil {
			t.Fatalf("%s failed: %v", name, err)
		}
		meta, _ := out["meta"].(map[string]any)
		prov, _ := meta["provenance"].(map[string]any)
		if prov["__start__"] != "doc" {
			t.Errorf("%s dropped meta.provenance (got %#v)", name, out["meta"])
		}
	}

	// ...and through serialisation, which is how it reaches a tool.
	pure, err := ToPureSpec(spec)
	if err != nil {
		t.Fatalf("ToPureSpec failed: %v", err)
	}
	var round map[string]any
	if err := json.Unmarshal([]byte(ToJsonic(pure, true, 0)), &round); err != nil {
		t.Fatalf("serialised grammar is not JSON: %v", err)
	}
	meta, _ := round["meta"].(map[string]any)
	prov, _ := meta["provenance"].(map[string]any)
	if prov["__start__"] != "doc" {
		t.Errorf("serialisation dropped meta.provenance (got %#v)", round["meta"])
	}
}

func TestProvenanceCanBeTurnedOff(t *testing.T) {
	// Off is opt-IN: a caller that says nothing gets the map, exactly as
	// the TypeScript `{tag: 'demo'}` does.
	spec, err := EmitGrammarSpec(repeatGrammar(),
		&ConvertOptions{Tag: "demo", Start: "doc", Provenance: boolPtr(false)})
	if err != nil {
		t.Fatalf("emit failed: %v", err)
	}
	if spec.Meta != nil {
		t.Errorf("expected no meta with provenance off, got %#v", spec.Meta)
	}
}

// ---- recovery sync tags --------------------------------------------
//
// Only a CLOSE alternate that names a token can be a recovery sync
// point, and this emitter produces exactly two: the `__start__`
// wrapper's `#ZZ` and a tail repeat's separator continuation. Tagging
// half of them would silently delete the other half's sync points, so
// both carry their group appended to the caller's tag.

// listGrammar is `doc = list`, `list = DIGIT [ "," list ]` — the tail
// repeat whose separator alternate is the second taggable close alt.
func listGrammar() *Grammar {
	return &Grammar{Productions: []*Production{
		{Name: "doc", Alts: []Sequence{{ref("list")}}},
		{Name: "list", Alts: []Sequence{{
			{Kind: KindRegex, Pattern: "[0-9]"},
			{Kind: KindOpt, Inner: &Element{Kind: KindGroup, Alts: []Sequence{
				{term(","), ref("list")},
			}}},
		}}},
	}}
}

func TestSyncTagsOnTheOnlyTwoTokenNamingCloseAlts(t *testing.T) {
	spec, err := EmitGrammarSpec(listGrammar(),
		&ConvertOptions{Tag: "demo", Start: "doc"})
	if err != nil {
		t.Fatalf("emit failed: %v", err)
	}

	closeAlt := func(rule string, idx int) *tabnas.GrammarAltSpec {
		t.Helper()
		rs := spec.Rule[rule]
		if rs == nil {
			t.Fatalf("no rule %s", rule)
		}
		alts := altListOf(rs.Close)
		if len(alts) <= idx {
			t.Fatalf("%s has %d close alternates, wanted at least %d",
				rule, len(alts), idx+1)
		}
		return alts[idx]
	}

	end := closeAlt("__start__", 0)
	if s, _ := end.S.(string); s != "#ZZ" {
		t.Fatalf("the start wrapper's close alt should name #ZZ, got %#v", end.S)
	}
	if end.G != "demo,end" {
		t.Errorf("__start__ close g = %q, want the tag with the end group", end.G)
	}

	sep := closeAlt("list", 0)
	if s, _ := sep.S.(string); !strings.HasPrefix(s, "#") {
		t.Fatalf("the separator close alt should name a token, got %#v", sep.S)
	}
	if sep.G != "demo,comma" {
		t.Errorf("separator close g = %q, want the tag with the comma group", sep.G)
	}

	// Every OTHER close alternate names no token, so tagging it would be
	// dead weight — and the tag it carries must stay the caller's own.
	for name, rs := range spec.Rule {
		if rs == nil {
			continue
		}
		for i, a := range altListOf(rs.Close) {
			if name == "__start__" || (name == "list" && i == 0) {
				continue
			}
			if s, _ := a.S.(string); s != "" {
				t.Errorf("%s close alt %d names token %q — a third sync "+
					"candidate the emitter is not accounting for", name, i, s)
			}
			if a.G != "demo" {
				t.Errorf("%s close alt %d g = %q, want the bare tag", name, i, a.G)
			}
		}
	}
}

// Strip the sync groups back off, and recovery must get measurably
// worse — otherwise the tags are decoration and this suite proves
// nothing about them. Mirrors the TS `sync tags` recovery test.
func TestSyncTagsKeepTheRestOfAListUnderATaggedHost(t *testing.T) {
	syncGroups := map[string]bool{"close": true, "comma": true, "end": true}

	closeAlts := func(rules map[string]any, rule string) []any {
		rm, _ := rules[rule].(map[string]any)
		cl, _ := rm["close"].([]any)
		return cl
	}

	// A host rule with its own sync tag, standing in for the grammar this
	// one gets embedded in. Its single tag is what disables the fallback
	// the generated rules would otherwise have relied on.
	underHost := func(data map[string]any) map[string]any {
		rules, _ := data["rule"].(map[string]any)
		rules["host"] = map[string]any{
			"open":  []any{map[string]any{"p": "doc", "g": "host"}},
			"close": []any{map[string]any{"s": "#ZZ", "a": "@bubble$", "g": "host,end"}},
		}
		opts, _ := data["options"].(map[string]any)
		rule, _ := opts["rule"].(map[string]any)
		rule["start"] = "host"
		return data
	}

	strip := func(data map[string]any) map[string]any {
		rules, _ := data["rule"].(map[string]any)
		for name := range rules {
			for _, av := range closeAlts(rules, name) {
				am, _ := av.(map[string]any)
				g, ok := am["g"].(string)
				if !ok {
					continue
				}
				kept := []string{}
				for _, tg := range strings.Split(g, ",") {
					if !syncGroups[tg] {
						kept = append(kept, tg)
					}
				}
				am["g"] = strings.Join(kept, ",")
			}
		}
		return data
	}

	// ToPureSpec clones, so each call yields an independent tree.
	pure := func() map[string]any {
		spec, err := EmitGrammarSpec(listGrammar(),
			&ConvertOptions{Start: "doc", Tag: "demo", Builtins: true})
		if err != nil {
			t.Fatalf("emit failed: %v", err)
		}
		data, err := ToPureSpec(spec)
		if err != nil {
			t.Fatalf("ToPureSpec failed: %v", err)
		}
		return data
	}

	// The whole point of a sync group is what the ENGINE does with it, so
	// the grammar goes through the serialised cross-runtime door and is
	// parsed for real.
	var srcOf func(v any) string
	srcOf = func(v any) string {
		switch x := v.(type) {
		case map[string]any:
			if s, ok := x["src"].(string); ok {
				return s
			}
			return srcOf(x["kids"])
		case []any:
			out := ""
			for _, e := range x {
				out += srcOf(e)
			}
			return out
		case string:
			return x
		}
		return ""
	}
	parse := func(what string, data map[string]any, src string) (int, string) {
		t.Helper()
		gs, err := tabnas.GrammarSpecFromJSON([]byte(ToJsonic(data, true, 0)))
		if err != nil {
			t.Fatalf("%s: loading the serialised grammar: %v", what, err)
		}
		j := tabnas.Make(tabnas.Options{Parse: &tabnas.ParseOptions{
			Recover: &tabnas.RecoverOptions{Enabled: true}}})
		if err := j.Grammar(gs); err != nil {
			t.Fatalf("%s: installing the grammar: %v", what, err)
		}
		value, errs, err := j.ParseRecover(src)
		if err != nil {
			t.Fatalf("%s: parse failed outright: %v", what, err)
		}
		return len(errs), srcOf(value)
	}

	taggedErrs, taggedSrc := parse("tagged", underHost(pure()), "1,!,3")
	bareErrs, bareSrc := parse("untagged", underHost(strip(pure())), "1,!,3")

	if taggedErrs != 1 || bareErrs != 1 {
		t.Fatalf("expected one recovered error each, got tagged=%d untagged=%d",
			taggedErrs, bareErrs)
	}
	// Tagged: resynchronises at the separator, so the trailing `3`
	// survives. Untagged: the host's tag has disabled the fallback, the
	// separator is no longer a sync point, and everything after the bad
	// token is lost.
	if !strings.Contains(taggedSrc, "3") {
		t.Errorf("tagged: the item after the error should survive, got %q", taggedSrc)
	}
	if strings.Contains(bareSrc, "3") {
		t.Errorf("untagged: without the separator sync point the tail is lost — "+
			"if this now passes, the tags are no longer doing anything (got %q)",
			bareSrc)
	}
}

// A grammar is free to contain a production actually called
// `__start__` — the IR reserves no names. The wrapper must then take a
// numbered name, as TypeScript does, or it silently overwrites the
// author's rule. With provenance on, the consequence is sharper still:
// recording the colliding name would claim an AUTHOR-WRITTEN rule was
// generated, breaking the map's one invariant.
func TestStartWrapperAvoidsAnAuthoredName(t *testing.T) {
	spec, err := EmitGrammarSpec(&Grammar{Productions: []*Production{
		{Name: "doc", Alts: []Sequence{{ref("__start__")}}},
		{Name: "__start__", Alts: []Sequence{{tok("#NR")}}},
	}}, &ConvertOptions{Tag: "demo", Start: "doc"})
	if err != nil {
		t.Fatalf("emit failed: %v", err)
	}
	if _, ok := spec.Rule["__start2__"]; !ok {
		t.Fatalf("wrapper did not take a numbered name; rules: %v", ruleNames(spec))
	}
	prov, _ := spec.Meta["provenance"].(map[string]any)
	if _, listed := prov["__start__"]; listed {
		t.Error("the author's __start__ rule is listed as generated")
	}
	if prov["__start2__"] != "doc" {
		t.Errorf("__start2__ provenance = %v, want doc", prov["__start2__"])
	}
}

// Meta is JSON-serialisable by contract, but a caller can hold ordinary
// typed Go containers in it. cloneData and ToJsonic recognise only the
// generic map[string]any / []any forms, so without normalisation those
// values serialise as `null` — the metadata silently replaced by
// nothing on the way out.
func TestCarriedMetaNormalisesTypedContainers(t *testing.T) {
	spec, err := EmitGrammarSpec(&Grammar{Productions: []*Production{
		{Name: "v", Alts: []Sequence{{tok("#NR")}}},
	}}, &ConvertOptions{Tag: "demo", Builtins: true})
	if err != nil {
		t.Fatalf("emit failed: %v", err)
	}
	spec.Meta = map[string]any{
		"pairs": map[string]string{"k": "v"},
		"tags":  []string{"a", "b"},
	}
	text := SpecToJSON(spec, 2)
	if strings.Contains(text, "null") {
		t.Errorf("typed containers serialised as null:\n%s", text)
	}
	for _, want := range []string{`"pairs"`, `"tags"`, `"a"`, `"b"`} {
		if !strings.Contains(text, want) {
			t.Errorf("serialised meta lost %s:\n%s", want, text)
		}
	}
}

func ruleNames(spec *tabnas.GrammarSpec) []string {
	out := []string{}
	for n := range spec.Rule {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ---- source spans --------------------------------------------------
//
// Source spans on the IR, and the compile errors that carry them.
//
// A front-end that records where each element came from gets compile
// errors with a range, so a tool can underline the offending text
// instead of parsing it back out of the message. A front-end that
// records nothing compiles to exactly the same grammar and gets the
// same messages — every assertion below has a no-span counterpart.
// Mirrors the TS `describe('source spans')`.

func at(s, e, r, c int) *SrcSpan { return &SrcSpan{S: s, E: e, R: r, C: c} }

// sameSpan compares by VALUE, not identity: a pass is free to copy the
// struct, and what matters is that the position is still the author's.
func sameSpan(got, want *SrcSpan) bool {
	if got == nil || want == nil {
		return got == want
	}
	return *got == *want
}

func spannedRef(name string, sp *SrcSpan) *Element {
	return &Element{Kind: KindRef, Name: name, Sp: sp}
}

// TestUnknownRuleRefCarriesTheElementSpan: the most common author-facing
// compile error, and the one an editor most wants to underline.
func TestUnknownRuleRefCarriesTheElementSpan(t *testing.T) {
	sp := at(10, 15, 2, 7)
	_, err := EmitGrammarSpec(&Grammar{Productions: []*Production{
		{Name: "doc", Alts: []Sequence{{spannedRef("nope", sp)}}},
	}}, &ConvertOptions{Tag: "demo"})
	if err == nil {
		t.Fatal("expected an unknown-rule failure")
	}
	var ee *EmitError
	if !errors.As(err, &ee) {
		t.Fatalf("error is %T (%v), want *EmitError", err, err)
	}
	if !strings.Contains(ee.Error(), "references unknown rule 'nope'") {
		t.Errorf("message = %q, want it to name the unknown rule", ee.Error())
	}
	if ee.Rule != "doc" {
		t.Errorf("Rule = %q, want %q", ee.Rule, "doc")
	}
	if !sameSpan(ee.Sp, sp) {
		t.Errorf("Sp = %+v, want %+v", ee.Sp, sp)
	}
}

// TestUnknownRuleRefWithoutSpansStillFailsIdentically is the
// backward-compatibility guarantee: a front-end that records nothing
// gets the message it always got, and no span invented for it.
func TestUnknownRuleRefWithoutSpansStillFailsIdentically(t *testing.T) {
	_, err := EmitGrammarSpec(&Grammar{Productions: []*Production{
		{Name: "doc", Alts: []Sequence{{ref("nope")}}},
	}}, &ConvertOptions{Tag: "demo"})
	if err == nil {
		t.Fatal("expected an unknown-rule failure")
	}
	var ee *EmitError
	if !errors.As(err, &ee) {
		t.Fatalf("error is %T (%v), want *EmitError", err, err)
	}
	if !strings.Contains(ee.Error(), "references unknown rule 'nope'") {
		t.Errorf("message = %q, want it to name the unknown rule", ee.Error())
	}
	if ee.Sp != nil {
		t.Errorf("Sp = %+v, want nil — no span recorded, so none reported", ee.Sp)
	}
}

// TestPurelyLeftRecursiveCarriesTheProductionSpan.
//
// NOTE the shape: this failure PANICS in Go where TypeScript throws (see
// TestDiagnosticsNameTheNotation, and abnf/go's suite, which pins it).
// The panic value is a *EmitError rather than a string precisely so the
// span survives — a recovering caller that stringifies still sees the
// message it always saw.
func TestPurelyLeftRecursiveCarriesTheProductionSpan(t *testing.T) {
	sp := at(0, 12, 1, 1)
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected a left-recursion failure")
		}
		ee, ok := r.(*EmitError)
		if !ok {
			t.Fatalf("panic value is %T (%v), want *EmitError", r, r)
		}
		if !strings.Contains(ee.Error(), "purely left-recursive") {
			t.Errorf("message = %q, want it to mention 'purely left-recursive'", ee.Error())
		}
		if ee.Rule != "loop" {
			t.Errorf("Rule = %q, want %q", ee.Rule, "loop")
		}
		if !sameSpan(ee.Sp, sp) {
			t.Errorf("Sp = %+v, want %+v", ee.Sp, sp)
		}
	}()
	// Every alternative re-enters the rule and consumes something, so
	// there is no seed to start from. (A bare `loop = loop` is a trivial
	// self-reference and is dropped instead.)
	_, _ = EmitGrammarSpec(&Grammar{Productions: []*Production{
		{Name: "loop", Sp: sp, Alts: []Sequence{
			{ref("loop"), term("x")},
			{ref("loop"), term("y")},
		}},
	}}, &ConvertOptions{Tag: "demo"})
}

// TestElementSpansSurviveToTheEmitter: elements are shared by reference
// through cloning and Paull's substitution, so a span recorded at parse
// time survives to the emitter. If that ever stops being true, the
// unknown-rule span above is the first casualty — this pins the
// mechanism rather than one symptom of it.
func TestElementSpansSurviveToTheEmitter(t *testing.T) {
	sp := at(20, 24, 3, 1)
	el := spannedRef("gone", sp)
	g := &Grammar{Productions: []*Production{
		{Name: "doc", Alts: []Sequence{{ref("mid")}}},
		{Name: "mid", Alts: []Sequence{{el, term("x")}}},
	}}
	_, err := EmitGrammarSpec(g, &ConvertOptions{Start: "doc", Tag: "demo"})
	if err == nil {
		t.Fatal("expected an unknown-rule failure")
	}
	var ee *EmitError
	if !errors.As(err, &ee) {
		t.Fatalf("error is %T (%v), want *EmitError", err, err)
	}
	if !sameSpan(ee.Sp, sp) {
		t.Errorf("Sp = %+v, want %+v — the span did not survive the rewrite passes",
			ee.Sp, sp)
	}
	// ...and the caller's own element is untouched.
	if !sameSpan(el.Sp, sp) {
		t.Errorf("caller's element Sp = %+v, want %+v", el.Sp, sp)
	}
}

// TestProductionSpansSurviveTheRewritePasses is the one that catches a
// dropped rebuild site.
//
// A production is REBUILT field by field by several passes, so a new
// field is silently lost unless every one of them carries it across —
// exactly the trap `Origin` fell into. The grammar below is shaped so
// that each rebuilding pass has a spanned production to mangle:
//
//	doc   probe dispatch      rewriteProbeDispatches' rebuild
//	expr  direct left rec.    eliminateDirectLeftRec's star return
//	lead  leading ref         substituteLeadingRef
//	triv  trivial self-ref    eliminateDirectLeftRec's seeds-only return
//	all   —                   eliminateLeftRecursion's working copy, desugar
//
// Every one of them must come out the far end still knowing where the
// author wrote it.
func TestProductionSpansSurviveTheRewritePasses(t *testing.T) {
	want := map[string]*SrcSpan{
		"doc":  at(0, 24, 1, 1),
		"x":    at(24, 40, 2, 1),
		"y":    at(40, 56, 3, 1),
		"expr": at(56, 80, 4, 1),
		"lead": at(80, 96, 5, 1),
		"triv": at(96, 112, 6, 1),
	}
	optGroup := optOf(&Element{Kind: KindGroup, Alts: []Sequence{
		{ref("x"), term("@")},
	}})
	g := &Grammar{Productions: []*Production{
		// The optional prefix shares vocabulary with the tail, so the
		// probe rewriter rebuilds `doc`.
		{Name: "doc", Sp: want["doc"], Alts: []Sequence{{optGroup, ref("y")}}},
		{Name: "x", Sp: want["x"], Alts: []Sequence{{term("a")}, {term("b")}}},
		{Name: "y", Sp: want["y"], Alts: []Sequence{{term("a")}, {term("c")}}},
		{Name: "expr", Sp: want["expr"], Alts: []Sequence{
			{ref("expr"), term("+"), ref("x")},
			{ref("x")},
		}},
		// A leading reference Paull's substitution inlines.
		{Name: "lead", Sp: want["lead"], Alts: []Sequence{{ref("expr"), term("!")}}},
		// A trivial self-reference, dropped rather than eliminated.
		{Name: "triv", Sp: want["triv"], Alts: []Sequence{{ref("triv")}, {term("z")}}},
	}}

	// The rewrite pipeline emitGrammarSpec runs, in its order.
	const start = "doc"
	out := cloneGrammar(g)
	if err := resolveProseTerminals(out); err != nil {
		t.Fatalf("resolveProseTerminals: %v", err)
	}
	liftLiteralTokens(out, start)
	normalizeBuiltinTokens(out)
	out = eliminateLeftRecursion(out)
	out = rewriteProbeDispatches(out)
	out = leftFactor(out)
	out = rewriteTailRepeats(out, start)
	out = desugar(out)

	got := map[string]*Production{}
	for _, p := range out.Productions {
		got[p.Name] = p
	}
	for name, sp := range want {
		p := got[name]
		if p == nil {
			t.Errorf("rule %q did not survive the rewrite passes", name)
			continue
		}
		if !sameSpan(p.Sp, sp) {
			t.Errorf("rule %q: Sp = %+v, want %+v — a rebuild site dropped it",
				name, p.Sp, sp)
		}
	}
	// The probe rewriter must actually have fired, or `doc` proves
	// nothing about its rebuild site.
	probed := false
	for _, p := range out.Productions {
		if p.ProbeDisp != nil {
			probed = true
		}
	}
	if !probed {
		t.Error("no probe dispatcher was synthesised; the grammar no longer " +
			"exercises rewriteProbeDispatches' rebuild")
	}
	// Synthesised productions locate themselves by Origin, not by a span
	// the author never wrote.
	for _, p := range out.Productions {
		if p.Origin != "" && p.Sp != nil {
			t.Errorf("synthesised rule %q carries a span (%+v) the author did not write",
				p.Name, p.Sp)
		}
	}
	// The caller's own IR is untouched.
	for _, p := range g.Productions {
		if !sameSpan(p.Sp, want[p.Name]) {
			t.Errorf("caller's rule %q: Sp = %+v, want %+v", p.Name, p.Sp, want[p.Name])
		}
	}
}

// emitFailure runs the emitter and returns its failure however it is
// raised — as an error return, or as the panic value the front-ends'
// emitSafely recovers.
func emitFailure(t *testing.T, g *Grammar) (failure error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			e, ok := r.(error)
			if !ok {
				panic(r)
			}
			failure = e
		}
	}()
	_, err := EmitGrammarSpec(g, &ConvertOptions{Tag: "demo"})
	if err == nil {
		t.Fatal("expected a compile failure")
	}
	return err
}

// TestConvertedFailureMessagesAreByteIdentical: EmitError changes the
// failure's TYPE at five sites. The message is the part callers have
// historically matched on — including this repo's own suite and the
// front-ends, which restamp the prefix and pass the rest through — so it
// must not move. Compared against the published compiler's exact
// strings. Mirrors the TS
// "keeps every converted failure message byte-identical".
func TestConvertedFailureMessagesAreByteIdentical(t *testing.T) {
	prose := func(text string) *Element {
		return &Element{Kind: KindProse, Text: text}
	}
	cases := []struct {
		name    string
		grammar *Grammar
		message string
	}{
		{"unknown rule reference", &Grammar{Productions: []*Production{
			{Name: "doc", Alts: []Sequence{{ref("nope")}}}}},
			"demo: rule 'doc' references unknown rule 'nope'"},
		{"purely left-recursive", &Grammar{Productions: []*Production{
			{Name: "loop", Alts: []Sequence{
				{ref("loop"), term("x")},
				{ref("loop"), term("y")}}}}},
			"demo: rule 'loop' is purely left-recursive " +
				"(no seed alternative); cannot eliminate"},
		{"prose inside an expression", &Grammar{Productions: []*Production{
			{Name: "doc", Alts: []Sequence{{term("a"), prose("stuff")}}}}},
			"demo: rule 'doc' uses prose ('<stuff>') inside an expression; " +
				"prose may only stand alone as the whole definition of a " +
				"built-in lexer token."},
		{"prose as a whole definition", &Grammar{Productions: []*Production{
			{Name: "doc", Alts: []Sequence{{prose("stuff")}}}}},
			"demo: rule 'doc' is defined only by prose ('<stuff>'), which " +
				"describes a terminal but does not define one. Prose is allowed " +
				"only for built-in lexer tokens (TX, NR, ST, VL)."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := emitFailure(t, c.grammar)
			var ee *EmitError
			if !errors.As(err, &ee) {
				t.Fatalf("failure is %T (%v), want *EmitError", err, err)
			}
			if ee.Error() != c.message {
				t.Errorf("message drifted:\n got %q\nwant %q", ee.Error(), c.message)
			}
		})
	}
}

// TestSpansDoNotChangeTheEmittedGrammar: spans are metadata for
// diagnostics and must be invisible in the output.
func TestSpansDoNotChangeTheEmittedGrammar(t *testing.T) {
	strip := func(g *Grammar) string {
		t.Helper()
		spec, err := EmitGrammarSpec(g, &ConvertOptions{Tag: "demo", Builtins: true})
		if err != nil {
			t.Fatalf("emit failed: %v", err)
		}
		pure, err := ToPureSpec(spec)
		if err != nil {
			t.Fatalf("ToPureSpec failed: %v", err)
		}
		return ToJsonic(pure, true, 0)
	}
	withSpans := &Grammar{Productions: []*Production{{
		Name: "doc",
		Alts: []Sequence{{&Element{
			Kind: KindTerm, Literal: "a", Sp: at(0, 3, 1, 1)}}},
		Sp: at(0, 9, 1, 1),
	}}}
	without := &Grammar{Productions: []*Production{
		{Name: "doc", Alts: []Sequence{{term("a")}}},
	}}
	if a, b := strip(withSpans), strip(without); a != b {
		t.Errorf("spans must be invisible in the emitted grammar\n with spans: %s\n without:    %s",
			a, b)
	}
}

// TestCloneGrammarKeepsEveryField pins the whole-struct copy.
//
// cloneGrammar rebuilt the Grammar from Productions alone, so Remove,
// ClearAll and Ambiguities were dropped on every clone — and emitGrammarSpec
// reads Remove/ClearAll OFF THE CLONE, so a front-end that set them directly
// on the IR had its removals silently ignored. TypeScript's cloneGrammar
// spreads the grammar and documents why; this is the same contract.
//
// Written field-by-field rather than with reflect.DeepEqual on purpose: a new
// Grammar field added later should make a reviewer decide whether it must be
// carried, and DeepEqual on a copied struct would pass silently either way.
func TestCloneGrammarKeepsEveryField(t *testing.T) {
	g := &Grammar{
		Productions: []*Production{{Name: "top", Alts: []Sequence{{}}}},
		Remove:      []string{"gone"},
		ClearAll:    true,
		Ambiguities: []AmbiguityReport{{Rule: "top", Reason: "test"}},
	}
	c := cloneGrammar(g)

	if 1 != len(c.Remove) || "gone" != c.Remove[0] {
		t.Errorf("Remove: got %v, want [gone]", c.Remove)
	}
	if !c.ClearAll {
		t.Error("ClearAll: got false, want true")
	}
	if 1 != len(c.Ambiguities) {
		t.Errorf("Ambiguities: got %d, want 1", len(c.Ambiguities))
	}

	// Still a CLONE: mutating the copy's productions must not reach the
	// original, which is the reason cloneGrammar exists at all.
	c.Productions[0].Name = "changed"
	if "top" != g.Productions[0].Name {
		t.Error("clone shares production storage with the original")
	}
}

// TestCloneGrammarDoesNotAliasCallerSlices pins the isolation the whole-struct
// copy could otherwise break. A struct copy duplicates slice HEADERS, so an
// append on the clone writes into the caller's backing array whenever the
// original has spare capacity — and resolveProseTerminals appends to Remove,
// on the clone. Found in review of the whole-struct copy, not afterwards.
func TestCloneGrammarDoesNotAliasCallerSlices(t *testing.T) {
	rm := make([]string, 1, 4) // spare capacity is the whole point
	rm[0] = "first"
	g := &Grammar{
		Productions: []*Production{{Name: "top", Alts: []Sequence{{}}}},
		Remove:      rm,
		Ambiguities: make([]AmbiguityReport, 1, 4),
	}
	c := cloneGrammar(g)
	c.Remove = append(c.Remove, "added-on-clone")
	c.Ambiguities = append(c.Ambiguities, AmbiguityReport{Rule: "added"})

	if got := rm[:2][1]; "" != got {
		t.Errorf("clone's append reached caller storage: %q", got)
	}
	if 1 != len(g.Remove) {
		t.Errorf("caller Remove length changed: %d", len(g.Remove))
	}
}

// TestEmitRemovalOnlyGrammarErrors pins the shape that preserving Remove made
// reachable. With Remove dropped on the clone, resolveProseTerminals rejected
// a production-less grammar as ruleless before anything indexed Productions[0].
// Preserving it let that grammar through to the start-rule selection, where an
// empty slice panicked. An error is the contract; a panic is not.
func TestEmitRemovalOnlyGrammarErrors(t *testing.T) {
	_, err := EmitGrammarSpec(&Grammar{Remove: []string{"gone"}}, nil)
	if nil == err {
		t.Fatal("removal-only grammar: got nil error, want a controlled error")
	}
	if !strings.Contains(err.Error(), "no productions") {
		t.Errorf("removal-only grammar: got %q, want it to name the cause", err)
	}
}

// TestMarkListingSkipsARemovedRule is the Go half of the TS
// "markListing skips a removed rule instead of crashing on it".
//
// Audit item B8, and the two ports reached it from opposite sides.
// `spec.Rule["gone"] = nil` is how a spec says "gone is removed"
// (Grammar.Remove). TS dereferenced the null and threw an uncaught TypeError;
// here cloneGrammar dropped the Remove field entirely, so the entry never
// existed, MarkListing walked a grammar with no removal in it and returned ""
// — no crash, and no listing either.
//
// Preserving the field is what makes the nil REACHABLE, which is why this
// test lives on the same branch as that fix: without the guard, MarkListing
// panics with a nil pointer dereference the moment removals survive.
//
// It pins the listing CONTENT, not just the absence of a panic. Asserting
// only "did not crash" would pass on the old behaviour, where the removal was
// gone before MarkListing ever saw it.
func TestMarkListingSkipsARemovedRule(t *testing.T) {
	spec, err := EmitGrammarSpec(&Grammar{
		Productions: []*Production{
			{Name: "top", Alts: []Sequence{{term("x")}, {term("y")}}},
			{Name: "gone", Alts: []Sequence{{term("z")}}},
		},
		Remove: []string{"gone"},
	}, &ConvertOptions{Tag: "demo", Marks: true})
	if nil != err {
		t.Fatalf("emit: %v", err)
	}

	// The removal survived as the nil marker — without this the test would
	// pass on a spec that simply has no removal in it.
	entry, present := spec.Rule["gone"]
	if !present {
		t.Fatal("the removal entry is missing entirely")
	}
	if nil != entry {
		t.Fatalf("removal entry = %v, want nil", entry)
	}

	listing := MarkListing(spec)
	if strings.Contains(listing, "gone") {
		t.Errorf("a removed rule must not be listed:\n%s", listing)
	}
	if !strings.Contains(listing, "top") {
		t.Errorf("the surviving rule's marks are missing:\n%s", listing)
	}
}
