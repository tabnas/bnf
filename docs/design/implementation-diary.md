# Implementation Diary — `$`-builtins & pure-data compilation

Working notes for the prototype built in `@tabnas/bnf` against a locally
cloned/built `@tabnas/parser`. Intended as the basis for the **production
update to `@tabnas/parser`** (and the matching `@tabnas/bnf` release). Design
rationale lives in [`alt-action-refs.md`](./alt-action-refs.md); this file is
the engineer's log: contracts, exact behaviours, gotchas, and a productionising
checklist.

The engine diff is captured verbatim in
[`engine-prototype.patch`](./engine-prototype.patch)
(`git apply` inside a `@tabnas/parser` checkout).

---

## 1. Goal reached

Any ABNF grammar now compiles to a **function-free, JSON/jsonic-serializable**
tabnas `GrammarSpec`. Two output flavours:

- **Recognition** (`bnfCompile(src)`): tree building dropped; structure +
  control (`@probe…$`) only.
- **Full AST** (`bnfCompile(src, {recognition:false})` / CLI `--full`): tree
  builders retained as `@node$`/`@capture$`/`@bubble$`; rebuilds the identical
  `{rule,src,kids}` tree.

Verified: full-mode pure-data output, reloaded on a bare engine, produces trees
**deep-equal** to the live plugin for `greet`, `pair`, `arith`, and a probe
grammar. `bnfConvert(src,{builtins:true}).ref` is `{}` for all — zero closures
escape to the wire.

---

## 2. Engine contract (the part to productionise in `@tabnas/parser`)

### 2.1 `$`-builtin resolution (the whole hook)

`grammar()` merges a standard builtin ref map **under** the spec's own refs,
then uses the merged map for both options resolution and per-rule `fnref`:

```ts
const ref = Object.assign(Object.create(null), BUILTIN_REFS, gs.ref || {})
// resolveFuncRefs(gs.options, ref); and per rule: rs.fnref(ref)
```

Why this is sufficient: alt `a:`/`c:` string refs are resolved in `normalt` via
`r.def.fnref[val]` (rules.ts), and `grammar()` calls `fnref(ref)` *before*
`open`/`close`. `isfnref` matches any `@`-prefixed string, so `@probeInit$`,
`@node$`, … resolve with no other change. `fnref`'s reserved-handler scan keys
on `@<rule>-<phase>`, which `$`-names never match, so nothing auto-installs.

### 2.2 The builtin library (`src/builtins.ts`)

Stateless, generic functions; all grammar-specific data arrives as **config**.

| Ref | Field | Config (read from) | Behaviour |
|---|---|---|---|
| `@node$` | `a` | `alt.k.node$ = {init?,rule?,kind?,nterms?}` | if `init`, `r.node = mkNode(rule,kind)`; then `r.node.src += r.o[i].src` for `i<nterms` |
| `@capture$` | `a` | `alt.k.capture$ = {rule?,kind?}` | merge `r.child.node` into `r.node` (tagged→push to `kids`; untagged→flatten src+kids) |
| `@bubble$` | `a` | — | `r.node = r.child.node` (lift, no merge) |
| `@probeInit$` | `a` | — | `r.k.pd_phase=0; r.k.pd_mark=ctx.mark()` |
| `@probeDecide$` | `a` | `r.k.pd_d` (token name) | peek `ctx.t[0]`; `ctx.rewind(r.k.pd_mark)`; `r.k.pd_phase = peek?.name===pd_d ? 1 : 2` |
| `@probePhase0$/1$/2$` | `c` | — | `r.k.pd_phase` equals 0/1/2 |

`mkNode(rule,kind)` = `kind==='user' ? {rule,src:'',kids:[]} : {src:'',kids:[]}`.
This **must** stay byte-identical to `@tabnas/bnf`'s `mkAstNode` — it is the
AST-shape contract between compiler and engine (see §4).

### 2.3 Call conventions relied upon (verified in rules.ts `process`)

- `AltAction = (rule, ctx, alt) => any`; the **normalized `alt` carries `.k`**
  (set in `normalt`), readable at action time.
- Per matched alt, ordering is: merge `alt.k`→`rule.k` (550) → move consumed
  tokens to `ctx.v` (so `ctx.rewind` in the action sees them) → run `alt.a`
  (592). So `@probeDecide$`'s rewind is safe.
- `k` is **propagated** to children via push/replace (`next.k={...rule.k}`).
  Tree builtins read per-alt config from the **3rd arg** `alt.k` (not `r.k`) to
  avoid relying on propagation and to avoid polluting children. The probe uses
  `r.k` deliberately (cross-phase: `pd_d`/`pd_mark`/`pd_phase` set in one
  alt/phase, read in another).
- `c:` conditions also get `(rule,ctx,alt)`, **but** at condition-eval time the
  normalized `alt.k` is not guaranteed populated — so the phase guards are three
  fixed nullary-ish builtins (`@probePhase0$/1$/2$`) rather than one
  config-parameterised condition. (Counter form `c:{n:{…}}` is the declarative
  alternative; not pursued — see §5.)

---

## 3. Compiler side (`@tabnas/bnf`)

- `BnfConvertOptions.builtins`: when true, the emitter routes **all** action
  emission through `RefRegistry.{node,capture,bubble}` and `emitProbeDispatch`'s
  builtin branch, producing `@…$` refs + `k` config instead of registered
  closures. Default (false) is byte-for-byte the old closure output (the live
  plugin install path is unchanged; full suite still green).
