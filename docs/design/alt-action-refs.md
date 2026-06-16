# Design: Referencing alt actions from ABNF via `@`-name refs

| | |
|---|---|
| **Status** | Draft / discussion |
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

## 6. Prior art

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

## 7. Non-goals

- Inlining executable code into ABNF text. Bindings stay in the `ref` map; the
  ABNF source remains valid RFC 5234.
- Addressing synthetic / compiler-internal rules.
- Stable marks for rules that the compiler structurally rewrites
  (left-recursion, probe dispatch) — those are look-up-only by design.

## 8. Open questions

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
