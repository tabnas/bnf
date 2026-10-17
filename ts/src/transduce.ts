/* Copyright (c) 2026 Richard Rodger and other contributors, MIT License */

/*  transduce.ts
 *  The transducer part of an emitted grammar: an alchemy program that
 *  makes the JSON events of the value the grammar builds from its rule
 *  passes (alchemy's `ParserEvents/1`), written from the value actions the
 *  compiler attached for the grammar's `; @object` and `; @array`
 *  annotations, so that a host streams a document of a custom grammar as
 *  it streams one of a format with a part of its own.
 *
 *  Notation-neutral, as spec.ts is: it reads an emitted `GrammarSpec`,
 *  whichever front-end the grammar came from, and the two emit modes
 *  alike, since the value actions are builtins in both and the tree
 *  builders, which it does not read, are the only difference.
 *
 *  The part is alchemy's declarative `transduce-tree` with a spec keyed by
 *  rule name, each entry what that rule's value actions do:
 *
 *    - `@object$` or `@array$` on a rule's open alternates begins its
 *      container (`:container`). A rule its close alternates replace it
 *      with carries that node on, as each step of the compiler's chain
 *      carries it, so it is a container rule of the same kind, and
 *      `transduce-tree` hands the container on through the replacement;
 *    - `@key$` with a constant (`lit`) names the member the rule's link
 *      fills (`:member`), after the container its rule begins;
 *    - `@setval$` or `@push$` with `src` on its close alternates fills the
 *      member with the text the rule it pushed matched (`:text`); without
 *      `src` the member is that rule's own value, which its own passes
 *      write.
 *
 *  Where the passes cannot write the value in the order the value holds
 *  it, the grammar is read whole, and the result says why:
 *
 *    - a start rule with no annotation builds the `{rule, src, kids}`
 *      tree, whose `src` comes before its `kids` and holds the text of
 *      every node beneath it, so no stream writes the root before the
 *      document has ended;
 *    - a member that is a rule's text with a value rule beneath it, whose
 *      events the text does not hold;
 *    - value actions this file does not read, alternates of one rule that
 *      build its value differently, a container or a text whose rule an
 *      open alternate replaces (its close, which ends the one or yields
 *      the other, never runs), a container handed to a rule that begins
 *      one of its own or one of the other kind, and a name alchemy cannot
 *      write as a keyword.
 *
 *  The text is the same in every runtime: the entries in byte order of
 *  their rule names, a member name quoted as below, every refusal
 *  checked in the same order.
 */

import type { GrammarSpec } from '@tabnas/parser'


// The part, or why the grammar is read whole.
export type TransducePart =
  | {
    // The definition the part makes: `<name>-transduce`, a function of
    // the input's parser events.
    readonly entry: string
    // The part's alchemy source.
    readonly source: string
  }
  | {
    // Why the grammar's documents are read whole.
    readonly whole: string
  }


// What one rule's value actions do.
type Entry = {
  container?: 'object' | 'array'
  // The container is the one a rule it replaced handed on.
  continues?: boolean
  member?: string
  // What the member its close alternates fill holds: the text of the rule
  // it pushed, or that rule's own value.
  fills?: 'text' | 'value'
}

// The engine's value builders. The compiler emits the first five; the
// other two are a refusal here.
const VALUE_ACTIONS = new Set([
  '@object$', '@array$', '@key$', '@setval$', '@push$', '@value$', '@reset$',
])

// A name alchemy reads as a keyword's, and as a definition's: a symbol
// that is not a number, and not one of the value words. Every name the
// compiler gives a rule is one.
const SYMBOL = /^[A-Za-z_][A-Za-z0-9_.$-]*$/
const VALUE_WORDS = new Set(['true', 'false', 'null'])

function isSymbol(name: string): boolean {
  return SYMBOL.test(name) && !VALUE_WORDS.has(name)
}

