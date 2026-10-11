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
// elements and alternatives (MaxLeftRecursionElements). Each bound refuses
// the grammar before the step that would pass it is made. Mirrors
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

// lrFan is top = s "x0" … "x(tail-1)", for each name in tops; s = m single
// literals. Inlining s into one of them adds (m − 1)·(1 + tail) + m − 1.
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

// lrGroupOf is a group holding one alternative of k single literals.
func lrGroupOf(k int) *Element {
	alt := Sequence{}
	for i := 0; i < k; i++ {
		alt = append(alt, lrLit(fmt.Sprintf("g%d", i)))
	}
	return &Element{Kind: KindGroup, Alts: []Sequence{alt}}
}

// lrEmptyGroup is a group of k empty alternatives.
func lrEmptyGroup(k int) *Element {
	alts := []Sequence{}
	for i := 0; i < k; i++ {
		alts = append(alts, Sequence{})
	}
	return &Element{Kind: KindGroup, Alts: alts}
}

// lrOptionalPrefix is A = [x0] … [x(k-1)] A "y" / "z": a hidden left
// recursion behind k options.
func lrOptionalPrefix(k int) *Grammar {
	alt := Sequence{}
	for i := 0; i < k; i++ {
		alt = append(alt, &Element{Kind: KindOpt, Inner: lrLit(fmt.Sprintf("x%d", i))})
	}
	alt = append(alt, lrRef("A"), lrLit("y"))
	return &Grammar{Productions: []*Production{
		{Name: "A", Alts: []Sequence{alt, {lrLit("z")}}},
	}}
}

func lrSplitting(rule string, remaining, added int) string {
	return fmt.Sprintf("cyc: rule '%s' exceeds the left-recursion element limit of 131072 "+
		"elements while splitting the elements that can match nothing before '%s' in one "+
		"of its alternatives, which would add more than %d elements to the %d already "+
		"added to the grammar. Write that alternative with fewer elements that can match "+
		"nothing before '%s'.",
		rule, rule, remaining, added, rule)
}

func lrElements(rule, source string, led, remaining, added int) string {
	return fmt.Sprintf("cyc: rule '%s' exceeds the left-recursion element limit of 131072 "+
		"elements while inlining '%s', which begins %d of its alternatives and would add more "+
		"than %d elements to the %d already added to the grammar. Factor '%s' out of the "+
		"alternatives it begins.",
		rule, source, led, remaining, added, source)
}

func lrRefusedWith(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || err.Error() != want {
		t.Fatalf("error:\n got  %v\n want %s", err, want)
	}
}

