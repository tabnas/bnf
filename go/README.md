# bnf (Go)

The shared compiler behind the BNF-family grammar front-ends for the
[tabnas](https://github.com/tabnas/parser) parsing engine.

This package holds no notation of its own. It defines a grammar IR and
compiles it into a tabnas `GrammarSpec`; a front-end parses one concrete
syntax into that IR:

```
ABNF / GBNF / EBNF text ──front-end──▶ Grammar ──EmitGrammarSpec──▶ GrammarSpec
```

| Front-end | Notation |
|---|---|
| [`tabnas/abnf`](https://github.com/tabnas/abnf) | RFC 5234 ABNF |
| [`tabnas/gbnf`](https://github.com/tabnas/gbnf) | llama.cpp GBNF |
| [`tabnas/ebnf`](https://github.com/tabnas/ebnf) | EBNF (best effort) |

Everything downstream of the IR is shared and lives here: desugaring
repetition into helper rules, left-recursion elimination, tail-repeat
rewriting, the probe/rewind dispatcher for prefixes beyond the engine's
bounded lookahead, literal lifting, token allocation, first-set analysis,
and `$stepN` chain emission.

## Install

```bash
go get github.com/tabnas/bnf/go@latest
```

```go
import bnf "github.com/tabnas/bnf/go"
```

## One example

`EmitGrammarSpec` is the entry point: an IR in, a `*tabnas.GrammarSpec`
and an `error` out.

```go
spec, err := bnf.EmitGrammarSpec(&bnf.Grammar{
	Productions: []*bnf.Production{
		{Name: "val", Alts: []bnf.Sequence{{
			{Kind: bnf.KindRef, Name: "add"},
		}}},
		{Name: "add", Alts: []bnf.Sequence{{
			{Kind: bnf.KindToken, Name: "#NR"},
		}}},
	},
}, &bnf.ConvertOptions{Tag: "demo"})
```

A `Sequence` is a slice of `*Element`, and an element's `Kind` picks what
it is — a rule reference, a lexer token, or a literal terminal.

Also exported: `EliminateLeftRecursion`, for a front-end that wants to
inspect or test the rewritten IR on its own; `AttachActions` and
`AttachActionSlots`, to bind alt-actions to an emitted spec; `MarkListing`,
to list the alternate marks a grammar produced; and `SpecToJSON` /
`SpecToData`, to serialise a spec for inspection or a golden test.

## Documentation

Full documentation follows the [Diátaxis](https://diataxis.fr) framework:

- [Tutorial](doc/tutorial.md) — a guided first compile, start to finish.
- [How-to guide](doc/guide.md) — short recipes for individual tasks.
- [Reference](doc/reference.md) — the public API and every option.
- [Concepts](doc/concepts.md) — the IR contract, the compiler passes, and
  how the Go version differs from TypeScript.

For the canonical TypeScript implementation, see
[`../ts/README.md`](../ts/README.md).

## License

Copyright (c) 2025 Richard Rodger and other contributors, MIT License.
