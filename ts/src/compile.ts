/* Copyright (c) 2026 Richard Rodger and other contributors, MIT License */

/*  compile.ts
 *  Compilation mode: turn an ABNF source into a *pure recognition*
 *  tabnas grammar and serialise it as jsonic text.
 *
 *  A tabnas `GrammarSpec` emitted by the converter carries function
 *  references (the `ref` map plus `a`/`bo`/`bc` hooks) that exist
 *  purely to build the `{rule, src, kids}` AST. Recognition itself —
 *  whether an input matches the grammar — is fully structural
 *  (`s`/`p`/`r`/`b`/`g`/`c` with declarative counter conditions). So a
 *  function-free spec recognises the same language; it just doesn't
 *  build the bespoke tree.
 *
 *  The one exception is the probe + phase-retry dispatcher used for
 *  optional-prefix (`[X D] Y`) ambiguity, which needs control
 *  functions for unbounded lookahead. Until those ship as engine
 *  `$`-builtins (see docs/design/alt-action-refs.md §6), compilation
 *  mode *refuses* such grammars rather than emit a broken one.
 */

import type { GrammarSpec } from '@tabnas/parser'

import { bnf as bnfConvert, BnfConvertOptions } from './converter'


// Hook fields whose string value is a `@`-ref into `spec.ref`. These
// are the AST-building actions compilation mode drops.
const REF_FIELDS = new Set(['a', 'bo', 'bc'])

// Tree-building `$`-builtins (emitted with `builtins: true`). Pure
// recognition mode drops these too; their per-alt config lives under
// `k.node$` / `k.capture$`.
const TREE_BUILTINS = new Set(['@node$', '@capture$', '@bubble$'])
const TREE_CONFIG_KEYS = ['node$', 'capture$']


export class BnfCompileError extends Error {
  rules: string[]
  constructor(message: string, rules: string[]) {
    super(message)
    this.name = 'BnfCompileError'
    this.rules = rules
  }
}


// Deep clone that preserves RegExp instances (match-token matchers)
// and drops every function-valued property.
function cloneData(v: any): any {
  if (v instanceof RegExp) return v
  if (Array.isArray(v)) return v.map(cloneData)
  if (v && 'object' === typeof v) {
    const o: any = {}
    for (const k of Object.keys(v)) {
      if ('function' === typeof v[k]) continue
      o[k] = cloneData(v[k])
    }
    return o
  }
  return v
}

// Like `cloneData`, but also drops the AST-building hooks: `a`/`bo`/`bc`
// fields that point at a dropped action (a `spec.ref` closure or a tree
// `$`-builtin), and the now-orphaned `k.node$` / `k.capture$` config.
// Control builtins (`@probe…$`) and structural fields are preserved.
function cloneRecognition(v: any, isDropped: (s: string) => boolean): any {
  if (v instanceof RegExp) return v
  if (Array.isArray(v)) return v.map((x) => cloneRecognition(x, isDropped))
  if (v && 'object' === typeof v) {
    const o: any = {}
    for (const k of Object.keys(v)) {
      const x = v[k]
      if ('function' === typeof x) continue
      if (REF_FIELDS.has(k) && 'string' === typeof x && isDropped(x)) continue
      if ('k' === k && x && 'object' === typeof x) {
        const kc = cloneRecognition(x, isDropped)
        for (const tk of TREE_CONFIG_KEYS) delete kc[tk]
        if (0 === Object.keys(kc).length) continue
        o[k] = kc
        continue
      }
      o[k] = cloneRecognition(x, isDropped)
    }
    return o
  }
  return v
}


// Find rules that reference the ref map from a field *other than* the
// droppable AST hooks — i.e. control functions (probe `c:` guards and
// dispatch actions). Their presence means the grammar can't be
// represented purely structurally yet.
function controlRefRules(
  spec: GrammarSpec, isRef: (s: string) => boolean): string[] {
  const offenders = new Set<string>()
  const scan = (o: any, rule: string) => {
    if (Array.isArray(o)) { o.forEach((x) => scan(x, rule)); return }
    if (!o || 'object' !== typeof o) return
    for (const k of Object.keys(o)) {
      const x = o[k]
      if (!REF_FIELDS.has(k) && 'string' === typeof x && isRef(x)) {
        offenders.add(rule)
      } else {
        scan(x, rule)
      }
    }
  }
  const rules = spec.rule ?? {}
  for (const name of Object.keys(rules)) scan((rules as any)[name], name)
  return [...offenders].sort()
}


// Strip a converted spec down to a function-free recognition grammar.
// Throws `BnfCompileError` for grammars whose control logic is still
// closures (i.e. a probe dispatcher converted without `builtins`).
export function toRecognitionSpec(spec: GrammarSpec): GrammarSpec {
  const ref: Record<string, unknown> = (spec as any).ref ?? {}
  const isRef = (s: string) => Object.prototype.hasOwnProperty.call(ref, s)

  const offenders = controlRefRules(spec, isRef)
  if (offenders.length > 0) {
    throw new BnfCompileError(
      'bnf: grammar needs control functions (probe / unbounded ' +
      'lookahead) and cannot be emitted as a pure recognition grammar; ' +
      'recompile with `builtins: true`. Offending rule(s): ' +
      offenders.join(', '),
      offenders,
    )
  }

  const isDropped = (s: string) => isRef(s) || TREE_BUILTINS.has(s)
  return cloneRecognition(
    { options: spec.options, rule: spec.rule }, isDropped) as GrammarSpec
}


