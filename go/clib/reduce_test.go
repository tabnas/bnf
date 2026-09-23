// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

package main

// What libtabnasbnf promises beyond the uniform contract core_test.go
// pins. core_test.go is template-owned and identical across the fleet;
// this file is the repository's own, and holds the guarantees that are
// specific to a library whose input is a serialized GrammarSpec and
// whose value is two reductions of it.
//
// Everything here goes through parseWith, the function tabnas_parse
// calls, so it checks what a C or Python caller actually receives.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	tabnas "github.com/tabnas/parser/go"
)

// treeSpec carries a TREE builtin with its config, which is what
// separates the two reductions, and an alternate written in array form.
const treeSpec = `{"rule":{"val":{"open":[{"s":["#NR"],"a":"@node$",` +
	`"k":{"node$":{"init":true,"rule":"val","nterms":1}}}]}}}`

// reduced runs one spec through the library and returns the reply's
// value, failing the test unless the spec was accepted.
func reduced(t *testing.T, spec string) map[string]any {
	t.Helper()
	h := loadHandle(t)
	defer freeGrammar(h)
	m := decode(t, parseWith(h, spec))
	if m["ok"] != true || m["accept"] != true {
		t.Fatalf("spec was not reduced: %v", m)
	}
	v, ok := m["value"].(map[string]any)
	if !ok {
		t.Fatalf("accepted with no value: %v", m)
	}
	return v
}

// reload installs one reduction in a fresh engine, the way a caller
// hands value.recognition to the engine's own library.
func reload(t *testing.T, which string, data any) *tabnas.Tabnas {
	t.Helper()
	text, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("%s: value will not re-encode: %v", which, err)
	}
	gs, err := tabnas.GrammarSpecFromJSON(text)
	if err != nil {
		t.Fatalf("%s: reduced spec will not load: %v\n%s", which, err, text)
	}
	tn := tabnas.Make()
	if err := tn.Grammar(gs); err != nil {
		t.Fatalf("%s: reduced spec will not install: %v\n%s", which, err, text)
	}
	return tn
}

// The engine's JSON-builder fixture, when a sibling tabnas/parser
// checkout has it (CI clones one). Skips rather than assert against a
// grammar invented here.
func engineFixture(t *testing.T) string {
	t.Helper()
	p := filepath.Join("..", "..", "..", "parser", "ts", "test",
		"json-builder.fixture.json")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Skip("no sibling tabnas/parser checkout with json-builder.fixture.json")
	}
	return string(b)
}

// The property that makes the reduction worth exposing at all: what
// comes out must still be a WORKING grammar, not merely valid JSON.
func TestReducedSpecStillParses(t *testing.T) {
	v := reduced(t, engineFixture(t))
	for _, which := range []string{"recognition", "pure"} {
		tn := reload(t, which, v[which])
		for _, c := range []struct {
			src  string
			want bool
		}{
			{`{"a":1}`, true},
			{`{"a":1,"b":[1,2]}`, true},
			{`{"a":1,}`, false},
			{`{oops`, false},
		} {
			_, err := tn.Parse(c.src)
			if (err == nil) != c.want {
				t.Errorf("%s: %q accept=%v want %v (%v)",
					which, c.src, err == nil, c.want, err)
			}
		}
	}
}

// The same property on a spec defined here, so it holds without the
// sibling checkout too — and pinned on what separates the reductions:
// the pure grammar still builds a value, the recognition grammar
// builds none.
func TestReducedTreeSpecStillParses(t *testing.T) {
	v := reduced(t, treeSpec)

	rec := reload(t, "recognition", v["recognition"])
	pure := reload(t, "pure", v["pure"])
	for which, tn := range map[string]*tabnas.Tabnas{"recognition": rec, "pure": pure} {
		if _, err := tn.Parse("1"); err != nil {
			t.Errorf("%s: rejected 1: %v", which, err)
		}
		if _, err := tn.Parse(`"x"`); err == nil {
			t.Errorf("%s: accepted a string the grammar does not allow", which)
		}
	}

	got, _ := rec.Parse("1")
	if got != nil {
		t.Errorf("recognition built a value: %#v", got)
	}
	got, _ = pure.Parse("1")
	if got == nil {
		t.Errorf("pure built no value; the tree builtin was lost")
	}
}

