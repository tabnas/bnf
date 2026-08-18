/* Copyright (c) 2025-2026 Richard Rodger and other contributors, MIT License */

/*  compiler.ts
 *  The notation-neutral half of a BNF-family grammar compiler: it turns
 *  a parsed grammar (the IR below) into a tabnas `GrammarSpec`.
 *
 *  Nothing here knows what notation the grammar was WRITTEN in. A
 *  front-end — `@tabnas/abnf`, `@tabnas/gbnf`, `@tabnas/ebnf` — parses
 *  its own syntax into `Grammar` and hands it to `emitGrammarSpec`; the
 *  passes below (desugaring, left-recursion elimination, tail-repeat
 *  rewriting, probe dispatch for unbounded lookahead, literal lifting,
 *  token allocation, first-set analysis, chain emission) are shared.
 *
 *  This code was extracted from `@tabnas/abnf`'s converter, where it had
 *  grown up alongside the ABNF parser. The ABNF-specific half — the
 *  notation grammar, core rules, `=/` incrementals, `%d`/`%x`/`%b`
 *  numeric values and prose terminals — stayed there.
 *
 *  The emitter turns each alternative into one or more tabnas rule
 *  alts. A "single-segment" alternative (at most one rule reference,
 *  trailing) collapses to a single tabnas alt; any alternative with
 *  two or more ref boundaries is chained through synthetic
 *  continuation rules named `<prodname>$stepN`.
 */

import type { GrammarSpec, Rule } from '@tabnas/parser'
import { util as engineUtil } from '@tabnas/parser'


// ABNF converter options.
export type ConvertOptions = {
  start?: string
  tag?: string
  // Emit the probe/phase-retry dispatcher using engine `$`-builtin
  // refs (`@probeInit$` / `@probeDecide$` / `@probePhaseN$`) + `k`
  // config, instead of registered closures. This keeps the dispatcher
  // function-free so it survives compilation (pure-recognition) mode.
  // Requires an engine that ships the probe `$`-builtins.
  builtins?: boolean
  // Emit a stable `m` (mark) on each user-rule alt, enabling
  // `@<rule>:o|c:<mark>` user-action references. Off by default so the
  // emitted spec shape is unchanged unless marks are wanted.
  marks?: boolean
  // Treat word-like string literals as whole-word keywords: a literal
  // ending in `[A-Za-z0-9_]` only matches when not immediately followed
  // by another word character. Without this, `"option"` would grab the
  // `option` prefix of an identifier `optional`. Use for tokenised,
  // keyword-rich languages (proto, SQL, …) that also use the whole-word
  // `TX` token; leave off for scannerless/char-level grammars (a literal
  // `"v"` before a hex digit in an RFC grammar must still match). Off by
  // default so existing grammars are unchanged.
  wordKeywords?: boolean
  // Emit `meta.provenance` — the map from each generated rule name back
  // to the author-written production it came from (see
  // `Production.origin`). On by default: the names are otherwise
  // unattributable, and every tool that shows a rule name to a human
  // needs it. Set `false` to keep an embedded grammar as small as
  // possible.
  provenance?: boolean
}


type Element =
  | {
      kind: 'term';
      literal: string;
      // ABNF quoted strings are case-insensitive by default; `%s"…"`
      // sets this flag to true. Omitting the flag preserves the
      // RFC 5234 default so the emitter can lower to a case-fold
      // match. (Has no effect when the literal contains no ASCII
      // letters — those match exactly either way.)
      caseSensitive?: boolean;
      // Preferred lexer token name, set by `liftLiteralTokens` when this
      // terminal came from a production that names it (`PL = "+"` →
      // `#PL`). Without it the emitter derives a name from the literal
      // text, which for punctuation degrades to `#T`, `#T1`, …
      tokenName?: string;
    }
  | {
      kind: 'ref';
      name: string;
      // Suffix-debt counter mutations to emit on the tabnas alt that
      // pushes this reference (`n: { <counter>: 1 | 0 }`). Written by
      // `resolveSuffixDebts`; see that pass for what the counter means.
      // Absent on every reference in a grammar with no contested tail
      // loop, which is all of them until one is detected.
      debt?: Record<string, number>;
    }
  // A terminal that matches a built-in engine lexer token directly (e.g.
  // `#TX`, `#NR`, `#ST`, `#VL`). Produced by normalising a bareword ref
  // whose name is in BUILTIN_TOKENS and isn't a defined rule — letting a
  // grammar reference the lexer's whole-word tokens (`ident = TX`) instead
  // of re-deriving them char-by-char. `name` is the full token name incl. `#`.
  | { kind: 'token'; name: string }
  // RFC 5234 `prose-val` — `<free text>`. Prose is *informational*: it
  // describes a terminal in English rather than defining one. The
  // converter accepts it only as the entire body of a production whose
  // name is a built-in lexer token (`NR = <number>`), where it documents
  // the token the lexer already provides; `resolveProseTerminals` then
  // drops the production so refs resolve to that built-in. Anywhere else
  // there is nothing to compile, and it is an error.
  | { kind: 'prose'; text: string }
  | { kind: 'regex'; pattern: string; flags: string }  // internal: for future %x
  | { kind: 'opt'; inner: Element }     // [ A ]
  | {
      kind: 'star';                     // *A
      inner: Element;
      // Name of the suffix-debt counter guarding this repetition, set by
      // `eliminateDirectLeftRec` on the tail loop it generates.
      // `desugar` carries it onto the helper production the star becomes;
      // `resolveSuffixDebts` then either confirms or drops it.
      debtGuard?: string;
    }
  | { kind: 'plus'; inner: Element }    // 1*A
  | { kind: 'rep'; min: number; max: number; inner: Element } // m*nA
  | { kind: 'group'; alts: Sequence[] } // ( A / B )

type Sequence = Element[]

type Production = {
  name: string
  alts: Sequence[]
  // ABNF `name =/ alt` — flagged during parse; a post-parse merge
  // step folds each incremental production's alts into the earlier
  // production with the same name. The flag is gone by the time
  // the AST reaches the emitter.
  incremental?: boolean
  // Set by `rewriteTailRepeats` on a production of the shape
  // `X = prefix [ sep X ]` (all-terminal prefix and separator, self-ref
  // last). The opt is removed from `alts` (leaving just the prefix) and
  // the separator elements are stashed here; the emitter compiles the
  // production to a same-depth close-phase repeat (`r: X`) instead of
  // the opt→group→push helper chain, so every iteration shares one
  // parent and the tree comes out flat.
  tailRepeat?: { sep: Sequence }
  // Set by `desugar` on the generated helpers that terminate a
  // repetition (`opt`/`star` and the tails of `plus`/`rep`). Their
  // terminating alternative is empty, so it names no token — and the
  // engine only offers a matcher at a position where the active rule
  // names it. The emitter therefore guards that alternative with a
  // FOLLOW-set peek, without which a repetition followed by a
  // character class cannot terminate. See computeFollowSets.
  repeatHelper?: boolean
  // Set by `desugar` on the star helper generated for a left-recursion
  // tail loop whose greediness contests a suffix of the rule it was
  // derived from, and confirmed by `resolveSuffixDebts`. Names the
  // counter whose value must be zero for the loop to keep going.
  debtGuard?: string
  // The loop's own FIRST tokens that an enclosing suffix can actually
  // compete for, set by `resolveSuffixDebts` alongside `debtGuard`. Only
  // the branches that could eat one of these are guarded: a multi-tail
  // loop (`A = A "y" / A "w" / "x" A "y" / "z"`) owes a `"y"` and
  // nothing else, so blocking its `"w"` branch as well would reject
  // `xzwy`.
  debtOwed?: string[]
  // Set on synthetic productions introduced by the probe-dispatch
  // rewriter. The emitter emits a phase-retry rule body instead of
  // compiling `alts` through the normal path.
  probeDispatch?: ProbeDispatchSpec
  // Set on synthetic probe-helper productions (`*(vocab)` style,
  // failure-proof). The emitter emits a one-rule loop over the vocab
  // token set with an empty-alt fallback. Vocab is stored as a list
  // of AbnfElements (term / regex) — resolved to token names at emit
  // time, after token allocation.
  probeHelper?: { vocabElements: Element[] }
  // How this rule contributes to the output AST:
  //   - 'user': emit a tagged node `{ rule, src, kids }`.
  //   - 'core': the RFC 5234 Appendix B.1 rules — flatten into the
  //     enclosing rule's `src` (char-class terminals shouldn't clutter
  //     the tree with one node per matched character).
  //   - 'helper': synthetic sugar / dispatcher / chain rules — also
  //     flatten. Their `src` and `kids` roll up into the enclosing
  //     user rule transparently.
  // Unset is treated as 'user' (default for freshly parsed productions).
  nodeKind?: 'user' | 'core' | 'helper'
  // The author-written production this one descends from. Set by every
  // pass that SYNTHESISES a production (desugar's sugar helpers, left
  // factoring's `$fact` tails, the probe rewriter's dispatch branches)
  // to the origin of the production being rewritten. ABSENT means the
  // production is itself author-written — so the source rule is always
  // `origin ?? name`, which is what `originOf` returns.
  //
  // A compiled grammar carries an order of magnitude more rules than the
  // author wrote (a 12-production ABNF grammar emits 118), and every one
  // of the extra names surfaces in rule stacks, hover and completion.
  // Carrying the origin is what lets `emitGrammarSpec` export the map
  // back out (`spec.meta.provenance`) so a tool can name the user's rule
  // instead of the machinery's.
  origin?: string
}


// The author-written production a (possibly synthesised) production
// descends from. Synthetic productions carry `origin`; an author-written
// one is its own origin. Always read `origin` through this — a bare
// `p.origin` is undefined for exactly the productions whose name is
// already the answer.
function originOf(prod: Production): string {
  return prod.origin ?? prod.name
}

// Configuration attached to a synthesised dispatcher production. The
// dispatcher is the replacement for an ambiguous `[X D] Y` subsequence
// in a user rule: on phase 0 it pushes `probeRule` (a failure-proof
// *vocab loop), peeks `ctx.t[0]` on return, rewinds to the mark, and
// sets `r.k.phase` to 1 (saw D → push `withBranch`) or 2 (didn't →
// push `noBranch`). `r:` retries the dispatcher so the committed
// branch is taken on the next pass.
//
// The disambiguator is stored as an Element (term or regex) rather
// than a token name so the rewriter doesn't need token allocation to
// have happened. `emitProbeDispatch` resolves it to a name at emit
// time from the literals / regex maps.
type ProbeDispatchSpec = {
  probeRule: string
  disambiguator: Element
  withBranch: string
  noBranch: string
}

type Grammar = {
  productions: Production[]
  // Diagnostics from the probe-dispatch analyser: one entry per
  // ambiguous `[X D] Y` pattern detected, populated whether or not
  // the rewrite was actually applied.
  ambiguities?: AmbiguityReport[]
  // `<remove>` directives. `remove` names rules/tokens to drop;
  // `clearAll` is `* = <remove>`, which wipes the instance first.
  remove?: string[]
  clearAll?: boolean
}

type AmbiguityReport = {
  rule: string
  altIdx: number
  optIdx: number
  reason: string
  resolved: boolean
}


// Lazily built tabnas instance that parses ABNF source. Deferred
// construction avoids a circular-import failure at module load time.


// Rewrite a grammar so that the only element kinds remaining are
// `term` and `ref`. Each `X?`, `X*`, `X+` occurrence is replaced by a
// reference to a newly-generated helper production that expresses the
// same language in plain ABNF.
// Eliminate left recursion — both direct (P → P α) and indirect
// (P → Q α, Q → P β) — via Paull's algorithm.
//
// Order the productions, and for each A_i walk back over A_1..A_{i-1}
// inlining any leading reference into A_i's alternatives. Once the
// only remaining leading self-reference on A_i is direct, rewrite to
// the iterative form
//   P → (β_1 | … | β_m) (α_1 | … | α_n)*
// which tabnas's push-down parser can execute without re-entering P
// at the same source position.
//
// The substitution step can duplicate alternatives, so pathological
// grammars will enlarge — caller is expected to keep the grammar
// reasonably small (this is a first-step converter, not a full
// toolchain).
// Sugar that can match nothing: `[X]`, `*X`, and `m*nX` with m = 0.
// Returns the element sequence for the branch where the sugar *does*
// match at least once, or null if the element is not nullable sugar.
//
// This is deliberately narrow. A nullable *reference* (`A = b A`, `b =
// "y" /`) is already handled by Paull's substitution below; what that
// machinery cannot see is sugar, because it inspects `ref` elements and
// this pass runs before `desugar`.
function nullableSugarPresent(el: Element): Sequence | null {
  if (el.kind === 'opt') return [el.inner]
  if (el.kind === 'star') return [el.inner, el]
  if (el.kind === 'rep' && el.min === 0) {
    if (el.max === 0) return null
    if (el.max === Infinity) return [el.inner, el]
    if (el.max === 1) return [el.inner]
    return [el.inner, { kind: 'rep', min: 0, max: el.max - 1, inner: el.inner }]
  }
  return null
}


// Does this alternative re-enter `name` at the same input position —
// reachable only because everything before the self-reference can match
// nothing? `A = ["x"] A "y"` is the shape: skip the leading nullable
// sugar and the next element is a reference back to A.
// Direct left recursion (`A = A "y"`) is excluded: there is no sugar to
// split, and `eliminateDirectLeftRec` already removes it.
function isHiddenLeftRecursive(alt: Sequence, name: string): boolean {
  let sawSugar = false
  for (const el of alt) {
    if (el.kind === 'ref') return sawSugar && el.name === name
    if (nullableSugarPresent(el) === null) return false
    sawSugar = true
  }
  return false
}


// Split leading nullable sugar into explicit present/absent
// alternatives, so that hidden left recursion becomes *direct* left
// recursion that `eliminateDirectLeftRec` can then remove.
//
//   A = ["x"] A "y" / "z"    becomes    A = "x" A "y" / A "y" / "z"
//
// Without this the recursion survives into the emitted grammar: the
// generated optional helper takes its empty branch and exposes A again
// at the same source position, so the parse loops or rejects strings
// the IR accepts. `eliminateLeftRecursion` runs before `desugar` (it
// has to — substitution works on the authored shape), so it never sees
// the sugar as the nullable thing it is.
//
// Only alternatives that are actually hidden-left-recursive are
// touched, so a grammar that compiles correctly today is unchanged.
function expandNullableLeftPrefixes(prods: Production[]): Production[] {
  return prods.map((p) => {
    let alts = p.alts
    // Each expansion shortens the leading sugar run of the alternative
    // it splits, so this terminates; the guard is belt-and-braces.
    const guard = alts.reduce((n, a) => n + a.length, 0) + 1
    for (let round = 0; round < guard; round++) {
      const idx = alts.findIndex((a) => isHiddenLeftRecursive(a, p.name))
      if (idx < 0) break
      const alt = alts[idx]
      const present = nullableSugarPresent(alt[0]) as Sequence
      alts = [
        ...alts.slice(0, idx),
        [...present, ...alt.slice(1)],
        alt.slice(1),
        ...alts.slice(idx + 1),
      ]
    }
    return alts === p.alts ? p : { ...p, alts }
  })
}


function eliminateLeftRecursion(grammar: Grammar): Grammar {
  const originalOrder = grammar.productions.map((p) => p.name)
  // Suffix-debt counter names handed out across the whole grammar.
  const debtNames = new Set<string>()

  // Order productions so that rules referenced at a leading position
  // are processed before the rules that reference them. Paull's
  // substitution inlines A_j's alts into A_i for j < i, so putting
  // dependencies first is what makes nullable-prefixed hidden left
  // recursion reachable by the substitution step.
  let prods = topoOrderForPaull(
    expandNullableLeftPrefixes(
      grammar.productions.map((p) => ({
        name: p.name,
        alts: p.alts.map((a) => a.slice()),
        nodeKind: p.nodeKind,
        origin: p.origin,
      })),
    ),
  )

  // Substitution normally runs for every production, even a cycle-free
  // one. That is pragmatic rather than theoretical: the multi-token
  // `altPrefixes` used to populate tcol (so the lexer's regex matchers
  // fire with the right tin in nested contexts) are read off the fully
  // inlined shape, and a rule choosing between several alternatives
  // needs that lookahead to dispatch. Scoping substitution to the cyclic
  // SCCs in general therefore has to wait for tcol to be computed from
  // the un-substituted grammar.
  //
  // One case is safe to exempt today, and it is the one that visibly
  // mangles a grammar: a *pure alias*, a production whose single
  // alternative is a single rule reference (`val = add`). Inlining it
  // rewrites `val = add` into `val = NR [ PL add ]` — the alias name is
  // dissolved, so the rule vanishes from the emitted AST and the grammar
  // no longer renders back to the ABNF it was written in. Because such a
  // production has exactly one alternative, it has nothing to dispatch
  // between: it unconditionally pushes its target, needs no lookahead,
  // and so cannot depend on the inlined prefixes. Aliases caught up in a
  // leading-reference cycle are still inlined — that is where Paull's
  // substitution is doing real work (`P = Q`, `Q = P a / b`).
  const cyclic = findLeadingRefCycleMembers(prods)
  const isExemptAlias = (p: Production): boolean =>
    p.alts.length === 1 &&
    p.alts[0].length === 1 &&
    p.alts[0][0].kind === 'ref' &&
    !cyclic.has(p.name) &&
    !cyclic.has((p.alts[0][0] as { name: string }).name)

  for (let i = 0; i < prods.length; i++) {
    // For each earlier production A_j, inline any alternative of
    // A_i whose leading element is a reference to A_j.
    //
    // Paull's invariant is that after this inner loop no alternative
    // of A_i begins with a ref to any A_j, j < i. A single increasing
    // pass gives that only when every A_j has itself been fully
    // substituted — but the pure-alias exemption above deliberately
    // leaves some A_j un-substituted, so inlining such an alias can
    // (re)introduce a leading ref to an A_k with k < j, which the pass
    // has already walked past. Left in place, a nullable A_k hides the
    // left recursion from `eliminateDirectLeftRec` and the emitted
    // grammar re-enters A_i at the same source position
    // (`a = b a / "x"`, `b = c`, `c = "y" /`). So re-run the pass
    // until it reaches a fixed point.
    //
    // Termination: each round only fires where a leading ref to an
    // earlier production remains, and earlier productions have already
    // had their own direct left recursion eliminated, so no
    // substitution can reproduce the ref it just consumed. The guard
    // is belt-and-braces against a pathological grammar.
    if (!isExemptAlias(prods[i])) {
      const guard = prods.length + 1
      for (let round = 0; round < guard; round++) {
        let changed = false
        for (let j = 0; j < i; j++) {
          if (!hasLeadingRefTo(prods[i], prods[j].name)) continue
          prods[i] = substituteLeadingRef(prods[i], prods[j])
          changed = true
        }
        if (!changed) break
      }
    }
    prods[i] = eliminateDirectLeftRec(prods[i], debtNames)
  }

  // Restore the caller's declared order, so the start rule still
  // ends up first (and the user sees their rule names in a
  // recognisable order when inspecting the spec).
  const byName = new Map(prods.map((p) => [p.name, p]))
  const ordered: Production[] = []
  for (const name of originalOrder) {
    const p = byName.get(name)
    if (p) { ordered.push(p); byName.delete(name) }
  }
  // Any generated productions created during substitution (none in
  // the current implementation) would fall through here.
  for (const p of byName.values()) ordered.push(p)

  return { productions: ordered }
}


// Tarjan-flavoured SCC scan over the leading-reference graph:
// returns the names of productions that participate in at least one
// cycle (self-loop or longer). Used to scope Paull's substitution to
// only the rules that actually need it.
function findLeadingRefCycleMembers(prods: Production[]): Set<string> {
  const byName = new Map(prods.map((p) => [p.name, p]))
  const leadingRefs = (p: Production): string[] => {
    const out: string[] = []
    for (const alt of p.alts) {
      if (alt.length === 0) continue
      const first = alt[0]
      if (first.kind === 'ref' && byName.has(first.name)) out.push(first.name)
    }
    return out
  }

  // Tarjan's SCC algorithm.
  let index = 0
  const stack: string[] = []
  const onStack = new Set<string>()
  const indices = new Map<string, number>()
  const lowlinks = new Map<string, number>()
  const cyclic = new Set<string>()

  function strongConnect(name: string) {
    indices.set(name, index)
    lowlinks.set(name, index)
    index++
    stack.push(name)
    onStack.add(name)

    const prod = byName.get(name)
    if (prod) {
      for (const target of leadingRefs(prod)) {
        if (!indices.has(target)) {
          strongConnect(target)
          lowlinks.set(name, Math.min(lowlinks.get(name)!, lowlinks.get(target)!))
        } else if (onStack.has(target)) {
          lowlinks.set(name, Math.min(lowlinks.get(name)!, indices.get(target)!))
        }
      }
    }

    if (lowlinks.get(name) === indices.get(name)) {
      // Pop the SCC. If it has more than one member, or it's a
      // single member with a self-loop, mark as cyclic.
      const scc: string[] = []
      let w: string
      do {
        w = stack.pop() as string
        onStack.delete(w)
        scc.push(w)
      } while (w !== name)
      const isCycle =
        scc.length > 1 ||
        (scc.length === 1 && leadingRefs(byName.get(scc[0])!).includes(scc[0]))
      if (isCycle) for (const n of scc) cyclic.add(n)
    }
  }

  for (const p of prods) {
    if (!indices.has(p.name)) strongConnect(p.name)
  }
  return cyclic
}


