// Copyright (c) 2026 tabnas, MIT License

package bnf

import (
	"errors"
	"fmt"
	"math"
	"testing"
	"time"
)

// What compiling one grammar may write is bounded (MaxEmitUnits), and so is
// what the enumeration that tells a choice's alternatives apart may hold
// while it runs. Each is charged before the allocation that would pass it,
// so a grammar is refused before the step that would pass the limit is
// made. The memos of token pairs are limited too (maxMemoEntries), and that
// limit changes no output. Mirrors ts/test/emit-limit.test.js; every count
// below is exact. A probe shape's refusal, that of the forty alternatives of
// up to forty options and that of a removal list are also shared fixtures,
// rs/tests/oracle/ir-emit-units-*.json, which
// TestOracleRefusalsMatchTypeScript grades.

func elLit(s string) *Element {
	return &Element{Kind: KindTerm, Literal: s, CaseSensitive: true, HasCaseSens: true}
}

func elRef(name string) *Element { return &Element{Kind: KindRef, Name: name} }

func elOpt(inner *Element) *Element { return &Element{Kind: KindOpt, Inner: inner} }

// elProbes is top = "k0" t0 / … ; t(i) = ["a" "d"] "a" v ;
// v = "w0" / … / "w(V-1)". Each t(i) is a probe over the V + 1 tokens of v
// and "a".
func elProbes(p, v int) *Grammar {
	top := &Production{Name: "top"}
	for i := 0; i < p; i++ {
		top.Alts = append(top.Alts, Sequence{elLit(fmt.Sprintf("k%d", i)), elRef(fmt.Sprintf("t%d", i))})
	}
	g := &Grammar{Productions: []*Production{top}}
	for i := 0; i < p; i++ {
		ad := &Element{Kind: KindGroup, Alts: []Sequence{{elLit("a"), elLit("d")}}}
		g.Productions = append(g.Productions, &Production{
			Name: fmt.Sprintf("t%d", i),
			Sp:   &SrcSpan{S: 10 * i, E: 10*i + 9, R: i + 2, C: 1},
			Alts: []Sequence{{elOpt(ad), elLit("a"), elRef("v")}},
		})
	}
	words := &Production{Name: "v"}
	for i := 0; i < v; i++ {
		words.Alts = append(words.Alts, Sequence{elLit(fmt.Sprintf("w%d", i))})
	}
	g.Productions = append(g.Productions, words)
	return g
}

// elOptions is A = [x0] … [x(k-1)] "y" / [x1] … [x(k-1)] "y" / … / "y" /
// "z": every first token but "z" begins several alternatives.
func elOptions(k int) *Grammar {
	a := &Production{Name: "A"}
	for j := 0; j < k; j++ {
		alt := Sequence{}
		for i := 0; i < k-j; i++ {
			alt = append(alt, elOpt(elLit(fmt.Sprintf("x%d", i+j))))
		}
		a.Alts = append(a.Alts, append(alt, elLit("y")))
	}
	a.Alts = append(a.Alts, Sequence{elLit("z")})
	return &Grammar{Productions: []*Production{a}}
}

// elRun is A = [x0] … [x(k-1)] "y" / "x0" "w": one run of options,
// contested at its first token alone.
func elRun(k int) *Grammar {
	alt := Sequence{}
	for i := 0; i < k; i++ {
		alt = append(alt, elOpt(elLit(fmt.Sprintf("x%d", i))))
	}
	alt = append(alt, elLit("y"))
	return &Grammar{Productions: []*Production{
		{Name: "A", Alts: []Sequence{alt, {elLit("x0"), elLit("w")}}},
	}}
}

// elNested is C character classes nested inside one another, [Ā-ā],
// [Ā-Ă], …, each beside a literal of its own: C(C + 1)/2 token-set members.
func elNested(c int) *Grammar {
	a := &Production{Name: "A"}
	for i := 0; i < c; i++ {
		pattern := fmt.Sprintf("[%c-%c]", rune(0x100), rune(0x101+i))
		a.Alts = append(a.Alts, Sequence{
			{Kind: KindRegex, Pattern: pattern, Flags: "u"},
			elLit(fmt.Sprintf("e%d", i)),
		})
	}
	return &Grammar{Productions: []*Production{a}}
}

