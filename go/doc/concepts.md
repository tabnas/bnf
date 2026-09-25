# Concepts (Go)

Why this package is shaped the way it is: what the IR is a contract
between, what the passes between it and a `GrammarSpec` do, and where
the Go port and the canonical TypeScript one differ. This is background
reading. For steps see the [tutorial](tutorial.md) and the
[how-to guide](guide.md); for signatures see the
[reference](reference.md).

## A compiler with no notation

Every other grammar package in this fleet owns a syntax. This one owns
none. It defines an intermediate representation and compiles that into
the engine's `GrammarSpec`:

```
ABNF / GBNF / EBNF text ──front-end──▶ Grammar ──EmitGrammarSpec──▶ GrammarSpec
```

A front-end's whole job is the first arrow: read one concrete notation,
build a `*Grammar`. Everything after the IR is shared, which is the
point. Three notations differ in how they spell repetition and grouping
and agree on what those mean, so the desugaring, the left-recursion
elimination, the dispatch analysis and the token allocation are written
once.

That also fixes where a change belongs. A defect in how `[a b]` compiles
is this package's; a defect in how `a?` is parsed into an `Opt` element
is the front-end's.

## The IR is the contract

`Grammar`, `Production`, `Sequence` and `Element` are the whole
interface. Two properties make them work as a boundary.

**A front-end fills in two fields per production.** `Name` and `Alts`.
Everything else on `Production` is written by the passes: `Origin`,
`NodeKind`, `TailRepeat`, `RepeatHelper`, `DebtGuard`, `DebtOwed`,
`ProbeDisp`, `ProbeHelper`. A front-end that sets them is reaching past
the boundary.

**One struct, tagged by `Kind`.** TypeScript expresses an element as a
union of ten shapes. Go has no union type, so `Element` is one struct
with the fields of all ten and a `Kind` saying which apply. The cost is
that an element carries fields its kind never reads; the benefit is that
the two runtimes describe the same IR, and a port can be checked field
by field.

### Spans are optional, and their units are not portable

`Element.Sp` and `Production.Sp` are pointers to a `SrcSpan`. A
front-end that records them gets compile errors that can underline the
offending text; one that does not compiles to exactly the same grammar.
Nothing downstream requires them.

The units are the front-end's own engine tokens', copied across with no
arithmetic, which is what keeps an off-by-one from creeping in at the
one place it easily could. It also means the units are runtime-native:
Go counts bytes, TypeScript counts UTF-16 code units. That is the same
divergence the engine already records for token positions, and it is not
resolvable here, because the IR does not hold the source text. A
consumer needing portable positions, such as an LSP server, converts at
the boundary where the document's encoding is known.

The pointer matters for the same reason the 1-based row and column do:
`SrcSpan{S: 0, E: 0}` is a real empty span at the very start of a file,
so absence has to be spelled some other way.

## What the passes do

`EmitGrammarSpec` clones the grammar first, so the caller's IR is never
modified, and then runs the passes in a fixed order. The order is the
design: each one relies on what the previous ones have already
normalised.

| Pass | What it does |
|---|---|
| `planValueAnnotations` | Records what each production builds, when a front-end says it builds something rather than the default node. |
| `resolveProseTerminals` | Drops a production whose whole body is prose naming a builtin token, so references resolve to the builtin. |
| `liftLiteralTokens` | Gives a literal the name of the production that defines it, so `PL = "+"` becomes `#PL` rather than `#T1`. |
| `normalizeBuiltinTokens` | Turns a bareword naming a builtin into a `KindToken` element. |
| `eliminateLeftRecursion` | Rewrites a left-recursive production into a seed plus a tail loop. |
| `rewriteProbeDispatches` | Synthesises a dispatcher for a subsequence the engine's bounded lookahead cannot decide. |
| `leftFactor` | Factors alternatives sharing a prefix beyond dispatch lookahead into a common prefix plus a transparent helper. |
| `rewriteTailRepeats` | Compiles `X = prefix [ sep X ]` to a same-depth close-phase repeat rather than a helper chain. |
| `desugar` | Turns repetition and grouping into helper productions. |
| `resolveSuffixDebts` | Confirms or drops the counter guarding a tail loop whose greediness contests an enclosing suffix. |
| `computeFollowSets` | Works out what may follow a repetition, so an empty terminating alternative can be guarded by a peek. |
| `tokenClassNames` | Under `TokenClasses`, names the productions whose alternatives are all single literals or tokens; each becomes one engine token set, kept out of the left-recursion substitution so it stays a rule of its own. |

