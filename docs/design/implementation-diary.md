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

## 0. Production status (2026-06-17)

The engine half **landed in `@tabnas/parser`** (production-hardened, not the
verbatim patch): typed `BuiltinRef`, frozen `BUILTIN_REFS`, a
`BUILTIN_SCHEMA_VERSION` + load-time version gate, `$`-namespace reservation,
the array-`a` composition, and the `@~/` eager sentinel — with an engine-owned
`builtins.test.js` (probe + eager exercised end-to-end via captured pure-data
fixtures, so the engine is self-tested without `@tabnas/bnf`). The compiler now
**emits `v:1`** in both recognition and full pure-data output and **reserves
`$`** in user action refs (`attachActions`/`attachActionSlots`).

Still open: the Go-port parity (committed fast-follow), a *shared* AST-shape
conformance fixture consumed by both repos, and the deferred items below
(declarative phase guards, strict collision mode, serialized rule-phase slots).

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

The full engine prototype is in `engine-prototype.patch` (5 files: `builtins.ts`,
`tabnas.ts`, `utility.ts`, `rules.ts`, `types.ts`).

- [ ] Land `src/builtins.ts` + the `grammar()` merge (apply
      `engine-prototype.patch`). Decide final home/name (`builtins.ts` vs
      folding into `defaults.ts`/`utility.ts`).
- [ ] Type the builtins properly (drop the `any` on `ctx`/`alt`; add a
      `BuiltinRef` type and a `BUILTIN_REFS` export to the public surface if
      plugins should extend it).
- [ ] Decide whether `$`-builtins are always-on or opt-in via a setting.
- [ ] Engine tests: load a hand-written function-free spec using each builtin;
      assert trees. Don't depend on `@tabnas/bnf` (keep the engine self-tested).
- [x] **Array-`a` composition** (§8): `resolveFunctionRef` resolves an `a` array
      into one ordered call; `AltSpec.a`/`GrammarAltSpec.a` typed. Prototyped +
      tested. (Production: review error-token short-circuit semantics; decide if
      `c`/other fields should ever accept arrays — currently `a`-only.)
- [x] **`eager$` fidelity** (§7): `@~/src/flags` sentinel in `resolveFuncRefs`.
- [ ] Reserve `$` in user-supplied refs (validate in `fnref`/`normalt`); error
      clearly on collision.
- [ ] Consider declarative phase guards (`c:{n:{pd_phase:N}}`) to shrink the
      condition builtins to zero — requires `@probeDecide$`/`@probeInit$` to use
      `n` counters instead of `r.k.pd_phase`. Verify counter semantics
      (`$eq` etc., see `COND_OPS` in rules.ts).
- [ ] Versioning policy for the config schema (§4.4).
- [ ] Changelog: note the `grammar()` behaviour change (§4.5).

## 6. `m`-mark user actions (now prototyped)

Pure ABNF + an out-of-band `actions` map; bindings keyed by `:`.

- **Marks.** `bnfConvert(src, {marks:true})` stamps each *user-rule* alt with a
  stable `m` = its leading discriminator (first token name sans `#`, the pushed
  rule name, or `_`; `~N` suffix on collision), via `assignMarks`. Same source
  alt → same mark, so FIRST-peek/dispatch fan-out copies share it. **Opt-in** —
  default conversion is byte-identical (existing structural tests untouched).
  Engine ignores the extra `m` field. CLI `--marks` / `markListing(spec)` lists
  them (discoverability, design §5).
- **Binding** (`attachActions(spec, actions)`, closure-mode spec):
  - `@<rule>:o|c:<mark>` → wrap the matching open/close alts' `a`. The compiler's
    own action runs **first**, then user actions in array order
    (`composeActions` — the synthetic wrapper). Implemented by registering a new
    `@bnf_userN` ref wrapping the previous one.
  - `@<rule>:bo|ao|bc|ac` → set `spec.ref['@<rule>-<phase>']`, reusing the
    engine's existing `fnref` auto-install. `fnref` builds the reserved key from
    the *known rule name* (`@${rn}-bo`), so a hyphenated ABNF rule name is **not**
    ambiguous (the parse-the-name problem only bites a generic parser, which
    `fnref` is not). This is why no engine change was needed for rule-phase.
  - **Validation**: throws `BnfActionError` on a ref matching no rule / no marked
    alt / bad selector.
- **Plugin**: `tn.bnf(src, {actions})` converts in closure mode with
  `marks:true`, then `attachActions`, then installs.
