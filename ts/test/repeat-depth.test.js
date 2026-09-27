/* Copyright (c) 2026 Richard Rodger and other contributors, MIT License */
'use strict'

// Every repetition compiles to a same-depth replace loop (AGENTS.md,
// "Repetition is replacement, never a push chain").
//
// A star, a plus and an unbounded rep are SEQUENCE, so the loop they
// desugar to runs every iteration in one frame: the item may be pushed,
// but the next iteration replaces the loop with itself (`r:`) rather
// than pushing a fresh copy. Rule depth is then bounded by the grammar's
// nesting, never by the input's length. The helper used to be emitted as
// the right recursion it is written as in the IR (`H = item H / ε`, each
// item pushing a new `H`), so a flat file of a few thousand records cost
// a frame per record, tripped the hosts' depth guards, and parsed in
// quadratic time.
//
// The proof is observed, not inferred from the spec: the engine reports
// the depth `d` of every rule it runs (`sub({ ruleDone })`), and the
// deepest one over ten thousand items must be exactly the deepest one
// over a single iteration. Time is the second observable: ten times the
// items must cost about ten times as long.

const { describe, it } = require('node:test')
const assert = require('node:assert')

const { emitGrammarSpec, toRecognitionSpec } = require('..')
const { Tabnas } = require('@tabnas/parser')

const lit = (s) => ({ kind: 'term', literal: s, caseSensitive: true })
const rx = (pattern) => ({ kind: 'regex', pattern, flags: '' })
const ref = (name) => ({ kind: 'ref', name })
const star = (inner) => ({ kind: 'star', inner })
const plus = (inner) => ({ kind: 'plus', inner })
const rep = (min, max, inner) => ({ kind: 'rep', min, max, inner })
const group = (...alts) => ({ kind: 'group', alts })
const prod = (name, ...alts) => ({ name, alts })

const N = 10000

// A parser for the grammar, and a way to read the deepest rule it ran.
const parser = (productions, opts = {}) => {
  const spec = emitGrammarSpec(
    { productions }, { tag: 'depth', start: productions[0].name, ...opts })
  const tn = new Tabnas()
  tn.grammar(opts.recognition ? toRecognitionSpec(spec) : spec)
  let deepest = 0
  tn.sub({ ruleDone: (rule) => { if (deepest < rule.d) deepest = rule.d } })
  return {
    spec,
    parse: (src) => {
      deepest = 0
      const out = tn.parse(src)
      return { out, deepest }
    },
  }
}

// The fastest of a few runs, in milliseconds, so one slow run (a GC, a
// JIT tier-up) does not decide the verdict.
const fastest = (fn, runs = 3) => {
  let best = Infinity
  for (let i = 0; i < runs; i++) {
    const t0 = process.hrtime.bigint()
    fn()
    const ms = Number(process.hrtime.bigint() - t0) / 1e6
    if (ms < best) best = ms
  }
  return best
}

// Linear time: ten times the items cost ten times as long, within a small
// factor. The factor is not 1, and not stable enough to pin near 1: a
// thousand items parse inside the young generation, and ten thousand do
// not, so the larger input pays a few times more per item for memory
// while the work per item is the same. A parse that is quadratic in the
// item count costs a hundred times as long. Measured on one machine, the
// loop costs 15 to 31 times; the push chain it replaced cost 44 to 99
// times wherever the item is a rule, because each level of the chain
// copied the kids of every level below it, and it ran out of memory at a
// hundred thousand items.
const assertLinear = (p, make, label) => {
  const small = make(N / 10)
  const large = make(N)
  for (let i = 0; i < 3; i++) p.parse(small)
  p.parse(large) // warm both sizes before timing either
  const tSmall = Math.max(fastest(() => p.parse(small), 5), 1)
  const tLarge = fastest(() => p.parse(large))
  assert.ok(tLarge < 50 * tSmall,
    `${label}: ${N} items took ${tLarge.toFixed(1)} ms, ${N / 10} took ` +
    `${tSmall.toFixed(1)} ms — ${(tLarge / tSmall).toFixed(1)}x, not linear`)
}

const times = (n, s, sep = '') => Array(n).fill(s).join(sep)