// Recognition drops the output builders — the tree family (@node$,
// @capture$, @bubble$, @fold$) AND the native-value family (@object$,
// @array$, @setval$, …) — while pure keeps every one of them.
func TestRecognitionDropsBuildersPureKeepsThem(t *testing.T) {
	const spec = `{"rule":{` +
		`"val":{"open":[{"s":["#NR"],"a":"@node$"},{"s":["#OB"],"p":"map","a":"@object$"}]},` +
		`"map":{"close":[{"s":["#CB"],"a":"@setval$"}]}}}`
	v := reduced(t, spec)

	text := func(which string) string {
		b, err := json.Marshal(v[which])
		if err != nil {
			t.Fatalf("%s: %v", which, err)
		}
		return string(b)
	}
	rec, pure := text("recognition"), text("pure")
	for _, b := range []string{"@node$", "@object$", "@setval$"} {
		if strings.Contains(rec, b) {
			t.Errorf("recognition kept the builder %s: %s", b, rec)
		}
		if !strings.Contains(pure, b) {
			t.Errorf("pure dropped the builder %s: %s", b, pure)
		}
	}
}

// The value is encoded with encoding/json, not ToJsonic. The engine
// loads a JSON `"s":["#NR"]` as a []string, which ToJsonic writes as
// null; the library before the uniform ABI returned exactly that for
// every alternate in array form. Pinned so the encoder cannot drift
// back.
func TestArrayFormTokensSurvive(t *testing.T) {
	v := reduced(t, treeSpec)
	for _, which := range []string{"recognition", "pure"} {
		spec, _ := v[which].(map[string]any)
		rule, _ := spec["rule"].(map[string]any)
		val, _ := rule["val"].(map[string]any)
		open, _ := val["open"].([]any)
		if len(open) != 1 {
			t.Fatalf("%s: want one alternate, got %v", which, spec)
		}
		alt, _ := open[0].(map[string]any)
		if !reflect.DeepEqual(alt["s"], []any{"#NR"}) {
			t.Errorf("%s: s = %#v, want [\"#NR\"]", which, alt["s"])
		}
	}
}

// A regex match token travels as an "@/source/flags" string, which the
// engine decodes on load. It must come out as that same string: an
// encoder that saw a compiled regexp would write something else, and
// the grammar would lex nothing — which this proves by lexing with it.
func TestMatchTokensSurviveTheReduction(t *testing.T) {
	const spec = `{"options":{"match":{"token":{"#ID":"@/[a-z]+x/"}}},` +
		`"rule":{"val":{"open":[{"s":["#ID"]}]}}}`
	v := reduced(t, spec)
	for _, which := range []string{"recognition", "pure"} {
		spec, _ := v[which].(map[string]any)
		opts, _ := spec["options"].(map[string]any)
		match, _ := opts["match"].(map[string]any)
		tok, _ := match["token"].(map[string]any)
		if tok["#ID"] != "@/[a-z]+x/" {
			t.Errorf("%s: match token = %#v, want the sentinel string", which, tok["#ID"])
		}

		tn := reload(t, which, v[which])
		if _, err := tn.Parse("abcx"); err != nil {
			t.Errorf("%s: the match token does not lex: %v", which, err)
		}
		if _, err := tn.Parse("abc"); err == nil {
			t.Errorf("%s: accepted input the match token cannot lex", which)
		}
	}
}

// A spec that cannot be reduced is a REJECTION (ok:true, accept:false),
// never a failed call: the library read it and answered. Before the
// uniform ABI these were ok:false with code spec or compile. Every one
// carries a message and no value, and a closure-bearing rule is named.
func TestUnreducibleSpecIsARejection(t *testing.T) {
	h := loadHandle(t)
	defer freeGrammar(h)
	for _, c := range []struct {
		name, src string
		rules     []any
	}{
		{"not JSON", "{not a spec", nil},
		{"truncated", `{"rule":`, nil},
		{"empty", "", nil},
		{"not an object", "[1]", nil},
		{"non-integer v", `{"v":2.5}`, nil},
		{"closure action", invalidSample, []any{"val"}},
	} {
		m := decode(t, parseWith(h, c.src))
		if m["ok"] != true || m["accept"] != false {
			t.Errorf("%s: want ok:true accept:false, got %v", c.name, m)
			continue
		}
		if _, leaked := m["value"]; leaked {
			t.Errorf("%s: a rejection must not carry a value: %v", c.name, m)
		}
		e, _ := m["error"].(map[string]any)
		if msg, _ := e["Message"].(string); msg == "" {
			t.Errorf("%s: rejection has no message: %v", c.name, m)
		}
		if c.rules != nil && !reflect.DeepEqual(e["Rules"], c.rules) {
			t.Errorf("%s: Rules = %v, want %v", c.name, e["Rules"], c.rules)
		}
	}
}
