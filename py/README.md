# bnf — spec reduction from Python

Reduce a serialized tabnas `GrammarSpec` to pure data.

```sh
cd ../go/clib && ./build.sh     # build the shared library first
cd ../../py && python3 -m unittest -v
```

```python
import bnf

bnf.recognition_spec(spec)   # drop AST-building hooks
bnf.pure_spec(spec)          # keep the tree builtins
```

## What this is for

`@tabnas/bnf` is the compiler *behind* the BNF-family front-ends, not
something an end user drives. So this binding is narrow on purpose: it
exposes the reduction, which is the piece that lets a caller with
neither Go nor Node assemble the whole pipeline.

```
GBNF text --libtabnasgbnf--> spec --libtabnasbnf--> recognition spec
                                              |
                                        libtabnas --> verdicts
```

**There is no "notation text in" function** — this package parses no
notation. For GBNF, use the [`gbnf`](https://github.com/tabnas/gbnf)
module, which both compiles and validates in one step.

## The two reductions

| call | keeps | for |
|---|---|---|
| `recognition_spec` | structure only | "is this input in the language" |
| `pure_spec` | tree `$`-builtins too | a grammar that still builds `{rule, src, kids}` |

Recognition drops the **tree** builtins (`@node$`, `@capture$`,
`@bubble$`). It does *not* drop the native-value family (`@object$`,
`@value$`, …), so for a spec built from those the two are identical —
worth knowing before expecting recognition mode to shrink something.

Both raise `BnfError` for a spec whose control logic is still closures:
those cannot be represented as data, and dropping them quietly would
return a grammar that no longer does what it says.

## `as_text`

Pass `as_text=True` to get the spec text rather than a dict. Text is the
form to hand to `libtabnas`: a regex travels as an `@/source/flags`
sentinel the engine decodes on load, so keep those strings intact if you
re-serialize yourself.

## Finding the library

`load()` looks for `$BNF_LIB`, then `libtabnasbnf.*` beside this module, then
`../go/clib/dist/libtabnasbnf-<goos>-<arch><ext>`. Or pass `path=`.

## One caveat

The library carries a Go runtime, and a Go runtime does not survive
`os.fork()` intact. With `multiprocessing`, choose `spawn` or
`forkserver` rather than `fork`.