func TestLeftRecursionElementLimitIs131072(t *testing.T) {
	if MaxLeftRecursionElements != 131072 {
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
	// 1 + 2 + … + 511 = 130816, and the one adding 512 passes the limit.
	lrRefusedWith(t, err, lrElements("r4486", "r4487", 1, 256, 130816))
	if ee.Rule != "r4486" {
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
	want := strings.TrimPrefix(lrElements("r4486", "r4487", 1, 256, 130816), "cyc")
	if refused == nil || !strings.HasSuffix(refused.Message, want) {
		t.Fatalf("refused = %v", refused)
	}
}

func TestLeftRecursionElementsRefuseAHundredThousandRuleChainOnEitherPath(t *testing.T) {
	// Every walk that runs before the bound has a stack of its own, so the
	// chain is the bound's to refuse.
	g := lrChain(100000)
	start := time.Now()
	lrRefusedWith(t, lrEmit(t, g, &ConvertOptions{Tag: "cyc"}),
		lrElements("r99486", "r99487", 1, 256, 130816))
	_, refused := lrEliminate(g)
	want := strings.TrimPrefix(lrElements("r99486", "r99487", 1, 256, 130816), "cyc")
	if refused == nil || !strings.HasSuffix(refused.Message, want) {
		t.Fatalf("refused = %v", refused)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("refused after %v", elapsed)
	}
}

func TestLeftRecursionElementsAdmitTheLongestChainTheLimitAllows(t *testing.T) {
	// n = 513 adds 1 + 2 + … + 511 = 130816; n = 514 would add 512 more.
	spec, err := EmitGrammarSpec(lrChain(513), &ConvertOptions{Tag: "cyc"})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := altListOf(spec.Rule["r0"].Open)[0].S.(string)
	if n := len(strings.Split(s, " ")); n != 513 {
		t.Fatalf("r0 matches %d tokens", n)
	}
	lrRefusedWith(t, lrEmit(t, lrChain(514), &ConvertOptions{Tag: "cyc"}),
		lrElements("r0", "r1", 1, 256, 130816))
}

func TestLeftRecursionElementsCountATailOncePerSourceAlternative(t *testing.T) {
	// 128 × (1 + 1022) + 129 − 1 = 131072 fills the limit; a tail of 1023
	// adds 131200.
	out, refused := lrEliminate(lrFan([]string{"top"}, 129, 1022))
	if refused != nil || len(out.Productions[0].Alts) != 129 {
		t.Fatalf("refused %v", refused)
	}
	lrRefusedWith(t, lrEmit(t, lrFan([]string{"top"}, 129, 1023), &ConvertOptions{Tag: "cyc"}),
		lrElements("top", "s", 1, 131072, 0))
}

func TestLeftRecursionElementsCountAGroupInATail(t *testing.T) {
	// The tail is one element, a group whose one alternative holds k
	// literals, so it counts 1 + 1 + k:
	// 512 × (1 + 2 + 252) + 513 − 1 = 131072, and a group of 253 adds 131584.
	tailGroup := func(k int) *Grammar {
		g := lrFan([]string{"top"}, 513, 0)
		g.Productions[0].Alts[0] = append(g.Productions[0].Alts[0], lrGroupOf(k))
		return g
	}
	out, refused := lrEliminate(tailGroup(252))
	if refused != nil || len(out.Productions[0].Alts) != 513 {
		t.Fatalf("refused %v", refused)
	}
	lrRefusedWith(t, lrEmit(t, tailGroup(253), &ConvertOptions{Tag: "cyc"}),
		lrElements("top", "s", 1, 131072, 0))
}

func TestLeftRecursionElementsCountEachAlternativeOfAGroupInATail(t *testing.T) {
	// The tail is a group of k empty alternatives, so it counts 1 + k:
	// 512 × (1 + 1 + 253) + 513 − 1 = 131072, and 254 of them add 131584.
	tailGroup := func(k int) *Grammar {
		g := lrFan([]string{"top"}, 513, 0)
		g.Productions[0].Alts[0] = append(g.Productions[0].Alts[0], lrEmptyGroup(k))
		return g
	}
	out, refused := lrEliminate(tailGroup(253))
	if refused != nil || len(out.Productions[0].Alts) != 513 {
		t.Fatalf("refused %v", refused)
	}
	lrRefusedWith(t, lrEmit(t, tailGroup(254), &ConvertOptions{Tag: "cyc"}),
		lrElements("top", "s", 1, 131072, 0))
}

func TestLeftRecursionElementsCountAGroupInTheInlinedRule(t *testing.T) {
	// s is one alternative holding a group of 500, which counts 502: each
	// rule that begins with it adds 501, so 261 fit and the 262nd passes the
	// limit.
	g := func(n int) *Grammar {
		out := &Grammar{}
		for i := 0; i < n; i++ {
			out.Productions = append(out.Productions, &Production{
				Name: fmt.Sprintf("t%d", i), Alts: []Sequence{{lrRef("s"), lrLit("x")}}})
		}
		out.Productions = append(out.Productions,
			&Production{Name: "s", Alts: []Sequence{{lrGroupOf(500)}}})
		return out
	}
	if _, refused := lrEliminate(g(261)); refused != nil {
		t.Fatal(refused)
	}
	lrRefusedWith(t, lrEmit(t, g(262), &ConvertOptions{Tag: "cyc"}),
		lrElements("t261", "s", 1, 311, 130761))
}

func TestLeftRecursionElementsCountEachAlternativeTheInlinedRuleWrites(t *testing.T) {
	// s is 513 empty alternatives: each rule that begins with it gains 512
	// of them, within the alternative limit, and adds 511 to the grammar, so
	// 256 fit and the 257th passes the limit.
	g := func(n int) *Grammar {
		out := &Grammar{}
		for i := 0; i < n; i++ {
			out.Productions = append(out.Productions, &Production{
				Name: fmt.Sprintf("t%d", i), Alts: []Sequence{{lrRef("s")}, {lrLit("x")}}})
		}
		s := &Production{Name: "s"}
		for i := 0; i < 513; i++ {
			s.Alts = append(s.Alts, Sequence{})
		}
		out.Productions = append(out.Productions, s)
		return out
	}
	out, refused := lrEliminate(g(256))
	if refused != nil || len(out.Productions[0].Alts) != 514 {
		t.Fatalf("refused %v", refused)
	}
	lrRefusedWith(t, lrEmit(t, g(257), &ConvertOptions{Tag: "cyc"}),
		lrElements("t256", "s", 1, 256, 130816))
}

func TestLeftRecursionElementsChargeSplittingAnOptionalPrefix(t *testing.T) {
	// Splitting the i-th of k options off writes the rest of the alternative
	// again, 2(k − i) + 2 elements. k = 361 adds 361 × 362 = 130682; k = 362
	// passes the limit at its 345th split, with 344 × 381 = 131064 added.
	out, refused := lrEliminate(lrOptionalPrefix(361))
	if refused != nil {
		t.Fatal(refused)
	}
	// A = ("x0" [x1] … A "y" | … | "z") "y"*: 361 split alternatives and "z".
	if seeds := out.Productions[0].Alts[0][0]; seeds.Kind != KindGroup || len(seeds.Alts) != 362 {
		t.Fatalf("seed group: %+v", seeds)
	}
	lrRefusedWith(t, lrEmit(t, lrOptionalPrefix(362), &ConvertOptions{Tag: "cyc"}),
		lrSplitting("A", 8, 131064))
}

func TestLeftRecursionElementsRefuseFiveThousandOptionsWithinASecond(t *testing.T) {
	start := time.Now()
	lrRefusedWith(t, lrEmit(t, lrOptionalPrefix(5000), &ConvertOptions{Tag: "cyc"}),
		lrSplitting("A", 1228, 129844))
	_, refused := lrEliminate(lrOptionalPrefix(5000))
	want := strings.TrimPrefix(lrSplitting("A", 1228, 129844), "cyc")
	if refused == nil || !strings.HasSuffix(refused.Message, want) {
		t.Fatalf("refused = %v", refused)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("refused after %v", elapsed)
	}
}

func TestLeftRecursionElementsSumEverySubstitutionInTheGrammar(t *testing.T) {
	// Each rule adds 128 × (1 + 400) + 129 − 1 = 51456: two fit, and the
	// third passes the limit.
	if _, refused := lrEliminate(lrFan([]string{"t1", "t2"}, 129, 400)); refused != nil {
		t.Fatal(refused)
	}
	lrRefusedWith(t,
		lrEmit(t, lrFan([]string{"t1", "t2", "t3"}, 129, 400), &ConvertOptions{Tag: "cyc"}),
		lrElements("t3", "s", 1, 28160, 102912))
}

func TestLeftRecursionElementsFloorWhatAShorterReplacementAdds(t *testing.T) {
	// Inlining e, one empty alternative, first takes an element out of each
	// of 100 alternatives. That frees nothing: s still adds 131200.
	g := lrFan([]string{"top"}, 129, 1023)
	lead := []Sequence{}
	for i := 0; i < 100; i++ {
		lead = append(lead, Sequence{lrRef("e"), lrLit(fmt.Sprintf("y%d", i))})
	}
	g.Productions[0].Alts = append(lead, g.Productions[0].Alts...)
	g.Productions = append(g.Productions, &Production{Name: "e", Alts: []Sequence{{}}})
	lrRefusedWith(t, lrEmit(t, g, &ConvertOptions{Tag: "cyc"}),
		lrElements("top", "s", 1, 131072, 0))
}

func TestLeftRecursionElementsChargeNothingForATokenClass(t *testing.T) {
	// Inlined member by member, s would add 131200.
	lrRefusedWith(t, lrEmit(t, lrFan([]string{"top"}, 129, 1023), &ConvertOptions{Tag: "cyc"}),
		lrElements("top", "s", 1, 131072, 0))
	spec, err := EmitGrammarSpec(lrFan([]string{"top"}, 129, 1023),
		&ConvertOptions{Tag: "cyc", TokenClasses: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Options.TokenSet["s"]) != 129 {
		t.Fatalf("token sets: %v", spec.Options.TokenSet)
	}
}
