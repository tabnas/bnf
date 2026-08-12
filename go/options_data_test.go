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
// disappear from every serialized grammar.
func TestUnhandledOptionFieldIsRefused(t *testing.T) {
	opt := exactLexing()
	opt.Error = map[string]string{"unexpected": "custom message"}

	_, err := optionsToData(opt)
	if err == nil {
		t.Fatal("an unhandled option field was accepted silently")
	}
	if !strings.Contains(err.Error(), "Error") {
		t.Errorf("error should name the field, got: %v", err)
	}
}

func TestNilOptionsIsEmptyNotAnError(t *testing.T) {
	data, err := optionsToData(nil)
	if err != nil || len(data) != 0 {
		t.Errorf("nil Options should be an empty block: %v %v", data, err)
	}
}
