# bnf — spec reduction from Python

Reduce a serialized tabnas `GrammarSpec` to pure data.

```sh
cd ../go/clib && ./build.sh     # build the shared library first
cd ../../py && python3 -m unittest -v
```

```python
import bnf

bnf.recognition_spec(spec)   # drop every output builder
bnf.pure_spec(spec)          # keep them
```

## What this is for

`@tabnas/bnf` is the compiler *behind* the BNF-family front-ends, not
something an end user drives. So this binding is narrow on purpose: it
exposes the reduction, which is the piece that lets a caller with
neither Go nor Node assemble the whole pipeline.

```
front-end --> spec --libtabnasbnf--> recognition spec
                                          |
                              engine library --> verdicts
```

The engine library is tabnas/parser's `go/clib`, which loads a
serialized spec through `tabnas_grammar`.

**There is no "notation text in" function** — this package parses no
notation. For GBNF, use the [`gbnf`](https://github.com/tabnas/gbnf)
module, which both compiles and validates in one step.

## The two reductions

| call | keeps | for |
|---|---|---|
| `recognition_spec` | structure only | "is this input in the language" |
| `pure_spec` | every output builder too | a grammar that still builds its value |

Recognition drops **every** output builder: the tree family (`@node$`,
`@capture$`, `@bubble$`, `@fold$`) and the native-value family
(`@object$`, `@array$`, `@setval$`, …) alike, so a reloaded
recognition grammar builds nothing. Pure keeps them all.

Both raise `BnfError` for a spec that cannot be reduced — one that is
not JSON, or whose control logic is still closures (those cannot be
represented as data, and dropping them quietly would return a grammar
that no longer does what it says). `err.rules` names the rules that
still need closures. `err.code` is `None` for these rejections, and is
the library's code (`handle`, `usage`, `grammar`, `internal`) only when
the call itself failed.

## `as_text`

Pass `as_text=True` to get the spec as JSON text rather than a dict.
Text is the form to hand to the engine library: a regex travels as an
`@/source/flags` string the engine decodes on load, so keep those
strings intact if you re-serialize yourself.

## The library underneath

`libtabnasbnf` exports the uniform tabnas C ABI (admin ADR-12) — the
same five symbols as every per-format tabnas library, documented in
[`go/clib/README.md`](../go/clib/README.md). Its parse input is the
spec, and its value carries both reductions (`value.recognition`,
`value.pure`); this module returns one of them.

`bnf.version()` returns what the library reports about itself:
`{"lib": "libtabnasbnf", "format": "bnf", "template": "v3"}`. It no
longer reports the compiler or engine version.

## Finding the library

`load()` looks for `$BNF_LIB`, then `libtabnasbnf.*` beside this module,
then `../go/clib/dist/libtabnasbnf-<goos>-<arch><ext>`. Or pass `path=`.
It refuses a library that reports any format but `bnf` — every tabnas
format library exports the same symbols — and one built before the
uniform ABI, which exported `bnf_*` symbols instead.

## Tests

`test_bnf.py` reloads the reductions into the engine library to prove
they still parse. It finds that library through `$TABNAS_LIB` or a
built sibling checkout (`../parser/go/clib/dist`), and skips those
checks when neither is present.

## One caveat

The library carries a Go runtime, and a Go runtime does not survive
`os.fork()` intact. With `multiprocessing`, choose `spawn` or
`forkserver` rather than `fork`.
