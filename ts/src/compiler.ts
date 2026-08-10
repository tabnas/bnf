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
  | { kind: 'ref'; name: string }
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
  | { kind: 'star'; inner: Element }    // *A
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
function eliminateLeftRecursion(grammar: Grammar): Grammar {
  const originalOrder = grammar.productions.map((p) => p.name)

  // Order productions so that rules referenced at a leading position
  // are processed before the rules that reference them. Paull's
  // substitution inlines A_j's alts into A_i for j < i, so putting
  // dependencies first is what makes nullable-prefixed hidden left
  // recursion reachable by the substitution step.
  let prods = topoOrderForPaull(
    grammar.productions.map((p) => ({
      name: p.name,
      alts: p.alts.map((a) => a.slice()),
      nodeKind: p.nodeKind,
    })),
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
    prods[i] = eliminateDirectLeftRec(prods[i])
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
  return { name: target.name, alts: newAlts, nodeKind: target.nodeKind }
}


// Rewrite a single production's direct left recursion to its
// iterative equivalent. Equivalent to the previous version of
// `eliminateLeftRecursion` but scoped to one production.
function eliminateDirectLeftRec(prod: Production): Production {
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
    return { name: prod.name, alts: seeds, nodeKind: prod.nodeKind }
  }
  if (seeds.length === 0) {
    throw new Error(
      `abnf: rule '${prod.name}' is purely left-recursive ` +
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

  return {
    name: prod.name,
    alts: [[seedElement, { kind: 'star', inner: tailInner }]],
    nodeKind: prod.nodeKind,
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


function desugar(grammar: Grammar): Grammar {
  const extra: Production[] = []
  const used = new Set(grammar.productions.map((p) => p.name))

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
        `abnf: internal: unresolved prose terminal '<${el.text}>'`)
    }

    if (el.kind === 'group') {
      // Recurse into the group's alts so nested sugar is flattened,
      // then emit a helper production whose body is those alts.
      const innerAlts = el.alts.map((a) => desugarAlt(a))
      const name = freshName('group')
      extra.push({ name, alts: innerAlts, nodeKind: 'helper' })
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
      extra.push({ name, alts: [[inner], []], nodeKind: 'helper' })
      return { kind: 'ref', name }
    }

    if (el.kind === 'star') {
      // H = inner H / (empty)
      const name = freshName('star_' + hint)
      const selfRef: Element = { kind: 'ref', name }
      extra.push({ name, alts: [[inner, selfRef], []], nodeKind: 'helper' })
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
      })
      extra.push({
        name: plusName,
        alts: [[inner, tailRef]],
        nodeKind: 'helper',
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
        extra.push({ name: groupName, alts: [seq], nodeKind: 'helper' })
        const groupRef: Element = { kind: 'ref', name: groupName }
        const optName = freshName('opt_' + groupName)
        extra.push({
          name: optName,
          alts: [[groupRef], []],
          nodeKind: 'helper',
        })
        nestedRef = { kind: 'ref', name: optName }
      }
      if (nestedRef) repAlt.push(nestedRef)
    }

    extra.push({ name: repName, alts: [desugarAlt(repAlt)], nodeKind: 'helper' })
    return { kind: 'ref', name: repName }
  }

  const rewritten: Production[] = grammar.productions.map((p) => {
    const out: Production = {
      name: p.name,
      alts: p.alts.map(desugarAlt),
      nodeKind: p.nodeKind,
    }
    // Probe-dispatch and tail-repeat flags survive desugar unchanged —
    // the emitter routes around the standard alt-compilation path for
    // these. (A tail-repeat separator is all-terminal by construction,
    // so it needs no desugaring of its own.)
    if (p.probeDispatch) out.probeDispatch = p.probeDispatch
    if (p.probeHelper) out.probeHelper = p.probeHelper
    if (p.tailRepeat) out.tailRepeat = p.tailRepeat
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
        })
        // Synthesise the committed branches. `with` = X D Y, `no` = Y.
        extra.push({
          name: withName,
          alts: [[...info.xSeq, info.disambiguator, ...ySeq]],
          nodeKind: 'helper',
        })
        extra.push({
          name: noName,
          alts: [ySeq],
          nodeKind: 'helper',
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
      `abnf: probe-dispatch rule '${prod.name}' has unresolvable ` +
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
        `abnf: '${prod.name}' is prose, and prose is only valid as a ` +
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
              `abnf: '<${target}>' is not a removal target. The only prose ` +
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
          `abnf: '${prod.name}' is prose, and prose is only valid as a ` +
          `production name for the removal directive '<all> = <remove>'.`)
      }

      if (BUILTIN_TOKENS[prod.name]) continue // informational — the lexer defines it
      throw new Error(
        `abnf: rule '${prod.name}' is defined only by prose ('<${text}>'), ` +
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
            `abnf: rule '${prod.name}' uses prose ('<${stray.text}>') inside an ` +
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
      'abnf: grammar defines no rules — only informational prose terminals.')
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
      'abnf: this @tabnas/parser is too old — it does not export ' +
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
function cloneGrammar(grammar: Grammar): Grammar {
  return {
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
        matchTokens[name] = new RegExp('^' + el.pattern, el.flags)
      }
    }
  }

  const knownRules = new Set(grammar.productions.map((p) => p.name))
  const { firstSets, nullable } = computeFirstSets(
    grammar, literals, regexTokens)
  const refs = new RefRegistry()
  refs.useBuiltins = !!opts?.builtins
  refs.emitMarks = !!opts?.marks

  const ruleSpec: NonNullable<GrammarSpec['rule']> = {}
  for (const prod of grammar.productions) {
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
      firstSets, nullable, refs,
    )
  }

  // Wrap the user-visible start rule in a synthetic rule that
  // explicitly consumes #ZZ. Without this, a user rule that pops
  // without matching the end-of-source token lets trailing content
  // slip past tabnas's post-loop endtkn check (the lookahead buffer
  // outlives the parse loop).
  const startWrapper = '__start__'
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
      g: tag,
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
      segs.push(current)
      current = { terms: [], ref: null }
    } else {
      // `opt`, `star`, `plus`, `group` must have been desugared
      // before reaching the emitter.
      throw new Error(
        `abnf: internal — unexpected element kind '${el.kind}' in emitter`)
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
        `abnf: rule '${ruleName}' references unknown rule '${el.name}'`)
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
    const name = `@abnf_a${this.counter++}` as `@${string}`
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
    g: tag,
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
) {
  for (const alt of prod.alts) {
    validateRefs(alt, knownRules, prod.name)
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
    const opens: any[] = []
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
          for (const tok of firstTokens) {
            const o: any = {
              s: tok,
              b: 1,
              p: seg.ref,
              ...refs.node(
                { init: true, rule: prod.name, kind: prodKind, nterms: 0 },
                (r: Rule) => { r.node = mkAstNode(prod.name, prodKind) }),
              g: tag,
            }
            if (mark) o.m = mark
            opens.push(o)
          }
          continue
        }
      }
      const o = segmentToAlt(seg, tag, refs, true, prod.name, prodKind)
      if (mark) o.m = mark
      opens.push(o)
    }

    const rs: any = { open: opens }

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
      ruleSpec, refs, prod.nodeKind ?? 'user')
    return
  }

  // Multi-alt with at least one multi-segment alternative: emit a
  // dispatcher. Each alt becomes its own chained impl rule
  // (`<prodname>$alt<i>`); the main rule's open peeks the first token
  // and pushes the matching impl rule. Using `p:` (not `r:`) keeps
  // the parent's `child` pointer valid so the parent can read the
  // impl's node in its close-state action.
  const dispatchOpen: any[] = []
  let emptyAltSeen = false
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

    emitChain(implName, alt, literals, regexTokens, tag, ruleSpec, refs,
      'helper')

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

    const LOOKAHEAD_K = 4
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
        dispatchOpen.push(o)
      }
    } else {
      const firstTokens = firstOfAlt(
        alt, literals, regexTokens, firstSets, nullable)
      if (firstTokens === null) {
        throw new Error(
          `abnf: rule '${prod.name}' alternative ${i} is nullable ` +
          `but is not the only empty alt; FIRST set is ambiguous`)
      }
      for (const tok of firstTokens) {
        const o: any = {
          s: tok, b: 1, p: implName, ...initDispatchFields, g: tag,
        }
        if (mark) o.m = mark
        dispatchOpen.push(o)
      }
    }
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
    dispatchOpen.push(o)
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
  ruleSpec[prod.name] = { open: dispatchOpen, close: [dispClose] }
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
          throw new Error(`abnf: internal — unexpected kind in FIRST: ${el.kind}`)
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
    throw new Error(`abnf: internal — unexpected kind in firstOfAlt: ${el.kind}`)
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


function allocTokenName(
  literal: string,
  used: Set<string>,
  preferred?: string,
): string {
  // A literal lifted from a named production (`PL = "+"`) keeps that
  // name, so the emitted grammar reads `PL` rather than `T`.
  if (preferred) {
    const want = '#' + preferred
    if (!used.has(want)) {
      used.add(want)
      return want
    }
  }
  const base = literal
    .replace(/[^A-Za-z0-9]/g, '_')
    .toUpperCase()
    .replace(/^_+|_+$/g, '')
  const candidate = base.length > 0 ? '#' + base : '#T'
  if (!used.has(candidate)) {
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