// Reduce a spec to a pure-data, function-free grammar that *keeps* the
// AST-building `$`-builtins (so the deserialized grammar builds the
// full `{rule,src,kids}` tree). Requires `builtins: true` conversion —
// throws if any closures remain in `spec.ref`.
export function toPureSpec(spec: GrammarSpec): GrammarSpec {
  const ref: Record<string, unknown> = (spec as any).ref ?? {}
  const closures = Object.keys(ref)
  if (closures.length > 0) {
    throw new BnfCompileError(
      'bnf: spec still contains closures; convert with `builtins: true` ' +
      'for pure-data output. Stray ref(s): ' + closures.slice(0, 3).join(', '),
      [],
    )
  }
  return cloneData({ options: spec.options, rule: spec.rule }) as GrammarSpec
}


export type JsonicOptions = { strict?: boolean; indent?: number }

const IDENT = /^[A-Za-z_$][A-Za-z0-9_$]*$/


// Serialise a (function-free) value as jsonic text. Relaxed by
// default: bare identifier keys, single-quoted strings, newline-
// separated entries. `strict: true` emits valid JSON (double quotes,
// comma-separated) for round-trip verification. RegExp instances are
// emitted as `@/source/flags` strings (jsonic's `resolveFuncRefs`
// reconstructs them on load).
export function toJsonic(value: any, opts: JsonicOptions = {}): string {
  const strict = !!opts.strict
  const ind = opts.indent ?? 2
  const sep = strict ? ',\n' : '\n'
  const pad = (n: number) => ' '.repeat(ind * n)

  const quote = (s: string, ch: string) =>
    ch + s
      .replace(/\\/g, '\\\\')
      .replace(new RegExp(ch, 'g'), '\\' + ch)
      .replace(/\n/g, '\\n') + ch

  const dq = (s: string) => quote(s, '"')
  const str = (s: string) => strict ? dq(s) : quote(s, "'")
  const key = (k: string) =>
    (!strict && IDENT.test(k)) ? k : dq(k)

  const ser = (v: any, depth: number): string => {
    if (null === v || undefined === v) return 'null'
    if (v instanceof RegExp) {
      // `@~/…/` carries the `eager$` matcher flag through serialisation;
      // plain `@/…/` for ordinary regexes.
      const sentinel = (v as any).eager$ ? '@~/' : '@/'
      return str(sentinel + v.source + '/' + v.flags)
    }
    const t = typeof v
    if ('number' === t || 'boolean' === t) return String(v)
    if ('string' === t) return str(v)
    if (Array.isArray(v)) {
      if (0 === v.length) return '[]'
      const items = v.map((x) => pad(depth + 1) + ser(x, depth + 1))
      return '[\n' + items.join(sep) + '\n' + pad(depth) + ']'
    }
    if ('object' === t) {
      const keys = Object.keys(v)
      if (0 === keys.length) return '{}'
      const items = keys.map(
        (k) => pad(depth + 1) + key(k) + ': ' + ser(v[k], depth + 1))
      return '{\n' + items.join(sep) + '\n' + pad(depth) + '}'
    }
    return 'null'
  }

  return ser(value, 0)
}


// ---- User semantic actions (the `m`-mark feature) -----------------

export class BnfActionError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'BnfActionError'
  }
}

type ActionFn = (r: any, ctx: any, alt: any) => any
export type ActionsMap = Record<string, ActionFn | ActionFn[]>

const PHASES = new Set(['bo', 'ao', 'bc', 'ac'])

// Compose a previous action (the compiler's own, run first) with the
// user's, in attachment order — the synthetic wrapper from the design.
function composeActions(prev: any, fns: ActionFn[]): ActionFn {
  const prevFn: ActionFn | null = 'function' === typeof prev ? prev : null
  return (r, ctx, alt) => {
    if (prevFn) prevFn(r, ctx, alt)
    for (const fn of fns) fn(r, ctx, alt)
  }
}

// Collapse a function list into a single action (or pass one through).
function seqActions(fns: ActionFn[]): ActionFn {
  return 1 === fns.length ? fns[0] : (r, ctx, alt) => {
    for (const fn of fns) fn(r, ctx, alt)
  }
}

// Append an action ref/fn to an alt's `a`, producing the engine's
// array-`a` form so the alt's own action runs first, then the new one.
function appendAction(existing: any, added: any): any {
  if (null == existing) return added
  return Array.isArray(existing) ? [...existing, added] : [existing, added]
}

function altListOf(field: any): any[] {
  return Array.isArray(field) ? field : (field && field.alts) || []
}