// elDispatched is S = "s" F D: F holds c nested classes, as elNested's A
// does, and D n alternatives of distinct heads, each past a reference,
// which a dispatcher decides.
func elDispatched(c, n int) *Grammar {
	f := elNested(c).Productions[0]
	f.Name = "F"
	d := &Production{Name: "D"}
	for i := 0; i < n; i++ {
		d.Alts = append(d.Alts, Sequence{elLit(fmt.Sprintf("k%d", i)), elRef("B"), elLit("e")})
	}
	return &Grammar{Productions: []*Production{
		{Name: "S", Alts: []Sequence{{elLit("s"), elRef("F"), elRef("D")}}},
		f,
		d,
		{Name: "B", Alts: []Sequence{{elLit("b")}, {elLit("c")}}},
	}}
}

const (
	elAdviceProbe = "Write that optional prefix and what follows it as two " +
		"alternatives, one with the prefix and one without."
	elAdviceAlternates = "Factor what its alternatives share into rules of their own."
	elAdviceLookahead  = "Give its alternatives first tokens that tell them apart, or " +
		"factor what they begin with into a rule of its own."
	elAdviceTokenSet = "Write fewer character classes that overlap it."
	elAdviceRemove   = "Remove fewer names."
)

func elAdds(subject, doing string, room, written int, advice string) string {
	return fmt.Sprintf("x: %s exceeds the emit limit of 3500000 units while %s, "+
		"which would add more than %d units to the %d already written. %s",
		subject, doing, room, written, advice)
}

func elHolds(rule string, room, written int) string {
	return fmt.Sprintf("x: rule '%s' exceeds the emit limit of 3500000 units while "+
		"enumerating the token sequences that tell its alternatives apart, which "+
		"would hold more than %d units on top of the %d already written. %s",
		rule, room, written, elAdviceLookahead)
}

func elRefused(t *testing.T, g *Grammar) (*EmitError, time.Duration) {
	t.Helper()
	start := time.Now()
	_, err := EmitGrammarSpec(g, &ConvertOptions{Tag: "x"})
	elapsed := time.Since(start)
	var ee *EmitError
	if !errors.As(err, &ee) {
		t.Fatalf("error = %v, want an *EmitError", err)
	}
	return ee, elapsed
}

// elRefusedWithin compiles what build makes, afresh each time, until a
// compile is refused within limit, at most three times, and returns the
// last refusal and the fastest time, since whatever else the machine is
// running can slow one compile down. The tests give three seconds: the
// refusals take 0.45 s and 0.6 to 0.7 s where they were measured, a slower
// machine has taken twice that, and what the limit guards against is the
// minutes these shapes took unbounded.
const elWithin = 3 * time.Second

func elRefusedWithin(t *testing.T, build func() *Grammar, limit time.Duration) (*EmitError, time.Duration) {
	t.Helper()
	var ee *EmitError
	best := time.Duration(math.MaxInt64)
	for i := 0; i < 3 && best > limit; i++ {
		var elapsed time.Duration
		ee, elapsed = elRefused(t, build())
		best = min(best, elapsed)
	}
	return ee, best
}

func elCompiles(t *testing.T, g *Grammar) {
	t.Helper()
	if _, err := EmitGrammarSpec(g, &ConvertOptions{Tag: "x"}); err != nil {
		t.Fatalf("refused: %v", err)
	}
}

func elWant(t *testing.T, ee *EmitError, want string) {
	t.Helper()
	if ee.Message != want {
		t.Fatalf("message:\n got  %s\n want %s", ee.Message, want)
	}
}

func TestEmitLimitIs3500000(t *testing.T) {
	if MaxEmitUnits != 3500000 {
		t.Fatalf("MaxEmitUnits = %d", MaxEmitUnits)
	}
}

