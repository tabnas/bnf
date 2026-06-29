/* Copyright (c) 2026 Richard Rodger and other contributors, MIT License */
'use strict'

const { describe, it } = require('node:test')
const assert = require('node:assert')

const { Tabnas } = require('@tabnas/parser')
const { abnf: abnfPlugin, abnfConvert: abnf } = require('..')

// Fresh engine per grammar: each test installs its own ABNF.
function make(grammar, opts) {
  const tn = new Tabnas({ plugins: [abnfPlugin], rewind: { history: 4096 } })
  tn.abnf(grammar, Object.assign({ tag: 'tk' }, opts))
  return tn
}

describe('built-in token terminals', () => {
  it('compiles a bare TX reference to an `s:#TX` terminal', () => {
    const spec = abnf('ident = TX', { tag: 'tk' })
    assert.deepEqual(
      spec.rule.ident.open.map((a) => a.s),
      ['#TX'],
    )
  })

  it('TX / NR / ST / VL each map to their lexer token', () => {
    const spec = abnf('w = TX\nn = NR\ns = ST\nv = VL', { tag: 'tk' })
    assert.equal(spec.rule.w.open[0].s, '#TX')
    assert.equal(spec.rule.n.open[0].s, '#NR')
    assert.equal(spec.rule.s.open[0].s, '#ST')
    assert.equal(spec.rule.v.open[0].s, '#VL')
  })

  it('parses a bareword via TX as a node with src', () => {
    const out = make('ident = TX').parse('hello')
    assert.equal(out.rule, 'ident')
    assert.equal(out.src, 'hello')
  })

  it('parses NR as a number token', () => {
    assert.equal(make('n = NR').parse('42').src, '42')
  })

  it('parses ST as a quoted string token', () => {
    assert.equal(make('s = ST').parse('"hi"').src, '"hi"')
  })

  it('a user rule of the same name wins over the built-in', () => {
    // TX defined locally => the bareword keeps the rule reference.
    const spec = abnf('top = TX\nTX = "literal"', { tag: 'tk' })
    // top pushes the TX rule (a ref), not a token terminal.
    assert.ok(spec.rule.TX, 'user TX rule is emitted')
    assert.equal(make('top = TX\nTX = "literal"').parse('literal').src, 'literal')
  })

  it('adjacent token terminals separate on whitespace (no greedy merge)', () => {
    // The whole point: scannerless char-classes would merge `a b`; whole-word
    // #TX tokens do not.
    const out = make('pair = a b\na = TX\nb = TX').parse('alpha beta')
    const kids = out.kids.filter((k) => k.rule)
    assert.deepEqual(kids.map((k) => k.src), ['beta'])
    // `a` is leading-ref-inlined; both barewords are still consumed distinctly.
    assert.equal(out.src, 'alphabeta')
  })

  it('a nullable optional before a token-bodied ref dispatches on the token', () => {
    // Regression: `[ "x" ] c` where c = TX must let the dispatcher enter via
    // the token when the optional is absent (altPrefixes empty-branch).
    const G = 'top = a / b\na = [ "x" ] c "z"\nc = TX\nb = "w"'
    assert.equal(make(G).parse('foo z').src, 'fooz')   // optional absent
    assert.equal(make(G).parse('x foo z').src, 'xfooz') // optional present
    assert.equal(make(G).parse('w').src, 'w')           // other alt
  })

  it('repetition over a token-bodied rule works', () => {
    // `*( "." TX )` style — dotted path.
    const out = make('path = TX *( "." TX )').parse('foo.bar.baz')
    assert.equal(out.src, 'foo.bar.baz')
  })

  it('wordKeywords: a keyword does not grab the prefix of an identifier', () => {
    // `map` is a prefix of the identifier `mapping`.
    const G = 'decl = "map" name ";"\nname = TX'
    const off = make(G) // default: no wordKeywords
    const on = new Tabnas({ plugins: [abnfPlugin], rewind: { history: 4096 } })
    on.abnf(G, { tag: 'tk', wordKeywords: true })

    // Off: `map` greedily matches the prefix of `mapping`, name = `ping`.
    assert.doesNotThrow(() => off.parse('mapping ;'))
    // On: `map` only matches the whole word, so `mapping` is not `map …`.
    assert.throws(() => on.parse('mapping ;'))
    // On: a real `map` keyword followed by an identifier still works.
    assert.equal(on.parse('map foo ;').src, 'mapfoo;')
  })
})