- **Originally closure-mode only** (the first cut wrapped the previous action
  *function*, which a `$`-builtin string doesn't provide). **§8 lifts this** via
  engine array-`a`, so `attachActions` now also works in `builtins`/pure-data
  mode and the wrapper is no longer needed for alt actions.

Worked demo (verified): `op = "inc" / "dec"` with
`{'@op:o:INC':(r)=>{r.node.delta=1}, '@op:o:DEC':(r)=>{r.node.delta=-1}}` →
`parse('inc').delta===1`, `parse('dec').delta===-1`; `@g:bo` fires on enter;
multiple actions run in order; bad refs throw.

## 7. `@/…/` `eager$` fidelity — fixed

Match-token RegExps carry `.eager$` (opts out of lexer tcol gating; see
`lexer.ts`). `@/src/flags` lost it. Fix = a sibling sentinel **`@~/src/flags`**:

- Engine `resolveFuncRefs` (utility.ts): a new branch matches `@~/(.*)/(\w*)`,
  builds the RegExp and sets `re.eager$ = true`. Placed before the funcref
  lookup; non-eager `@/…/` unchanged.
- `@tabnas/bnf` `toJsonic`: emits `(re as any).eager$ ? '@~/…' : '@/…'`.

Verified: a reloaded `#HI` match token is a `RegExp` with `eager$===true` and
still recognises. **Note:** `tsc --build` is incremental and *missed* the
`utility.ts` edit on first run — had to `tsc --build src --force`. Force/clean
the engine after editing it.

## 8. Composing user actions with `$`-builtins (now done)

The last engine item: user actions on **pure-data / builtins-mode** grammars,
where the alt's `a` is a `$`-builtin string with no compiler-side function to
wrap. Solved with **array-`a`** in the engine — `a` may be a list of
refs/functions, run in order.

- **Engine** (`rules.ts` `resolveFunctionRef`, gated to `k === 'a'`): when `a`
  is an array, resolve each element (string→fn via `fnref`, or pass a function
  through) and replace it with one `composedAction` that calls each in order,
  short-circuiting on an error-token return. `process()` is unchanged (it still
  calls a single `alt.a`). Types: `AltSpec.a` / `GrammarAltSpec.a` now accept
  `(AltAction|FuncRef)[]`. The composition is resolved once at `normalt` time —
  no per-parse overhead beyond the loop.
- **Compiler** (`compile.ts`):
  - `attachActions` no longer wraps closures; it injects `alt.a =
    appendAction(alt.a, '@bnf_userN')` (array-`a`), with the user fn(s) in
    `spec.ref`. The alt's own action (closure **or** `$`-builtin) runs first.
    Works in **both** modes now — the builtins-mode restriction is gone.
  - `attachActionSlots(spec, refNames)`: injects `@<rule>:o|c:<mark>` ref *names*
    into array-`a` **without** functions, for the serialized path. The grammar
    stays pure data (`toPureSpec` passes — slot names are strings); the consumer
    binds them at load via `gs.ref`.

Verified: (A) live builtins-mode — `op="inc"/"dec"` with `builtins:true` +
`attachActions({'@op:o:INC':r=>{r.node.delta=1}})` → `parse('inc')` has
`rule:'op'` (tree via `@node$`) **and** `delta:1`. (B) serialized — same grammar
+ `attachActionSlots(['@op:o:INC'])` → `toPureSpec`/`toJsonic` (no `@bnf_`),
reload with `gs.ref={'@op:o:INC':r=>{r.node.delta=7}}` → `delta:7`, tree intact.
(C) hand-written `a:['@one','@two']` runs in order.

## 9. Still design-only

- Reserving `$` in user refs and accepting `:` directly in the engine `fnref`
  reserved scan (the compiler currently translates `:`→`-` for rule phases;
  unambiguous because `fnref` builds the key from the known rule name).
- Serialized **rule-phase** slots (bo/ao/bc/ac bound at load). Alt-action slots
  work; rule-phase still relies on `fnref` with functions in `gs.ref` keyed
  `@<rule>-<phase>`, which is serializable-by-name but untested here.

---

## 10. Verification log

- `greet`/`pair`/`arith`/`probe`: full-mode pure-data tree `deepStrictEqual`
  live-plugin tree. ✓
- `bnfConvert(src,{builtins:true}).ref === {}` for all. ✓
- Recognition mode drops `@node$/@capture$/@bubble$` and orphaned `k` config;
  still accepts/rejects correctly. ✓
- Probe round-trip (reparse→`resolveFuncRefs`→bare engine) recognises
  `ab/a@b/a`, rejects `a@/@`. ✓
- Eager match token round-trips: reloaded `#HI` is a `RegExp` with
  `eager$===true`; still recognises. ✓
- User actions: `@op:o:INC/DEC` set `node.delta` (compiler action first);
  multiple actions ordered; `@g:bo` fires on enter; bad refs throw
  `BnfActionError`. ✓ Marks are opt-in (default spec unchanged). ✓
- Builtins-mode composition: live `attachActions` over `@node$` → tree + user
  action; serialized `attachActionSlots` → `toPureSpec` (no closures) → reload +
  bind → tree + user action; array-`a` runs in order. ✓
- Suites: `@tabnas/parser` 169/169; `@tabnas/bnf` 142 pass / 5 skipped / 0 fail
  (`compile.test.js` 29/29). Node 22 locally (engines want ≥24; CI uses 24).
