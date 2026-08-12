// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

package bnf

// The options a front-end sets ARE part of the accepted language, so a
// serialized spec that loses them is not a smaller spec — it is a
// different grammar. These tests pin the round-trip through the engine's
// own reader (MapToOptions), because that is what actually consumes the
// emitted block.

import (
	"encoding/json"
	"strings"
	"testing"

	tabnas "github.com/tabnas/parser/go"
)

func ptrBool(b bool) *bool { return &b }

// exactLexing is what a scannerless front-end sets: every default
// matcher off, an empty ignore set, negotiated lexing on. Before this
// was serialized, a reloaded GBNF grammar lexed "a+b" as one text token
// and rejected input a natively installed copy accepted.
func exactLexing() *tabnas.Options {
	off := false
	on := true
	return &tabnas.Options{
		TokenSet: map[string][]string{"IGNORE": {}},
		Space:    &tabnas.SpaceOptions{Lex: &off},
		Line:     &tabnas.LineOptions{Lex: &off},
		Comment:  &tabnas.CommentOptions{Lex: &off},
		String:   &tabnas.StringOptions{Lex: &off},
		Number:   &tabnas.NumberOptions{Lex: &off},
		Text:     &tabnas.TextOptions{Lex: &off},
		Value:    &tabnas.ValueOptions{Lex: &off},
		Lex:      &tabnas.LexOptions{Empty: &off, Relex: &on},
		Rule:     &tabnas.RuleOptions{Start: "__start__"},
	}
}

func TestExactLexingSurvivesSerialisation(t *testing.T) {
	data, err := optionsToData(exactLexing())
	if err != nil {
		t.Fatalf("optionsToData: %v", err)
	}

	// Through text and back, the way a serialized grammar travels.
	var back map[string]any
	if err := json.Unmarshal([]byte(ToJsonic(data, true, 0)), &back); err != nil {
		t.Fatalf("emitted options are not valid JSON: %v", err)
	}
	got := tabnas.MapToOptions(back)

	for _, c := range []struct {
		name string
		lex  *bool
	}{
		{"space", got.Space.Lex}, {"line", got.Line.Lex},
		{"comment", got.Comment.Lex}, {"string", got.String.Lex},
		{"number", got.Number.Lex}, {"text", got.Text.Lex},
		{"value", got.Value.Lex},
	} {
		if c.lex == nil || *c.lex {
			t.Errorf("%s.lex did not survive: %v", c.name, c.lex)
		}
	}
	if got.Lex == nil || got.Lex.Relex == nil || !*got.Lex.Relex {
		t.Error("lex.relex did not survive — alternates cannot re-cut a span")
	}
	if ts, ok := got.TokenSet["IGNORE"]; !ok || len(ts) != 0 {
		t.Errorf("empty IGNORE token set did not survive: %v", got.TokenSet)
	}
	if got.Rule == nil || got.Rule.Start != "__start__" {
		t.Errorf("rule.start did not survive: %v", got.Rule)
	}
}

// A spec whose options carry a function cannot be emitted as data. The
// old code dropped such fields silently; silence is the bug.
func TestFunctionValuedOptionsAreRefused(t *testing.T) {
	opt := exactLexing()
	opt.Number.Exclude = func(string) bool { return false }

	if _, err := optionsToData(opt); err == nil {
		t.Fatal("a func-valued option was accepted; it must be refused")
	} else if !strings.Contains(err.Error(), "number.exclude") {
		t.Errorf("error should name the offending field, got: %v", err)
	}
}

// The guard that keeps this file honest as the engine grows: an Options
// field nobody taught optionsToData about must fail loudly rather than
// disappear from every serialized grammar. Parser holds a func, so it
// is both unhandled AND genuinely unserialisable.
func TestUnhandledOptionFieldIsRefused(t *testing.T) {
	opt := exactLexing()
	opt.Parser = &tabnas.ParserOptions{
		Start: func(string, *tabnas.Tabnas, map[string]any) (any, error) {
			return nil, nil
		},
	}

	_, err := optionsToData(opt)
	if err == nil {
		t.Fatal("an unhandled option field was accepted silently")
	}
	if !strings.Contains(err.Error(), "Parser") {
		t.Errorf("error should name the field, got: %v", err)
	}
}

// Diagnostics are plain data and TS's cloneData carries them through.
// Refusing them would be a Go-only failure for a spec the canonical
// runtime serialises happily — a divergence in the other direction.
func TestDiagnosticOptionsAreCarriedNotRefused(t *testing.T) {
	opt := exactLexing()
	opt.Error = map[string]string{"unexpected": "custom message"}
	opt.Hint = map[string]string{"unexpected": "try a comma"}

	data, err := optionsToData(opt)
	if err != nil {
		t.Fatalf("plain-data diagnostics were refused: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal([]byte(ToJsonic(data, true, 0)), &back); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	got := tabnas.MapToOptions(back)
	if got.Error["unexpected"] != "custom message" {
		t.Errorf("error templates did not survive: %v", got.Error)
	}
	if got.Hint["unexpected"] != "try a comma" {
		t.Errorf("hints did not survive: %v", got.Hint)
	}
}

// A dump API that swallowed the refusal returned an EMPTY spec while
// reporting success — and panicked on a nil map when the spec carried
// refs. An empty grammar presented as the real one is precisely the
// silent-wrong-grammar failure this package exists to avoid.
func TestDumpAPIsDoNotSwallowRefusals(t *testing.T) {
	opt := exactLexing()
	opt.Number.Exclude = func(string) bool { return false }
	spec := &tabnas.GrammarSpec{
		Options: opt,
		Ref:     map[string]any{"@x": func() {}}, // the nil-map panic path
	}

	if _, err := SpecToDataErr(spec); err == nil {
		t.Error("SpecToDataErr accepted an unserialisable spec")
	}
	if _, err := SpecToJSONErr(spec, 0); err == nil {
		t.Error("SpecToJSONErr accepted an unserialisable spec")
	}
	// The signature-compatible forms must not lie, and must not panic.
	if got := SpecToData(spec); got != nil {
		t.Errorf("SpecToData should yield nil, not a plausible spec: %v", got)
	}
	if got := SpecToJSON(spec, 0); got != "" {
		t.Errorf("SpecToJSON should yield \"\", not %q", got)
	}
}

// JSON forbids raw C0 controls in strings. space.chars carrying a tab
// is entirely plausible, and used to serialise to text that would not
// parse back.
func TestControlCharactersSurviveSerialisation(t *testing.T) {
	for _, ch := range []string{"\t", "\r", "\n", "\x01", "\x1f"} {
		opt := exactLexing()
		opt.Space.Chars = ch
		data, err := optionsToData(opt)
		if err != nil {
			t.Fatalf("chars=%q: %v", ch, err)
		}
		text := ToJsonic(data, true, 0)
		var back map[string]any
		if err := json.Unmarshal([]byte(text), &back); err != nil {
			t.Errorf("chars=%q emitted invalid JSON: %v", ch, err)
			continue
		}
		if got := tabnas.MapToOptions(back); got.Space.Chars != ch {
			t.Errorf("chars=%q came back as %q", ch, got.Space.Chars)
		}
	}
}

func TestNilOptionsIsEmptyNotAnError(t *testing.T) {
	data, err := optionsToData(nil)
	if err != nil || len(data) != 0 {
		t.Errorf("nil Options should be an empty block: %v %v", data, err)
	}
}