// Topological order over the "leading-position reference" graph:
// an edge A → B exists when A has at least one alternative whose
// first element is a reference to B. Cycles are preserved as-is
// (Paull's handles them via the substitution + direct-LR rewrite).
function topoOrderForPaull(prods: Production[]): Production[] {
  const byName = new Map(prods.map((p) => [p.name, p]))
  const colour = new Map<string, number>() // 0 unseen, 1 in-progress, 2 done
  const order: Production[] = []

  function visit(name: string) {
    const c = colour.get(name) ?? 0
    if (c !== 0) return // already seen or on the current path
    colour.set(name, 1)
    const p = byName.get(name)
    if (p) {
      for (const alt of p.alts) {
        if (alt.length > 0 && alt[0].kind === 'ref' && byName.has(alt[0].name)) {
          visit(alt[0].name)
        }
      }
      colour.set(name, 2)
      order.push(p)
    } else {
      colour.set(name, 2)
    }
  }

  for (const p of prods) visit(p.name)
  return order
}


// True when at least one alternative of `prod` begins with a
// reference to `name` — i.e. `substituteLeadingRef` would change it.
function hasLeadingRefTo(prod: Production, name: string): boolean {
  for (const alt of prod.alts) {
    if (alt.length > 0 && alt[0].kind === 'ref' && alt[0].name === name) {
      return true
    }
  }
  return false
}


// For every alternative of `target` that begins with a ref to
// `source`, replace that alt with |source.alts| copies — each one
// with the leading source-ref expanded to one of source's alts.
function substituteLeadingRef(
  target: Production,
  source: Production,
): Production {
  const newAlts: Sequence[] = []
  for (const alt of target.alts) {
    if (
      alt.length > 0 &&
      alt[0].kind === 'ref' &&
      alt[0].name === source.name
    ) {
      const tail = alt.slice(1)
      for (const srcAlt of source.alts) {
        newAlts.push([...srcAlt, ...tail])
      }
    } else {
      newAlts.push(alt)
    }
  }
  return {
    name: target.name,
    alts: newAlts,
    nodeKind: target.nodeKind,
    origin: target.origin,
  }
}


// Allocate a suffix-debt counter name for a production. Counter names
// end up in a declarative condition path (`n.<counter>`), which the
// engine splits on `.`, and in serialised jsonic output, where a bare
// identifier avoids quoting — so reduce the rule name to word
// characters and disambiguate against what has already been handed out.
function freshDebtCounter(ruleName: string, used: Set<string>): string {
  // Unicode-aware, so an astral rule name reduces to one underscore per
  // code point rather than one per UTF-16 surrogate half — the Go port
  // iterates runes, and the two must agree on the name they mint.
  const base = 'debt_' + ruleName.replace(/[^A-Za-z0-9_]/gu, '_')
  let name = base
  let i = 0
  while (used.has(name)) name = base + '_' + (++i)
  used.add(name)
  return name
}


// Does any seed alternative re-enter this rule at all? A seed that does
// is the shape whose inner tail loop can compete with the enclosing
// alternative's suffix.
//
// This is only a cheap pre-filter: it decides whether to allocate a
// counter, not whether one is warranted. `resolveSuffixDebts` does the
// real analysis on the desugared grammar — where a self-reference buried
// in a group has become an ordinary reference in an ordinary production
// — and drops the flag again when nothing turns out to compete. So the
// filter can afford to say yes broadly, and has to: reading only the top
// level of each seed missed `A = A "y" / ( "x" A "y" / "z" )` entirely.
function seedsReferenceSelf(seeds: Sequence[], name: string): boolean {
  const refs = new Set<string>()
  for (const alt of seeds) refsIn(alt, refs)
  return refs.has(name)
}


// Rewrite a single production's direct left recursion to its
// iterative equivalent. Equivalent to the previous version of
// `eliminateLeftRecursion` but scoped to one production.
//
// `debtNames` accumulates the suffix-debt counters allocated so far, so
// two rules whose names reduce to the same word characters still get
// distinct counters.
function eliminateDirectLeftRec(
  prod: Production,
  debtNames: Set<string> = new Set(),
): Production {
  const recursive: Sequence[] = []
  const seeds: Sequence[] = []
  for (const alt of prod.alts) {
    if (
      alt.length > 0 &&
      alt[0].kind === 'ref' &&
      alt[0].name === prod.name
    ) {
      recursive.push(alt.slice(1))
    } else {
      seeds.push(alt)
    }
  }

  // A trivial recursive alt `[P]` (P ::= P, nothing else) would
  // derive P from P with no progress — semantically a no-op. Drop
  // them silently, since nullable-prefix expansion in Paull's can
  // legitimately produce them and erroring would hide a legal
  // grammar.
  const nonTrivialRecursive = recursive.filter((t) => t.length > 0)
  if (nonTrivialRecursive.length === 0) {
    // Either no recursion at all, or only trivial self-refs — keep
    // just the seeds.
    return {
      name: prod.name,
      alts: seeds,
      nodeKind: prod.nodeKind,
      origin: prod.origin,
    }
  }
  if (seeds.length === 0) {
    throw new Error(
      `${diagName()}: rule '${prod.name}' is purely left-recursive ` +
      `(no seed alternative); cannot eliminate`)
  }

  const seedElement: Element =
    seeds.length === 1 && seeds[0].length === 1
      ? seeds[0][0]
      : { kind: 'group', alts: seeds }

  const tailInner: Element =
    nonTrivialRecursive.length === 1 && nonTrivialRecursive[0].length === 1
      ? nonTrivialRecursive[0][0]
      : { kind: 'group', alts: nonTrivialRecursive }

  // The rewrite is correct as a CFG, but it introduces a repetition
  // whose greediness can compete with a suffix of the very alternative
  // it was derived from. `A = ["x"] A "y" / "z"` becomes
  // `A = ( "x" A "y" | "z" ) "y"*`: parsing `xzy` needs the inner A's
  // tail loop to match ZERO `"y"`s so the outer `"y"` has something to
  // consume, and a greedy loop eats it instead. No amount of lookahead
  // can decide that — the repeated token and the follow token are the
  // same token, and whether to continue depends on how many enclosing
  // frames still owe a `"y"`, which is stack depth, not a token window.
  //
  // So count the debt instead. Flag the loop here; `resolveSuffixDebts`
  // confirms the contest against real FIRST sets and wires up the
  // counter, or drops the flag when the suffix and the loop cannot
  // collide (`A = A "w" / "(" A ")" / "z"` — `")"` never contests
  // `"w"`). Issue #6.
  const star: Element = { kind: 'star', inner: tailInner }
  if (seedsReferenceSelf(seeds, prod.name)) {
    star.debtGuard = freshDebtCounter(prod.name, debtNames)
  }

  return {
    name: prod.name,
    alts: [[seedElement, star]],
    nodeKind: prod.nodeKind,
    origin: prod.origin,
  }
}


// Rewrite tail self-references into same-depth repeats.
//
//   X = prefix [ sep X ]
//
// compiles naturally to a rule that repeats itself (`r: X`) from its
// close phase — the form a hand-written tabnas grammar uses — rather
// than to an optional-group helper chain that re-pushes X. The repeat
// keeps every iteration at one stack depth with the SAME parent, which
// is what makes `r.parent.node` usable from user actions, and flattens
// the tree: `1+2+3` yields sibling X kids instead of a right-nested
// chain.
//
// The rewrite is deliberately narrow. It applies only when:
//   - the production has exactly one alternative;
//   - its last element is an option wrapping `sep… X` with the
//     self-reference LAST and at least one separator element;
//   - every prefix and separator element is a terminal (literal,
//     token, or regex — all resolved before this pass runs);
//   - the production is not the start production (the `__start__`
//     wrapper allocates no node for a fold to land in).
// Anything else keeps the existing compilation.
function rewriteTailRepeats(grammar: Grammar, start: string): Grammar {
  const isTerminal = (el: Element) =>
    el.kind === 'term' || el.kind === 'token' || el.kind === 'regex'

  for (const prod of grammar.productions) {
    if (prod.probeDispatch || prod.probeHelper) continue
    if (prod.name === start) continue
    if (prod.alts.length !== 1) continue
    const alt = prod.alts[0]
    if (alt.length < 2) continue
    const last = alt[alt.length - 1]
    if (last.kind !== 'opt') continue

    // Normalize the option body to a sequence: `[ a b ]` parses as
    // opt(group([[a, b]])); `[ a ]` as opt(a).
    let seq: Sequence
    if (last.inner.kind === 'group') {
      if (last.inner.alts.length !== 1) continue
      seq = last.inner.alts[0]
    } else {
      seq = [last.inner]
    }
    if (seq.length < 2) continue // need at least one separator + the self-ref

    const tail = seq[seq.length - 1]
    if (tail.kind !== 'ref' || tail.name !== prod.name) continue
    const sep = seq.slice(0, -1)
    if (!sep.every(isTerminal)) continue

    const prefix = alt.slice(0, -1)
    if (prefix.length === 0 || !prefix.every(isTerminal)) continue

    prod.alts = [prefix]
    prod.tailRepeat = { sep }
  }
  return grammar
}


// How many concrete tokens the multi-alt dispatcher fans each
// alternative's prefix out to (see emitProduction). Left factoring
// uses the same bound to decide which shared prefixes the dispatcher
// can already see past.
const LOOKAHEAD_K = 4

// Structural equality of IR elements, the comparison left factoring
// uses to recognise a shared prefix. Terms compare by their token key
// (literal + effective case-sensitivity — the same identity the token
// allocator uses), refs and built-in tokens by name, and the sugar
// forms recursively. Prose never equals anything: there is nothing to
// factor out of an informational terminal.
function elemEqual(a: Element, b: Element): boolean {
  if (a.kind !== b.kind) return false
  switch (a.kind) {
    case 'term':
      return termKey(a) === termKey(b as typeof a)
    case 'token':
      return a.name === (b as typeof a).name
    // Two references to the same rule are still distinct when they
    // carry different suffix-debt mutations: left factoring would
    // otherwise merge them and one of the two counters would silently
    // stop being maintained. (`debt` is only ever set after factoring
    // has run, so in practice both sides are undefined here.)
    case 'ref':
      return a.name === (b as typeof a).name &&
        debtKey(a.debt) === debtKey((b as typeof a).debt)
    case 'regex':
      return a.pattern === (b as typeof a).pattern &&
        a.flags === (b as typeof a).flags
    case 'prose':
      return false
    // Likewise for a guarded tail loop: same repeated element, but one
    // yields to an enclosing suffix and the other does not.
    case 'star':
      return a.debtGuard === (b as typeof a).debtGuard &&
        elemEqual(a.inner, (b as typeof a).inner)
    case 'opt':
    case 'plus':
      return elemEqual(a.inner, (b as { inner: Element }).inner)
    case 'rep':
      return a.min === (b as typeof a).min &&
        a.max === (b as typeof a).max &&
        elemEqual(a.inner, (b as typeof a).inner)
    case 'group':
      return a.alts.length === (b as typeof a).alts.length &&
        a.alts.every((alt, i) => seqEqual(alt, (b as typeof a).alts[i]))
  }
}

function seqEqual(a: Sequence, b: Sequence): boolean {
  return a.length === b.length && a.every((el, i) => elemEqual(el, b[i]))
}

// Order-independent identity for a reference's suffix-debt mutations,
// for element comparison. Empty and absent are the same thing.
function debtKey(debt: Record<string, number> | undefined): string {
  if (null == debt) return ''
  return Object.keys(debt).sort().map((k) => k + '=' + debt[k]).join(',')
}

// The most tokens a sequence can span, for deciding whether the
// dispatcher's K-token lookahead can see past it. Runs on the raw IR —
// left factoring precedes desugaring, so the sugar kinds are still
// here and each has to be counted on its own terms. Returns Infinity
// for anything unbounded (a star/plus, an unbounded rep, a cycle) or
// unknown (prose), which is what makes factoring the only remedy.
//
// This must NOT be approximated by asking altPrefixesRaw: that walker
// treats every element it does not recognise as a truncation, and
// before desugar it recognises none of the sugar — so a bounded `("a")`
// or `["-"]` read as unbounded and got factored, collapsing
// alternatives the dispatcher separates perfectly well (and, with
// `marks: true`, silently merging their user-visible marks).
function seqTokenSpan(
  seq: Sequence,
  grammar: Grammar,
  visited: Set<string>,
): number {
  let total = 0
  for (const el of seq) {
    total += elementTokenSpan(el, grammar, visited)
    if (LOOKAHEAD_K < total) return Infinity
  }
  return total
}

function elementTokenSpan(
  el: Element,
  grammar: Grammar,
  visited: Set<string>,
): number {
  switch (el.kind) {
    case 'term':
    case 'regex':
    case 'token':
      return 1
    case 'prose':
      return Infinity
    case 'opt':
      // Absent contributes 0, present contributes the inner span; the
      // MOST it can span is the inner span.
      return elementTokenSpan(el.inner, grammar, visited)
    case 'star':
    case 'plus':
      return Infinity
    case 'rep':
      return Infinity === el.max
        ? Infinity
        : el.max * elementTokenSpan(el.inner, grammar, visited)
    case 'group': {
      let most = 0
      for (const alt of el.alts) {
        const n = seqTokenSpan(alt, grammar, visited)
        if (most < n) most = n
      }
      return most
    }
    case 'ref': {
      if (visited.has(el.name)) return Infinity
      const target = grammar.productions.find((p) => p.name === el.name)
      if (!target || 0 === target.alts.length) return Infinity
      const sub = new Set(visited)
      sub.add(el.name)
      let most = 0
      for (const alt of target.alts) {
        const n = seqTokenSpan(alt, grammar, sub)
        if (most < n) most = n
      }
      return most
    }
  }
}


// First-character coverage of an element, from the raw IR (token
// allocation has not happened when left factoring runs). Returns null
// when coverage cannot be established — a nullable element leaks its
// FOLLOW, a cycle or unparseable regex is unknown — and the caller
// treats null as "not provably disjoint".
function firstCharRangesOfElement(
  el: Element,
  grammar: Grammar,
  visited: Set<string>,
): Array<[number, number]> | null {
  switch (el.kind) {
    case 'term': {
      if (0 === el.literal.length) return null
      const cp = el.literal.codePointAt(0) as number
      if (!isEffectivelyCaseSensitive(el)) {
        const c = String.fromCodePoint(cp)
        const lo = c.toLowerCase().codePointAt(0) as number
        const up = c.toUpperCase().codePointAt(0) as number
        return lo === up ? [[cp, cp]] : [[lo, lo], [up, up]]
      }
      return [[cp, cp]]
    }
    case 'regex':
      return patternCharRanges(el.pattern)
    case 'ref': {
      if (visited.has(el.name)) return null
      const target = grammar.productions.find((p) => p.name === el.name)
      if (!target || 0 === target.alts.length) return null
      const sub = new Set(visited)
      sub.add(el.name)
      const out: Array<[number, number]> = []
      for (const alt of target.alts) {
        if (0 === alt.length) return null
        const r = firstCharRangesOfElement(alt[0], grammar, sub)
        if (null == r) return null
        out.push(...r)
      }
      return out
    }
    case 'group': {
      const out: Array<[number, number]> = []
      for (const alt of el.alts) {
        if (0 === alt.length) return null
        const r = firstCharRangesOfElement(alt[0], grammar, visited)
        if (null == r) return null
        out.push(...r)
      }
      return out
    }
    case 'plus':
      return firstCharRangesOfElement(el.inner, grammar, visited)
    case 'rep':
      return 0 < el.min
        ? firstCharRangesOfElement(el.inner, grammar, visited)
        : null
    case 'opt':
    case 'star':
    case 'token':
    case 'prose':
      return null
  }
}

// See through a single-alternative group: `( a b )` as an entire
// alternative is just `a b`. (A multi-alternative group is a real
// alternation and stays opaque.)
function unwrapAlt(alt: Sequence): Sequence {
  let a = alt
  while (1 === a.length && 'group' === a[0].kind && 1 === a[0].alts.length) {
    a = a[0].alts[0]
  }
  return a
}

// Left-factor alternatives that share a leading element prefix.
//
// tabnas alternates are first-match-wins: once an alternative's first
// token matches, the engine commits to it. Two alternatives sharing a
// non-trivial prefix (`stmt = ident SP "=" … / ident SP "(" …`) can
// therefore never both be reachable — the first wins the shared prefix
// and fails where the second would have succeeded, and no finite token
// lookahead can separate them (the shared prefix has unbounded token
// length). The classical fix is mechanical: factor the prefix out and
// defer the choice to a helper that dispatches on the first token
// AFTER the prefix, where the alternatives really differ.
//
//   P = α β1 / α β2   ⇒   P = α P$factN ; P$factN = β1 / β2
//
// Only CONSECUTIVE alternatives merge: folding a later alternative
// over an intervening one would promote it in first-match order, which
// is observable whenever the intervening alternative overlaps. The
// helper is a transparent 'helper' node, so factoring never changes
// the emitted tree; a helper whose tails include the empty sequence is
// flagged `repeatHelper` so its empty alternative gets the same FOLLOW
// guards a repetition's terminator does.
function leftFactor(grammar: Grammar): Grammar {
  const used = new Set(grammar.productions.map((p) => p.name))
  const out: Production[] = []
  const queue = [...grammar.productions]

  const freshName = (base: string): string => {
    let i = 0
    let name: string
    do { name = `${base}$fact${i++}` } while (used.has(name))
    used.add(name)
    return name
  }

  while (0 < queue.length) {
    const prod = queue.shift() as Production
    if (prod.probeDispatch || prod.probeHelper || prod.tailRepeat) {
      out.push(prod)
      continue
    }
    let alts = prod.alts
    for (;;) {
      const next = factorOnce(
        prod.name, originOf(prod), alts, freshName, queue, grammar)
      if (null == next) break
      alts = next
    }
    out.push(alts === prod.alts ? prod : { ...prod, alts })
  }

  return { productions: out }
}

// One factoring step over one production's alternatives: find the
// first consecutive run sharing a leading element, replace it with a
// single factored alternative, and queue the tail helper (so it is
// itself factored in turn). Returns null when nothing shares a prefix.
//
// Only prefixes the dispatcher cannot see past are factored: the
// emitter separates competing alternatives with up to LOOKAHEAD_K
// concrete-token prefixes, so a short bounded shared prefix (`"a" X /
// "a" Y`) is already handled — and left alone, which also preserves
// the per-alternative identity that collision marks and tree tests
// depend on. A prefix that can span LOOKAHEAD_K tokens or has
// unbounded token length (`identifier ws "=" … / identifier ws "(" …`)
// is beyond any finite lookahead, and factoring is the only fix.
function factorOnce(
  prodName: string,
  prodOrigin: string,
  alts: Sequence[],
  freshName: (base: string) => string,
  queue: Production[],
  grammar: Grammar,
): Sequence[] | null {
  const views = alts.map(unwrapAlt)

  const prefixBeyondLookahead = (prefix: Sequence): boolean =>
    LOOKAHEAD_K < seqTokenSpan(prefix, grammar, new Set())

  for (let i = 0; i < alts.length - 1; i++) {
    if (0 === views[i].length) continue
    const headEl = views[i][0]

    // Gather run members. A later alternative joins the run when its
    // first element equals the head — directly, or after inlining a
    // single-alternative rule it starts with (`funcCall` in `factor =
    // identifier / … / funcCall`, where `funcCall = identifier "(" …`;
    // the inlined rule's own node flattens into the enclosing rule for
    // that alternative — acceptance is what factoring buys here, and
    // only this shape pays for it). Alternatives between members are
    // skipped over only when their first characters are provably
    // disjoint from the head's, so promoting a member across them
    // cannot change which alternative wins any input.
    const members: number[] = [i]
    const memberViews = new Map<number, Sequence>([[i, views[i]]])
    let headRanges: Array<[number, number]> | null | undefined
    for (let j = i + 1; j < alts.length; j++) {
      const v = views[j]
      if (0 < v.length && elemEqual(headEl, v[0])) {
        members.push(j)
        memberViews.set(j, v)
        continue
      }
      const inlined = 0 < v.length ? inlineHeadRef(v, headEl, grammar) : null
      if (null != inlined) {
        members.push(j)
        memberViews.set(j, inlined)
        continue
      }
      // Not a member — skippable only if provably disjoint.
      if (undefined === headRanges) {
        headRanges = firstCharRangesOfElement(headEl, grammar, new Set())
      }
      if (null == headRanges || 0 === v.length) break
      const r = firstCharRangesOfElement(v[0], grammar, new Set())
      if (null == r || charRangesOverlap(headRanges, r)) break
    }
    if (members.length < 2) continue

    const run = members.map((m) => memberViews.get(m) as Sequence)
    // Longest element-wise common prefix of the run.
    let plen = 1
    outer: for (;;) {
      for (const v of run) {
        if (v.length <= plen || !elemEqual(run[0][plen], v[plen])) break outer
      }
      plen++
    }

    const prefix = run[0].slice(0, plen)
    if (!prefixBeyondLookahead(prefix)) {
      // Dispatch lookahead already separates these — leave them be.
      continue
    }
    // Structurally duplicate tails collapse — a duplicated alternative
    // can never win over its first copy under first-match-wins.
    const tails: Sequence[] = []
    for (const v of run) {
      const tail = v.slice(plen)
      if (!tails.some((t) => seqEqual(t, tail))) tails.push(tail)
    }
    // Empty tail last: it matches anything, so the longer
    // continuations must be offered first.
    tails.sort((a, b) => (0 === a.length ? 1 : 0) - (0 === b.length ? 1 : 0))

    let factored: Sequence
    if (1 === tails.length) {
      // All run members were structurally identical.
      factored = [...prefix, ...tails[0]]
    } else {
      const helper = freshName(prodName)
      const helperProd: Production = {
        name: helper,
        alts: tails,
        nodeKind: 'helper',
        origin: prodOrigin,
      }
      if (tails.some((t) => 0 === t.length)) helperProd.repeatHelper = true
      queue.push(helperProd)
      factored = [...prefix, { kind: 'ref', name: helper }]
    }

    // Preserve the enclosing shape: when every run member arrived
    // wrapped in its own single-alt group, keep the factored
    // alternative wrapped too, so a production whose alternatives were
    // all simple group refs stays that way for the emitter.
    const wrapped = members.every(
      (m) => 1 === alts[m].length && 'group' === alts[m][0].kind)
    const replacement: Sequence = wrapped
      ? [{ kind: 'group', alts: [factored] }]
      : factored

    const removed = new Set(members.slice(1))
    const out: Sequence[] = []
    for (let k = 0; k < alts.length; k++) {
      if (removed.has(k)) continue
      out.push(k === i ? replacement : alts[k])
    }
    return out
  }

  return null
}

