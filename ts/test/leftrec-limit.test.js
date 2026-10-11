/* Copyright (c) 2026 Richard Rodger and other contributors, MIT License */

// What one rule may gain from Paull's substitution is bounded
// (MAX_LEFT_RECURSION_EXPANSION), and the bound refuses the grammar before
// the substitution that would pass it is made.
//
// A cycle of k rules, each with two alternatives led by the next one,
// doubles at every substitution: without the bound, k = 20 exhausted
// memory before elimination finished. Every count below is exact.

const { describe, it } = require('node:test')
const assert = require('node:assert')

const {
  EmitError,
  emitGrammarSpec,
  eliminateLeftRecursion,
  MAX_LEFT_RECURSION_EXPANSION,
} = require('../dist/bnf')

const lit = (literal) => ({ kind: 'term', literal, caseSensitive: true })
const ref = (name) => ({ kind: 'ref', name })

// root = r0 ; r(i) = r(i+1) "a(i)" / r(i+1) "b(i)" / "x(i)", wrapping at k.
const cycle = (k) => {
  const productions = [{ name: 'root', alts: [[ref('r0')]] }]
  for (let i = 0; i < k; i++) {
    const next = 'r' + ((i + 1) % k)
    productions.push({
      name: 'r' + i,
      sp: { s: 10 * i, e: 10 * i + 9, r: i + 2, c: 1 },
      alts: [
        [ref(next), lit('a' + i)],
        [ref(next), lit('b' + i)],
        [lit('x' + i)],
      ],
    })
  }
  return { productions }
}

// A rule of n single-literal alternatives.
const choice = (name, n, prefix = name) => ({
  name,
  alts: Array.from({ length: n }, (_, i) => [lit(prefix + i)]),
})

const refusal = (rule, source, led, has) =>
  `cyc: rule '${rule}' exceeds the left-recursion expansion limit of 512 ` +
  `alternatives while inlining '${source}', which begins ${led} of its ` +
  `alternatives and has ${has} of its own. Factor '${source}' out of the ` +
  `alternatives it begins.`

describe('left-recursion expansion limit', () => {
  it('is 512 alternatives', () => {
    assert.equal(MAX_LEFT_RECURSION_EXPANSION, 512)
  })

  it('refuses a cycle of twenty rules within a second', () => {
    const t0 = process.hrtime.bigint()
    assert.throws(
      () => emitGrammarSpec(cycle(20), { tag: 'cyc' }),
      (error) => {
        assert.ok(error instanceof EmitError)
        // The topological order processes r19 first, so r12 has grown to
        // 511 alternatives when r11 inlines it twice: a gain of 1020.
        assert.equal(error.message, refusal('r11', 'r12', 2, 511))
        assert.equal(error.rule, 'r11')
        assert.deepEqual(error.sp, { s: 110, e: 119, r: 13, c: 1 })
        return true
      },
    )
    const ms = Number(process.hrtime.bigint() - t0) / 1e6
    assert.ok(ms < 1000, `refused after ${ms.toFixed(0)} ms`)
  })

  it('refuses the cycle in the standalone pass as well', () => {
    assert.throws(
      () => eliminateLeftRecursion(cycle(20)),
      (error) => error instanceof EmitError &&
        /rule 'r11' exceeds the left-recursion expansion limit/.test(error.message),
    )
  })

  it('admits the cycle while each rule gains at most the limit', () => {
    // k = 8: r0 inlines r1 (255 alternatives) twice, a gain of 508.
    const out = eliminateLeftRecursion(cycle(8))
    const r0 = out.productions.find((p) => p.name === 'r0')
    assert.equal(r0.alts.length, 1)
    assert.equal(r0.alts[0][0].alts.length, 255)
    assert.equal(r0.alts[0][1].inner.alts.length, 256)
    // k = 9: r1 has 511 when r0 inlines it twice, a gain of 1020.
    assert.throws(
      () => emitGrammarSpec(cycle(9), { tag: 'cyc' }),
      { message: refusal('r0', 'r1', 2, 511) },
    )
  })

  it('counts the alternatives a substitution adds, up to the limit and no further', () => {
    // top inlines s once: s's 513 alternatives replace one, a gain of 512.
    const at = { productions: [
      { name: 'top', alts: [[ref('s'), lit('x')]] }, choice('s', 513)] }
    assert.equal(eliminateLeftRecursion(at).productions[0].alts.length, 513)
    const past = { productions: [
      { name: 'top', alts: [[ref('s'), lit('x')]] }, choice('s', 514)] }
    assert.throws(
      () => emitGrammarSpec(past, { tag: 'cyc' }),
      { message: refusal('top', 's', 1, 514) },
    )
  })

  it('sums every substitution into one rule', () => {
    // 299 from `a`, then 299 from `b`: 598.
    const g = { productions: [
      { name: 'top', alts: [[ref('a'), lit('1')], [ref('b'), lit('2')]] },
      choice('a', 300), choice('b', 300)] }
    assert.throws(
      () => emitGrammarSpec(g, { tag: 'cyc' }),
      { message: refusal('top', 'b', 1, 300) },
    )
  })

  it('charges nothing for a rule\'s own alternatives or a one-for-one inlining', () => {
    // 2000 alternatives of its own and 2000 led by a rule of one: no gain.
    const own = choice('top', 2000, 'k')
    own.alts.push(...Array.from({ length: 2000 }, (_, i) => [ref('one'), lit('t' + i)]))
    const g = { productions: [own, { name: 'one', alts: [[lit('o')]] }] }
    assert.equal(eliminateLeftRecursion(g).productions[0].alts.length, 4000)
  })

  it('charges nothing for a token class, which is inlined as its token', () => {
    // Inlined member by member, kw's 600 alternatives would add 1198.
    const g = { productions: [
      { name: 'top', alts: [[ref('kw'), lit('x')], [ref('kw'), lit('y')]] },
      choice('kw', 600, 'w')] }
    assert.throws(
      () => emitGrammarSpec(g, { tag: 'cyc' }),
      { message: refusal('top', 'kw', 2, 600) },
    )
    const spec = emitGrammarSpec(g, { tag: 'cyc', tokenClasses: true })
    assert.equal(spec.options.tokenSet.kw.length, 600)
  })
})
