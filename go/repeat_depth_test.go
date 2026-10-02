// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

package bnf

// Every repetition compiles to a same-depth replace loop (AGENTS.md,
// "Repetition is replacement, never a push chain"). Mirrors
// ts/test/repeat-depth.test.js.
//
// A star, a plus and an unbounded rep are SEQUENCE, so the loop they
// desugar to runs every iteration in one frame: the item may be pushed,
// but the next iteration replaces the loop with itself (`r:`) rather than
// pushing a fresh copy. Rule depth is then bounded by the grammar's
// nesting, never by the input's length. The helper used to be emitted as
// the right recursion it is written as in the IR (`H = item H / ε`, each
// item pushing a new `H`), so a flat file of a few thousand records cost a
// frame per record, tripped the hosts' depth guards, and parsed in
// quadratic time.
//
// The proof is observed, not inferred from the spec: the engine reports
// the depth `D` of every rule it runs (SubRuleDone), and the deepest one
// over ten thousand items must be exactly the deepest one over a single
// iteration. Time is the second observable: ten times the items must cost
// about ten times as long.

import (
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	tabnas "github.com/tabnas/parser/go"
)

const rdN = 10000

func rdLit(s string) *Element {
	return &Element{Kind: KindTerm, Literal: s, CaseSensitive: true, HasCaseSens: true}
}
func rdRx(pattern string) *Element      { return &Element{Kind: KindRegex, Pattern: pattern} }
func rdStar(inner *Element) *Element    { return &Element{Kind: KindStar, Inner: inner} }
func rdPlus(inner *Element) *Element    { return &Element{Kind: KindPlus, Inner: inner} }
func rdOpt(inner *Element) *Element     { return &Element{Kind: KindOpt, Inner: inner} }
func rdGroup(alts ...Sequence) *Element { return &Element{Kind: KindGroup, Alts: alts} }
func rdRep(min, max int, inner *Element) *Element {
	return &Element{Kind: KindRep, Min: min, Max: max, Inner: inner}
}
func rdProd(name string, alts ...Sequence) *Production {
	return &Production{Name: name, Alts: alts}
}

// rdParser is a parser for the grammar, and a way to read the deepest
// rule it ran.
type rdParser struct {
	spec    *tabnas.GrammarSpec
	j       *tabnas.Tabnas
	deepest int
}

type rdOpts struct{ builtins, recognition bool }

func newRdParser(t *testing.T, prods []*Production, o rdOpts) *rdParser {
	t.Helper()
	spec, err := EmitGrammarSpec(&Grammar{Productions: prods},
		&ConvertOptions{Tag: "depth", Start: prods[0].Name, Builtins: o.builtins})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	gs := spec
	if o.recognition {
		data, err := ToRecognitionSpec(spec)
		if err != nil {
			t.Fatalf("recognition: %v", err)
		}
		if gs, err = tabnas.GrammarSpecFromJSON([]byte(ToJsonic(data, true, 0))); err != nil {
			t.Fatalf("loading the recognition grammar: %v", err)
		}
	}
	p := &rdParser{spec: spec, j: tabnas.Make()}
	if err := p.j.Grammar(gs); err != nil {
		t.Fatalf("install: %v", err)
	}
	p.j.SubRuleDone(func(r *tabnas.Rule, _ *tabnas.Context, _ tabnas.RuleDone) {
		if p.deepest < r.D {
			p.deepest = r.D
		}
	})
	return p
}

// parse returns the value and the deepest rule depth the parse reached.
func (p *rdParser) parse(t *testing.T, src string) (any, int) {
	t.Helper()
	p.deepest = 0
	out, err := p.j.Parse(src)
	if err != nil {
		t.Fatalf("parse of %d bytes: %v", len(src), err)
	}
	return tabnas.UnwrapUndefined(out), p.deepest
}

func rdNode(t *testing.T, v any) map[string]any {
	t.Helper()
	n, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("not a tree node: %T %v", v, v)
	}
	return n
}

func rdKids(t *testing.T, v any) []any {
	t.Helper()
	kids, _ := rdNode(t, v)["kids"].([]any)
	return kids
}

func rdSrc(t *testing.T, v any) string {
	t.Helper()
	s, _ := rdNode(t, v)["src"].(string)
	return s
}

func rdTimes(n int, s, sep string) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = s
	}
	return strings.Join(parts, sep)
}

// rdSample is how much time one timing sample runs for. A clock can be
// coarse (some platforms advance it in steps of about 15.6 ms), so a
// single parse of a thousand items can read as nothing at all: each sample
// repeats the parse until it has run this long, and reports the cost of
// one.
const rdSample = 150 * time.Millisecond

