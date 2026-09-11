/* Copyright (c) 2026 Richard Rodger and other contributors, MIT License */
'use strict'

// The limits of the overlapping-class partition, driven through the IR
// rather than a notation.
//
// These cases exist because the first cut of the partition got them
// wrong, and no front-end in the fleet can express them: ABNF's `%x`
// ranges are always single-code-point and case-sensitive, so its suite —
// the usual oracle for this compiler — cannot reach a multi-character
// regex terminal or a case-insensitive class at all. A notation-neutral
// compiler has to be safe for the front-ends that can.

const { describe, it } = require('node:test')
const assert = require('node:assert')

const { emitGrammarSpec } = require('..')

const rx = (pattern, flags = '') => ({ kind: 'regex', pattern, flags })
const term = (literal) => ({ kind: 'term', literal })

const emit = (productions, opts) =>
  emitGrammarSpec({ productions }, Object.assign({ tag: 't' }, opts))

const matchers = (spec) =>
  Object.fromEntries(
    Object.entries(spec.options.match?.token ?? {}).map(([n, re]) => [n, String(re)]))

describe('overlapping-class partition: what it may touch', () => {
  // patternCharRanges answers "what can this pattern's FIRST character
  // be?" — right for contest detection, wrong for deciding a class's
  // full coverage. Partitioning REPLACES the matcher with one-character
  // atoms, so a pattern that matches more than one code point would lose
  // the rest of itself. Each of these three lost something real.
  const MULTI = [
    ['top-level alternation', 'a|bc'],
    ['quantifier', '[a-z]+'],
    ['two classes in sequence', '[aA][bB]'],
  ]

  for (const [label, pattern] of MULTI) {
    it(`leaves a multi-character regex alone: ${label}`, () => {
      // `[a]` overlaps the first character of every pattern above, so
      // without the guard each one becomes "contested" and is replaced.
      const spec = emit([
        { name: 'top', alts: [[rx(pattern)], [rx('[a]')]] },
      ])
      const src = Object.values(matchers(spec))
      assert.ok(
        src.some((s) => s.includes(pattern)),
        `${pattern} lost its matcher; emitted ${JSON.stringify(src)}`,
      )
      assert.deepEqual(
        spec.options.tokenSet ?? {}, {},
        'a pattern that can match more than one code point must not be partitioned',
      )
    })
  }

  it('leaves a case-insensitive class alone', () => {
    // foldCaseRanges folds ASCII A-Z/a-z and nothing else, so atoms
    // derived from `[é]/i` would cover `é` but not `É` — the matcher
    // would say one thing and the ranges another.
    const spec = emit([
      { name: 'top', alts: [[rx('[\\u00e9]', 'i')], [rx('[\\u00e9]')]] },
    ])
    const insensitive = Object.values(spec.options.match.token)
      .filter((re) => re.flags.includes('i'))
    assert.equal(insensitive.length, 1, 'the /i matcher must survive')
    assert.ok(insensitive[0].test('É'), 'and must still cover É')
    assert.deepEqual(spec.options.tokenSet ?? {}, {})
  })

  it('still partitions two plain single-code-point classes', () => {
    // The guard above must not have disarmed the fix itself.
    const spec = emit([
      { name: 'top', alts: [[rx('[\\u0030-\\u0039]')], [rx('[\\u0031-\\u0039]')]] },
    ])
    const sets = spec.options.tokenSet ?? {}
    assert.deepEqual(
      Object.keys(sets).sort(),
      ['RX___U0030__U0039', 'RX___U0031__U0039'],
    )
  })
})

describe('overlapping-class partition: names stay put', () => {
  it('a contested class keeps its own token name', () => {
    // Marks come from altDiscriminator, which reads the token name out
    // of regexTokens. Pointing a one-atom class at the atom renamed it,
    // so a user action bound to `@top:o:<mark>` silently detached the
    // moment some OTHER production mentioned an overlapping class.
    const alts = [[rx('[123456789]'), term('x')], [term('y')]]
    const alone = emit([{ name: 'top', alts }], { marks: true })
    const contested = emit([
      { name: 'top', alts },
      { name: 'other', alts: [[rx('[0-9]')]] },
    ], { marks: true })

    assert.deepEqual(
      contested.rule.top.open.map((a) => a.m ?? null),
      alone.rule.top.open.map((a) => a.m ?? null),
      'an unrelated overlapping class must not move this rule’s marks',
    )
    assert.deepEqual(
      contested.rule.top.open.map((a) => a.s ?? null),
      alone.rule.top.open.map((a) => a.s ?? null),
    )
  })

  it('atoms are named apart from classes, so neither displaces the other', () => {
    // An atom minted as `rx_<pattern>` collides with the natural name of
    // any class spelling the same span: `%x31-39`'s atom took
    // `#RX___U0031__U0039` first and pushed the class to a suffixed name.
    const spec = emit([
      { name: 'top', alts: [[rx('[\\u0030-\\u0039]')], [rx('[\\u0031-\\u0039]')]] },
    ])
    assert.deepEqual(
      spec.rule.top.open.map((a) => a.s),
      ['#RX___U0030__U0039', '#RX___U0031__U0039'],
      'both classes keep the name they would have had unpartitioned',
    )
    for (const n of Object.keys(spec.options.match.token)) {
      assert.ok(n.startsWith('#RXA'), `${n} should be an atom token`)
    }
  })
})
