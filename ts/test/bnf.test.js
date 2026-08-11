/* Copyright (c) 2026 Richard Rodger and other contributors, MIT License */

// Smoke tests for the notation-neutral compiler. The heavy verification
// lives downstream: @tabnas/abnf's suite drives this pipeline through
// RFC 3986, left recursion, round-trips and a 68-file conformance corpus.
// What is checked here is that the package's own surface works without a
// front-end present.

const { describe, it } = require('node:test')
const assert = require('node:assert')

const {
  emitGrammarSpec,
  eliminateLeftRecursion,
  refsIn,
  escapeRegExp,
  BUILTIN_TOKENS,
  toJsonic,
  attachActions,
  markListing,
  VERSION,
} = require('../dist/bnf')

const ref = (name) => ({ kind: 'ref', name })
const term = (literal) => ({ kind: 'term', literal })
const token = (name) => ({ kind: 'token', name })

describe('bnf', () => {
  it('compiles a minimal IR grammar into a GrammarSpec', () => {
    const spec = emitGrammarSpec({
      productions: [
        { name: 'val', alts: [[ref('add')]] },
        { name: 'add', alts: [[token('#NR')]] },
      ],
    }, { tag: 'demo' })

    assert.ok(spec.rule.val, 'expected a val rule')
    assert.ok(spec.rule.add, 'expected an add rule')
  })

  it('stamps the caller-supplied group tag, not a notation name', () => {
    const spec = emitGrammarSpec({
      productions: [{ name: 'top', alts: [[term('x')]] }],
    }, { tag: 'demo' })

    const tags = JSON.stringify(spec)
    assert.match(tags, /demo/)
    assert.doesNotMatch(tags, /\babnf\b/, 'must not assume a notation')
  })

  it('defaults the tag to bnf', () => {
    const spec = emitGrammarSpec({
      productions: [{ name: 'top', alts: [[term('x')]] }],
    })
    assert.match(JSON.stringify(spec), /bnf/)
  })

  it('lifts a single-literal production into a named lexer token', () => {
    const spec = emitGrammarSpec({
      productions: [
        { name: 'top', alts: [[ref('PL')]] },
        { name: 'PL', alts: [[term('+')]] },
      ],
    }, { tag: 'demo' })
    assert.equal(spec.options?.fixed?.token?.['#PL'], '+')
  })

  it('eliminates left recursion into iterative form', () => {
    const grammar = {
      productions: [
        { name: 'expr', alts: [[ref('expr'), term('+'), ref('num')], [ref('num')]] },
        { name: 'num', alts: [[token('#NR')]] },
      ],
    }
    const out = eliminateLeftRecursion(grammar)
    const expr = out.productions.find((p) => p.name === 'expr')
    const stillLeftRec = expr.alts.some(
      (alt) => alt[0] && alt[0].kind === 'ref' && alt[0].name === 'expr')
    assert.equal(stillLeftRec, false, 'expr must no longer start with itself')
  })

  it('collects rule references from a sequence', () => {
    const out = new Set()
    refsIn([ref('a'), { kind: 'group', alts: [[ref('b')]] }, term('x')], out)
    assert.deepStrictEqual([...out].sort(), ['a', 'b'])
  })

  it('escapes regex metacharacters in literals', () => {
    assert.equal(new RegExp('^' + escapeRegExp('a.b')).test('a.b'), true)
    assert.equal(new RegExp('^' + escapeRegExp('a.b')).test('axb'), false)
  })

  it('maps bareword names to engine built-in token names', () => {
    // `ident = TX` lets a grammar reference the lexer's whole-word token
    // instead of re-deriving it character by character.
    assert.equal(BUILTIN_TOKENS.NR, '#NR')
    assert.equal(BUILTIN_TOKENS.TX, '#TX')
    assert.equal(BUILTIN_TOKENS.ST, '#ST')
    assert.equal(BUILTIN_TOKENS.VL, '#VL')
  })

  it('serialises a spec as jsonic text', () => {
    const spec = emitGrammarSpec({
      productions: [{ name: 'top', alts: [[term('x')]] }],
    }, { tag: 'demo', builtins: true })
    const text = toJsonic(spec)
    assert.equal(typeof text, 'string')
    assert.ok(0 < text.length)
  })

  it('guards a repetition helper with its FOLLOW set', () => {
    // A repetition helper terminates on an empty alternative, which
    // names no token. The engine only offers a matcher where the active
    // rule names it, so without a guard the token that follows the
    // repetition is never lexed and the parse dies at the loop exit.
    // See issue #3.
    //
    //   root = star D    where   star = *W
    //
    // The generated `_gen1_star_W` must name `#D` on its terminating
    // alternative, peeking and pushing straight back so nothing extra
    // is consumed.
    const spec = emitGrammarSpec({
      productions: [
        {
          name: 'root',
          alts: [[{ kind: 'star', inner: ref('W') }, ref('D')]],
        },
        { name: 'W', alts: [[{ kind: 'regex', pattern: '[ ]', flags: '' }]] },
        { name: 'D', alts: [[{ kind: 'regex', pattern: '[0-9]', flags: '' }]] },
      ],
    }, { tag: 'demo' })

    // The helper itself, not its `$alt0`/`$step1` chain rules.
    const starRule = Object.entries(spec.rule)
      .find(([name]) => /^_gen\d+_star_/.test(name) && !name.includes('$'))
    assert.ok(starRule, 'expected a generated star helper')

    const [, rs] = starRule
    const dToken = spec.rule.D.open[0].s
    const guards = rs.open.filter((alt) => alt.s === dToken)
    assert.equal(
      guards.length, 1,
      'the star helper must name D on its terminating alternative')
    assert.equal(
      guards[0].b, 1,
      'the FOLLOW guard must push its peeked token back')
    assert.ok(
      !guards[0].p && !guards[0].r,
      'the FOLLOW guard must not push or replace a rule')
    // The unguarded empty alternative stays last as the fallback.
    const last = rs.open[rs.open.length - 1]
    assert.equal(last.s, undefined, 'expected a bare fallback alternative')
  })

  it('carries FOLLOW through a nullable suffix', () => {
    // `root = star X Y` where X is nullable: what follows the star is
    // FIRST(X) *and* FIRST(Y), because X can vanish.
    const spec = emitGrammarSpec({
      productions: [
        {
          name: 'root',
          alts: [[{ kind: 'star', inner: ref('W') }, ref('X'), ref('Y')]],
        },
        { name: 'W', alts: [[{ kind: 'regex', pattern: '[ ]', flags: '' }]] },
        { name: 'X', alts: [[token('#NR')], []] },
        { name: 'Y', alts: [[token('#TX')]] },
      ],
    }, { tag: 'demo' })

    const [, rs] = Object.entries(spec.rule)
      .find(([name]) => /^_gen\d+_star_/.test(name) && !name.includes('$'))
    const guarded = new Set(rs.open.map((alt) => alt.s).filter(Boolean))
    assert.ok(guarded.has('#NR'), 'expected FIRST(X) in the guard')
    assert.ok(
      guarded.has('#TX'),
      'expected FIRST(Y) in the guard, since X is nullable')
  })

  it('groups a regex before anchoring it', () => {
    // `^` binds tighter than `|`, so `^a|bc` anchors only the first
    // branch and the second can match anywhere in the input, producing
    // a token from the wrong position. Issue #2 defect 2.
    const spec = emitGrammarSpec({
      productions: [
        { name: 'top', alts: [[{ kind: 'regex', pattern: 'a|bc', flags: '' }]] },
      ],
    }, { tag: 'demo' })

    const re = Object.values(spec.options.match.token)[0]
    assert.equal(re.source, '^(?:a|bc)')
    // The point of the grouping: the second branch must not match at a
    // non-zero offset.
    assert.equal(re.test('xbc'), false)
    assert.equal(re.test('bc'), true)
  })

  it('keeps grammar-level remove/clearAll across the internal clone', () => {
    // `emitGrammarSpec` clones the grammar and then reads these fields
    // off the clone, so a clone that copied only `productions` dropped
    // them silently. Issue #2 defect 3.
    const spec = emitGrammarSpec({
      productions: [
        { name: 'top', alts: [[token('#NR')]] },
        { name: 'gone', alts: [[token('#TX')]] },
      ],
      remove: ['gone'],
      clearAll: true,
    }, { tag: 'demo', start: 'top' })

    assert.equal(spec.rule.gone, undefined, 'expected `gone` to be removed')
    assert.equal(spec.clear, true, 'expected clearAll to set spec.clear')
  })

  it('does not overwrite a user rule named __start__', () => {
    // The IR reserves no names, so a grammar may legitimately contain a
    // production called `__start__`. Issue #2 defect 4.
    const spec = emitGrammarSpec({
      productions: [
        { name: '__start__', alts: [[token('#NR')]] },
      ],
    }, { tag: 'demo' })

    const start = spec.options.rule.start
    assert.notEqual(
      start, '__start__',
      'the wrapper must not take the user rule\'s name')
    // The user's rule survives, and the wrapper pushes it rather than
    // pushing itself.
    assert.deepEqual(spec.rule.__start__.open[0].s, '#NR')
    assert.equal(spec.rule[start].open[0].p, '__start__')
  })

  it('escapes every control character in strict jsonic output', () => {
    // Strict mode promises valid JSON; a raw tab or CR inside a string
    // makes JSON.parse reject it. Issue #2 defect 5.
    const text = toJsonic({ s: 'a\tb\rcd' }, { strict: true })
    assert.doesNotThrow(() => JSON.parse(text))
    assert.equal(JSON.parse(text).s, 'a\tb\rcd')
  })

  it('allocates fresh action refs on a second attachActions call', () => {
    // The counter used to reset per call, so the second call reused
    // `@bnf_user0` and clobbered the first. Issue #2 defect 6.
    const spec = emitGrammarSpec({
      productions: [
        { name: 'top', alts: [[term('x')], [term('y')]] },
      ],
    }, { tag: 'demo', marks: true })

    // markListing renders `<rule>  o:<mark>  …`; the action ref for an
    // alt is `@<rule>:<phase>:<mark>`.
    const marks = markListing(spec)
      .split('\n')
      .map((line) => /^(\S+)\s+([oc]):(\S+)/.exec(line))
      .filter(Boolean)
      .filter((m) => m[1] === 'top' && m[3] !== '_')
      .map((m) => `@${m[1]}:${m[2]}:${m[3]}`)
    assert.ok(2 <= marks.length, 'expected at least two marked alts')

    attachActions(spec, { [marks[0]]: () => 'first' })
    attachActions(spec, { [marks[1]]: () => 'second' })

    const refs = Object.entries(spec.ref)
      .filter(([k]) => k.startsWith('@bnf_user'))
    assert.equal(refs.length, 2, 'expected two distinct action refs')
    assert.equal(
      new Set(refs.map(([k]) => k)).size, 2, 'ref names must be distinct')
  })

  it('eliminates left recursion hidden behind nullable sugar', () => {
    // `A = ["x"] A "y"` is left-recursive whenever the optional takes
    // its empty branch, but elimination runs before desugar and so only
    // sees a leading `opt`, not a leading ref. Issue #2 defect 1.
    const grammar = {
      productions: [{
        name: 'A',
        alts: [
          [{ kind: 'opt', inner: term('x') }, ref('A'), term('y')],
          [term('z')],
        ],
      }],
    }

    const out = eliminateLeftRecursion(grammar)
    const a = out.productions.find((p) => p.name === 'A')
    // No surviving alternative may re-enter A at the same position:
    // every alt either starts with something that must consume input,
    // or does not reference A first.
    for (const alt of a.alts) {
      const leads = alt.length > 0 && alt[0].kind === 'ref' &&
        alt[0].name === 'A'
      assert.equal(leads, false, 'A still re-enters itself immediately')
      const hidden = alt.length > 1 && alt[0].kind === 'opt' &&
        alt[1].kind === 'ref' && alt[1].name === 'A'
      assert.equal(hidden, false, 'A still re-enters itself behind an opt')
    }
  })

  it('never allocates a lifted literal an engine-owned token name', () => {
    // `NR = "NR"` wanted `#NR`, which the lexer's number matcher owns.
    // The engine rejects a fixed.token entry under a matcher-owned name
    // at configuration time, so this turned an ordinary grammar into a
    // hard failure before parsing began. Issue #2, footer note.
    const lift = (lit) => emitGrammarSpec({
      productions: [
        { name: 'top', alts: [[ref(lit)]] },
        {
          name: lit,
          alts: [[{ kind: 'term', literal: lit, caseSensitive: true }]],
        },
      ],
    }, { tag: 'demo' }).options.fixed.token

    for (const owned of ['NR', 'TX', 'ST', 'VL', 'ZZ', 'SP', 'LN', 'CM']) {
      const fixed = lift(owned)
      assert.ok(
        !Object.prototype.hasOwnProperty.call(fixed, '#' + owned),
        `#${owned} is engine-owned and must not be allocated to a literal`)
      // The literal still gets a token, just under a free name.
      assert.deepEqual(Object.values(fixed), [owned])
    }

    // A name the engine does not own is still used as-is.
    assert.deepEqual(lift('PL'), { '#PL': 'PL' })
  })

  // Left factoring rewrites a user rule's alternatives, so it must fire
  // only where the dispatcher genuinely cannot separate them: a
  // factored rule keeps ONE alternative, which merges the per-branch
  // collision marks that actions bind to. The prefix-span measurement
  // is what draws that line, and it runs on the raw IR — where the
  // sugar kinds are still present and easy to misread as unbounded.
  describe('left factoring is bounded by dispatch lookahead', () => {
    const rep = (min, max, inner) => ({ kind: 'rep', min, max, inner })
    const group = (...alts) => ({ kind: 'group', alts })
    const factored = (grammar, name) =>
      Object.keys(emitGrammarSpec(grammar, { tag: 'demo' }).rule)
        .some((rn) => rn.startsWith(name + '$fact'))

    const twoAlts = (prefix) => ({
      productions: [
        {
          name: 'g',
          alts: [
            [...prefix, ref('P')],
            [...prefix, ref('Q')],
          ],
        },
        { name: 'P', alts: [[term('p')]] },
        { name: 'Q', alts: [[term('q')]] },
      ],
    })

    it('leaves a short prefix of plain terminals to the dispatcher', () => {
      assert.equal(factored(twoAlts([term('a'), term('x')]), 'g'), false)
    })

    it('leaves a short prefix wrapped in a group', () => {
      // `("a") "x"` spans two tokens like the case above. Reading the
      // group as unbounded factored it, and the two branches then
      // shared one mark.
      assert.equal(
        factored(twoAlts([group([term('a')]), term('x')]), 'g'), false)
    })

    it('leaves a short prefix containing an optional', () => {
      assert.equal(
        factored(twoAlts([{ kind: 'opt', inner: term('-') }, term('1')]), 'g'),
        false)
    })

    it('leaves a bounded repetition that fits the lookahead', () => {
      assert.equal(factored(twoAlts([rep(2, 2, term('a'))]), 'g'), false)
    })

    it('factors a prefix longer than the lookahead', () => {
      const long = ['a', 'b', 'c', 'd', 'e'].map(term)
      assert.equal(factored(twoAlts(long), 'g'), true)
    })

    it('factors an unbounded repetition', () => {
      assert.equal(
        factored(twoAlts([{ kind: 'plus', inner: term('a') }]), 'g'), true)
    })

    it('keeps a mark per branch when it does not factor', () => {
      const listing = markListing(
        emitGrammarSpec(twoAlts([group([term('a')]), term('x')]),
          { tag: 'demo', marks: true }))
      // Two distinct open marks — one per original alternative.
      const marks = listing.split('\n')
        .filter((l) => l.includes('o:'))
        .map((l) => l.trim())
      assert.equal(marks.length, 2, listing)
      assert.notEqual(marks[0], marks[1], listing)
    })
  })


  // Coverage feeding the contest checks must agree with what a matcher
  // actually matches, or no guard is emitted where one is needed.
  it('counts both cases of a case-insensitive literal as covered', () => {
    // A bare literal is case-insensitive by default here, so `"G"` also
    // matches `g` and contests a lowercase class.
    const spec = emitGrammarSpec({
      productions: [
        {
          name: 'g',
          alts: [
            [term('G'), ref('L')],
            [ref('L')],
          ],
        },
        { name: 'L', alts: [[{ kind: 'regex', pattern: '[a-z]', flags: '' }]] },
      ],
    }, { tag: 'demo' })

    // The contest was detected, so the keyword entry carries lookahead
    // and outranks the bare class entry.
    const first = spec.rule.g.open[0]
    assert.ok(
      'string' === typeof first.s && 1 < first.s.split(' ').length,
      'expected a multi-token keyword guard first, got ' +
      JSON.stringify(spec.rule.g.open))
  })


  it('exports a semver-shaped VERSION matching package.json', () => {
    assert.match(VERSION, /^\d+\.\d+\.\d+/)
    assert.equal(VERSION, require('../package.json').version)
  })
})
