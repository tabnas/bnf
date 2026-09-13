// Copyright (c) 2026 tabnas, MIT License

// The code in go/doc/*.md, run. A documented example that no longer
// compiles is a defect in the documentation, and the prose gate cannot
// see it: Vale and ts/test/docs.test.js both strip fenced blocks before
// they look.
package bnf_test

import (
	"errors"
	"strings"
	"testing"

	bnf "github.com/tabnas/bnf/go"
	tabnas "github.com/tabnas/parser/go"
)

// tutorial.md, steps 2 to 4.
func TestDocTutorialFirstCompile(t *testing.T) {
	grammar := &bnf.Grammar{Productions: []*bnf.Production{
		{Name: "val", Alts: []bnf.Sequence{
			{{Kind: bnf.KindToken, Name: "#NR"}},
		}},
	}}
	spec, err := bnf.EmitGrammarSpec(grammar, &bnf.ConvertOptions{
		Start: "val",
		Tag:   "demo",
	})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	j := tabnas.Make()
	if err := j.Grammar(spec); err != nil {
		t.Fatalf("grammar: %v", err)
	}
	out, err := j.Parse("42")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("want map[string]any, got %T", out)
	}
	if "val" != m["rule"] || "42" != m["src"] {
		t.Errorf("want rule val src 42, got %v %v", m["rule"], m["src"])
	}
	if kids, _ := m["kids"].([]any); 0 != len(kids) {
		t.Errorf("want no kids, got %d", len(kids))
	}
}

// tutorial.md, step 5: the rule count and the parse the page states.
func TestDocTutorialNesting(t *testing.T) {
	grammar := &bnf.Grammar{Productions: []*bnf.Production{
		{Name: "list", Alts: []bnf.Sequence{{
			{Kind: bnf.KindTerm, Literal: "("},
			{Kind: bnf.KindStar, Inner: &bnf.Element{
				Kind: bnf.KindRef, Name: "item"}},
			{Kind: bnf.KindTerm, Literal: ")"},
		}}},
		{Name: "item", Alts: []bnf.Sequence{
			{{Kind: bnf.KindToken, Name: "#NR"}},
			{{Kind: bnf.KindRef, Name: "list"}},
		}},
	}}
	spec, err := bnf.EmitGrammarSpec(grammar, &bnf.ConvertOptions{
		Start: "list", Tag: "demo",
	})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if 13 != len(spec.Rule) {
		t.Errorf("tutorial says two productions give thirteen rules, got %d",
			len(spec.Rule))
	}
	j := tabnas.Make()
	if err := j.Grammar(spec); err != nil {
		t.Fatalf("grammar: %v", err)
	}
	out, err := j.Parse("(1 2 (3))")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	m := out.(map[string]any)
	if "list" != m["rule"] || "(12(3))" != m["src"] {
		t.Errorf("want list / (12(3)), got %v / %v", m["rule"], m["src"])
	}
	if kids, _ := m["kids"].([]any); 3 != len(kids) {
		t.Errorf("want three item kids, got %d", len(kids))
	}
}

// tutorial.md step 6 and guide.md: the message, the type, the prefix.
func TestDocUnknownRuleError(t *testing.T) {
	_, err := bnf.EmitGrammarSpec(&bnf.Grammar{Productions: []*bnf.Production{
		{Name: "a", Alts: []bnf.Sequence{
			{{Kind: bnf.KindRef, Name: "missing"}},
		}},
	}}, &bnf.ConvertOptions{Tag: "demo"})
	want := "demo: rule 'a' references unknown rule 'missing'"
	if nil == err || want != err.Error() {
		t.Fatalf("want %q, got %v", want, err)
	}
	var ee *bnf.EmitError
	if !errors.As(err, &ee) {
		t.Fatalf("want *EmitError, got %T", err)
	}
	if "a" != ee.Rule {
		t.Errorf("want rule a, got %q", ee.Rule)
	}
}

