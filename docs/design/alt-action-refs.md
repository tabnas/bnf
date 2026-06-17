# Design: Referencing alt actions from ABNF via `@`-name refs

| | |
|---|---|
| **Status** | **Shipped (TS + Go).** Engine (`$`-builtins, array-`a`, eager sentinel, `$`-reservation, schema-version gate) landed in `@tabnas/parser` for **both** the TS engine and the Go port; the compiler emits `v:1` and reserves `$`. A serialized, function-free grammar recognizes identically on both engines, pinned by shared cross-engine fixtures. |
| **Scope** | `@tabnas/bnf` (ABNF → `GrammarSpec` compiler) + a proposed `@tabnas/parser` engine extension |
| **Repo** | This document lives in `tabnas/abnf` because that is where the feature is driven. The engine-side changes (the alt `m` field and the `fnref` resolver extension) ultimately belong in `tabnas/parser`. |

## 1. Problem

The `@tabnas/bnf` compiler turns ABNF source into a tabnas `GrammarSpec`. The
grammar it emits is purely *structural* — it can match input and build a tree,
but there is no way for a grammar author to attach **custom code** (semantic
actions) to the productions, the way the hand-written programmatic grammars do.

The canonical illustration is the small "add" grammar from the tabnas parser
README. Programmatically it carries two actions:

```js
tn.grammar({
  options: { fixed: { token: { '#PL': '+' } }, rule: { start: 'val' } },
  rule: {
    val: {
      open:  [ { p: 'add', a: (r) => { r.node = 0 } } ],            // action A
      close: [ {} ],
    },
    add: {
      open:  [ { s: '#NR', a: (r) => { r.parent.node += r.o[0].val } } ], // action B
      close: [ { s: '#PL', r: 'add' }, {} ],
    },
  },
})
```

…but its ABNF form has nowhere to put A and B:

```abnf
val = add
add = NR [ PL add ]
```

- **A** = "zero the accumulator" — fires on `val` *before* descending into `add`
  (a rule-level, before-open action).
- **B** = "add this number" — fires on `add` *after* the `NR` open match
  (reads `r.o[0]`, so it is tied to a specific alternative).

The design goal: **let an author attach named actions to the compiled grammar
without mangling the ABNF syntax** — the source stays valid RFC 5234 ABNF, and
the binding happens out of band through a reference map.

## 2. What already exists (and what it isn't)

### 2.1 jsonic: `resolveFuncRefs` (value-level `@`)

tabnas descends from jsonic. jsonic resolves `@`-strings in a spec via a single
recursive utility (`src/utility.ts`):

```ts
// Recursively resolve FuncRef strings in an options object to actual functions,
// and `@/pattern/flags` strings to RegExp instances.
function resolveFuncRefs(obj, ref) {
  if (/* scalar string starting with '@' */) {
    if ('@' === obj[1]) return obj.substring(1)        // @@x  -> literal "@x"
    if ('SKIP' === obj.substring(1)) return SKIP       // @SKIP -> SKIP sentinel
    const m = obj.match(/^@\/(.*)\/([\w]*)$/)
    if (m) return new RegExp(m[1], m[2])               // @/re/flags -> RegExp
    if (ref) { const fn = ref[obj]; if (typeof fn === 'function') return fn } // @name -> fn
  }
  // ...recurse into arrays / plain objects...
}
```

The convention here is **value resolution only**: a `@`-string becomes a
function / RegExp / sentinel / escaped literal. It carries **no phase meaning** —
the role of a resolved function is decided entirely by *which field* holds it.

### 2.2 tabnas: `fnref()` (the `@<rule>-<phase>` convention)

On top of that, `tabnas/parser`'s `rules.ts` adds a real **name → phase**
convention in `fnref()`:

```ts
const reserved = [`@${rn}-bo`, `@${rn}-ao`, `@${rn}-bc`, `@${rn}-ac`]
```

