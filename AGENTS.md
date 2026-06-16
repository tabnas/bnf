# Agents Guide — abnf

## What this project is

`@tabnas/bnf` is a **grammar compiler**: it reads ABNF source and emits
a [`@tabnas/parser`](https://github.com/tabnas/parser) `GrammarSpec`,
optionally installing it on a tabnas instance. Where most tabnas grammar
packages hand-write one fixed grammar, this one is a *meta* plugin — the
grammar it installs is whatever ABNF text you feed it at runtime.

It is a **plugin** for the tabnas engine. Once installed it decorates the
instance with a callable `bnf` member:

- `tn.bnf(src)` compiles `src` and installs the resulting grammar on `tn`.
- `tn.bnf.toSpec(src)` compiles and returns the spec **without** installing.
- Bare exports `bnfConvert(src, opts)` / `parseBnf` / `emitGrammarSpec`
  let you convert without an instance.

The compiler synthesises a `__start__` wrapper rule that pushes the real
start production and consumes end-of-source (`#ZZ`); `bnfConvert` sets
`spec.options.rule.start = '__start__'`. The start production defaults to
the first one declared and can be overridden (`opts.start` / CLI `--start`).

Note the repo/package name mismatch: the **repo is `abnf`** but the
**package is `@tabnas/bnf`**.

## The dialect is ABNF, not classic BNF

Rules are RFC 5234 ABNF, **not** the `<x> ::= a | b` style:

- `name = element ...` — `=` defines a rule, not `::=`.
- `/` is choice (`greet = "hi" / "hello"`), not `|`.
- Literals are **double-quoted** and **case-insensitive** by default;
  `%s"…"` forces case-sensitivity.
- `;` starts a line comment.
- Repetition / option / group: `*A`, `1*A`, `m*nA`, `[ A ]`, `( A / B )`.
- `name =/ alt` incrementally adds alternatives to an existing rule.
- The RFC 5234 Appendix B.1 **core rules** (`ALPHA`, `DIGIT`, `HEXDIG`,
  …) are auto-included when referenced and not locally defined; a local
  `DIGIT = …` always wins. They are defined in `converter.ts` (search
  `RFC 5234 Appendix B.1`) and emitted as flattened `core` nodes so a
  matched char class doesn't litter the tree with one node per character.

Classic-BNF `::=` / `|` does **not** parse. (Some stale comments in
`src/converter.ts` and a CLI example in `ts/README.md` still show `::=` —
ignore those; the parser only accepts the ABNF forms above.)

## Repository map

| Path | What it is |
|---|---|
| [`ts/`](ts/) | **Canonical** (and only) implementation — the `@tabnas/bnf` package, plus the `tabnas-bnf` CLI. |
| [`ts/src/bnf.ts`](ts/src/bnf.ts) | Plugin entry point. Wires `tn.bnf` / `tn.bnf.toSpec` and re-exports the converter. Thin. |
| [`ts/src/converter.ts`](ts/src/converter.ts) | The whole compiler (~2.3k lines): ABNF parser (`parseBnf`), left-recursion rewriter (`eliminateLeftRecursion`), probe-dispatch analyser, and the `GrammarSpec` emitter (`emitGrammarSpec`). |
| [`ts/src/bin/tabnas-bnf-cli.ts`](ts/src/bin/tabnas-bnf-cli.ts) | CLI implementation (`run(argv, console)`). |
| [`ts/bin/tabnas-bnf`](ts/bin/tabnas-bnf) | Executable shim → `dist/bin/tabnas-bnf-cli`. The `bin` entry in `package.json`. |
| [`ts/test/`](ts/test/) | `node --test` suite (see below). |
| [`ts/test/grammar/`](ts/test/grammar/) | `.bnf` / `.abnf` fixture grammars (`greet`, `pair`, `arith`, `arith-leftrec`, `json-subset`, `rfc3986-uri`). |

There is **no `go/` directory** — this package is TypeScript-only, so the
usual "Go port tracks TS" contract does not apply here.

## How the compiler is itself a tabnas grammar

The ABNF source is parsed by a tabnas instance whose grammar is the
declarative `bnfRules` table inside `converter.ts` — i.e. the converter
eats its own dog food. The emitter then walks that AST and produces the
output `GrammarSpec`.

Unlike the json/csv plugins (which layer on jsonic's grammar and prune
unwanted rules with `tn.rule(name, null)`), the compiled output is a
**complete grammar built from the ABNF source alone** — `bnfConvert`
returns a freestanding spec, and the CLI's parse mode runs it on a bare
`new Tabnas()` with no other plugin. (The install path does call
`j.rule(name, null)` internally while wiring rules onto the instance.)

Non-obvious things an agent should know before touching `converter.ts`:

- **Left recursion is rewritten automatically.** Direct left recursion
  `P = P a / b` becomes `P = b *(a)` (`eliminateLeftRecursion`). See the
  `arith-leftrec.bnf` fixture, which must parse identically to `arith.bnf`.
- **Optional-prefix ambiguity uses a probe + phase-retry pattern.** For
  shapes like `[X D] Y` where X and Y share a character vocabulary and D
  is a terminal disambiguator, the rewriter synthesises a *dispatcher*
  rule that marks the token position, runs a failure-proof `*vocab`
  probe, peeks `ctx.t[0]`, rewinds, and commits to the right branch on a
  retry pass. This is the trickiest part of the compiler; `probe.test.js`
  documents and pins it. Don't "simplify" it without re-reading that test.
- **Synthetic rules.** Multi-segment alternatives are chained through
  `<prodname>$stepN` continuation rules; probe machinery adds dispatcher
  and `*vocab` helper rules. Output AST nodes carry a `nodeKind`
  (`user` / `core` / `helper`); only `user` nodes get their own tree
  node, the others flatten their `src`/`kids` into the enclosing rule.
- **Larger ABNF surface is still partial.** `%x`/`%d`/`%b` numeric ranges,
  prose-val, and similar are in-progress (the `rfc3986-uri.abnf` fixture
  documents the workarounds it needed). When you extend the dialect, add a
  fixture grammar under `ts/test/grammar/` and an end-to-end test.

## The tabnas engine dependency

The engine is consumed as a **sibling checkout** (the same model the rest
of tabnas uses until `@tabnas/parser` publishes tagged releases):

- `@tabnas/parser` is a **`peerDependency`** (`"file:../../parser/ts"`)
  and is mirrored as a `file:` **devDependency** so local builds resolve.
- `@tabnas/debug` and `@tabnas/railroad` are **dev-only** `file:`
  devDependencies — `debug` for the `debug.model()` composition test,
  `railroad` for regenerating the README railroad diagram. Neither is a
  runtime dependency.
- `engines.node` is `">=24"`; npm ≥ 7 auto-installs the peer.

Clone `https://github.com/tabnas/parser` (and `debug`) as siblings of
this repo and build `parser/ts` before working here. CI does this for you
(see below).

## Build & test

From `ts/` (or use the top-level `Makefile`):

```bash
cd ts && npm install && npm run build   # tsc --build src
npm test                                # node --enable-source-maps --test test/**/*.test.js
```

Top-level `Makefile` targets (TS-only — no Go targets):

```bash
make build        # cd ts && npm run build
make test         # cd ts && npm test
make clean        # rm -rf ts/dist ts/dist-test
make publish-ts   # test, then npm publish --access public
make reset        # cd ts && npm run reset  (clean + install + build + test)
```

The test suite (`ts/test/*.test.js`, run against the built `dist`):

- `bnf.test.js` — the core converter/parser unit suite.
- `probe.test.js` — the probe + phase-retry disambiguation pattern.
- `rfc3986.test.js` — end-to-end: compiles `test/grammar/rfc3986-uri.abnf`
  and parses URIs, exercising most of the supported ABNF surface.
- `doc-examples.test.js` — keeps the README/doc examples honest.
- `debug-model.test.js` — composition test with `@tabnas/debug`: compiles
  a small ABNF grammar, installs the `Debug` plugin, and asserts
  `j.debug.model()` (the rule-name set including the `__start__` wrapper,
  `m.config.start === '__start__'`, the `#ZZ` close, `m.plugins`, and the
  rule-reference graph edges). It **dynamically resolves** `@tabnas/debug`
  and **skips** when absent (or when `TABNAS_DEBUG_PATH` is unset and the
  dep is missing), so it is safe outside the package.

## CLI (`tabnas-bnf`)

`bin/tabnas-bnf` → `dist/bin/tabnas-bnf-cli`. By default it prints the
compiled `GrammarSpec` as JSON. Flags (`run` in
`src/bin/tabnas-bnf-cli.ts`): `-`/stdin, `--file`/`-f`, `--start`/`-s`,
`--tag`/`-t` (group tag on every emitted alt, default `bnf`),
`--compact`/`-c`, `--parse`/`-P` and `--parse-file` (compile, install on a
bare engine, parse the sample(s), print the tree(s), exit non-zero on any
failure), and `--help`/`-h`. Bare non-flag args are treated as inline ABNF
source. Example: `tabnas-bnf 'greet = "hi" / "hello"' --parse 'hi'`.

## CI

`.github/workflows/build.yml` runs on `ubuntu`/`windows`/`macos` ×
Node 24. It sets `core.autocrlf false` (CRLF would corrupt fixtures),
**clones the sibling closure** `parser debug json railroad` from
`github.com/tabnas`, builds them plus this repo in topo order
(`parser debug json abnf railroad`), then runs `npm test` in `abnf/ts`.
Packages are not published to npm, hence the sibling-checkout strategy.
