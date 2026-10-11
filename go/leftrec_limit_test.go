// Copyright (c) 2026 tabnas, MIT License

package bnf

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// What one rule may gain from Paull's substitution is bounded
// (MaxLeftRecursionExpansion), and the bound refuses the grammar before the
// substitution that would pass it is made. Mirrors
// ts/test/leftrec-limit.test.js; every count below is exact.

func lrLit(s string) *Element {
	return &Element{Kind: KindTerm, Literal: s, CaseSensitive: true, HasCaseSens: true}
}

func lrRef(name string) *Element { return &Element{Kind: KindRef, Name: name} }

// lrCycle is root = r0 ; r(i) = r(i+1) "a(i)" / r(i+1) "b(i)" / "x(i)",
// wrapping at k.
func lrCycle(k int) *Grammar {
	g := &Grammar{Productions: []*Production{{Name: "root", Alts: []Sequence{{lrRef("r0")}}}}}
	for i := 0; i < k; i++ {
		next := fmt.Sprintf("r%d", (i+1)%k)
		g.Productions = append(g.Productions, &Production{
			Name: fmt.Sprintf("r%d", i),
			Sp:   &SrcSpan{S: 10 * i, E: 10*i + 9, R: i + 2, C: 1},
			Alts: []Sequence{
				{lrRef(next), lrLit(fmt.Sprintf("a%d", i))},
				{lrRef(next), lrLit(fmt.Sprintf("b%d", i))},
				{lrLit(fmt.Sprintf("x%d", i))},
			},
		})
	}
	return g
}

// lrChoice is a rule of n single-literal alternatives.
func lrChoice(name string, n int, prefix string) *Production {
	p := &Production{Name: name}
	for i := 0; i < n; i++ {
		p.Alts = append(p.Alts, Sequence{lrLit(fmt.Sprintf("%s%d", prefix, i))})
	}
	return p
}

func lrRefusal(rule, source string, led, has int) string {
	return fmt.Sprintf("cyc: rule '%s' exceeds the left-recursion expansion limit of 512 "+
		"alternatives while inlining '%s', which begins %d of its alternatives and has %d "+
		"of its own. Factor '%s' out of the alternatives it begins.",
		rule, source, led, has, source)
}

func lrEmit(t *testing.T, g *Grammar, opts *ConvertOptions) error {
	t.Helper()
	_, err := EmitGrammarSpec(g, opts)
	return err
}

// lrEliminate runs the standalone pass, which panics with *EmitError on a
// grammar it refuses, as it always has for a purely left-recursive rule.
func lrEliminate(g *Grammar) (out *Grammar, refused *EmitError) {
	defer func() {
		if r := recover(); r != nil {
			ee, ok := r.(*EmitError)
			if !ok {
				panic(r)
			}
			refused = ee
		}
	}()
	return EliminateLeftRecursion(g), nil
}

func TestLeftRecursionExpansionLimitIs512(t *testing.T) {
	if MaxLeftRecursionExpansion != 512 {
		t.Fatalf("MaxLeftRecursionExpansion = %d", MaxLeftRecursionExpansion)
	}
}

func TestLeftRecursionExpansionRefusesTwentyRuleCycleWithinASecond(t *testing.T) {
	start := time.Now()
	err := lrEmit(t, lrCycle(20), &ConvertOptions{Tag: "cyc"})
	elapsed := time.Since(start)
	var ee *EmitError
	if !errors.As(err, &ee) {
		t.Fatalf("error = %v, want an *EmitError", err)
	}
	// The topological order processes r19 first, so r12 has grown to 511
	// alternatives when r11 inlines it twice: a gain of 1020.
	if want := lrRefusal("r11", "r12", 2, 511); ee.Message != want {
		t.Fatalf("message:\n got  %s\n want %s", ee.Message, want)
	}
	if ee.Rule != "r11" || ee.Sp == nil || *ee.Sp != (SrcSpan{S: 110, E: 119, R: 13, C: 1}) {
		t.Fatalf("rule %q, span %+v", ee.Rule, ee.Sp)
	}
	if elapsed > time.Second {
		t.Fatalf("refused after %v", elapsed)
	}
}

func TestLeftRecursionExpansionRefusesInTheStandalonePass(t *testing.T) {
	_, refused := lrEliminate(lrCycle(20))
	if refused == nil || !strings.Contains(refused.Message,
		"rule 'r11' exceeds the left-recursion expansion limit") {
		t.Fatalf("refused = %v", refused)
	}
}