// rdFastest is the cheapest per-call cost over a few samples, so one slow
// sample (a GC, another process) does not decide the verdict.
func rdFastest(fn func(), runs int) time.Duration {
	best := time.Duration(1<<63 - 1)
	for i := 0; i < runs; i++ {
		n := 0
		t0 := time.Now()
		var d time.Duration
		for d < rdSample && n < 10000 {
			fn()
			n++
			d = time.Since(t0)
		}
		if per := d / time.Duration(n); per < best {
			best = per
		}
	}
	return best
}

// rdAssertLinear: ten times the items cost ten times as long, within a
// small factor. The factor is the TypeScript suite's, so the runtimes hold
// the compiler to one bound; a parse quadratic in the item count costs a
// hundred times as long. A measurement over the bound is taken again, up
// to three times, before it counts: a quadratic parse is over it every
// time, a machine that was busy for a moment is not.
func rdAssertLinear(t *testing.T, p *rdParser, make func(int) string, label string) {
	t.Helper()
	small, large := make(rdN/10), make(rdN)
	for i := 0; i < 3; i++ {
		p.parse(t, small)
	}
	p.parse(t, large) // warm both sizes before timing either
	seen := ""
	for attempt := 0; attempt < 3; attempt++ {
		tSmall := rdFastest(func() { p.parse(t, small) }, 3)
		tLarge := rdFastest(func() { p.parse(t, large) }, 2)
		if tLarge < 50*tSmall {
			t.Logf("%s: %.1fx (%v against %v per parse)", label,
				float64(tLarge)/float64(tSmall), tLarge, tSmall)
			return
		}
		seen += fmt.Sprintf(" %.1fx (%v against %v per parse);",
			float64(tLarge)/float64(tSmall), tLarge, tSmall)
	}
	t.Errorf("%s: %d items against %d took%s not linear", label, rdN, rdN/10, seen)
}

type rdCase struct {
	name    string
	grammar func() []*Production
	make    func(n int) string
	// The smallest n that runs one full iteration of every repetition in it.
	one   int
	check func(t *testing.T, out any, n int)
}

func rdItem() *Production { return rdProd("item", Sequence{rdRx("[a-z]+")}) }

// rdPairGrammar is `doc = *pair`, `pair = key "=" val ";"` building an
// object of its two members, each of which is a star.
func rdPairGrammar() []*Production {
	return []*Production{
		rdProd("doc", Sequence{rdStar(ref("pair"))}),
		{Name: "pair", Value: &ValueAnnotation{Kind: "object", Members: []string{"key", "val"}},
			Alts: []Sequence{{ref("key"), rdLit("="), ref("val"), rdLit(";")}}},
		rdProd("key", Sequence{rdStar(rdRx("[a-z]"))}),
		rdProd("val", Sequence{rdStar(rdRx("[0-9]"))}),
	}
}

