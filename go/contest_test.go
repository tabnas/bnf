/* Copyright (c) 2026 Richard Rodger, MIT License */

package bnf

// Whether two dispatch heads CONTEST (can the lexer hand the same input
// to both) decides how deep the dispatcher looks. An answer of "no" that
// is wrong emits one-token entries for a decision that needs two, and the
// first branch commits on the first token and rejects input the other
// branch accepts. Mirrors ts/test/contest.test.js.

import (
	"reflect"
	"strings"
	"testing"

	tabnas "github.com/tabnas/parser/go"
)

func ctLit(s string) *Element {
	return &Element{Kind: KindTerm, Literal: s, CaseSensitive: true, HasCaseSens: true}
}
func ctILit(s string) *Element { return &Element{Kind: KindTerm, Literal: s} }
func ctRef(n string) *Element  { return &Element{Kind: KindRef, Name: n} }

// doc = x ; x = <a> t u / <b> u t ; t = "." "." ; u = "," ","
func ctGrammar(a, b *Element) *Grammar {
	return &Grammar{Productions: []*Production{
		{Name: "doc", Alts: []Sequence{{ctRef("x")}}},
		{Name: "x", Alts: []Sequence{{a, ctRef("t"), ctRef("u")}, {b, ctRef("u"), ctRef("t")}}},
		{Name: "t", Alts: []Sequence{{ctLit("."), ctLit(".")}}},
		{Name: "u", Alts: []Sequence{{ctLit(","), ctLit(",")}}},
	}}
}

// ctDepths is the number of lookahead tokens each open alternate of x
// names. Open is `any`; its elements carry the alternate's `s` either as
// a map entry or as a struct field, depending on the emitter's form.
func ctDepths(t *testing.T, spec *tabnas.GrammarSpec) []int {
	t.Helper()
	rule, ok := spec.Rule["x"]
	if !ok || rule == nil {
		t.Fatal("no rule x")
	}
	v := reflect.ValueOf(rule.Open)
	if !v.IsValid() || v.Kind() != reflect.Slice {
		t.Fatalf("open is %T", rule.Open)
	}
	var out []int
	for i := 0; i < v.Len(); i++ {
		el := v.Index(i)
		for el.Kind() == reflect.Interface || el.Kind() == reflect.Ptr {
			el = el.Elem()
		}
		var s string
		switch el.Kind() {
		case reflect.Map:
			s, _ = el.MapIndex(reflect.ValueOf("s")).Interface().(string)
		case reflect.Struct:
			f := el.FieldByName("S")
			if f.IsValid() {
				s, _ = f.Interface().(string)
			}
		default:
			t.Fatalf("alternate %d is %s", i, el.Kind())
		}
		out = append(out, len(strings.Fields(s)))
	}
	return out
}

func ctParses(t *testing.T, spec *tabnas.GrammarSpec, src string, relex bool) bool {
	t.Helper()
	j := tabnas.Make(tabnas.Options{Lex: &tabnas.LexOptions{Relex: tabnas.Bool(relex)}})
	if err := j.Grammar(spec); err != nil {
		t.Fatal(err)
	}
	_, err := j.Parse(src)
	return err == nil
}

func TestContestWordKeywordAgainstPunctuationContinuation(t *testing.T) {
	spec, err := EmitGrammarSpec(ctGrammar(ctLit("a"), ctLit("a-b")), &ConvertOptions{Tag: "ct", Start: "doc", WordKeywords: true})
	if err != nil {
		t.Fatal(err)
	}
	if d := ctDepths(t, spec); d[0] != 2 || d[1] != 2 {
		t.Fatalf("depths %v, want [2 2]", d)
	}
	for _, src := range []string{"a-b,,..", "a..,,"} {
		if !ctParses(t, spec, src, true) {
			t.Errorf("%q should parse", src)
		}
	}
	word, err := EmitGrammarSpec(ctGrammar(ctLit("a"), ctLit("ab")), &ConvertOptions{Tag: "ct", Start: "doc", WordKeywords: true})
	if err != nil {
		t.Fatal(err)
	}
	if d := ctDepths(t, word); d[0] != 1 || d[1] != 1 {
		t.Fatalf("depths %v, want [1 1]: a word continuation keeps the guard", d)
	}
}

