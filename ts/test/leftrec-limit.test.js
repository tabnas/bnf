/* Copyright (c) 2026 Richard Rodger and other contributors, MIT License */

// What one rule may gain from Paull's substitution is bounded
// (MAX_LEFT_RECURSION_EXPANSION), and so is what the whole grammar may gain
// in element occurrences (MAX_LEFT_RECURSION_ELEMENTS). Each bound refuses
// the grammar before the substitution that would pass it is made.
//
// A cycle of k rules, each with two alternatives led by the next one,
// doubles at every substitution: without the first bound, k = 20 exhausted
// memory before elimination finished. A chain of n rules of one alternative
// each, led by the next one, gains no alternatives but copies the rest of
// the chain into every rule: without the second, n = 5,000 held hundreds of
// megabytes. Every count below is exact.

const { describe, it } = require('node:test')
const assert = require('node:assert')

const {
  EmitError,
  emitGrammarSpec,
  eliminateLeftRecursion,
  MAX_LEFT_RECURSION_EXPANSION,
  MAX_LEFT_RECURSION_ELEMENTS,
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


// root = r0 ; r(i) = r(i+1) "x(i)" ; r(n-1) = "end"
const chain = (n) => {
  const productions = [{ name: 'root', alts: [[ref('r0')]] }]
  for (let i = 0; i < n - 1; i++) {
    productions.push({ name: 'r' + i, alts: [[ref('r' + (i + 1)), lit('x' + i)]] })
  }
  productions.push({ name: 'r' + (n - 1), alts: [[lit('end')]] })
  return { productions }
}

// top = s "x1" … "x(tail)", for each name in `tops`; s = m single
// literals. Inlining s into one of them adds m + (m − 1)·tail − 1.
const fan = (tops, m, tail) => ({
  productions: [
    ...tops.map((name) => ({
      name,
      alts: [[ref('s'), ...Array.from({ length: tail }, (_, i) => lit('x' + i))]],
    })),
    choice('s', m),
  ],
})

const elements = (rule, source, led, gain, added) =>
  `cyc: rule '${rule}' exceeds the left-recursion element limit of 32768 ` +
  `elements while inlining '${source}', which begins ${led} of its ` +
  `alternatives and would add ${gain} elements to the ${added} already ` +
  `added to the grammar. Factor '${source}' out of the alternatives it begins.`

describe('left-recursion element limit', () => {
  it('is 32768 element occurrences', () => {
    assert.equal(MAX_LEFT_RECURSION_ELEMENTS, 32768)
  })

  it('refuses a chain of five thousand rules within a second', () => {
    const t0 = process.hrtime.bigint()
    assert.throws(
      () => emitGrammarSpec(chain(5000), { tag: 'cyc' }),
      (error) => {
        assert.ok(error instanceof EmitError)
        // The last rule is lifted into a token, and r(4998 - g) inlines
        // r(4999 - g) with g + 1 elements, adding g. The substitutions add
        // 1 + 2 + … + 255 = 32640, and the one adding 256 passes the limit.
        assert.equal(error.message, elements('r4742', 'r4743', 1, 256, 32640))
        assert.equal(error.rule, 'r4742')
        return true
      },
    )
    const ms = Number(process.hrtime.bigint() - t0) / 1e6
    assert.ok(ms < 1000, `refused after ${ms.toFixed(0)} ms`)
  })

  it('refuses the chain in the standalone pass as well', () => {
    // Nothing is lifted here, and r4998 inlines "end" one for one, adding
    // nothing. The prefix is whatever the last emit set.
    const want = elements('r4742', 'r4743', 1, 256, 32640).slice('cyc'.length)
    assert.throws(
      () => eliminateLeftRecursion(chain(5000)),
      (error) => error instanceof EmitError && error.message.endsWith(want),
    )
  })

  it('admits the longest chain the limit allows', () => {
    // n = 257 adds 1 + 2 + … + 255 = 32640; n = 258 would add 256 more.
    const spec = emitGrammarSpec(chain(257), { tag: 'cyc' })
    assert.equal(spec.rule.r0.open[0].s.split(' ').length, 257)
    assert.throws(
      () => emitGrammarSpec(chain(258), { tag: 'cyc' }),
      { message: elements('r0', 'r1', 1, 256, 32640) },
    )
  })

  it('counts a tail once per alternative of the rule inlined before it', () => {
    // 65 + 64 × 511 − 1 = 32768 fills the limit; a tail of 512 adds 32832.
    const at = eliminateLeftRecursion(fan(['top'], 65, 511))
    assert.equal(at.productions[0].alts.length, 65)
    assert.throws(
      () => emitGrammarSpec(fan(['top'], 65, 512), { tag: 'cyc' }),
      { message: elements('top', 's', 1, 32832, 0) },
    )
  })

  it('sums every substitution in the grammar', () => {
    // Each rule adds 65 + 64 × 255 − 1 = 16384: two fill the limit.
    eliminateLeftRecursion(fan(['t1', 't2'], 65, 255))
    assert.throws(
      () => emitGrammarSpec(fan(['t1', 't2', 't3'], 65, 255), { tag: 'cyc' }),
      { message: elements('t3', 's', 1, 16384, 32768) },
    )
  })

  it('floors at zero what an alternative replaced by fewer elements adds', () => {
    // Inlining e, one empty alternative, first takes an element out of each
    // of 100 alternatives. That frees nothing: s still adds 32832.
    const g = fan(['top'], 65, 512)
    g.productions[0].alts.unshift(
      ...Array.from({ length: 100 }, (_, i) => [ref('e'), lit('y' + i)]))
    g.productions.push({ name: 'e', alts: [[]] })
    assert.throws(
      () => emitGrammarSpec(g, { tag: 'cyc' }),
      { message: elements('top', 's', 1, 32832, 0) },
    )
  })

  it('charges nothing for a token class, which is inlined as its token', () => {
    // Inlined member by member, s would add 32832.
    assert.throws(
      () => emitGrammarSpec(fan(['top'], 65, 512), { tag: 'cyc' }),
      { message: elements('top', 's', 1, 32832, 0) },
    )
    const spec = emitGrammarSpec(fan(['top'], 65, 512), { tag: 'cyc', tokenClasses: true })
    assert.equal(spec.options.tokenSet.s.length, 65)
  })
})
