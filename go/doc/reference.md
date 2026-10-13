# Reference (Go)

The complete public surface of the Go `bnf` module: every exported name
with its signature, the IR types a front-end builds, every option field,
and the error contract. For a guided introduction see the
[tutorial](tutorial.md); for task recipes see the
[how-to guide](guide.md); for what the compiler does between the IR and
the spec see [concepts](concepts.md).

## Module

```bash
go get github.com/tabnas/bnf/go@latest
```

```go
import (
    bnf "github.com/tabnas/bnf/go"
    tabnas "github.com/tabnas/parser/go"
)
```

| | |
|---|---|
| Module | `github.com/tabnas/bnf/go` |
| Package | `bnf` |
| Engine | `github.com/tabnas/parser/go` (imported as `tabnas`) |
| Notation | none: this package compiles an IR, not text |
| Front-ends | [`abnf`](https://github.com/tabnas/abnf), [`gbnf`](https://github.com/tabnas/gbnf), [`ebnf`](https://github.com/tabnas/ebnf) |

## Compiling

### `func EmitGrammarSpec(grammar *Grammar, opts *ConvertOptions) (*tabnas.GrammarSpec, error)`

Compiles a grammar IR into a spec the engine can install. This is the
whole surface a front-end needs: parse your notation into a `*Grammar`,
then call this.

```go
spec, err := bnf.EmitGrammarSpec(grammar, &bnf.ConvertOptions{
    Start: "top", Tag: "demo",
})
```

The call is serialised internally, because the diagnostic prefix is
per-emit package state. Concurrent calls are safe and take their turn.
It returns an `*EmitError` for a grammar that would write more than
`MaxEmitUnits`.

### `func EliminateLeftRecursion(grammar *Grammar) *Grammar`

Runs the left-recursion pass alone, for a front-end that wants to
inspect or test the rewritten IR. Returns a new grammar; the input is
not modified. A grammar the pass refuses, a purely left-recursive rule,
a rule past `MaxLeftRecursionExpansion` or a grammar past
`MaxLeftRecursionElements`, panics with an `*EmitError`, which
`EmitGrammarSpec` returns as its error instead.

## Options

### `type ConvertOptions`

```go
type ConvertOptions struct {
    Start        string
    Tag          string
    Builtins     bool
    Marks        bool
    WordKeywords bool
    TokenClasses bool
    Provenance   *bool
}
```

| Field | Default | Effect |
|---|---|---|
| `Start` | none | The production the engine begins at. The compile wraps it as `__start__`. |
| `Tag` | `"bnf"` | Stamped on every emitted alternate, and the prefix on every diagnostic this compiler raises. Pass the notation's own name. A front-end's parse error on the grammar source is raised before these options apply and keeps that front-end's own fixed prefix. |
| `Builtins` | `false` | Emit actions as `@name$` strings rather than closures. Required by `ToPureSpec`. |
| `Marks` | `false` | Record a mark on each user alternate, so semantic actions can bind to it. |
| `WordKeywords` | `false` | Append a `\b` guard to a literal ending in a word character, so `option` does not match inside `optional`. |
| `TokenClasses` | `false` | Compile a production whose alternatives are all single literals or tokens to one engine token set, named after it, that stands as one token at lookahead positions. The tree is unchanged: a leading reference the plain compile would inline is consumed as the one token, and the rule keeps its alternates and its node everywhere else. A production stays plain when its name is empty, holds whitespace or matches an engine token, when a member consumes nothing, such as an empty literal, `#ZZ` or `#AA`, or when the grammar already spells its set name as a token. |
| `Provenance` | on | Emit `Meta["provenance"]`. A pointer, so absent means on and only an explicit `false` turns it off. |

The Go `WordKeywords` uses `\b` where TypeScript uses a
`(?![A-Za-z0-9_])` lookahead. RE2 has no lookahead, and the two are
equivalent here.

### `type CompileOptions`

```go
type CompileOptions struct {
    Start       string
    Tag         string
    Strict      bool
    Indent      int
    Recognition *bool // default true
}
```

Declared to mirror the TypeScript type of the same name, which is the
options argument to its `compileSpec`. Nothing in the Go package takes
one: there is no Go `compileSpec`, and a front-end compiling and
serialising in two steps passes `ConvertOptions` to `EmitGrammarSpec`
and an indent to `SpecToJSON`. It is exported so a port checking parity
can name it.

## The IR

A front-end's only job is to produce one of these.

### `type Grammar`

```go
type Grammar struct {
    Productions []*Production
    Ambiguities []AmbiguityReport
    Remove      []string
    ClearAll    bool
}
```

`Remove` names rules or tokens to drop from the host instance;
`ClearAll` wipes it first. A removal reaches the spec as a nil rule
entry, which is how the engine represents one, so `spec.Rule["val"]` is
present and nil rather than absent.

### `type Production`

```go
type Production struct {
    Name string
    Alts []Sequence
    // and compiler-written fields
}
```

`Name` and `Alts` are what a front-end fills in. The rest are written by
the passes: `Origin` (the author-written production a synthesised one
descends from), `NodeKind`, `TailRepeat`, `RepeatHelper`, `DebtGuard`,
`DebtOwed`, `ProbeDisp`, `ProbeHelper`, and `Sp`.

`NodeKind` decides how a production contributes to the output AST:
`"user"` (the default, and what an empty string means) emits a tagged
node; `"core"` and `"helper"` flatten into the enclosing node.

### `type Sequence`

```go
type Sequence []*Element
```

One alternative: the elements to match in order.

### `type Element`

```go
type Element struct {
    Kind ElemKind
    Sp   *SrcSpan
    // term
    Literal       string
    CaseSensitive bool
    HasCaseSens   bool
    TokenName     string
    // prose
    Text string
    // regex
    Pattern string
    Flags   string
    // ref
    Name string
    Debt map[string]int
    // opt / star / plus / rep
    Inner     *Element
    Min, Max  int
    DebtGuard string
    // group
    Alts []Sequence
}
```

One struct tagged by `Kind`, rather than a union. Which fields are read
depends on the kind:

| `Kind` | Means | Fields read |
|---|---|---|
| `KindTerm` | a literal terminal | `Literal`, `CaseSensitive`, `HasCaseSens`, `TokenName` |
| `KindRef` | a reference to another production | `Name`, `Debt` |
| `KindToken` | a builtin lexer token, such as `#NR` | `Name` |
| `KindRegex` | a regex terminal | `Pattern`, `Flags` |
| `KindOpt` | zero or one | `Inner` |
| `KindStar` | zero or more | `Inner`, `DebtGuard` |
| `KindPlus` | one or more | `Inner` |
| `KindRep` | a counted repetition | `Inner`, `Min`, `Max` |
| `KindGroup` | a nested alternation | `Alts` |
| `KindProse` | RFC 5234 prose, as in `NR = <number>` | `Text` |

`Max` is `bnf.MaxInfinity` (`1 << 30`) for an unbounded repetition.

`TokenName` is the preferred lexer token name, set when a terminal came
from a production that names it (`PL = "+"` gives `#PL`). Without it the
emitter derives a name from the literal, which for punctuation degrades
to `#T`, `#T1` and so on.

`KindProse` is informational: it describes a terminal in English rather
than defining one. It is accepted only as the entire body of a
production naming a builtin token, where it documents the token the
lexer already provides. Anywhere else there is nothing to compile, and
it is an error.

### `type SrcSpan`

```go
type SrcSpan struct {
    S int // start offset, inclusive
    E int // end offset, exclusive
    R int // row of the start, 1-based
    C int // column of the start, 1-based
}
```

Offsets are in the same units the front-end's own engine tokens use, so
a front-end copies `sI`, `rI` and `cI` straight across with no
arithmetic. Those units are runtime-native: Go counts **bytes** and
TypeScript counts UTF-16 code units, the same divergence the engine
already records for token positions. Convert at the boundary that knows
the document encoding; nothing here can, because the IR does not hold
the source text.

`R` and `C` are 1-based, so zero in either means "not recorded". The
span is reached through a pointer on `Element` and `Production` for the
same reason: `SrcSpan{S: 0, E: 0}` is a real empty span at the start of
a file.

Spans are optional everywhere. A front-end that records them gets ranged
compile errors; one that does not compiles to exactly the same grammar.

### `const MaxInfinity = 1 << 30`

The unbounded upper bound on a repetition, standing in for TypeScript's
`Infinity`.

### `const MaxLeftRecursionExpansion = 512`

The most alternatives one rule may gain when the left-recursion pass
substitutes the rules that lead its alternatives. Substituting a rule
turns each alternative that begins with it into one alternative per
alternative of that rule, so a chain of such rules multiplies at every
step. Past the bound, `EmitGrammarSpec` returns an `*EmitError` that
names the rule and the rule it was substituting, before making the
alternatives. Factoring the leading reference out of the alternatives
it begins keeps a grammar under it.

### `const MaxLeftRecursionElements = 131072`

What the left-recursion pass may add to the whole grammar, counted in
elements and alternatives. An element's deep size is 1 and everything
nested in it, all the way down: 1 for each alternative of a group and
the deep sizes of the elements in it, and the deep size of the inner
element of an option or a repetition. An alternative counts the same
way: 1 and the deep size of each of its elements. Two steps of the pass
write more than they replace, and the bound holds the sum of what they
add over the whole grammar, each step counted as what it writes less
what it replaces, floored at zero:

- Substituting a rule with the alternatives `a_1 … a_m` into an
  alternative that begins with it adds
  `(m − 1)·(1 + deep|tail|) + Σ deep|a_i| − 1`.
- Splitting a leading element `s` that can match nothing off an
  alternative `s rest`, in front of a hidden left recursion, writes
  `present(s) rest` and `rest`, and adds
  `1 + deep|present(s)| + deep|rest| − deep|s|`.

A chain of rules, each one alternative that begins with the next, gains
no alternatives but copies the rest of the chain into every rule, so
what it adds grows with the square of its length: the bound refuses such
a chain from 514 rules on. `A = [x1] … [xk] A "y" / "z"` needs no
substitution, and its split adds k² + k: the bound refuses it from
k = 362 on. Past the bound, `EmitGrammarSpec` returns an `*EmitError`
that names the rule, before the step that would pass it allocates.

### `const MaxEmitUnits = 3500000`

What compiling one grammar may write, counted in units. A rule of the
spec counts 1, and an alternate 1, with 1 more for each token its `s`
names and 1 for the rule it names in `p` or `r`. Each name the grammar
removes counts 2, for the rule and the fixed token it writes in their
place. A token set counts 1 for each member, and so do three tables the
emitter builds on the way:
the tokens each probe helper lists, the tokens a rule can begin with
and those that can follow it, and the pairs of tokens that can follow
it. The emitter charges a probe helper's rule with its list, which fixes
it.

The emitter also counts, while they last, the token paths a choice
enumerates to tell its alternatives apart. While one alternative's
enumeration runs, every path it makes counts 1, and 1 more for each
token it holds, at every level. Once the enumeration returns, only the
paths it returned stay counted. Once the emitter decides the choice, only
the prefixes it dispatches on stay counted, until the emitter has written
them as alternates. What the emitter writes and what it holds never pass
the bound together.

The emitter charges each unit before the allocation that would hold it.
Past the bound, `EmitGrammarSpec` returns an `*EmitError` that names the
rule, or the character class whose token set would pass it, and says
what the emitter was writing. P optional prefixes over a vocabulary of V
tokens write about 4·P·V units, so the bound refuses a thousand of each,
and it refuses forty alternatives of up to forty optional elements each,
whose enumeration grows with the fourth power of their number. No
grammar the downstream suites compile writes more than 329,213 units.
The bound holds memory, not time: a grammar can still spend seconds on
the choices the emitter decides under it. The memos the contest checks
keep, of token pairs answered, hold no more than 65,536 answers each.
Past that, the check answers a pair without keeping it, which changes no
output.

## Semantic actions

### `type ActionFn`, `type ActionsMap`

```go
type ActionFn = tabnas.AltAction
type ActionsMap map[string][]ActionFn
```

A slice per ref, so several functions can share one; they run in order.

### `func AttachActions(spec *tabnas.GrammarSpec, actions ActionsMap) error`

Binds user actions to a spec in place. A ref is `@rule:phase:mark`,
where phase is `o` or `c`:

```go
err := bnf.AttachActions(spec, bnf.ActionsMap{
    "@op:o:INC": {fn},
})
```

Marks exist only when the spec was compiled with `Marks: true`. A ref
that matches nothing is an error, not a silent no-op.

Each call allocates fresh ref names past whatever the spec already
holds, so a second call cannot overwrite the first call's functions.

### `func AttachActionSlots(spec *tabnas.GrammarSpec, refNames []string) error`

Declares slots by name without supplying functions, for a pure-data spec
that cannot carry closures. Alternate actions only: a rule-phase ref is
refused.

### `func MarkListing(spec *tabnas.GrammarSpec) string`

The compiler-assigned marks, one per line, as
`rule  phase:mark  what`:

```
op  o:INC  s:#INC
op  o:DEC  s:#DEC
```

A mark comes from the alternate's first element. Duplicates within a
rule are suffixed `~2`, `~3` and so on. Removed rules contribute no
lines.

## Serialising

### `func SpecToJSON(spec *tabnas.GrammarSpec, indent int) string`
### `func SpecToData(spec *tabnas.GrammarSpec) map[string]any`

JSON text, and the same content as a data tree. Both return an empty
result on failure.

The tree's root is a `map[string]any`, and every object inside it is a
`*tabnas.OrderedMap`, which holds its keys in the order the TypeScript
compiler writes them: the rules in the order the emitter commits them,
the tokens in the order it allocates them, and each alternate's keys in
the order it sets them. `ToRecognitionSpec` and `ToPureSpec` return the
same kind of tree. The match tokens' order is in their keys, and is not
written as a list of its own.

### `func SpecToJSONErr(...) (string, error)`
### `func SpecToDataErr(...) (map[string]any, error)`

The same two with the failure surfaced. The signatures of the first pair
are kept so existing callers do not break; prefer these anywhere the
output is not immediately read by a person.

### `func ToRecognitionSpec(spec *tabnas.GrammarSpec) (map[string]any, error)`

Strips the tree and value builders, leaving a grammar that recognises
and builds nothing. Refuses when control logic is still a closure, which
means a grammar needing a probe dispatcher compiled without builtins.

### `func ToPureSpec(spec *tabnas.GrammarSpec) (map[string]any, error)`

Reduces a spec to pure, function-free data. Requires a `Builtins: true`
compile, and says so when it does not get one:

```
demo: spec still contains closures; convert with `builtins: true` for
pure-data output. Stray ref(s): @abnf_a0, @abnf_a1
```

### `func ToJsonic(value any, strict bool, indent int) string`

Serialises a function-free value as jsonic text, or as JSON when
`strict` is set. A `*tabnas.OrderedMap` is written in its key order and
a `map[string]any` in a fixed one, so a tree from `ToPureSpec`,
`ToRecognitionSpec` or `SpecToData` is written in the order the
TypeScript compiler writes it. A match token is written as
`@/source/flags`, or `@~/source/flags` when it is eager, in JavaScript's
dialect, as the TypeScript compiler writes it: a code point as `\uHHHH`,
or as `\u{H}` under `u` when the pattern names one at U+10000 or beyond,
and the flags the IR gave the class.

## Helpers

| Function | Returns |
|---|---|
| `BuiltinTokens() map[string]string` | The bareword-to-token map: `NR`, `ST`, `TX`, `VL` to `#NR`, `#ST`, `#TX`, `#VL`. |
| `EscapeRegexp(s string) string` | The literal with regex metacharacters quoted: `a.b*c` gives `a\.b\*c`. |
| `IsEffectivelyCaseSensitive(el *Element) bool` | Whether a literal's case actually matters. True when set explicitly, and true for a literal with no ASCII letter in it. |
| `TermKey(el *Element) string` | A terminal's identity for token allocation: `cs:+`, `ci:if`. |
| `RefsIn(alt Sequence, out map[string]bool)` | Collects the rule references in a sequence into `out`. |
| `IsProseName(name string) bool` | Whether a prose text is a compiler directive, which means wrapped in angle brackets. |

### `const VERSION`

This module's version. It must equal `ts/package.json` `"version"`; a
drift test in each runtime enforces it.

## Metadata on the emitted spec

`spec.Meta["provenance"]` is a `map[string]any` from each generated rule
name to the author-written production it descends from. A compiled
grammar carries an order of magnitude more rules than the author wrote,
and every generated name reaches rule stacks, hover and completion, so a
tool that shows a name to a person reads this first. It is on unless
`ConvertOptions.Provenance` points at `false`.

## Errors

| Type | Raised by | Carries |
|---|---|---|
| `*EmitError` | the five author-facing compile diagnostics | `Message`, `Rule`, `Sp`, `Cause` |
| `*ParseError` | a grammar the IR cannot express | `Message`, `Line`, `Column`, `Cause` |
| `*CompileError` | a grammar that cannot reduce to pure data | `Message`, `Rules` |
| `*ActionError` | a malformed or unresolvable action ref | `Message` |

All four implement `error`, and `EmitError` and `ParseError` implement
`Unwrap`.

Every message is prefixed with `ConvertOptions.Tag`, so a front-end's
users see the notation they wrote:

```
demo: rule 'a' references unknown rule 'missing'
```

`EmitError.Sp` is populated only when the offending IR node carried a
span, so reading it is a strict improvement on every path and a change
of behaviour on none.

A purely left-recursive production is an error return like any other:

```
demo: rule 'a' is purely left-recursive (no seed alternative); cannot
eliminate
```

The pass that raises it panics internally with an `*EmitError` value,
and `EmitGrammarSpec` recovers and returns it. Only `*EmitError` is
converted that way: the other panics in this package say "internal", and
those are compiler defects rather than bad input, so they keep
panicking.

The TypeScript reference for the same surface is in
[`../../ts/README.md`](../../ts/README.md).