func rdCases() []rdCase {
	kidsN := func(t *testing.T, out any, n int) {
		t.Helper()
		if got := len(rdKids(t, out)); got != n {
			t.Errorf("%d kids, want %d", got, n)
		}
	}
	spaced := func(n int) string { return rdTimes(n, "ab", " ") }
	return []rdCase{
		{
			name:    "*item",
			grammar: func() []*Production { return []*Production{rdProd("doc", Sequence{rdStar(ref("item"))}), rdItem()} },
			make:    spaced, one: 1, check: kidsN,
		},
		{
			name:    "1*item",
			grammar: func() []*Production { return []*Production{rdProd("doc", Sequence{rdPlus(ref("item"))}), rdItem()} },
			make:    spaced, one: 2, check: kidsN,
		},
		{
			name: "3*item",
			grammar: func() []*Production {
				return []*Production{rdProd("doc", Sequence{rdRep(3, MaxInfinity, ref("item"))}), rdItem()}
			},
			make: spaced, one: 4, check: kidsN,
		},
		{
			name:    `*"x" (a terminal item)`,
			grammar: func() []*Production { return []*Production{rdProd("doc", Sequence{rdStar(rdLit("x")), rdLit(";")})} },
			make:    func(n int) string { return rdTimes(n, "x", "") + ";" },
			one:     1,
			check: func(t *testing.T, out any, n int) {
				if got := len(rdSrc(t, out)); got != n+1 {
					t.Errorf("src length %d, want %d", got, n+1)
				}
			},
		},
		{
			name: `item *("," item)`,
			grammar: func() []*Production {
				return []*Production{rdProd("doc", Sequence{ref("item"),
					rdStar(rdGroup(Sequence{rdLit(","), ref("item")}))}), rdItem()}
			},
			make: func(n int) string { return rdTimes(n, "ab", ",") },
			one:  2,
			check: func(t *testing.T, out any, n int) {
				kidsN(t, out, n-1)
			},
		},
		{
			name: `*( item ";" / "!" ) (a star of a group)`,
			grammar: func() []*Production {
				return []*Production{rdProd("doc", Sequence{rdStar(rdGroup(
					Sequence{ref("item"), rdLit(";")}, Sequence{rdLit("!")}))}), rdItem()}
			},
			make: func(n int) string { return rdTimes(n, "ab;", "") }, one: 1, check: kidsN,
		},
		{
			name: `*row, row = "[" *cell "]" (a star inside the item)`,
			grammar: func() []*Production {
				return []*Production{
					rdProd("doc", Sequence{rdStar(ref("row"))}),
					rdProd("row", Sequence{rdLit("["), rdStar(ref("cell")), rdLit("]")}),
					rdProd("cell", Sequence{rdRx("[a-z]")}),
				}
			},
			make: func(n int) string { return rdTimes(n, "[abc]", "") },
			one:  1,
			check: func(t *testing.T, out any, n int) {
				kids := rdKids(t, out)
				kidsN(t, out, n)
				if got := len(rdKids(t, kids[n-1])); got != 3 {
					t.Errorf("the last row has %d cells, want 3", got)
				}
			},
		},
		{
			name: `row = "[" *cell "]" (ten thousand cells in one row)`,
			grammar: func() []*Production {
				return []*Production{
					rdProd("row", Sequence{rdLit("["), rdStar(ref("cell")), rdLit("]")}),
					rdProd("cell", Sequence{rdRx("[a-z]")}),
				}
			},
			make: func(n int) string { return "[" + rdTimes(n, "a", "") + "]" }, one: 1, check: kidsN,
		},
		{
			name: "doc = *a *b (two sibling stars)",
			grammar: func() []*Production {
				return []*Production{
					rdProd("doc", Sequence{rdStar(ref("a")), rdStar(ref("b"))}),
					rdProd("a", Sequence{rdLit("a")}, Sequence{rdLit("A")}),
					rdProd("b", Sequence{rdLit("b")}, Sequence{rdLit("B")}),
				}
			},
			make: func(n int) string { return rdTimes(n, "a", "") + rdTimes(n, "b", "") },
			one:  1,
			check: func(t *testing.T, out any, n int) {
				kidsN(t, out, 2*n)
				if got := rdSrc(t, rdKids(t, out)[n]); got != "b" {
					t.Errorf("kid %d is %q, want b", n, got)
				}
			},
		},
		{
			name: `*( "(" *item ")" ) (a star directly inside a star's group)`,
			grammar: func() []*Production {
				return []*Production{rdProd("doc", Sequence{rdStar(rdGroup(
					Sequence{rdLit("("), rdStar(ref("item")), rdLit(")")}))}), rdItem()}
			},
			make: func(n int) string { return rdTimes(n, "(ab cd)", "") },
			one:  1,
			check: func(t *testing.T, out any, n int) {
				kidsN(t, out, 2*n)
			},
		},
		{
			// The item builds a value, and each of its members is a star: the
			// loops over `key`'s letters and `val`'s digits run inside an item
			// of the loop over `pair`, side by side. Each allocates the node it
			// accumulates into on its own way in, which is what the member
			// reads back; a loop that took its pusher's instead would write its
			// letters into the object (see TestRepeatLoopEntersAfresh).
			name:    `*pair, pair = key "=" val ";" ; @object (stars in an item that builds a value)`,
			grammar: rdPairGrammar,
			make:    func(n int) string { return rdTimes(n, "ab=12;", "") },
			one:     1,
			check: func(t *testing.T, out any, n int) {
				kids := rdKids(t, out)
				kidsN(t, out, n)
				if !valueEquals(kids[n-1], map[string]any{"key": "ab", "val": "12"}) {
					t.Errorf("the last pair is %#v", kids[n-1])
				}
			},
		},
		{
			// Left recursion is rewritten to `seed tail*`: the star it
			// synthesises is a loop like any other, and the tree stays flat.
			name: `sum = sum "+" item / item (left recursion)`,
			grammar: func() []*Production {
				return []*Production{rdProd("sum",
					Sequence{ref("sum"), rdLit("+"), ref("item")}, Sequence{ref("item")}), rdItem()}
			},
			make: func(n int) string { return rdTimes(n, "ab", "+") },
			one:  2,
			check: func(t *testing.T, out any, n int) {
				if got := rdNode(t, out)["rule"]; got != "sum" {
					t.Errorf("rule %v, want sum", got)
				}
				kidsN(t, out, n-1)
			},
		},
	}
}

