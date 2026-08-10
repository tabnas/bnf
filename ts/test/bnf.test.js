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

  it('exports a semver-shaped VERSION matching package.json', () => {
    assert.match(VERSION, /^\d+\.\d+\.\d+/)
    assert.equal(VERSION, require('../package.json').version)
  })
})