// reference.md and concepts.md: a purely left-recursive rule RETURNS.
func TestDocPurelyLeftRecursiveReturns(t *testing.T) {
	defer func() {
		if r := recover(); nil != r {
			t.Fatalf("the pages say this returns; it panicked with %v", r)
		}
	}()
	_, err := bnf.EmitGrammarSpec(&bnf.Grammar{Productions: []*bnf.Production{
		{Name: "a", Alts: []bnf.Sequence{
			{{Kind: bnf.KindRef, Name: "a"}, {Kind: bnf.KindTerm, Literal: "y"}},
		}},
	}}, &bnf.ConvertOptions{Tag: "demo", Start: "a"})
	var ee *bnf.EmitError
	if !errors.As(err, &ee) {
		t.Fatalf("want *EmitError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "purely left-recursive") {
		t.Errorf("unexpected message: %v", err)
	}
}

// guide.md: marks, the listing format, and binding an action to one.
func TestDocMarksAndActions(t *testing.T) {
	spec, err := bnf.EmitGrammarSpec(&bnf.Grammar{Productions: []*bnf.Production{
		{Name: "op", Alts: []bnf.Sequence{
			{{Kind: bnf.KindTerm, Literal: "inc"}},
			{{Kind: bnf.KindTerm, Literal: "dec"}},
		}},
	}}, &bnf.ConvertOptions{Tag: "demo", Start: "op", Marks: true})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	want := "op  o:INC  s:#INC\nop  o:DEC  s:#DEC"
	if want != bnf.MarkListing(spec) {
		t.Errorf("want\n%s\ngot\n%s", want, bnf.MarkListing(spec))
	}

	ran := 0
	if err := bnf.AttachActions(spec, bnf.ActionsMap{
		"@op:o:INC": {func(r *tabnas.Rule, ctx *tabnas.Context) { ran++ }},
	}); nil != err {
		t.Fatalf("attach: %v", err)
	}
	j := tabnas.Make()
	if err := j.Grammar(spec); err != nil {
		t.Fatalf("grammar: %v", err)
	}
	if _, err := j.Parse("inc"); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if 1 != ran {
		t.Errorf("want the action to run once, ran %d times", ran)
	}
}

// guide.md: a slot is a declaration, and a spec carrying one the
// installer has not filled in is refused by the engine.
func TestDocActionSlots(t *testing.T) {
	spec, err := bnf.EmitGrammarSpec(&bnf.Grammar{Productions: []*bnf.Production{
		{Name: "op", Alts: []bnf.Sequence{
			{{Kind: bnf.KindTerm, Literal: "dec"}},
		}},
	}}, &bnf.ConvertOptions{Tag: "demo", Start: "op", Marks: true})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if err := bnf.AttachActionSlots(spec, []string{"@op:o:DEC"}); nil != err {
		t.Fatalf("slots: %v", err)
	}
	if err := bnf.AttachActionSlots(spec, []string{"@op:bo"}); nil == err {
		t.Error("the guide says a rule-phase ref is refused as a slot")
	}
	if err := tabnas.Make().Grammar(spec); nil == err ||
		!strings.Contains(err.Error(), "unknown action function reference") {
		t.Errorf("the guide says an unfilled slot is refused at install: %v", err)
	}
}


// guide.md: an unmatched action ref is an error, and names the mark.
func TestDocUnmatchedActionRef(t *testing.T) {
	spec, _ := bnf.EmitGrammarSpec(&bnf.Grammar{Productions: []*bnf.Production{
		{Name: "op", Alts: []bnf.Sequence{
			{{Kind: bnf.KindTerm, Literal: "inc"}},
		}},
	}}, &bnf.ConvertOptions{Tag: "demo", Start: "op", Marks: true})
	err := bnf.AttachActions(spec, bnf.ActionsMap{
		"@op:o:inc": {func(r *tabnas.Rule, ctx *tabnas.Context) {}},
	})
	want := "demo: action ref '@op:o:inc' matches no open alt with mark 'inc' in rule 'op'"
	if nil == err || want != err.Error() {
		t.Fatalf("want %q, got %v", want, err)
	}
}

// reference.md: a duplicate mark within a rule is suffixed.
func TestDocDuplicateMark(t *testing.T) {
	spec, err := bnf.EmitGrammarSpec(&bnf.Grammar{Productions: []*bnf.Production{
		{Name: "op", Alts: []bnf.Sequence{
			{{Kind: bnf.KindToken, Name: "#NR"}},
			{{Kind: bnf.KindToken, Name: "#NR"}, {Kind: bnf.KindTerm, Literal: "!"}},
		}},
	}}, &bnf.ConvertOptions{Tag: "demo", Start: "op", Marks: true})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if !strings.Contains(bnf.MarkListing(spec), "o:NR~2") {
		t.Errorf("want a ~2 suffix, got %q", bnf.MarkListing(spec))
	}
}

// guide.md: pure data needs Builtins, and says so when it does not have
// it. Recognition does not, for an ordinary grammar.
func TestDocReductions(t *testing.T) {
	prod := []*bnf.Production{
		{Name: "top", Alts: []bnf.Sequence{{{Kind: bnf.KindToken, Name: "#NR"}}}},
	}
	withBuiltins, err := bnf.EmitGrammarSpec(&bnf.Grammar{Productions: prod},
		&bnf.ConvertOptions{Tag: "demo", Start: "top", Builtins: true})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if _, err := bnf.ToPureSpec(withBuiltins); err != nil {
		t.Errorf("pure: %v", err)
	}
	if _, err := bnf.ToRecognitionSpec(withBuiltins); err != nil {
		t.Errorf("recognition: %v", err)
	}

	plain, err := bnf.EmitGrammarSpec(&bnf.Grammar{Productions: prod},
		&bnf.ConvertOptions{Tag: "demo", Start: "top"})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	_, perr := bnf.ToPureSpec(plain)
	if nil == perr || !strings.Contains(perr.Error(), "builtins: true") {
		t.Errorf("want the builtins message, got %v", perr)
	}
	if _, rerr := bnf.ToRecognitionSpec(plain); rerr != nil {
		t.Errorf("the guide says recognition does not need builtins: %v", rerr)
	}
}

// guide.md: the word-boundary guard, on and off.
func TestDocWordKeywords(t *testing.T) {
	emit := func(on bool) string {
		spec, err := bnf.EmitGrammarSpec(&bnf.Grammar{Productions: []*bnf.Production{
			{Name: "stmt", Alts: []bnf.Sequence{
				{{Kind: bnf.KindTerm, Literal: "option"}},
			}},
		}}, &bnf.ConvertOptions{Tag: "demo", Start: "stmt", WordKeywords: on})
		if err != nil {
			t.Fatalf("emit: %v", err)
		}
		return bnf.SpecToJSON(spec, 0)
	}
	if !strings.Contains(emit(false), `"#OPTION": "@~/^option/i"`) {
		t.Error("want the plain anchored regex")
	}
	if !strings.Contains(emit(true), `"#OPTION": "@~/^option\\b/i"`) {
		t.Error("want the word-boundary guard")
	}
}

// guide.md: a removal reaches the spec as a nil rule entry.
func TestDocRemoval(t *testing.T) {
	spec, err := bnf.EmitGrammarSpec(&bnf.Grammar{
		Productions: []*bnf.Production{
			{Name: "top", Alts: []bnf.Sequence{{{Kind: bnf.KindToken, Name: "#NR"}}}},
		},
		Remove: []string{"val"},
	}, &bnf.ConvertOptions{Tag: "demo", Start: "top"})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	entry, present := spec.Rule["val"]
	if !present || nil != entry {
		t.Errorf("want a present nil entry, got present=%v entry=%v", present, entry)
	}
}

// guide.md and reference.md: provenance is on unless turned off.
func TestDocProvenance(t *testing.T) {
	grammar := &bnf.Grammar{Productions: []*bnf.Production{
		{Name: "list", Alts: []bnf.Sequence{{
			{Kind: bnf.KindTerm, Literal: "("},
			{Kind: bnf.KindStar, Inner: &bnf.Element{
				Kind: bnf.KindRef, Name: "item"}},
			{Kind: bnf.KindTerm, Literal: ")"},
		}}},
		{Name: "item", Alts: []bnf.Sequence{{{Kind: bnf.KindToken, Name: "#NR"}}}},
	}}
	on, err := bnf.EmitGrammarSpec(grammar,
		&bnf.ConvertOptions{Tag: "demo", Start: "list"})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	prov, ok := on.Meta["provenance"].(map[string]any)
	if !ok {
		t.Fatalf("want a provenance map, got %T", on.Meta["provenance"])
	}
	if "list" != prov["__start__"] {
		t.Errorf("want __start__ to name list, got %v", prov["__start__"])
	}

	off := false
	small, err := bnf.EmitGrammarSpec(grammar, &bnf.ConvertOptions{
		Tag: "demo", Start: "list", Provenance: &off})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if _, present := small.Meta["provenance"]; present {
		t.Error("want no provenance key when turned off")
	}
}

// guide.md: the pass returns a new grammar and leaves the input alone.
func TestDocEliminateLeftRecursion(t *testing.T) {
	in := &bnf.Grammar{Productions: []*bnf.Production{
		{Name: "expr", Alts: []bnf.Sequence{
			{{Kind: bnf.KindRef, Name: "expr"},
				{Kind: bnf.KindTerm, Literal: "+"},
				{Kind: bnf.KindToken, Name: "#NR"}},
			{{Kind: bnf.KindToken, Name: "#NR"}},
		}},
	}}
	alts := len(in.Productions[0].Alts)
	out := bnf.EliminateLeftRecursion(in)
	if out == in {
		t.Error("want a new grammar")
	}
	if alts != len(in.Productions[0].Alts) {
		t.Error("the input grammar was modified")
	}
}

// reference.md: the helper table.
func TestDocHelpers(t *testing.T) {
	builtins := bnf.BuiltinTokens()
	for bare, token := range map[string]string{
		"NR": "#NR", "ST": "#ST", "TX": "#TX", "VL": "#VL"} {
		if token != builtins[bare] {
			t.Errorf("want %s -> %s, got %s", bare, token, builtins[bare])
		}
	}
	if 4 != len(builtins) {
		t.Errorf("the pages list four builtin tokens, found %d", len(builtins))
	}
	if `a\.b\*c` != bnf.EscapeRegexp("a.b*c") {
		t.Errorf("got %q", bnf.EscapeRegexp("a.b*c"))
	}
	plus := &bnf.Element{Kind: bnf.KindTerm, Literal: "+"}
	word := &bnf.Element{Kind: bnf.KindTerm, Literal: "if"}
	if "cs:+" != bnf.TermKey(plus) || !bnf.IsEffectivelyCaseSensitive(plus) {
		t.Error("a literal with no ASCII letter is case-sensitive either way")
	}
	if "ci:if" != bnf.TermKey(word) || bnf.IsEffectivelyCaseSensitive(word) {
		t.Error("a word literal is insensitive unless said otherwise")
	}
	if !bnf.IsProseName("<remove>") || bnf.IsProseName("remove") {
		t.Error("a prose directive is the angle-bracketed form")
	}
	refs := map[string]bool{}
	bnf.RefsIn(bnf.Sequence{
		{Kind: bnf.KindRef, Name: "a"}, {Kind: bnf.KindRef, Name: "b"}}, refs)
	if 2 != len(refs) {
		t.Errorf("want two refs, got %d", len(refs))
	}
	if 1<<30 != bnf.MaxInfinity {
		t.Errorf("want 1<<30, got %d", bnf.MaxInfinity)
	}
}

// tutorial.md: the serialisation pair.
func TestDocSerialise(t *testing.T) {
	spec, err := bnf.EmitGrammarSpec(&bnf.Grammar{Productions: []*bnf.Production{
		{Name: "top", Alts: []bnf.Sequence{{{Kind: bnf.KindToken, Name: "#NR"}}}},
	}}, &bnf.ConvertOptions{Tag: "demo", Start: "top"})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if "" == bnf.SpecToJSON(spec, 2) {
		t.Error("want JSON text")
	}
	data, derr := bnf.SpecToDataErr(spec)
	if derr != nil {
		t.Fatalf("data: %v", derr)
	}
	for _, key := range []string{"rule", "options"} {
		if _, present := data[key]; !present {
			t.Errorf("want a %q key", key)
		}
	}
}