Then the emitter walks the normalised grammar and writes alternates. A
choice dispatches on the first token of each alternative
(`dispatchPrefixes`), and peeks one token deeper only under a head two
alternatives share, up to the engine's four-token window, so the table
grows with the number of decisions rather than with the product of the
tokens that can fill the window. A token class counts as one token at
every such position.

The result is bigger than it looks. Two productions in the
[tutorial](tutorial.md) compile to thirteen rules; a twelve-production
ABNF grammar emits over a hundred. Most of the extra names are helpers
that no author wrote.

### Why there is a provenance map

Those generated names are not an implementation detail once a tool shows
them to a person. A rule stack, a hover, a completion list, and an
outline all name rules, and `list$star2` means nothing to somebody who
wrote `list`.

So every pass that synthesises a production records the author-written
production it descends from, in `Production.Origin`, and the emitter
exports the map as `spec.Meta["provenance"]`. It is on by default
because the names are otherwise unattributable. Turning it off is for an
embedded grammar where size beats names.

### Why the emit is serialised

The diagnostic prefix is package state: it is set from
`ConvertOptions.Tag` at the start of an emit and read by around forty
diagnostics across seven files. Two concurrent compiles raced, and the
loser reported the winner's notation on an error about its own grammar,
so an ABNF author saw `gbnf:` on a diagnostic about a rule they wrote.

`emitGrammarSpec` therefore takes a lock for the duration. A threaded
parameter would be the alternative, at the cost of a prefix argument on
twenty functions across passes that have no other business with it, and
this is a once-per-grammar-install call. TypeScript needs none of it:
its pipeline is synchronous and single-threaded, which is exactly why
the module-scoped equivalent is safe there.

## Why a grammar can be reduced

A compiled spec normally carries closures: the tree builders, the value
builders, and any control logic a probe dispatcher needs. Two reductions
take that away, for two different reasons.

**`ToRecognitionSpec`** removes the tree and value builders. What is
left recognises input and builds nothing, which is what a validator
wants. It refuses only when control logic is still a closure.

**`ToPureSpec`** removes everything callable, leaving data that
`encoding/json` can write. That needs a `Builtins: true` compile, where
each action is emitted as a `@name$` string the engine resolves for
itself, and it says so rather than silently emitting something
unserialisable.

The pair is what lets a front-end ship a grammar as data and install it
without recompiling.

## Differences from the TypeScript version

The Go port follows TypeScript, which defines the language. The
behavioural gaps are tracked in
[differences.md](differences.md); what follows is the API shape, which
differs because Go does.

- **No union types.** TypeScript's `AbnfElement` union becomes one
  `Element` struct tagged by `Kind`.
- **No optional properties.** TypeScript distinguishes an absent
  property from a `false` one for free. Go needs a second field or a
  pointer, so an explicit case-sensitivity flag is `CaseSensitive` plus
  `HasCaseSens`, and a default-on option is `Provenance *bool`.
- **No `Infinity`.** `MaxInfinity` is `1 << 30`.
- **Errors, not throws.** `EmitGrammarSpec` returns an `error`. A
  purely left-recursive production is included: the pass raising it
  panics internally with an `*EmitError` value, and the facade recovers
  and returns it. Only `*EmitError` is converted that way, because the
  other panics say "internal" and are compiler defects rather than bad
  input; returning those as errors would report a fault of this
  package's as a fault in the caller's grammar.
- **A lock around the emit**, for the reason above.
- **RE2, so no lookahead.** `WordKeywords` emits a `\b` guard where
  TypeScript emits `(?![A-Za-z0-9_])`. The two are equivalent for the
  literals this applies to.
- **Two serialisation surfaces.** TypeScript returns its `GrammarSpec`
  object; Go returns the generic pure-data tree (`map[string]any`,
  `[]any`, scalars) from `ToRecognitionSpec` and `ToPureSpec`.
- **`SpecToData` and `SpecToJSON` swallow failures**, returning an empty
  result, because their signatures predate the error return and callers
  depend on them. `SpecToDataErr` and `SpecToJSONErr` are the same
  functions with the failure surfaced.

The canonical implementation and its own notes are in
[`../../ts/README.md`](../../ts/README.md).