func TestRepeatDepthIsOneItems(t *testing.T) {
	for _, c := range rdCases() {
		for _, builtins := range []bool{false, true} {
			mode := "closures"
			if builtins {
				mode = "builtins"
			}
			t.Run(fmt.Sprintf("%s (%s)", c.name, mode), func(t *testing.T) {
				p := newRdParser(t, c.grammar(), rdOpts{builtins: builtins})
				_, one := p.parse(t, c.make(c.one))
				out, many := p.parse(t, c.make(rdN))
				c.check(t, out, rdN)
				if many != one {
					t.Errorf("%d items reached depth %d; one iteration needs %d",
						rdN, many, one)
				}
			})
		}
	}
}

// TestRepeatDepthTimeIsLinear holds a repetition to linear time where this
// package decides it, and to constant depth everywhere.
//
// Time over a repetition is the depth of the loop plus the cost of growing
// the text of the node it accumulates into. The first is this package's
// and is constant. In closure mode this package's own tree builders
// (refRegistry.node, capture and fold) append in place through srcAcc. In
// builtins mode the engine's `@node$`, `@capture$` and `@fold$` do the same
// since parser v0.12.8. Both modes are therefore held to the same linear
// bound, as TypeScript and Rust are.
func TestRepeatDepthTimeIsLinear(t *testing.T) {
	for _, c := range rdCases() {
		t.Run(c.name+" (closures)", func(t *testing.T) {
			rdAssertLinear(t, newRdParser(t, c.grammar(), rdOpts{}), c.make, c.name+" (closures)")
		})
		t.Run(c.name+" (builtins)", func(t *testing.T) {
			rdAssertLinear(t, newRdParser(t, c.grammar(), rdOpts{builtins: true}), c.make, c.name+" (builtins)")
		})
	}
}

func TestRepeatDepthHoldsInARecognitionGrammar(t *testing.T) {
	// The loop's back-edge and its `rep` counter are structural, so a spec
	// stripped of every tree builder still loops in one frame.
	g := []*Production{rdProd("doc", Sequence{ref("item"),
		rdStar(rdGroup(Sequence{rdLit(","), ref("item")}))}), rdItem()}
	p := newRdParser(t, g, rdOpts{builtins: true, recognition: true})
	_, one := p.parse(t, rdTimes(2, "ab", ","))
	_, many := p.parse(t, rdTimes(rdN, "ab", ","))
	if many != one {
		t.Errorf("recognition: %d items reached depth %d; one iteration needs %d",
			rdN, many, one)
	}
}

func TestRepeatDepthCollectsOneElementPerItem(t *testing.T) {
	// `list = "(" *( item ";" ) ")"  ; @array` — the array-collection
	// planner hands the loop the array it inherits, and each iteration
	// pushes one element into it.
	g := func() []*Production {
		return []*Production{
			{Name: "list", Value: &ValueAnnotation{Kind: "array"}, Alts: []Sequence{{
				rdLit("("), rdStar(rdGroup(Sequence{ref("item"), rdLit(";")})), rdLit(")")}}},
			rdItem(),
		}
	}
	for _, builtins := range []bool{false, true} {
		p := newRdParser(t, g(), rdOpts{builtins: builtins})
		out, one := p.parse(t, "(ab;)")
		if !valueEquals(out, []any{"ab"}) {
			t.Errorf("builtins=%v: (ab;) built %#v", builtins, out)
		}
		out, many := p.parse(t, "("+rdTimes(rdN, "ab;", "")+")")
		arr, ok := out.([]any)
		if !ok || len(arr) != rdN {
			t.Fatalf("builtins=%v: %d items built %T of length %d", builtins, rdN, out, len(arr))
		}
		for i, e := range arr {
			if e != "ab" {
				t.Fatalf("builtins=%v: element %d is %#v", builtins, i, e)
			}
		}
		if many != one {
			t.Errorf("builtins=%v: depth %d over %d items, %d over one", builtins, many, rdN, one)
		}
	}
}

