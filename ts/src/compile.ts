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


export class BnfCompileError extends Error {
  rules: string[]
  constructor(message: string, rules: string[]) {
    super(message)
    this.name = 'BnfCompileError'
    this.rules = rules
  }
}


// Deep clone that preserves RegExp instances (match-token matchers),
// drops every function-valued property, and drops the AST-building
// hook fields (`a`/`bo`/`bc`) that point at the ref map.
function cloneRecognition(v: any, isRef: (s: string) => boolean): any {
  if (v instanceof RegExp) return v
  if (Array.isArray(v)) return v.map((x) => cloneRecognition(x, isRef))
  if (v && 'object' === typeof v) {
    const o: any = {}
    for (const k of Object.keys(v)) {
      const x = v[k]
      if ('function' === typeof x) continue
      if (REF_FIELDS.has(k) && 'string' === typeof x && isRef(x)) continue
      o[k] = cloneRecognition(x, isRef)
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
// Throws `BnfCompileError` for grammars that need control functions.
export function toRecognitionSpec(spec: GrammarSpec): GrammarSpec {
  const ref: Record<string, unknown> = (spec as any).ref ?? {}
  const isRef = (s: string) => Object.prototype.hasOwnProperty.call(ref, s)

  const offenders = controlRefRules(spec, isRef)
  if (offenders.length > 0) {
    throw new BnfCompileError(
      'bnf: grammar needs control functions (probe / unbounded ' +
      'lookahead) and cannot be emitted as a pure recognition grammar ' +
      'yet; offending rule(s): ' + offenders.join(', '),
      offenders,
    )
  }

  return cloneRecognition(
    { options: spec.options, rule: spec.rule }, isRef) as GrammarSpec
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
    if (v instanceof RegExp) return str('@/' + v.source + '/' + v.flags)
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


export type BnfCompileOptions = BnfConvertOptions & JsonicOptions


// Compile ABNF source into a pure recognition grammar in jsonic text.
export function bnfCompile(src: string, opts: BnfCompileOptions = {}): string {
  const spec = bnfConvert(src, { start: opts.start, tag: opts.tag })
  const rspec = toRecognitionSpec(spec)
  return toJsonic(rspec, { strict: opts.strict, indent: opts.indent })
}
