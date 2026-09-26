# Divergences

Same IR, different emitted grammar across runtimes, is a divergence. The
org rule (admin DECISIONS.md ADR-13/14) is to repair the port that
violates what TypeScript defines, and to record here what cannot be
repaired now. There is no `test/spec` register in this repository; the
executable record is three files. `rs/tests/oracle_test.rs` holds the
Rust emitter to TypeScript's serialised output byte for byte and
registers the ENGINE-level entries below. `rs/tests/divergence_test.rs`
is the register for the Rust port's own entries, one test per entry, so
repairing an entry means deleting its section here and its test there.
`go/bnf_test.go` carries the Go entries the same way. A row with no test
is a defect, not a record.

## Rust

### The word-keyword guard is `\b`, not `(?![A-Za-z0-9_])`

TypeScript emits `^option(?![A-Za-z0-9_])` for a `wordKeywords`
literal. The Rust engine compiles serialised terminals with the `regex`
crate, which has no lookaround (parser `DIVERGENCE.md`, "Regex dialect in
serialized terminals"), so the Rust port emits `^option\b`, as the Go
port does. The two agree on ASCII input and differ only for a keyword
immediately followed by a non-ASCII letter, which `\b` treats as a word
character and the lookahead does not.

Registered by `the_word_keyword_guard_is_a_word_boundary`.

### Regular expression terminals are refused at emit time

Both compilers construct the matcher when the token is allocated, a
class the overlap partition replaces with atoms included, so an invalid
pattern fails the emit in each; the dialects differ. A pattern
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

Registered by `a_pattern_outside_the_engine_dialect_is_refused_at_emit`
and `a_pattern_both_dialects_accept_still_emits`, with the flag half in
`rs/tests/regex_flags_test.rs`.

### An escape is read in the dialect of the engine that runs it

The dispatcher looks one token deep where two heads cannot meet and
deeper where they can, and whether a regex head meets a literal depends
on what its escapes mean. Each port answers for the matcher its own
engine compiles: TypeScript for JavaScript's `RegExp`, the Rust port for
the `regex` crate, the Go port for RE2. The three dialects read some
escapes differently (parser `DIVERGENCE.md`, "Regex dialect in
serialized terminals"), so the same IR emits a different dispatch depth.
For a head written without flags, beside a one-character literal:

| head | beside | TypeScript | Go | Rust |
|---|---|---|---|---|
| `\a` | a literal BEL | 1 | 2 | 2 |
| `\a` | `a` | 2 | 1 | 1 |
| `\x{41}` | `x` | 2 | 1 | 1 |
| `\U00000041` | `U` | 2 | refused | 1 |
| `\A`, `\z` | `x` | 1 | 2 | 2 |
| `\<`, `\>` | `x` | 1 | 1 | 2 |

`\a` is BEL to RE2 and to the crate, and the letter `a` to JavaScript.
`\x{41}` is `A` to RE2 and to the crate; to JavaScript without the `u`
flag it is `x` repeated 41 times, which the canonical reader leaves
unnamed, so it contests every head. `\U` spells a code point to the
crate alone, is the letter `U` to JavaScript without `u`, and is refused
by RE2 (the Go entry below). `\A` and `\z` are the start and end of the
text to RE2 and to the crate, and `\<` and `\>` the start and end of a
word to the crate alone. JavaScript without `u` reads each of the four
as the character it escapes, as RE2 reads `\<` and `\>`. A zero-width
head names no first character, so it contests every head.
Each port's reading is exact for its own engine, and the other readings
are wrong on it: read as the letter `a`, a `\a` head is held apart from
a literal BEL that the crate's matcher takes, and the literal's branch
is never reached. That was the Rust and Go ports' reading until 0.1.21.

The escapes the dialects read alike (`\x41`, `\.`, `\\`) emit the same
depth in all three, and so do the control escapes (`\n`, `\t`) and any
head no reader can name, all of which contest every head. Reading every
escape whose meaning differs between the dialects as unnamed, in all
three ports, would close this at the cost of a deeper dispatch where
none is needed. That moves the canonical compiler's output, so it is
left as a decision rather than taken here.

Registered by `an_escape_is_read_in_the_regex_crate_dialect`, with the
TypeScript side pinned in `ts/test/bnf.test.js` and the Go side by
`TestEscapeIsReadInTheRE2Dialect` in `go/bnf_test.go`.

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

Registered by `element_nesting_is_refused_one_level_past_the_limit` and
the two tests beside it, which pin the last accepted depth as well as
the first refused one.

## Engine, observed through this compiler

This section records differences the parser makes between its runtimes,
seen here because the same compiled document runs on both.
`rs/tests/oracle_test.rs` registers each one so it cannot pass or regress
silently.

Its value register, `ENGINE_VALUE_DIVERGENCES`, is empty. The one entry
it held was the probe dispatcher `R = [ A "@" ] A`, which the TypeScript
engine returns as `{ rule: "R", src: "", kids: [] }` and the Rust engine
returned as the full tree. tabnas/parser#206 (commit a801621) made the
Rust engine keep the parent's child link on the rule it pushed, as
TypeScript and Go do, and the two now agree.

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

The Go port registers its entries in `go/bnf_test.go`. It also reads an
escape in its own engine's dialect, RE2, as the Rust entry above records
for all three ports; `TestEscapeIsReadInTheRE2Dialect` pins its side.

### Closure-mode action refs are named `@abnf_a<n>`

The Go port's closure-mode action refs are named `@abnf_a<n>` where
TypeScript and Rust say `@bnf_a<n>`. Registered by
`TestActionRefPrefixDivergesFromTypeScript`, with its TypeScript twin
in `ts/test/bnf.test.js`.

### A pattern RE2 cannot compile is refused at emit time

As in the Rust port, the Go port compiles a regex terminal when it
allocates the token, so a pattern JavaScript accepts and RE2 does not
(lookaround, a backreference, JavaScript's four-digit Unicode escape,
`\cA`, `\U`) fails the emit with the token named, where TypeScript
emits it. Before 0.1.21 the Go port
panicked out of `EmitGrammarSpec` on such a pattern instead of
returning an error.

Registered by `TestEmitRefusesAPatternGoCannotCompile`.