func TestContestCaseInsensitiveLiteralsFoldAsTheirMatchersDo(t *testing.T) {
	// A literal with no ASCII letter compiles to an exact fixed token, so
	// Σ and ς never meet; with ASCII letters the matcher folds case.
	spec, err := EmitGrammarSpec(ctGrammar(ctILit("Σ"), ctILit("ς")), &ConvertOptions{Tag: "ct", Start: "doc"})
	if err != nil {
		t.Fatal(err)
	}
	if d := ctDepths(t, spec); d[0] != 1 || d[1] != 1 {
		t.Fatalf("depths %v, want [1 1]", d)
	}
	for _, src := range []string{"ς,,..", "Σ..,,"} {
		if !ctParses(t, spec, src, false) {
			t.Errorf("%q should parse", src)
		}
	}
	ascii, err := EmitGrammarSpec(ctGrammar(ctILit("a"), ctILit("A-b")), &ConvertOptions{Tag: "ct", Start: "doc"})
	if err != nil {
		t.Fatal(err)
	}
	if d := ctDepths(t, ascii); d[0] != 2 || d[1] != 2 {
		t.Fatalf("depths %v, want [2 2]", d)
	}
	for _, src := range []string{"A-B,,..", "A..,,"} {
		if !ctParses(t, ascii, src, true) {
			t.Errorf("%q should parse", src)
		}
	}
}

func TestContestRegexHeadThatIsNotOneAtom(t *testing.T) {
	spec, err := EmitGrammarSpec(ctGrammar(&Element{Kind: KindRegex, Pattern: "a|b"}, ctLit("b")), &ConvertOptions{Tag: "ct", Start: "doc"})
	if err != nil {
		t.Fatal(err)
	}
	if d := ctDepths(t, spec); d[0] != 2 || d[1] != 2 {
		t.Fatalf("depths %v, want [2 2]", d)
	}
	for _, src := range []string{"b,,..", "a..,,"} {
		if !ctParses(t, spec, src, true) {
			t.Errorf("%q should parse", src)
		}
	}
}

func TestContestAnyTokenMeetsEveryHead(t *testing.T) {
	spec, err := EmitGrammarSpec(ctGrammar(&Element{Kind: KindToken, Name: "#AA"}, ctLit("b")), &ConvertOptions{Tag: "ct", Start: "doc"})
	if err != nil {
		t.Fatal(err)
	}
	if d := ctDepths(t, spec); d[0] != 2 || d[1] != 2 {
		t.Fatalf("depths %v, want [2 2]", d)
	}
	for _, src := range []string{"b,,..", "z..,,"} {
		if !ctParses(t, spec, src, false) {
			t.Errorf("%q should parse", src)
		}
	}
}

func TestContestRegexHeadWhoseCoverageIsUnnamed(t *testing.T) {
	// \n, \t and \v are one atom each, but patternCharRanges declines to
	// name what a control escape covers, so nothing says the literal
	// character is not what the pattern matches.
	for _, c := range []struct{ pattern, literal string }{
		{`\n`, "\n"}, {`\t`, "\t"}, {`\v`, "\v"},
	} {
		spec, err := EmitGrammarSpec(ctGrammar(&Element{Kind: KindRegex, Pattern: c.pattern}, ctLit(c.literal)), &ConvertOptions{Tag: "ct", Start: "doc"})
		if err != nil {
			t.Fatalf("%s: %v", c.pattern, err)
		}
		if d := ctDepths(t, spec); d[0] != 2 || d[1] != 2 {
			t.Fatalf("%s: depths %v, want [2 2]", c.pattern, d)
		}
	}
	spec, err := EmitGrammarSpec(ctGrammar(&Element{Kind: KindRegex, Pattern: `\v`}, ctLit("\v")), &ConvertOptions{Tag: "ct", Start: "doc"})
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{"\v..,,", "\v,,.."} {
		if !ctParses(t, spec, src, true) {
			t.Errorf("%q should parse", src)
		}
	}
}

