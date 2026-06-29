# Concepts

This document explains how `@tabnas/abnf` works and why it is built the
way it is. For the API see [reference.md](reference.md).

## What the compiler is, and what the engine is

`@tabnas/abnf` is a **compiler**, not a parser. It reads ABNF source and
emits a tabnas `GrammarSpec` — a declarative description of rules,
tokens, and AST-building actions. The actual parsing is done by the
**tabnas engine** (`@tabnas/parser`), a push-down recursive-descent
parser. The relationship is:

```
ABNF source ──abnfConvert──▶ GrammarSpec ──tn.grammar──▶ engine ──parse──▶ AST
```

The compiler decides *what* grammar the engine should run; the engine
decides *whether a given input matches* and *what tree to build*. This
separation is why the same compiled grammar can be serialised, shipped,
and re-loaded on a bare engine in another process or language — the spec
is just data (plus, optionally, references to engine-provided builtins).

## The ABNF dialect: `=` and `/`

This is RFC 5234 ABNF, not EBNF or BNF. Rules are defined with `=`,
alternatives separated with `/`:

```abnf
greet = "hi" / "hello"
```

That choice has practical consequences inside the implementation. `/`
is the alternation operator, which means the regex-literal delimiter
(also `/`) cannot be used — so character classes are expressed with ABNF
numeric values (`%x30-39`) instead. Comments use `;` (the ABNF
convention), and the meta-grammar that parses ABNF disables the
engine's default `//` and `/* */` comment styles so they don't collide
with `/`.

ABNF's case rules are honoured: a bare `"GET"` is case-*insensitive*
(it lowers to an anchored, `i`-flagged regex match token); `%s"GET"`
forces case-sensitivity (a fixed token); `%i"GET"` is the explicit form
of the default. A literal with no ASCII letters (`"+"`) is
case-independent regardless and emits as a fixed token.

## Built-in lexer tokens: `TX`, `NR`, `ST`, `VL`

ABNF is scannerless — a rule like `ident = ALPHA *( ALPHA / DIGIT )`
matches one character at a time. That is fine for a language with no
whitespace (URIs), but it fights the engine's lexer for a
whitespace-significant language. The tabnas lexer already tokenises whole
words and **ignores** whitespace and comments between tokens, so a
char-by-char rule would greedily merge two space-separated identifiers
(`int32 name` → `int32name`): the space the grammar relied on as a
boundary was skipped by the lexer before the rule ever saw it.

The fix is to reference the lexer's whole-word tokens directly. Four bare
uppercase names are reserved for this and compile to a single token
terminal (`s: '#…'`) rather than a rule reference:

| Name | Token  | Matches (default lexer)        |
|------|--------|--------------------------------|
| `TX` | `#TX`  | bareword / identifier          |
| `NR` | `#NR`  | number (int, float, hex, …)    |
| `ST` | `#ST`  | quoted string                  |
| `VL` | `#VL`  | `true` / `false` / `null`      |

```abnf
field = [ label ] type ident "=" fieldNumber ";"
type  = ident *( "." ident )
ident = TX
fieldNumber = NR
```

A token terminal is a *terminal*, so it never participates in
left-recursion inlining and `int32 name` parses as two `#TX` tokens. Wrap
a token in a named rule (`ident = TX`) when you want it to surface as its
own `{rule, src, kids}` node. A user (or RFC 5234 core) rule of the same
name always wins — define `TX = …` and `TX` is your rule again. This is
the same convention abnf's own meta-grammar uses (`#TX` for rule names,
`#ST` for string literals); it keeps lexical atoms in the simple lexer and
structure in the grammar.

### Whole-word keywords: `wordKeywords`

In a tokenised grammar, string-literal keywords must align with the
whole-word `TX` tokenisation. By default a literal matches greedily, so
`"option"` would match the `option` prefix of the identifier `optional`.
The `wordKeywords: true` convert option fixes this: a literal ending in a
word character only matches when not immediately followed by another word
character (`[A-Za-z0-9_]`). Turn it on for keyword-rich tokenised
languages (proto, SQL); leave it off for char-level/scannerless grammars
where a literal legitimately precedes a word character (e.g. `"v" 1*HEXDIG`
in RFC 3986, where `v1f` must match `v` then `1f`).

