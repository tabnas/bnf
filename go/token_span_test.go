// Copyright (c) 2026 tabnas, MIT License

package bnf

import (
	"fmt"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// Left factoring and the specificity ordering ask how many tokens a
// sequence can span (seqTokenSpan), and only up to lookaheadKSpan. The walk
// that answers keeps its own stack and one set of the rules on its way, and
// stops as soon as a sequence passes what the window leaves it. The walk it
// replaced recursed once per rule it followed and copied the path at every
// one: on a chain of rules each ending in the next, at the head of two
// alternatives, it followed the chain to its end, allocating 2.6 to 2.9 GB
// at 10,000 rules.

// spanChain is top = c0 "f0" / c0 "f1" ; c(i) = "g(i)" c(i+1) / … ;
// c(n-1) = "g(n-1)". The two alternatives of top share their first
// element, so left factoring asks how far c0 reaches. With two, each rule
// of the chain but the last has a second alternative, "h(i)" c(i+1).
func spanChain(n int, two bool) *Grammar {
	g := &Grammar{Productions: []*Production{{Name: "top", Alts: []Sequence{
		{elRef("c0"), elLit("f0")},
		{elRef("c0"), elLit("f1")},
	}}}}
	for i := 0; i < n; i++ {
		c := &Production{Name: fmt.Sprintf("c%d", i)}
		if i+1 < n {
			next := fmt.Sprintf("c%d", i+1)
			c.Alts = append(c.Alts, Sequence{elLit(fmt.Sprintf("g%d", i)), elRef(next)})
			if two {
				c.Alts = append(c.Alts, Sequence{elLit(fmt.Sprintf("h%d", i)), elRef(next)})
			}
		} else {
			c.Alts = append(c.Alts, Sequence{elLit(fmt.Sprintf("g%d", i))})
		}
		g.Productions = append(g.Productions, c)
	}
	return g
}

// spanChainAllocates compiles the chain of n rules and returns what the
// compile allocated in all.
func spanChainAllocates(t *testing.T, n int) uint64 {
	t.Helper()
	g := spanChain(n, false)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	spec, err := EmitGrammarSpec(g, &ConvertOptions{Tag: "x"})
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatalf("the chain of %d rules: %v", n, err)
	}
	// The last rule, one literal, is lifted into a token; the one before it
	// is the deepest rule left.
	if _, ok := spec.Rule[fmt.Sprintf("c%d", n-2)]; !ok {
		t.Fatalf("the chain of %d rules lost c%d", n, n-2)
	}
	return after.TotalAlloc - before.TotalAlloc
}

func TestTokenSpanWalkFollowsAChainOf30000RulesNoFurther(t *testing.T) {
	// Doubling the chain about doubles what the compile allocates: the walk
	// adds nothing that grows with the chain. The walk this replaced copied
	// the path at every rule it followed, n²/2 names, which grows four times
	// over, so the small chains tell before the large one is tried.
	small := spanChainAllocates(t, 3000)
	double := spanChainAllocates(t, 6000)
	if double > 3*small {
		t.Fatalf("6,000 rules allocated %d bytes, 3,000 allocated %d", double, small)
	}
	whole := spanChainAllocates(t, 30000)
	t.Logf("allocated %d bytes for 3,000 rules, %d for 6,000 and %d for 30,000",
		small, double, whole)
	if whole > 15*small {
		t.Fatalf("30,000 rules allocated %d bytes, 3,000 allocated %d", whole, small)
	}
}

func TestTokenSpanWalkFollowsEachAlternativeOfARuleNoFurther(t *testing.T) {
	// 30 rules of two alternatives each: 2³⁰ paths to the end of the chain,
	// and at most 2⁵ within the window.
	start := time.Now()
	if _, err := EmitGrammarSpec(spanChain(30, true), &ConvertOptions{Tag: "x"}); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("compiled after %v", elapsed)
	}
}

func TestTokenSpanCountsARepetitionOfAtMostNoneAsNoTokens(t *testing.T) {
	// Two alternatives share a repetition of at most none, of an unbounded
	// element, and then n literals. The repetition spans no tokens, so the
	// prefix spans n: within the lookahead at 4, and factored at 5. The walk
	// this replaced read the repetition as NaN, which always passed the
	// lookahead; TypeScript's took the same NaN as within it, and Rust's
	// read the repetition as unbounded.
	shared := func(n int) *Grammar {
		top := &Production{Name: "top"}
		for _, last := range []string{"y", "z"} {
			alt := Sequence{{Kind: KindRep, Min: 0, Max: 0,
				Inner: &Element{Kind: KindStar, Inner: elLit("x")}}}
			for i := 0; i < n; i++ {
				alt = append(alt, elLit(fmt.Sprintf("a%d", i)))
			}
			top.Alts = append(top.Alts, append(alt, elLit(last)))
		}
		return &Grammar{Productions: []*Production{top}}
	}
	factored := func(n int) []string {
		spec, err := EmitGrammarSpec(shared(n), &ConvertOptions{Tag: "x"})
		if err != nil {
			t.Fatalf("%d literals: %v", n, err)
		}
		names := []string{}
		for name := range spec.Rule {
			if strings.Contains(name, "$fact") {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		return names
	}
	if got := factored(4); len(got) != 0 {
		t.Fatalf("4 literals factored into %v", got)
	}
	if got := factored(5); !reflect.DeepEqual(got, []string{"top$fact0"}) {
		t.Fatalf("5 literals factored into %v", got)
	}
}
