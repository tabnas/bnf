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
        assert.match(error.message, /rule 'top'.*without consuming input/)
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
      /unbounded repetition.*without consuming input/,
    )
  })

  it('refuses a regex that can match zero width only in context', () => {
    assert.throws(
      () => emit([{
        name: 'top',
        alts: [[{ kind: 'star', inner: {
          kind: 'regex', pattern: '\\b', flags: '',
        } }]],
      }]),
      /unbounded repetition.*without consuming input/,
    )
  })

  it('allows a boundary regex that must also consume input', () => {
    const spec = emit([{
      name: 'top',
      alts: [[{ kind: 'star', inner: {
        kind: 'regex', pattern: '\\b[a-z]+', flags: '',
      } }]],
    }])
    assert.ok(spec.rule.top)
  })

  it('refuses a quantified group containing only an assertion', () => {
    assert.throws(
      () => emit([{
        name: 'top',
        alts: [[{ kind: 'star', inner: {
          kind: 'regex', pattern: '(?:\\b)+', flags: '',
        } }]],
      }]),
      /unbounded repetition.*without consuming input/,
    )
  })

  it('validates the implicit loop produced by a tail-repeat rewrite', () => {
    const boundary = () => ({ kind: 'regex', pattern: '\\b', flags: '' })
    assert.throws(
      () => emit([
        { name: 'top', alts: [[ref('X')]] },
        { name: 'X', alts: [[
          boundary(),
          { kind: 'opt', inner: {
            kind: 'group', alts: [[boundary(), ref('X')]],
          } },
        ]] },
      ]),
      /rule 'X'.*unbounded tail repetition.*without consuming input/,
    )
  })

  it('attributes a copied left-recursion loop to its source rule', () => {
    const sp = { s: 20, e: 21, r: 2, c: 1 }
    assert.throws(
      () => emit([
        { name: 'top', alts: [[ref('A')]] },
        {
          name: 'A', sp,
          alts: [
            [{ kind: 'opt', inner: lit('x') }, ref('A'),
              { kind: 'opt', inner: lit('y') }],
            [lit('z')],
          ],
        },
      ]),
      (error) => {
        assert.equal(error.rule, 'A')
        assert.deepEqual(error.sp, sp)
        assert.match(error.message, /rule 'A'.*unbounded repetition/)
        return true
      },
    )
  })

  it('attributes a nullable grouped tail to the alternative that made it nullable', () => {
    const csp = { s: 40, e: 41, r: 4, c: 1 }
    assert.throws(
      () => emit([
        { name: 'top', alts: [[ref('A')]] },
        { name: 'A', alts: [[ref('B')], [ref('C')], [lit('z')]] },
        { name: 'B', alts: [[ref('A'), lit('x')]] },
        { name: 'C', sp: csp,
          alts: [[ref('A'), { kind: 'opt', inner: lit('y') }]] },
      ]),
      (error) => {
        assert.equal(error.rule, 'C')
        assert.deepEqual(error.sp, csp)
        return true
      },
    )
  })

  it('rejects negative, fractional, and inverted repetition bounds', () => {
    for (const [min, max] of [[-2, -2], [0.5, 2], [3, 2]]) {
      assert.throws(
        () => emit([{ name: 'top', alts: [[rep(min, max, lit('a'))]] }]),
        /invalid repetition bounds/,
      )
    }
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
