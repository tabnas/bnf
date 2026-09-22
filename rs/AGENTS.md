# Agents Guide — rs/

The Rust port of the canonical TypeScript in [`../ts`](../ts). Read
[`../AGENTS.md`](../AGENTS.md) first: it holds the cross-runtime rules
(TypeScript wins, nothing here may know a notation, the tag defaults to
`bnf`, the version sites), and this file only covers what is specific to
this crate.

## Layout

| Path | Mirrors |
|---|---|
| `src/ir.rs` | the type half of `ts/src/compiler.ts`: `Element`/`Kind`, `Production`, `Grammar`, `ConvertOptions`, `EmitError`, the small exported helpers |
| `src/prose.rs` | `resolveProseTerminals`, `liftLiteralTokens`, `normalizeBuiltinTokens`, `nullableRules` |
| `src/annotate.rs` | `planValueAnnotations`, `planArrayHelpers` |
| `src/leftrec.rs` | `eliminateLeftRecursion` (Paull's, hidden recursion, suffix-debt counters), `rewriteTailRepeats` |
| `src/probe.rs` | `rewriteProbeDispatches` |
| `src/factor.rs` | `leftFactor`, `elemEqual`, `seqTokenSpan` |
| `src/desugar.rs` | `desugar` |
| `src/analysis.rs` | FIRST/FOLLOW/FOLLOW₂, `resolveSuffixDebts`, `altPrefixes` |
| `src/ranges.rs` | `patternCharRanges`, `partitionRanges`, `classAnalysis` and the rest of the character coverage code |
| `src/emit.rs` | `emitGrammarSpec` and every emitter: token allocation, the contest context, the ref registry, productions, chains, tail repeats, probes |
| `src/spec.rs` | the emitted `GrammarSpec` and `ts/src/spec.ts`: `to_pure_spec`, `to_recognition_spec`, `to_jsonic`, `attach_actions`, `attach_action_slots`, `mark_listing`, and the closure-mode `RefAction` binding |
| `tests/oracle_test.rs` | the emitter held to TypeScript's output, byte for byte, over `tests/oracle/*.json` |
| `tests/bnf_test.rs` | `go/bnf_test.go` and `ts/test/bnf.test.js` |
| `tests/value_annotation_test.rs` | `go/value_annotation_test.go` |
| `tests/class_partition_test.rs`, `tests/empty_input_test.rs` | their Go and TypeScript twins |
| `tests/options_data_test.rs` | `go/options_data_test.go`: a front-end's options survive the reductions, strict serialisation and the engine's loader (here they are data by construction, so nothing is refused) |
| `tests/regex_flags_test.rs` | no twin: the `RegExp` constructor's own rules on an `Element::regex` flag string, which TypeScript gets from the constructor for free and this port has to state — the flags it knows, no repeats, `u` and `v` never together, and the fixed order `RegExp.prototype.flags` reports |
| `tests/doc_examples_test.rs` | `go/doc_examples_test.go`: the claims the crate documentation makes |
| `tests/divergence_test.rs` | the executable register for the Rust entries in [`../DIVERGENCE.md`](../DIVERGENCE.md): one test per recorded difference, so a repaired entry has to lose its test as well as its section |
| `tests/version_test.rs` | the version sites must agree |
| `README.md` | the crate front page; its `rust` fences run as doctests |

## The emitted text is compared byte for byte

`tests/oracle_test.rs` reproduces TypeScript's strict-jsonic output
exactly, and that is the parity claim this port makes: the engine reads
the same document from either compiler. Two things follow.

**Alternates are ordered maps with JavaScript assignment semantics.**
`AltSpec` in `src/spec.rs` keeps its fields in insertion order, `set` on
an existing key updates it in place, and every emission site in
`src/emit.rs` writes the keys in the order the canonical compiler's
object literal does (`{ g, s, p, n, a, k }` from `segmentToAlt`,
`{ s, b, p, a, k, g }` for a dispatch entry, `{ ...o, s, b }` for a
peek copy, and so on). A `serde_json::Map` with `preserve_order` is
what makes that possible. Do not "tidy" a site into a fixed field
order; the oracle test will tell you which line moved.

**Regenerating the fixtures needs the TypeScript oracle.**
`tests/oracle/generate.cjs` hooks the canonical compiler as each
front-end resolves it, captures the exact IR handed to
`emitGrammarSpec`, and writes the fixture. It needs `../../parser/ts`,
`../../abnf/ts` and (optionally) `../../ebnf/ts` built:

```bash
cd ../../parser/ts && npm install && npm run build
cd ../../bnf/ts && npm install && npm run build
cd ../../abnf/ts && npm install && npm run build
node rs/tests/oracle/generate.cjs corpus rs/tests/oracle      # from the repo root
```

The committed set is the ten notation-corpus cases, the
`abnf-grammar-*` fixtures made with the `file` mode from abnf's own
`ts/test/grammar/*.abnf` (the two largest, `json-subset` and
`rfc3986-uri`, are left out for size), and the hand-written `ir-*` ones
below. Nothing from the third-party abnf conformance corpus is
committed: those grammars are separately licensed and abnf itself never
vendors them.

`ORACLE_DIR=<dir> cargo test --test oracle_test` grades any directory of
fixtures, which is how that corpus (68 `.abnf` files, fetched by
`../abnf/test/fetch-abnf-corpus.sh`) is graded when needed:
`node rs/tests/oracle/generate.cjs file <grammar.abnf> <out.json>` per
file, then the test with `ORACLE_DIR` pointing at the directory (use
`--release`; the biggest grammars emit tens of megabytes).
`ORACLE_DUMP=<dir>` writes every text this port produced, for a full diff
rather than the first differing line.

## One engine, two compilers

The oracle test grades engine verdicts on ONE engine. It installs both
TypeScript's spec and this port's spec on the Rust engine and requires
the same value or the same error code from each; that is the compiler
claim. It separately compares against the TypeScript ENGINE's recorded
verdict: accept/reject and the error code must match, and a VALUE that
differs has to be listed in `ENGINE_VALUE_DIVERGENCES`, asserted both
ways (a listed case that starts agreeing fails as stale). The one entry
today is the probe-dispatch grammar `R = [ A "@" ] A`, where the
TypeScript engine returns `R` with an empty `src` and no kids and this
engine the full tree; the parser repository's own
`ci/rust/notation-corpus.js` reports the same difference. It is the
engine's, not this compiler's.

A second register, `ENGINE_REJECTS_WHAT_TYPESCRIPT_ACCEPTS`, holds the
cases the TypeScript engine accepts and this engine rejects from the
same document. The one entry today is `ir-nullable-suffix`
(`A = [ "x" ] A [ "y" ] / "z"`), asserted both ways as well.

Fixtures named `ir-*` are built from hand-written IR rather than from a
front-end, so there is no grammar text to go back to: the fixture is its
own input, and `node tests/oracle/generate.cjs regen <fixture.json>`
replays its `ir`, `opts` and case sources through TypeScript again.
Those need only this repository's `ts/` and the engine, no front-end. A
fixture whose `pureError` is set records an IR TypeScript REFUSED, and
that refusal message is graded byte for byte like the emitted text.

## Untrusted IR, and where the stack still runs out

`emit_grammar_spec` measures element nesting before anything walks it
and refuses past `MAX_ELEMENT_DEPTH` (exported, defined in `src/ir.rs`),
because the
passes are recursive and a Rust stack that runs out aborts the process
rather than unwinding. The walks over the REFERENCE graph (Tarjan's
components and Paull's ordering in `src/leftrec.rs`) carry their own
explicit stack for the same reason: a chain of a few thousand rules
overflowed them when they recursed. Keep both properties when editing
either file.

What is left, and is the caller's: DROPPING a deeply nested `Element`
recurses through the `Box` chain, so a tree the compiler refused still
overflows the stack when it goes out of scope, measured between 10000
and 30000 levels. Reaching that needs a front-end that can itself build
a tree that deep; an IR that arrives as JSON cannot, since `serde_json`
stops at 128.

A bounded repetition expands into roughly two rules per count, in this
port and in TypeScript alike, so `0*1000000a` exhausts memory in either
runtime. That is the compiler's design, not a difference between the
ports.

## Things that look like bugs and are not

- **`\b` for `wordKeywords`.** The engine's `regex` crate has no
  lookaround; `(?![A-Za-z0-9_])` cannot be expressed. Go does the same.
- **`is_matcher_token_name` is a list.** TypeScript asks the engine
  (`util.isMatcherToken`); the Rust engine exports no such helper, so the
  list mirrors `MATCHER_TOKEN_NAMES` in `parser/ts/src/utility.ts`
  (`BD ZZ UK AA SP LN CM NR ST TX VL`). If the engine grows a matcher
  token, add it here.
- **Diagnostics use a thread-local prefix.** TypeScript's `_diagName` is
  module state; here it is `thread_local!`, which is what keeps
  `concurrent_emits_keep_their_own_diag_prefix` green.
- **`Grammar` is cloned by value** on entry to `emit_grammar_spec`, so the
  caller's IR is never modified; the TypeScript `cloneGrammar` hazards
  (dropping `remove`/`clearAll`, aliasing element arrays) cannot arise.
- **`GrammarSpec::to_value` carries `meta` and `clear`;**
  `to_pure_spec` and `to_recognition_spec` carry `meta` and `v` only,
  exactly as the TypeScript transforms do.

## Running it

`make test-rs` from the repository root is the fast loop. `ci/rust/run.sh`
is the full gate and is what CI would run: it adds `cargo fmt --check`, a
build, doctests, the lockfile check and the MSRV pin. The doctests
include `README.md` through a `#[cfg(doctest)]` include in `src/lib.rs`,
so every `rust` fence in the README is compiled and run as written: keep
each one a complete `fn main` example. The engine must be a sibling
checkout at `../../parser`.

The README is in the published set and is gated by
[`../docs/STYLE-GUIDE.md`](../docs/STYLE-GUIDE.md): no em dashes in
prose, no first person singular, no link from it to any `AGENTS.md`, no
project history.

## The README is doctested

`src/lib.rs` includes `README.md` as crate documentation under
`#[cfg(doctest)]`, so `cargo test --doc` compiles and runs every `rust`
fence in the README exactly as it appears on the page. Keep each fence a
complete `fn main` example (no top-level `?`, no hidden `# ` lines), and
expect `readme_examples (line N)` entries in the doctest output, one per
fence.