func TestLeftRecursionExpansionAdmitsTheCycleUpToTheLimit(t *testing.T) {
	// k = 8: r0 inlines r1 (255 alternatives) twice, a gain of 508.
	out, refused := lrEliminate(lrCycle(8))
	if refused != nil {
		t.Fatal(refused)
	}
	var r0 *Production
	for _, p := range out.Productions {
		if p.Name == "r0" {
			r0 = p
		}
	}
	if r0 == nil || len(r0.Alts) != 1 || len(r0.Alts[0][0].Alts) != 255 ||
		len(r0.Alts[0][1].Inner.Alts) != 256 {
		t.Fatalf("r0 is not the 255-seed, 256-tail loop: %+v", r0)
	}
	// k = 9: r1 has 511 when r0 inlines it twice, a gain of 1020.
	err := lrEmit(t, lrCycle(9), &ConvertOptions{Tag: "cyc"})
	if err == nil || err.Error() != lrRefusal("r0", "r1", 2, 511) {
		t.Fatalf("error = %v", err)
	}
}

func TestLeftRecursionExpansionCountsUpToTheLimitAndNoFurther(t *testing.T) {
	// top inlines s once: s's 513 alternatives replace one, a gain of 512.
	at := &Grammar{Productions: []*Production{
		{Name: "top", Alts: []Sequence{{lrRef("s"), lrLit("x")}}}, lrChoice("s", 513, "s")}}
	out, refused := lrEliminate(at)
	if refused != nil || len(out.Productions[0].Alts) != 513 {
		t.Fatalf("refused %v, alts %d", refused, len(out.Productions[0].Alts))
	}
	past := &Grammar{Productions: []*Production{
		{Name: "top", Alts: []Sequence{{lrRef("s"), lrLit("x")}}}, lrChoice("s", 514, "s")}}
	err := lrEmit(t, past, &ConvertOptions{Tag: "cyc"})
	if err == nil || err.Error() != lrRefusal("top", "s", 1, 514) {
		t.Fatalf("error = %v", err)
	}
}

func TestLeftRecursionExpansionSumsEverySubstitutionIntoOneRule(t *testing.T) {
	// 299 from `a`, then 299 from `b`: 598.
	g := &Grammar{Productions: []*Production{
		{Name: "top", Alts: []Sequence{{lrRef("a"), lrLit("1")}, {lrRef("b"), lrLit("2")}}},
		lrChoice("a", 300, "a"), lrChoice("b", 300, "b")}}
	err := lrEmit(t, g, &ConvertOptions{Tag: "cyc"})
	if err == nil || err.Error() != lrRefusal("top", "b", 1, 300) {
		t.Fatalf("error = %v", err)
	}
}

func TestLeftRecursionExpansionChargesNothingForOwnOrOneForOne(t *testing.T) {
	// 2000 alternatives of its own and 2000 led by a rule of one: no gain.
	top := lrChoice("top", 2000, "k")
	for i := 0; i < 2000; i++ {
		top.Alts = append(top.Alts, Sequence{lrRef("one"), lrLit(fmt.Sprintf("t%d", i))})
	}
	g := &Grammar{Productions: []*Production{top, {Name: "one", Alts: []Sequence{{lrLit("o")}}}}}
	out, refused := lrEliminate(g)
	if refused != nil || len(out.Productions[0].Alts) != 4000 {
		t.Fatalf("refused %v, alts %d", refused, len(out.Productions[0].Alts))
	}
}

func TestLeftRecursionExpansionChargesNothingForATokenClass(t *testing.T) {
	// Inlined member by member, kw's 600 alternatives would add 1198.
	grammar := func() *Grammar {
		return &Grammar{Productions: []*Production{
			{Name: "top", Alts: []Sequence{{lrRef("kw"), lrLit("x")}, {lrRef("kw"), lrLit("y")}}},
			lrChoice("kw", 600, "w")}}
	}
	err := lrEmit(t, grammar(), &ConvertOptions{Tag: "cyc"})
	if err == nil || err.Error() != lrRefusal("top", "kw", 2, 600) {
		t.Fatalf("error = %v", err)
	}
	spec, err := EmitGrammarSpec(grammar(), &ConvertOptions{Tag: "cyc", TokenClasses: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Options.TokenSet["kw"]) != 600 {
		t.Fatalf("token sets: %v", spec.Options.TokenSet)
	}
}
