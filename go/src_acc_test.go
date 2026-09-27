// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

package bnf

// The closure-mode tree builders grow a node's `src` in place (srcAcc), so
// that a repetition, which grows one node by one item at a time, costs time
// linear in its text. What they build must be exactly what concatenation
// built: these hold the accumulator to that, piece by piece and on every
// repetition shape, against the engine's own builtins.

import (
	"strings"
	"testing"

	tabnas "github.com/tabnas/parser/go"
)

func TestSrcAccAppendsWhatConcatenationWould(t *testing.T) {
	acc := srcAcc{}
	n := map[string]any{"src": ""}
	want := ""
	for i := 0; i < 2000; i++ {
		piece := strings.Repeat(string(rune('a'+i%26)), 1+i%7)
		acc.append(n, piece)
		want += piece
		if n["src"] != want {
			t.Fatalf("after %d pieces src is %d bytes, want %d", i+1, len(n["src"].(string)), len(want))
		}
	}
	if len(acc) != 1 {
		t.Errorf("one growing node keeps %d builders", len(acc))
	}
}

func TestSrcAccHonoursTextSetElsewhere(t *testing.T) {
	long := strings.Repeat("x", 2*srcAccMin)
	acc := srcAcc{}
	n := map[string]any{"src": long}
	acc.append(n, "a")
	// Something other than the accumulator replaces src: a user action,
	// say. The next piece goes after what is there now.
	n["src"] = "replaced"
	acc.append(n, "b")
	if n["src"] != "replacedb" {
		t.Errorf("src %q after a replacement", n["src"])
	}
	// The same length as the builder's text, but other text.
	acc.append(n, long)
	other := strings.Repeat("y", len(n["src"].(string)))
	n["src"] = other
	acc.append(n, "c")
	if n["src"] != other+"c" {
		t.Error("an equal-length replacement was not honoured")
	}
	// Not a string at all reads as no text, as concatenation read it.
	n["src"] = 7
	acc.append(n, long)
	if n["src"] != long {
		t.Error("a non-string src did not read as empty")
	}
}

func TestSrcAccKeepsSharedTextApart(t *testing.T) {
	// Two nodes holding one string: growing either must not show in the
	// other, however the builder's buffer is shared.
	acc := srcAcc{}
	a := map[string]any{"src": ""}
	acc.append(a, strings.Repeat("a", 3*srcAccMin))
	acc.append(a, "1")
	b := map[string]any{"src": a["src"]}
	base := a["src"].(string)
	acc.append(a, "AAA")
	acc.append(b, "B")
	acc.append(a, "more")
	if a["src"] != base+"AAAmore" || b["src"] != base+"B" {
		t.Errorf("shared text crossed: a ends %q, b ends %q",
			a["src"].(string)[len(base):], b["src"].(string)[len(base):])
	}
	if base != strings.Repeat("a", 3*srcAccMin)+"1" {
		t.Error("a string handed out earlier changed")
	}
}

func TestSrcAccWithoutAParseConcatenates(t *testing.T) {
	var acc srcAcc
	n := map[string]any{"src": strings.Repeat("z", srcAccMin)}
	acc.append(n, "!")
	if n["src"] != strings.Repeat("z", srcAccMin)+"!" {
		t.Error("a nil accumulator did not concatenate")
	}
	if srcAccOf(nil) != nil || srcAccOf(&tabnas.Context{}) != nil {
		t.Error("an accumulator without a parse bag to keep it in")
	}
}

func TestSrcAccBuildsTheTreesTheBuiltinsBuild(t *testing.T) {
	// The engine's builtins grow src by concatenation; the closures grow it
	// in place. Over two thousand items, text well past srcAccMin, on every
	// repetition shape, the two build the same value.
	const n = rdN / 5
	for _, c := range rdCases() {
		t.Run(c.name, func(t *testing.T) {
			src := c.make(n)
			closures, _ := newRdParser(t, c.grammar(), rdOpts{}).parse(t, src)
			builtins, _ := newRdParser(t, c.grammar(), rdOpts{builtins: true}).parse(t, src)
			if !valueEquals(closures, builtins) {
				t.Errorf("closures and builtins built different values over %d items", n)
			}
		})
	}
}
