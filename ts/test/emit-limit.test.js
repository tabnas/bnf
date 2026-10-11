/* Copyright (c) 2026 Richard Rodger and other contributors, MIT License */

// What compiling one grammar may write is bounded (MAX_EMIT_UNITS), and so
// is what the enumeration that tells a choice's alternatives apart may hold
// while it runs. Each is charged before the allocation that would pass it,
// so a grammar is refused before the step that would pass the limit is
// made, and every count below is exact.
//
// Two shapes made the emitter write far more than the grammar it was given.
// A probe helper lists every token its optional prefix and what follows it
// can reach, so P optional prefixes over V tokens write about 4·P·V units: a
// thousand of each made a 45 MB spec from a 400 KB grammar. A choice whose
// alternatives begin with runs of optional elements enumerates a polynomial
// of degree up to four in their number: forty alternatives of up to forty
// options did not finish in a minute.
//
// A probe shape's refusal, that of the forty alternatives of up to forty
// options and that of a removal list are also shared fixtures,
// rs/tests/oracle/ir-emit-units-*.json, which oracle-refusal.test.js and the
// other runtimes' oracle tests grade.
//
// The memos of token pairs are limited too (MAX_MEMO_ENTRIES), and that
// limit changes no output, which the last tests show.

const { describe, it } = require('node:test')
const assert = require('node:assert')

const {
  EmitError,
  emitGrammarSpec,
  toJsonic,
  toPureSpec,
  MAX_EMIT_UNITS,
} = require('../dist/bnf')
const { setMemoLimit } = require('../dist/compiler')

const lit = (literal) => ({ kind: 'term', literal, caseSensitive: true })
const ref = (name) => ({ kind: 'ref', name })
const opt = (inner) => ({ kind: 'opt', inner })

// top = "k0" t0 / … ; t(i) = ["a" "d"] "a" v ; v = "w0" / … / "w(V-1)".
// Each t(i) is a probe over the V + 1 tokens of v and "a".
const probes = (P, V) => {
  const productions = [{
    name: 'top',
    alts: Array.from({ length: P }, (_, i) => [lit('k' + i), ref('t' + i)]),
  }]
  for (let i = 0; i < P; i++) {
    productions.push({
      name: 't' + i,
      sp: { s: 10 * i, e: 10 * i + 9, r: i + 2, c: 1 },
      alts: [[opt({ kind: 'group', alts: [[lit('a'), lit('d')]] }),
        lit('a'), ref('v')]],
    })
  }
  productions.push({
    name: 'v',
    alts: Array.from({ length: V }, (_, i) => [lit('w' + i)]),
  })
  return { productions }
}

// A = [x0] … [x(k-1)] "y" / [x1] … [x(k-1)] "y" / … / "y" / "z": every
// first token but "z" begins several alternatives.
const options = (k) => ({
  productions: [{
    name: 'A',
    alts: [
      ...Array.from({ length: k }, (_, j) => [
        ...Array.from({ length: k - j }, (_, i) => opt(lit('x' + (i + j)))),
        lit('y'),
      ]),
      [lit('z')],
    ],
  }],
})

// A = [x0] … [x(k-1)] "y" / "x0" "w": one run of options, contested at its
// first token alone, so the enumeration grows as k³ and nothing after it
// costs more than k.
const run = (k) => ({
  productions: [{
    name: 'A',
    alts: [
      [...Array.from({ length: k }, (_, i) => opt(lit('x' + i))), lit('y')],
      [lit('x0'), lit('w')],
    ],
  }],
})

// C character classes nested inside one another, [Ā-ā], [Ā-Ă], …, each
// beside a literal of its own. Class i is laid over i + 1 atoms of the
// partition, so the classes write C(C + 1)/2 token-set members.
const nested = (C) => ({
  productions: [{
    name: 'A',
    alts: Array.from({ length: C }, (_, i) => [{
      kind: 'regex',
      pattern: `[${String.fromCodePoint(0x100)}-${String.fromCodePoint(0x101 + i)}]`,
      flags: 'u',
    }, lit('e' + i)]),
  }],
})

// S = "s" F D: F holds C nested classes, as above, and D N alternatives of
// distinct heads, each past a reference, which a dispatcher decides.
const dispatched = (C, N) => ({
  productions: [
    { name: 'S', alts: [[lit('s'), ref('F'), ref('D')]] },
    { name: 'F', alts: nested(C).productions[0].alts },
    {
      name: 'D',
      alts: Array.from({ length: N }, (_, i) => [lit('k' + i), ref('B'), lit('e')]),
    },
    { name: 'B', alts: [[lit('b')], [lit('c')]] },
  ],
})

