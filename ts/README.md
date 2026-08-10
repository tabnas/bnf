# @tabnas/bnf

The shared compiler behind the BNF-family grammar front-ends for the
[tabnas](https://github.com/tabnas/parser) parsing engine.

This package holds no notation of its own. It defines a grammar IR and
compiles it into a tabnas `GrammarSpec`; a front-end parses one concrete
syntax into that IR:

```
ABNF / GBNF / EBNF text ──front-end──▶ Grammar ──emitGrammarSpec──▶ GrammarSpec
```

| Front-end | Notation |
|---|---|
| [`@tabnas/abnf`](https://github.com/tabnas/abnf) | RFC 5234 ABNF |
| [`@tabnas/gbnf`](https://github.com/tabnas/gbnf) | llama.cpp GBNF |
| [`@tabnas/ebnf`](https://github.com/tabnas/ebnf) | EBNF (best effort) |

```js
const { emitGrammarSpec } = require('@tabnas/bnf')

const spec = emitGrammarSpec({
  productions: [
    { name: 'val', alts: [[{ kind: 'ref', name: 'add' }]] },
    { name: 'add', alts: [[{ kind: 'token', name: '#NR' }]] },
  ],
}, { tag: 'demo' })
```

Everything downstream of the IR is shared and lives here: desugaring
repetition into helper rules, left-recursion elimination, tail-repeat
rewriting, the probe/rewind dispatcher for prefixes beyond the engine's
bounded lookahead, literal lifting, token allocation, first-set analysis,
and `$stepN` chain emission.

**Full documentation, including the IR contract and the options table, is
in the [repository README](https://github.com/tabnas/bnf#readme).**

## Install

```sh
npm install @tabnas/bnf @tabnas/parser
```

## License

MIT. Copyright (c) 2026 tabnas.