## The meta-grammar: ABNF is parsed by tabnas itself

The ABNF source is parsed by a tabnas instance whose grammar (the
`abnfRules` table in `converter.ts`) is *itself* written in tabnas
rules. There is no separate hand-rolled lexer/parser for ABNF — the
compiler bootstraps on the engine it targets. That table remaps the
engine's default JSON-oriented tokens (`{`, `}`, `:`, …) to ABNF's
operators (`=`, `=/`, `/`, `*`, `(`, `)`, `[`, `]`, `%xNN`, …) and adds
match-token regexes for repetition counts and numeric values.

## The compilation pipeline

`emitGrammarSpec` runs these passes in order:

1. **Merge incrementals** — fold each `name =/ alt` into the earlier
   `name = …` production.
2. **Splice core rules** — transitively add any referenced RFC 5234
   Appendix B.1 core rules (`ALPHA`, `DIGIT`, `HEXDIG`, …) the user
   didn't define locally. User definitions always win.
3. **Eliminate left recursion** — Paull's algorithm rewrites direct
   (`P → P α | β`) and indirect (cycles through other rules) left
   recursion into the right-recursive iterative form
   `P → β (α)*`, which a push-down parser can run without re-entering a
   rule at the same source position.
4. **Rewrite probe dispatches** — detect the ambiguous optional-prefix
   pattern `[X D] Y` and rewrite it into a probe + phase-retry
   dispatcher (see below).
5. **Desugar** — flatten EBNF sugar (`?`, `*`, `+`, `m*n`, grouping)
   into plain ABNF helper productions.
6. **Allocate tokens** — one fixed token per unique case-sensitive
   literal, one match-token regex per case-insensitive literal or
   numeric range.
7. **Compute FIRST sets** — to disambiguate alternatives by lookahead.
8. **Emit rules** — each production becomes one or more tabnas rules.

## The output AST: `{rule, src, kids}`

Every rule emits a node with three fields:

- `rule` — the grammar rule's name (only on *user-declared* rules).
- `src` — the concatenated source text the rule matched.
- `kids` — child nodes, one per referenced user rule.

Productions have a `nodeKind`:

- **user** — your declared rules. They become tagged `{rule, src, kids}`
  nodes.
- **core** — the RFC 5234 char-class bricks (`ALPHA`, `DIGIT`). They
  *flatten*: their `src` rolls up into the enclosing user rule rather
  than cluttering the tree with one node per matched character.
- **helper** — synthetic rules from desugaring, dispatching, and
  multi-segment chaining. They also flatten transparently.

This is why `1*DIGIT` inside a rule contributes characters to that
rule's `src` but adds no child nodes — `DIGIT` is a core rule.

### A subtlety: leading refs get inlined

