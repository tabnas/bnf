# How-to guide (Go)

Recipes for the tasks a front-end actually has to do, one at a time. For
a guided introduction see the [tutorial](tutorial.md); for exact
signatures see the [reference](reference.md); for what happens between
the IR and the spec see [concepts](concepts.md).

```go
import (
    bnf "github.com/tabnas/bnf/go"
    tabnas "github.com/tabnas/parser/go"
)
```

## Compile an IR and install it

`EmitGrammarSpec` is the entry point, and `Grammar` on an engine
instance is where the result goes:

```go
spec, err := bnf.EmitGrammarSpec(grammar, &bnf.ConvertOptions{
    Start: "top",
    Tag:   "demo",
})
if err != nil {
    return err
}
j := tabnas.Make()
if err := j.Grammar(spec); err != nil {
    return err
}
value, err := j.Parse(src)
```

Always pass `Tag`. It is stamped on every emitted alternate, and it is
also the prefix on every diagnostic this compiler raises, so a compile
failure reads `demo: ...` rather than naming a package the author has
never used. Omitting it defaults the tag to `bnf`, which asserts nothing
true about the syntax. It reaches only what this compiler raises: a
front-end's parse error on the grammar source comes before `Tag` is
applied and keeps that front-end's own fixed prefix.

## Attach a semantic action to an alternate

Actions bind to alternates by **mark**, and marks are off by default.
Turn them on, ask what they are called, then bind:

```go
spec, _ := bnf.EmitGrammarSpec(&bnf.Grammar{Productions: []*bnf.Production{
    {Name: "op", Alts: []bnf.Sequence{
        {{Kind: bnf.KindTerm, Literal: "inc"}},
        {{Kind: bnf.KindTerm, Literal: "dec"}},
    }},
}}, &bnf.ConvertOptions{Tag: "demo", Start: "op", Marks: true})

bnf.MarkListing(spec)
// op  o:INC  s:#INC
// op  o:DEC  s:#DEC
```

The mark comes from the alternate's first element, so a literal `"inc"`
becomes the token `#INC` and the mark `INC`. An action ref is
`@rule:phase:mark`, where the phase is `o` for open or `c` for close:

```go
err := bnf.AttachActions(spec, bnf.ActionsMap{
    "@op:o:INC": {func(r *tabnas.Rule, ctx *tabnas.Context) {
        // runs when the INC alternate opens
    }},
})
```

The value is a slice, so several functions can share one ref; they run
in order. A ref that matches no alternate is an error rather than a
silent no-op:

```
demo: action ref '@op:o:inc' matches no open alt with mark 'inc' in rule 'op'
```

Use `MarkListing` rather than guessing the mark, which is what that
message is telling you to do.

## Declare a slot without supplying a function

A pure-data spec cannot carry closures, so a grammar meant for embedding
declares the slot by name and lets whoever installs it fill in the
function later:

```go
err := bnf.AttachActionSlots(spec, []string{"@op:o:DEC"})
```

A slot is a declaration, not a binding. Installing a spec whose slot
nobody filled in is refused by the engine:

```
Grammar: unknown action function reference: @op:o:DEC
```

So declare slots in the grammar you ship, and call `AttachActions` with
the same refs before `Grammar`.

Slots are for alternate actions only. A rule-phase ref is refused, since
a slot has nowhere to hang on one.

## Emit a grammar you can embed

Two steps. Compile with `Builtins: true`, which makes every action a
`@name$` string rather than a closure, then reduce:

```go
spec, _ := bnf.EmitGrammarSpec(grammar, &bnf.ConvertOptions{
    Start: "top", Tag: "demo", Builtins: true,
})
pure, err := bnf.ToPureSpec(spec)
```

`pure` is a `map[string]any` of plain data, ready for `encoding/json`.
Skipping the first step fails loudly rather than emitting something that
cannot be serialised:

```
demo: spec still contains closures; convert with `builtins: true` for
pure-data output. Stray ref(s): @abnf_a0, @abnf_a1
```

## Emit a recognition-only grammar

`ToRecognitionSpec` strips the tree and value builders out, leaving a
grammar that decides whether input matches and builds nothing:

```go
rec, err := bnf.ToRecognitionSpec(spec)
```

Unlike `ToPureSpec`, this does not need the `Builtins: true` compile for
an ordinary grammar: the tree and value builders it would have tripped
over are the very things it removes. It refuses only when **control**
logic is still a closure, which happens when a grammar needing a probe
dispatcher is compiled without builtins.

## Make keyword literals match whole words

By default a literal compiles to an anchored, case-insensitive regex, so
`option` also matches the first six characters of `optional`. The
`WordKeywords` option adds a word-boundary guard to any literal ending
in a word character:

```go
spec, _ := bnf.EmitGrammarSpec(grammar, &bnf.ConvertOptions{
    Start: "stmt", Tag: "demo", WordKeywords: true,
})
// #OPTION: "@~/^option/i"    without it
// #OPTION: "@~/^option\b/i"  with it
```

Punctuation is unaffected, since a word boundary after `+` would be
wrong.

