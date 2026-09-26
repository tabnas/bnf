package bnf

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	tabnas "github.com/tabnas/parser/go"
)

// The limits of the overlapping-class partition, driven through the IR
// rather than a notation. Mirrors ts/test/class-partition.test.js.
//
// These cases exist because the first cut of the partition got them
// wrong, and no front-end in the fleet can express them: ABNF's `%x`
// ranges are always single-code-point and case-sensitive, so its suite —
// the usual oracle for this compiler — cannot reach a multi-character
// regex terminal or a case-insensitive class at all. A notation-neutral
// compiler has to be safe for the front-ends that can.

func rxEl(pattern, flags string) *Element {
	return &Element{Kind: KindRegex, Pattern: pattern, Flags: flags}
}

func termEl(literal string) *Element {
	return &Element{Kind: KindTerm, Literal: literal}
}

func emitIR(t *testing.T, prods []*Production, opts *ConvertOptions) *tabnas.GrammarSpec {
	t.Helper()
	if opts == nil {
		opts = &ConvertOptions{Tag: "t"}
	}
	spec, err := EmitGrammarSpec(&Grammar{Productions: prods}, opts)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	return spec
}

func matchSources(spec *tabnas.GrammarSpec) map[string]string {
	out := map[string]string{}
	if spec.Options == nil || spec.Options.Match == nil {
		return out
	}
	for n, re := range spec.Options.Match.Token {
		out[n] = re.String()
	}
	return out
}

func altSeqs(t *testing.T, spec *tabnas.GrammarSpec, rule string) []any {
	t.Helper()
	rs := spec.Rule[rule]
	if rs == nil {
		t.Fatalf("no %s rule", rule)
	}
	open, ok := rs.Open.([]*tabnas.GrammarAltSpec)
	if !ok {
		t.Fatalf("%s open is %T", rule, rs.Open)
	}
	var out []any
	for _, a := range open {
		out = append(out, a.S)
	}
	return out
}

func altMarks(t *testing.T, spec *tabnas.GrammarSpec, rule string) []string {
	t.Helper()
	rs := spec.Rule[rule]
	if rs == nil {
		t.Fatalf("no %s rule", rule)
	}
	open, ok := rs.Open.([]*tabnas.GrammarAltSpec)
	if !ok {
		t.Fatalf("%s open is %T", rule, rs.Open)
	}
	// The Go emitter stashes a mark in U under `m$` (see
	// emit_support.go: mapToAlt), where markListing and attachActions
	// look for it; there is no dedicated field.
	var out []string
	for _, a := range open {
		mark := ""
		if a.U != nil {
			if v, ok := a.U["m$"].(string); ok {
				mark = v
			}
		}
		out = append(out, mark)
	}
	return out
}

// patternCharRanges answers "what can this pattern's FIRST character
// be?" — right for contest detection, wrong for deciding a class's full
// coverage. Partitioning REPLACES the matcher with one-character atoms,
// so a pattern that matches more than one code point would lose the rest
// of itself. Each of these three lost something real.
func TestPartitionLeavesMultiCharRegexAlone(t *testing.T) {
	for _, c := range []struct{ label, pattern string }{
		{"top-level alternation", "a|bc"},
		{"quantifier", "[a-z]+"},
		{"two classes in sequence", "[aA][bB]"},
	} {
		// `[a]` overlaps the first character of every pattern above, so
		// without the guard each one becomes "contested" and is replaced.
		spec := emitIR(t, []*Production{
			{Name: "top", Alts: []Sequence{{rxEl(c.pattern, "")}, {rxEl("[a]", "")}}},
		}, nil)

		found := false
		for _, src := range matchSources(spec) {
			if strings.Contains(src, c.pattern) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: %q lost its matcher; emitted %v",
				c.label, c.pattern, matchSources(spec))
		}
		if spec.Options.TokenSet != nil && len(spec.Options.TokenSet) != 0 {
			t.Errorf("%s: a pattern that can match more than one code point "+
				"must not be partitioned; got sets %v", c.label, spec.Options.TokenSet)
		}
	}
}

// foldCaseRanges folds ASCII A-Z/a-z and nothing else, so atoms derived
// from `[é]/i` would cover `é` but not `É` — the matcher would say one
// thing and the ranges another.
func TestPartitionLeavesCaseInsensitiveClassAlone(t *testing.T) {
	spec := emitIR(t, []*Production{
		{Name: "top", Alts: []Sequence{
			{rxEl(`[\x{00e9}]`, "i")},
			{rxEl(`[\x{00e9}]`, "")},
		}},
	}, nil)

	var insensitive []string
	for n, src := range matchSources(spec) {
		if strings.HasPrefix(src, "(?i)") {
			insensitive = append(insensitive, n)
		}
	}
	if len(insensitive) != 1 {
		t.Fatalf("the case-insensitive matcher must survive; got %v of %v",
			insensitive, matchSources(spec))
	}
	re := spec.Options.Match.Token[insensitive[0]]
	if !re.MatchString("É") {
		t.Errorf("%s must still cover É, got %s", insensitive[0], re)
	}
	if spec.Options.TokenSet != nil && len(spec.Options.TokenSet) != 0 {
		t.Errorf("a case-insensitive class must not be partitioned; got %v",
			spec.Options.TokenSet)
	}
}

