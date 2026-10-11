/* Copyright (c) 2026 Richard Rodger and other contributors, MIT License */

// What one rule may gain from Paull's substitution is bounded
// (MAX_LEFT_RECURSION_EXPANSION), and so is what the whole grammar may gain
// in elements and alternatives (MAX_LEFT_RECURSION_ELEMENTS). Each bound
// refuses the grammar before the step that would pass it is made.
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

// top = s "x0" … "x(tail - 1)", for each name in `tops`; s = m single
// literals. Inlining s into one of them adds (m − 1)·(1 + tail) + m − 1.
const fan = (tops, m, tail) => ({
  productions: [
    ...tops.map((name) => ({
      name,
      alts: [[ref('s'), ...Array.from({ length: tail }, (_, i) => lit('x' + i))]],
    })),
    choice('s', m),
  ],
})

// A group holding one alternative of k single literals.
const groupOf = (k) => ({
  kind: 'group',
  alts: [Array.from({ length: k }, (_, i) => lit('g' + i))],
})

// A group of k empty alternatives.
const emptyGroup = (k) => ({
  kind: 'group',
  alts: Array.from({ length: k }, () => []),
})

// A = [x0] … [x(k - 1)] A "y" / "z": a hidden left recursion behind k
// options.
const optionalPrefix = (k) => ({
  productions: [{
    name: 'A',
    alts: [
      [...Array.from({ length: k }, (_, i) => ({ kind: 'opt', inner: lit('x' + i) })),
        ref('A'), lit('y')],
      [lit('z')],
    ],
  }],
})

const splitting = (rule, remaining, added) =>
  `cyc: rule '${rule}' exceeds the left-recursion element limit of 131072 ` +
  `elements while splitting the elements that can match nothing before ` +
  `'${rule}' in one of its alternatives, which would add more than ` +
  `${remaining} elements to the ${added} already added to the grammar. ` +
  `Write that alternative with fewer elements that can match nothing ` +
  `before '${rule}'.`

const elements = (rule, source, led, remaining, added) =>
  `cyc: rule '${rule}' exceeds the left-recursion element limit of 131072 ` +
  `elements while inlining '${source}', which begins ${led} of its ` +
  `alternatives and would add more than ${remaining} elements to the ` +
  `${added} already added to the grammar. Factor '${source}' out of the ` +
  `alternatives it begins.`

