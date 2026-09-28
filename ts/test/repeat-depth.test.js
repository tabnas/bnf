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

// The cheapest of a few runs, in milliseconds of CPU time, so one slow
// run (a GC, a JIT tier-up) does not decide the verdict. CPU time and not
// wall time: other processes on a loaded machine stretch the wall clock
// of a run without adding to the work it does, and a verdict about the
// work must not depend on them.
//
// The CPU clock is coarse on some platforms (about 15.6 ms on Windows),
// so a single parse of a thousand items can read as zero. Each sample
// therefore repeats the parse until it has used at least SAMPLE_MS of CPU
// and reports the cost per parse, and the fastest of the samples counts.
const SAMPLE_MS = 150
const cpuMs = (t0) => {
  const t = process.cpuUsage(t0)
  return (t.user + t.system) / 1000
}
const fastest = (fn, runs = 3) => {
  let best = Infinity
  for (let i = 0; i < runs; i++) {
    const t0 = process.cpuUsage()
    let n = 0
    let ms = 0
    do {
      fn()
      n++
      ms = cpuMs(t0)
    } while (ms < SAMPLE_MS && n < 10000)
    if (ms / n < best) best = ms / n
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
// hundred thousand items. A measurement that lands above the bound is
// taken again, up to three times, before it counts: a quadratic parse
// is over it every time.
const assertLinear = (p, make, label) => {
  const small = make(N / 10)
  const large = make(N)
  for (let i = 0; i < 3; i++) p.parse(small)
  p.parse(large) // warm both sizes before timing either
  let seen = ''
  for (let attempt = 0; attempt < 3; attempt++) {
    const tSmall = fastest(() => p.parse(small))
    const tLarge = fastest(() => p.parse(large), 2)
    if (tLarge < 50 * tSmall) return
    seen += ` ${(tLarge / tSmall).toFixed(1)}x (${tLarge.toFixed(2)} ms against ` +
      `${tSmall.toFixed(2)} ms per parse);`
  }
  assert.fail(`${label}: ${N} items against ${N / 10} took${seen} not linear`)
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
    name: 'doc = *a *b (two sibling stars)',
    grammar: [
      prod('doc', [star(ref('a')), star(ref('b'))]),
      prod('a', [lit('a')], [lit('A')]),
      prod('b', [lit('b')], [lit('B')]),
    ],
    make: (n) => times(n, 'a') + times(n, 'b'),
    one: 1,
    check: (out, n) => {
      assert.equal(out.kids.length, 2 * n)
      assert.equal(out.kids[n].src, 'b')
    },
  },
  {
    name: '*( "(" *item ")" ) (a star directly inside a star\'s group)',
    grammar: [prod('doc', [star(group([lit('('), star(ref('item')), lit(')')]))]), item],
    make: (n) => times(n, '(ab cd)'),
    one: 1,
    check: (out, n) => assert.equal(out.kids.length, 2 * n),
  },
  {
    // The item builds a value, and each of its members is a star: the
    // loops over `key`'s letters and `val`'s digits run inside an item of
    // the loop over `pair`, side by side. Each allocates the node it
    // accumulates into on its own way in, which is what the member reads
    // back; a loop that took its pusher's instead would write its letters
    // into the object (see 'the loop counter' below).
    name: '*pair, pair = key "=" val ";" ; @object (stars in an item that builds a value)',
    grammar: [
      prod('doc', [star(ref('pair'))]),
      {
        name: 'pair',
        value: { kind: 'object', members: ['key', 'val'] },
        alts: [[ref('key'), lit('='), ref('val'), lit(';')]],
      },
      prod('key', [star(rx('[a-z]'))]),
      prod('val', [star(rx('[0-9]'))]),
    ],
    make: (n) => times(n, 'ab=12;'),
    one: 1,
    check: (out, n) => {
      assert.equal(out.kids.length, n)
      assert.deepEqual(out.kids[n - 1], { key: 'ab', val: '12' })
    },
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
    for (const opts of [{}, { builtins: true }]) {
      const mode = opts.builtins ? 'builtins' : 'closures'
      it(`${c.name}: time is linear in the item count (${mode})`, () => {
        assertLinear(parser(c.grammar, opts), c.make, `${c.name} (${mode})`)
      })
    }
  }

  it('holds in a recognition-only grammar', () => {
    // The loop's back-edge and its `rep` counter are structural, so a spec
    // stripped of every tree builder still loops in one frame.
    const g = [prod('doc', [ref('item'), star(group([lit(','), ref('item')]))]), item]
    const p = parser(g, { builtins: true, recognition: true })
    const one = p.parse(times(2, 'ab', ','))
    const many = p.parse(times(N, 'ab', ','))
    assert.equal(many.deepest, one.deepest)
  })

  it('collects one element per item into an array, at one depth', () => {
    // `list = "(" *( item ";" ) ")"  ; @array` — the array-collection
    // planner hands the loop the array it inherits, and each
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

  it('takes two hundred thousand items whose item has kids (closures)', () => {
    // The loop's node collects the kids of every item: each iteration
    // capturing its item appends the item's kids to the node it inherits.
    // Appended one at a time, the node grows as the input does; appended
    // as a spread (`push(...kids)`), every item's kids went on the
    // engine's call stack as arguments, and past about a hundred and
    // thirty thousand of them the capture threw `RangeError` from inside
    // a parse whose rule depth was two. The closure mode is the compiler's
    // own code; the builtins mode runs the engine's `@capture$` and
    // `@fold$`, which join this test once the engine release that appends
    // the same way is what the package depends on.
    const n = 200000
    const g = [prod('doc', [star(ref('item'))]), prod('item', [lit('a'), lit('b')])]
    const p = parser(g)
    const one = p.parse('ab')
    const many = p.parse(times(n, 'ab'))
    assert.equal(many.out.kids.length, n)
    assert.equal(many.out.kids[n - 1].src, 'ab')
    assert.equal(many.out.src.length, 2 * n)
    assert.equal(many.deepest, one.deepest)
  })

  it('emits the loop as a replace, never a push of itself', () => {
    // The shape, for `doc = *item`. The loop's first alternative is its
    // entry: on the way in (counter `rep` still 0) it allocates the node
    // and re-enters the loop, counted. Every other alternative decides
    // continue or exit as the right-recursive helper always did, but
    // continuing hands over to the iteration by replacement: `$alt0`
    // pushes the item (clearing the counter for whatever the item holds)
    // and replaces itself, counted again, with `$step1`, which replaces
    // itself with the loop. The rule names are the ones the helper
    // always compiled to.
    const { spec } = parser([prod('doc', [star(ref('item'))]), item], { builtins: true })
    const loop = Object.keys(spec.rule).find((n) => /^_gen\d+_star_item$/.test(n))
    assert.ok(loop, Object.keys(spec.rule).join(' '))
    const iter = loop + '$alt0'
    const step = iter + '$step1'
    assert.deepEqual(Object.keys(spec.rule).sort(),
      ['__start__', loop, iter, step, 'doc', 'item'].sort())

    assert.deepEqual(spec.rule.doc.open.map((a) => a.p), [loop])

    const [entry, ...open] = spec.rule[loop].open
    assert.deepEqual(entry.c, { 'n.rep': 0 })
    assert.deepEqual(entry.n, { rep: 1 })
    assert.equal(entry.r, loop)
    assert.equal(entry.a, '@node$')
    assert.equal(entry.k.node$.init, true)
    const takes = open.filter((a) => null != a.r)
    assert.ok(0 < takes.length)
    for (const a of takes) {
      assert.equal(a.r, iter, 'continuing hands over to the iteration')
      assert.equal(a.p, undefined)
      assert.equal(a.c, undefined)
      assert.equal(a.a, undefined, 'an iteration allocates nothing')
    }
    for (const a of open.filter((a) => null == a.r)) {
      assert.equal(a.p, undefined, 'an exit takes no item')
      assert.equal(a.a, undefined, 'an exit allocates nothing')
    }
    assert.equal(spec.rule[loop].close, undefined)

    assert.deepEqual(spec.rule[iter].open.map((a) => [a.p, a.n]),
      [['item', { rep: 0 }]])
    assert.deepEqual(spec.rule[iter].close.map((a) => [a.r, a.n, a.a]),
      [[step, { rep: 1 }, '@capture$']])
    assert.deepEqual(spec.rule[step].open.map((a) => a.r), [loop])
    assert.equal(spec.rule[step].close, undefined)

    // Nothing but the enclosing rule pushes the loop, and nothing pushes
    // the iteration or its step.
    for (const [name, rs] of Object.entries(spec.rule)) {
      for (const a of [...(rs.open ?? []), ...(rs.close ?? [])]) {
        if (name !== 'doc') assert.notEqual(a.p, loop, `${name} pushes the loop`)
        assert.notEqual(a.p, iter, `${name} pushes the iteration`)
        assert.notEqual(a.p, step, `${name} pushes the step`)
      }
    }
  })

  it('matches a terminal item and re-enters the loop in one alternative', () => {
    // `*"x"` needs no iteration rule: the loop is the one rule the helper
    // always was, and its continue alternative matches the item and
    // replaces the loop with itself.
    const { spec } = parser([prod('doc', [star(lit('x')), lit(';')])], { builtins: true })
    const loop = Object.keys(spec.rule).find((n) => /^_gen\d+_star_/.test(n))
    assert.deepEqual(Object.keys(spec.rule).filter((n) => n.startsWith('_gen')), [loop])
    const [entry, ...open] = spec.rule[loop].open
    assert.deepEqual(entry.c, { 'n.rep': 0 })
    const takes = open.filter((a) => null != a.r)
    assert.equal(takes.length, 1)
    assert.equal(takes[0].r, loop)
    assert.equal(takes[0].k.node$.init, false)
    assert.equal(takes[0].k.node$.nterms, 1)
    assert.equal(spec.rule[loop].close, undefined)
  })

  it('enters a loop afresh inside an item of the same loop', () => {
    // `v = "[" *v "]" / "x"`: the loop over `v` is reached again from
    // inside one of its own items. The counter the outer loop holds at 1
    // is cleared by the push of the item, so the inner loop allocates a
    // node of its own and the trees nest as the brackets do.
    const g = [prod('v', [lit('['), star(ref('v')), lit(']')], [lit('x')])]
    const p = parser(g)
    const out = p.parse('[x[xx]x]').out
    assert.equal(out.src, '[x[xx]x]')
    assert.deepEqual(out.kids.map((k) => k.src), ['x', '[xx]', 'x'])
    assert.deepEqual(out.kids[1].kids.map((k) => k.src), ['x', 'x'])
  })
})


// Every loop allocates the node its iterations accumulate into once, on
// the way in, and it is a node of its own rather than the node of the
// rule that pushed it. The `rep` counter is what tells the way in from
// the way back, and counters are INHERITED: a pushed rule and a replacing
// rule both start with a copy of their predecessor's. So a loop reached
// inside an item of another loop, beside another loop, or inside a
// loop's group could see the 1 its neighbour set, skip its entry and
// write into whatever node it was pushed with. The push of an item clears
// the counter, which is what prevents that; these watch the engine to
// show it does, from every place a loop can be reached.
describe('the loop counter', () => {
  const cell = prod('cell', [rx('[a-z]')])
  const entryCases = [
    {
      name: 'a star inside the item of a star',
      grammar: [prod('doc', [star(ref('row'))]),
        prod('row', [lit('['), star(ref('cell')), lit(']')]), cell],
      src: '[ab][][c]',
    },
    {
      name: 'two sibling stars',
      grammar: [prod('doc', [star(ref('cell')), lit(';'), star(ref('cell'))]), cell],
      src: 'ab;cd',
    },
    {
      name: 'two sibling stars inside the item of a star',
      grammar: [prod('doc', [star(ref('row'))]),
        prod('row', [lit('['), star(ref('cell')), lit(';'), star(ref('cell')), lit(']')]),
        cell],
      src: '[ab;cd][;][a;]',
    },
    {
      name: 'a star directly inside a star\'s group',
      grammar: [prod('doc', [star(group([lit('('), star(ref('cell')), lit(')')]))]), cell],
      src: '(ab)()(c)',
    },
    {
      name: 'three stars, each in the group of the one outside it',
      grammar: [prod('doc', [star(group([lit('['),
        star(group([lit('('), star(ref('cell')), lit(')')])), lit(']')]))]), cell],
      src: '[(ab)()][][(c)]',
    },
    {
      name: 'a plus and an m* inside a star\'s group',
      grammar: [prod('doc', [star(group([lit('<'), plus(ref('cell')), lit(';'),
        rep(2, Infinity, ref('cell')), lit('>')]))]), cell],
      src: '<a;bc><abc;def>',
    },
    {
      name: 'a star of a terminal inside a star\'s group',
      grammar: [prod('doc', [star(group([lit('('), star(lit('x')), lit(')')]))])],
      src: '(xx)()(x)',
    },
    {
      name: 'a loop reached again inside one of its own items',
      grammar: [prod('v', [lit('['), star(ref('v')), lit(']')], [lit('x')])],
      src: '[x[xx[]]x]',
    },
    {
      name: 'stars in an item that builds a value',
      grammar: [
        prod('doc', [star(ref('pair'))]),
        {
          name: 'pair',
          value: { kind: 'object', members: ['key', 'val'] },
          alts: [[ref('key'), lit('='), ref('val'), lit(';')]],
        },
        prod('key', [star(rx('[a-z]'))]),
        prod('val', [star(rx('[0-9]'))]),
      ],
      src: 'ab=12;=;c=3;',
      value: {
        rule: 'doc', src: '',
        kids: [{ key: 'ab', val: '12' }, { key: '', val: '' }, { key: 'c', val: '3' }],
      },
    },
  ]

  // The loops of a spec that its start rule can reach: the rules whose
  // first alternative is a loop's entry. (A production whose only use was
  // inlined, as a leading member is, keeps its own loop, which nothing
  // runs.)
  const loopsOf = (spec) => {
    const reached = new Set([spec.options.rule.start])
    for (const name of reached) {
      const rs = spec.rule[name]
      for (const a of [...(rs.open ?? []), ...(rs.close ?? [])]) {
        for (const to of [a.p, a.r]) if (null != to) reached.add(to)
      }
    }
    return [...reached].filter((name) => 0 === spec.rule[name].open?.[0]?.c?.['n.rep'])
  }

  for (const c of entryCases) {
    for (const opts of [{}, { builtins: true }]) {
      const mode = opts.builtins ? 'builtins' : 'closures'
      it(`enters afresh: ${c.name} (${mode})`, () => {
        const spec = emitGrammarSpec({ productions: c.grammar },
          { tag: 'depth', start: c.grammar[0].name, ...opts })
        const loops = loopsOf(spec)
        assert.ok(0 < loops.length)
        const tn = new Tabnas()
        tn.grammar(spec)
        // For each pusher and loop, the nodes the loop's rules ran in.
        const ran = new Map()
        const borrowed = []
        tn.sub({
          ruleDone: (rule) => {
            if (!loops.includes(rule.name)) return
            if (rule.node === rule.parent.node) borrowed.push(rule.name)
            if (!ran.has(rule.parent)) ran.set(rule.parent, new Map())
            const byLoop = ran.get(rule.parent)
            if (!byLoop.has(rule.name)) byLoop.set(rule.name, new Set())
            byLoop.get(rule.name).add(rule.node)
          },
        })
        const out = tn.parse(c.src)
        if (c.value) assert.deepEqual(out, c.value)

        assert.deepEqual(borrowed, [], 'a loop ran in the node of its pusher')
        const entered = new Set()
        for (const byLoop of ran.values()) {
          for (const [name, nodes] of byLoop) {
            entered.add(name)
            assert.equal(nodes.size, 1, `${name} allocated ${nodes.size} nodes on one entry`)
          }
        }
        assert.deepEqual([...entered].sort(), [...loops].sort(),
          'the input must reach every loop')
      })
    }
  }
})