const ADVICE = {
  probe: 'Write that optional prefix and what follows it as two ' +
    'alternatives, one with the prefix and one without.',
  alternates: 'Factor what its alternatives share into rules of their own.',
  lookahead: 'Give its alternatives first tokens that tell them apart, or ' +
    'factor what they begin with into a rule of its own.',
  tokenSet: 'Write fewer character classes that overlap it.',
  remove: 'Remove fewer names.',
}

const adds = (subject, doing, room, written, advice) =>
  `x: ${subject} exceeds the emit limit of 3500000 units while ${doing}, ` +
  `which would add more than ${room} units to the ${written} already ` +
  `written. ${advice}`

const holds = (rule, room, written) =>
  `x: rule '${rule}' exceeds the emit limit of 3500000 units while ` +
  `enumerating the token sequences that tell its alternatives apart, which ` +
  `would hold more than ${room} units on top of the ${written} already ` +
  `written. ${ADVICE.lookahead}`

const emit = (g) => emitGrammarSpec(g, { tag: 'x' })

// Each try builds the grammar afresh and checks the refusal in full. The
// time is the fastest of up to three tries, since whatever else the machine
// is running can slow one compile down. The limit is three seconds: the two
// refusals below take 0.2 s and 0.6 to 0.8 s where they were measured, a
// slower machine has taken twice that, and what the limit guards against is
// the minutes these shapes took unbounded.
const refusedWithin = (make, ms, check) => {
  let best = Infinity
  for (let i = 0; i < 3 && best >= ms; i++) {
    const g = make()
    const t0 = process.hrtime.bigint()
    let error
    try {
      emit(g)
    } catch (e) {
      error = e
    }
    best = Math.min(best, Number(process.hrtime.bigint() - t0) / 1e6)
    assert.ok(error instanceof EmitError, `not refused: ${error}`)
    check(error)
  }
  assert.ok(best < ms, `refused after ${best.toFixed(0)} ms at best`)
}