Given the current rule name `rn`, `fnref()` scans for these reserved keys and
**auto-installs** each function onto the corresponding rule **state-action hook**:

- `bo` / `ao` / `bc` / `ac` = before-open / after-open / before-close / after-close.
- Modifiers `@<rule>-<phase>/replace`, `/prepend`, `/append` (plain ⇒ append)
  control layering. `/replace` clears prior actions for the phase and *owns* it;
  install is deduped by function identity per phase.

This is the connotational, name-drives-phase mechanism. It cleanly covers
**rule-level** actions.

### 2.3 The gap

`fnref()`'s convention addresses **rule phases**, not **individual
alternatives**. Action A above is a rule-phase action (`val` before-open) and
fits perfectly as `@val-bo`. Action B is per-alternative (it belongs to the
`#NR` alt of `add`, and reads that alt's matched token), and there is no name
for "the action of *this* alt."

Addressing an alt by its **source position** does not work, because the
ABNF → tabnas mapping is neither one-to-one nor onto (see `converter.ts`):

- `[ ]`, `*`, `1*`, `( )` are **desugared before emission**.
- A multi-ref alternative **splits** into `<head>$step1`, `$step2`… rules.
- A ref-only alternative **fans out** to one tabnas alt *per FIRST-set token*.
- Empty alts are **reordered** to the end.
- Left recursion is **rewritten** (`P = P a / b` → `P = b *(a)`).
- Optional-prefix ambiguity synthesises a **probe + dispatcher** with helper rules.
- `name =/ alt` **folds** alternatives from several productions into one rule.

So "the third element on line 2" has no stable single image in the output.

## 3. Proposal

### 3.1 An alt `m` (mark) field

Extend the engine so every alt may carry an optional **`m` (mark)**: an
identifier for the alternative within its rule phase. Marks are emitted by the
compiler, not written by the author. Adding `m` is backward-compatible (absent ⇒
ignored).

Alt actions are then referenced by **mark** rather than by source position:
the action attaches to the emitted alt(s) bearing that mark.

### 3.2 Reference-name grammar — use `:` as the separator

The legacy convention used `-` as the separator (`@<rule>-bo`). That is
**ambiguous for ABNF**, because RFC 5234 rule names legitimately contain
hyphens (`pl-add`, `path-abempty`, …). `@pl-add-bo` cannot be unambiguously
split into rule + phase. We therefore standardise on `:` as the separator:

```
ref     = "@" rulename ":" suffix [ ":" mark ]
suffix  = "bo" / "ao" / "bc" / "ac"   ; rule-phase hook  (no mark)
        / "o"  / "c"                   ; per-alt action  (mark required)
mark    = 1*( ALPHA / DIGIT / "_" / discriminator chars )
```

Examples:

| Ref | Meaning |
|---|---|
| `@val:bo` | `val` rule, before-open hook |
| `@add:ao` | `add` rule, after-open hook |
| `@add:o:NR` | `add` rule, **open**-phase alt marked `NR` |
| `@pl-add:c:_` | `pl-add` rule, **close**-phase fallback (empty) alt |

The full alt-action form is therefore **`@name:<suffix>:<mark>`**. Rule names
may contain `-` without ambiguity because the structural separator is now `:`.
Modifier suffixes (`/replace`, `/prepend`, `/append`) are retained and attach at
the end (`@add:o:NR/prepend`).

### 3.3 Mark derivation — logical source-alternative identity

Marks must be **deterministic** *and* **predictable by a human who only reads
the ABNF source** (deterministic alone is not enough — raw output ordinals are
deterministic but shift under every transformation in §2.3).

Rule: **a mark identifies the *logical source alternative***, and the emitter
stamps the *same* mark on **every physical alt that descends from that one
source alternative** — all FIRST-set fan-out siblings, the `$stepN`
continuations, etc.

The mark itself is a readable **discriminator**, preferring the alternative's
FIRST token(s) (`@add:o:NR`), with reserved marks for tokenless alts
(`_` for the empty/fallback alt). Where two alternatives share a leading token
(the cases that already require a peek/probe), the discriminator is extended to
stay unique per logical alternative.

Consequences:

- **Uniqueness is per *logical* alternative, not per physical alt.** Several
  emitted alts may share a mark; an action installs on **all** of them. This is
  what dissolves the fan-out ambiguity ("which of the N emitted alts?") — the
  answer is "all alts of this logical branch."
- A grammar edit that does not change a branch's discriminator does not rebind
  its action.

### 3.4 Multiple actions on one alt — synthetic wrapper

An alt's `a:` is a single function, not a list. When more than one action
resolves to the same alt — e.g. the compiler's own tree-building action plus a
user action, or two user actions — **replace `a:` with a synthetic wrapper**
that invokes the constituent actions **in attachment order**:

```ts
// pseudo
function wrap(actions) {
  return (r, ctx) => { for (const fn of actions) fn(r, ctx) }
}
alt.a = wrap([existingA, newA])   // attachment order preserved
```

Attachment order is: the compiler's auto-generated action first (it allocates /
accumulates the node), then user actions in resolution order. The `/prepend`,
`/append`, `/replace` modifiers adjust position within this list:

- plain / `/append` — appended (runs after existing).
- `/prepend` — inserted before existing user actions (but conventionally still
  after the compiler's node-init action; see Open Questions).
- `/replace` — discards prior actions for that alt and owns it.

This keeps the engine's scalar-`a:` shape intact while supporting composition,
mirroring the layering semantics `fnref()` already provides for phase hooks.

### 3.5 Worked example — the add grammar

ABNF stays pristine:

```abnf
val = add
add = NR [ PL add ]
```

Actions supplied entirely out of band, keyed by the convention:

```js
tn.bnf(src, {
  ref: {
    '@val:bo':   (r) => { r.node = 0 },                    // A: rule-phase hook
    '@add:o:NR': (r) => { r.parent.node += r.o[0].val },   // B: alt action, NR branch
  },
})
```

- `@val:bo` installs on `val`'s before-open hook (unchanged `fnref` mechanism,
  new separator).
- `@add:o:NR` installs on every emitted alt of `add` that descends from the
  `NR …` source alternative, wrapped after the compiler's src-accumulation
  action.

> **Implemented** (closure mode). Marks are assigned from each alt's leading
> discriminator (token name sans `#` / pushed rule / `_`), opt-in via
> `bnfConvert(src,{marks:true})` and listed by CLI `--marks`. Verified demo:
> `op = "inc" / "dec"` with `{'@op:o:INC':…, '@op:o:DEC':…}`. See
> [`implementation-diary.md`](./implementation-diary.md) §6.

## 4. Feasibility & edge cases

| Case | Handling |
|---|---|
| FIRST-set fan-out (1 src alt → N alts) | Same mark on all N; action installs on all. |
| `$stepN` continuation chains | Steps inherit the head alt's mark. |
| `=/` folded alternatives | Each keeps its own source identity / discriminator. |
| Empty / fallback alt | Reserved mark `_`. |
| Left-recursion rewrite | Source alternatives do not survive as distinct branches — **look-up-only** marks (§5) or refactor into named rules. |
| Probe + dispatcher | Alternatives are redistributed across synthetic rules — **look-up-only** marks or refactor. |
| Synthetic rules (`$stepN`, dispatcher, core) | Not user-addressable; user actions belong on user-declared productions. |

The two genuinely hard cases (left-recursion, probe dispatch) are the ones that
*restructure* rather than *expand* alternatives; no source-only derivation is
predictable there. For those, the author falls back to either look-up marks or
splitting the rule into named sub-rules and using `@<rule>:<phase>` (which needs
**no** new machinery).

## 5. Discoverability & validation

Determinism without a lookup is a guessing game, so two cheap supports make the
feature usable:

- **Emit marks into the CLI output.** `tabnas-bnf` already prints the compiled
  `GrammarSpec`; include `m` on each alt, and add a `--marks` listing:
  `rule → phase → mark → matched tokens → source span`. "Predict the mark"
  becomes "look it up."
- **Validate refs at resolution.** Error on any `@rule:o:<mark>` (or
  `@rule:<phase>`) that matches no emitted alt/hook. `fnref()` currently
  *silently* ignores unknown keys, so a typo or a stale mark after an edit
  would no-op invisibly. Loud failure keeps the coupling honest.

## 6. Compilation mode: pure-recognition output and `$`-builtins

A separate `@tabnas/bnf` operating mode — **compilation mode** — emits a
serializable tabnas grammar (jsonic format) rather than installing a live one.
The key realisation that makes this clean is that **functions are not needed to
*represent* ABNF; they are only used to *shape the output tree*.**

### 6.1 Recognition is structural; trees need actions

Tabnas's alternate-spec fields split cleanly:

- **Structural (data):** `s` (token pattern), `p` (push), `r` (replace),
  `b` (backtrack), `g` (group tag), `n` (counter increment), `u`/`k` (custom
  data).
- **Function-valued:** `a` (action), `c` (condition — *also* expressible
  declaratively as an object matched against `rule.n` counters), `h`, `e`.

Every function the converter currently emits is for **AST construction**, not
recognition: `segmentToAlt`'s `a:` (allocate `r.node`, accumulate
`r.o[i].src`), `captureChildRef` (merge child into `kids`), the `__start__`
close `a:` (copy child node up), the FIRST-peek alts' `a:` (node init only —
the `s`/`b`/`p` do the recognising). Strip them all and the grammar still
recognises the same language.

