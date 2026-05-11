# @tabnas/bnf

BNF / ABNF grammar compiler for the [`tabnas`](https://github.com/rjrodger/tabnas) parser.

Takes BNF or ABNF source and emits a tabnas `GrammarSpec`. Also ships a
CLI (`tabnas-bnf`) that does the same thing from the shell.

## Install

```bash
npm install tabnas @tabnas/bnf
```

## Use

```js
const { Tabnas } = require('tabnas')
const { bnf } = require('@tabnas/bnf')

const am = new Tabnas({ plugins: [bnf] })
am.bnf(`<greet> ::= "hi" | "hello"`)
am.parse('hi')
```

Or convert without installing:

```js
const { bnfConvert } = require('@tabnas/bnf')
const spec = bnfConvert(`<greet> ::= "hi"`)
```

## CLI

```bash
tabnas-bnf -f grammar.bnf
tabnas-bnf '<g> ::= "a"' --parse 'a'
```

## License

MIT.