describe('left-recursion element limit', () => {
  it('is 131072', () => {
    assert.equal(MAX_LEFT_RECURSION_ELEMENTS, 131072)
  })

  it('refuses a chain of five thousand rules within a second', () => {
    const t0 = process.hrtime.bigint()
    assert.throws(
      () => emitGrammarSpec(chain(5000), { tag: 'cyc' }),
      (error) => {
        assert.ok(error instanceof EmitError)
        // The last rule is lifted into a token, and r(4998 - g) inlines
        // r(4999 - g) with g + 1 elements, adding g. The substitutions add
        // 1 + 2 + … + 511 = 130816, and the one adding 512 passes the limit.
        assert.equal(error.message, elements('r4486', 'r4487', 1, 256, 130816))
        assert.equal(error.rule, 'r4486')
        return true
      },
    )
    const ms = Number(process.hrtime.bigint() - t0) / 1e6
    assert.ok(ms < 1000, `refused after ${ms.toFixed(0)} ms`)
  })

  it('refuses the chain in the standalone pass as well', () => {
    // Nothing is lifted here, and r4998 inlines "end" one for one, adding
    // nothing. The prefix is whatever the last emit set.
    const want = elements('r4486', 'r4487', 1, 256, 130816).slice('cyc'.length)
    assert.throws(
      () => eliminateLeftRecursion(chain(5000)),
      (error) => error instanceof EmitError && error.message.endsWith(want),
    )
  })

  it('refuses a chain of a hundred thousand rules on either path, not the stack', () => {
    // Every walk that runs before the bound has a stack of its own, so the
    // chain is the bound's to refuse.
    const g = chain(100000)
    const t0 = process.hrtime.bigint()
    assert.throws(
      () => emitGrammarSpec(g, { tag: 'cyc' }),
      { message: elements('r99486', 'r99487', 1, 256, 130816) },
    )
    const want = elements('r99486', 'r99487', 1, 256, 130816).slice('cyc'.length)
    assert.throws(
      () => eliminateLeftRecursion(g),
      (error) => error instanceof EmitError && error.message.endsWith(want),
    )
    const ms = Number(process.hrtime.bigint() - t0) / 1e6
    assert.ok(ms < 10000, `refused after ${ms.toFixed(0)} ms`)
  })

  it('admits the longest chain the limit allows', () => {
    // n = 513 adds 1 + 2 + … + 511 = 130816; n = 514 would add 512 more.
    const spec = emitGrammarSpec(chain(513), { tag: 'cyc' })
    assert.equal(spec.rule.r0.open[0].s.split(' ').length, 513)
    assert.throws(
      () => emitGrammarSpec(chain(514), { tag: 'cyc' }),
      { message: elements('r0', 'r1', 1, 256, 130816) },
    )
  })

  it('counts a tail once per alternative of the rule inlined before it', () => {
    // 128 × (1 + 1022) + 129 − 1 = 131072 fills the limit; a tail of 1023
    // adds 131200.
    const at = eliminateLeftRecursion(fan(['top'], 129, 1022))
    assert.equal(at.productions[0].alts.length, 129)
    assert.throws(
      () => emitGrammarSpec(fan(['top'], 129, 1023), { tag: 'cyc' }),
      { message: elements('top', 's', 1, 131072, 0) },
    )
  })

  it('counts everything a group in a tail holds', () => {
    // The tail is one element, a group whose one alternative holds k
    // literals, so it counts 1 + 1 + k:
    // 512 × (1 + 2 + 252) + 513 − 1 = 131072, and a group of 253 adds 131584.
    const tailGroup = (k) => {
      const g = fan(['top'], 513, 0)
      g.productions[0].alts[0].push(groupOf(k))
      return g
    }
    const at = eliminateLeftRecursion(tailGroup(252))
    assert.equal(at.productions[0].alts.length, 513)
    assert.throws(
      () => emitGrammarSpec(tailGroup(253), { tag: 'cyc' }),
      { message: elements('top', 's', 1, 131072, 0) },
    )
  })

  it('counts each alternative of a group in a tail, empty or not', () => {
    // The tail is a group of k empty alternatives, so it counts 1 + k:
    // 512 × (1 + 1 + 253) + 513 − 1 = 131072, and 254 of them add 131584.
    const tailGroup = (k) => {
      const g = fan(['top'], 513, 0)
      g.productions[0].alts[0].push(emptyGroup(k))
      return g
    }
    const at = eliminateLeftRecursion(tailGroup(253))
    assert.equal(at.productions[0].alts.length, 513)
    assert.throws(
      () => emitGrammarSpec(tailGroup(254), { tag: 'cyc' }),
      { message: elements('top', 's', 1, 131072, 0) },
    )
  })

  it('counts everything a group in the inlined rule holds', () => {
    // s is one alternative holding a group of 500, which counts 502: each
    // rule that begins with it adds 501, so 261 fit and the 262nd passes the
    // limit.
    const g = (n) => ({
      productions: [
        ...Array.from({ length: n }, (_, i) => ({ name: 't' + i, alts: [[ref('s'), lit('x')]] })),
        { name: 's', alts: [[groupOf(500)]] },
      ],
    })
    eliminateLeftRecursion(g(261))
    assert.throws(
      () => emitGrammarSpec(g(262), { tag: 'cyc' }),
      { message: elements('t261', 's', 1, 311, 130761) },
    )
  })

  it('counts each alternative the inlined rule writes', () => {
    // s is 513 empty alternatives: each rule that begins with it gains 512
    // of them, within the alternative limit, and adds 511 to the grammar,
    // so 256 fit and the 257th passes the limit.
    const g = (n) => ({
      productions: [
        ...Array.from({ length: n }, (_, i) => ({ name: 't' + i, alts: [[ref('s')], [lit('x')]] })),
        { name: 's', alts: Array.from({ length: 513 }, () => []) },
      ],
    })
    const at = eliminateLeftRecursion(g(256))
    assert.equal(at.productions[0].alts.length, 514)
    assert.throws(
      () => emitGrammarSpec(g(257), { tag: 'cyc' }),
      { message: elements('t256', 's', 1, 256, 130816) },
    )
  })

  it('charges splitting off what can match nothing before a hidden left recursion', () => {
    // Splitting the i-th of k options off writes the rest of the
    // alternative again, 2(k − i) + 2 elements. k = 361 adds
    // 361 × 362 = 130682; k = 362 passes the limit at its 345th split,
    // with 344 × 381 = 131064 added.
    const at = eliminateLeftRecursion(optionalPrefix(361))
    // A = ("x0" [x1] … A "y" | … | "z") "y"*: 361 split alternatives and "z".
    assert.equal(at.productions[0].alts[0][0].alts.length, 362)
    assert.throws(
      () => emitGrammarSpec(optionalPrefix(362), { tag: 'cyc' }),
      { message: splitting('A', 8, 131064) },
    )
  })

  it('refuses five thousand options before a hidden left recursion within a second', () => {
    const t0 = process.hrtime.bigint()
    assert.throws(
      () => emitGrammarSpec(optionalPrefix(5000), { tag: 'cyc' }),
      { message: splitting('A', 1228, 129844) },
    )
    const want = splitting('A', 1228, 129844).slice('cyc'.length)
    assert.throws(
      () => eliminateLeftRecursion(optionalPrefix(5000)),
      (error) => error instanceof EmitError && error.message.endsWith(want),
    )
    const ms = Number(process.hrtime.bigint() - t0) / 1e6
    assert.ok(ms < 1000, `refused after ${ms.toFixed(0)} ms`)
  })

  it('sums every substitution in the grammar', () => {
    // Each rule adds 128 × (1 + 400) + 129 − 1 = 51456: two fit, and the
    // third passes the limit.
    eliminateLeftRecursion(fan(['t1', 't2'], 129, 400))
    assert.throws(
      () => emitGrammarSpec(fan(['t1', 't2', 't3'], 129, 400), { tag: 'cyc' }),
      { message: elements('t3', 's', 1, 28160, 102912) },
    )
  })

  it('floors at zero what an alternative replaced by less adds', () => {
    // Inlining e, one empty alternative, first takes an element out of each
    // of 100 alternatives. That frees nothing: s still adds 131200.
    const g = fan(['top'], 129, 1023)
    g.productions[0].alts.unshift(
      ...Array.from({ length: 100 }, (_, i) => [ref('e'), lit('y' + i)]))
    g.productions.push({ name: 'e', alts: [[]] })
    assert.throws(
      () => emitGrammarSpec(g, { tag: 'cyc' }),
      { message: elements('top', 's', 1, 131072, 0) },
    )
  })

  it('charges nothing for a token class, which is inlined as its token', () => {
    // Inlined member by member, s would add 131200.
    assert.throws(
      () => emitGrammarSpec(fan(['top'], 129, 1023), { tag: 'cyc' }),
      { message: elements('top', 's', 1, 131072, 0) },
    )
    const spec = emitGrammarSpec(fan(['top'], 129, 1023), { tag: 'cyc', tokenClasses: true })
    assert.equal(spec.options.tokenSet.s.length, 129)
  })
})
