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
points they spell; reading only `\xHH` makes every Go-emitted class's
coverage UNKNOWN, which silently switches off every check downstream.
`\a` is BEL and escaped ASCII punctuation is itself, while JavaScript's
`\u`, the zero-width `\A` and `\z`, and the quoting `\Q…\E` name no code
point. Where the two dialects read an escape differently (`\a` is the
letter `a` to JavaScript), each port reads it as its own matcher does,
so the same pattern can dispatch at a different depth.

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

## FOLLOW-guard emission order: divergent, behaviour-neutral

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