// Each case: the grammar, the input for n items, the smallest n that runs
// one full iteration of every repetition in it, and what the value must
// hold for n items.
const item = prod('item', [rx('[a-z]+')])
const cases = [
  {
    name: '*item',
    grammar: [prod('doc', [star(ref('item'))]), item],
    make: (n) => times(n, 'ab', ' '),
    one: 1,
    check: (out, n) => assert.equal(out.kids.length, n),
  },
  {
    name: '1*item',
    grammar: [prod('doc', [plus(ref('item'))]), item],
    make: (n) => times(n, 'ab', ' '),
    one: 2,
    check: (out, n) => assert.equal(out.kids.length, n),
  },
  {
    name: '3*item',
    grammar: [prod('doc', [rep(3, Infinity, ref('item'))]), item],
    make: (n) => times(n, 'ab', ' '),
    one: 4,
    check: (out, n) => assert.equal(out.kids.length, n),
  },
  {
    name: '*"x" (a terminal item)',
    grammar: [prod('doc', [star(lit('x')), lit(';')])],
    make: (n) => times(n, 'x') + ';',
    one: 1,
    check: (out, n) => assert.equal(out.src.length, n + 1),
  },
  {
    name: 'item *("," item)',
    grammar: [prod('doc', [ref('item'), star(group([lit(','), ref('item')]))]), item],
    make: (n) => times(n, 'ab', ','),
    one: 2,
    check: (out, n) => assert.equal(out.kids.length, n - 1),
  },
  {
    name: '*( item ";" / "!" ) (a star of a group)',
    grammar: [prod('doc', [star(group([ref('item'), lit(';')], [lit('!')]))]), item],
    make: (n) => times(n, 'ab;'),
    one: 1,
    check: (out, n) => assert.equal(out.kids.length, n),
  },
  {
    name: '*row, row = "[" *cell "]" (a star inside the item)',
    grammar: [
      prod('doc', [star(ref('row'))]),
      prod('row', [lit('['), star(ref('cell')), lit(']')]),
      prod('cell', [rx('[a-z]')]),
    ],
    make: (n) => times(n, '[abc]'),
    one: 1,
    check: (out, n) => {
      assert.equal(out.kids.length, n)
      assert.equal(out.kids[n - 1].kids.length, 3)
    },
  },
  {
    name: 'row = "[" *cell "]" (ten thousand cells in one row)',
    grammar: [
      prod('row', [lit('['), star(ref('cell')), lit(']')]),
      prod('cell', [rx('[a-z]')]),
    ],
    make: (n) => '[' + times(n, 'a') + ']',
    one: 1,
    check: (out, n) => assert.equal(out.kids.length, n),
  },
  {
    // Left recursion is rewritten to `seed tail*`: the star it
    // synthesises is a loop like any other, and the tree stays flat.
    name: 'sum = sum "+" item / item (left recursion)',
    grammar: [prod('sum', [ref('sum'), lit('+'), ref('item')], [ref('item')]), item],
    make: (n) => times(n, 'ab', '+'),
    one: 2,
    check: (out, n) => {
      assert.equal(out.rule, 'sum')
      assert.equal(out.kids.length, n - 1)
    },
  },
]