Because left-recursion elimination uses Paull's substitution, a rule
whose alternative *begins* with a reference to another user rule has
that rule inlined — so it does **not** appear as a separate child node.
`p = "a" q` keeps `q` as a child (it's preceded by `"a"`), but
`p = q "a"` would inline `q`. If you want a sub-rule to stay visible in
the tree, don't put its reference at the very start of an alternative.

## Multi-segment alternatives and chains

A "single-segment" alternative has at most one rule reference, at the
end (`"a" "b" inner`). It collapses to one tabnas alt. An alternative
with an interior reference (`"a" inner "b" inner "c"`) is split into a
**chain**: the head rule consumes the first run of terminals and pushes
the first ref; synthetic `<rule>$stepN` continuation rules handle the
rest. The chain rules are helpers, so they flatten — the user sees a
single tidy node.

## The probe dispatcher: unbounded lookahead

ABNF has grammars that aren't LL(k) for any bounded k. The canonical
case is the `[ userinfo "@" ] host` shape in RFC 3986's `authority`:
`userinfo` and `host`'s `reg-name` share a character vocabulary, so no
fixed-length lookahead can tell whether you're inside the optional
prefix until you reach (or fail to reach) the disambiguating `@`, which
may be arbitrarily far away.

For the pattern `[X D] Y` — an optional group ending in a terminal `D`,
followed by a tail `Y` whose leading vocabulary overlaps with `X`'s —
the compiler emits a **probe + phase-retry dispatcher**:

1. On first entry it marks the position and pushes a failure-proof
   *probe* rule that greedily consumes the joint `X`/`Y` vocabulary
   (everything except `D`).
2. When the probe returns, it peeks the next token: if it's `D`, commit
   to the `X D Y` branch; otherwise commit to the `Y` branch. Then it
   rewinds to the mark and retries itself so the committed branch
   actually parses.

The primitives this uses (`r:` retry, `k:` counter config, `c:`
guards, `ctx.mark`/`ctx.rewind`/`ctx.t`) are all ordinary engine
machinery — no special parser support is needed.

## Recognition vs. tree-building, and pure-data emission

A `GrammarSpec` carries two kinds of behaviour:

- **Recognition** — whether an input matches. This is fully *structural*
  (`s`/`p`/`r`/`b`/`g` plus declarative `c:` counter conditions). No
  functions required.
- **Tree-building** — constructing the `{rule, src, kids}` AST. By
  default this is done by closures in the spec's `ref` map.

`abnfCompile` (and `toRecognitionSpec` / `toPureSpec`) exploit this
split to emit **pure data** — a grammar with no closures, serialisable
as jsonic text and re-loadable anywhere:

- **Recognition mode** (`recognition: true`, the default) drops *all*
  tree-building actions. The result recognises the same language; it
  just returns a generic tree.
- **Full mode** (`recognition: false`) keeps the tree-building actions,
  but as engine `$`-builtin references (`@node$`, `@capture$`,
  `@bubble$`) plus per-alt `k` config — still pure data, and it rebuilds
  the exact `{rule, src, kids}` tree on load.

To make this possible the converter offers `builtins: true`, which emits
both control logic (the probe dispatcher) and tree-building as
`$`-builtin refs instead of closures. The one case that *cannot* be pure
recognition data is a probe grammar converted *without* `builtins` — its
control logic is closures — so `toRecognitionSpec` refuses it with an
`AbnfCompileError`. `abnfCompile` sidesteps this by always converting
with `builtins: true`.

## User actions and marks: the `m`-mark design

User actions hook into the grammar without you editing the generated
spec by hand. The trick is **marks**: with `marks: true`, every
user-rule alternative is stamped with a stable, human-predictable
identifier `m` — its leading discriminator (token name, pushed rule
name, or `_` for empty), with `~N` suffixes to break collisions. You
then bind a function to `@<rule>:o:<mark>` (open) or `@<rule>:c:<mark>`
(close), or to a rule-phase hook `@<rule>:<bo|ao|bc|ac>`.

Your action is injected into the alt's action list *after* the
compiler's own tree action (using the engine's array-`a` form), so
`r.node` already holds the `{rule, src, kids}` node when your code runs.
This composes cleanly in both closure and `builtins` mode. `$` is
reserved for engine builtins, so a user action ref may not contain it.

`attachActionSlots` is the serialisable variant: it injects *named*
action slots into a pure-data spec, to be bound by the consumer at load
time — letting a shipped grammar carry user-action hooks resolved by
name.

## Design trade-offs

- **Bootstrapping on the engine** keeps the ABNF parser tiny and exact —
  but means the compiler can only target tabnas, and the ABNF
  meta-grammar inherits the engine's tokenisation model.
- **Paull's substitution always runs** (even for cycle-free grammars),
  because populating the lookahead token columns from inlined prefixes
  is what makes nested regex matchers fire correctly. The cost is that
  leading-ref sub-rules get inlined and lose their own AST node.
- **Pure-data emission** trades a little expressiveness (probe grammars
  need the `$`-builtins to be present on the loading engine) for
  portability — a compiled grammar is just text.
- The probe dispatcher handles the common *terminal-disambiguated*
  optional-prefix ambiguity; grammars whose disambiguator is itself a
  non-terminal are not yet covered.
