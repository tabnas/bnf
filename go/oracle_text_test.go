// Copyright (c) 2026 tabnas, MIT License

package bnf

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// textDivergent is the shared fixtures whose text the Go port writes
// otherwise than TypeScript, each with where the difference is recorded.
// An entry whose text starts matching fails, and must then be deleted.
var textDivergent = map[string]string{
	"ir-loop-array-terminal-guarded": "DIVERGENCE.md, \"A FIRST or FOLLOW set's alternates " +
		"are listed in token-name order\"",
	"ir-loop-array-terminal-guarded-lead": "DIVERGENCE.md, \"A FIRST or FOLLOW set's alternates " +
		"are listed in token-name order\"",
}

// The shared fixtures under rs/tests/oracle/ hold the canonical compiler's
// text for each IR (generate.cjs): `pure`, ToJsonic over toPureSpec of a
// builtins compile, and `recognition`, over toRecognitionSpec of a closure
// compile. The Rust port is held to them byte for byte
// (rs/tests/oracle_test.rs), and this holds the Go port to them too.
//
// Go wrote every map of a spec in name order, and the match tokens' order
// in a list of its own (`options.match.tokenOrder`), where TypeScript and
// Rust write a spec in the order the compiler makes it: the rules in the
// order it commits them, which ranks the match tokens, each token in the
// order it is allocated, each alternate's keys in the order they are set.
// The notations' translation renders read a spec back in that order, and
// refused Go's. See spec_order.go.
//
// A fixture whose IR spells a code point as JavaScript does (`0`) is
// not compared: RE2 does not read that spelling, a Go front-end hands the
// compiler `\x{0030}` instead, and a class token is named after its
// pattern, so the names differ by construction.
func TestOracleTextMatchesTypeScript(t *testing.T) {
	dir := filepath.Join("..", "rs", "tests", "oracle")
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no oracle fixtures under %s: %v", dir, err)
	}
	compared := map[string]bool{}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var fx struct {
			Name             string          `json:"name"`
			IR               json.RawMessage `json:"ir"`
			Opts             ConvertOptions  `json:"opts"`
			Pure             *string         `json:"pure"`
			Recognition      *string         `json:"recognition"`
			RecognitionError *string         `json:"recognitionError"`
		}
		if err := json.Unmarshal(raw, &fx); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		// A refusal: TestOracleRefusalsMatchTypeScript grades it.
		if fx.Pure == nil {
			continue
		}
		if strings.Contains(string(fx.IR), `\\u`) {
			continue
		}
		var ir map[string]any
		if err := json.Unmarshal(fx.IR, &ir); err != nil {
			t.Fatalf("%s: %v", fx.Name, err)
		}
		pure, recognition, recErr := oracleTexts(t, fx.Name, ir, fx.Opts)

		if where, listed := textDivergent[fx.Name]; listed {
			if pure == *fx.Pure && (fx.Recognition == nil || recognition == *fx.Recognition) {
				t.Errorf("%s now writes TypeScript's text: delete its textDivergent entry (%s)",
					fx.Name, where)
			}
			continue
		}
		compared[fx.Name] = true
		if pure != *fx.Pure {
			t.Errorf("%s: the pure spec differs from TypeScript's at %s", fx.Name,
				firstDifference(*fx.Pure, pure))
		}
		switch {
		case fx.Recognition != nil && recognition != *fx.Recognition:
			t.Errorf("%s: the recognition spec differs from TypeScript's at %s", fx.Name,
				firstDifference(*fx.Recognition, recognition))
		case fx.RecognitionError != nil && recErr != *fx.RecognitionError:
			t.Errorf("%s: recognition refused as %q, TypeScript refused as %q",
				fx.Name, recErr, *fx.RecognitionError)
		}
	}

	// The three shapes the defect was found on: literals the compiler makes
	// case-folding match tokens (`pair = "a" "b"`), keywords and character
	// classes, which make fixed and match tokens whose allocation order is
	// not their name order, and rules the compiler commits out of name
	// order (`val` before `add`; a dispatch's chains before the rule).
	for _, name := range []string{"abnf-1", "ir-order-keywords-classes", "abnf-grammar-addition"} {
		if !compared[name] {
			t.Errorf("%s was not compared with TypeScript's text", name)
		}
	}
}

// oracleTexts is what the Go port writes for an IR, as generate.cjs writes
// TypeScript's: the pure spec of a builtins compile, and the recognition
// spec of a closure compile or the message refusing it.
func oracleTexts(t *testing.T, name string, ir map[string]any, opts ConvertOptions) (string, string, string) {
	t.Helper()
	opts.Builtins = true
	spec, err := EmitGrammarSpec(oracleGrammar(t, ir), &opts)
	if err != nil {
		t.Fatalf("%s: TypeScript emits this IR, Go refused it: %v", name, err)
	}
	data, err := ToPureSpec(spec)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	pure := ToJsonic(data, true, 0)

	opts.Builtins = false
	closure, err := EmitGrammarSpec(oracleGrammar(t, ir), &opts)
	if err != nil {
		t.Fatalf("%s: TypeScript emits this IR, Go refused it: %v", name, err)
	}
	rec, err := ToRecognitionSpec(closure)
	if err != nil {
		return pure, "", err.Error()
	}
	return pure, ToJsonic(rec, true, 0), ""
}

// firstDifference names the first line at which two texts differ.
func firstDifference(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			return "line " + itoa(i+1) + ": want " + strings.TrimSpace(wl) +
				", got " + strings.TrimSpace(gl)
		}
	}
	return "nowhere"
}
