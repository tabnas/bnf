# tabnas-bnf (Rust)

The shared compiler behind the BNF-family grammar front-ends for the
[`tabnas`](https://github.com/tabnas/parser) parsing engine, crate
`tabnas_bnf`.

This crate holds no notation of its own. It defines an intermediate
representation (a `Grammar` of `Production`s over `Element`s) and
compiles that IR into a tabnas `GrammarSpec`. A front-end parses one
concrete syntax (ABNF, GBNF, EBNF) into the IR and calls
`emit_grammar_spec`; everything hard about that second step lives here
and is shared: desugaring repetition into helper rules, left-recursion
elimination with suffix-debt counters, tail-repeat rewriting, probe
dispatch for optional prefixes beyond the engine's bounded lookahead,
literal lifting into named lexer tokens, token allocation, first-set
analysis, and chain emission through synthetic `$stepN` rules.

This is the Rust port of the canonical TypeScript implementation in
[`../ts`](../ts); the TypeScript version is authoritative and this crate
tracks it. The Go port in [`../go`](../go) has the same shape.

## The IR

| Element | Constructor |
|---|---|
| a string literal | `Element::term("+")`, `Element::term_cs("if", true)` for a stated case-sensitivity |
| a rule reference | `Element::reference("expr")` |
| an engine lexer token | `Element::token("#NR")` |
| a character class or other regular expression | `Element::regex("[a-z]", "")` |
| a prose terminal | `Element::prose("number")` |
| `[ A ]`, `*A`, `1*A`, `m*nA` | `Element::opt`, `Element::star`, `Element::plus`, `Element::rep(min, max, inner)` |
| `( A / B )` | `Element::group(vec![alt_a, alt_b])` |

A `Production` is a name and a list of alternatives, each a sequence of
elements; `Grammar::new(productions)` is what the compiler takes. Every
IR type also serializes with serde under the TypeScript field names, so
an IR built in one runtime loads in the other.

## Use

Build an IR, compile it, and install the result on an engine:

```rust
use tabnas_bnf::{emit_grammar_spec, ConvertOptions, Element, Grammar, Production};

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let grammar = Grammar::new(vec![
        Production::new("val", vec![vec![Element::reference("add")]]),
        Production::new("add", vec![vec![Element::token("#NR")]]),
    ]);
    let spec = emit_grammar_spec(&grammar, &ConvertOptions::tag("demo"))?;
    assert!(spec.rule.contains_key("val"));

    let mut parser = tabnas::Tabnas::new();
    spec.install(&mut parser)?;
    let tree = parser.parse("42")?;
    println!("{}", tree.to_json());
    Ok(())
}
```

`install` binds the compiler's closure-mode actions by name and installs
the grammar. For a grammar that has to travel as data, compile with
`builtins` on, reduce it to the engine's pure wire format, and load that
anywhere:

```rust
use tabnas_bnf::{emit_grammar_spec, to_jsonic, to_pure_spec, ConvertOptions, Element, Grammar, JsonicOptions, Production};

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let grammar = Grammar::new(vec![Production::new(
        "greet",
        vec![vec![Element::term("hi")], vec![Element::term("hello")]],
    )]);
    let opts = ConvertOptions::tag("demo").builtins(true);
    let spec = emit_grammar_spec(&grammar, &opts)?;

    let pure = to_pure_spec(&spec)?;
    let text = to_jsonic(&pure, JsonicOptions { strict: true, indent: None });

    let mut parser = tabnas::Tabnas::new();
    parser.grammar(&tabnas::GrammarSpec::from_json(&text)?)?;
    let tree = parser.parse("hello")?;
    assert_eq!(tree.to_json()["rule"], "greet");
    Ok(())
}
```

`to_recognition_spec` drops the tree and value builders as well, for a
grammar that only has to recognise. User semantic actions attach by rule
phase or by alt mark (`ConvertOptions::marks`) through `attach_actions`
and `attach_action_slots`; `mark_listing` prints the marks the compiler
assigned.

## Options

| Option | Effect |
|---|---|
| `start` | Start rule name (default: the first production). |
| `tag` | Group tag stamped on every emitted alt, and the prefix the shared compiler's own diagnostics carry (default `bnf`; a front-end passes its own). A front-end's parse error on the grammar source is raised before these options apply, so it keeps that front-end's own fixed prefix. |
| `builtins` | Emit probe dispatch and tree building as engine `$`-builtin refs instead of closures, keeping the spec function-free and serializable. |
| `marks` | Emit a stable `m` mark per user-rule alt, enabling `@<rule>:o\|c:<mark>` user-action references. |
| `word_keywords` | Treat word-like literals as whole-word keywords, so `"option"` does not match the prefix of `optional`. |
| `token_classes` | Compile a production whose alternatives are all single literals or tokens to one engine token set, named after it, that stands as one token at lookahead positions. The tree is unchanged: a leading reference the plain compile would inline is consumed as the one token, and the rule keeps its alternates and its node everywhere else. |
| `provenance` | Emit `meta.provenance`, the map from each generated rule name back to the production it came from. On by default. |

## Install

The `tabnas` crate is not published to a registry, so the engine is
consumed as a **sibling checkout**, the standard tabnas development
model. Clone `https://github.com/tabnas/parser` next to this repository
and point at it:

```toml
[dependencies]
tabnas-bnf = { path = "../bnf/rs" }
tabnas = { path = "../parser/rs" }
```

Both entries are needed. A crate's dependencies are not passed on to its
dependents, so `tabnas-bnf` alone does not put `tabnas` in your extern
prelude, and the examples above that name `tabnas::Tabnas` would not
resolve.

## Differences from the canonical TypeScript

The emitted grammar is held to the TypeScript compiler's output byte for
byte (see `tests/oracle_test.rs`). The differences are in the surface,
not the grammar:

- **The word-keyword guard is `\b`.** TypeScript emits the negative
  lookahead `(?![A-Za-z0-9_])` after a keyword; the engine's regex
  dialect has no lookaround, so this port emits `\b`, as the Go port
  does. The two agree on ASCII text and differ only when a keyword is
  immediately followed by a non-ASCII letter.
- **Regular expression terminals are checked at emit time** against the
  engine's dialect, so a pattern the engine would refuse at install
  (lookaround, backreferences) is refused here first, naming the token.
- **Source spans count bytes.** `SrcSpan` offsets are in the units the
  front-end's own engine tokens use, and this engine counts bytes where
  TypeScript counts UTF-16 code units.
- **Closure-mode actions are descriptors.** A spec compiled without
  `builtins` carries `RefAction` values in `refs` rather than functions;
  `GrammarSpec::install` (or `bind`) registers them on an engine by name.
  User actions are `Arc` closures in an `ActionsMap`, a list in
  attachment order.
- **Element nesting is refused past 128 levels.** A grammar is untrusted
  input and the passes over an element are recursive, so a grammar that
  nests one element deeper than that is an error return naming the rule.
  TypeScript keeps going several hundred levels further before raising a
  `RangeError` the caller can catch. No grammar an author writes comes close.
- **Diagnostics are prefixed per thread.** The tag of the most recent
  emit on the current thread prefixes every diagnostic this compiler
  raises, so two threads compiling two notations never see each other's
  prefix.

## Build and test

The engine is a path dependency on the sibling checkout, so there is
nothing to fetch:

```bash
cargo test --all-targets
cargo test --doc
```

Or, from the repository root, `make test-rs`. For what CI would say,
including formatting and the `Cargo.lock` check, run `ci/rust/run.sh`.

The suite ports the TypeScript and Go tests that pin behaviour through
the IR, and holds the emitter to the canonical compiler with the oracle
fixtures under `tests/oracle/`: each one is an IR, either exactly as a
front-end handed it over or written by hand for a shape no front-end
emits, together with the strict-jsonic text TypeScript emitted for it (or
the message TypeScript refused it with) and the engine's verdict on a few
sources.

## License

MIT.
