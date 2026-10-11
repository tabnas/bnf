/* Copyright (c) 2026 Richard Rodger and other contributors, MIT License */

// Left factoring and the specificity ordering ask how many tokens a
// sequence can span (seqTokenSpan), and only up to LOOKAHEAD_K. The walk
// that answers keeps its own stack and one set of the rules on its way, and
// stops as soon as a sequence passes what the window leaves it.
//
// The walk it replaced recursed once per rule it followed, copied the path
// at every one, and followed every alternative of every rule to the end.
// On a chain of rules each ending in the next, at the head of two
// alternatives, TypeScript ran out of stack between 2,000 and 2,100 rules,
// and a chain of rules of two alternatives each took twice as long for
// every rule more: 2.8 s at 24 rules.

const { describe, it } = require('node:test')
const assert = require('node:assert')

const { emitGrammarSpec } = require('../dist/bnf')

const lit = (literal) => ({ kind: 'term', literal, caseSensitive: true })
const ref = (name) => ({ kind: 'ref', name })

// top = c0 "f0" / c0 "f1" ; c(i) = "g(i)" c(i+1) / … ; c(n-1) = "g(n-1)".
// The two alternatives of top share their first element, so left factoring
// asks how far c0 reaches. With `two`, each rule of the chain but the last
// has a second alternative, "h(i)" c(i+1).
const chain = (n, two) => ({
  productions: [
    { name: 'top', alts: [[ref('c0'), lit('f0')], [ref('c0'), lit('f1')]] },
    ...Array.from({ length: n }, (_, i) => ({
      name: 'c' + i,
      alts: i + 1 < n
        ? [
          [lit('g' + i), ref('c' + (i + 1))],
          ...(two ? [[lit('h' + i), ref('c' + (i + 1))]] : []),
        ]
        : [[lit('g' + i)]],
    })),
  ],
})

describe('token span walk', () => {
  it('follows a chain of 30,000 rules no further than the lookahead', () => {
    const spec = emitGrammarSpec(chain(30000, false), { tag: 'x' })
    // The last rule, one literal, is lifted into a token; the one before it
    // is the deepest rule left.
    assert.ok(null != spec.rule.c29998)
    // The most this test's process has held, the compile included: about
    // 210 MB where it was measured. A walk that grew with the square of the
    // chain would want tens of gigabytes.
    const peak = process.resourceUsage().maxRSS * 1024
    assert.ok(peak < 1024 * 1024 * 1024,
      `the process held ${Math.round(peak / 1048576)} MB`)
  })

  it('follows each alternative of a rule no further than the lookahead', () => {
    // 30 rules of two alternatives each: 2³⁰ paths to the end of the chain,
    // and at most 2⁵ within the window.
    const t0 = process.hrtime.bigint()
    const spec = emitGrammarSpec(chain(30, true), { tag: 'x' })
    const spent = Number(process.hrtime.bigint() - t0) / 1e6
    assert.ok(null != spec.rule.top)
    assert.ok(spent < 5000, `compiled after ${spent.toFixed(0)} ms`)
  })

  it('counts a repetition of at most none as no tokens', () => {
    // Two alternatives share a repetition of at most none, of an unbounded
    // element, and then n literals. The repetition spans no tokens, so the
    // prefix spans n: within the lookahead at 4, and factored at 5. The walk
    // this replaced read the repetition as NaN, which never passed the
    // lookahead; Go's took the same NaN as past it, and Rust's read the
    // repetition as unbounded.
    const none = {
      kind: 'rep', min: 0, max: 0, inner: { kind: 'star', inner: lit('x') },
    }
    const shared = (n) => ({
      productions: [{
        name: 'top',
        alts: ['y', 'z'].map((last) => [
          none, ...Array.from({ length: n }, (_, i) => lit('a' + i)), lit(last),
        ]),
      }],
    })
    const factored = (n) => Object.keys(emitGrammarSpec(shared(n), { tag: 'x' }).rule)
      .filter((name) => name.includes('$fact'))
    assert.deepEqual(factored(4), [])
    assert.deepEqual(factored(5), ['top$fact0'])
  })
})
