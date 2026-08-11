# Agents Guide — bnf

## What this project is

`@tabnas/bnf` is the **notation-neutral compiler** shared by the
BNF-family grammar front-ends (`abnf`, `gbnf`, `ebnf`). It compiles a
grammar IR into a tabnas `GrammarSpec`. It parses no syntax itself.

```
front-end (text -> IR)  ─▶  Grammar  ─▶  emitGrammarSpec  ─▶  GrammarSpec
        abnf/gbnf/ebnf                        (here)
```

## The one rule that matters

**Nothing in this package may know what notation a grammar was written
in.** If a change here needs to ask "is this ABNF?", it belongs in the
front-end instead. The IR is the contract: a front-end lowers its syntax
into `Element`/`Production`, and everything downstream is shared.

Two consequences worth stating, because both look like exceptions and
are not:

- **`prose` elements** (`NR = <number>`) are in the IR and resolved here.
  Prose comes from ABNF, but "a terminal named after a built-in lexer
  token" is useful to any notation, so the *mechanism* is neutral even
  though only ABNF currently spells it that way.
- **`caseSensitive`** exists because notations disagree: ABNF quoted
  strings are case-insensitive by default, GBNF's are case-sensitive.
  The front-end states the intent; the emitter lowers it. Neither
  default is baked in here.

## Repository map

| Path | What it is |
|---|---|
| `ts/src/compiler.ts` | The IR types and the whole emit pipeline: desugar, left-recursion elimination (incl. suffix-debt counters for contested tail loops), tail repeats, probe dispatch, literal lifting, token allocation, first sets, chain emission. |
| `ts/src/spec.ts` | Spec-level transforms: recognition/pure lowering, jsonic serialisation, user-action attachment. Operates on an emitted `GrammarSpec`. |
| `ts/src/bnf.ts` | Package entry; re-exports the public surface. |
| `go/` | Go port (follows TS). |

## Provenance, and why the tests live downstream

This package was extracted from `@tabnas/abnf`. The extraction was
mechanical: every line of the old `converter.ts` landed either here or in
the ABNF front-end, and the split was checked by concatenating the two
halves back to the original.

**The verification oracle is `@tabnas/abnf`'s suite**, which exercises
this compiler hard — RFC 3986 (probe/ambiguity machinery), left
recursion, round-trip rendering, and a conformance run over 68
third-party `.abnf` files. After the split, all 300 of its tests passed
unchanged. When you change something here, run that suite as well as
this package's own; a green build here proves much less.

## Authority and alignment rules

1. **TypeScript is canonical.** When TS and Go disagree, TS wins.
2. A change to the emit pipeline affects every front-end. Run the
   downstream suites (`abnf`, `gbnf`, `ebnf`) before considering it done.
3. The `tag` option defaults to `'bnf'`; each front-end passes its own so
   emitted alts stay attributable. Do not hard-code a notation's tag.
4. `VERSION` in `ts/src/bnf.ts` and `go/bnf.go` MUST equal
   `ts/package.json` "version".

## Build & test

```bash
cd ts && npm install && npm run build && npm test
cd go && go build ./... && go test ./...
```
