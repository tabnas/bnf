/* Copyright (c) 2026 Richard Rodger and other contributors, MIT License */
'use strict'

// `tokenClasses` compiles a production whose alternatives are all single
// literals or engine tokens to one engine token set. What a grammar
// accepts must not depend on the option: each case here is one where it
// once did (tabnas/bnf#74 review). Go and Rust pin the same cases.

const { describe, it } = require('node:test')
const assert = require('node:assert')

const { emitGrammarSpec } = require('../dist/bnf')
const { Tabnas } = require('@tabnas/parser')

const lit = (s) => ({ kind: 'term', literal: s, caseSensitive: true })
const rx = (pattern) => ({ kind: 'regex', pattern, flags: '' })
const tok = (name) => ({ kind: 'token', name })
const ref = (name) => ({ kind: 'ref', name })
const prod = (name, ...alts) => ({ name, alts })

const emit = (productions, tokenClasses) =>
  emitGrammarSpec({ productions }, { tag: 'tc', start: 'doc', tokenClasses })
const parses = (spec, src, opts) => {
  try {
    return new Tabnas(opts || {}).grammar(spec).parse(src).rule === 'doc'
  } catch (e) {
    return false
  }
}
const seqs = (spec, name) => spec.rule[name].open.map((o) => o.s)
const sets = (spec) => Object.keys(spec.options.tokenSet || {})
const relex = { lex: { relex: true } }

describe('token-class', () => {

  it('a class set head takes the keyword-shadow order its members had', () => {
    // doc = x ; x = C / [a-z] D ; C = "a" / "b" ; D = [0-9]
    // With the option off C's literals are heads of x, each guarded ahead
    // of the character class that shadows it. The set that stands for
    // them is ordered the same way, or \`a1\` commits to C at \`a\` and
    // fails at \`1\`.
    const g = [
      prod('doc', [ref('x')]),
      prod('x', [ref('C')], [rx('[a-z]'), ref('D')]),
      prod('C', [lit('a')], [lit('b')]),
      prod('D', [rx('[0-9]')]),
    ]
    const off = emit(g, false)
    const on = emit(g, true)
    assert.deepEqual(seqs(on, 'x'), ['#C #ZZ', '#RX__A_Z', '#C'])
    for (const src of ['a1', 'a', 'b', 'c1', 'b1']) {
      assert.ok(parses(off, src, relex), 'off: ' + src)
      assert.ok(parses(on, src, relex), 'on: ' + src)
    }
  })

  it('a class with a set among its members stays a production', () => {
    // C = #C / "a" names its own set; C = #D / "a" beside D = #C / "b"
    // names another class's. A set of sets is one the engine cannot
    // resolve, and expanding it never ended.
    const x = prod('x', [ref('C'), ref('t'), ref('u')], [lit('z'), ref('u'), ref('t')])
    const rest = [prod('t', [lit('.'), lit('.')]), prod('u', [lit(','), lit(',')])]
    for (const classes of [
      [prod('C', [tok('#C')], [lit('a')])],
      [prod('C', [tok('#D')], [lit('a')]), prod('D', [tok('#C')], [lit('b')])],
    ]) {
      const g = [prod('doc', [ref('x')]), x, ...rest, ...classes]
      const on = emit(g, true)
      assert.deepEqual(sets(on), [])
      assert.deepEqual(seqs(on, 'x'), seqs(emit(g, false), 'x'))
      assert.ok(parses(on, 'a..,,'))
      assert.ok(parses(on, 'z,,..'))
    }
  })

  it('an empty production name is not a class', () => {
    // doc = x ; x = <""> "!" ; <""> = "a" / "b". The set would be named
    // \`#\`, which names nothing, and so would the token standing for the
    // reference.
    const g = [
      prod('doc', [ref('x')]),
      prod('x', [ref(''), lit('!')]),
      prod('', [lit('a')], [lit('b')]),
    ]
    const on = emit(g, true)
    assert.deepEqual(sets(on), [])
    assert.deepEqual(seqs(on, 'x'), seqs(emit(g, false), 'x'))
    assert.ok(parses(on, 'a!'))
    assert.ok(parses(on, 'b!'))
  })

})