func TestEmitLimitRefusesAThousandProbesInSeconds(t *testing.T) {
	// Each probe's list is charged with the rule it becomes, 4·1,001 + 2
	// units, as it is made: t873's would pass the limit.
	ee, elapsed := elRefusedWithin(t, func() *Grammar { return elProbes(1000, 1000) }, elWithin)
	elWant(t, ee, elAdds("rule 't873'", "writing the probe for one of its optional prefixes",
		2762, 3497238, elAdviceProbe))
	if ee.Rule != "t873" || ee.Sp == nil || *ee.Sp != (SrcSpan{S: 8730, E: 8739, R: 875, C: 1}) {
		t.Fatalf("rule %q, span %+v", ee.Rule, ee.Sp)
	}
	if elapsed > elWithin {
		t.Fatalf("refused after %v at best", elapsed)
	}
}

func TestEmitLimitCountsEveryUnitTheProbeShapeWrites(t *testing.T) {
	// P probes over V tokens write 4PV + 44P + 3V + 10 units: 3,498,599 for
	// a thousand over 863, which compiles, and 3,502,602 over 864, which is
	// refused while t881's dispatcher is written.
	spec, err := EmitGrammarSpec(elProbes(1000, 863), &ConvertOptions{Tag: "x"})
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	if n := len(altListOf(spec.Rule["t999$pd0$probe"].Open)); n != 865 {
		t.Fatalf("t999's probe holds %d alternates", n)
	}
	ee, _ := elRefused(t, elProbes(1000, 864))
	elWant(t, ee, elAdds("rule 't881'", "writing its alternates", 9, 3499991, elAdviceAlternates))
}

func TestEmitLimitRefusesFortyAlternativesOfFortyOptionsInSeconds(t *testing.T) {
	ee, elapsed := elRefusedWithin(t, func() *Grammar { return elOptions(40) }, elWithin)
	elWant(t, ee, elHolds("A", 3486837, 13163))
	if ee.Rule != "A" {
		t.Fatalf("rule %q", ee.Rule)
	}
	if elapsed > elWithin {
		t.Fatalf("refused after %v at best", elapsed)
	}
}

func TestEmitLimitRefusesTheOptionsShapeFromThirtyNine(t *testing.T) {
	ee, _ := elRefused(t, elOptions(39))
	elWant(t, ee, elHolds("A", 3487738, 12262))
}

func TestEmitLimitCountsWhatAChoiceEnumerates(t *testing.T) {
	// The run of 133 options enumerates just under the limit and compiles;
	// the run of 134 passes it while the enumeration is under way.
	elCompiles(t, elRun(133))
	ee, _ := elRefused(t, elRun(134))
	elWant(t, ee, elHolds("A", 3490551, 9449))
}

func TestEmitLimitCountsEveryTokenSetMember(t *testing.T) {
	// C nested classes write C(C + 1)/2 members and 4C + 7 units more:
	// 3,499,332 for 2,641 classes, which compile, and 3,501,978 for 2,642,
	// refused while A is written.
	spec, err := EmitGrammarSpec(elNested(2641), &ConvertOptions{Tag: "x"})
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	members := 0
	for _, set := range spec.Options.TokenSet {
		members += len(set)
	}
	if members != 2641*2642/2 {
		t.Fatalf("%d token-set members", members)
	}
	ee, _ := elRefused(t, elNested(2642))
	elWant(t, ee, elAdds("rule 'A'", "writing its alternates", 5954, 3494046, elAdviceAlternates))
}

func TestEmitLimitHoldsADispatchersPrefixesUntilItsAlternatesAreWritten(t *testing.T) {
	// The nested classes fill most of the limit; D's dispatcher then holds
	// the prefixes it decided on, two units for each of its n heads, while
	// it writes the alternates made from them.
	elCompiles(t, elDispatched(2635, 569))
	ee, _ := elRefused(t, elDispatched(2635, 570))
	elWant(t, ee, elAdds("rule 'D'", "writing its alternates", 15372, 3484628, elAdviceAlternates))
}

