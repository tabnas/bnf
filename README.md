# @tabnas/bnf

The shared compiler behind the BNF-family grammar front-ends for the
[tabnas](https://github.com/tabnas/parser) parsing engine.

This package holds **no notation of its own**. It defines an intermediate
representation — a `Grammar` of `Production`s over `Element`s — and
compiles that IR into a tabnas `GrammarSpec`. Each front-end parses one
concrete syntax into the IR:

| Front-end | Notation |
|---|---|
| [`@tabnas/abnf`](https://github.com/tabnas/abnf) | RFC 5234 ABNF |
| [`@tabnas/gbnf`](https://github.com/tabnas/gbnf) | llama.cpp GBNF |
| [`@tabnas/ebnf`](https://github.com/tabnas/ebnf) | EBNF (best effort) |

```
ABNF / GBNF / EBNF text ──front-end──▶ Grammar ──emitGrammarSpec──▶ GrammarSpec
```

Everything hard about that second arrow lives here, and is shared:

- **desugaring** repetition (`*A`, `1*A`, `m*nA`, `[A]`) into helper rules;
- **left-recursion elimination**, rewriting a left-recursive rule into
  iterative form so it runs on a push-down engine — including the
  recursion hidden behind nullable sugar (`A = ["x"] A "y"`), and the
  **suffix-debt counters** that stop the generated tail loop from eating
  a token the enclosing alternative still owes;
- **tail-repeat rewriting**, turning `X = prefix [ sep X ]` into a
  same-depth close-phase loop so iterations share one parent;
- **probe dispatch**, the mark/rewind machinery for optional prefixes that
  exceed the engine's bounded lookahead;
- **literal lifting**, turning single-literal productions (`PL = "+"`)
  into named lexer tokens (`#PL`);
- **token allocation**, **first-set analysis**, and **chain emission**
  through synthetic `$stepN` continuation rules.

Writing a new front-end therefore means writing a parser for your
notation that produces `Production[]` — and nothing else.

## The IR

```ts
type Element =
  | { kind: 'term'; literal: string; caseSensitive?: boolean; tokenName?: string }
  | { kind: 'ref'; name: string }
  | { kind: 'token'; name: string }        // an engine lexer token: #TX, #NR, …
  | { kind: 'prose'; text: string }        // informational: `NR = <number>`
  | { kind: 'regex'; pattern: string; flags: string }
  | { kind: 'opt'; inner: Element }        // [ A ]
  | { kind: 'star'; inner: Element }       // *A
  | { kind: 'plus'; inner: Element }       // 1*A
  | { kind: 'rep'; min: number; max: number; inner: Element }
  | { kind: 'group'; alts: Sequence[] }

type Sequence = Element[]

type Production = {
  name: string
  alts: Sequence[]
  nodeKind?: 'user' | 'core' | 'helper'
}

type Grammar = { productions: Production[] }
```

`caseSensitive` exists because ABNF quoted strings are case-*insensitive*
by default while GBNF's are case-sensitive: the front-end states the
intent and the emitter lowers it (case-folding regex, or a plain fixed
token). `regex` is how character classes arrive — GBNF's `[a-z]`,
ABNF's `%x41-5A` — since the engine matches those with a lexer matcher
rather than a rule per character.

## Usage

```js
const { emitGrammarSpec } = require('@tabnas/bnf')

// A front-end would build this from its own syntax.
const grammar = {
  productions: [
    { name: 'val', alts: [[{ kind: 'ref', name: 'add' }]] },
    { name: 'add', alts: [[{ kind: 'token', name: '#NR' }]] },
  ],
}

const spec = emitGrammarSpec(grammar, { tag: 'demo' })
Object.keys(spec.rule).includes('val')   // => true
```

## Options

| Option | Effect |
|---|---|
| `start` | Start rule name (default: the first production). |
| `tag` | Group tag stamped on every emitted alt (default `'bnf'`; front-ends pass their own, e.g. `'abnf'`). |
| `builtins` | Emit probe dispatch and tree building as engine `$`-builtin refs instead of closures, keeping the spec function-free and serializable. |
| `marks` | Emit a stable `m` mark per user-rule alt, enabling `@<rule>:o\|c:<mark>` user-action references. |
| `wordKeywords` | Treat word-like literals as whole-word keywords, so `"option"` does not match the prefix of `optional`. For tokenised, keyword-rich languages; leave off for char-level grammars. |

## Provenance

This code was extracted from `@tabnas/abnf`, where it grew up alongside
the ABNF parser. The split is verified by that package's own suite: after
moving ~2,500 lines here and rebuilding ABNF as a front-end, all 300 of
its tests still pass, including the conformance run over 68 third-party
`.abnf` files.

## License

MIT. Copyright (c) 2026 tabnas.
