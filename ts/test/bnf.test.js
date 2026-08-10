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

  it('exports a semver-shaped VERSION matching package.json', () => {
    assert.match(VERSION, /^\d+\.\d+\.\d+/)
    assert.equal(VERSION, require('../package.json').version)
  })
})