func TestEmitLimitCountsTheEntriesTheRemovalsWrite(t *testing.T) {
	// Each name a grammar removes writes a nil rule and a nil fixed token in
	// the spec, 2 units. Beside the 3,499,332 units 2,641 nested classes
	// write, 334 names fill the limit exactly, and 335 would pass it.
	removing := func(n int) *Grammar {
		g := elNested(2641)
		for i := 0; i < n; i++ {
			g.Remove = append(g.Remove, fmt.Sprintf("gone%d", i))
		}
		return g
	}
	spec, err := EmitGrammarSpec(removing(334), &ConvertOptions{Tag: "x"})
	if err != nil {
		t.Fatalf("334 removals refused: %v", err)
	}
	if rs, ok := spec.Rule["gone333"]; !ok || rs != nil {
		t.Fatalf("gone333 is %v, present %v; want a nil rule", rs, ok)
	}
	ee, _ := elRefused(t, removing(335))
	elWant(t, ee, elAdds("the removal list of 335 names", "writing the rules and tokens it removes",
		668, 3499332, elAdviceRemove))
	if ee.Rule != "" {
		t.Fatalf("rule %q, want none", ee.Rule)
	}
}

func TestEmitLimitRefusesTheTokenSetThatWouldPassItNamingTheClass(t *testing.T) {
	// The first 2,645 classes list 3,499,335 members; the next would list
	// 2,646 more.
	ee, _ := elRefused(t, elNested(2646))
	class := fmt.Sprintf("the character class /[%c-%c]/", rune(0x100), rune(0xb56))
	elWant(t, ee, elAdds(class, "listing the members of its token set", 665, 3499335,
		elAdviceTokenSet))
	if ee.Rule != "" {
		t.Fatalf("rule %q", ee.Rule)
	}
}

// elText is the pure spec a grammar compiles to, as text.
func elText(t *testing.T, g *Grammar) string {
	t.Helper()
	spec, err := EmitGrammarSpec(g, &ConvertOptions{Tag: "x", Builtins: true})
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	pure, err := ToPureSpec(spec)
	if err != nil {
		t.Fatalf("pure: %v", err)
	}
	return ToJsonic(pure, true, 0)
}

// elSameUnderAnyLimit holds the grammar's spec to be the same with the
// memo limit lifted and with a memo that keeps nothing.
func elSameUnderAnyLimit(t *testing.T, g *Grammar) {
	t.Helper()
	kept := elText(t, g)
	was := memoLimit
	defer func() { memoLimit = was }()
	memoLimit = math.MaxInt
	if elText(t, g) != kept {
		t.Fatal("the spec differs with no limit")
	}
	memoLimit = 0
	if elText(t, g) != kept {
		t.Fatal("the spec differs keeping nothing")
	}
}

func TestMemoLimitChangesNoOutputWhenARuleAsksMorePairsThanItKeeps(t *testing.T) {
	// A rule of 400 two-segment alternatives with distinct heads goes to the
	// dispatcher, whose contested-head scan asks 79,800 pairs, past the
	// 65,536 kept.
	a := &Production{Name: "A"}
	for i := 0; i < 400; i++ {
		a.Alts = append(a.Alts, Sequence{elLit(fmt.Sprintf("k%06d", i)), elRef("B"), elLit("e")})
	}
	elSameUnderAnyLimit(t, &Grammar{Productions: []*Production{a,
		{Name: "B", Alts: []Sequence{{elLit("b")}, {elLit("c")}}}}})
}

func TestMemoLimitChangesNoOutputWhenTheKeywordShadowOrderingPassesIt(t *testing.T) {
	// 300 literal heads against 300 class heads, every pair overlapping:
	// 90,000 answers.
	a := &Production{Name: "A"}
	for i := 0; i < 300; i++ {
		a.Alts = append(a.Alts, Sequence{elLit(fmt.Sprintf("a%d", i)), elRef("B")})
	}
	for i := 0; i < 300; i++ {
		pattern := fmt.Sprintf("[a-%c]", rune(0x101+i))
		a.Alts = append(a.Alts, Sequence{{Kind: KindRegex, Pattern: pattern, Flags: "u"}, elRef("B")})
	}
	elSameUnderAnyLimit(t, &Grammar{Productions: []*Production{a,
		{Name: "B", Alts: []Sequence{{elLit("b")}}}}})
}

// The memo limit is the one the canonical compiler keeps.
func TestMemoLimitIs65536(t *testing.T) {
	if maxMemoEntries != 65536 || memoLimit != maxMemoEntries {
		t.Fatalf("maxMemoEntries = %d, memoLimit = %d", maxMemoEntries, memoLimit)
	}
}
