package bnf

import (
	"reflect"
	"testing"
)

// partitionRanges is the core of the overlapping-class fix, and the one
// piece of it with a property worth asserting directly: the spans it
// returns must be pairwise disjoint, and every input coverage must be an
// exact union of them. Everything downstream — the token sets, the
// lexer's freedom from allocation order — rests on those two facts.
func TestPartitionRanges(t *testing.T) {
	cases := []struct {
		name string
		in   [][]charRange
		want []charRange
	}{
		{
			// The reported case: %x30-39 against %x31-39.
			name: "containment",
			in:   [][]charRange{{{'0', '9'}}, {{'1', '9'}}},
			want: []charRange{{'0', '0'}, {'1', '9'}},
		},
		{
			// Neither contains the other — the case a containment-only
			// fix would miss. 13% of overlapping pairs in the corpus.
			name: "partial overlap",
			in:   [][]charRange{{{'0', '5'}}, {{'3', '9'}}},
			want: []charRange{{'0', '2'}, {'3', '5'}, {'6', '9'}},
		},
		{
			// Gaps between coverages are not atoms.
			name: "disjoint inputs stay whole",
			in:   [][]charRange{{{'a', 'c'}}, {{'x', 'z'}}},
			want: []charRange{{'a', 'c'}, {'x', 'z'}},
		},
		{
			name: "multi-span coverage",
			in:   [][]charRange{{{'A', 'Z'}, {'a', 'z'}}, {{'A', 'F'}}},
			want: []charRange{{'A', 'F'}, {'G', 'Z'}, {'a', 'z'}},
		},
		{
			name: "identical coverages collapse",
			in:   [][]charRange{{{'0', '9'}}, {{'0', '9'}}},
			want: []charRange{{'0', '9'}},
		},
		{
			name: "no input",
			in:   nil,
			want: nil,
		},
	}

	for _, c := range cases {
		got := partitionRanges(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
			continue
		}
		// Disjoint, and in order.
		for i := 1; i < len(got); i++ {
			if got[i-1].hi >= got[i].lo {
				t.Errorf("%s: atoms overlap at %d: %v", c.name, i, got)
			}
		}
		// Every input coverage is an exact union of atoms: each of its
		// characters is in exactly one atom, and no atom straddles its
		// boundary.
		for _, coverage := range c.in {
			for _, r := range coverage {
				for cp := r.lo; cp <= r.hi; cp++ {
					n := 0
					for _, a := range got {
						if a.lo <= cp && cp <= a.hi {
							n++
							if a.lo < r.lo || r.hi < a.hi {
								t.Errorf("%s: atom %v straddles coverage %v",
									c.name, a, r)
							}
						}
					}
					if n != 1 {
						t.Errorf("%s: %q is in %d atoms, want 1", c.name, cp, n)
					}
				}
			}
		}
	}
}

func TestClassPattern(t *testing.T) {
	// The escape the ABNF front-end already uses for a %x range on this
	// side, so an atom's token name reads like every other class token.
	if got := classPattern('0', '9'); got != `[\x{0030}-\x{0039}]` {
		t.Errorf("classPattern: got %q", got)
	}
}
