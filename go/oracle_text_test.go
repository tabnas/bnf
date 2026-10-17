// Copyright (c) 2026 tabnas, MIT License

package bnf

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	tabnas "github.com/tabnas/parser/go"
)

// textDivergent is the shared fixtures whose text the Go port writes
// otherwise than TypeScript, each with where the difference is recorded.
// An entry whose text starts matching fails, and must then be deleted.
var textDivergent = map[string]string{
	"ir-loop-array-terminal-guarded":      fanOutOrder,
	"ir-loop-array-terminal-guarded-lead": fanOutOrder,
	"abnf-2":                              fanOutOrder,
	"abnf-grammar-arith":                  fanOutOrder,
	"abnf-grammar-arith-leftrec":          fanOutOrder,
}

const fanOutOrder = "DIVERGENCE.md, \"A FIRST or FOLLOW set's alternates are listed in token-name order\""

// oracleFixture is one shared fixture under rs/tests/oracle/, as
// generate.cjs writes it: an IR, the canonical compiler's text for it, and
// the TypeScript engine's verdict on each case source.
type oracleFixture struct {
	Name             string         `json:"name"`
	IR               map[string]any `json:"ir"`
	Opts             ConvertOptions `json:"opts"`
	Pure             *string        `json:"pure"`
	Recognition      *string        `json:"recognition"`
	RecognitionError *string        `json:"recognitionError"`
	Cases            []struct {
		Source   string `json:"source"`
		Accepted bool   `json:"accepted"`
		Code     string `json:"code"`
	} `json:"cases"`
	Transduce *struct {
		Entry  string `json:"entry"`
		Source string `json:"source"`
		Whole  string `json:"whole"`
	} `json:"transduce"`
}

