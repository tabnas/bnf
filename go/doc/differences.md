# Differences from the TypeScript compiler

The Go port follows TS (see AGENTS.md). This file records where the two
stand, so any gap is a statement rather than a surprise.

## Contested-alternative machinery: Aligned (as of tabnas/bnf#13)

The TS compiler decides alternatives that a scannerless grammar
contests at the character level. All of it is now ported:

- **FOLLOW / FOLLOW₂ repetition-exit guards**: Aligned
  (`computeFollowSets`, `computeFollowPairs`, `pairExitGuards` in
  `go/follow.go` and `go/contested.go`).
- **Keyword-shadow guards**: Aligned. Literal-headed dispatch entries
  contested by class-headed entries get 2-token guards and reordering
  (`synthKeywordGuards`, `reorderKeywordShadow` in `go/contested.go`).
- **Left factoring**: Aligned. An IR pass that factors alternatives
  sharing a prefix beyond dispatch lookahead into a common prefix plus a
  transparent helper, with one-level head-ref inlining (`leftFactor`,
  `factorOnce`, `inlineHeadRef` in `go/factor.go`).
- **Specificity ordering and contested K-token peeks**: Aligned
  (`specificityPermute`, `altHeadContested`, `contestedByFollow` in
  `go/contested.go`).
- **ε-derivation re-issue for nullable dispatch alternatives**: Aligned
  (the `nullableImpls` loop in `emitProduction`), and `repeatHelper`
  survives desugar for upstream-created helpers.
- **Engine matcher-name reservation**: Aligned. The literal-token
  allocator refuses `#AA`/`#BD`/`#UK`/`#ZZ`/`#SP`/`#LN`/`#CM` and the
  builtin token names, falling through to the numbered form
  (`isEngineOwnedToken` in `go/compiler.go`).

