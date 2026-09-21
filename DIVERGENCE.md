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

## Engine, observed through this compiler

`R = [ A "@" ] A` with `A = 1*ALPHA` (a probe dispatcher) compiles to
the same document in both runtimes. Run on the TypeScript engine the
value is `{ rule: "R", src: "", kids: [] }`; on the Rust engine it is the
full tree. This is the parser's difference, not this compiler's: the
parser repository's `ci/rust/notation-corpus.js` reports it for the
identical case. `rs/tests/oracle_test.rs` registers it in
`ENGINE_VALUE_DIVERGENCES` so it cannot pass or regress silently.

## Go

The Go port records its own open divergence in `go/bnf_test.go`
(`TestActionRefPrefixDivergesFromTypeScript`): its closure-mode action
refs are named `@abnf_a<n>` where TypeScript and Rust say `@bnf_a<n>`.