// A rule state's alternates, in either shape a spec may hold them.
function altsOf(state: unknown): any[] {
  if (Array.isArray(state)) return state
  if (null != state && 'object' === typeof state && Array.isArray((state as any).alts)) {
    return (state as any).alts
  }
  return []
}

// The rule an alternate pushes or replaces its rule with, if it names one.
function named(alt: any, field: 'p' | 'r'): string | undefined {
  const name = alt?.[field]
  return 'string' === typeof name && '' !== name ? name : undefined
}

// An alternate's value actions, each with the config the engine binds it
// (`k.key$` for `@key$`), as one comparable signature.
function signatureOf(alt: any): string {
  const a = alt?.a
  const refs: string[] = 'string' === typeof a ? [a] : Array.isArray(a) ? a : []
  const acts = refs.filter((ref) => VALUE_ACTIONS.has(ref))
  return JSON.stringify(acts.map((act) => [act, alt?.k?.[act.slice(1)] ?? null]))
}

// An alchemy string: double quotes, a backslash before a quote or a
// backslash, a control character as `\u00XX`, and every other character as
// it is.
function quoted(text: string): string {
  let out = '"'
  for (const ch of text) {
    const code = ch.codePointAt(0)!
    if ('"' === ch || '\\' === ch) out += '\\' + ch
    else if (code < 0x20) out += '\\u' + code.toString(16).padStart(4, '0')
    else out += ch
  }
  return out + '"'
}

function whole(why: string): TransducePart {
  return { whole: why }
}