Character coverage (the question all three guards ask, "can these two
tokens claim the same input character?") lives in `go/ranges.go`. Its
escape reader reads RE2, the dialect Go's regexp compiles every matcher
in, not JavaScript. The Go emitter writes RE2's `\x{…}` brace form where
JS writes `\u…`, so the reader takes `\x{…}` and `\xHH` as the code
points they spell. Reading only `\xHH` makes every Go-emitted class's
coverage UNKNOWN, which silently switches off every check downstream.
`\a` is the bell character and escaped punctuation is itself, while
JavaScript's `\u`, the zero-width `\A` and `\z`, and the quoting `\Q…\E`
name no code point. Where the two dialects read an escape differently,
each port reads it as its own matcher does, so the same pattern can
dispatch at a different depth: `\a` is the letter `a` to JavaScript.
`DIVERGENCE.md` records the depths.

### How this was verified

Against the ACCEPT/REJECT tables that `tabnas/gbnf`'s
`ts/test/corpus.test.js` pins, the contract the port is written to:

| | before | after |
|---|---|---|
| accept | 47/65 | **65/65** |
| reject | 30/30 | **30/30** |

Rule-for-rule, the Go and TS emitters now produce the **same entry set
for all 506 rules** of `c.gbnf`, compared with token names normalised
across the two runtimes' regex spellings.

The cases the port was explicitly required to get right, all matching
TS: `int f(){f(1);}` still rejects (it needs unbounded lookahead;
`int f(){f (1);}`, with the space, accepts), and the prefixed
identifiers `int intx(){intx = 3;}` and `int whilex(){whilex = 1;}`
accept.

`abnf` and `ebnf`, the other front-ends on this compiler, have no
skipped or expected-to-fail cases of these shapes in their Go suites,
and both suites pass against this compiler unchanged. Every guard here
is inert for a tokenising notation, which is why.

## FIRST- and FOLLOW-set emission order: divergent, behaviour-neutral

TS iterates a FOLLOW set in insertion order; Go sorts it, because Go map
iteration is randomised and the emitted spec has to be reproducible. 57
of `c.gbnf`'s 506 rules therefore list the same FOLLOW-guard entries in
a different order.

This cannot change a parse. Every entry in a FOLLOW re-issue group is
built from one template and differs only in the token it peeks: same
backtrack count, same push target, same fields. When two of them overlap
at the character level (`"<"` and `"<="`, `"int"` and the identifier
class; 43 such pairs in `c.gbnf`), whichever matches first performs the
identical action, so the order between them is not observable. Only a
group whose entries differed in `p` or `b` would be order-sensitive, and
the FOLLOW loop cannot produce one.

The same holds wherever the emitter fans an alternate out over a FIRST
set: the peek entries of an alternative that begins with a reference,
the entry per first token of a dispatched alternative, and the second
tokens of a keyword guard or a FOLLOW₂ guard. Each group comes from one
template, and TS lists it in the order its analysis met the tokens, Go
in token-name order. Over a corpus of 1,169 grammars captured as IR from
the front-ends' suites, 808 of which compile, 78 list one such group in
another order, and in each of them every group holds the same alternates
as in TS. Apart from the order of one reference's suffix-debt counters
(the next section), it is the one difference of order left between the
two serialised specs.

## The serialised spec: written in TypeScript's order

`ToJsonic` over `ToPureSpec`, `ToRecognitionSpec` or `SpecToData` writes
a spec in the order TS writes it. The rules come in the order the
emitter commits them, the tokens and token sets in the order it makes
them, and each alternate's keys in the order it sets them. For a grammar
both ports compile alike, the text is byte for byte the same, and
`go/oracle_text_test.go` holds the Go port to the canonical compiler's
own text for every shared fixture it can compare.

The order means something. The emitter allocates the match tokens as it
walks the rules, an engine numbers a spec's tokens in the order its
options list them, and of two tokens that a position expects, the lower
number is tried first. A notation's render writes the rules back in the
spec's order, so the text it writes compiles back to the same ranks.

A Go map keeps no order, so the order comes from three places:

| What | Where the order is kept |
|---|---|
| the rules | `GrammarSpec.RuleOrder`, which `EmitGrammarSpec` fills, and which the engine installs the rules in |
| the match tokens | `MatchOptions.TokenOrder` |
| the fixed tokens, the token sets, each alternate's keys | recorded for each spec `EmitGrammarSpec` returns, for as long as the spec lives |

A spec built by hand, loaded from JSON or copied has no record. It is
written in the order its own fields give, and in name order for the
rest.

The tree these functions return carries the order. Its root is a
`map[string]any`, and every object inside it is a `*tabnas.OrderedMap`,
which `ToJsonic` writes in its key order, and `encoding/json` writes in
its key order too. Code that walks the tree reads an object through its
`Keys` and `Vals`, or `Get`.

`options.match.tokenOrder` is not written: the order is in the keys, as
it is in the canonical compiler's spec, and the notations' renders
refuse a spec that gives the order as a list of its own. A Go engine
that loads a serialised spec through `tabnas.GrammarSpecFromJSON` takes
the rules' order from the keys but not the match tokens', so it ranks
two match tokens that claim the same input by name, where the TypeScript
engine ranks them by the order the spec lists them. That is the engine's
reading of the JSON, the same for a spec either compiler wrote. A spec
installed in memory keeps `TokenOrder` and ranks its tokens as the
emitter did.

Of the 808 grammars in the previous section's corpus that TS compiles,
450 serialise to its text byte for byte, 169 more do once their IR
spells its code points alike (a Go front-end writes `\x{0030}` where TS
writes `\u0030`, and a token is named after its pattern), and 5 more
once a class atom's hex digits are read in either case. 78 differ in the
order of a FIRST or FOLLOW group (as that section describes), and one in
the order of a reference's two suffix-debt counters, which a Go IR holds
in a map. Go refuses 16, whose patterns RE2 cannot compile. The other 89
differ in value, not order: the regular expression of a matcher as each
port writes it (the word guard `\b`, a code point outside the Basic
Multilingual Plane, the flags past `i`, an alternation that TS wraps in
a group, a `/` that TS escapes), the dispatch depth where the two
dialects read an escape apart, a rule named `__proto__`, which TS drops
when it serialises, and a rule named `CA`, which TS lifts to a literal
on the engine's comma token `#CA` where Go keeps it a rule.

## Hidden left recursion (ported, tabnas/bnf#6)

Both ports now handle left recursion reached through nullable sugar
(`A = ["x"] A "y" / "z"`):

- `expandNullableLeftPrefixes` splits the leading sugar into explicit
  present/absent alternatives, turning hidden left recursion into the
  direct kind Paull's machinery removes. This landed in TS with
  tabnas/bnf#4 and was missed by the port; it is here now.
- `resolveSuffixDebts` settles the tail loop the rewrite leaves behind,
  whose greediness contests a suffix of the alternative it came from.

The decision itself is about enclosing stack depth, not about how a
character is cut, so neither pass depends on negotiated lexing. The
counters and declarative conditions they emit (`n`, `c`) have identical
semantics in both engines, and the condition uses the scalar `$eq`
shorthand, which is the one spelling both accept, so the emitted shape
is the same in each port.

One step inside `resolveSuffixDebts` is still TS-only: deciding whether
the suffix and the loop *compete*. TS compares the two tokens' character
coverage (`tokensOverlap`), so a fixed `"a"` token and a `[a-z]` match
token read as competing; Go compares token identity, and a grammar whose
contest crosses a fixed/match token boundary gets no counter there. That
comparison is part of the contested-alternative machinery above, and
acting on its answer needs the same negotiated lexing. With the Go
engine as it stands, a guard emitted for that shape would be inert
anyway, because the class matcher wins the first cut and the enclosing
suffix can never be re-cut to its own token. Port it with the rest of
that list.


## Concurrency: Go serialises one emit at a time (deliberate)

`EmitGrammarSpec` takes a package-level lock; the TypeScript
`emitGrammarSpec` takes nothing. That is not a gap in the port: it is
the same design costing different things in the two languages.

Both compilers hold the notation's diagnostic prefix (`diagPrefix` /
`_diagName`) in module state for the duration of one emit, rather than
threading it through the twenty-odd functions that raise a diagnostic.
In TypeScript that is free: the pipeline is synchronous and the runtime
is single-threaded, so no second conversion can begin until the first
returns. In Go nothing stops two goroutines calling `EmitGrammarSpec` at
once, and when they did, the loser's diagnostics named the *winner's*
notation (`gbnf: rule 'x' …` on an error about a rule the ABNF author
wrote). The race detector reports it as a write-write race on
`diagPrefix`; what a user saw was the wrong prefix.

The lock is the proportionate fix rather than a threaded parameter,
because the alternative touches every pass in the package to solve a
problem in none of them, and because this is a once-per-grammar-install
call: serialising it costs nothing measurable.

The value-annotation plan had the same shape and is NOT under the lock:
it is now passed down from `emitGrammarSpec` to `emitChain` as an
argument (`nested []bool`) in both ports, because it is read in exactly
one place and threading it there is a two-argument change.

Two tests pin this. `TestValueAnnotationConcurrentEmitsDoNotShareAPlan`
runs two differently annotated grammars through 200 concurrent emits and
asserts each gets its own values; it also trips the race detector under
`go test -race`. `TestConcurrentEmitsKeepTheirOwnDiagPrefix` asserts the
user-visible symptom instead (each conversion's diagnostic names its own
notation), so it fails on a plain `go test` too, without depending on
whether CI passes `-race`.
