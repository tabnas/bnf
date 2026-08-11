# Differences from the TypeScript compiler

The Go port follows TS (see AGENTS.md). This file records where it is
currently behind, so the gap is a statement rather than a surprise.

## Contested-alternative machinery (TS only, as of tabnas/bnf#8)

The TS compiler decides alternatives that a scannerless grammar
contests at the character level. None of it is ported yet:

- **FOLLOW₂ pair exit guards** on contested repetitions
  (`computeFollowPairs`, `pairExitGuards`).
- **Keyword-shadow guards** — literal-headed dispatch entries contested
  by class-headed entries get 2-token guards and reordering
  (`synthKeywordGuards`, `reorderKeywordShadow`).
- **Left factoring** — an IR pass (`leftFactor`) that factors
  alternatives sharing a prefix beyond dispatch lookahead into a
  common prefix plus a transparent helper, with one-level head-ref
  inlining.
- **Specificity ordering and contested K-token peeks**
  (`specificityPermute`, `altHeadContested`, `contestedByFollow`) —
  longer lookahead first among overlapping class heads; contested
  ref-only FIRST peeks fan out to K-token prefixes.
- **ε-derivation re-issue for nullable dispatch alternatives**, and
  `repeatHelper` surviving desugar for upstream-created helpers.
- **Engine matcher-name reservation**: the TS literal-token allocator
  reserves `#AA`/`#BD`/`#UK` in addition to the names both ports
  already reserved.

Practical impact: grammars whose terminals never contest a character —
every ABNF and EBNF grammar in the shared fixtures — behave the same in
both ports, because all of the machinery above is inert for them. The
GBNF corpus work (tabnas/gbnf) additionally depends on the engine's
negotiated lexing (`lex.relex`, tabnas/parser#76), which the Go engine
also does not implement yet; porting this compiler machinery without
that engine option would be exercise without effect.

When the Go engine gains negotiated lexing, port the items above in the
same order they landed in TS, moving each from this list into the code.

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