// Resolve `@<rule>:<sel>` against the spec; return the matched alts (for
// o:/c: selectors) or the rule-phase string (for bo/ao/bc/ac).
function resolveTarget(
  spec: GrammarSpec, key: string,
): { phase: string } | { alts: any[]; rule: string } {
  const m = /^@([^:]+):(.+)$/.exec(key)
  if (!m) {
    throw new BnfActionError(
      `bnf: malformed action ref '${key}' ` +
      `(expected @rule:phase or @rule:o|c:mark)`)
  }
  const rule = m[1]
  const sel = m[2]
  const rules = (spec.rule ?? {}) as any
  if (!rules[rule]) {
    throw new BnfActionError(
      `bnf: action ref '${key}' targets unknown rule '${rule}'`)
  }
  if (PHASES.has(sel)) return { phase: sel }

  const pm = /^([oc]):(.+)$/.exec(sel)
  if (!pm) throw new BnfActionError(`bnf: malformed action ref '${key}'`)
  const phase = 'o' === pm[1] ? 'open' : 'close'
  const mark = pm[2]
  const alts = altListOf(rules[rule][phase]).filter((a) => a && a.m === mark)
  if (0 === alts.length) {
    throw new BnfActionError(
      `bnf: action ref '${key}' matches no ${phase} alt with mark ` +
      `'${mark}' in rule '${rule}'`)
  }
  return { alts, rule }
}

// Attach user semantic actions to a spec, in place. Keys:
// `@<rule>:<phase>` (bo/ao/bc/ac) or `@<rule>:o|c:<mark>`. Values: a
// function or array of functions, run *after* the compiler's own action
// in attachment order. Works in both closure and `builtins` mode — alt
// actions are injected as the engine's array-`a` form (the alt's own
// action, builtin or closure, runs first). Throws `BnfActionError` for
// a ref matching no rule / hook / marked alt.
export function attachActions(spec: GrammarSpec, actions: ActionsMap): GrammarSpec {
  const ref: Record<string, any> =
    ((spec as any).ref = (spec as any).ref ?? {})
  let counter = 0

  for (const key of Object.keys(actions)) {
    const fns = ([] as ActionFn[]).concat(actions[key] as any)
    const target = resolveTarget(spec, key)

    if ('phase' in target) {
      // Rule-phase hook: reuse the engine's `@<rule>-<phase>` fnref
      // auto-install (fnref builds the key from the rule name, so a
      // hyphenated ABNF rule name stays unambiguous).
      const rule = /^@([^:]+):/.exec(key)![1]
      const fkey = `@${rule}-${target.phase}`
      ref[fkey] = composeActions(ref[fkey], fns)
      continue
    }

    for (const alt of target.alts) {
      const userRef = `@bnf_user${counter++}`
      ref[userRef] = seqActions(fns)
      alt.a = appendAction(alt.a, userRef)
    }
  }
  return spec
}

// Declare user-action *slots* on a (pure-data) spec without supplying
// functions: each `@<rule>:o|c:<mark>` ref name is injected into the
// matched alt's array-`a`, to be resolved at load time from a
// user-supplied ref map. Lets a serialized grammar carry user-action
// hooks that the consumer binds by name. Throws on unknown targets.
export function attachActionSlots(spec: GrammarSpec, refNames: string[]): GrammarSpec {
  for (const name of refNames) {
    const target = resolveTarget(spec, name)
    if ('phase' in target) {
      throw new BnfActionError(
        `bnf: slot '${name}' is a rule-phase ref; slots are for ` +
        `@rule:o|c:mark alt actions`)
    }
    for (const alt of target.alts) alt.a = appendAction(alt.a, name)
  }
  return spec
}

// Human-readable listing of the marks the compiler assigned, for
// discoverability (CLI `--marks`).
export function markListing(spec: GrammarSpec): string {
  const lines: string[] = []
  const rules = spec.rule ?? {}
  for (const rule of Object.keys(rules)) {
    for (const [ph, sym] of [['open', 'o'], ['close', 'c']] as const) {
      const f = (rules as any)[rule][ph]
      const list: any[] = Array.isArray(f) ? f : (f && f.alts) || []
      for (const a of list) {
        if (a && null != a.m) {
          const what = a.s ? `s:${a.s}` : a.p ? `p:${a.p}` : '(empty)'
          lines.push(`${rule}  ${sym}:${a.m}  ${what}`)
        }
      }
    }
  }
  return lines.join('\n')
}


export type BnfCompileOptions = BnfConvertOptions & JsonicOptions & {
  // Default `true`: emit a pure *recognition* grammar (tree-building
  // dropped). Set `false` to emit the full AST grammar with tree
  // `$`-builtins retained — still pure data, builds `{rule,src,kids}`.
  recognition?: boolean
}


// Compile ABNF source into a pure-data tabnas grammar as jsonic text.
// Always converts with `builtins: true` so probe dispatch and tree
// building serialise as `@…$` builtin refs (no closures).
export function bnfCompile(src: string, opts: BnfCompileOptions = {}): string {
  const spec = bnfConvert(src,
    { start: opts.start, tag: opts.tag, builtins: true, marks: true })
  const out = (false === opts.recognition)
    ? toPureSpec(spec)
    : toRecognitionSpec(spec)
  return toJsonic(out, { strict: opts.strict, indent: opts.indent })
}