func TestRepeatDepthTakesARepetitionMemberAsItsText(t *testing.T) {
	// An object member that is a star is the run's source text: the loop
	// accumulates into the node its entry allocated, which is what the
	// member reads back, not the object it sits in.
	g := func() []*Production {
		return []*Production{
			{Name: "obj", Value: &ValueAnnotation{Kind: "object", Members: []string{"ds", "e"}},
				Alts: []Sequence{{rdLit("<"), rdStar(ref("d")), rdLit(">"), ref("e")}}},
			rdProd("d", Sequence{rdRx("[0-9]")}),
			rdProd("e", Sequence{rdLit("!"), rdLit("?")}),
		}
	}
	for _, builtins := range []bool{false, true} {
		p := newRdParser(t, g(), rdOpts{builtins: builtins})
		if out, _ := p.parse(t, "<>!?"); !valueEquals(out, map[string]any{"ds": "", "e": "!?"}) {
			t.Errorf("builtins=%v: <>!? built %#v", builtins, out)
		}
		out, one := p.parse(t, "<7>!?")
		if !valueEquals(out, map[string]any{"ds": "7", "e": "!?"}) {
			t.Errorf("builtins=%v: <7>!? built %#v", builtins, out)
		}
		out, many := p.parse(t, "<"+rdTimes(rdN, "7", "")+">!?")
		if !valueEquals(out, map[string]any{"ds": rdTimes(rdN, "7", ""), "e": "!?"}) {
			t.Errorf("builtins=%v: %d digits did not come back as the member's text", builtins, rdN)
		}
		if many != one {
			t.Errorf("builtins=%v: depth %d over %d items, %d over one", builtins, many, rdN, one)
		}
	}
}

func TestRepeatDepthEndsAStarOfSomethingThatCanMatchNothing(t *testing.T) {
	// `*[x]` is `*x`: an iteration whose item matched nothing makes no
	// progress, so the loop does not take one. The push chain took it
	// forever, and failed when the engine's step budget ran out.
	p := newRdParser(t, []*Production{rdProd("doc", Sequence{rdStar(rdOpt(rdLit("x"))), rdLit(";")})}, rdOpts{})
	for _, src := range []string{";", "xxx;"} {
		if out, _ := p.parse(t, src); rdSrc(t, out) != src {
			t.Errorf("%q came back as %q", src, rdSrc(t, out))
		}
	}
	_, many := p.parse(t, rdTimes(rdN, "x", "")+";")
	_, one := p.parse(t, "x;")
	if many != one {
		t.Errorf("depth %d over %d items, %d over one", many, rdN, one)
	}
}

func rdAlts(rs *tabnas.GrammarRuleSpec) []*tabnas.GrammarAltSpec {
	if rs == nil {
		return nil
	}
	return append(altListOf(rs.Open), altListOf(rs.Close)...)
}

func rdNodeCfg(t *testing.T, a *tabnas.GrammarAltSpec) map[string]any {
	t.Helper()
	cfg, _ := a.K["node$"].(map[string]any)
	if cfg == nil {
		t.Fatalf("no node$ config on %#v", a)
	}
	return cfg
}

func TestRepeatDepthEmitsTheLoopAsAReplace(t *testing.T) {
	// The shape, for `doc = *item`. The loop's first alternative is its
	// entry: on the way in (counter `rep` still 0) it allocates the node and
	// re-enters the loop, counted. Every other alternative decides continue
	// or exit as the right-recursive helper always did, but continuing
	// hands over to the iteration by replacement: `$alt0` pushes the item
	// (clearing the counter for whatever the item holds) and replaces
	// itself, counted again, with `$step1`, which replaces itself with the
	// loop. The rule names are the ones the helper always compiled to.
	p := newRdParser(t, []*Production{rdProd("doc", Sequence{rdStar(ref("item"))}), rdItem()},
		rdOpts{builtins: true})
	spec := p.spec
	loopRe := regexp.MustCompile(`^_gen\d+_star_item$`)
	loop := ""
	names := []string{}
	for name := range spec.Rule {
		names = append(names, name)
		if loopRe.MatchString(name) {
			loop = name
		}
	}
	sort.Strings(names)
	if loop == "" {
		t.Fatalf("no loop among %v", names)
	}
	iter := loop + "$alt0"
	step := iter + "$step1"
	want := []string{"__start__", loop, iter, step, "doc", "item"}
	sort.Strings(want)
	if !sameStrings(names, want) {
		t.Errorf("rules %v, want %v", names, want)
	}

	if docs := altListOf(spec.Rule["doc"].Open); len(docs) != 1 || docs[0].P != loop {
		t.Errorf("doc must push the loop, got %#v", docs)
	}

	opens := altListOf(spec.Rule[loop].Open)
	entry, open := opens[0], opens[1:]
	if cd, _ := entry.C.(map[string]any); len(cd) != 1 || cd["n.rep"] != 0 {
		t.Errorf("entry condition %#v, want {n.rep: 0}", entry.C)
	}
	if len(entry.N) != 1 || entry.N["rep"] != 1 {
		t.Errorf("entry counts %#v, want {rep: 1}", entry.N)
	}
	if entry.R != loop || entry.P != "" {
		t.Errorf("entry must replace with the loop, got r=%q p=%q", entry.R, entry.P)
	}
	if entry.A != "@node$" || rdNodeCfg(t, entry)["init"] != true {
		t.Errorf("entry must allocate the node, got a=%v k=%v", entry.A, entry.K)
	}
	takes := 0
	for _, a := range open {
		if a.R == "" {
			if a.P != "" {
				t.Errorf("an exit takes no item, got p=%q", a.P)
			}
			if a.A != nil {
				t.Errorf("an exit allocates nothing, got a=%v", a.A)
			}
			continue
		}
		takes++
		if a.R != iter {
			t.Errorf("continuing hands over to the iteration, got r=%q", a.R)
		}
		if a.P != "" || a.C != nil || a.A != nil {
			t.Errorf("an iteration pushes, tests and allocates nothing: %#v", a)
		}
	}
	if takes == 0 {
		t.Error("the loop has no continue alternative")
	}
	if spec.Rule[loop].Close != nil && len(altListOf(spec.Rule[loop].Close)) > 0 {
		t.Errorf("the loop has a close: %#v", spec.Rule[loop].Close)
	}

	iopen := altListOf(spec.Rule[iter].Open)
	if len(iopen) != 1 || iopen[0].P != "item" || len(iopen[0].N) != 1 || iopen[0].N["rep"] != 0 {
		t.Errorf("the iteration must push the item with rep cleared, got %#v", iopen)
	}
	iclose := altListOf(spec.Rule[iter].Close)
	if len(iclose) != 1 || iclose[0].R != step || iclose[0].N["rep"] != 1 || iclose[0].A != "@capture$" {
		t.Errorf("the iteration must capture and replace with its step, got %#v", iclose)
	}
	sopen := altListOf(spec.Rule[step].Open)
	if len(sopen) != 1 || sopen[0].R != loop {
		t.Errorf("the step must replace with the loop, got %#v", sopen)
	}
	if len(altListOf(spec.Rule[step].Close)) > 0 {
		t.Errorf("the step has a close: %#v", spec.Rule[step].Close)
	}

	// Nothing but the enclosing rule pushes the loop, and nothing pushes
	// the iteration or its step.
	for name, rs := range spec.Rule {
		for _, a := range rdAlts(rs) {
			if name != "doc" && a.P == loop {
				t.Errorf("%s pushes the loop", name)
			}
			if a.P == iter || a.P == step {
				t.Errorf("%s pushes %s", name, a.P)
			}
		}
	}
}

