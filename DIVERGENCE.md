# Divergences

Same IR, different emitted grammar across runtimes, is a divergence. The
org rule (admin DECISIONS.md ADR-13/14) is to repair the port that
violates what TypeScript defines, and to record here what cannot be
repaired now. There is no `test/spec` register in this repository; the
executable record is `rs/tests/oracle_test.rs`, which holds the Rust
emitter to TypeScript's serialised output byte for byte.

## Rust

### The word-keyword guard is `\b`, not `(?![A-Za-z0-9_])`

TypeScript emits `^option(?![A-Za-z0-9_])` for a `wordKeywords`
literal. The Rust engine compiles serialised terminals with the `regex`
crate, which has no lookaround (parser `DIVERGENCE.md`, "Regex dialect in
serialized terminals"), so the Rust port emits `^option\b`, as the Go
port does. The two agree on ASCII input and differ only for a keyword
immediately followed by a non-ASCII letter, which `\b` treats as a word
character and the lookahead does not.

### Regular expression terminals are refused at emit time

Both compilers construct the matcher when the token is allocated, so an
invalid pattern fails the emit in each; the dialects differ. A pattern
JavaScript accepts and the `regex` crate does not (lookaround,
backreferences) is refused by the Rust port with the token named, where
TypeScript emits it and the Rust engine refuses it at install.

The FLAGS are checked in both, at the same points and against the same
rules — `d g i m s u v y`, no repeats, `u` and `v` never together —
because TypeScript gets them from the `RegExp` constructor and the Rust
port states them (`canonical_regex_flags` in `src/emit.rs`). Only the
wording of the refusal differs, as it does for the pattern above. The
constructor also REPORTS the flags in that fixed order rather than the
order they were written, and the canonical compiler serialises what it
reports, so the Rust port emits them in the same order. `v` is emitted
by both and the Rust engine alone refuses it at install; that is the
engine's limit, not this compiler's.

### Element nesting is refused past 128 levels

The passes over an element walk it recursively, as the canonical
compiler does. A Rust stack that runs out aborts the process instead of
unwinding, and a grammar is untrusted input, so `emit_grammar_spec`
measures the nesting first (iteratively) and refuses a grammar that
nests one element more than `MAX_ELEMENT_DEPTH` deep, naming the rule.
TypeScript keeps going several hundred levels further and then raises a
catchable `RangeError`. The limit is `serde_json`'s own default for a
nested document, so an IR that arrives as JSON is already held to it,
and it is far past anything an author writes: the deepest nesting in the
ABNF conformance corpus is in single figures.

## Engine, observed through this compiler

`R = [ A "@" ] A` with `A = 1*ALPHA` (a probe dispatcher) compiles to
the same document in both runtimes. Run on the TypeScript engine the
value is `{ rule: "R", src: "", kids: [] }`; on the Rust engine it is the
full tree. This is the parser's difference, not this compiler's: the
parser repository's `ci/rust/notation-corpus.js` reports it for the
identical case. `rs/tests/oracle_test.rs` registers it in
`ENGINE_VALUE_DIVERGENCES` so it cannot pass or regress silently.

### A nullable suffix around hidden left recursion is not recognised

`A = [ "x" ] A [ "y" ] / "z"` compiles to the same document in both
runtimes: the suffix is nullable, so no suffix debt is owed and the tail
loop stays greedy. On the TypeScript engine the document recognises
`x* z y*` (`z`, `xz`, `zy`, `xzy` all parse, with `undefined` as the
value rather than a node); on the Rust engine every one of them is
rejected with `unexpected`, while the sources outside the language are
rejected by both. This is the parser's difference, not this compiler's:
the emitted text is byte identical, and the TypeScript compiler's own
spec installed on the Rust engine is rejected in exactly the same way.
`rs/tests/oracle/ir-nullable-suffix.json` holds the case and
`rs/tests/oracle_test.rs` registers it in
`ENGINE_REJECTS_WHAT_TYPESCRIPT_ACCEPTS`, asserted both ways, so it
cannot pass or regress silently.

## Go

The Go port records its own open divergence in `go/bnf_test.go`
(`TestActionRefPrefixDivergesFromTypeScript`): its closure-mode action
refs are named `@abnf_a<n>` where TypeScript and Rust say `@bnf_a<n>`.
