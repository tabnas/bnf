# libtabnasbnf — the C ABI

The shared compiler's one capability that is useful **without a
front-end**: reducing an already-serialized `GrammarSpec` to pure data.

```sh
./build.sh            # host
ZIG=/path/to/zig ./build.sh all
```

## What this is, and is not

`@tabnas/bnf` is the compiler *behind* the BNF-family front-ends (GBNF,
ABNF, EBNF). It is a library those front-ends call, not something an end
user drives — so this surface is deliberately narrow. There is **no
"notation text in" function**, because this package parses no notation. A
front-end does. For GBNF, use [`libtabnasgbnf`](https://github.com/tabnas/gbnf),
which both compiles and validates.

What is left is the reduction, and it is worth exposing because it is the
piece that lets a caller with neither Go nor Node assemble the whole
pipeline:

```
GBNF text ──libtabnasgbnf──▶ spec ──libtabnasbnf──▶ recognition spec
                                              │
                                        libtabnas ──▶ verdicts
```

## Two reductions

| Function | Keeps | For |
|---|---|---|
| `bnf_recognition_spec` | structure only | "is this input in the language" |
| `bnf_pure_spec` | tree `$`-builtins too | a reloaded grammar that still builds `{rule, src, kids}` |

Recognition drops the **tree** builtins (`@node$`, `@capture$`,
`@bubble$`) and the spec's own ref-backed actions. It does *not* drop the
native-value family (`@object$`, `@value$`, …) — so for a spec built from
those the two reductions are byte-identical. Worth knowing before
reaching for recognition mode expecting it to shrink something.

Both refuse a spec whose control logic is still closures. Those cannot be
represented as data at all, and a reduction that dropped them silently
would hand back a grammar that no longer does what it says.

## The contract

| Function | Returns |
|---|---|
| `bnf_version()` | `{"ok":true,"version":"…","engine":"…"}` |
| `bnf_recognition_spec(spec, len)` | `{"ok":true,"spec":"…"}` |
| `bnf_pure_spec(spec, len)` | `{"ok":true,"spec":"…"}` |
| `bnf_free(str)` | — |

1. **Every call returns JSON.** A C ABI has one return value and no
   exceptions, so each entry point returns a document and a binding in
   any language is *call, decode*.
2. **A failure carries no spec.** `ok:false` never comes with a `spec`
   field, so a caller cannot half-succeed into using an empty grammar.
3. **Lengths are explicit,** and `(NULL, 0)` is the empty buffer, as C
   spells one.
4. **The caller owns what it is given.** Every `char*` must be released
   with `bnf_free` (it is `malloc`'d, so that is `free(3)`).

## A note on the wire format

The emitted spec is written with `ToJsonic`, not `encoding/json`. A regex
travels as an `@/source/flags` sentinel that the engine decodes on load;
`encoding/json` sees the internal holder's unexported fields and writes
`{}`, which would drop every match token and leave a grammar that lexes
nothing. If you re-serialize the spec yourself, keep those strings
intact.

## Cross-compiling

| target | how |
|---|---|
| `linux/amd64`, `linux/arm64` | zig, cross |
| `windows/amd64` | zig, cross |
| `darwin/*` | **native macOS host only** |

macOS needs Apple's SDK (`CoreFoundation`, `libresolv`), which zig cannot
redistribute. `build.sh all` skips darwin unless already running on it; a
target named explicitly on the command line fails instead of skipping, so
release automation cannot mistake an incomplete artifact set for success.

## Layout

- `core.go` — the behaviour, in plain Go.
- `bnf_c.go` — the cgo shim: `(pointer, length)` in, `malloc`'d string
  out, nothing else.
- `core_test.go` — the contract.

Go does not support cgo in `_test.go` files, so keeping the behaviour in
`core.go` is what makes it testable at all.