func TestRepeatDepthMatchesATerminalItemInOneAlternative(t *testing.T) {
	// `*"x"` needs no iteration rule: the loop is the one rule the helper
	// always was, and its continue alternative matches the item and
	// replaces the loop with itself.
	p := newRdParser(t, []*Production{rdProd("doc", Sequence{rdStar(rdLit("x")), rdLit(";")})},
		rdOpts{builtins: true})
	spec := p.spec
	gen := []string{}
	for name := range spec.Rule {
		if strings.HasPrefix(name, "_gen") {
			gen = append(gen, name)
		}
	}
	if len(gen) != 1 || !strings.Contains(gen[0], "_star_") {
		t.Fatalf("generated rules %v, want the loop alone", gen)
	}
	loop := gen[0]
	opens := altListOf(spec.Rule[loop].Open)
	if cd, _ := opens[0].C.(map[string]any); len(cd) != 1 || cd["n.rep"] != 0 {
		t.Errorf("entry condition %#v, want {n.rep: 0}", opens[0].C)
	}
	takes := []*tabnas.GrammarAltSpec{}
	for _, a := range opens[1:] {
		if a.R != "" {
			takes = append(takes, a)
		}
	}
	if len(takes) != 1 || takes[0].R != loop {
		t.Fatalf("continue alternatives %#v, want one replacing with %s", takes, loop)
	}
	cfg := rdNodeCfg(t, takes[0])
	if cfg["init"] != false || cfg["nterms"] != 1 {
		t.Errorf("the take must accumulate one term into the loop's node, got %v", cfg)
	}
	if len(altListOf(spec.Rule[loop].Close)) > 0 {
		t.Errorf("the loop has a close: %#v", spec.Rule[loop].Close)
	}
}

func TestRepeatDepthEntersALoopAfreshInsideAnItemOfTheSameLoop(t *testing.T) {
	// `v = "[" *v "]" / "x"`: the loop over `v` is reached again from
	// inside one of its own items. The counter the outer loop holds at 1 is
	// cleared by the push of the item, so the inner loop allocates a node
	// of its own and the trees nest as the brackets do.
	p := newRdParser(t, []*Production{rdProd("v",
		Sequence{rdLit("["), rdStar(ref("v")), rdLit("]")}, Sequence{rdLit("x")})}, rdOpts{})
	out, _ := p.parse(t, "[x[xx]x]")
	if got := rdSrc(t, out); got != "[x[xx]x]" {
		t.Errorf("src %q", got)
	}
	srcs := func(v any) []string {
		out := []string{}
		for _, k := range rdKids(t, v) {
			out = append(out, rdSrc(t, k))
		}
		return out
	}
	if got := srcs(out); !sameStrings(got, []string{"x", "[xx]", "x"}) {
		t.Errorf("kids %v", got)
	}
	if got := srcs(rdKids(t, out)[1]); !sameStrings(got, []string{"x", "x"}) {
		t.Errorf("inner kids %v", got)
	}
}

