/* Copyright (c) 2026 Richard Rodger and other contributors, MIT License */

const { describe, it } = require('node:test')
const assert = require('node:assert')

const {
  EmitError,
  emitGrammarSpec,
  MAX_REPEAT_EXPANSION,
} = require('../dist/bnf')

const lit = (literal) => ({ kind: 'term', literal, caseSensitive: true })
const ref = (name) => ({ kind: 'ref', name })
const rep = (min, max, inner) => ({ kind: 'rep', min, max, inner })
const emit = (productions) => emitGrammarSpec(
  { productions }, { tag: 'safe', start: 'top' })

describe('repetition safety', () => {
  it('refuses an unbounded repetition whose item is nullable', () => {
    const sp = { s: 4, e: 18, r: 1, c: 5 }
    assert.throws(
      () => emit([{
        name: 'top', sp,
        alts: [[{
          kind: 'star',
          inner: { kind: 'group', alts: [[lit('a'), lit('b')], []] },
        }]],
      }]),
      (error) => {
        assert.ok(error instanceof EmitError)
        assert.equal(error.rule, 'top')
        assert.deepEqual(error.sp, sp)
        assert.match(error.message, /rule 'top'.*item can match the empty string/)
        return true
      },
    )
  })

  it('finds nullability through a rule reference', () => {
    assert.throws(
      () => emit([
        { name: 'top', alts: [[{ kind: 'plus', inner: ref('empty') }]] },
        { name: 'empty', alts: [[]] },
      ]),
      /unbounded repetition.*empty string/,
    )
  })

  it('refuses a numeric expansion before allocating its helpers', () => {
    assert.equal(MAX_REPEAT_EXPANSION, 8192)
    assert.throws(
      () => emit([{ name: 'top', alts: [[rep(1, 5000, lit('a'))]] }]),
      /rule 'top'.*limit of 8192.*'1\*5000'/,
    )
  })

  it('counts the mandatory prefix of an unbounded numeric repetition', () => {
    assert.throws(
      () => emit([{
        name: 'top',
        alts: [[rep(MAX_REPEAT_EXPANSION + 1, Infinity, lit('a'))]],
      }]),
      /repetition expansion limit/,
    )
  })

  it('counts nested numeric expansions against the same budget', () => {
    assert.throws(
      () => emit([{
        name: 'top',
        alts: [[rep(0, 2200, rep(0, 2200, lit('a')))]],
      }]),
      /repetition expansion limit/,
    )
  })

  it('leaves an invalid regex for the specific terminal diagnostic', () => {
    assert.throws(
      () => emit([{
        name: 'top',
        alts: [[{
          kind: 'star',
          inner: {
            kind: 'group',
            alts: [[lit('a')], [{ kind: 'regex', pattern: '[z-a]', flags: '' }]],
          },
        }]],
      }]),
      /invalid regular expression/i,
    )
  })

  it('allows a bounded nullable item because its bound guarantees termination', () => {
    const spec = emit([{
      name: 'top',
      alts: [[rep(0, 2, { kind: 'group', alts: [[lit('a')], []] })]],
    }])
    assert.ok(spec.rule.top)
  })
})
