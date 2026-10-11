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
// (MaxLeftRecursionExpansion), and so is what the whole grammar may gain in
// element occurrences (MaxLeftRecursionElements). Each bound refuses the
// grammar before the substitution that would pass it is made. Mirrors
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

// lrChain is root = r0 ; r(i) = r(i+1) "x(i)" ; r(n-1) = "end".
func lrChain(n int) *Grammar {
	g := &Grammar{Productions: []*Production{{Name: "root", Alts: []Sequence{{lrRef("r0")}}}}}
	for i := 0; i < n-1; i++ {
		g.Productions = append(g.Productions, &Production{
			Name: fmt.Sprintf("r%d", i),
			Alts: []Sequence{{lrRef(fmt.Sprintf("r%d", i+1)), lrLit(fmt.Sprintf("x%d", i))}},
		})
	}
	g.Productions = append(g.Productions, &Production{
		Name: fmt.Sprintf("r%d", n-1),
		Alts: []Sequence{{lrLit("end")}},
	})
	return g
}

// lrFan is top = s "x1" … "x(tail)", for each name in tops; s = m single
// literals. Inlining s into one of them adds m + (m − 1)·tail − 1.
func lrFan(tops []string, m, tail int) *Grammar {
	g := &Grammar{}
	for _, name := range tops {
		alt := Sequence{lrRef("s")}
		for i := 0; i < tail; i++ {
			alt = append(alt, lrLit(fmt.Sprintf("x%d", i)))
		}
		g.Productions = append(g.Productions, &Production{Name: name, Alts: []Sequence{alt}})
	}
	g.Productions = append(g.Productions, lrChoice("s", m, "s"))
	return g
}

func lrElements(rule, source string, led, gain, added int) string {
	return fmt.Sprintf("cyc: rule '%s' exceeds the left-recursion element limit of 32768 "+
		"elements while inlining '%s', which begins %d of its alternatives and would add %d "+
		"elements to the %d already added to the grammar. Factor '%s' out of the "+
		"alternatives it begins.",
		rule, source, led, gain, added, source)
}

func TestLeftRecursionElementLimitIs32768(t *testing.T) {
	if MaxLeftRecursionElements != 32768 {
		t.Fatalf("MaxLeftRecursionElements = %d", MaxLeftRecursionElements)
	}
}

func TestLeftRecursionElementsRefuseFiveThousandRuleChainWithinASecond(t *testing.T) {
	start := time.Now()
	err := lrEmit(t, lrChain(5000), &ConvertOptions{Tag: "cyc"})
	elapsed := time.Since(start)
	var ee *EmitError
	if !errors.As(err, &ee) {
		t.Fatalf("error = %v, want an *EmitError", err)
	}
	// The last rule is lifted into a token, and r(4998 - g) inlines
	// r(4999 - g) with g + 1 elements, adding g. The substitutions add
	// 1 + 2 + … + 255 = 32640, and the one adding 256 passes the limit.
	if want := lrElements("r4742", "r4743", 1, 256, 32640); ee.Message != want {
		t.Fatalf("message:\n got  %s\n want %s", ee.Message, want)
	}
	if ee.Rule != "r4742" {
		t.Fatalf("rule %q", ee.Rule)
	}
	if elapsed > time.Second {
		t.Fatalf("refused after %v", elapsed)
	}
}

func TestLeftRecursionElementsRefuseInTheStandalonePass(t *testing.T) {
	// Nothing is lifted here, and r4998 inlines "end" one for one, adding
	// nothing. The prefix is whatever the last emit set.
	_, refused := lrEliminate(lrChain(5000))
	want := strings.TrimPrefix(lrElements("r4742", "r4743", 1, 256, 32640), "cyc")
	if refused == nil || !strings.HasSuffix(refused.Message, want) {
		t.Fatalf("refused = %v", refused)
	}
}

func TestLeftRecursionElementsAdmitTheLongestChainTheLimitAllows(t *testing.T) {
	// n = 257 adds 1 + 2 + … + 255 = 32640; n = 258 would add 256 more.
	spec, err := EmitGrammarSpec(lrChain(257), &ConvertOptions{Tag: "cyc"})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := altListOf(spec.Rule["r0"].Open)[0].S.(string)
	if n := len(strings.Split(s, " ")); n != 257 {
		t.Fatalf("r0 matches %d tokens", n)
	}
	err = lrEmit(t, lrChain(258), &ConvertOptions{Tag: "cyc"})
	if err == nil || err.Error() != lrElements("r0", "r1", 1, 256, 32640) {
		t.Fatalf("error = %v", err)
	}
}

func TestLeftRecursionElementsCountATailOncePerSourceAlternative(t *testing.T) {
	// 65 + 64 × 511 − 1 = 32768 fills the limit; a tail of 512 adds 32832.
	out, refused := lrEliminate(lrFan([]string{"top"}, 65, 511))
	if refused != nil || len(out.Productions[0].Alts) != 65 {
		t.Fatalf("refused %v", refused)
	}
	err := lrEmit(t, lrFan([]string{"top"}, 65, 512), &ConvertOptions{Tag: "cyc"})
	if err == nil || err.Error() != lrElements("top", "s", 1, 32832, 0) {
		t.Fatalf("error = %v", err)
	}
}

func TestLeftRecursionElementsSumEverySubstitutionInTheGrammar(t *testing.T) {
	// Each rule adds 65 + 64 × 255 − 1 = 16384: two fill the limit.
	if _, refused := lrEliminate(lrFan([]string{"t1", "t2"}, 65, 255)); refused != nil {
		t.Fatal(refused)
	}
	err := lrEmit(t, lrFan([]string{"t1", "t2", "t3"}, 65, 255), &ConvertOptions{Tag: "cyc"})
	if err == nil || err.Error() != lrElements("t3", "s", 1, 16384, 32768) {
		t.Fatalf("error = %v", err)
	}
}

func TestLeftRecursionElementsFloorWhatAShorterReplacementAdds(t *testing.T) {
	// Inlining e, one empty alternative, first takes an element out of each
	// of 100 alternatives. That frees nothing: s still adds 32832.
	g := lrFan([]string{"top"}, 65, 512)
	lead := []Sequence{}
	for i := 0; i < 100; i++ {
		lead = append(lead, Sequence{lrRef("e"), lrLit(fmt.Sprintf("y%d", i))})
	}
	g.Productions[0].Alts = append(lead, g.Productions[0].Alts...)
	g.Productions = append(g.Productions, &Production{Name: "e", Alts: []Sequence{{}}})
	err := lrEmit(t, g, &ConvertOptions{Tag: "cyc"})
	if err == nil || err.Error() != lrElements("top", "s", 1, 32832, 0) {
		t.Fatalf("error = %v", err)
	}
}

func TestLeftRecursionElementsChargeNothingForATokenClass(t *testing.T) {
	// Inlined member by member, s would add 32832.
	err := lrEmit(t, lrFan([]string{"top"}, 65, 512), &ConvertOptions{Tag: "cyc"})
	if err == nil || err.Error() != lrElements("top", "s", 1, 32832, 0) {
		t.Fatalf("error = %v", err)
	}
	spec, err := EmitGrammarSpec(lrFan([]string{"top"}, 65, 512),
		&ConvertOptions{Tag: "cyc", TokenClasses: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Options.TokenSet["s"]) != 65 {
		t.Fatalf("token sets: %v", spec.Options.TokenSet)
	}
}