## Compile a keyword class to one token

A language whose identifiers admit keywords writes its identifier rule
as a choice: `ident = TX / "message" / "enum" / "option" / ...`. Each
place the grammar peeks at an identifier would then dispatch on every
member, and a rule that peeks at two of them in a row on their product.
The `TokenClasses` option compiles such a production, one whose
alternatives are all single literals or tokens, to one engine token set
named after it, and every lookahead position that would have named a
member names the set instead:

```go
spec, _ := bnf.EmitGrammarSpec(grammar, &bnf.ConvertOptions{
    Start: "file", Tag: "demo", WordKeywords: true, TokenClasses: true,
})
// spec.Options["tokenSet"]["ident"] lists the members' tokens; an
// alternate that peeks an identifier carries `s: "#ident"`.
```

The tree is the same with the option on or off. Where the plain compile
keeps a reference to the class (a non-leading position), the rule keeps
one alternate per member and builds its node as before; where the plain
compile inlines it (a leading reference, which Paull's substitution
expands into one alternative per member), the one set token is consumed
instead, which is the same tree without the fan-out. Only the size of
what the rules around it dispatch on changes.

## Control literal case sensitivity

ABNF strings are case-insensitive by default, and this compiler keeps
that. Set both fields to override it for one literal:

```go
&bnf.Element{
    Kind: bnf.KindTerm, Literal: "If",
    CaseSensitive: true, HasCaseSens: true,
}
```

`HasCaseSens` is what distinguishes "explicitly insensitive" from "not
specified", which Go's zero value cannot do on its own.

A literal with no ASCII letter in it is case-sensitive either way, and
`IsEffectivelyCaseSensitive` reports the answer the compiler will use.
`TermKey` shows the identity a terminal is allocated under: `cs:+` for
that literal, `ci:if` for an insensitive word.

## Remove rules from the host instance

`Grammar.Remove` names rules or tokens to drop, and `ClearAll` wipes the
instance first. Both survive into the spec:

```go
grammar := &bnf.Grammar{
    Productions: []*bnf.Production{ /* ... */ },
    Remove:      []string{"val", "map"},
}
```

A removal is carried as a nil rule entry, which is how the engine
represents one. So `spec.Rule["val"]` is present and nil; it has not
been left out.

## Name the author's rule, not the machinery's

A compiled grammar carries an order of magnitude more rules than the
author wrote, and every generated name shows up in rule stacks, hover
and completion. The compiler exports a map from each generated name back
to the production it descends from:

```go
provenance, _ := spec.Meta["provenance"].(map[string]any)
provenance["__start__"] // "list"
```

It is on unless you turn it off. For an embedded grammar where size
matters more than names:

```go
off := false
spec, _ := bnf.EmitGrammarSpec(grammar, &bnf.ConvertOptions{
    Start: "list", Tag: "demo", Provenance: &off,
})
// spec.Meta has no "provenance" key
```

The field is a pointer for that reason: absent means on, and only an
explicit `false` turns it off.

## Give your errors a source range

Fill in `Sp` on the elements and productions your parser builds, and a
compile failure can point at the text:

```go
{Kind: bnf.KindRef, Name: "expr", Sp: &bnf.SrcSpan{S: 12, E: 16, R: 2, C: 5}}
```

`S` and `E` are offsets in the same units your engine tokens use, so
copy `sI` across rather than converting. In Go those are **bytes**,
where TypeScript counts UTF-16 code units; convert at the boundary that
knows the document encoding, such as an LSP server, because the IR does
not hold the source text and cannot convert for you.

`R` and `C` are 1-based, so zero means "not recorded". The whole span is
a pointer for the same reason: `SrcSpan{S: 0, E: 0}` is a real empty
span at the start of a file and must not read as absence.

## Read a compile failure

```go
var ee *bnf.EmitError
if errors.As(err, &ee) {
    ee.Rule // the rule being compiled
    ee.Sp   // where, when the IR knew
}
```

`EmitError` is what the author-facing diagnostics raise. The rest raise
`*bnf.ParseError` or a plain error, so match on `error` and narrow when
you want the span. A purely left-recursive production is an error return
like the rest: the pass raising it panics internally with an
`*bnf.EmitError`, and `EmitGrammarSpec` recovers and returns it.

## Inspect the left-recursion rewrite

`EliminateLeftRecursion` runs that pass alone, which is what you want in
a test that pins the rewrite rather than the emitted spec:

```go
rewritten := bnf.EliminateLeftRecursion(grammar)
```

It returns a new `*Grammar`; the input is not modified.

## Serialise a spec for a golden test

```go
text := bnf.SpecToJSON(spec, 2)   // indent 2
data := bnf.SpecToData(spec)      // map[string]any
```

Both return an empty result on failure. The `Err` variants,
`SpecToJSONErr` and `SpecToDataErr`, return the failure instead, and are
the ones to use anywhere the output is not immediately eyeballed.

The TypeScript recipes for the same tasks are in
[`../../ts/README.md`](../../ts/README.md).