func TestContestCaseInsensitiveHeadBeyondASCII(t *testing.T) {
	// (?i)[Σ] takes ς, and the coverage folds ASCII letters alone.
	spec, err := EmitGrammarSpec(ctGrammar(&Element{Kind: KindRegex, Pattern: "[Σ]", Flags: "i"}, ctLit("ς")), &ConvertOptions{Tag: "ct", Start: "doc"})
	if err != nil {
		t.Fatal(err)
	}
	if d := ctDepths(t, spec); d[0] != 2 || d[1] != 2 {
		t.Fatalf("depths %v, want [2 2]", d)
	}
	for _, src := range []string{"ς..,,", "Σ..,,", "ς,,.."} {
		if !ctParses(t, spec, src, true) {
			t.Errorf("%q should parse", src)
		}
	}
	// Within ASCII the folded coverage stays exact: (?i)[b] meets no a.
	ascii, err := EmitGrammarSpec(ctGrammar(&Element{Kind: KindRegex, Pattern: "[b]", Flags: "i"}, ctLit("a")), &ConvertOptions{Tag: "ct", Start: "doc"})
	if err != nil {
		t.Fatal(err)
	}
	if d := ctDepths(t, ascii); d[0] != 1 || d[1] != 1 {
		t.Fatalf("depths %v, want [1 1]", d)
	}
}

// ctSemi is ctGrammar with tails no engine matcher can take (a number
// would swallow the `.` of `1..`): t = ";" ";" ; u = "!" "!".
func ctSemi(a, b *Element) *Grammar {
	return &Grammar{Productions: []*Production{
		{Name: "doc", Alts: []Sequence{{ctRef("x")}}},
		{Name: "x", Alts: []Sequence{{a, ctRef("t"), ctRef("u")}, {b, ctRef("u"), ctRef("t")}}},
		{Name: "t", Alts: []Sequence{{ctLit(";"), ctLit(";")}}},
		{Name: "u", Alts: []Sequence{{ctLit("!"), ctLit("!")}}},
	}}
}

func TestContestEngineTokenMeetsWhatItsMatcherCanTake(t *testing.T) {
	// Negotiated lexing runs only the matchers that can produce the token
	// an alternative wants: the number matcher takes a leading digit, the
	// string matcher a quote, and the text matcher any text no fixed
	// literal claims. The four-token dispatch this replaced kept each of
	// these pairs apart; a one-token dispatch did not.
	tk := func(n string) *Element { return &Element{Kind: KindToken, Name: n} }
	rx := func(p string) *Element { return &Element{Kind: KindRegex, Pattern: p} }
	for _, c := range []struct {
		a, b *Element
		text string
	}{
		{tk("#NR"), ctLit("1"), "1"},
		{tk("#ST"), ctLit("'a'"), "'a'"},
		{tk("#TX"), ctILit("let"), "let"},
		{tk("#NR"), tk("#TX"), "1"},
		{tk("#NR"), rx("[0-9]"), "1"},
		{tk("#TX"), rx("[a-z]+"), "let"},
	} {
		spec, err := EmitGrammarSpec(ctSemi(c.a, c.b), &ConvertOptions{Tag: "ct", Start: "doc"})
		if err != nil {
			t.Fatal(err)
		}
		if d := ctDepths(t, spec); d[0] != 2 || d[1] != 2 {
			t.Fatalf("%s: depths %v, want [2 2]", c.text, d)
		}
		for _, src := range []string{c.text + ";;!!", c.text + "!!;;"} {
			if !ctParses(t, spec, src, true) {
				t.Errorf("%q should parse", src)
			}
		}
	}
	// The text matcher defers to a fixed literal, and an emitted grammar
	// lexes no values: those stay one token deep.
	for _, c := range []struct{ a, b *Element }{
		{tk("#TX"), ctLit("let")}, {tk("#VL"), ctLit("true")},
	} {
		spec, err := EmitGrammarSpec(ctSemi(c.a, c.b), &ConvertOptions{Tag: "ct", Start: "doc"})
		if err != nil {
			t.Fatal(err)
		}
		if d := ctDepths(t, spec); d[0] != 1 || d[1] != 1 {
			t.Fatalf("%s: depths %v, want [1 1]", c.a.Name, d)
		}
	}
	// TypeScript and Rust also pin that `\u{1}` is inexact without the u
	// flag. Go's regexp has no `\u{...}` escape, so such a pattern never
	// reaches the predicate here.
}