- `RefRegistry` gained `useBuiltins` + `node()/capture()/bubble()` returning the
  alt-spec fields to spread (`{a}` or `{a,k}`). This is the single chokepoint —
  every tree closure site (`segmentToAlt`, FIRST-peek, dispatcher init, empty-
  alt fallback, `captureChildFields`, chain captures, `__start__` bubble, probe
  bubble) was converted to call it.
- `compile.ts`:
  - `cloneData` (RegExp-preserving, function-dropping) + `cloneRecognition`
    (also drops `a/bo/bc` that are dropped refs/tree builtins, and the orphaned
    `k.node$`/`k.capture$`).
  - `toRecognitionSpec` (drop tree, keep probe; refuse closure-mode probe),
    `toPureSpec` (keep all builtins; refuse if any `spec.ref` closure remains).
  - `toJsonic` (relaxed default; `strict:true` = valid JSON; `RegExp →
    @/source/flags`).
  - `bnfCompile({recognition=true})`.
- CLI: `--compile` (recognition), `--full` (full AST). Both pure data.

---

## 4. Gotchas / invariants (do not regress)

1. **AST-shape contract.** `@node$`/`@capture$`/`@bubble$` + `mkNode` must mirror
   `mkAstNode`/`segmentToAlt`/`captureChildFields` exactly. The tree-equality
   test (`compile.test.js` "full pure-data AST mode") is the guard — keep it.
2. **`eager$` fidelity gap.** Match-token RegExps carry `.eager$=true`. `@/…/`
   round-trips only source+flags, so `eager$` is lost on reload. It did not
   affect recognition/trees in tested grammars, but is a latent bug for grammars
   where eager lexing matters. Fix options: (a) engine convention to set
   `eager$` from a flag/marker; (b) object encoding for match tokens. **Open.**
3. **`$` namespace is reserved.** Trailing `$` in a *ref* name = builtin. User
   refs must not contain `$`. Distinct from synthetic *rule* names (`<h>$stepN`)
   — different namespace. The compiler should reject user `$` refs once the
   user-action (`@<rule>:<phase>:<mark>`) feature lands.
4. **Config schema is a public contract.** `k.node$`, `k.capture$`, `r.k.pd_d`,
   `r.k.pd_phase`/`pd_mark` shapes are shared between compiler output and engine
   builtins. Version them (e.g. a `v` field, or `@probe$2`) if changed.
5. **`grammar()` now always merges builtins + always calls `fnref(ref)`**
   (previously only when `gs.ref`). Harmless (builtins never trigger reserved
   handlers) but is a behaviour change to note in the engine changelog.
6. **`k` propagation pollutes `r.k`.** Tree config keys land in `r.k` and
   propagate to children. Harmless today (namespaced, read via `alt.k`), but if
   a future builtin reads `r.k.node$` it would see an ancestor's config. Prefer
   `alt.k` for per-alt config; reserve `r.k` for deliberately cross-phase state.

---

## 5. Productionising checklist (`@tabnas/parser`)

- [ ] Land `src/builtins.ts` + the `grammar()` merge (apply
      `engine-prototype.patch`). Decide final home/name (`builtins.ts` vs
      folding into `defaults.ts`/`utility.ts`).
- [ ] Type the builtins properly (drop the `any` on `ctx`/`alt`; add a
      `BuiltinRef` type and a `BUILTIN_REFS` export to the public surface if
      plugins should extend it).
- [ ] Decide whether `$`-builtins are always-on or opt-in via a setting.
- [ ] Engine tests: load a hand-written function-free spec using each builtin;
      assert trees. Don't depend on `@tabnas/bnf` (keep the engine self-tested).
- [ ] Resolve the `eager$` fidelity gap (§4.2) — pick (a) or (b), add a test.
- [ ] Reserve `$` in user-supplied refs (validate in `fnref`/`normalt`); error
      clearly on collision.
- [ ] Consider declarative phase guards (`c:{n:{pd_phase:N}}`) to shrink the
      condition builtins to zero — requires `@probeDecide$`/`@probeInit$` to use
      `n` counters instead of `r.k.pd_phase`. Verify counter semantics
      (`$eq` etc., see `COND_OPS` in rules.ts).
- [ ] Versioning policy for the config schema (§4.4).
- [ ] Changelog: note the `grammar()` behaviour change (§4.5).

## 6. Still design-only (not in this prototype)

- The `m`-mark **user action** wiring (`@<rule>:<phase>:<mark>`,
  alt-action-refs.md §3) — depends on the same `$`/`:` ref conventions but adds
  user-supplied functions + the wrapper-composition for multiple actions per
  alt.
- `@/…/` `eager$` fidelity (§4.2).

---

## 7. Verification log

- `greet`/`pair`/`arith`/`probe`: full-mode pure-data tree `deepStrictEqual`
  live-plugin tree. ✓
- `bnfConvert(src,{builtins:true}).ref === {}` for all. ✓
- Recognition mode drops `@node$/@capture$/@bubble$` and orphaned `k` config;
  still accepts/rejects correctly. ✓
- Probe round-trip (reparse→`resolveFuncRefs`→bare engine) recognises
  `ab/a@b/a`, rejects `a@/@`. ✓
- Suites: `@tabnas/parser` 169/169; `@tabnas/bnf` 132 pass / 5 skipped / 0 fail
  (`compile.test.js` 19/19). Node 22 locally (engines want ≥24; CI uses 24).