// Every loop allocates the node its iterations accumulate into once, on
// the way in, and it is a node of its own rather than the node of the rule
// that pushed it. The `rep` counter is what tells the way in from the way
// back, and counters are INHERITED: a pushed rule and a replacing rule both
// start with a copy of their predecessor's. So a loop reached inside an
// item of another loop, beside another loop, or inside a loop's group could
// see the 1 its neighbour set, skip its entry and write into whatever node
// it was pushed with. The push of an item clears the counter, which is what
// prevents that; these watch the engine to show it does, from every place a
// loop can be reached. Mirrors "the loop counter" in
// ts/test/repeat-depth.test.js.

type rdEntryCase struct {
	name    string
	grammar func() []*Production
	src     string
	value   any
}

func rdEntryCases() []rdEntryCase {
	cell := func() *Production { return rdProd("cell", Sequence{rdRx("[a-z]")}) }
	return []rdEntryCase{
		{
			name: "a star inside the item of a star",
			grammar: func() []*Production {
				return []*Production{rdProd("doc", Sequence{rdStar(ref("row"))}),
					rdProd("row", Sequence{rdLit("["), rdStar(ref("cell")), rdLit("]")}), cell()}
			},
			src: "[ab][][c]",
		},
		{
			name: "two sibling stars",
			grammar: func() []*Production {
				return []*Production{rdProd("doc",
					Sequence{rdStar(ref("cell")), rdLit(";"), rdStar(ref("cell"))}), cell()}
			},
			src: "ab;cd",
		},
		{
			name: "two sibling stars inside the item of a star",
			grammar: func() []*Production {
				return []*Production{rdProd("doc", Sequence{rdStar(ref("row"))}),
					rdProd("row", Sequence{rdLit("["), rdStar(ref("cell")), rdLit(";"),
						rdStar(ref("cell")), rdLit("]")}),
					cell()}
			},
			src: "[ab;cd][;][a;]",
		},
		{
			name: "a star directly inside a star's group",
			grammar: func() []*Production {
				return []*Production{rdProd("doc", Sequence{rdStar(rdGroup(
					Sequence{rdLit("("), rdStar(ref("cell")), rdLit(")")}))}), cell()}
			},
			src: "(ab)()(c)",
		},
		{
			name: "three stars, each in the group of the one outside it",
			grammar: func() []*Production {
				return []*Production{rdProd("doc", Sequence{rdStar(rdGroup(Sequence{rdLit("["),
					rdStar(rdGroup(Sequence{rdLit("("), rdStar(ref("cell")), rdLit(")")})),
					rdLit("]")}))}), cell()}
			},
			src: "[(ab)()][][(c)]",
		},
		{
			name: "a plus and an m* inside a star's group",
			grammar: func() []*Production {
				return []*Production{rdProd("doc", Sequence{rdStar(rdGroup(Sequence{rdLit("<"),
					rdPlus(ref("cell")), rdLit(";"), rdRep(2, MaxInfinity, ref("cell")),
					rdLit(">")}))}), cell()}
			},
			src: "<a;bc><abc;def>",
		},
		{
			name: "a star of a terminal inside a star's group",
			grammar: func() []*Production {
				return []*Production{rdProd("doc", Sequence{rdStar(rdGroup(
					Sequence{rdLit("("), rdStar(rdLit("x")), rdLit(")")}))})}
			},
			src: "(xx)()(x)",
		},
		{
			name: "a loop reached again inside one of its own items",
			grammar: func() []*Production {
				return []*Production{rdProd("v",
					Sequence{rdLit("["), rdStar(ref("v")), rdLit("]")}, Sequence{rdLit("x")})}
			},
			src: "[x[xx[]]x]",
		},
		{
			name:    "stars in an item that builds a value",
			grammar: rdPairGrammar,
			src:     "ab=12;=;c=3;",
			value: map[string]any{
				"rule": "doc", "src": "",
				"kids": []any{
					map[string]any{"key": "ab", "val": "12"},
					map[string]any{"key": "", "val": ""},
					map[string]any{"key": "c", "val": "3"},
				},
			},
		},
	}
}