func TestContestEscapeWhoseCodePointCannotBeReadIsNotExact(t *testing.T) {
	// The head scanner and the coverage reader take an escape the same
	// way: one code point they can name, or unknown. `\u1` and `\x1` are
	// `u` then `1` and `x` then `1` in the JavaScript matcher the
	// canonical runtime emits for, and `\cA`, `\p{L}`, `\k` and a digit
	// escape are a control character, a property, a group or a back
	// reference, none of them the letter after the backslash. Go's regexp
	// compiles only some of these, so they are pinned on the two readers.
	// JavaScript's `\u` is not an RE2 escape at all, so even with its full
	// digits it names nothing here (readEscape reads RE2).
	for _, p := range []string{`\u1`, `\x1`, `\u12`, `\uD83D`, `\u{}`, `\u{4g}`, `\x{110000}`,
		`\cA`, `\p{L}`, `\PL`, `\k<a>`, `\1`, `\0`, `\d`, `\u0041`, `\u{1F600}`} {
		if end := regexHeadAtomEnd(p); end != -1 {
			t.Errorf("regexHeadAtomEnd(%q) = %d, want -1", p, end)
		}
		if r := patternCharRanges(p, ""); r != nil {
			t.Errorf("patternCharRanges(%q) = %v, want unknown", p, r)
		}
	}
	for _, p := range []string{`[\p{L}]`, `[a\1]`, `[\u1]`} {
		if r := patternCharRanges(p, ""); r != nil {
			t.Errorf("patternCharRanges(%q) = %v, want unknown", p, r)
		}
	}
	for _, c := range []struct {
		p   string
		end int
		cp  rune
	}{
		{`\x41`, 4, 'A'}, {`\x{41}`, 6, 'A'}, {`\x{1F600}`, 9, 0x1F600}, {`\.`, 2, '.'},
	} {
		if end := regexHeadAtomEnd(c.p); end != c.end {
			t.Errorf("regexHeadAtomEnd(%q) = %d, want %d", c.p, end, c.end)
		}
		if r := patternCharRanges(c.p, ""); len(r) != 1 || r[0] != (charRange{c.cp, c.cp}) {
			t.Errorf("patternCharRanges(%q) = %v, want %U", c.p, r, c.cp)
		}
	}
	// And through the dispatcher, where the regexp compiles: a property
	// class meets a letter it covers.
	spec, err := EmitGrammarSpec(ctSemi(&Element{Kind: KindRegex, Pattern: `[\p{L}]`}, ctLit("é")),
		&ConvertOptions{Tag: "ct", Start: "doc"})
	if err != nil {
		t.Fatal(err)
	}
	if d := ctDepths(t, spec); d[0] != 2 || d[1] != 2 {
		t.Fatalf(`[\p{L}]: depths %v, want [2 2]`, d)
	}
}