describe('emit limit', () => {
  it('is 3,500,000 units', () => {
    assert.equal(MAX_EMIT_UNITS, 3500000)
  })

  it('refuses a thousand probes over a thousand tokens in seconds', () => {
    // Each probe's list is charged with the rule it becomes, 4·1,001 + 2
    // units, as it is made: t873's would pass the limit.
    refusedWithin(() => probes(1000, 1000), 3000, (error) => {
      assert.equal(error.message, adds("rule 't873'",
        'writing the probe for one of its optional prefixes', 2762, 3497238,
        ADVICE.probe))
      assert.equal(error.rule, 't873')
      assert.deepEqual(error.sp, { s: 8730, e: 8739, r: 875, c: 1 })
    })
  })

  it('counts every unit the probe shape writes, up to the limit and no further', () => {
    // P probes over V tokens write 4PV + 44P + 3V + 10 units: 3,498,599 for
    // a thousand over 863, which compiles, and 3,502,602 over 864, which is
    // refused while t881's dispatcher is written.
    const spec = emit(probes(1000, 863))
    assert.equal(spec.rule['t999$pd0$probe'].open.length, 865)
    assert.throws(() => emit(probes(1000, 864)), {
      message: adds("rule 't881'", 'writing its alternates', 9, 3499991,
        ADVICE.alternates),
    })
  })

  it('refuses forty alternatives of up to forty options in seconds', () => {
    refusedWithin(() => options(40), 3000, (error) => {
      assert.equal(error.message, holds('A', 3486837, 13163))
      assert.equal(error.rule, 'A')
    })
  })

  it('refuses the options shape from thirty-nine alternatives on', () => {
    assert.throws(() => emit(options(39)), {
      message: holds('A', 3487738, 12262),
    })
  })

  it('counts what a choice enumerates, up to the limit and no further', () => {
    // The run of 133 options enumerates just under the limit and compiles;
    // the run of 134 passes it while the enumeration is under way.
    const spec = emit(run(133))
    assert.ok(null != spec.rule.A)
    assert.throws(() => emit(run(134)), {
      message: holds('A', 3490551, 9449),
    })
  })

  it('counts every token-set member, up to the limit and no further', () => {
    // C nested classes write C(C + 1)/2 members and 4C + 7 units more:
    // 3,499,332 for 2,641 classes, which compile, and 3,501,978 for 2,642,
    // refused while A is written.
    const spec = emit(nested(2641))
    const members = Object.values(spec.options.tokenSet)
      .reduce((n, set) => n + set.length, 0)
    assert.equal(members, 2641 * 2642 / 2)
    assert.throws(() => emit(nested(2642)), {
      message: adds("rule 'A'", 'writing its alternates', 5954, 3494046,
        ADVICE.alternates),
    })
  })

  it('holds the prefixes a choice dispatches on until its alternates are written', () => {
    // The nested classes fill most of the limit; D's dispatcher then holds
    // the prefixes it decided on, two units for each of its N heads, while
    // it writes the alternates made from them.
    assert.ok(null != emit(dispatched(2635, 569)).rule.D)
    assert.throws(() => emit(dispatched(2635, 570)), {
      message: adds("rule 'D'", 'writing its alternates', 15372, 3484628,
        ADVICE.alternates),
    })
  })

  it('counts the entries the removals write, up to the limit and no further', () => {
    // Each name a grammar removes writes a null rule and a null fixed token
    // in the spec, 2 units. Beside the 3,499,332 units 2,641 nested classes
    // write, 334 names fill the limit exactly, and 335 would pass it.
    const removing = (n) => ({
      ...nested(2641),
      remove: Array.from({ length: n }, (_, i) => 'gone' + i),
    })
    const spec = emit(removing(334))
    assert.equal(spec.rule.gone333, null)
    assert.equal(spec.options.fixed.token['#gone333'], null)
    assert.throws(() => emit(removing(335)), (error) => {
      assert.ok(error instanceof EmitError)
      assert.equal(error.message, adds('the removal list of 335 names',
        'writing the rules and tokens it removes', 668, 3499332,
        ADVICE.remove))
      assert.equal(error.rule, undefined)
      return true
    })
  })

  it('refuses the token set that would pass the limit, naming its class', () => {
    // The first 2,645 classes list 3,499,335 members; the next would list
    // 2,646 more.
    assert.throws(() => emit(nested(2646)), (error) => {
      assert.ok(error instanceof EmitError)
      assert.equal(error.message, adds(
        `the character class /[${String.fromCodePoint(0x100)}-` +
        `${String.fromCodePoint(0xb56)}]/`,
        'listing the members of its token set', 665, 3499335,
        ADVICE.tokenSet))
      assert.equal(error.rule, undefined)
      return true
    })
  })
})

describe('memo limit', () => {
  // A rule of N two-segment alternatives with distinct heads goes to the
  // dispatcher, whose contested-head scan asks every pair of its heads.
  const wide = (N) => ({
    productions: [
      {
        name: 'A',
        alts: Array.from({ length: N }, (_, i) =>
          [lit('k' + String(i).padStart(6, '0')), ref('B'), lit('e')]),
      },
      { name: 'B', alts: [[lit('b')], [lit('c')]] },
    ],
  })

  // N literal heads and N character-class heads in one rule, every pair of
  // them overlapping: the keyword-shadow ordering decides N·N of them.
  const shadowed = (N) => ({
    productions: [{
      name: 'A',
      alts: [
        ...Array.from({ length: N }, (_, i) => [lit('a' + i), ref('B')]),
        ...Array.from({ length: N }, (_, i) => [{
          kind: 'regex',
          pattern: `[a-${String.fromCodePoint(0x101 + i)}]`,
          flags: 'u',
        }, ref('B')]),
      ],
    }, { name: 'B', alts: [[lit('b')]] }],
  })

  const text = (g) => toJsonic(toPureSpec(emitGrammarSpec(g, {
    tag: 'x', builtins: true,
  })), { strict: true })

  const sameUnderAnyLimit = (g) => {
    const kept = text(g)
    const was = setMemoLimit(Infinity)
    try {
      assert.equal(text(g), kept, 'the spec differs with no limit')
      setMemoLimit(0)
      assert.equal(text(g), kept, 'the spec differs keeping nothing')
    } finally {
      setMemoLimit(was)
    }
  }

  it('changes no output when a rule asks more pairs than the memos keep', () => {
    // 400 heads make 79,800 pairs, past the 65,536 kept.
    sameUnderAnyLimit(wide(400))
  })

  it('changes no output when the keyword-shadow ordering passes it', () => {
    // 300 literal heads against 300 class heads: 90,000 answers.
    sameUnderAnyLimit(shadowed(300))
  })
})
