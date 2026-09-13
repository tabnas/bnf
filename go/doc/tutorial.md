# Tutorial: your first compile (Go)

This walks you from nothing to a working parser, built out of a grammar
IR rather than out of notation text. Follow it in order; each step builds
on the last. When you finish you will have compiled an IR into a tabnas
`GrammarSpec`, installed it on an engine, parsed input with it, added
repetition and a start rule, read a compile error, and serialised the
result.

For recipes covering one task at a time, see the
[how-to guide](guide.md). For exact signatures and every option, see the
[reference](reference.md). For what the compiler does between the IR and
the spec, see [concepts](concepts.md).

## 1. Install

`bnf` is the shared compiler behind the BNF-family front-ends. The
engine it compiles for is a dependency of the module, so one `go get` is
enough:

```bash
go get github.com/tabnas/bnf/go@latest
```

```go
import (
    bnf "github.com/tabnas/bnf/go"
    tabnas "github.com/tabnas/parser/go"
)
```

## 2. Build the smallest grammar

This package holds no notation. You hand it a `*bnf.Grammar`, which is a
list of productions, each an alternation of sequences of elements:

```go
grammar := &bnf.Grammar{Productions: []*bnf.Production{
    {Name: "val", Alts: []bnf.Sequence{
        {{Kind: bnf.KindToken, Name: "#NR"}},
    }},
}}
```

A `Sequence` is a `[]*Element`, and an element's `Kind` says what it is.
Three kinds carry the whole of this tutorial:

| Kind | What it matches | Field that carries it |
|---|---|---|
| `bnf.KindToken` | a lexer token the engine already provides | `Name` |
| `bnf.KindTerm` | a literal the compiler allocates a token for | `Literal` |
| `bnf.KindRef` | another production by name | `Name` |

`#NR` is one of four builtin tokens. `bnf.BuiltinTokens()` returns the
whole map: `NR`, `ST`, `TX` and `VL`, for numbers, strings, bare text and
keyword values.

## 3. Compile it

`EmitGrammarSpec` takes the IR and options, and returns a
`*tabnas.GrammarSpec`:

```go
spec, err := bnf.EmitGrammarSpec(grammar, &bnf.ConvertOptions{
    Start: "val",
    Tag:   "demo",
})
```

`Tag` is the notation's own name, and it is stamped on every alternate
the compile emits. Pass the syntax your front-end reads, so a rule stack
or a diagnostic names what the author actually wrote. `Start` names the
production the engine begins at.

## 4. Install it and parse

A spec goes onto a bare engine with `Grammar`:

```go
j := tabnas.Make()
if err := j.Grammar(spec); err != nil {
    return err
}
out, err := j.Parse("42")
```

`out` is the AST the compiler's tree builders produce: a
`map[string]any` with `rule`, `src` and `kids`.

```go
// map[string]any{"rule": "val", "src": "42", "kids": []any{}}
```

## 5. Add structure

Now something with nesting and repetition. `KindStar` is zero or more of
its `Inner` element, and a production can reference itself through
another:

```go
grammar := &bnf.Grammar{Productions: []*bnf.Production{
    {Name: "list", Alts: []bnf.Sequence{{
        {Kind: bnf.KindTerm, Literal: "("},
        {Kind: bnf.KindStar, Inner: &bnf.Element{
            Kind: bnf.KindRef, Name: "item"}},
        {Kind: bnf.KindTerm, Literal: ")"},
    }}},
    {Name: "item", Alts: []bnf.Sequence{
        {{Kind: bnf.KindToken, Name: "#NR"}},
        {{Kind: bnf.KindRef, Name: "list"}},
    }},
}}

spec, err := bnf.EmitGrammarSpec(grammar, &bnf.ConvertOptions{
    Start: "list", Tag: "demo",
})
```

Two productions go in; thirteen rules come out. The compiler desugars
the star into a helper production, adds the dispatch machinery the
engine needs, and wraps the start rule. Parsing `(1 2 (3))` gives a
`list` node with three `item` children, the last of them holding a
nested `list`:

```go
j := tabnas.Make()
j.Grammar(spec)
out, _ := j.Parse("(1 2 (3))")
// rule "list", src "(12(3))", three item kids
```

The `src` has no spaces in it because the engine's lexer skips them
between tokens, not because anything was lost.

## 6. Read a compile error

A grammar that references a production it does not define fails to
compile, and the failure says which rule:

```go
_, err := bnf.EmitGrammarSpec(&bnf.Grammar{Productions: []*bnf.Production{
    {Name: "a", Alts: []bnf.Sequence{
        {{Kind: bnf.KindRef, Name: "missing"}},
    }},
}}, &bnf.ConvertOptions{Tag: "demo"})

// err.Error() == "demo: rule 'a' references unknown rule 'missing'"
```

Note the prefix. It is the `Tag` you passed, not this package's name,
because the person reading it wrote `demo` syntax and has never heard of
`bnf`.

The error is a `*bnf.EmitError`, which carries the rule and, when the
front-end recorded one, a source span:

```go
var ee *bnf.EmitError
if errors.As(err, &ee) {
    ee.Rule // "a"
    ee.Sp   // *bnf.SrcSpan, or nil when nothing recorded one
}
```

Spans are optional everywhere. A front-end that fills in `Element.Sp`
and `Production.Sp` gets ranged errors; one that does not compiles to
exactly the same grammar.

## 7. Serialise the result

`SpecToJSON` renders a spec as JSON, which is what you want for a golden
test or for an embedded grammar:

```go
text := bnf.SpecToJSON(spec, 2)
```

The second argument is the indent. `SpecToData` returns the same content
as a `map[string]any` instead of text. Both have `Err` variants that
surface the failure rather than returning an empty result:

```go
data, err := bnf.SpecToDataErr(spec)
```

## Where to go next

- [How-to guide](guide.md). Recipes: semantic actions, recognition-only
  specs, whole-word keywords, rule removal, source spans.
- [Reference](reference.md). Every exported name, every option field,
  and the error types.
- [Concepts](concepts.md). What the passes between the IR and the spec
  do, and why the IR is the boundary.
- The root [README](../../README.md) and [AGENTS.md](../../AGENTS.md).
  How this package relates to the front-ends that feed it.