// The transducer part of an emitted grammar, defining `<name>-transduce`;
// or why its documents are read whole.
export function transduceAlc(spec: GrammarSpec, name: string): TransducePart {
  if (!isSymbol(name)) {
    return whole(`the name ${quoted(name)} is not one alchemy can give a definition`)
  }
  const rules: Record<string, any> = spec.rule ?? {}
  const names = Object.keys(rules).filter((rule) => null != rules[rule]).sort()

  // What each rule's value actions do.
  const entries = new Map<string, Entry>()
  for (const rule of names) {
    const entry: Entry = {}
    for (const side of ['open', 'close'] as const) {
      const signatures = new Set(altsOf(rules[rule][side]).map(signatureOf))
      if (1 < signatures.size) {
        return whole(`rule ${rule}'s ${side} alternates build its value differently`)
      }
      for (const [act, cfg] of signatures.size ? JSON.parse([...signatures][0]) : []) {
        if ('open' === side && ('@object$' === act || '@array$' === act) && undefined === entry.container) {
          entry.container = '@object$' === act ? 'object' : 'array'
        }
        else if ('open' === side && '@key$' === act && 'string' === typeof cfg?.lit && undefined === entry.member) {
          entry.member = cfg.lit
        }
        else if ('close' === side && ('@setval$' === act || '@push$' === act) && undefined === entry.fills) {
          entry.fills = true === cfg?.src ? 'text' : 'value'
        }
        else {
          return whole(`rule ${rule}'s ${side} alternates use ${act} as no transducer part reads it`)
        }
      }
    }
    if (undefined !== entry.container || undefined !== entry.member || undefined !== entry.fills) {
      entries.set(rule, entry)
    }
  }

  // The rules a container rule's close replaces it with carry the
  // container on, and the rules theirs replace them with, to a fixed point.
  for (let changed = true; changed;) {
    changed = false
    for (const rule of names) {
      const entry = entries.get(rule)
      if (undefined === entry?.container) continue
      for (const alt of altsOf(rules[rule].close)) {
        const next = named(alt, 'r')
        if (undefined === next) continue
        if (null == rules[next]) {
          return whole(`rule ${rule} hands its ${entry.container} on to rule ${next}, which the grammar does not define`)
        }
        const target = entries.get(next) ?? {}
        if (undefined === target.container) {
          target.container = entry.container
          target.continues = true
          entries.set(next, target)
          changed = true
        }
        else if (target.container !== entry.container) {
          return whole(`rule ${rule} hands its ${entry.container} on to rule ${next}, a rule of ${'object' === target.container ? 'an object' : 'an array'}`)
        }
        else if (!target.continues) {
          return whole(`rule ${rule} hands its ${entry.container} on to rule ${next}, which begins one of its own`)
        }
      }
    }
  }

  const sorted = [...entries.keys()].sort()
  for (const rule of sorted) {
    if (!isSymbol(rule)) {
      return whole(`rule ${quoted(rule)} has no name alchemy can write as a keyword`)
    }
  }

  // A container ends, and a text is yielded, on its rule's close, which an
  // open alternate that replaces the rule skips.
  for (const rule of sorted) {
    const entry = entries.get(rule)!
    if (undefined === entry.container && 'text' !== entry.fills) continue
    for (const alt of altsOf(rules[rule].open)) {
      if (undefined !== named(alt, 'r')) {
        return whole(`rule ${rule}'s open alternate replaces it, so its close never runs`)
      }
    }
  }

  // A member that is a rule's text holds no value of its own: a value rule
  // beneath it builds what the text does not hold.
  const beneath = (start: string): string | undefined => {
    const seen = new Set<string>()
    const stack = [start]
    while (0 < stack.length) {
      const rule = stack.pop()!
      if (seen.has(rule) || null == rules[rule]) continue
      seen.add(rule)
      if (entries.has(rule)) return rule
      const next: string[] = []
      for (const side of ['open', 'close'] as const) {
        for (const alt of altsOf(rules[rule][side])) {
          for (const field of ['p', 'r'] as const) {
            const to = named(alt, field)
            if (undefined !== to) next.push(to)
          }
        }
      }
      stack.push(...next.reverse())
    }
    return undefined
  }
  for (const rule of sorted) {
    if ('text' !== entries.get(rule)!.fills) continue
    for (const alt of altsOf(rules[rule].open)) {
      const pushed = named(alt, 'p')
      if (undefined === pushed) continue
      const found = beneath(pushed)
      if (undefined !== found) {
        return whole(`rule ${rule}'s member is the text of rule ${pushed}, beneath which rule ${found} builds a value of its own`)
      }
    }
  }

  // The value is the start rule's, which the compiler's start wrapper
  // pushes.
  const wrapper = spec.options?.rule?.start
  const start = 'string' === typeof wrapper && null != rules[wrapper]
    ? altsOf(rules[wrapper].open).map((alt) => named(alt, 'p')).find((p) => undefined !== p)
    : undefined
  if (undefined === start) {
    return whole('the grammar has no start rule a wrapper pushes')
  }
  const root = entries.get(start)
  if (undefined === root?.container || root.continues) {
    return whole(
      `the start rule ${start} has no annotation, so the value is the {rule, src, kids} tree, ` +
      'whose src comes before its kids and holds the text of every node beneath it: ' +
      'no stream writes the root before the document has ended')
  }

  const entry = `${name}-transduce`
  const lines = [
    `; The transducer part of the grammar ${name}: the JSON events of the value`,
    '; its @object and @array annotations build, from its rule passes, as',
    '; tabnas-bnf wrote them from the value actions those annotations attach.',
    `def ${entry} [input]`,
    '  transduce-tree',
    '    record',
  ]
  for (const rule of sorted) {
    const e = entries.get(rule)!
    const fields: string[] = []
    if (undefined !== e.container) fields.push(`(entry :container :${e.container})`)
    if (undefined !== e.member) fields.push(`(entry :member ${quoted(e.member)})`)
    if ('text' === e.fills) fields.push('(entry :text true)')
    if (0 < fields.length) lines.push(`      entry :${rule} (record ${fields.join(' ')})`)
  }
  lines.push('    input', '')
  return { entry, source: lines.join('\n') }
}