// If `v` starts with a ref to a single-alternative production whose
// body's first element equals `headEl`, return `v` with the ref
// replaced by that body. One level deep, and never through the
// synthetic production kinds. Returns null when the shape doesn't
// apply.
function inlineHeadRef(
  v: Sequence,
  headEl: Element,
  grammar: Grammar,
): Sequence | null {
  const h = v[0]
  if ('ref' !== h.kind) return null
  const target = grammar.productions.find((p) => p.name === h.name)
  if (!target || 1 !== target.alts.length) return null
  if (target.probeDispatch || target.probeHelper || target.tailRepeat) {
    return null
  }
  const body = unwrapAlt(target.alts[0])
  if (0 === body.length || !elemEqual(body[0], headEl)) return null
  return [...body, ...v.slice(1)]
}


function desugar(grammar: Grammar): Grammar {
  const extra: Production[] = []
  const used = new Set(grammar.productions.map((p) => p.name))

  // Origin of the production currently being desugared: every helper
  // minted below belongs to it, and says so, so the emitted provenance
  // map can point `_gen7_star_DIGIT` back at the rule the author wrote.
  // Closure state rather than a parameter because `desugarAlt` is
  // handed straight to `Array.map`, which would fill a second parameter
  // with the array index.
  let origin = ''

  function freshName(hint: string): string {
    // Collision-avoiding name like `_gen1`, `_gen2`, …
    let i = extra.length
    let name: string
    do {
      i++
      name = `_gen${i}_${hint}`
    } while (used.has(name))
    used.add(name)
    return name
  }

  function desugarAlt(alt: Sequence): Sequence {
    return alt.map(desugarElement)
  }

  function desugarElement(el: Element): Element {
    if (el.kind === 'term' || el.kind === 'ref' || el.kind === 'regex' ||
        el.kind === 'token') {
      return el
    }

    if (el.kind === 'prose') {
      // Unreachable: `resolveProseTerminals` drops every prose element
      // (or throws) before desugaring runs.
      throw new Error(
        `${diagName()}: internal: unresolved prose terminal '<${el.text}>'`)
    }

    if (el.kind === 'group') {
      // Recurse into the group's alts so nested sugar is flattened,
      // then emit a helper production whose body is those alts.
      const innerAlts = el.alts.map((a) => desugarAlt(a))
      const name = freshName('group')
      extra.push({ name, alts: innerAlts, nodeKind: 'helper', origin })
      return { kind: 'ref', name }
    }

    // `opt`, `star`, `plus` all wrap a single inner element.
    const inner = desugarElement(el.inner)
    // Name the generated helper after what it repeats. A literal lifted
    // from a named production (`PL = "+"`) carries that name, and a
    // built-in token carries its own, so `*PL` still yields
    // `_genN_star_PL` rather than an anonymous `_genN_star_term`.
    const hint =
      inner.kind === 'ref' ? inner.name :
        inner.kind === 'term' ? (inner.tokenName ?? 'term') :
          inner.kind === 'token' ? inner.name.replace(/^#/, '') : 'x'

    if (el.kind === 'opt') {
      // H ::= inner | (empty)
      const name = freshName('opt_' + hint)
      extra.push({
        name, alts: [[inner], []], nodeKind: 'helper', repeatHelper: true,
        origin,
      })
      return { kind: 'ref', name }
    }

    if (el.kind === 'star') {
      // H = inner H / (empty)
      const name = freshName('star_' + hint)
      const selfRef: Element = { kind: 'ref', name }
      const helper: Production = {
        name,
        alts: [[inner, selfRef], []],
        nodeKind: 'helper',
        repeatHelper: true,
        origin,
      }
      // A left-recursion tail loop that may have to yield to an
      // enclosing suffix carries its counter onto the helper it becomes
      // — the rule the guard is actually emitted on.
      if (el.debtGuard) helper.debtGuard = el.debtGuard
      extra.push(helper)
      return { kind: 'ref', name }
    }

    if (el.kind === 'plus') {
      // H = inner Tail   where   Tail = inner Tail / (empty)
      const tailName = freshName('star_' + hint)
      const plusName = freshName('plus_' + hint)
      const tailRef: Element = { kind: 'ref', name: tailName }
      extra.push({
        name: tailName,
        alts: [[inner, tailRef], []],
        nodeKind: 'helper',
        repeatHelper: true,
        origin,
      })
      extra.push({
        name: plusName,
        alts: [[inner, tailRef]],
        nodeKind: 'helper',
        origin,
      })
      return { kind: 'ref', name: plusName }
    }

    // ABNF m*n bounded repetition. Desugars to a concatenation of
    // `min` mandatory copies of the inner element followed by a
    // tail that accepts up to `(max - min)` more.
    //   m*n A   =>   A{m}  [A[A[A...[A]]]]   (nested optionals)
    //   m*  A   =>   A{m}  *A                 (mandatory prefix + star)
    //   *n  A   =>   [A [A ... [A]]]          (n nested optionals)
    // The helper's single alt has `min` repetitions of inner, then
    // either a star-helper for (min, ∞) or `max - min` nested
    // optionals for a finite range.
    const { min, max } = el
    const repName = freshName('rep_' + hint)
    const repAlt: Sequence = []
    for (let i = 0; i < min; i++) repAlt.push(inner)

    if (max === Infinity) {
      // Tail: unbounded star of inner.
      const tailStarName = freshName('star_' + hint)
      const tailStarRef: Element = { kind: 'ref', name: tailStarName }
      extra.push({
        name: tailStarName,
        alts: [[inner, tailStarRef], []],
        nodeKind: 'helper',
        repeatHelper: true,
        origin,
      })
      repAlt.push(tailStarRef)
    } else {
      // Nest (max - min) optionals: [A [A [A ...]]].
      //
      // Built bottom-up as an explicit chain of helper productions
      // rather than as one deeply-nested inline element tree. The
      // shape (and every generated name) is identical either way, but
      // handing a nested tree to `desugarAlt` costs a stack frame per
      // repetition, and real ABNF carries big bounds — RFC 5322's
      // `body = (*(*998text CRLF) *998text)` blew the call stack with
      // `Maximum call stack size exceeded` before reaching the emitter.
      //
      // Each level is exactly what `desugarElement` would have emitted
      // for `opt(group([[inner, <previous level>]]))`: the group helper
      // first, then the optional wrapping a reference to it. Pushing in
      // that order keeps `freshName`'s numbering unchanged.
      let nestedRef: Element | null = null
      for (let i = 0; i < max - min; i++) {
        const seq: Sequence = nestedRef ? [inner, nestedRef] : [inner]
        const groupName = freshName('group')
        extra.push({ name: groupName, alts: [seq], nodeKind: 'helper', origin })
        const groupRef: Element = { kind: 'ref', name: groupName }
        const optName = freshName('opt_' + groupName)
        extra.push({
          name: optName,
          alts: [[groupRef], []],
          nodeKind: 'helper',
          repeatHelper: true,
          origin,
        })
        nestedRef = { kind: 'ref', name: optName }
      }
      if (nestedRef) repAlt.push(nestedRef)
    }

    extra.push({
      name: repName, alts: [desugarAlt(repAlt)], nodeKind: 'helper', origin,
    })
    return { kind: 'ref', name: repName }
  }

  const rewritten: Production[] = grammar.productions.map((p) => {
    origin = originOf(p)
    const out: Production = {
      name: p.name,
      alts: p.alts.map(desugarAlt),
      nodeKind: p.nodeKind,
      origin: p.origin,
    }
    // Probe-dispatch and tail-repeat flags survive desugar unchanged —
    // the emitter routes around the standard alt-compilation path for
    // these. (A tail-repeat separator is all-terminal by construction,
    // so it needs no desugaring of its own.) `repeatHelper` also
    // arrives from upstream now: left factoring flags its nullable
    // tail helpers so their empty alternative gets FOLLOW guards.
    if (p.probeDispatch) out.probeDispatch = p.probeDispatch
    if (p.probeHelper) out.probeHelper = p.probeHelper
    if (p.tailRepeat) out.tailRepeat = p.tailRepeat
    if (p.repeatHelper) out.repeatHelper = p.repeatHelper
    if (p.debtGuard) out.debtGuard = p.debtGuard
    return out
  })

  return { productions: [...rewritten, ...extra] }
}


function refsIn(alt: Sequence, out: Set<string>): void {
  for (const el of alt) {
    if (el.kind === 'ref') out.add(el.name)
    else if (el.kind === 'opt' || el.kind === 'star' ||
             el.kind === 'plus' || el.kind === 'rep') {
      refsIn([el.inner], out)
    } else if (el.kind === 'group') {
      for (const a of el.alts) refsIn(a, out)
    }
  }
}


// -- Probe-dispatch analyser + rewriter -----------------------------
//
// ABNF has a large family of grammars that aren't LL(k) for any
// bounded k. The canonical example is RFC 3986's `authority`:
//
//   authority = [ userinfo "@" ] host [ ":" port ]
//   userinfo  = *( unreserved / pct-encoded / sub-delims / ":" )
//   host      = IP-literal / IPv4address / reg-name
//   reg-name  = *( unreserved / pct-encoded / sub-delims )
//
// `userinfo` and `reg-name` share a character vocabulary, so a
// FIRST-set dispatcher can't decide which branch the optional
// `[ userinfo "@" ]` belongs to — the disambiguating `@` can be
// arbitrarily far from the start.
//
// For the common pattern `[X D] Y` — an optional group whose body
// ends with a terminal D, followed by a sequence Y whose leading
// terminals overlap with X's — we handle the ambiguity by rewriting
// the rule to a probe+phase-retry dispatcher:
//
//   1. On first entry (phase 0), mark the token position and push a
//      failure-proof probe rule that greedily consumes every token
//      in the joint vocabulary of X and Y.
//   2. When the probe returns, peek ctx.t[0]:
//        D seen   → phase = 1 (take the `X D Y` branch)
//        D absent → phase = 2 (take the `Y` branch)
//      Rewind to the mark and `r:` back into the dispatcher.
//   3. The dispatcher's open has a `c:`-guarded alt for each phase
//      that pushes the corresponding committed branch.
//
// The primitives used (`r:`, `k:`, `c:`, `ctx.mark`, `ctx.rewind`,
// `ctx.t`) are the same building blocks rules/parser already exposes
// — no new tabnas machinery is needed.


// Predicate: element is `[ X D ]` where X is one or more elements
// and D is a terminal literal or a regex terminal.
function isProbeableOpt(el: Element): null | {
  xSeq: Sequence
  disambiguator: Element
} {
  if (el.kind !== 'opt') return null
  const inner = el.inner
  if (inner.kind !== 'group') return null
  if (inner.alts.length !== 1) return null
  const seq = inner.alts[0]
  if (seq.length < 2) return null
  const last = seq[seq.length - 1]
  if (last.kind !== 'term' && last.kind !== 'regex' && last.kind !== 'token')
    return null
  return { xSeq: seq.slice(0, -1), disambiguator: last }
}


// Union of every terminal reachable by walking an element's subtree,
// following refs transitively. Cycles are broken by the visited set.
// Returns terminals as AbnfElements so the caller isn't tied to the
// emitter's token-allocation step.
function collectTerminalVocabElements(
  el: Element,
  grammar: Grammar,
  out: Map<string, Element>,
  visited: Set<string>,
): void {
  if (el.kind === 'term') {
    const k = termKey(el)
    if (!out.has(k)) out.set(k, el)
    return
  }
  if (el.kind === 'regex') {
    const k = regexKey(el)
    if (!out.has(k)) out.set(k, el)
    return
  }
  if (el.kind === 'token') {
    if (!out.has(el.name)) out.set(el.name, el)
    return
  }
  if (el.kind === 'ref') {
    if (visited.has(el.name)) return
    visited.add(el.name)
    const prod = grammar.productions.find((p) => p.name === el.name)
    if (!prod) return
    for (const alt of prod.alts)
      for (const sub of alt)
        collectTerminalVocabElements(sub, grammar, out, visited)
    return
  }
  if (el.kind === 'opt' || el.kind === 'star' || el.kind === 'plus' ||
      el.kind === 'rep') {
    collectTerminalVocabElements(el.inner, grammar, out, visited)
    return
  }
  if (el.kind === 'group') {
    for (const alt of el.alts)
      for (const sub of alt)
        collectTerminalVocabElements(sub, grammar, out, visited)
    return
  }
}


function collectSeqVocabElements(
  seq: Sequence,
  grammar: Grammar,
): Map<string, Element> {
  const out = new Map<string, Element>()
  const visited = new Set<string>()
  for (const el of seq)
    collectTerminalVocabElements(el, grammar, out, visited)
  return out
}


function mapsOverlap<K, V>(a: Map<K, V>, b: Map<K, V>): boolean {
  for (const x of a.keys()) if (b.has(x)) return true
  return false
}


// Rewrite every ambiguous `[X D] Y` subsequence in `grammar` into a
// probe-dispatch pattern. The grammar at this point still has `opt`,
// `group`, `star`, `plus`, `rep` sugar — intentionally, since that's
// where the pattern is easy to recognise. Runs BEFORE token
// allocation; probe metadata stores AbnfElements, and the emitter
// resolves them to token names at emit time.
function rewriteProbeDispatches(grammar: Grammar): Grammar {
  const reports: AmbiguityReport[] = grammar.ambiguities ?? []
  const extra: Production[] = []
  const used = new Set<string>(grammar.productions.map((p) => p.name))

  function freshName(hint: string): string {
    let name = hint
    let i = 1
    while (used.has(name)) { name = hint + i; i++ }
    used.add(name)
    return name
  }

  const rewritten: Production[] = []

  for (const prod of grammar.productions) {
    let newAlts: Sequence[] = []
    let touched = false
    for (let altIdx = 0; altIdx < prod.alts.length; altIdx++) {
      const alt = prod.alts[altIdx]
      let resultAlt: Sequence = []
      for (let i = 0; i < alt.length; i++) {
        const el = alt[i]
        const info = isProbeableOpt(el)
        if (!info) { resultAlt.push(el); continue }
        const ySeq = alt.slice(i + 1)
        if (ySeq.length === 0) {
          // `[X D]` is the last thing in the alt — nothing follows, so
          // there's nothing to disambiguate against. Standard emit.
          resultAlt.push(el); continue
        }
        const xVocab = collectSeqVocabElements(info.xSeq, grammar)
        const yVocab = collectSeqVocabElements(ySeq, grammar)
        if (!mapsOverlap(xVocab, yVocab)) {
          // The optional's leading tokens don't overlap with the tail's
          // leading tokens, so the normal FIRST-based dispatcher can
          // decide. No rewrite needed.
          resultAlt.push(el); continue
        }

        // Joint vocab: union of everything the probe might need to
        // consume. Includes the disambiguator, which we then remove so
        // the probe stops on it and the peek works.
        const vocab = new Map<string, Element>([...xVocab, ...yVocab])
        const d = info.disambiguator
        const dKey = d.kind === 'term' ? termKey(d)
          : d.kind === 'regex' ? regexKey(d)
          : d.kind === 'token' ? d.name
          : null
        if (dKey) vocab.delete(dKey)

        const dispatchName = freshName(`${prod.name}$pd${i}`)
        const probeName = freshName(`${dispatchName}$probe`)
        const withName = freshName(`${dispatchName}$with`)
        const noName = freshName(`${dispatchName}$no`)

        // Synthesise the probe helper.
        extra.push({
          name: probeName,
          alts: [],
          probeHelper: { vocabElements: [...vocab.values()] },
          nodeKind: 'helper',
          origin: originOf(prod),
        })
        // Synthesise the committed branches. `with` = X D Y, `no` = Y.
        extra.push({
          name: withName,
          alts: [[...info.xSeq, info.disambiguator, ...ySeq]],
          nodeKind: 'helper',
          origin: originOf(prod),
        })
        extra.push({
          name: noName,
          alts: [ySeq],
          nodeKind: 'helper',
          origin: originOf(prod),
        })
        // Synthesise the dispatcher. The `alts` list is a "virtual"
        // spec — two ref-only alts — that exists solely to feed
        // computeFirstSets the right FIRST/nullable answers (FIRST
        // = FIRST(with) ∪ FIRST(no)). The emitter checks
        // `probeDispatch` first and emits the phase-retry body
        // instead of compiling `alts`.
        extra.push({
          name: dispatchName,
          alts: [
            [{ kind: 'ref', name: withName }],
            [{ kind: 'ref', name: noName }],
          ],
          probeDispatch: {
            probeRule: probeName,
            disambiguator: info.disambiguator,
            withBranch: withName,
            noBranch: noName,
          },
          nodeKind: 'helper',
          origin: originOf(prod),
        })

        reports.push({
          rule: prod.name, altIdx, optIdx: i,
          reason: `optional prefix shares vocabulary with tail`,
          resolved: true,
        })

        resultAlt.push({ kind: 'ref', name: dispatchName })
        // Everything that followed the opt is now inside the dispatcher
        // (withBranch / noBranch), so skip the rest of the alt.
        i = alt.length
        touched = true
      }
      newAlts.push(resultAlt)
    }
    if (touched) {
      rewritten.push({
        name: prod.name,
        alts: newAlts,
        nodeKind: prod.nodeKind,
        origin: prod.origin,
      })
    } else {
      rewritten.push(prod)
    }
  }

  return {
    productions: [...rewritten, ...extra],
    ambiguities: reports,
  }
}


// Emit a probe helper production. A self-looping rule that matches any
// one of the vocab tokens and restarts; a final empty-alt fallback
// ensures the rule NEVER fails — if the current lookahead isn't in the
// vocab (or we're at #ZZ), the rule pops cleanly. This is the
// failure-proof property the probe pattern relies on.
function emitProbeHelper(
  prod: Production,
  tag: string,
  ruleSpec: NonNullable<GrammarSpec['rule']>,
  literals: Map<string, string>,
  regexTokens: Map<string, string>,
): void {
  const elems = prod.probeHelper!.vocabElements
  const opens: any[] = []
  for (const el of elems) {
    const tok = el.kind === 'term'
      ? literals.get(termKey(el))
      : el.kind === 'regex' ? regexTokens.get(regexKey(el))
      : el.kind === 'token' ? el.name
      : undefined
    if (tok) opens.push({ s: tok, r: prod.name, g: tag })
  }
  // Empty fallback — pops without consuming anything. Must be last.
  opens.push({ g: tag })
  ruleSpec[prod.name] = { open: opens }
}


// Emit a probe-dispatch production. Encodes the three-phase retry
// pattern; uses only standard tabnas primitives (r:, p:, c:, k:,
// ctx.mark/rewind/t).
function emitProbeDispatch(
  prod: Production,
  tag: string,
  ruleSpec: NonNullable<GrammarSpec['rule']>,
  refs: RefRegistry,
  literals: Map<string, string>,
  regexTokens: Map<string, string>,
  useBuiltins: boolean,
): void {
  const { probeRule, disambiguator, withBranch, noBranch } =
    prod.probeDispatch!
  const disambiguatorToken =
    disambiguator.kind === 'term'
      ? literals.get(termKey(disambiguator))
      : disambiguator.kind === 'regex'
        ? regexTokens.get(regexKey(disambiguator))
        : disambiguator.kind === 'token'
          ? disambiguator.name
          : undefined
  if (!disambiguatorToken) {
    throw new Error(
      `${diagName()}: probe-dispatch rule '${prod.name}' has unresolvable ` +
      `disambiguator (kind=${disambiguator.kind})`)
  }

  // `bubble` lifts the committed child's node up — pure tree-building
  // (a `@bubble$` builtin or a closure, per refs mode; dropped in
  // recognition mode either way).
  const bubbleFields = refs.bubble((r: Rule) => {
    if (r.child && r.child.node !== undefined) r.node = r.child.node
  })

  if (useBuiltins) {
    // Function-free dispatcher: control logic is engine `$`-builtins,
    // the disambiguator token rides in `k` config. See
    // docs/design/alt-action-refs.md §6.3.
    ruleSpec[prod.name] = {
      open: [
        { c: '@probePhase0$', a: '@probeInit$', p: probeRule,
          k: { pd_d: disambiguatorToken }, g: tag } as any,
        { c: '@probePhase1$', p: withBranch, g: tag } as any,
        { c: '@probePhase2$', p: noBranch, g: tag } as any,
      ],
      close: [
        { c: '@probePhase0$', a: '@probeDecide$', r: prod.name, g: tag } as any,
        { ...bubbleFields, g: tag },
      ],
    }
    return
  }

  const initMark = refs.register((r: Rule, ctx: any) => {
    r.k.pd_phase = 0
    r.k.pd_mark = ctx.mark()
  })

  const decide = refs.register((r: Rule, ctx: any) => {
    // ctx.t[0] is the first token the probe didn't consume. The probe
    // never fails, so this always reflects a real position.
    const peek = ctx.t[0]
    ctx.rewind(r.k.pd_mark)
    const matched = peek && peek.name === disambiguatorToken
    r.k.pd_phase = matched ? 1 : 2
  })

  ruleSpec[prod.name] = {
    open: [
      // Phase 0 — first pass: mark and probe.
      {
        c: refs.register((r: Rule) => !r.k.pd_phase),
        a: initMark,
        p: probeRule,
        g: tag,
      },
      // Phase 1 — disambiguator was seen: commit to X D Y.
      {
        c: refs.register((r: Rule) => r.k.pd_phase === 1),
        p: withBranch,
        g: tag,
      },
      // Phase 2 — disambiguator was not seen: commit to Y alone.
      {
        c: refs.register((r: Rule) => r.k.pd_phase === 2),
        p: noBranch,
        g: tag,
      },
    ],
    close: [
      // Phase 0 close: decide phase based on peek, rewind, retry self.
      {
        c: refs.register((r: Rule) => r.k.pd_phase === 0),
        a: decide,
        r: prod.name,
        g: tag,
      },
      // Phase 1 / 2 close: lift the committed child's node up.
      { ...bubbleFields, g: tag },
    ],
  }
}


// Built-in engine lexer tokens that an ABNF rule may reference by a bare
// uppercase name, mapping the name to the token the lexer emits. This lets a
// grammar say `ident = TX` to match the lexer's whole-word text token rather
// than re-deriving identifiers char-by-char (which, since whitespace is
// ignored between tokens, would greedily merge across spaces). A user (or
// core) rule of the same name always wins.
const BUILTIN_TOKENS: Record<string, string> = {
  TX: '#TX',  // bareword / identifier (text matcher)
  NR: '#NR',  // number (number matcher)
  ST: '#ST',  // quoted string (string matcher)
  VL: '#VL',  // keyword value: true / false / null (value matcher)
}


// The one prose directive the compiler acts on. Matched case-insensitively
// after trimming, so `<remove>`, `<Remove>` and `< remove >` all work.
const REMOVE_PROSE = 'remove'

// The one prose *name*: `<all> = <remove>` clears the whole grammar.
const REMOVE_ALL = 'all'

// A production whose name came from a prose token keeps its angle
// brackets, which no ordinary rulename can contain — so `<all>` and a
// production actually called `all` stay distinct.
const isProseName = (name: string): boolean =>
  name.startsWith('<') && name.endsWith('>')


// Resolve RFC 5234 `prose-val` terminals (`<free text>`).
//
// Prose is informational by definition — RFC 5234 §4 calls it a "last
// resort" escape hatch for describing a terminal in English when no
// formal notation will do. There is nothing to compile, so the converter
// accepts it in exactly one position: as the *entire* body of a
// production naming a built-in lexer token.
//
//   NR = <number>
//
// That line documents, in the grammar text, that `NR` is the engine's
// number token. The lexer already supplies it, so the production is
// dropped here and every `NR` reference falls through to
// `normalizeBuiltinTokens`, which binds it to `#NR`. This is what lets a
// grammar state its terminals explicitly and still round-trip: the same
// line is what @tabnas/debug emits when it renders a live grammar back
// to ABNF.
//
// Prose anywhere else has no definition behind it, so it is an error
// rather than a silently-ignored rule.
function resolveProseTerminals(grammar: Grammar): void {
  const isProse = (el: Element) => el.kind === 'prose'
  const kept: Production[] = []

  for (const prod of grammar.productions) {
    const onlyProse =
      prod.alts.length === 1 &&
      prod.alts[0].length === 1 &&
      isProse(prod.alts[0][0])

    // A prose name is only ever the removal directive. Checked here as
    // well as in the prose-body branch below, because `<all> = "x"` has
    // a *literal* body and would otherwise fall through and be lifted
    // into a token literally named `#<all>`.
    if (isProseName(prod.name) && !onlyProse) {
      throw new Error(
        `${diagName()}: '${prod.name}' is prose, and prose is only valid as a ` +
        `production name for the removal directive '<all> = <remove>'.`)
    }

    if (onlyProse) {
      const text = (prod.alts[0][0] as { text: string }).text

      // `<remove>` — the one prose form that *does* compile to something.
      // Prose is otherwise informational, which is exactly why it is the
      // right place for a directive: it cannot collide with a real
      // terminal, and RFC 5234 already says a tool may interpret it.
      //
      //   name = <remove>   drop that rule and that fixed token
      //   * = <remove>      drop everything — a fresh empty grammar
      if (REMOVE_PROSE === text.trim().toLowerCase()) {
        if (isProseName(prod.name)) {
          const target = prod.name.slice(1, -1).trim().toLowerCase()
          if (REMOVE_ALL !== target) {
            throw new Error(
              `${diagName()}: '<${target}>' is not a removal target. The only prose ` +
              `name is '<all>', as in '<all> = <remove>', which clears the ` +
              `whole grammar. To remove one rule or token, name it directly: ` +
              `'${target} = <remove>'.`)
          }
          grammar.clearAll = true
        }
        else {
          (grammar.remove ??= []).push(prod.name)
        }
        continue
      }

      if (isProseName(prod.name)) {
        throw new Error(
          `${diagName()}: '${prod.name}' is prose, and prose is only valid as a ` +
          `production name for the removal directive '<all> = <remove>'.`)
      }

      if (BUILTIN_TOKENS[prod.name]) continue // informational — the lexer defines it
      throw new Error(
        `${diagName()}: rule '${prod.name}' is defined only by prose ('<${text}>'), ` +
        `which describes a terminal but does not define one. Prose is ` +
        `allowed only for built-in lexer tokens (${
          Object.keys(BUILTIN_TOKENS).join(', ')
        }).`)
    }

    // Any surviving prose is embedded in a larger expression, where it
    // cannot be given a meaning. Search nested groups and repetitions
    // too, so `x = ( <foo> / "a" )` reports the same clear error as a
    // top-level `x = "a" <foo>`.
    const findStray = (el: Element): { text: string } | undefined => {
      if (isProse(el)) return el as { text: string }
      if (el.kind === 'opt' || el.kind === 'star' || el.kind === 'plus' ||
          el.kind === 'rep') {
        return findStray(el.inner)
      }
      if (el.kind === 'group') {
        for (const alt of el.alts) {
          for (const inner of alt) {
            const hit = findStray(inner)
            if (hit) return hit
          }
        }
      }
      return undefined
    }
    for (const alt of prod.alts) {
      for (const el of alt) {
        const stray = findStray(el)
        if (stray) {
          throw new Error(
            `${diagName()}: rule '${prod.name}' uses prose ('<${stray.text}>') inside an ` +
            `expression; prose may only stand alone as the whole definition ` +
            `of a built-in lexer token.`)
        }
      }
    }
    kept.push(prod)
  }

  if (kept.length === 0 && !grammar.clearAll &&
    (undefined === grammar.remove || 0 === grammar.remove.length)) {
    throw new Error(
      `${diagName()}: grammar defines no rules — only informational prose terminals.`)
  }

  grammar.productions = kept
}


// Which token names belong to a lexer matcher is the engine's rule, and
// the engine is where it is enforced (a matcher binding throws from
// `configure()`). This compiler asks rather than keeping its own copy,
// so the two cannot drift: an engine that grows a matcher token gets the
// right compilation here without a matching edit.
//
// The distinction still matters at compile time, because it selects how
// a single-literal production is compiled, not whether it is allowed:
//
//   CA = ";"        fixed token — a literal by definition, so this
//                   rebinds #CA, exactly as `fixed: { token: … }` would
//   TX = "literal"  matcher token — cannot be rebound, so the production
//                   stays an ordinary rule shadowing the bareword
//
// ABNF production names are bare (`TX`), engine token names are prefixed
// (`#TX`).
const isMatcherTokenName = (name: string): boolean => {
  const fn = (engineUtil as any).isMatcherToken
  if ('function' !== typeof fn) {
    throw new Error(
      `${diagName()}: this @tabnas/parser is too old — it does not export ` +
      'util.isMatcherToken, which the compiler needs to tell a fixed ' +
      'token from a matcher-owned one. Upgrade @tabnas/parser.')
  }
  return fn('#' + name)
}


// A literal production lifted to a named token, as returned by
// `liftLiteralTokens` so the emitter can allocate the token even when
// nothing in the grammar references it.
type LiftedLiteral = {
  kind: 'term'
  literal: string
  caseSensitive?: boolean
  tokenName: string
}


// Lift single-literal productions into *named lexer tokens*.
//
// A production whose whole body is one string literal is a lexical
// definition, not a syntactic rule:
//
//   PL = "+"
//
// Compiled as a rule it would produce a token named after the literal
// text — and since `+` has no word characters to name it after, that
// degrades to `#T` / `#T1` / … — plus a one-token `PL` rule wrapping it,
// so a grammar rendered back to ABNF reads `PL = T` with a separate
// `T = "+"`. Lifting instead binds the literal to `#PL` directly and
// drops the rule, which is exactly how the same grammar is written by
// hand against the engine (`fixed: { token: { '#PL': '+' } }`), and what
// lets `PL = "+"` survive the round-trip through @tabnas/debug unchanged.
//
// The start rule is never lifted: it has to stay a rule for the grammar
// to have an entry point, so `greet = "hi"` still compiles to a rule.
// Multi-alternative productions (`sign = "+" / "-"`) are real choices and
// are left alone, as are the RFC 5234 core rules, which callers expect to
// behave as rules wherever they are referenced.
//
// Naming an existing fixed token *redefines* it: `CA = ";"` binds the
// comma token to a semicolon, the same as `fixed: { token: { '#CA': ';' } }`
// by hand. Matcher-owned names are never lifted — see
// isMatcherTokenName.
function liftLiteralTokens(
  grammar: Grammar,
  start: string,
): LiftedLiteral[] {
  const lifted = new Map<string, { literal: string; caseSensitive?: boolean }>()

  for (const prod of grammar.productions) {
    if (prod.name === start) continue
    // A matcher-owned name is never lifted. `TX = "literal"` stays an
    // ordinary rule that shadows the bareword for references inside this
    // grammar (see token.test.js, 'a user rule of the same name wins over
    // the built-in') — which leaves #TX itself untouched. Lifting would
    // instead try to bind #TX to a literal, which the engine refuses.
    if (isMatcherTokenName(prod.name)) continue
    if (prod.nodeKind === 'core') continue
    if (prod.alts.length !== 1 || prod.alts[0].length !== 1) continue
    const el = prod.alts[0][0]
    if (el.kind !== 'term') continue
    // `path-empty = ""` (RFC 3986) matches the empty string — a rule that
    // derives epsilon, not a token the lexer could ever emit.
    if (el.literal === '') continue
    lifted.set(prod.name, { literal: el.literal, caseSensitive: el.caseSensitive })
  }

  // The engine keys its fixed tokens by literal (`cfg.fixed.token` is
  // inverted to src -> tin), so one literal is one token — two names for
  // the same literal cannot both become tokens. When `A = "+"` and
  // `B = "+"` both claim `+`, lifting either would silently drop the
  // other, so neither is lifted and both stay ordinary rules.
  const byLiteral = new Map<string, string[]>()
  for (const [name, lit] of lifted) {
    const key = termKey(lit)
    const names = byLiteral.get(key)
    if (names) names.push(name)
    else byLiteral.set(key, [name])
  }
  for (const names of byLiteral.values()) {
    if (1 < names.length) for (const n of names) lifted.delete(n)
  }

  if (lifted.size === 0) return []

  const walk = (el: Element): Element => {
    if (el.kind === 'ref') {
      const lit = lifted.get(el.name)
      return lit
        ? { kind: 'term', ...lit, tokenName: el.name }
        : el
    }
    if (el.kind === 'opt' || el.kind === 'star' || el.kind === 'plus' ||
        el.kind === 'rep') {
      return { ...el, inner: walk(el.inner) }
    }
    if (el.kind === 'group') {
      return { kind: 'group', alts: el.alts.map((a) => a.map(walk)) }
    }
    return el
  }

  grammar.productions = grammar.productions
    .filter((p) => !lifted.has(p.name))
    .map((p) => ({ ...p, alts: p.alts.map((alt) => alt.map(walk)) }))

  // Return every lifted definition, referenced or not. The production is
  // gone from the grammar, so an unreferenced one (`top = "x"` with a
  // spare `PL = "+"`) would otherwise leave no element behind for the
  // emitter to allocate a token from, and the user's declaration would
  // vanish silently instead of compiling to the promised named token.
  return [...lifted].map(([name, lit]) => ({ kind: 'term' as const, ...lit, tokenName: name }))
}


// Rewrite every bareword reference whose name is a built-in token AND is not a
// defined production into a `token` terminal element. Run before any other
// pass so the rest of the pipeline treats these as ordinary terminals.
function normalizeBuiltinTokens(grammar: Grammar): void {
  const defined = new Set(grammar.productions.map((p) => p.name))
  const walk = (el: Element): Element => {
    if (el.kind === 'ref') {
      const tok = BUILTIN_TOKENS[el.name]
      if (tok && !defined.has(el.name)) return { kind: 'token', name: tok }
      return el
    }
    if (el.kind === 'opt' || el.kind === 'star' || el.kind === 'plus' ||
        el.kind === 'rep') {
      return { ...el, inner: walk(el.inner) }
    }
    if (el.kind === 'group') {
      return { kind: 'group', alts: el.alts.map((a) => a.map(walk)) }
    }
    return el
  }
  for (const prod of grammar.productions) {
    prod.alts = prod.alts.map((alt) => alt.map(walk))
  }
}


// Allocate the lexer token for a string-literal terminal. A
// case-sensitive literal is normally a fixed token and a case-insensitive
// one an anchored `i`-flagged regex. When `wordKeywords` is set and the
// literal ends in a word character, it is emitted as a regex with a
// trailing `(?![A-Za-z0-9_])` guard so the keyword matches only as a whole
// word (e.g. `option` won't match inside `optional`).
function emitLiteralToken(
  el: { literal: string; caseSensitive?: boolean },
  name: string,
  fixedTokens: Record<string, string>,
  matchTokens: Record<string, RegExp>,
  wordKeywords: boolean,
): void {
  const boundary =
    wordKeywords && /[A-Za-z0-9_]$/.test(el.literal)
      ? '(?![A-Za-z0-9_])'
      : ''
  if (isEffectivelyCaseSensitive(el) && boundary === '') {
    fixedTokens[name] = el.literal
    return
  }
  // Insensitive literal, or a word-keyword needing a boundary guard:
  // emit as an anchored regex. Mark it `eager$` so the lexer fires it
  // even when the current rule's tcol doesn't list its tin.
  const flags = isEffectivelyCaseSensitive(el) ? '' : 'i'
  const re = new RegExp(
    '^' + escapeRegExp(el.literal) + boundary,
    flags,
  ) as RegExp & { eager$?: boolean }
  re.eager$ = true
  matchTokens[name] = re
}


// Copy a grammar deeply enough that the emit pipeline cannot disturb the
// caller's AST. The passes below replace `productions`, `alts` and the
// individual sequences, but treat elements as immutable (each rewriting
// walk returns fresh element objects), so sharing elements is safe.
// Diagnostics name the NOTATION the grammar was written in, not this
// package: a front-end's users should not see "bnf:" on an error about
// their own syntax. `emitGrammarSpec` sets this from `opts.tag` (which
// each front-end supplies) for the duration of one emit; the pipeline is
// synchronous, so a module-scoped value is safe and avoids threading a
// prefix through every pass.
let _diagName = 'bnf'

function diagName(): string {
  return _diagName
}


// Recovery sync groups (@tabnas/parser `parse.recover.syncGroups`, whose
// shipped default is exactly ['close','comma','end']). A close alternate
// tagged with one of these offers its LEADING token as a resynchronisation
// point after a syntax error.
//
// Two rules govern how these are stamped, and both are the engine's, not
// preferences:
//
//  1. Only a CLOSE alternate that NAMES A TOKEN can be a sync point. An
//     open alternate contributes nothing, and a close alternate with no
//     `s` is skipped before its tags are even read. Exactly two of this
//     emitter's alternates qualify: the `__start__` wrapper's `#ZZ`, and
//     a tail repeat's separator continuation. Everything else it emits
//     closes by capturing a child, naming no token.
//
//  2. Tag ALL of them or NONE. The engine falls back to "every close
//     alternate's leading token on the stack" only while the tagged set
//     is EMPTY — and that test is over the whole live rule stack, not
//     per rule. So one tagged alternate anywhere switches the fallback
//     off for every rule below it too. Tagging half of them would
//     therefore silently DELETE the other half's sync points. Since the
//     two sites below are the complete set, tagging both is exactly
//     equivalent to the fallback for a grammar parsed on its own — and
//     strictly better when it is composed with an already-tagged
//     grammar, where the host's tags would otherwise disable the
//     fallback these rules were relying on.
//
// Emitted as a comma-separated string because that is the form BOTH
// runtimes accept (`g` as an array is TypeScript-only), with no spaces
// around the comma (the TS grammar builder rejects a padded tag).
function syncG(tag: string, group: 'close' | 'comma' | 'end'): string {
  return tag + ',' + group
}

// Wrap a pattern in a non-capturing group if — and only if — it has
// top-level alternation.
//
// `^` binds tighter than `|`, so `^a|bc` means "starts with a" OR
// "contains bc": the second branch is unanchored and can match at any
// offset, producing a token from the wrong position. Grouping fixes
// that. This matters far more now than it did for ABNF's `%x` ranges,
// because GBNF alternation arrives as `regex` elements.
//
// The grouping is conditional rather than unconditional because
// downstream front-ends read the emitted matcher's `source` to decide
// whether a token is a plain character class — gbnf's eager-lexing pass
// is one — and wrapping every pattern breaks that recognition.
function hasTopLevelAlternation(pattern: string): boolean {
  let inClass = false
  let depth = 0
  for (let i = 0; i < pattern.length; i++) {
    const c = pattern[i]
    if (c === '\\') { i++; continue }
    if (inClass) { if (c === ']') inClass = false; continue }
    if (c === '[') { inClass = true; continue }
    if (c === '(') { depth++; continue }
    if (c === ')') { if (depth > 0) depth--; continue }
    if (c === '|' && depth === 0) return true
  }
  return false
}

function anchorable(pattern: string): string {
  return hasTopLevelAlternation(pattern) ? '(?:' + pattern + ')' : pattern
}


function cloneGrammar(grammar: Grammar): Grammar {
  // Spread the grammar, not just its productions. `emitGrammarSpec`
  // reads `remove`/`clearAll` off the clone, so copying `productions`
  // alone silently discarded them for any front-end that set those
  // fields directly on the IR — a documented part of the `Grammar`
  // contract. ABNF happened not to notice because it populates them via
  // `resolveProseTerminals`, which runs on the clone.
  return {
    ...grammar,
    productions: grammar.productions.map((p) => ({
      ...p,
      alts: p.alts.map((alt) => alt.slice()),
    })),
  }
}


// Convert an ABNF grammar AST into a tabnas GrammarSpec.
function emitGrammarSpec(
  grammar: Grammar,
  opts?: ConvertOptions,
): GrammarSpec {
  // Work on a copy: `resolveProseTerminals`, `liftLiteralTokens` and
  // `normalizeBuiltinTokens` all rewrite the grammar in place, so emitting
  // twice from one `parseAbnf` result would otherwise give two different
  // specs — the second missing every lifted production, since the first
  // pass had already removed them.
  grammar = cloneGrammar(grammar)

  // Drop informational prose definitions (`NR = <number>`) first, so the
  // names they document fall through to the built-in tokens — and so a
  // leading prose line is never mistaken for the start rule.
  resolveProseTerminals(grammar)

  // Capture the <remove> directives before the rewrite passes below:
  // each returns a fresh grammar object carrying only `productions`, so
  // anything else on the grammar is dropped at the first reassignment.
  const removeNames = grammar.remove ? [...grammar.remove] : []
  const clearAll = !!grammar.clearAll

  const start = opts?.start ?? grammar.productions[0].name
  const tag = opts?.tag ?? 'bnf'
  _diagName = tag
  const wordKeywords = !!opts?.wordKeywords

  // Turn single-literal productions (`PL = "+"`) into named lexer
  // tokens, then resolve bare built-in token names (`TX`/`NR`/`ST`/`VL`)
  // to token terminals — both before any structural pass sees them as
  // rule references.
  const liftedLiterals = liftLiteralTokens(grammar, start)
  normalizeBuiltinTokens(grammar)

  // Eliminate direct left recursion (P → P α | β) by rewriting to
  // the equivalent right-recursive form P → β (α)*, then detect
  // ambiguous `[X D] Y` optional-prefix patterns and rewrite them
  // into probe-dispatch helpers; finally flatten any EBNF sugar
  // (`?`, `*`, `+`, grouping) into plain ABNF.
  grammar = eliminateLeftRecursion(grammar)
  grammar = rewriteProbeDispatches(grammar)
  // Left factoring runs after the probe rewriter (so `[X D] Y`
  // patterns are recognised in their original alternatives) and
  // before tail-repeat detection and desugaring.
  grammar = leftFactor(grammar)
  grammar = rewriteTailRepeats(grammar, start)
  grammar = desugar(grammar)

  // Allocate a fixed token for each unique literal, and a match
  // token for each unique regex terminal. Literals are keyed by
  // (literal, effective-case-sensitivity) so a `%s"foo"` (sensitive)
  // and a bare `"foo"` (insensitive) produce distinct tokens.
  const literals = new Map<string, string>()        // literal-key -> token name
  const regexTokens = new Map<string, string>()     // regex key -> token name
  const usedNames = new Set<string>()
  const fixedTokens: Record<string, string> = {}
  const matchTokens: Record<string, RegExp> = {}

  // Gather every terminal first. Probe-helper productions store their
  // vocab as AbnfElements rather than in `alts`, so walk those too. The
  // lifted literals are seeded up front: their productions no longer
  // exist, so an unreferenced one has no element anywhere in `alts`.
  const terminals: Element[] = [...liftedLiterals]
  for (const prod of grammar.productions) {
    for (const alt of prod.alts) terminals.push(...alt)
    if (prod.probeHelper) terminals.push(...prod.probeHelper.vocabElements)
    // A tail repeat's separator is REMOVED from `alts` by
    // `rewriteTailRepeats` and stashed here, so walking `alts` alone
    // misses it. Every other terminal in the grammar is reachable from
    // `alts`, which is why the omission survived: a separator normally
    // shares its literal with some other rule and picks up that rule's
    // token. When it does not — `list = %x30-39 [ "," list ]`, where the
    // comma appears nowhere else — no token is allocated, the emitted
    // separator alternate comes out as `s: ''`, and the repeat can never
    // match. The grammar then silently accepts one element instead of a
    // list.
    if (prod.tailRepeat) terminals.push(...prod.tailRepeat.sep)
  }

  // Terminals carrying a lifted production name are allocated first, so
  // the name wins even when the same literal also appears inline in an
  // earlier rule (`PL = "+"` must yield `#PL`, not `#T`, regardless of
  // where the bare `"+"` shows up).
  const named = terminals.filter((el) => el.kind === 'term' && el.tokenName)
  for (const el of [...named, ...terminals]) {
    if (el.kind === 'term') {
      const key = termKey(el)
      if (!literals.has(key)) {
        const name = allocTokenName(el.literal, usedNames, el.tokenName)
        literals.set(key, name)
        emitLiteralToken(el, name, fixedTokens, matchTokens, wordKeywords)
      }
    } else if (el.kind === 'regex') {
      const key = regexKey(el)
      if (!regexTokens.has(key)) {
        const name = allocTokenName('rx_' + el.pattern, usedNames)
        regexTokens.set(key, name)
        matchTokens[name] = new RegExp('^' + anchorable(el.pattern), el.flags)
      }
    }
  }

  const knownRules = new Set(grammar.productions.map((p) => p.name))
  const { firstSets, nullable } = computeFirstSets(
    grammar, literals, regexTokens)
  const followSets = computeFollowSets(
    grammar, literals, regexTokens, firstSets, nullable, start)
  const followPairs = computeFollowPairs(
    grammar, literals, regexTokens, firstSets, nullable, followSets)

  // Character coverage per token, for the contested-repetition check.
  // A fixed token covers its literal's first code point; a match token
  // covers whatever its leading character class covers; null means
  // unknown (and the guard emission stays conservative).
  const rangeCache = new Map<string, Array<[number, number]> | null>()
  const tokenRangesOf = (tok: string): Array<[number, number]> | null => {
    if (rangeCache.has(tok)) return rangeCache.get(tok) as any
    let r: Array<[number, number]> | null = null
    const lit = fixedTokens[tok]
    if ('string' === typeof lit && 0 < lit.length) {
      const cp = lit.codePointAt(0) as number
      r = [[cp, cp]]
    } else {
      const re = matchTokens[tok]
      if (null != re) {
        // Strip the emitter's own `^` anchor (and grouping) so the
        // parser sees the pattern as written.
        let src = re.source.replace(/^\^/, '')
        const m = /^\(\?:(.*)\)$/.exec(src)
        if (m) src = m[1]
        r = patternCharRanges(src)
        // A case-insensitive matcher covers both cases of every letter
        // it names, and the pattern text only spells one of them. ABNF
        // literals are case-insensitive by default, so without this an
        // unquoted `"GET"` reads as covering `G` alone — no contest is
        // detected against a lowercase identifier class, no guards are
        // emitted, and a valid sentence is rejected. Keeps this in step
        // with firstCharRangesOfElement, which folds case already.
        if (null != r && re.flags.includes('i')) {
          r = foldCaseRanges(r)
        }
      }
    }
    rangeCache.set(tok, null == r ? null : normalizeRanges(r))
    return rangeCache.get(tok) as any
  }

  // Do two tokens' character coverages intersect? Memoised on the token
  // PAIR, because the contest checks below are quadratic in dispatch
  // entries while the distinct token pairs behind them are few — a
  // grammar with hundreds of entries per rule asks the same handful of
  // questions over and over.
  const overlapCache = new Map<string, boolean>()
  const tokensOverlap = (a: string, b: string): boolean => {
    // '\u0000' as an ESCAPE, not a literal NUL byte. A literal one
    // makes this whole file binary to grep, which silently reports no
    // matches for anything in it — including the names of every
    // function below, which is how a Go port came to be scoped as
    // absent when it was merely unfindable.
    const key = a < b ? a + '\u0000' + b : b + '\u0000' + a
    const hit = overlapCache.get(key)
    if (undefined !== hit) return hit
    const ra = tokenRangesOf(a)
    const rb = tokenRangesOf(b)
    const out = null != ra && null != rb && charRangesOverlap(ra, rb)
    overlapCache.set(key, out)
    return out
  }

  // Settle the contested left-recursion tail loops flagged during
  // elimination, now that FIRST sets can say whether the competition is
  // real and `tokensOverlap` can say so at the character level. Runs on
  // the desugared grammar, because the loop is a helper production by
  // this point, and only annotates — it never changes the language the
  // grammar describes, so nothing computed above depends on it.
  resolveSuffixDebts(
    grammar, literals, regexTokens, firstSets, nullable, tokensOverlap)

  const refs = new RefRegistry()
  refs.useBuiltins = !!opts?.builtins
  refs.emitMarks = !!opts?.marks

  // Synthetic-rule provenance, accumulated as rules are emitted (see
  // `Production.origin`). Recorded here rather than derived from the
  // emitted names afterwards: the names compose
  // (`_gen6_star__gen5_group$alt0$step1`), and a front-end's notation may
  // allow `$` in a rule name, so parsing a name back into its parts would
  // be guesswork. Each minting site knows the answer; it just has to say.
  const prov: Map<string, string> | undefined =
    false === opts?.provenance ? undefined : new Map()

  const ruleSpec: NonNullable<GrammarSpec['rule']> = {}
  for (const prod of grammar.productions) {
    // Productions synthesised by the rewrite passes (sugar helpers,
    // factored tails, probe branches) are emitted under their own names.
    if (null != prov && originOf(prod) !== prod.name) {
      prov.set(prod.name, originOf(prod))
    }
    if (prod.probeHelper) {
      emitProbeHelper(prod, tag, ruleSpec, literals, regexTokens)
      continue
    }
    if (prod.probeDispatch) {
      emitProbeDispatch(
        prod, tag, ruleSpec, refs, literals, regexTokens, !!opts?.builtins)
      continue
    }
    // Standard path: a (possibly single-segment) set of alternatives
    // compiled to tabnas alts. Simple alts collapse into `open` alts
    // directly; multi-segment alts emit a chain of aux rules.
    emitProduction(
      prod, grammar, literals, regexTokens, knownRules, tag, ruleSpec,
      firstSets, nullable, refs, followSets, followPairs, tokenRangesOf,
      tokensOverlap, prov,
    )
  }

  // Wrap the user-visible start rule in a synthetic rule that
  // explicitly consumes #ZZ. Without this, a user rule that pops
  // without matching the end-of-source token lets trailing content
  // slip past tabnas's post-loop endtkn check (the lookahead buffer
  // outlives the parse loop).
  // Normally `__start__`, but the IR reserves no names, so a grammar is
  // free to contain a production actually called that. Assigning
  // unconditionally would overwrite the user's rule — and if it were
  // also the start rule, the wrapper would push itself forever. Fall
  // back to a numbered variant, the way the other synthetic passes
  // allocate.
  let startWrapper = '__start__'
  if (knownRules.has(startWrapper)) {
    let n = 2
    while (knownRules.has(`__start${n}__`)) n++
    startWrapper = `__start${n}__`
  }
  // The wrapper stands in for the start rule, so that is what it is
  // named after: a rule stack reading `__start__` helps nobody.
  if (null != prov) prov.set(startWrapper, start)

  ruleSpec[startWrapper] = {
    open: [{
      p: start,
      g: tag,
    }],
    close: [{
      s: '#ZZ',
      // Return the start rule's AST node directly — the `__start__`
      // wrapper exists only to ensure end-of-source gets consumed.
      // The caller of `tabnas(src)` receives the tagged user-rule
      // node (e.g. `{rule: 'URI', src, kids: [...]}`) unadorned.
      ...refs.bubble((r: Rule) => {
        if (r.child && r.child.node !== undefined) {
          r.node = r.child.node
        }
      }),
      // End of source: the one anchor every grammar has, and the last
      // resort for a parse that cannot resynchronise anywhere else.
      g: syncG(tag, 'end'),
    }],
  }

  const options: any = {
    fixed: { token: fixedTokens },
    rule: { start: startWrapper },
  }
  if (Object.keys(matchTokens).length > 0) {
    options.match = { token: matchTokens }
  }

  const spec: GrammarSpec = {
    ref: refs.map,
    options,
    rule: ruleSpec,
  }

  // Engine-ignored tool metadata (@tabnas/parser GrammarSpec.meta): the
  // map from each generated rule name to the author-written production
  // it came from. Sorted, so a serialised grammar is byte-stable across
  // runs and a committed fixture diffs cleanly.
  if (null != prov && 0 < prov.size) {
    const provenance: Record<string, string> = {}
    for (const name of [...prov.keys()].sort()) {
      provenance[name] = prov.get(name) as string
    }
    spec.meta = { provenance }
  }

  // `<remove>` directives. `* = <remove>` maps to the engine's `clear`,
  // which wipes rules and fixed tokens before the rest of the spec is
  // applied — so a grammar can reset an instance and rebuild it in one
  // pass. A named removal drops both the rule and the fixed token of
  // that name, because ABNF does not distinguish them at the point of
  // use and a removal that matches nothing is a no-op either way.
  if (clearAll) {
    spec.clear = true
  }
  if (0 < removeNames.length) {
    for (const name of removeNames) {
      ;(spec.rule as any)[name] = null
      options.fixed.token['#' + name] = null
    }
  }

  return spec
}


type Segment = {
  terms: string[]   // token names (e.g. '#HI')
  ref: string | null // rule name to push after consuming terms
  // Counter mutations the pushing alt carries, from the reference's
  // `debt` annotation. See `resolveSuffixDebts`.
  debt?: Record<string, number>
}


// Break an alternative into segments. Each segment is a (possibly
// empty) run of terminal tokens followed by at most one rule
// reference. A single-segment alt has at most one ref, located at the
// very end; everything else has two or more segments.
function segmentize(
  alt: Sequence,
  literals: Map<string, string>,
  regexTokens: Map<string, string>,
): Segment[] {
  const segs: Segment[] = []
  let current: Segment = { terms: [], ref: null }
  for (const el of alt) {
    if (el.kind === 'term') {
      current.terms.push(literals.get(termKey(el)) as string)
    } else if (el.kind === 'regex') {
      const key = regexKey(el)
      current.terms.push(regexTokens.get(key) as string)
    } else if (el.kind === 'token') {
      current.terms.push(el.name)
    } else if (el.kind === 'ref') {
      current.ref = el.name
      if (el.debt) current.debt = el.debt
      segs.push(current)
      current = { terms: [], ref: null }
    } else {
      // `opt`, `star`, `plus`, `group` must have been desugared
      // before reaching the emitter.
      throw new Error(
        `${diagName()}: internal — unexpected element kind '${el.kind}' in emitter`)
    }
  }
  if (current.terms.length > 0 || segs.length === 0) {
    segs.push(current)
  }
  return segs
}


function regexKey(el: { pattern: string; flags: string }): string {
  return `/${el.pattern}/${el.flags}`
}


function isSingleSegment(alt: Sequence): boolean {
  let sawRef = false
  for (const el of alt) {
    if (el.kind === 'ref') {
      if (sawRef) return false
      sawRef = true
    } else if (el.kind === 'term' || el.kind === 'regex' ||
               el.kind === 'token') {
      if (sawRef) return false // terminal after a ref — multi-segment
    } else {
      // Desugar should have eliminated sugar kinds.
      return false
    }
  }
  return true
}


function validateRefs(
  alt: Sequence,
  knownRules: Set<string>,
  ruleName: string,
) {
  for (const el of alt) {
    if (el.kind === 'ref' && !knownRules.has(el.name)) {
      throw new Error(
        `${diagName()}: rule '${ruleName}' references unknown rule '${el.name}'`)
    }
  }
}


// Registry used by the emitter to allocate unique `@`-prefixed
// FuncRef names for inline action functions. The resulting spec is
// still declarative: every function appears once, keyed by name,
// under the spec's `ref` map.
class RefRegistry {
  private refs: Record<string, Function> = {}
  private counter = 0
  // When set, tree-building actions are emitted as engine `$`-builtin
  // refs + `k` config (pure data) instead of registered closures. See
  // docs/design/alt-action-refs.md §6.4 and implementation-diary.md.
  useBuiltins = false
  // When set, the emitter stamps user-rule alts with a `m` mark.
  emitMarks = false
  register(fn: Function): `@${string}` {
    const name = `@bnf_a${this.counter++}` as `@${string}`
    this.refs[name] = fn
    return name
  }
  get map(): Record<string, Function> {
    return this.refs
  }

  // Tree-action emitters. Each returns the alt-spec fields to merge
  // (`{a}` in closure mode, `{a, k}` in builtins mode).
  node(cfg: Record<string, any>, closure: Function): { a: any; k?: any } {
    return this.useBuiltins
      ? { a: '@node$', k: { node$: cfg } }
      : { a: this.register(closure) }
  }
  capture(cfg: Record<string, any>, closure: Function): { a: any; k?: any } {
    return this.useBuiltins
      ? { a: '@capture$', k: { capture$: cfg } }
      : { a: this.register(closure) }
  }
  bubble(closure: Function): { a: any } {
    return this.useBuiltins ? { a: '@bubble$' } : { a: this.register(closure) }
  }
  fold(cfg: Record<string, any>, closure: Function): { a: any; k?: any } {
    return this.useBuiltins
      ? { a: '@fold$', k: { fold$: cfg } }
      : { a: this.register(closure) }
  }
}


// Closure-mode twin of the engine's `@fold$` builtin (see
// `@tabnas/parser` builtins.ts — the two MUST stay behaviourally
// identical; the fixture suite pins this). Folds a tail-repeat
// iteration's node into its parent as a sibling kid, appends `cN`
// close-phase (separator) tokens' src to the parent, and clears the
// own node so the parent's capture no-ops on its stale first-iteration
// child pointer.
function mkFoldClosure(cN: number): (r: Rule) => void {
  return (r: Rule) => {
    const p = r.parent && (r.parent.node as any)
    if (null == p || 'object' !== typeof p || !('src' in p)) return
    const own = r.node as any
    if (null != own && 'object' === typeof own && 'src' in own && own !== p) {
      p.src += own.src
      if (own.rule) p.kids.push(own)
      else if (Array.isArray(own.kids)) p.kids.push(...own.kids)
    }
    for (let i = 0; i < cN; i++) p.src += r.c[i].src
    r.node = undefined
  }
}


// Output AST node shape. Every rule produces a `{src, kids}` object.
// User-declared rules additionally carry a `rule` tag so callers can
// navigate the tree by grammar-rule name. Core rules (ALPHA, DIGIT
// …) and synthesised helpers (desugar / dispatcher / chain steps)
// leave `rule` unset — their contribution flattens into the
// enclosing user rule (src accumulates; kids extend).
type AstNode = {
  rule?: string
  src: string
  kids: AstNode[]
}

function mkAstNode(ruleName: string, nodeKind: Production['nodeKind']): AstNode {
  return nodeKind === 'user'
    ? { rule: ruleName, src: '', kids: [] }
    : { src: '', kids: [] }
}


// A stable, human-predictable "mark" for an alternative — its leading
// discriminator: the first matched token name (sans `#`), the pushed
// rule name, or `_` for the empty alt. Used for `@<rule>:o|c:<mark>`
// user-action references. See docs/design/alt-action-refs.md §3.
function altDiscriminator(
  alt: Sequence,
  literals: Map<string, string>,
  regexTokens: Map<string, string>,
): string {
  if (alt.length === 0) return '_'
  const el = alt[0]
  if (el.kind === 'term') {
    return (literals.get(termKey(el)) || '').replace(/^#/, '') || '_'
  }
  if (el.kind === 'regex') {
    return (regexTokens.get(regexKey(el)) || '').replace(/^#/, '') || '_'
  }
  if (el.kind === 'token') return el.name.replace(/^#/, '') || '_'
  if (el.kind === 'ref') return el.name
  return '_'
}

// Assign a unique mark per source alternative (same alt object → same
// mark, so fan-out copies share it). Collisions get a `~N` suffix.
function assignMarks(
  alts: Sequence[],
  literals: Map<string, string>,
  regexTokens: Map<string, string>,
): Map<Sequence, string> {
  const marks = new Map<Sequence, string>()
  const seen = new Map<string, number>()
  for (const alt of alts) {
    const base = altDiscriminator(alt, literals, regexTokens)
    const n = (seen.get(base) || 0) + 1
    seen.set(base, n)
    marks.set(alt, n === 1 ? base : `${base}~${n}`)
  }
  return marks
}


function segmentToAlt(
  seg: Segment,
  tag: string,
  refs: RefRegistry,
  initNode: boolean,
  ruleName: string,
  nodeKind: Production['nodeKind'],
): any {
  const spec: any = { g: tag }
  if (seg.terms.length > 0) spec.s = seg.terms.join(' ')
  if (seg.ref) spec.p = seg.ref
  // Suffix-debt bookkeeping rides on the alt that does the push, so the
  // child inherits the updated counter: the engine applies `n` before
  // it copies counters into the pushed rule.
  if (seg.debt) spec.n = { ...seg.debt }

  // Default tree-building: accumulate each matched terminal's source
  // text into `r.node.src`. Head alts also allocate a fresh AST node
  // so the child doesn't inherit (and then mutate) its parent's.
  const nterms = seg.terms.length
  if (nterms > 0 || initNode) {
    Object.assign(spec, refs.node(
      { init: initNode, rule: ruleName, kind: nodeKind, nterms },
      (r: Rule) => {
        if (initNode) r.node = mkAstNode(ruleName, nodeKind)
        const n = r.node as AstNode
        for (let i = 0; i < nterms; i++) n.src += r.o[i].src
      }))
  }
  return spec
}


// Close-state action: merge the just-returned child rule's AST node
// into the current rule's. Tagged children (user rules) get pushed
// verbatim into `kids`; untagged (helper / core) flatten — their
// `src` appends and their `kids` extend. Either way `src`
// concatenates so every ancestor's `.src` reflects everything it
// matched.
function captureChildFields(
  refs: RefRegistry,
  ruleName: string,
  nodeKind: Production['nodeKind'],
): { a: any; k?: any } {
  return refs.capture({ rule: ruleName, kind: nodeKind }, (r: Rule) => {
    if (r.node == null) r.node = mkAstNode(ruleName, nodeKind)
    const n = r.node as AstNode
    const c = r.child && r.child.node as AstNode | undefined
    if (c == null) return
    if (typeof c !== 'object' || !('src' in c)) {
      // Legacy shape — wrap as a leaf kid.
      n.kids.push(c as any)
      return
    }
    // Defensive: if the child somehow shares this rule's node
    // object, skip the merge rather than push a self-reference. (A
    // properly-emitted grammar always allocates fresh child nodes.)
    if (c === n) return
    n.src += c.src
    if (c.rule) n.kids.push(c)
    else if (Array.isArray(c.kids)) n.kids.push(...c.kids)
  })
}


// Emit a production marked by `rewriteTailRepeats`:
//
//   open:  [ { s: prefix,  node$ init } ]
//   close: [ { s: sep, r: SELF, fold$ cN } , { fold$ } ]
//
// The same shape a hand-written tabnas grammar uses for `X = a [ b X ]`.
// Every iteration folds itself into the parent (see mkFoldClosure /
// `@fold$`); marks land on the close alts too, so `@X:c:<sep>` and
// `@X:c:_` user actions can attach.
function emitTailRepeat(
  prod: Production,
  literals: Map<string, string>,
  regexTokens: Map<string, string>,
  tag: string,
  ruleSpec: NonNullable<GrammarSpec['rule']>,
  refs: RefRegistry,
) {
  const prodKind = prod.nodeKind ?? 'user'
  const prefixAlt = prod.alts[0]
  const sep = prod.tailRepeat!.sep

  // All-terminal sequences (guaranteed by the rewrite's guards), so
  // each segmentizes to exactly one ref-free segment.
  const prefixSeg = segmentize(prefixAlt, literals, regexTokens)[0]
  const sepSeg = segmentize(sep, literals, regexTokens)[0]

  const marks = (prodKind === 'user' && refs.emitMarks)
    ? assignMarks([prefixAlt, sep], literals, regexTokens)
    : null

  const open = segmentToAlt(prefixSeg, tag, refs, true, prod.name, prodKind)
  if (marks) open.m = marks.get(prefixAlt)

  const repeat: any = {
    s: sepSeg.terms.join(' '),
    r: prod.name,
    ...refs.fold({ cN: sepSeg.terms.length },
      mkFoldClosure(sepSeg.terms.length)),
    // The separator continuation of a repetition — the `,` of a comma
    // list, whatever the grammar spells it as. Recovering here drops one
    // bad item and keeps the rest of the list, which is the single most
    // useful resync point a list grammar has. Only the separator's FIRST
    // token becomes the sync point; a multi-token separator syncs on its
    // leading token.
    g: syncG(tag, 'comma'),
  }
  if (marks) repeat.m = marks.get(sep)

  const end: any = {
    ...refs.fold({}, mkFoldClosure(0)),
    g: tag,
  }
  if (marks) end.m = '_'

  ruleSpec[prod.name] = { open: [open], close: [repeat, end] }
}


function emitProduction(
  prod: Production,
  grammar: Grammar,
  literals: Map<string, string>,
  regexTokens: Map<string, string>,
  knownRules: Set<string>,
  tag: string,
  ruleSpec: NonNullable<GrammarSpec['rule']>,
  firstSets: Map<string, Set<string>>,
  nullable: Set<string>,
  refs: RefRegistry,
  followSets: Map<string, Set<string>>,
  followPairs: Map<string, Map<string, Set<string>>>,
  tokenRangesOf: (tok: string) => Array<[number, number]> | null,
  tokensOverlap: (a: string, b: string) => boolean,
  prov?: Map<string, string>,
) {
  for (const alt of prod.alts) {
    validateRefs(alt, knownRules, prod.name)
  }

  // FOLLOW₂ exit guards for a CONTESTED repetition — one whose repeated
  // element covers a follow token at the character level, so that at
  // that character both continuing the loop and exiting are locally
  // viable (`ws = *[ \t\n]` before the literal "\n"). The 2-token guard
  // writes the decision down: exit exactly when the follow token is
  // followed by something only the exit path can accept. Ordered BEFORE
  // the continue alternatives; under the engine's negotiated lexing the
  // guard can re-cut the character to the follow token's identity, and
  // a failed guard leaves the loop's own alternatives to re-cut it
  // back. Without negotiated lexing the class matcher wins the first
  // cut and the guards are inert, which is what keeps this safe for
  // notations that never contest.
  const pairExitGuards = (baseO: any): any[] => {
    if (!prod.repeatHelper) return []
    const pairs = followPairs.get(prod.name)
    if (null == pairs || 0 === pairs.size) return []
    const contFirst = firstSets.get(prod.name) ?? new Set<string>()
    const out: any[] = []
    const seen = new Set<string>()
    for (const [t, us] of pairs) {
      if (0 === us.size) continue
      if (null == tokenRangesOf(t)) continue
      let contested = false
      for (const f of contFirst) {
        if (f === t) continue
        if (tokensOverlap(t, f)) {
          contested = true
          break
        }
      }
      if (!contested) continue
      for (const u of us) {
        const s = t + ' ' + u
        if (seen.has(s)) continue
        seen.add(s)
        out.push({ ...baseO, s, b: 2 })
      }
    }
    return out
  }

  // Keyword-shadow reordering. A dispatch list built in grammar order
  // puts a character-class alternative (`identifier`) ahead of
  // literal-keyword alternatives (`"while" …`) whenever the grammar
  // listed them that way — and a scannerless lexer cuts `w` as the
  // class token first, so the class alternative wins the dispatch and
  // the keyword alternative is unreachable (`while(…)` dies inside
  // `identifier ws …`). The symmetric problem when the literal comes
  // FIRST: under negotiated lexing it re-cuts `intx` to `int` and
  // steals the identifier. So every literal-headed dispatch entry
  // contested by a class-headed entry gets 2-token guards (`{s:
  // '#WHILE #T1', b: 2}` — the keyword plus a token only the keyword
  // alternative can follow it with) placed ahead of the first
  // contesting class entry, while its 1-token original drops behind
  // the class entries so it can no longer steal; entries that already
  // carry multi-token prefixes simply move ahead. Without negotiated
  // lexing the class matcher wins the first cut and the guards never
  // match, so tokenising notations keep their exact prior behavior.
  const synthKeywordGuards = (
    o: any, alt: Sequence, f: string, consumed: number,
  ): any[] | null => {
    const paths = altPrefixesRaw(alt, grammar, literals, regexTokens, 2)
    const seconds = new Set<string>()
    for (const p of paths) {
      if (p.tokens[0] !== f) continue
      if (2 <= p.tokens.length) { seconds.add(p.tokens[1]); continue }
      // The literal can end the alternative (or the prefix was cut
      // short by a cycle): the second token is whatever may follow
      // the production. An unknown FOLLOW means no guard — and then
      // no reordering either.
      const fol = followSets.get(prod.name)
      if (null == fol || 0 === fol.size) return null
      for (const t of fol) seconds.add(t)
    }
    if (0 === seconds.size || 16 < seconds.size) return null
    return [...seconds].map((u) => ({ ...o, s: f + ' ' + u, b: 2 - consumed }))
  }

  const reorderKeywordShadow = (
    entries: Array<{ o: any; alt: Sequence | null }>,
  ): any[] => {
    const litToks = new Set(literals.values())
    const classToks = new Set(regexTokens.values())

    // Head token and lookahead length, resolved ONCE per entry. A
    // dispatch list can hold hundreds of entries whose `s` is a
    // four-token prefix, and this loop is quadratic in them — splitting
    // those strings per comparison dominated compile time (a 332-rule
    // grammar took a minute).
    const N = entries.length
    const heads: Array<string | null> = new Array(N)
    const sLens: number[] = new Array(N)
    for (let i = 0; i < N; i++) {
      const s = entries[i].o.s
      if ('string' !== typeof s || 0 === s.length) {
        heads[i] = null
        sLens[i] = 0
        continue
      }
      const sp = s.indexOf(' ')
      heads[i] = -1 === sp ? s : s.substring(0, sp)
      let n = 1
      for (let k = 0; k < s.length; k++) if (32 === s.charCodeAt(k)) n++
      sLens[i] = n
    }

    // Class-headed entries, with their character coverage resolved once.
    const classIdx: number[] = []
    const classRanges: Array<Array<[number, number]>> = []
    for (let i = 0; i < N; i++) {
      const f = heads[i]
      if (null == f || !classToks.has(f)) continue
      const r = tokenRangesOf(f)
      if (null == r) continue
      classIdx.push(i)
      classRanges.push(r)
    }
    if (0 === classIdx.length) return entries.map((e) => e.o)

    type Placed = { o: any; rank: number; seq: number }
    const placed: Placed[] = []
    let seq = 0
    const put = (o: any, rank: number) => placed.push({ o, rank, seq: seq++ })

    // Which class entries a given literal head contests, decided once
    // per distinct head token. Entries repeat their head heavily (one
    // per lookahead prefix of the same alternative), and the scan below
    // is quadratic, so without this the coverage test runs on every
    // pair. A literal head is never itself a class head — the two token
    // name sets are disjoint — so `c === i` cannot arise here.
    const contestsByHead = new Map<string, Uint8Array>()

    for (let i = 0; i < N; i++) {
      const { o, alt } = entries[i]
      const f = heads[i]
      const fr = null != f && litToks.has(f) ? tokenRangesOf(f) : null

      // First and last contesting class entry, in one pass.
      let firstC = -1
      let lastC = -1
      if (null != fr && null != alt) {
        let hits = contestsByHead.get(f as string)
        if (undefined === hits) {
          hits = new Uint8Array(classIdx.length)
          for (let k = 0; k < classIdx.length; k++) {
            hits[k] = charRangesOverlap(fr, classRanges[k]) ? 1 : 0
          }
          contestsByHead.set(f as string, hits)
        }
        for (let k = 0; k < classIdx.length; k++) {
          if (0 === hits[k]) continue
          const c = classIdx[k]
          // Same descent target either way — order is moot.
          if (null != o.p && entries[c].o.p === o.p) continue
          if (-1 === firstC) firstC = c
          lastC = c
        }
      }

      if (-1 === firstC) { put(o, i); continue }
      const front = firstC - 0.5
      const back = lastC + 0.5

      if (2 <= sLens[i]) {
        // Already carries its own lookahead — just outrank the class.
        put(o, Math.min(i, front))
        continue
      }

      const consumed = 1 - (o.b ?? 0)
      const guards = (0 === consumed || 1 === consumed)
        ? synthKeywordGuards(o, alt as Sequence, f as string, consumed)
        : null
      if (null == guards) { put(o, i); continue }
      for (const g of guards) put(g, Math.min(i, front))
      put(o, Math.max(i, back))
    }

    return placed
      .sort((a, b) => a.rank - b.rank || a.seq - b.seq)
      .map((p) => p.o)
  }

  // Specificity ordering among contested class heads. llama.cpp's
  // schema converter loves `[0-9] | [1] [0-9] | [2] [0-3]` (a bounded
  // integer): the 1-token alternative is listed first and, matching
  // any digit, shadows the 2-token ones — `23` dies after `2`. Among
  // entries whose class heads overlap at the character level and whose
  // descents differ, longer lookahead goes first (maximal munch): the
  // longer entry only matches where its full prefix does, and a failed
  // longer entry still falls through to the shorter one. Without
  // negotiated lexing a token's single identity picks the same entry
  // in either order, so tokenising notations are unaffected. Entries
  // are permuted among their own slots so everything else stays put.
  const specificityPermute = (
    entries: Array<{ o: any; alt: Sequence | null }>,
  ): void => {
    const classToks = new Set(regexTokens.values())
    // Head token and lookahead length once per entry — the loop below
    // is quadratic, and re-splitting multi-token `s` strings inside it
    // is what made large grammars slow.
    const N = entries.length
    const sLens: number[] = new Array(N)
    const heads: Array<string | null> = new Array(N)
    for (let i = 0; i < N; i++) {
      const { o, alt } = entries[i]
      const s = o.s
      if ('string' !== typeof s || 0 === s.length) {
        heads[i] = null
        sLens[i] = 0
        continue
      }
      let n = 1
      for (let k = 0; k < s.length; k++) if (32 === s.charCodeAt(k)) n++
      sLens[i] = n
      if (null == alt) { heads[i] = null; continue }
      const sp = s.indexOf(' ')
      const f = -1 === sp ? s : s.substring(0, sp)
      heads[i] = classToks.has(f) ? f : null
    }
    // Coverage per candidate head, resolved once.
    const ranges: Array<Array<[number, number]> | null> = new Array(N)
    for (let i = 0; i < N; i++) {
      ranges[i] = null == heads[i] ? null : tokenRangesOf(heads[i] as string)
    }
    const idxs: number[] = []
    for (let i = 0; i < N; i++) {
      const ri = ranges[i]
      if (null == ri) continue
      for (let j = 0; j < N; j++) {
        if (j === i || null == ranges[j]) continue
        // Same descent target: the order between them is moot. Only
        // when a descent EXISTS, though — terminal-only alternatives
        // all carry `p: undefined`, and reading those as "same target"
        // excludes the whole rule from the permutation, so
        // `[0-9] / [2] [0-3]` keeps its 1-token entry first and
        // misparses `23`.
        if (null != entries[i].o.p && entries[j].o.p === entries[i].o.p) {
          continue
        }
        if (charRangesOverlap(ri, ranges[j] as Array<[number, number]>)) {
          idxs.push(i)
          break
        }
      }
    }
    if (idxs.length < 2) return
    // How much the alternative behind an entry can consume in total.
    // Two contested entries can carry the SAME lookahead length when
    // the prefix walk was truncated by a descent (`[0-9]` beside
    // `[1-9] [0-9]{0,15}`, both fanning out to one token), and then
    // lookahead alone cannot rank them. The longer alternative is the
    // more specific one, so it goes first — maximal munch again, one
    // level up.
    // Computed once per contested entry, not inside the comparator.
    const spans = new Map<number, number>()
    for (const i of idxs) {
      const alt = entries[i].alt
      const n = null == alt ? 0 : seqTokenSpan(alt, grammar, new Set())
      spans.set(i, Number.isFinite(n) ? n : 1e9)
    }
    const sorted = idxs
      .slice()
      .sort((a, b) =>
        sLens[b] - sLens[a] ||
        (spans.get(b) as number) - (spans.get(a) as number))
      .map((i) => entries[i])
    idxs.forEach((slot, k) => { entries[slot] = sorted[k] })
  }

  // True for a repetition helper whose content can start with a token
  // its FOLLOW also contains (`( "," space b-kv )? ( "," space c-kv )?`
  // — both sides open with the comma). One token can never decide
  // continue-vs-exit there; K-token prefixes on the continue side let
  // a failed deep match fall through to the exit peeks instead of
  // committing.
  const contestedByFollow = (alt: Sequence): boolean => {
    if (!prod.repeatHelper) return false
    const mine = firstOfAlt(alt, literals, regexTokens, firstSets, nullable)
    if (null == mine) return false
    const fol = followSets.get(prod.name) ?? new Set<string>()
    for (const t of mine) {
      for (const f of fol) {
        if (f === t) return true
        if (tokensOverlap(t, f)) return true
      }
    }
    return false
  }

  // True when this alternative's first tokens overlap another
  // alternative's at the character level with a different descent —
  // the condition under which a 1-token FIRST peek cannot pick the
  // right alternative and K-token prefixes are worth their weight.
  const altHeadContested = (
    alt: Sequence,
    all: Sequence[],
  ): boolean => {
    const mine = firstOfAlt(alt, literals, regexTokens, firstSets, nullable)
    if (null == mine) return false
    for (const other of all) {
      if (other === alt || 0 === other.length) continue
      const theirs = firstOfAlt(
        other, literals, regexTokens, firstSets, nullable)
      if (null == theirs) continue
      for (const t of mine) {
        for (const u of theirs) {
          if (tokensOverlap(t, u)) return true
        }
      }
    }
    return false
  }

  // Suffix-debt guard for a contested left-recursion tail loop: a branch
  // that would eat a token an enclosing frame still owes may only run
  // while the debt is zero. Applies to the continue alternatives (`alt`
  // non-empty) and never to the exit peeks or the bare fallback, which
  // is what lets the loop yield rather than fail.
  //
  // Only the branches whose head token is contested are guarded. A loop
  // built from several tails repeats several tokens, and the ones the
  // suffix does not compete for must stay open at any debt — otherwise
  // `A = A "y" / A "w" / "x" A "y" / "z"` rejects `xzwy`, where the
  // inner A must consume the `w` before yielding the `y`.
  // See `resolveSuffixDebts`.
  const applyDebtGuard = (
    list: Array<{ o: any; alt: Sequence | null }>,
  ): void => {
    if (null == prod.debtGuard || null == prod.debtOwed) return
    const owed = new Set(prod.debtOwed)
    // Scalar shorthand for `$eq`, which both runtimes accept — the Go
    // engine's declarative form takes an int or a `CondOp`, not a nested
    // operator object, so this is the one spelling that is shape-identical
    // in each.
    const c = { ['n.' + prod.debtGuard]: 0 }
    for (const e of list) {
      if (null == e.alt || 0 === e.alt.length) continue
      // Entries are keyed by the token sequence they peek, so the head
      // token says which branch this is. A continue alternative always
      // names one; if it somehow does not, guard it — that is the
      // direction that keeps the loop from starving its parent.
      const s = e.o.s
      const head = 'string' === typeof s ? s.split(' ')[0] : null
      if (null != head && !owed.has(head)) continue
      e.o.c = c
    }
  }

  if (prod.tailRepeat) {
    emitTailRepeat(prod, literals, regexTokens, tag, ruleSpec, refs)
    return
  }

  const allSimple = prod.alts.every(isSingleSegment)

  if (allSimple) {
    // Every alternative collapses to one tabnas alt — emit them
    // directly into the production's open state. This is a head
    // rule, so each alt initialises its own node array. Empty alts
    // are sorted to the end so tabnas's first-match-wins doesn't let
    // them short-circuit non-empty alternatives.
    const ordered = [
      ...prod.alts.filter((alt) => alt.length > 0),
      ...prod.alts.filter((alt) => alt.length === 0),
    ]

    // Ref-only alternatives have no terminal to discriminate on, so
    // tabnas's first-match-wins would silently let them shadow any
    // later alternative. Guard them with FIRST-set peeks when the
    // production has more than one alt.
    const prodKind = prod.nodeKind ?? 'user'
    const marks = (prodKind === 'user' && refs.emitMarks)
      ? assignMarks(ordered, literals, regexTokens)
      : null
    const needsPeek = ordered.length > 1
    const entries: Array<{ o: any; alt: Sequence | null }> = []
    for (const alt of ordered) {
      const segs = segmentize(alt, literals, regexTokens)
      const seg = segs[0]
      const isRefOnly = alt.length >= 1 &&
        alt.every((el) => el.kind === 'ref') &&
        seg.terms.length === 0 &&
        seg.ref != null

      const mark = marks ? marks.get(alt) : undefined
      if (needsPeek && isRefOnly) {
        const firstTokens = firstOfAlt(
          alt, literals, regexTokens, firstSets, nullable)
        if (firstTokens) {
          const nodeFields = refs.node(
            { init: true, rule: prod.name, kind: prodKind, nterms: 0 },
            (r: Rule) => { r.node = mkAstNode(prod.name, prodKind) })
          // A contested head cannot be decided by one token — fan out
          // to K-token prefixes (bounded, deduped; bail to the 1-token
          // peek if the fan-out is degenerate) so the specificity
          // ordering has lookahead to work with.
          let paths: string[][] | null = null
          if (altHeadContested(alt, ordered) || contestedByFollow(alt)) {
            const pfx = altPrefixes(
              alt, grammar, literals, regexTokens, LOOKAHEAD_K)
              .filter((p) => 0 < p.length)
            if (0 < pfx.length && pfx.length <= 64) paths = pfx
          }
          // This path builds the push alt by hand rather than through
          // `segmentToAlt`, so it has to carry the same suffix-debt
          // bookkeeping. (An annotated reference always has a mandatory
          // suffix behind it, which makes its alternative multi-segment
          // and keeps it out of this branch — but a guard that depends
          // on where the emitter happens to route an alt is a guard
          // waiting to go quiet.)
          const debt = seg.debt ? { n: { ...seg.debt } } : null
          if (paths) {
            for (const p of paths) {
              const o: any = {
                s: p.join(' '),
                b: p.length,
                p: seg.ref,
                ...debt,
                ...nodeFields,
                g: tag,
              }
              if (mark) o.m = mark
              entries.push({ o, alt })
            }
          } else {
            for (const tok of firstTokens) {
              const o: any = {
                s: tok,
                b: 1,
                p: seg.ref,
                ...debt,
                ...nodeFields,
                g: tag,
              }
              if (mark) o.m = mark
              entries.push({ o, alt })
            }
          }
          continue
        }
      }
      const o = segmentToAlt(seg, tag, refs, true, prod.name, prodKind)
      if (mark) o.m = mark
      // The terminating alternative of a repetition helper names no
      // token, so the lexer is never asked to produce whatever follows
      // the repetition. Re-issue that alternative once per FOLLOW
      // token, peeking and pushing straight back (`b: 1`) so the token
      // column widens without anything extra being consumed. The bare
      // alternative stays last as the fallback.
      if (alt.length === 0 && prod.repeatHelper) {
        for (const tok of followSets.get(prod.name) ?? []) {
          entries.push({ o: { ...o, s: tok, b: 1 }, alt: null })
        }
        // Contested repetitions additionally get FOLLOW₂ guards, at
        // the FRONT so they outrank the continue alternatives.
        entries.unshift(
          ...pairExitGuards(o).map((g: any) => ({ o: g, alt: null })))
      }
      entries.push({ o, alt: 0 < alt.length ? alt : null })
    }

    applyDebtGuard(entries)
    specificityPermute(entries)
    const rs: any = { open: reorderKeywordShadow(entries) }

    // If any alt has a push, the close state must capture the
    // returned child. Add a universal fallback close alt whose
    // action is a no-op when there was no push.
    if (prod.alts.some((alt) => alt.some((el) => el.kind === 'ref'))) {
      const close: any = {
        ...captureChildFields(refs, prod.name, prod.nodeKind ?? 'user'),
        g: tag,
      }
      if (marks) close.m = '_'
      rs.close = [close]
    }
    ruleSpec[prod.name] = rs
    return
  }

  if (prod.alts.length === 1) {
    // Single-alt, multi-segment: chain rules directly on the
    // production.
    emitChain(prod.name, prod.alts[0], literals, regexTokens, tag,
      ruleSpec, refs, prod.nodeKind ?? 'user', prov, originOf(prod))
    return
  }

  // Multi-alt with at least one multi-segment alternative: emit a
  // dispatcher. Each alt becomes its own chained impl rule
  // (`<prodname>$alt<i>`); the main rule's open peeks the first token
  // and pushes the matching impl rule. Using `p:` (not `r:`) keeps
  // the parent's `child` pointer valid so the parent can read the
  // impl's node in its close-state action.
  const dispatchEntries: Array<{ o: any; alt: Sequence | null }> = []
  let emptyAltSeen = false
  const nullableImpls: Array<{ implName: string; fields: any; mark?: string }> =
    []
  const dispatchMarks = ((prod.nodeKind ?? 'user') === 'user' && refs.emitMarks)
    ? assignMarks(prod.alts, literals, regexTokens)
    : null

  for (let i = 0; i < prod.alts.length; i++) {
    const alt = prod.alts[i]
    const implName = `${prod.name}$alt${i}`
    const mark = dispatchMarks ? dispatchMarks.get(alt) : undefined

    if (alt.length === 0) {
      // Empty alt acts as fallback — handled after the loop.
      emptyAltSeen = true
      continue
    }

    // One impl rule per alternative of a multi-segment dispatch: the
    // author wrote one rule with alternatives, not N rules. Recorded
    // here, beside the emission, because an EMPTY alternative returns
    // above without emitting anything — claiming a rule that does not
    // exist is worse than omitting one that does.
    if (null != prov) prov.set(implName, originOf(prod))

    emitChain(implName, alt, literals, regexTokens, tag, ruleSpec, refs,
      'helper', prov, originOf(prod))

    // Fan out this alt into one dispatch entry per concrete token
    // sequence it can start with. Up to LOOKAHEAD_K tokens per
    // prefix is enough for the grammars this converter targets; a
    // ref with multiple alts produces one prefix per sub-alt so
    // overlapping FIRST sets between competing alts can still be
    // separated by their second (or later) token.
    // The dispatcher itself is a user (or helper) rule — it must
    // allocate its own AST node on every dispatch alt, otherwise the
    // node inherited from the parent via makeRule(ctx, rule.node)
    // would be shared and the dispatcher's captureChildRef would
    // mutate the parent's tree.
    const dispatchKind = prod.nodeKind ?? 'user'
    const initDispatchFields = refs.node(
      { init: true, rule: prod.name, kind: dispatchKind, nterms: 0 },
      (r: Rule) => { r.node = mkAstNode(prod.name, dispatchKind) })

    const rawPaths = altPrefixesRaw(
      alt, grammar, literals, regexTokens, LOOKAHEAD_K)
    // An alternative that can derive ε (all elements nullable — a
    // complete zero-token path, not a cycle truncation) loses that
    // derivation in the `usable` filter below. Remember it: after the
    // loop it is re-issued as FOLLOW-guarded entries plus a bare
    // fallback, ordered after every content entry so an ε-derivation
    // never preempts a real match.
    if (rawPaths.some((p) => 0 === p.tokens.length && !p.done)) {
      nullableImpls.push({ implName, fields: initDispatchFields, mark })
    }
    const prefixes = altPrefixes(
      alt, grammar, literals, regexTokens, LOOKAHEAD_K)
    const usable = prefixes.filter((p) => p.length > 0)
    if (usable.length > 0) {
      for (const p of usable) {
        const o: any = {
          s: p.join(' '),
          b: p.length,
          p: implName,
          ...initDispatchFields,
          g: tag,
        }
        if (mark) o.m = mark
        dispatchEntries.push({ o, alt })
      }
    } else {
      const firstTokens = firstOfAlt(
        alt, literals, regexTokens, firstSets, nullable)
      if (firstTokens === null) {
        throw new Error(
          `${diagName()}: rule '${prod.name}' alternative ${i} is nullable ` +
          `but is not the only empty alt; FIRST set is ambiguous`)
      }
      for (const tok of firstTokens) {
        const o: any = {
          s: tok, b: 1, p: implName, ...initDispatchFields, g: tag,
        }
        if (mark) o.m = mark
        dispatchEntries.push({ o, alt })
      }
    }
  }

  // Re-issue each nullable alternative's ε-derivation: FOLLOW peeks
  // first (they name the follow token, so the lexer offers it at this
  // position), then one unguarded fallback that pushes the impl with
  // nothing consumed. Everything here ranks after all content entries.
  for (const n of nullableImpls) {
    for (const tok of followSets.get(prod.name) ?? []) {
      const o: any = { s: tok, b: 1, p: n.implName, ...n.fields, g: tag }
      if (n.mark) o.m = n.mark
      dispatchEntries.push({ o, alt: null })
    }
    const o: any = { p: n.implName, ...n.fields, g: tag }
    if (n.mark) o.m = n.mark
    dispatchEntries.push({ o, alt: null })
  }

  if (emptyAltSeen) {
    // Fallback: matches any token (or none), pops immediately with
    // an empty tree. Tagged with the user rule name so a consumer
    // walking the tree still gets a placeholder node for the empty
    // alternative.
    const fallbackKind = prod.nodeKind ?? 'user'
    const o: any = {
      ...refs.node(
        { init: true, rule: prod.name, kind: fallbackKind, nterms: 0 },
        (r: Rule) => { r.node = mkAstNode(prod.name, fallbackKind) }),
      g: tag,
    }
    if (dispatchMarks) o.m = '_'
    // Same FOLLOW guard as the single-segment path above.
    if (prod.repeatHelper) {
      for (const tok of followSets.get(prod.name) ?? []) {
        dispatchEntries.push({ o: { ...o, s: tok, b: 1 }, alt: null })
      }
      // Contested repetitions additionally get FOLLOW₂ guards, at the
      // FRONT so they outrank the continue alternatives.
      dispatchEntries.unshift(
        ...pairExitGuards(o).map((g: any) => ({ o: g, alt: null })))
    }
    dispatchEntries.push({ o, alt: null })
  }

  const dispClose: any = {
    // Merge the chosen impl's result up into the dispatcher's node,
    // tagged with the user rule name (so the enclosing rule sees a
    // `{rule, src, kids}` child, not the impl chain's transparent
    // `{src, kids}`).
    ...captureChildFields(refs, prod.name, prod.nodeKind ?? 'user'),
    g: tag,
  }
  if (dispatchMarks) dispClose.m = '_'
  applyDebtGuard(dispatchEntries)
  specificityPermute(dispatchEntries)
  ruleSpec[prod.name] = {
    open: reorderKeywordShadow(dispatchEntries),
    close: [dispClose],
  }
}


// Emit a (possibly single-step) chain of rules for one alt under the
// given head rule name. Segment 0 goes into `headName`; later
// segments get synthetic `<headName>$stepN` continuations.
//
// `headKind` controls the head rule's AST node shape: 'user' tags
// the head's node with the rule name; 'helper' leaves it untagged
// (transparent to the enclosing user rule). Step rules are always
// helpers — they inherit and accumulate into the head's node via
// `r:` replacement.
function emitChain(
  headName: string,
  alt: Sequence,
  literals: Map<string, string>,
  regexTokens: Map<string, string>,
  tag: string,
  ruleSpec: NonNullable<GrammarSpec['rule']>,
  refs: RefRegistry,
  headKind: Production['nodeKind'] = 'helper',
  prov?: Map<string, string>,
  origin?: string,
) {
  const segs = segmentize(alt, literals, regexTokens)
  const chainName = (i: number) =>
    i === 0 ? headName : `${headName}$step${i}`

  for (let i = 0; i < segs.length; i++) {
    const name = chainName(i)
    const seg = segs[i]
    const kind = i === 0 ? headKind : 'helper'
    // Only the head of the chain initialises the node object; later
    // steps inherit and continue to accumulate into it via `r:`.
    const headAlt = segmentToAlt(seg, tag, refs, i === 0, name, kind)
    // Single-alt user rule: the head alt is user-addressable.
    if (i === 0 && headKind === 'user' && refs.emitMarks) {
      headAlt.m = altDiscriminator(alt, literals, regexTokens)
    }
    const open = [headAlt]
    const rs: any = { open }

    // Step rules exist only because the alternative had more than one
    // segment; nothing in the author's grammar is named after them.
    if (0 < i && null != prov && null != origin) prov.set(name, origin)

    const isLast = i === segs.length - 1
    if (!isLast) {
      // Non-last step: after the push returns, capture the child's
      // node and replace with the next step rule.
      rs.close = [{
        r: chainName(i + 1),
        ...captureChildFields(refs, name, kind),
        g: tag,
      }]
    } else if (seg.ref) {
      // Last step, but it had a push — we still need to capture the
      // final child before popping.
      rs.close = [{ ...captureChildFields(refs, name, kind), g: tag }]
    }
    ruleSpec[name] = rs
  }
}


// Compute FIRST(ref) for every production, plus which productions
// are nullable (can derive the empty string). Iterates to a fixed
// point. Terminals in FIRST sets are represented by their allocated
// token names (e.g. `#X`).
function computeFirstSets(
  grammar: Grammar,
  literals: Map<string, string>,
  regexTokens: Map<string, string>,
): { firstSets: Map<string, Set<string>>; nullable: Set<string> } {
  const firstSets = new Map<string, Set<string>>()
  const nullable = new Set<string>()
  for (const p of grammar.productions) firstSets.set(p.name, new Set())

  let changed = true
  while (changed) {
    changed = false
    for (const prod of grammar.productions) {
      const first = firstSets.get(prod.name) as Set<string>
      for (const alt of prod.alts) {
        // Walk the alt, accumulating FIRST until a non-nullable
        // position is hit.
        let altNullable = true
        for (const el of alt) {
          if (el.kind === 'term' || el.kind === 'regex' ||
              el.kind === 'token') {
            const tok = el.kind === 'term'
              ? literals.get(termKey(el)) as string
              : el.kind === 'token'
                ? el.name
                : regexTokens.get(regexKey(el)) as string
            if (!first.has(tok)) { first.add(tok); changed = true }
            altNullable = false
            break
          }
          if (el.kind === 'ref') {
            const refFirst = firstSets.get(el.name) ?? new Set<string>()
            for (const tok of refFirst) {
              if (!first.has(tok)) { first.add(tok); changed = true }
            }
            if (!nullable.has(el.name)) {
              altNullable = false
              break
            }
            continue
          }
          // Desugar should have eliminated other kinds.
          throw new Error(`${diagName()}: internal — unexpected kind in FIRST: ${el.kind}`)
        }
        if (altNullable && !nullable.has(prod.name)) {
          nullable.add(prod.name)
          changed = true
        }
      }
    }
  }

  return { firstSets, nullable }
}


// -- Suffix debt: contested left-recursion tail loops ----------------
//
// `eliminateDirectLeftRec` rewrites `A = ["x"] A "y" / "z"` into
//
//   A = ( "x" A "y" | "z" ) "y"*
//
// which is a correct CFG and a broken parser. Parsing `xzy` needs the
// inner A's tail loop to match ZERO `"y"`s, so the enclosing
// `"x" A "y"` has one left to consume; the loop is greedy, eats it, and
// the outer alternative starves. Widening the loop's lookahead cannot
// help, because the two cases it must separate are indistinguishable
// through any token window:
//
//   input | remaining at the decision | required
//   ------|--------------------------|-------------------------------
//   xzy   | #Y #ZZ                   | exit — `"x" A "y"` owes a #Y
//   zy    | #Y #ZZ                   | continue — nothing owes a #Y
//
// Same rule, same tokens, opposite answers. What differs is how many
// enclosing frames have committed to consuming a `"y"` once the current
// subtree returns — stack depth, not a token window. The engine already
// propagates exactly that kind of state: `n` counters flow from a rule
// to every rule it pushes, and never back up.
//
// So count the debt. For each contested loop, on every push that can
// reach the recursive rule:
//
//   suffix after the push          | counter
//   -------------------------------|------------------------------------
//   empty, or can derive ε         | inherited (the push is in tail
//                                  | position; the ancestor's debt stands)
//   mandatory, FIRST hits the loop | +1 — this frame owes the loop's token
//   mandatory, FIRST disjoint      | 0  — a barrier; the frame re-anchors
//
// and guard the loop branches that could eat what is owed with
// `n.<counter> == 0`. The barrier reset is not an optimisation: in
// `A = ["x"] A "y" / "(" A ")" / "z"` the paren alternative owes a
// `")"`, not a `"y"`, so an A pushed from there must start clean or
// `x(zy)y` cannot parse.
//
// "Could eat what is owed" is per branch, not per loop. A loop built
// from several tails repeats several tokens, and only the ones an
// enclosing suffix competes for may be blocked — guarding the whole
// helper makes `A = A "y" / A "w" / "x" A "y" / "z"` reject `xzwy`,
// where the inner A has to consume the `w` before yielding the `y`. The
// contested tokens are recorded in `debtOwed` for the emitter.
//
// Competition is decided at the character level, not by token identity:
// a fixed `"a"` token and a `[a-z]` match token are different names for
// overlapping input, and reading them as disjoint drops the guard on a
// loop that really does contest the suffix.
//
// The counter needs no explicit decrement. `n` is copied down at push
// time and a parent's own counters are untouched by what its children
// do, so unwinding out of a frame restores that frame's debt by
// construction.
//
// Nothing is emitted unless some push actually owes the loop's token,
// so a grammar whose loop was never contested compiles unchanged.
// Issue #6.
function resolveSuffixDebts(
  grammar: Grammar,
  literals: Map<string, string>,
  regexTokens: Map<string, string>,
  firstSets: Map<string, Set<string>>,
  nullable: Set<string>,
  tokensOverlap: (a: string, b: string) => boolean,
): void {
  const guarded = grammar.productions.filter((p) => null != p.debtGuard)
  if (0 === guarded.length) return

  for (const loop of guarded) {
    const counter = loop.debtGuard as string

    // The recursive rule is whatever references the loop helper.
    // `eliminateDirectLeftRec` emits one such reference and `desugar`
    // mints the helper, so there is exactly one candidate.
    const owner = grammar.productions.find((p) =>
      p !== loop &&
      p.alts.some((alt) =>
        alt.some((el) => el.kind === 'ref' && el.name === loop.name)))

    // FIRST of the helper is FIRST of what it repeats: its other
    // alternative is empty.
    const loopFirst = firstSets.get(loop.name) ?? new Set<string>()
    if (null == owner || 0 === loopFirst.size) {
      delete loop.debtGuard
      continue
    }

    // Only a push into something that can still reach the recursive
    // rule can end up inside the contested loop; everything else is
    // left alone.
    const carries = refCallersOf(grammar, owner.name)

    const pending: Array<{ alt: Sequence; i: number; delta: number }> = []
    // The loop's own tokens that some enclosing suffix competes for.
    const owed = new Set<string>()
    for (const prod of grammar.productions) {
      for (const alt of prod.alts) {
        for (let i = 0; i < alt.length; i++) {
          const el = alt[i]
          if (el.kind !== 'ref' || !carries.has(el.name)) continue
          const suffix = alt.slice(i + 1)
          if (0 === suffix.length) continue
          const f = firstOfSeq(
            suffix, literals, regexTokens, firstSets, nullable)
          // A suffix that can vanish commits the frame to nothing, so
          // the push stays in tail position and inherits.
          if (f.nullable) continue
          // Collect every loop token this suffix competes for, rather
          // than stopping at the first: they are exactly the branches
          // the emitter may block, and the rest must stay open.
          let hits = false
          for (const u of f.tokens) {
            for (const t of loopFirst) {
              if (t === u || tokensOverlap(t, u)) { owed.add(t); hits = true }
            }
          }
          pending.push({ alt, i, delta: hits ? 1 : 0 })
        }
      }
    }

    if (0 === owed.size) {
      // Nothing anywhere competes with this loop — the shape matched
      // syntactically but the tokens never collide. Leave the grammar
      // exactly as it was.
      delete loop.debtGuard
      continue
    }
    loop.debtOwed = [...owed]

    for (const { alt, i, delta } of pending) {
      const el = alt[i] as Extract<Element, { kind: 'ref' }>
      // Replace rather than mutate. Elements are shared between
      // alternatives, and `cloneGrammar` copies only one level deep, so
      // writing through this reference would annotate occurrences this
      // pass never inspected — including in the caller's own grammar.
      alt[i] = { ...el, debt: { ...(el.debt ?? {}), [counter]: delta } }
    }
  }
}


// Names of the productions from which `target` can be reached through
// rule references, `target` itself included. A backward walk: the
// forward closure would cost a traversal per production, and only this
// one node's ancestry is ever asked for.
function refCallersOf(grammar: Grammar, target: string): Set<string> {
  const rev = new Map<string, string[]>()
  for (const p of grammar.productions) {
    const out = new Set<string>()
    for (const alt of p.alts) refsIn(alt, out)
    // A dispatcher's branches live outside `alts`, but a push into one
    // is still a push.
    if (p.probeDispatch) {
      out.add(p.probeDispatch.probeRule)
      out.add(p.probeDispatch.withBranch)
      out.add(p.probeDispatch.noBranch)
    }
    for (const to of out) {
      const from = rev.get(to)
      if (from) from.push(p.name)
      else rev.set(to, [p.name])
    }
  }

  const seen = new Set<string>([target])
  const queue = [target]
  while (0 < queue.length) {
    for (const from of rev.get(queue.pop() as string) ?? []) {
      if (seen.has(from)) continue
      seen.add(from)
      queue.push(from)
    }
  }
  return seen
}


// FIRST of a sequence, reporting nullability separately rather than
// collapsing it to `null` the way `firstOfAlt` does. FOLLOW needs both
// halves of the answer: the tokens a suffix can start with, *and*
// whether that suffix can vanish (in which case the enclosing rule's
// FOLLOW carries through).
function firstOfSeq(
  seq: Sequence,
  literals: Map<string, string>,
  regexTokens: Map<string, string>,
  firstSets: Map<string, Set<string>>,
  nullable: Set<string>,
): { tokens: Set<string>; nullable: boolean } {
  const tokens = new Set<string>()
  for (const el of seq) {
    if (el.kind === 'term' || el.kind === 'regex' || el.kind === 'token') {
      tokens.add(el.kind === 'term'
        ? literals.get(termKey(el)) as string
        : el.kind === 'token'
          ? el.name
          : regexTokens.get(regexKey(el)) as string)
      return { tokens, nullable: false }
    }
    if (el.kind === 'ref') {
      for (const tok of firstSets.get(el.name) ?? []) tokens.add(tok)
      if (!nullable.has(el.name)) return { tokens, nullable: false }
      continue
    }
    throw new Error(
      `${diagName()}: internal — unexpected kind in firstOfSeq: ${el.kind}`)
  }
  return { tokens, nullable: true }
}


// FOLLOW sets — the tokens that may legitimately appear immediately
// after each production.
//
// This exists for one reason: the engine lexes *under the direction of
// the active rule*. A matcher-backed token (a character class) is only
// offered at a position where the current rule names it. A generated
// repetition helper ends on an empty alternative, which names nothing,
// so at the moment the loop could terminate the following token is not
// on offer and the lex fails instead. `root ::= sign? [0-9]+` dies one
// character in for exactly this reason.
//
// Naming FOLLOW on that terminating alternative puts those tokens back
// in the rule's token column. The guard alternatives peek and push the
// token straight back (`b: 1`), so they accept nothing extra — they
// only widen what the lexer is willing to produce there.
function computeFollowSets(
  grammar: Grammar,
  literals: Map<string, string>,
  regexTokens: Map<string, string>,
  firstSets: Map<string, Set<string>>,
  nullable: Set<string>,
  start: string,
): Map<string, Set<string>> {
  const follow = new Map<string, Set<string>>()
  for (const p of grammar.productions) follow.set(p.name, new Set())
  // End-of-source can follow the start rule.
  follow.get(start)?.add('#ZZ')

  let changed = true
  while (changed) {
    changed = false
    for (const prod of grammar.productions) {
      const prodFollow = follow.get(prod.name) as Set<string>
      for (const alt of prod.alts) {
        for (let i = 0; i < alt.length; i++) {
          const el = alt[i]
          if (el.kind !== 'ref') continue
          const target = follow.get(el.name)
          if (!target) continue
          const add = (tok: string) => {
            if (!target.has(tok)) { target.add(tok); changed = true }
          }
          const rest = firstOfSeq(
            alt.slice(i + 1), literals, regexTokens, firstSets, nullable)
          for (const tok of rest.tokens) add(tok)
          // Nothing (or nothing mandatory) follows this reference inside
          // the alternative, so whatever can follow the enclosing
          // production can follow the reference too.
          if (rest.nullable) for (const tok of prodFollow) add(tok)
        }
      }
    }
  }

  return follow
}


// FOLLOW₂ pairs — for each production R, the pairs (t, u) such that R
// may be followed by token t and then token u.
//
// This exists for exactly one decision the 1-token FOLLOW guard cannot
// make: a repetition whose repeated element COVERS a follow token at
// the character level. `ws = *[ \t\n]` followed by the literal "\n" is
// the canonical case — at a newline, continuing the loop and exiting
// are both locally viable, and which is right depends on what comes
// AFTER the newline. The pair (t, u) is what lets the emitter write
// that decision down as an ordered guard.
//
// Deliberately approximate, in one direction only: pairs whose t would
// come from inside a following REFERENCE are not collected (walking
// FIRST₂ through rules needs its own fixpoint), so some contested
// repetitions get no pair guards and keep today's behaviour. Pairs that
// ARE collected are exact.
function computeFollowPairs(
  grammar: Grammar,
  literals: Map<string, string>,
  regexTokens: Map<string, string>,
  firstSets: Map<string, Set<string>>,
  nullable: Set<string>,
  follow: Map<string, Set<string>>,
): Map<string, Map<string, Set<string>>> {
  const pairs = new Map<string, Map<string, Set<string>>>()
  for (const p of grammar.productions) pairs.set(p.name, new Map())

  const tokOf = (el: Element): string | undefined =>
    el.kind === 'term'
      ? literals.get(termKey(el))
      : el.kind === 'token'
        ? el.name
        : el.kind === 'regex'
          ? regexTokens.get(regexKey(el))
          : undefined

  const addPair = (R: string, t: string, u: string): boolean => {
    const m = pairs.get(R)
    if (!m) return false
    let us = m.get(t)
    if (!us) {
      us = new Set()
      m.set(t, us)
    }
    if (us.has(u)) return false
    us.add(u)
    return true
  }

  let changed = true
  while (changed) {
    changed = false
    for (const prod of grammar.productions) {
      const prodFollow = follow.get(prod.name) ?? new Set<string>()
      const prodPairs = pairs.get(prod.name) as Map<string, Set<string>>
      for (const alt of prod.alts) {
        for (let i = 0; i < alt.length; i++) {
          const el = alt[i]
          if (el.kind !== 'ref' || !pairs.has(el.name)) continue

          let j = i + 1
          let blocked = false
          while (j < alt.length) {
            const ej = alt[j]
            if (ej.kind === 'ref') {
              // A following reference blocks the walk unless it can
              // vanish; pairs starting inside it are not collected
              // (see the header note).
              if (nullable.has(ej.name)) { j++; continue }
              blocked = true
              break
            }
            const t = tokOf(ej)
            if (null == t) { blocked = true; break }
            const rest = firstOfSeq(
              alt.slice(j + 1), literals, regexTokens, firstSets, nullable)
            for (const u of rest.tokens) {
              if (addPair(el.name, t, u)) changed = true
            }
            if (rest.nullable) {
              for (const u of prodFollow) {
                if (addPair(el.name, t, u)) changed = true
              }
            }
            blocked = true
            break
          }
          if (!blocked) {
            // Nothing mandatory follows the reference, so the enclosing
            // production's pairs follow it too.
            for (const [t, us] of prodPairs) {
              for (const u of us) {
                if (addPair(el.name, t, u)) changed = true
              }
            }
          }
        }
      }
    }
  }

  return pairs
}


// Character coverage of an emitted matcher pattern, as sorted
// code-point ranges — or null when the pattern is not a shape this
// parser understands (the caller must then treat coverage as unknown
// and stay conservative). Handles exactly what the emitter itself
// produces: a leading character class with \uXXXX / \u{…} / \xXX
// escapes and ranges, `[\s\S]`, negation, or a single (possibly
// escaped) literal character. Trailing content after the first class
// (`[aA][bB]`, boundary guards) is irrelevant here: only the FIRST
// character's coverage decides whether two tokens can contest one
// input position.
function patternCharRanges(
  pattern: string,
): Array<[number, number]> | null {
  if ('[\\s\\S]' === pattern) return [[0, 0x10FFFF]]

  let i = 0

  const one = (): number | null => {
    const c = pattern[i]
    if (undefined === c) return null
    if ('\\' === c) {
      const m = pattern[i + 1]
      if (undefined === m) return null
      if ('u' === m) {
        if ('{' === pattern[i + 2]) {
          const e = pattern.indexOf('}', i + 3)
          if (e < 0) return null
          const cp = parseInt(pattern.slice(i + 3, e), 16)
          if (isNaN(cp)) return null
          i = e + 1
          return cp
        }
        const cp = parseInt(pattern.substr(i + 2, 4), 16)
        if (isNaN(cp)) return null
        i += 6
        return cp
      }
      if ('x' === m) {
        const cp = parseInt(pattern.substr(i + 2, 2), 16)
        if (isNaN(cp)) return null
        i += 4
        return cp
      }
      if ('dDwWsSbB0nrtfv'.includes(m)) {
        // Shorthand classes and control escapes: bail rather than
        // guess — unknown coverage keeps the caller conservative.
        return null
      }
      i += 2
      return m.codePointAt(0) as number
    }
    const cp = pattern.codePointAt(i) as number
    i += cp > 0xFFFF ? 2 : 1
    return cp
  }

  if ('[' !== pattern[0]) {
    const cp = one()
    if (null == cp) return null
    return [[cp, cp]]
  }

  i = 1
  let neg = false
  if ('^' === pattern[i]) {
    neg = true
    i++
  }
  const ranges: Array<[number, number]> = []
  while (i < pattern.length && ']' !== pattern[i]) {
    const lo = one()
    if (null == lo) return null
    if ('-' === pattern[i] && ']' !== pattern[i + 1] &&
      i + 1 < pattern.length) {
      i++
      const hi = one()
      if (null == hi) return null
      ranges.push([lo, hi])
    } else {
      ranges.push([lo, lo])
    }
  }
  if (']' !== pattern[i]) return null

  if (!neg) return ranges

  // Complement over the code-point space.
  ranges.sort((a, b) => a[0] - b[0])
  const out: Array<[number, number]> = []
  let next = 0
  for (const [lo, hi] of ranges) {
    if (next < lo) out.push([next, lo - 1])
    if (hi + 1 > next) next = hi + 1
  }
  if (next <= 0x10FFFF) out.push([next, 0x10FFFF])
  return out
}


// Widen character ranges to cover both cases of every ASCII letter in
// them, for matchers carrying the `i` flag. ASCII only: the ranges feed
// contest detection between a keyword and a character class, and both
// sides of that contest are ASCII in every notation this compiler
// targets. A non-ASCII letter simply keeps its own case, which is the
// conservative answer (a missed contest emits no guard).
function foldCaseRanges(
  ranges: Array<[number, number]>,
): Array<[number, number]> {
  const out: Array<[number, number]> = [...ranges]
  const A = 0x41, Z = 0x5a, a = 0x61, z = 0x7a
  const DELTA = a - A
  for (const [lo, hi] of ranges) {
    // Uppercase portion of the range -> its lowercase image, and back.
    const uLo = Math.max(lo, A), uHi = Math.min(hi, Z)
    if (uLo <= uHi) out.push([uLo + DELTA, uHi + DELTA])
    const lLo = Math.max(lo, a), lHi = Math.min(hi, z)
    if (lLo <= lHi) out.push([lLo - DELTA, lHi - DELTA])
  }
  return out
}


// Sort by low bound and merge touching/overlapping spans, so the
// overlap test below can sweep both sides once instead of comparing
// every pair. Called from tokenRangesOf, whose result is cached, so
// each token pays for this at most once.
function normalizeRanges(
  r: Array<[number, number]>,
): Array<[number, number]> {
  if (r.length < 2) return r
  const sorted = r.slice().sort((x, y) => x[0] - y[0] || x[1] - y[1])
  const out: Array<[number, number]> = [sorted[0]]
  for (let i = 1; i < sorted.length; i++) {
    const last = out[out.length - 1]
    const cur = sorted[i]
    if (cur[0] <= last[1] + 1) {
      if (last[1] < cur[1]) last[1] = cur[1]
    } else {
      out.push(cur)
    }
  }
  return out
}


// Do two coverages share a character? Both sides come from
// tokenRangesOf and are therefore sorted and merged, so one linear
// sweep decides it. This runs inside the quadratic contest loops, and
// a pairwise scan here is what made a 332-rule grammar take a minute.
function charRangesOverlap(
  a: Array<[number, number]>,
  b: Array<[number, number]>,
): boolean {
  let i = 0
  let j = 0
  while (i < a.length && j < b.length) {
    const ai = a[i]
    const bj = b[j]
    if (ai[0] <= bj[1] && bj[0] <= ai[1]) return true
    if (ai[1] < bj[1]) i++
    else j++
  }
  return false
}


// FIRST set for a specific alternative (not the whole production).
// Returns null if the alt is nullable — the caller must treat that
// case separately (typically as a fallback empty alt).
function firstOfAlt(
  alt: Sequence,
  literals: Map<string, string>,
  regexTokens: Map<string, string>,
  firstSets: Map<string, Set<string>>,
  nullable: Set<string>,
): Set<string> | null {
  const out = new Set<string>()
  for (const el of alt) {
    if (el.kind === 'term' || el.kind === 'regex' || el.kind === 'token') {
      const tok = el.kind === 'term'
        ? literals.get(termKey(el)) as string
        : el.kind === 'token'
          ? el.name
          : regexTokens.get(regexKey(el)) as string
      out.add(tok)
      return out
    }
    if (el.kind === 'ref') {
      const rf = firstSets.get(el.name) ?? new Set<string>()
      for (const tok of rf) out.add(tok)
      if (!nullable.has(el.name)) return out
      // else keep walking into the next element
      continue
    }
    throw new Error(`${diagName()}: internal — unexpected kind in firstOfAlt: ${el.kind}`)
  }
  // Alt is nullable — no non-empty prefix.
  return null
}


// Longest deterministic terminal prefix of a rule — the longest
// sequence of tokens that every alternative of the rule starts
// with. Refs are followed into their target rule, with a `visited`
// set guarding cycles. An empty array means there's no confident
// prefix (the rule either has divergent alts, starts with a multi-
// alt ref, or hits a cycle), so the caller should fall back to a
// single-token FIRST-set lookahead instead.
function ruleLiteralPrefix(
  name: string,
  grammar: Grammar,
  literals: Map<string, string>,
  regexTokens: Map<string, string>,
  visited: Set<string>,
): string[] {
  if (visited.has(name)) return []
  const next = new Set(visited); next.add(name)
  const prod = grammar.productions.find((p) => p.name === name)
  if (!prod || prod.alts.length === 0) return []

  const prefixes = prod.alts.map((alt) =>
    altLiteralPrefix(alt, grammar, literals, regexTokens, next))
  if (prefixes.some((p) => p.length === 0)) return []
  const minLen = Math.min(...prefixes.map((p) => p.length))
  const common: string[] = []
  for (let i = 0; i < minLen; i++) {
    const tok = prefixes[0][i]
    if (prefixes.every((p) => p[i] === tok)) common.push(tok)
    else break
  }
  return common
}


function altLiteralPrefix(
  alt: Sequence,
  grammar: Grammar,
  literals: Map<string, string>,
  regexTokens: Map<string, string>,
  visited: Set<string>,
): string[] {
  const out: string[] = []
  for (const el of alt) {
    if (el.kind === 'term') {
      out.push(literals.get(termKey(el)) as string)
    } else if (el.kind === 'regex') {
      out.push(regexTokens.get(regexKey(el)) as string)
    } else if (el.kind === 'token') {
      out.push(el.name)
    } else if (el.kind === 'ref') {
      const sub = ruleLiteralPrefix(
        el.name, grammar, literals, regexTokens, visited)
      // Take the ref's literal prefix and stop — we can't see past
      // the ref without more expensive analysis.
      out.push(...sub)
      return out
    } else {
      return out
    }
  }
  return out
}


type PrefixPath = { tokens: string[]; done: boolean }


// Enumerate concrete token-sequence prefixes an alternative can
// start with, each at most `maxK` tokens long. Refs with multiple
// alternatives fan out into one prefix per sub-alternative so the
// caller can emit a dedicated dispatch alt for each path. When a
// ref cycles back or exhausts depth, the path is *terminated* at
// the tokens accumulated so far — the `done` flag is propagated
// out of nested calls so a truncated sub-prefix is never extended
// with tokens from elements the outer alt happens to list after the
// cycled ref.
function altPrefixesRaw(
  alt: Sequence,
  grammar: Grammar,
  literals: Map<string, string>,
  regexTokens: Map<string, string>,
  maxK: number,
  visited: Set<string> = new Set(),
): PrefixPath[] {
  let paths: PrefixPath[] = [{ tokens: [], done: false }]

  for (const el of alt) {
    const next: PrefixPath[] = []
    for (const p of paths) {
      if (p.done || p.tokens.length >= maxK) { next.push(p); continue }
      if (el.kind === 'term') {
        next.push({
          tokens: [...p.tokens, literals.get(termKey(el)) as string],
          done: false,
        })
      } else if (el.kind === 'regex') {
        next.push({
          tokens: [...p.tokens, regexTokens.get(regexKey(el)) as string],
          done: false,
        })
      } else if (el.kind === 'token') {
        next.push({ tokens: [...p.tokens, el.name], done: false })
      } else if (el.kind === 'ref') {
        if (visited.has(el.name)) {
          next.push({ tokens: p.tokens, done: true })
          continue
        }
        const childVisited = new Set(visited); childVisited.add(el.name)
        const target = grammar.productions.find((pr) => pr.name === el.name)
        if (!target || target.alts.length === 0) {
          next.push({ tokens: p.tokens, done: true })
          continue
        }
        for (const sub of target.alts) {
          const subPaths = altPrefixesRaw(
            sub, grammar, literals, regexTokens,
            maxK - p.tokens.length, childVisited)
          for (const sp of subPaths) {
            next.push({
              tokens: [...p.tokens, ...sp.tokens],
              // Propagate `done` so the outer loop won't extend a
              // cycle-truncated sub-prefix.
              done: sp.done,
            })
          }
        }
      } else {
        // Desugar should have eliminated group/star/etc. at this point.
        next.push({ tokens: p.tokens, done: true })
      }
    }
    paths = next
    if (paths.every((p) => p.done || p.tokens.length >= maxK)) break
  }

  return paths
}


function altPrefixes(
  alt: Sequence,
  grammar: Grammar,
  literals: Map<string, string>,
  regexTokens: Map<string, string>,
  maxK: number,
): string[][] {
  const raw = altPrefixesRaw(alt, grammar, literals, regexTokens, maxK)
  const seen = new Set<string>()
  const out: string[][] = []
  for (const p of raw) {
    const key = p.tokens.join(' ')
    if (!seen.has(key)) { seen.add(key); out.push(p.tokens) }
  }
  return out
}


// A quoted-string literal is effectively case-sensitive either
// when the user explicitly wrote `%s"…"` or when it contains no
// ASCII letters (there's nothing to fold — `"+"` matches `+` in
// any "case").
function isEffectivelyCaseSensitive(el: {
  literal: string
  caseSensitive?: boolean
}): boolean {
  if (el.caseSensitive === true) return true
  return !/[A-Za-z]/.test(el.literal)
}


// Map a term element to the key used to look up (or allocate) its
// emitted token. The key folds together the literal and its
// effective case-sensitivity so a sensitive and an insensitive
// occurrence of the same string are distinct tokens.
function termKey(el: { literal: string; caseSensitive?: boolean }): string {
  return (isEffectivelyCaseSensitive(el) ? 'cs:' : 'ci:') + el.literal
}


function escapeRegExp(s: string): string {
  return s.replace(/[\\^$.*+?()[\]{}|]/g, '\\$&')
}


// Token names the engine's own matchers own. A lifted literal that
// would land on one — `NR = "NR"` wants `#NR`, `end = "ZZ"` wants `#ZZ`
// — must be renamed instead: the engine rejects a `fixed.token` entry
// under a matcher-owned name at configuration time, so emitting it
// turns an otherwise ordinary grammar into a hard failure before
// parsing starts. Falling through to the numbered form (`#NR1`) costs
// nothing but a less pretty name.
function isEngineOwnedToken(name: string): boolean {
  // Mirrors the engine's MATCHER_TOKEN_NAMES: tokens its own matchers
  // produce, which a grammar's fixed literal must not be named after
  // (`"aa"` would otherwise derive `#AA`, the engine's ANY token).
  return Object.prototype.hasOwnProperty.call(BUILTIN_TOKENS, name.slice(1)) ||
    '#BD' === name || '#ZZ' === name || '#UK' === name || '#AA' === name ||
    '#SP' === name || '#LN' === name || '#CM' === name
}


function allocTokenName(
  literal: string,
  used: Set<string>,
  preferred?: string,
): string {
  // A literal lifted from a named production (`PL = "+"`) keeps that
  // name, so the emitted grammar reads `PL` rather than `T`.
  if (preferred) {
    const want = '#' + preferred
    if (!used.has(want) && !isEngineOwnedToken(want)) {
      used.add(want)
      return want
    }
  }
  const base = literal
    .replace(/[^A-Za-z0-9]/g, '_')
    .toUpperCase()
    .replace(/^_+|_+$/g, '')
  const candidate = base.length > 0 ? '#' + base : '#T'
  if (!used.has(candidate) && !isEngineOwnedToken(candidate)) {
    used.add(candidate)
    return candidate
  }
  let i = 1
  while (used.has(candidate + i)) i++
  const chosen = candidate + i
  used.add(chosen)
  return chosen
}

// ---- Public surface -----------------------------------------------
//
// A front-end parses its own notation into `Grammar` and calls
// `emitGrammarSpec`. Everything else is exported because the front-ends
// (and their tests) legitimately reach for it: the IR types to build a
// grammar, `eliminateLeftRecursion` to run that pass alone, and the
// small helpers a notation parser needs when lowering its own syntax
// (`refsIn` to collect references, `escapeRegExp` for literal-to-regex,
// `BUILTIN_TOKENS` to recognise engine token names).
export {
  diagName,
  emitGrammarSpec,
  eliminateLeftRecursion,
  refsIn,
  escapeRegExp,
  termKey,
  isEffectivelyCaseSensitive,
  BUILTIN_TOKENS,
  REMOVE_PROSE,
  REMOVE_ALL,
  isProseName,
}

// (`ConvertOptions` is exported at its own declaration above.)
export type {
  Element,
  Sequence,
  Production,
  Grammar,
  ProbeDispatchSpec,
  AmbiguityReport,
}
