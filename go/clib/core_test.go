// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

package main

// The library's contract, tested where it is testable. The cgo shim in
// bnf_c.go cannot be unit-tested (Go forbids cgo in _test.go), which is
// exactly why the behaviour lives in core.go.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	tabnas "github.com/tabnas/parser/go"
)

func doc(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("result is not JSON: %v (%q)", err, s)
	}
	return m
}

// A serialized spec to reduce. The engine's own JSON-builder fixture is
// used when a sibling checkout has it; otherwise these tests skip rather
// than assert against a grammar invented here.
func fixture(t *testing.T) string {
	t.Helper()
	for _, p := range []string{
		filepath.Join("..", "..", "..", "parser", "ts", "test",
			"json-builder.fixture.json"),
		filepath.Join("..", "..", "test", "json-builder.fixture.json"),
	} {
		if b, err := os.ReadFile(p); err == nil {
			return string(b)
		}
	}
	t.Skip("no serialized spec fixture available")
	return ""
}

func TestVersionDoc(t *testing.T) {
	got := doc(t, versionDoc())
	if got["ok"] != true || got["version"] == "" || got["engine"] == "" {
		t.Errorf("version: %v", got)
	}
}

// The property that makes the reduction worth exposing at all: what
// comes out must still be a working grammar, not merely valid JSON.
func TestReducedSpecStillParses(t *testing.T) {
	for _, recognition := range []bool{true, false} {
		res := doc(t, reduce(fixture(t), recognition))
		if res["ok"] != true {
			t.Fatalf("recognition=%v: %v", recognition, res)
		}
		out, _ := res["spec"].(string)
		if out == "" {
			t.Fatalf("recognition=%v: empty spec", recognition)
		}

		gs, err := tabnas.GrammarSpecFromJSON([]byte(out))
		if err != nil {
			t.Fatalf("recognition=%v: reduced spec will not load: %v",
				recognition, err)
		}
		tn := tabnas.Make()
		if err := tn.Grammar(gs); err != nil {
			t.Fatalf("recognition=%v: reduced spec will not install: %v",
				recognition, err)
		}
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
				t.Errorf("recognition=%v: %q accept=%v want %v",
					recognition, c.src, err == nil, c.want)
			}
		}
	}
}

// What actually separates the two reductions, pinned on a spec that
// exercises it.
//
// Recognition drops the TREE builtins (@node$ / @capture$ / @bubble$)
// and the spec's own ref-backed actions; pure keeps them. It does NOT
// drop the native-value family (@object$, @value$, …), so for a spec
// built from those the two reductions are byte-identical — which is
// true of the engine's json-builder fixture, and worth knowing before
// reaching for recognition mode expecting it to shrink something.
func TestRecognitionDropsTreeBuiltinsPureKeepsThem(t *testing.T) {
	const spec = `{"rule":{"val":{"open":[{"s":["#NR"],"a":"@node$"}]}}}`

	rec := doc(t, reduce(spec, true))
	pure := doc(t, reduce(spec, false))
	if rec["ok"] != true || pure["ok"] != true {
		t.Fatalf("reduction failed: %v / %v", rec, pure)
	}

	if containsStr(rec["spec"].(string), "@node$") {
		t.Errorf("recognition kept a tree builtin: %s", rec["spec"])
	}
	if !containsStr(pure["spec"].(string), "@node$") {
		t.Errorf("pure dropped a tree builtin it should keep: %s", pure["spec"])
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestBadInputIsACallErrorNotASpec(t *testing.T) {
	for _, c := range []struct{ name, src string }{
		{"not JSON", "{not a spec"},
		{"truncated", `{"rule":`},
	} {
		res := doc(t, reduce(c.src, true))
		if res["ok"] != false {
			t.Errorf("%s: accepted, got %v", c.name, res)
		}
		if _, leaked := res["spec"]; leaked {
			t.Errorf("%s: a failure must not carry a spec: %v", c.name, res)
		}
	}
}

// Emitted through ToJsonic rather than encoding/json, because a regex
// travels as an "@/src/flags" sentinel. encoding/json would write {} for
// the holder and the grammar would lex nothing — the failure this
// asserts against.
func TestMatchTokensSurviveTheReduction(t *testing.T) {
	out := doc(t, reduce(fixture(t), true))["spec"].(string)
	var back map[string]any
	if err := json.Unmarshal([]byte(out), &back); err != nil {
		t.Fatalf("emitted spec is not valid JSON: %v", err)
	}
	opts, _ := back["options"].(map[string]any)
	if m, ok := opts["match"].(map[string]any); ok {
		tok, _ := m["token"].(map[string]any)
		for name, v := range tok {
			s, isStr := v.(string)
			if !isStr || (len(s) > 1 && s[0] != '@') {
				t.Errorf("match token %q serialized as %#v, not a sentinel",
					name, v)
			}
		}
	}
}