// rdLoopsOf is the loops of a spec that its start rule can reach: the rules
// whose first alternative is a loop's entry. (A production whose only use
// was inlined, as a leading member is, keeps its own loop, which nothing
// runs.)
func rdLoopsOf(spec *tabnas.GrammarSpec) []string {
	start := "__start__"
	if spec.Options != nil && spec.Options.Rule != nil && spec.Options.Rule.Start != "" {
		start = spec.Options.Rule.Start
	}
	reached := map[string]bool{start: true}
	for queue := []string{start}; len(queue) > 0; queue = queue[1:] {
		for _, a := range rdAlts(spec.Rule[queue[0]]) {
			for _, to := range []string{a.P, a.R} {
				if to != "" && !reached[to] {
					reached[to] = true
					queue = append(queue, to)
				}
			}
		}
	}
	loops := []string{}
	for name := range reached {
		rs := spec.Rule[name]
		if rs == nil {
			continue
		}
		if opens := altListOf(rs.Open); len(opens) > 0 {
			if cd, ok := opens[0].C.(map[string]any); ok {
				if v, ok := cd["n.rep"]; ok && v == 0 {
					loops = append(loops, name)
				}
			}
		}
	}
	sort.Strings(loops)
	return loops
}

// rdNodeID is the identity of a loop's node, which is a tree node: a map.
func rdNodeID(t *testing.T, node any) uintptr {
	t.Helper()
	v := reflect.ValueOf(node)
	if !v.IsValid() || v.Kind() != reflect.Map {
		t.Fatalf("a loop ran in %T %v, not a tree node", node, node)
	}
	return v.Pointer()
}

// rdSameNode is whether two rules hold one node: the same map, or the same
// comparable value (both absent, say).
func rdSameNode(a, b any) bool {
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	switch {
	case !va.IsValid() || !vb.IsValid():
		return !va.IsValid() && !vb.IsValid()
	case va.Kind() == reflect.Map && vb.Kind() == reflect.Map:
		return va.Pointer() == vb.Pointer()
	case va.Type().Comparable() && vb.Type().Comparable():
		return a == b
	}
	return false
}

func TestRepeatLoopEntersAfresh(t *testing.T) {
	for _, c := range rdEntryCases() {
		for _, builtins := range []bool{false, true} {
			mode := "closures"
			if builtins {
				mode = "builtins"
			}
			t.Run(fmt.Sprintf("%s (%s)", c.name, mode), func(t *testing.T) {
				prods := c.grammar()
				spec, err := EmitGrammarSpec(&Grammar{Productions: prods},
					&ConvertOptions{Tag: "depth", Start: prods[0].Name, Builtins: builtins})
				if err != nil {
					t.Fatalf("emit: %v", err)
				}
				loops := rdLoopsOf(spec)
				if len(loops) == 0 {
					t.Fatal("no loop is reachable")
				}
				isLoop := map[string]bool{}
				for _, name := range loops {
					isLoop[name] = true
				}
				j := tabnas.Make()
				if err := j.Grammar(spec); err != nil {
					t.Fatalf("install: %v", err)
				}
				// For each pusher and loop, the nodes the loop's rules ran in.
				type entry struct {
					pusher *tabnas.Rule
					loop   string
				}
				ran := map[entry]map[uintptr]bool{}
				borrowed := []string{}
				j.SubRuleDone(func(r *tabnas.Rule, _ *tabnas.Context, _ tabnas.RuleDone) {
					if !isLoop[r.Name] {
						return
					}
					if rdSameNode(r.Node, r.Parent.Node) {
						borrowed = append(borrowed, r.Name)
					}
					e := entry{r.Parent, r.Name}
					if ran[e] == nil {
						ran[e] = map[uintptr]bool{}
					}
					ran[e][rdNodeID(t, r.Node)] = true
				})
				out, err := j.Parse(c.src)
				if err != nil {
					t.Fatalf("parse %q: %v", c.src, err)
				}
				if c.value != nil && !valueEquals(tabnas.UnwrapUndefined(out), c.value) {
					t.Errorf("%q built %#v", c.src, out)
				}
				if len(borrowed) > 0 {
					t.Errorf("a loop ran in the node of its pusher: %v", borrowed)
				}
				entered := map[string]bool{}
				for e, nodes := range ran {
					entered[e.loop] = true
					if len(nodes) != 1 {
						t.Errorf("%s allocated %d nodes on one entry", e.loop, len(nodes))
					}
				}
				got := []string{}
				for name := range entered {
					got = append(got, name)
				}
				sort.Strings(got)
				if !sameStrings(got, loops) {
					t.Errorf("the input must reach every loop: ran %v of %v", got, loops)
				}
			})
		}
	}
}