func TestContestEscapeIsReadAsRE2ReadsIt(t *testing.T) {
	// This port compiles every matcher with Go's regexp, so an escape is
	// read as RE2 reads it, not as JavaScript does. `\a` is BEL, which a
	// JavaScript matcher reads as the letter `a`; read as `a`, a head `\a`
	// was held apart from a literal BEL that it takes, and the literal's
	// branch was never reached. The zero-width `\A` and `\z`, the quoting
	// `\Q…\E`, `\C` and letters RE2 does not define name no one code point.
	for _, c := range []struct {
		p   string
		end int
		cp  rune
	}{
		{`\a`, 2, 0x07}, {`\x07`, 4, 0x07}, {`\_`, 2, '_'}, {`\-`, 2, '-'},
	} {
		if end := regexHeadAtomEnd(c.p); end != c.end {
			t.Errorf("regexHeadAtomEnd(%q) = %d, want %d", c.p, end, c.end)
		}
		if r := patternCharRanges(c.p, ""); len(r) != 1 || r[0] != (charRange{c.cp, c.cp}) {
			t.Errorf("patternCharRanges(%q) = %v, want %U", c.p, r, c.cp)
		}
	}
	if r := patternCharRanges(`[\a-\x{0d}]`, ""); len(r) != 1 || r[0] != (charRange{0x07, 0x0D}) {
		t.Errorf(`patternCharRanges([\a-\x{0d}]) = %v, want U+0007-U+000D`, r)
	}
	for _, p := range []string{`\A`, `\z`, `\Qa\E`, `\Q+\E`, `\C`, `\E`, `\e`, `\U00000041`} {
		if end := regexHeadAtomEnd(p); end != -1 {
			t.Errorf("regexHeadAtomEnd(%q) = %d, want -1", p, end)
		}
		if r := patternCharRanges(p, ""); r != nil {
			t.Errorf("patternCharRanges(%q) = %v, want unknown", p, r)
		}
	}
	// Through the dispatcher: each of these heads can take the input the
	// literal takes, so the choice looks two tokens deep and both branches
	// are reached.
	for _, c := range []struct{ pattern, text string }{
		{`\a`, "\a"}, {`[\a]`, "\a"}, {`\a+`, "\a"}, {`\Q+\E`, "+"},
	} {
		spec, err := EmitGrammarSpec(ctSemi(&Element{Kind: KindRegex, Pattern: c.pattern}, ctLit(c.text)),
			&ConvertOptions{Tag: "ct", Start: "doc"})
		if err != nil {
			t.Fatal(err)
		}
		if d := ctDepths(t, spec); d[0] != 2 || d[1] != 2 {
			t.Errorf("%s: depths %v, want [2 2]", c.pattern, d)
		}
		for _, src := range []string{c.text + ";;!!", c.text + "!!;;"} {
			if !ctParses(t, spec, src, true) {
				t.Errorf("%s: %q should parse", c.pattern, src)
			}
		}
	}
	// A range ending on a hex escape that names a surrogate is read as RE2
	// reads it, and laid over the partition beside the class it overlaps.
	// (The TypeScript reader once named no surrogate escape at all, and
	// lost the second branch to the lexer.)
	spec, err := EmitGrammarSpec(ctSemi(&Element{Kind: KindRegex, Pattern: `[\x{0041}-\x{d800}]`},
		&Element{Kind: KindRegex, Pattern: `[\x{0041}-\x{005a}]`}), &ConvertOptions{Tag: "ct", Start: "doc"})
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{"A;;!!", "A!!;;", "b;;!!"} {
		if !ctParses(t, spec, src, false) {
			t.Errorf("%q should parse", src)
		}
	}
}

