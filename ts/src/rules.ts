/* Copyright (c) 2026 Richard Rodger and other contributors, MIT License */

/*  rules.ts
 *  The schema `grammar-rules`: a grammar's rules as its text writes them,
 *  uncompiled and in source order, which the ABNF, EBNF and GBNF front
 *  ends read their documents as and write them from (alchemy's
 *  docs/streaming-plan.md, section 13.1; docs/grammar-rules.md here).
 *
 *    { "type": "grammar", ["start": name], "rules": [ rule, ... ] }
 *    rule     { "type": "rule", "name", ["incremental": true], "alternatives": [ [ element, ... ], ... ] }
 *    element  { "type", ...its members, ["repeat": [ { "min", ["max"] }, ... ]] }
 *
 *  Every object's members come in the schema's order, `type` first, since
 *  a reader streams a tree as its rule passes reach each member, and a
 *  member in brackets is there only where the text writes it. Nothing here
 *  knows a notation: what a front end cannot write it refuses in its own
 *  render, by its own rule.
 *
 *  `checkRules` holds a value to the schema, and fails with a `RulesError`
 *  whose `code` names what was wrong and whose `path` says where:
 *
 *    rules_root    the root is not a grammar
 *    rules_type    a node of a type no rule or element has
 *    rules_member  a member the node does not have, one it lacks, or one
 *                  out of the schema's order or repeated
 *    rules_value   a member whose value is not what the schema gives it
 */

// The largest code point.
const MAX_CODE_POINT = 0x10ffff

type RulesRepeat = { min: number; max?: number }

// An element of an alternative, its members in the schema's order.
type RulesElement = (
  | { type: 'reference'; name: string }
  | { type: 'literal'; text: string; caseSensitive: boolean }
  | { type: 'char'; value: number }
  | { type: 'range'; from: number; to: number }
  | { type: 'class'; negated: boolean; members: { from: number; to: number }[] }
  | { type: 'any' }
  | { type: 'token'; negated: boolean; text: string }
  | { type: 'prose'; text: string }
  | { type: 'group'; alternatives: RulesElement[][] }
) & { repeat?: RulesRepeat[] }

type RulesRule = {
  type: 'rule'
  name: string
  incremental?: true
  alternatives: RulesElement[][]
}

type GrammarRules = { type: 'grammar'; start?: string; rules: RulesRule[] }

class RulesError extends Error {
  code: string
  path: string
  constructor(code: string, path: string, message: string) {
    super(`grammar-rules: ${path}: ${message}`)
    this.code = code
    this.path = path
  }
}

// A member of a node: its name, whether every node of the kind has it,
// and the check its value meets.
type Member = [name: string, required: boolean, check: (v: unknown, at: string) => void]

const isObject = (v: unknown): v is Record<string, unknown> =>
  null != v && 'object' === typeof v && !Array.isArray(v)

function text(v: unknown, at: string): void {
  if ('string' !== typeof v) throw new RulesError('rules_value', at, 'is not text')
}

function name(v: unknown, at: string): void {
  text(v, at)
  if ('' === v) throw new RulesError('rules_value', at, 'is empty')
}

function bool(v: unknown, at: string): void {
  if ('boolean' !== typeof v) throw new RulesError('rules_value', at, 'is not true or false')
}

function yes(v: unknown, at: string): void {
  if (true !== v) throw new RulesError('rules_value', at, 'is true where it is written')
}

function count(v: unknown, at: string): void {
  if (!Number.isInteger(v) || (v as number) < 0) {
    throw new RulesError('rules_value', at, 'is not a whole number')
  }
}

function codePoint(v: unknown, at: string): void {
  if (!Number.isInteger(v) || (v as number) < 0 || MAX_CODE_POINT < (v as number)) {
    throw new RulesError('rules_value', at, 'is not a code point')
  }
}

// An object's members, held to the kind's list: each in the list's order,
// none twice, every required one there, and no other.
function members(node: Record<string, unknown>, list: Member[], at: string): void {
  let next = 0
  for (const key of Object.keys(node)) {
    let found = -1
    for (let i = next; i < list.length; i++) {
      if (list[i][0] === key) { found = i; break }
    }
    if (found < 0) {
      throw new RulesError('rules_member', `${at}.${key}`,
        list.some((m) => m[0] === key)
          ? 'comes out of its place, or twice'
          : 'is no member of this node')
    }
    for (let i = next; i < found; i++) {
      if (list[i][1]) throw new RulesError('rules_member', `${at}.${list[i][0]}`, 'is missing')
    }
    list[found][2](node[key], `${at}.${key}`)
    next = found + 1
  }
  for (let i = next; i < list.length; i++) {
    if (list[i][1]) throw new RulesError('rules_member', `${at}.${list[i][0]}`, 'is missing')
  }
}