describe('repetition depth', () => {
  for (const c of cases) {
    for (const opts of [{}, { builtins: true }]) {
      const mode = opts.builtins ? 'builtins' : 'closures'
      it(`${c.name}: depth over ${N} items is one item's (${mode})`, () => {
        const p = parser(c.grammar, opts)
        const one = p.parse(c.make(c.one))
        const many = p.parse(c.make(N))
        c.check(many.out, N)
        assert.equal(many.deepest, one.deepest,
          `${N} items reached depth ${many.deepest}; one iteration needs ` +
          `${one.deepest}`)
      })
    }
    it(`${c.name}: time is linear in the item count`, () => {
      assertLinear(parser(c.grammar, { builtins: true }), c.make, c.name)
    })
  }

  it('holds in a recognition-only grammar', () => {
    // The loop's back-edge and its `u.rep` flag are structural, so a spec
    // stripped of every tree builder still loops in one frame.
    const g = [prod('doc', [ref('item'), star(group([lit(','), ref('item')]))]), item]
    const p = parser(g, { builtins: true, recognition: true })
    const one = p.parse(times(2, 'ab', ','))
    const many = p.parse(times(N, 'ab', ','))
    assert.equal(many.deepest, one.deepest)
  })

  it('collects one element per item into an array, at one depth', () => {
    // `list = "(" *( item ";" ) ")"  ; @array` — the array-collection
    // planner hands the loop the array its entry inherited, and each
    // iteration pushes one element into it.
    const g = [
      { name: 'list', alts: [[lit('('), star(group([ref('item'), lit(';')])), lit(')')]], value: { kind: 'array' } },
      item,
    ]
    for (const opts of [{}, { builtins: true }]) {
      const p = parser(g, opts)
      const one = p.parse('(ab;)')
      assert.deepEqual(one.out, ['ab'])
      const many = p.parse('(' + times(N, 'ab;') + ')')
      assert.equal(many.out.length, N)
      assert.ok(many.out.every((e) => 'ab' === e))
      assert.equal(many.deepest, one.deepest)
    }
  })

  it('takes a repetition member as its text, at one depth', () => {
    // An object member that is a star is the run's source text: the loop
    // accumulates into the node its entry allocated, which is what the
    // member reads back, not the object it sits in.
    const g = [
      { name: 'obj', alts: [[lit('<'), star(ref('d')), lit('>'), ref('e')]], value: { kind: 'object', members: ['ds', 'e'] } },
      prod('d', [rx('[0-9]')]),
      prod('e', [lit('!'), lit('?')]),
    ]
    for (const opts of [{}, { builtins: true }]) {
      const p = parser(g, opts)
      assert.deepEqual(p.parse('<>!?').out, { ds: '', e: '!?' })
      const one = p.parse('<7>!?')
      assert.deepEqual(one.out, { ds: '7', e: '!?' })
      const many = p.parse('<' + times(N, '7') + '>!?')
      assert.equal(many.out.ds.length, N)
      assert.equal(many.deepest, one.deepest)
    }
  })

  it('ends a star of something that can match nothing', () => {
    // `*[x]` is `*x`: an iteration whose item matched nothing makes no
    // progress, so the loop does not take one. The push chain took it
    // forever, and failed when the engine's step budget ran out.
    const g = [prod('doc', [star({ kind: 'opt', inner: lit('x') }), lit(';')])]
    const p = parser(g)
    assert.equal(p.parse(';').out.src, ';')
    assert.equal(p.parse('xxx;').out.src, 'xxx;')
    assert.equal(p.parse(times(N, 'x') + ';').deepest, p.parse('x;').deepest)
  })

  it('emits the loop as a replace, never a push of itself', () => {
    // The shape, for `doc = *item`: the entry allocates the node and
    // hands over by replacement; the loop's open takes one item (flagging
    // `u.rep`) or exits; its close replaces the loop with itself when the
    // open took an item.
    const { spec } = parser([prod('doc', [star(ref('item'))]), item], { builtins: true })
    const loop = Object.keys(spec.rule).find((n) => /^_gen\d+_star_item$/.test(n))
    assert.ok(loop, Object.keys(spec.rule).join(' '))
    const entry = loop + '$in'

    assert.deepEqual(spec.rule.doc.open.map((a) => a.p), [entry])
    assert.deepEqual(spec.rule[entry].open.map((a) => a.r), [loop])
    assert.equal(spec.rule[entry].close, undefined)

    const open = spec.rule[loop].open
    const takes = open.filter((a) => null != a.p)
    assert.ok(0 < takes.length)
    for (const a of takes) {
      assert.equal(a.p, 'item', 'an iteration pushes the item')
      assert.deepEqual(a.u, { rep: 1 })
    }
    for (const a of open.filter((a) => null == a.p)) {
      assert.equal(a.u, undefined, 'an exit takes no item')
      assert.equal(a.r, undefined)
    }
    const close = spec.rule[loop].close
    assert.deepEqual(close[0].c, { 'u.rep': 1 })
    assert.equal(close[0].r, loop, 'the back-edge is a replace')
    assert.equal(close[1].r, undefined)
    assert.equal(close[1].c, undefined)

    // Nothing anywhere pushes a loop or re-enters an entry.
    for (const [name, rs] of Object.entries(spec.rule)) {
      for (const a of [...(rs.open ?? []), ...(rs.close ?? [])]) {
        assert.notEqual(a.p, loop, `${name} pushes the loop`)
        if (name !== 'doc') assert.notEqual(a.p, entry, `${name} pushes the entry`)
      }
    }
  })
})