func oracleFixtures(t *testing.T) []oracleFixture {
	t.Helper()
	dir := filepath.Join("..", "rs", "tests", "oracle")
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no oracle fixtures under %s: %v", dir, err)
	}
	var out []oracleFixture
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var fx oracleFixture
		if err := json.Unmarshal(raw, &fx); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		out = append(out, fx)
	}
	return out
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
// A fixture whose IR spells a code point as JavaScript does (`\u0030`,
// `\u{1f600}`) is compared in the spelling a Go front-end hands this
// compiler, `\x{…}`, since RE2 does not read JavaScript's. A class token is
// named after its pattern, so the two spellings name one class two ways;
// the names are mapped, and every other byte is compared.
func TestOracleTextMatchesTypeScript(t *testing.T) {
	compared := map[string]bool{}
	for _, fx := range oracleFixtures(t) {
		// A refusal: TestOracleRefusalsMatchTypeScript grades it.
		if fx.Pure == nil {
			continue
		}
		ir, renamed := inRE2Spelling(fx.IR)
		pure, recognition, recErr := oracleTexts(t, fx.Name, ir, fx.Opts)
		pure, recognition = withNames(pure, renamed), withNames(recognition, renamed)

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

	// The shapes the defects were found on: literals the compiler makes
	// case-folding match tokens (`pair = "a" "b"`), keywords and character
	// classes, which make fixed and match tokens whose allocation order is
	// not their name order, rules the compiler commits out of name order
	// (`val` before `add`; a dispatch's chains before the rule), classes
	// spelled in JavaScript's `\uHHHH` (`ALPHA`), and classes past U+FFFF,
	// which the canonical compiler writes in braces under `u`.
	for _, name := range []string{"abnf-1", "ir-order-keywords-classes", "abnf-grammar-addition",
		"abnf-3", "abnf-grammar-astral"} {
		if !compared[name] {
			t.Errorf("%s was not compared with TypeScript's text", name)
		}
	}
}

// The transducer part TransduceAlc writes for the emitted spec, under the
// fixture's name, or why the grammar is read whole, is TypeScript's in
// both emit modes (rs/tests/oracle_test.rs holds the Rust port the same
// way). A fixture whose spec text this port writes otherwise is compared
// too: the part reads the value actions and the rule names, not the order
// of the alternates.
func TestOracleTransducerPartMatchesTypeScript(t *testing.T) {
	checked := 0
	for _, fx := range oracleFixtures(t) {
		if fx.Pure == nil || fx.Transduce == nil {
			continue
		}
		ir, _ := inRE2Spelling(fx.IR)
		for _, builtins := range []bool{true, false} {
			opts := fx.Opts
			opts.Builtins = builtins
			spec, err := EmitGrammarSpec(oracleGrammar(t, ir), &opts)
			if err != nil {
				t.Fatalf("%s: TypeScript emits this IR, Go refused it: %v", fx.Name, err)
			}
			got := TransduceAlc(spec, fx.Name)
			checked++
			if got.Entry != fx.Transduce.Entry || got.Whole != fx.Transduce.Whole {
				t.Errorf("%s: got entry %q whole %q, want entry %q whole %q", fx.Name,
					got.Entry, got.Whole, fx.Transduce.Entry, fx.Transduce.Whole)
			}
			if got.Source != fx.Transduce.Source {
				t.Errorf("%s: the part differs from TypeScript's at %s", fx.Name,
					firstDifference(fx.Transduce.Source, got.Source))
			}
		}
	}
	if checked == 0 {
		t.Fatal("no fixture records a transducer part")
	}
}

// TestAnAstralClassReadsBackAsTypeScriptReadsIt installs the spec this port
// writes for abnf-grammar-astral, read back through the engine's JSON door,
// and holds it to the verdict the TypeScript engine gave each case on the
// canonical compiler's spec. Its classes reach past U+FFFF, and a matcher
// spelled `\u1F600` is U+1F60 followed by `0` to both engines: that spec
// took `ὠ!` and `0!` and refused `😀!`. A class that loses its `u` takes
// half an astral character to the TypeScript engine.
func TestAnAstralClassReadsBackAsTypeScriptReadsIt(t *testing.T) {
	var fx *oracleFixture
	for _, f := range oracleFixtures(t) {
		if f.Name == "abnf-grammar-astral" {
			fx = &f
		}
	}
	if fx == nil || len(fx.Cases) == 0 {
		t.Fatal("no abnf-grammar-astral fixture with cases under rs/tests/oracle")
	}
	ir, _ := inRE2Spelling(fx.IR)
	opts := fx.Opts
	opts.Builtins = true
	spec, err := EmitGrammarSpec(oracleGrammar(t, ir), &opts)
	if err != nil {
		t.Fatal(err)
	}
	data, err := ToPureSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	text := ToJsonic(data, true, 0)

	// A class atom the partition mints, and a class from the IR that runs
	// from U+0080 past U+FFFF, each as the canonical compiler writes it.
	for _, want := range []string{`"@~/^[\\u{1F600}-\\u{1F63F}]/u"`, `"@~/^[\\u{80}-\\u{1f5ff}]/u"`} {
		if !strings.Contains(text, want) {
			t.Errorf("the spec does not write the matcher %s:\n%s", want, text)
		}
	}

	gs, err := tabnas.GrammarSpecFromJSON([]byte(text))
	if err != nil {
		t.Fatalf("the engine cannot read the spec back: %v", err)
	}
	j := tabnas.Make()
	if err := j.Grammar(gs); err != nil {
		t.Fatalf("the engine cannot install the spec: %v", err)
	}
	for _, c := range fx.Cases {
		_, err := j.Parse(c.Source)
		code := ""
		var te *tabnas.TabnasError
		if errors.As(err, &te) {
			code = te.Code
		} else if err != nil {
			code = err.Error()
		}
		switch {
		case c.Accepted && err != nil:
			t.Errorf("%q: the TypeScript engine accepts it, this one refuses it (%s)", c.Source, code)
		case !c.Accepted && err == nil:
			t.Errorf("%q: the TypeScript engine refuses it (%s), this one accepts it", c.Source, c.Code)
		case !c.Accepted && code != c.Code:
			t.Errorf("%q: refused as %s, the TypeScript engine refuses it as %s", c.Source, code, c.Code)
		}
	}
}

// jsCodePoint is JavaScript's two spellings of a code point in a pattern.
var jsCodePoint = regexp.MustCompile(`\\u\{([0-9A-Fa-f]+)\}|\\u([0-9A-Fa-f]{4})`)

// inRE2Spelling is an IR as a Go front-end hands it to this compiler, each
// code point of a pattern spelled `\x{…}`, with the name that spelling
// gives each class token mapped to the name the canonical compiler gives
// it from JavaScript's.
func inRE2Spelling(ir map[string]any) (map[string]any, map[string]string) {
	renamed := map[string]string{}
	var walk func(v any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case map[string]any:
			out := make(map[string]any, len(x))
			for k, e := range x {
				out[k] = walk(e)
			}
			if p, ok := x["pattern"].(string); ok && x["kind"] == "regex" {
				q := jsCodePoint.ReplaceAllString(p, `\x{$1$2}`)
				out["pattern"] = q
				if q != p {
					renamed[allocTokenName("rx_"+q, map[string]bool{}, "")] =
						allocTokenName("rx_"+p, map[string]bool{}, "")
				}
			}
			return out
		case []any:
			out := make([]any, len(x))
			for i, e := range x {
				out[i] = walk(e)
			}
			return out
		}
		return v
	}
	return walk(ir).(map[string]any), renamed
}

// withNames writes each name in text as renamed maps it: in an alternate's
// token list and as a key, `#` or not, never inside a longer name.
func withNames(text string, renamed map[string]string) string {
	for from, to := range renamed {
		from, to = strings.TrimPrefix(from, "#"), strings.TrimPrefix(to, "#")
		re := regexp.MustCompile(`(^|[^A-Za-z0-9_])` + regexp.QuoteMeta(from) + `([^A-Za-z0-9_]|$)`)
		// Twice over at most: a match takes the character after the name,
		// which the same name right after it needs as its own boundary.
		for prev := ""; prev != text; {
			prev, text = text, re.ReplaceAllString(text, "${1}"+to+"${2}")
		}
	}
	return text
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