// The guard above must not have disarmed the fix itself.
func TestPartitionStillPartitionsPlainClasses(t *testing.T) {
	spec := emitIR(t, []*Production{
		{Name: "top", Alts: []Sequence{
			{rxEl(`[\x{0030}-\x{0039}]`, "")},
			{rxEl(`[\x{0031}-\x{0039}]`, "")},
		}},
	}, nil)
	var names []string
	for n := range spec.Options.TokenSet {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) != 2 {
		t.Fatalf("expected a set per overlapping class, got %v", names)
	}
}

// Marks come from altDiscriminator, which reads the token name out of
// regexTokens. Pointing a one-atom class at the atom renamed it, so a
// user action bound to `@top:o:<mark>` silently detached the moment some
// OTHER production mentioned an overlapping class.
func TestPartitionKeepsClassTokenNames(t *testing.T) {
	alts := []Sequence{{rxEl("[123456789]", ""), termEl("x")}, {termEl("y")}}
	opts := &ConvertOptions{Tag: "t", Marks: true}

	alone := emitIR(t, []*Production{{Name: "top", Alts: alts}}, opts)
	contested := emitIR(t, []*Production{
		{Name: "top", Alts: alts},
		{Name: "other", Alts: []Sequence{{rxEl("[0-9]", "")}}},
	}, opts)

	if got, want := altMarks(t, contested, "top"), altMarks(t, alone, "top"); !reflect.DeepEqual(got, want) {
		t.Errorf("an unrelated overlapping class moved this rule's marks: %v, want %v",
			got, want)
	}
	if got, want := altSeqs(t, contested, "top"), altSeqs(t, alone, "top"); !reflect.DeepEqual(got, want) {
		t.Errorf("token sequences moved: %v, want %v", got, want)
	}
}

// An atom minted as `rx_<pattern>` collides with the natural name of any
// class spelling the same span: `%x31-39`'s atom took that name first
// and pushed the class to a suffixed one.
func TestPartitionNamesAtomsApartFromClasses(t *testing.T) {
	spec := emitIR(t, []*Production{
		{Name: "top", Alts: []Sequence{
			{rxEl(`[\x{0030}-\x{0039}]`, "")},
			{rxEl(`[\x{0031}-\x{0039}]`, "")},
		}},
	}, nil)
	for n := range matchSources(spec) {
		if !strings.HasPrefix(n, "#RXA") {
			t.Errorf("%s should be an atom token, not a class token", n)
		}
	}
	for n := range spec.Options.TokenSet {
		if strings.HasPrefix(n, "RXA") {
			t.Errorf("set %s took an atom's name", n)
		}
	}
}

// A class without u or v whose code-point reading reaches past U+FFFF is
// left out of the partition, as the TypeScript emitter leaves it:
// JavaScript's matcher reads it in UTF-16 code units, and atoms compiled
// with u would take an astral character whole. RE2 reads code points
// whatever the flags say, so here the rule keeps the emitted grammar the
// same as TypeScript's, and the class keeps its own matcher. Under u it is
// still partitioned (tabnas/bnf#75 review).
func TestPartitionLeavesOutACodeUnitClassPastTheBmp(t *testing.T) {
	grammar := func(pattern, flags string) *tabnas.GrammarSpec {
		return emitIR(t, []*Production{
			{Name: "doc", Alts: []Sequence{
				{rxEl(pattern, flags), rxEl(pattern, flags), termEl(";")},
				{rxEl("[b-c]", ""), termEl("!")},
			}},
		}, &ConvertOptions{Tag: "cp", Start: "doc"})
	}
	for _, pattern := range []string{`[^a]`, `[\s\S]`, "[b\U0001F600]"} {
		if sets := grammar(pattern, "").Options.TokenSet; len(sets) != 0 {
			t.Errorf("%q: a class read in code units past U+FFFF must keep its "+
				"own matcher; got sets %v", pattern, sets)
		}
	}
	if sets := grammar(`[^a]`, "u").Options.TokenSet; len(sets) != 2 {
		t.Errorf("[^a] under u: want the class and [b-c] laid over the partition, got sets %v", sets)
	}
}