**Backtracking is structural.** `b` is a number ("match for the decision, back
up N"), so ordered first-match alts, bounded lookahead, optional/repetition are
all function-free. This was verified empirically: a function-stripped `greet`
spec installed on a bare engine still accepts `hi`/`hello` and rejects `nope`.

### 6.2 The one exception: unbounded lookahead

The **probe + phase-retry dispatcher** (optional-prefix ambiguity, `[X D] Y`,
where the disambiguator D is arbitrarily far ahead) is the *only* place
functions do recognition work — a fixed `b: N` cannot express "scan a run of
unknown length, then peek for D." It synthesises `a:` control actions
(mark / probe / peek / rewind) and `c:` guards keyed on `r.k.pd_phase`
(`converter.ts:1411-1454`). So a probe-requiring grammar is the only thing that
cannot be emitted purely structurally as the converter compiles it today.

### 6.3 Standard `$`-builtins

Rather than refuse those grammars, expose the probe machinery as **named engine
builtins** the grammar references by name. Convention:

- **`@name$`** — a trailing `$` in a *ref name* marks an engine-provided
  builtin, resolved from a standard ref library the engine merges into
  `spec.ref` at load time.
- **`$` is disallowed in user refs.** This partitions the namespace so builtins
  never collide with user `@<rule>:<phase>:<mark>` refs, and makes serialized
  grammars self-documenting (a reader sees `@probe$` and knows it is stock).
- Namespace note: `$` already appears in synthetic **rule names**
  (`<head>$stepN`). Those are a *different* namespace; the convention is
  specifically "trailing `$` in a **ref** name = builtin."

A builtin is **parameterised by declarative config**, not by being many distinct
functions. The probe carries its vocab token set, disambiguator token, and
branch rule names in a serializable `k`/`u` blob the builtin reads:

```jsonic
R$pd0: {
  open: [
    { c: { n: { pd_phase: 0 } }, a: '@probeMark$',   k: { probe$: {
        vocab: ['#ALPHA'], d: '#AT', withBranch: 'R$pd0$with', elseBranch: 'R$pd0$no' } } }
    { c: { n: { pd_phase: 1 } }, p: 'R$pd0$with' }
    { c: { n: { pd_phase: 2 } }, p: 'R$pd0$no' }
  ]
  close: [ { c: { n: { pd_phase: 0 } }, a: '@probeDecide$' } ]
}
```

Note the phase guards became **declarative** `c: { n: { pd_phase: N } }`
counter conditions, leaving only the genuinely procedural pieces
(`@probeMark$` / `@probeDecide$`) as code. All token/rule references are names;
the whole rule is pure data.

### 6.4 The generalisation — everything becomes data

The probe is not special: **every** converter-emitted function is generic and
parameterisable from data —

- `segmentToAlt`'s action ← `(initNode, ruleName, nodeKind, nterms)` → a
  `@node$` / `@src$` builtin reading those from `k`.
- `captureChildRef` ← `(ruleName, nodeKind)` → a `@capture$` builtin.

All parameters are strings / numbers / bools. So the two "modes" collapse: the
**output is always pure data**, and the only difference is *which* refs appear:

| Mode | Structure | Tree builtins | Probe builtins | User actions |
|---|---|---|---|---|
| Pure recognition | ✓ | — | `@probe…$` (when needed) | — |
| Full (AST) | ✓ | `@node$`/`@capture$` | `@probe…$` | `@<rule>:<phase>:<mark>` |

Functions then live in exactly two places — the engine's `$`-builtin stdlib and
the user's supplied ref map — and **never in the wire format**. "Does
compilation need functions?" → no; it *references* named ones.

> **Implemented.** The tree builtins (`@node$`, `@capture$`, `@bubble$`) and the
> full pure-data mode now exist (CLI `--full`, `bnfCompile(src,
> {recognition:false})`). A full-mode grammar reloaded on a bare engine builds
> trees **deep-equal** to the live plugin (greet / pair / arith / probe), and
> `bnfConvert(src,{builtins:true}).ref` is empty — zero closures reach the wire.
> See [`implementation-diary.md`](./implementation-diary.md).

### 6.5 Caveats

1. **Architectural seam.** The `$`-builtins must ship with `@tabnas/parser`
   (or a stdlib the engine auto-merges) so a serialized grammar loads
   standalone. Engine change, not just compiler.
2. **Config schema is a public contract** between compiler output and the
   builtins. Probe-algorithm or node-shape changes must stay schema-compatible
   or be versioned (`@probe$2`, or a version field in the config).
3. **Serialisation fidelity.** Match-token RegExps carry an extra `eager$`
   flag (`{source, flags, eager$:true}`) that `@/source/flags` cannot represent.
   **Resolved:** a sibling sentinel `@~/source/flags` carries it, and
   `resolveFuncRefs` reconstructs `eager$` on load.
4. **Maintenance.** One builtin library to test, but it becomes load-bearing
   for every compiled grammar.

### 6.6 Prototype status

Implemented and verified end-to-end against a locally-built engine.

**`@tabnas/bnf` (this repo):**

- `toRecognitionSpec(spec)` — RegExp-preserving transform that drops the `ref`
  map and the `a`/`bo`/`bc` tree-building functions. It **refuses**
  (`BnfCompileError`) any spec with *control* refs surviving the strip — i.e. a
  closure-mode probe dispatcher (§6.1/§6.2 boundary, enforced).
- `toJsonic(value)` — relaxed-jsonic serialiser (unquoted identifier keys,
  single-quoted strings, `RegExp → @/source/flags`), plus a strict-JSON mode
  used for round-trip verification.
- A `builtins` convert option that emits the probe dispatcher with `@probe…$`
  refs + `k` config (§6.3) instead of closures; `bnfCompile` turns it on so
  probe grammars serialise as **pure data**.
- `bnfCompile(src, opts)` and a CLI `--compile` flag.

**`@tabnas/parser` (engine — prototyped locally, captured in
[`engine-prototype.patch`](./engine-prototype.patch)):**

- `src/builtins.ts` — the `$`-builtin stdlib: `@probeInit$`, `@probeDecide$`,
  `@probePhase0$`/`1$`/`2$`. Generic functions; the disambiguator token rides
  in `k.pd_d`.
- `grammar()` merges `BUILTIN_REFS` under any spec-supplied `ref` before
  resolving, so a serialized function-free spec resolves `@probe…$` by name.

Verified: an optional-prefix grammar (`R = [ A "@" ] A`) compiles with **no**
`@bnf_` closures, only `@probe…$` builtins; round-trips (reparse → resolve →
install on a bare engine) and recognises correctly (`ab`, `a@b`, `a` accept;
`a@`, `@` reject). Full suites green: engine 169/169, `@tabnas/bnf` 122 pass /
5 skipped.

**Tree-builder generalisation (§6.4) — now implemented.** `@node$`/`@capture$`/
`@bubble$` ship in the engine builtins; the converter's `builtins` option routes
*all* tree closures through them, so a full grammar serialises as pure data and
round-trips to identical trees (verified greet / pair / arith / probe).
`bnfCompile` gained `recognition:false` and the CLI a `--full` flag;
`toPureSpec` emits the full AST grammar.

**`m`-mark user actions (§3) — now implemented.** `bnfConvert(src,{marks:true})`
stamps user-rule alts; `tn.bnf(src,{actions})` binds `@<rule>:o|c:<mark>` (alt)
and `@<rule>:bo|ao|bc|ac` (phase) user functions, composed after the compiler's
own action (`attachActions`, with `BnfActionError` validation). CLI `--marks`
lists them. Works in **both** closure and `builtins` (pure-data) mode via the
engine's array-`a` composition; `attachActionSlots` injects load-time slots so a
serialized grammar can carry user-action hooks bound by name at load.

**`eager$` fidelity (§6.5.3) — fixed.** A sibling sentinel `@~/source/flags`
carries the matcher's `eager$` flag through serialisation; `resolveFuncRefs`
reconstructs it. Verified round-trip.

A full engineer's log — contracts, gotchas, and a productionising checklist for
`@tabnas/parser` — is in [`implementation-diary.md`](./implementation-diary.md).

## 7. Prior art

- **ANTLR** per-alternative labels (`expr # AddExpr`) generate a visitor method
  per alt — the closest analogue. ANTLR pays for stability by **requiring** the
  label in the grammar; this proposal keeps the grammar pristine and pays
  instead with mark *derivation* + a lookup tool.
- **Lark** `-> alias` names an alternative; a `Transformer` method supplies the
  code — decoupled binding by name.
- **Ohm / Raku grammars** keep all semantics in a separate object/actions class
  keyed by rule name — pure grammar, code referenced by name. The `fnref`
  `@<rule>:<phase>` convention is tabnas's native version of this.
- **Bison** named references (`$[name]`) and **jsonic**'s `resolveFuncRefs` are
  the value-level precedents.

## 8. Non-goals

- Inlining executable code into ABNF text. Bindings stay in the `ref` map; the
  ABNF source remains valid RFC 5234.
- Addressing synthetic / compiler-internal rules.
- Stable marks for rules that the compiler structurally rewrites
  (left-recursion, probe dispatch) — those are look-up-only by design.

## 9. Open questions

1. **Ordering of the node-init action.** Should `/prepend` ever be allowed to
   run before the compiler's node-allocation action, or is that always pinned
   first? (Running a user action before the node exists is likely a footgun.)
2. **Mark stability contract.** What exactly do we promise across compiler
   versions for discriminator-based marks vs look-up-only marks?
3. **Collision policy.** When a discriminator is not unique and cannot be
   extended cleanly, do we fall back to an ordinal suffix (`NR#1`) or error?
4. **Engine vs compiler split.** The `m` field + wrapper composition + `:`-ref
   parsing belong in `@tabnas/parser`; mark derivation + CLI `--marks` belong in
   `@tabnas/bnf`. Confirm the seam and whether `:`-separated refs should be the
   engine's canonical form (with `-` kept as a deprecated alias).
</content>
</invoke>