// TestContestCodeUnitLeadSurrogateMeetsAstralHead pins that a head the
// canonical matcher reads in code units (no `u` or `v`) and that names a
// lead surrogate meets every astral character that surrogate begins:
// there it takes U+D800 as the first half of U+10000. Compared as code
// points the heads were disjoint, the decision stayed one token deep, and
// the first branch took the input the second accepts. RE2 never meets a
// surrogate, but this port decides alike, so the three ports emit the
// same grammar (tabnas/bnf#75 review). The TS test's lone-surrogate
// literal has no Go spelling: a Go string cannot hold one.
func TestContestCodeUnitLeadSurrogateMeetsAstralHead(t *testing.T) {
	rx := func(p, f string) *Element { return &Element{Kind: KindRegex, Pattern: p, Flags: f} }
	astral := rx(`\x{10000}`, "u")
	emit := func(g *Grammar) *tabnas.GrammarSpec {
		spec, err := EmitGrammarSpec(g, &ConvertOptions{Tag: "ct", Start: "doc"})
		if err != nil {
			t.Fatal(err)
		}
		return spec
	}
	for _, c := range []struct {
		label string
		a, b  *Element
	}{
		{"escape", rx(`\x{d800}`, ""), astral},
		{"class", rx(`[\x{d800}-\x{dbff}]`, ""), rx(`[\x{10000}-\x{10ffff}]`, "u")},
		{"astral literal", rx(`\x{d800}`, ""), ctLit("\U00010000")},
	} {
		spec := emit(ctSemi(c.a, c.b))
		if d := ctDepths(t, spec); d[0] != 2 || d[1] != 2 {
			t.Errorf("%s: depths %v, want [2 2]", c.label, d)
		}
		if !ctParses(t, spec, "\U00010000!!;;", true) {
			t.Errorf("%s: U+10000 then !!;; should parse", c.label)
		}
	}
	// Read in code points a lead surrogate is only ever one standing
	// alone, and meets no astral character, whether the class keeps its
	// own matcher or is laid over the partition, whose atoms read as the
	// classes they stand for.
	lone := rx(`[\x{d800}]`, "u")
	contested := ctSemi(lone, astral)
	contested.Productions[0].Alts = append(contested.Productions[0].Alts,
		Sequence{rx(`[\x{d800}-\x{dbff}]`, "u")})
	for label, g := range map[string]*Grammar{"alone": ctSemi(lone, astral), "partitioned": contested} {
		spec := emit(g)
		if partitioned := len(spec.Options.TokenSet) > 0; partitioned != (label == "partitioned") {
			t.Fatalf("%s: token sets %v", label, spec.Options.TokenSet)
		}
		if d := ctDepths(t, spec); d[0] != 1 || d[1] != 1 {
			t.Errorf("%s: depths %v, want [1 1]", label, d)
		}
	}
}

// TestContestCodeUnitClassTakesAnAstralCharacterAsTwo pins that a class
// the canonical matcher reads in code units (no `u` or `v`) holds an
// astral character written in it as its lead and trail surrogates, so
// `[😀]` meets a `😁` head through the lead. RE2 reads the code point, but
// this port adds the units beside it, so the three ports emit the same
// grammar (tabnas/bnf#75 review). Mirrors the TS test.
func TestContestCodeUnitClassTakesAnAstralCharacterAsTwo(t *testing.T) {
	rx := func(p, f string) *Element { return &Element{Kind: KindRegex, Pattern: p, Flags: f} }
	emit := func(g *Grammar) *tabnas.GrammarSpec {
		spec, err := EmitGrammarSpec(g, &ConvertOptions{Tag: "ct", Start: "doc"})
		if err != nil {
			t.Fatal(err)
		}
		return spec
	}
	for _, p := range []string{"[\U0001F600]", "[b\U0001F600]"} {
		spec := emit(ctSemi(rx(p, ""), ctLit("\U0001F601")))
		if d := ctDepths(t, spec); d[0] != 2 || d[1] != 2 {
			t.Errorf("%s: depths %v, want [2 2]", p, d)
		}
		if !ctParses(t, spec, "\U0001F601!!;;", true) {
			t.Errorf("%s: U+1F601 then !!;; should parse", p)
		}
	}
	for _, c := range [][2]string{{"[\U0001F600]", "u"}, {"\U0001F600", ""}} {
		spec := emit(ctSemi(rx(c[0], c[1]), ctLit("\U0001F601")))
		if d := ctDepths(t, spec); d[0] != 1 || d[1] != 1 {
			t.Errorf("%s %s: depths %v, want [1 1]", c[0], c[1], d)
		}
	}
	if r := patternCharRanges("[\U0001F600]", ""); len(r) != 3 {
		t.Errorf("[😀] in code units: %v, want U+1F600 and its two units", r)
	}
	if r := patternCharRanges("[\U0001F600]", "u"); len(r) != 1 {
		t.Errorf("[😀] under u: %v, want U+1F600 alone", r)
	}
}
