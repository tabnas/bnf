# Differences from the TypeScript compiler

The Go port follows TS (see AGENTS.md). This file records where the two
stand, so any gap is a statement rather than a surprise.

## Contested-alternative machinery: Aligned (as of tabnas/bnf#13)

The TS compiler decides alternatives that a scannerless grammar
contests at the character level. All of it is now ported:

- **FOLLOW / FOLLOW₂ repetition-exit guards**: Aligned
  (`computeFollowSets`, `computeFollowPairs`, `pairExitGuards` —
  `go/follow.go`, `go/contested.go`).
- **Keyword-shadow guards**: Aligned — literal-headed dispatch entries
  contested by class-headed entries get 2-token guards and reordering
  (`synthKeywordGuards`, `reorderKeywordShadow` — `go/contested.go`).
- **Left factoring**: Aligned — an IR pass that factors alternatives
  sharing a prefix beyond dispatch lookahead into a common prefix plus a
  transparent helper, with one-level head-ref inlining (`leftFactor`,
  `factorOnce`, `inlineHeadRef` — `go/factor.go`).
- **Specificity ordering and contested K-token peeks**: Aligned
  (`specificityPermute`, `altHeadContested`, `contestedByFollow` —
  `go/contested.go`).
- **ε-derivation re-issue for nullable dispatch alternatives**: Aligned
  (the `nullableImpls` loop in `emitProduction`), and `repeatHelper`
  survives desugar for upstream-created helpers.
- **Engine matcher-name reservation**: Aligned — the literal-token
  allocator refuses `#AA`/`#BD`/`#UK`/`#ZZ`/`#SP`/`#LN`/`#CM` and the
  builtin token names, falling through to the numbered form
  (`isEngineOwnedToken` — `go/compiler.go`).

Character coverage — the question all three guards ask, "can these two
tokens claim the same input character?" — lives in `go/ranges.go`. One
Go-specific detail there has no TS counterpart: the Go emitter writes
RE2's `\x{…}` brace form where JS writes `\u{…}`, so the coverage reader
must understand both. Reading only `\xHH` makes every Go-emitted class's
coverage UNKNOWN, which silently switches off every check downstream.

### How this was verified

Against the ACCEPT/REJECT tables that `tabnas/gbnf`'s
`ts/test/corpus.test.js` pins — the contract the port is written to:

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

`abnf` and `ebnf` — the other front-ends on this compiler — have no
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
class — 43 such pairs in `c.gbnf`), whichever matches first performs the
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
shorthand, which is the one spelling both accept — so the emitted shape
is the same in each port.

One step inside `resolveSuffixDebts` is still TS-only: deciding whether
the suffix and the loop *compete*. TS compares the two tokens' character
coverage (`tokensOverlap`), so a fixed `"a"` token and a `[a-z]` match
token read as competing; Go compares token identity, and a grammar whose
contest crosses a fixed/match token boundary gets no counter there. That
comparison is part of the contested-alternative machinery above, and
acting on its answer needs the same negotiated lexing — with the Go
engine as it stands, a guard emitted for that shape would be inert
anyway, because the class matcher wins the first cut and the enclosing
suffix can never be re-cut to its own token. Port it with the rest of
that list.