function repeats(v: unknown, at: string): void {
  if (!Array.isArray(v) || 0 === v.length) {
    throw new RulesError('rules_value', at, 'is not a list of repetitions')
  }
  v.forEach((r, i) => {
    const rat = `${at}[${i}]`
    if (!isObject(r)) throw new RulesError('rules_value', rat, 'is not a repetition')
    members(r, [['min', true, count], ['max', false, count]], rat)
    if (undefined !== r.max && (r.max as number) < (r.min as number)) {
      throw new RulesError('rules_value', `${rat}.max`, 'is less than min')
    }
  })
}

function classMembers(v: unknown, at: string): void {
  if (!Array.isArray(v)) throw new RulesError('rules_value', at, 'is not a list')
  v.forEach((m, i) => {
    const mat = `${at}[${i}]`
    if (!isObject(m)) throw new RulesError('rules_value', mat, 'is not a span of code points')
    members(m, [['from', true, codePoint], ['to', true, codePoint]], mat)
    if ((m.to as number) < (m.from as number)) {
      throw new RulesError('rules_value', `${mat}.to`, 'is less than from')
    }
  })
}

const ELEMENTS: Record<string, Member[]> = {
  reference: [['name', true, name]],
  literal: [['text', true, text], ['caseSensitive', true, bool]],
  char: [['value', true, codePoint]],
  range: [['from', true, codePoint], ['to', true, codePoint]],
  class: [['negated', true, bool], ['members', true, classMembers]],
  any: [],
  token: [['negated', true, bool], ['text', true, text]],
  prose: [['text', true, text]],
  group: [['alternatives', true, alternatives]],
}

function element(v: unknown, at: string): void {
  if (!isObject(v)) throw new RulesError('rules_value', at, 'is not an element')
  const type = v.type
  const list = 'string' === typeof type ? ELEMENTS[type] : undefined
  if (undefined === list || 'type' !== Object.keys(v)[0]) {
    throw new RulesError('rules_type', at,
      `is no element: its first member is not a type an element has (${JSON.stringify(type)})`)
  }
  members(v, [['type', true, () => {}], ...list, ['repeat', false, repeats]], at)
  if ('range' === type && (v.to as number) < (v.from as number)) {
    throw new RulesError('rules_value', `${at}.to`, 'is less than from')
  }
}

function alternatives(v: unknown, at: string): void {
  if (!Array.isArray(v) || 0 === v.length) {
    throw new RulesError('rules_value', at, 'is not a list of alternatives')
  }
  v.forEach((alt, i) => {
    const aat = `${at}[${i}]`
    if (!Array.isArray(alt)) throw new RulesError('rules_value', aat, 'is not a list of elements')
    alt.forEach((el, j) => element(el, `${aat}[${j}]`))
  })
}

function rule(v: unknown, at: string): void {
  if (!isObject(v) || 'rule' !== v.type || 'type' !== Object.keys(v)[0]) {
    throw new RulesError('rules_type', at, 'is no rule: its first member is not the type rule')
  }
  members(v, [
    ['type', true, () => {}],
    ['name', true, name],
    ['incremental', false, yes],
    ['alternatives', true, alternatives],
  ], at)
}

// Hold a value to the schema `grammar-rules`, or fail with a RulesError.
function checkRules(tree: unknown): asserts tree is GrammarRules {
  if (!isObject(tree) || 'grammar' !== tree.type || 'type' !== Object.keys(tree)[0]) {
    throw new RulesError('rules_root', '$', 'is not a grammar: its first member is not the type grammar')
  }
  members(tree, [
    ['type', true, () => {}],
    ['start', false, name],
    ['rules', true, (v, at) => {
      if (!Array.isArray(v)) throw new RulesError('rules_value', at, 'is not a list of rules')
      v.forEach((r, i) => rule(r, `${at}[${i}]`))
    }],
  ], '$')
}

export { checkRules, RulesError, MAX_CODE_POINT }
export type { GrammarRules, RulesRule, RulesElement, RulesRepeat }
