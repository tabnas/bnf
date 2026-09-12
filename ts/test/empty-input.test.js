/* Copyright (c) 2026 Richard Rodger and other contributors, MIT License */
'use strict'

// Whether the empty input is in the language, driven through the IR.
//
// The engine short-circuits `''` before the parse loop starts, so no rule
// ever sees it and `options.lex.empty` alone decides. Nothing set it, and
// the engine's default is permissive, so every emitted grammar accepted
// `''` — `S = "a"` included. The front-ends could not fix this between
// them either: gbnf answered it itself, ebnf did not, and a grammar's
// nullability is a property of the IR rather than of any notation.
//
// These go through `emitGrammarSpec` directly because that is the one
// place every front-end passes through.

const { describe, it } = require('node:test')
const assert = require('node:assert')

const { emitGrammarSpec } = require('..')

const term = (literal) => ({ kind: 'term', literal })
const ref = (name) => ({ kind: 'ref', name })
const rx = (pattern, flags = '') => ({ kind: 'regex', pattern, flags })

const emptyOf = (productions, opts) =>
  emitGrammarSpec({ productions }, Object.assign({ tag: 't' }, opts))
    .options.lex.empty

const one = (alt) => [{ name: 'S', alts: [alt] }]


describe('the empty input', () => {

  describe('a terminal consumes, so the grammar does not derive empty', () => {
    const CONSUMES = [
      ['a literal', [term('a')]],
      ['a sequence', [term('a'), term('b')]],
      ['1*A', [{ kind: 'plus', inner: term('a') }]],
      ['a class', [rx('[a-z]')]],
      ['a built-in token', [{ kind: 'token', name: '#TX' }]],
      ['a group of consuming alternatives',
        [{ kind: 'group', alts: [[term('a')], [term('b')]] }]],
    ]
    for (const [label, alt] of CONSUMES) {
      it(label, () => assert.equal(emptyOf(one(alt)), false))
    }
  })


  describe('these derive empty', () => {
    const DERIVES = [
      ['*A', [{ kind: 'star', inner: term('a') }]],
      ['[ A ]', [{ kind: 'opt', inner: term('a') }]],
      ['0*2A', [{ kind: 'rep', min: 0, max: 2, inner: term('a') }]],
      ['a group with one empty-deriving branch',
        [{ kind: 'group', alts: [[term('a')], [{ kind: 'opt', inner: term('b') }]] }]],
    ]
    for (const [label, alt] of DERIVES) {
      it(label, () => assert.equal(emptyOf(one(alt)), true))
    }
  })


  // Two element kinds can match nothing without looking like it, and
  // reading them as consuming would reject input the grammar admits —
  // the worse direction of the two.
  it('an empty literal matches nothing, so it derives empty', () => {
    // `liftLiteralTokens` keeps `""` rather than refusing it.
    assert.equal(emptyOf(one([term('')])), true)
  })


  it('a class that can match nothing derives empty', () => {
    // Decided by asking the regex. `[a-z]` consumes; `[a-z]*` does not
    // have to, and no front-end promises to desugar its quantifiers away
    // before the IR.
    assert.equal(emptyOf(one([rx('[a-z]')])), false)
    assert.equal(emptyOf(one([rx('[a-z]*')])), true)
    assert.equal(emptyOf(one([rx('a|')])), true)
  })


  // Nullability is a least fixed point over the rules, not a property of
  // one production read alone. Each of these needs more than one pass.
  it('follows nullability through other rules', () => {
    assert.equal(emptyOf([
      { name: 'S', alts: [[ref('A')]] },
      { name: 'A', alts: [[ref('B')]] },
      { name: 'B', alts: [[{ kind: 'star', inner: term('a') }]] },
    ]), true)

    assert.equal(emptyOf([
      { name: 'S', alts: [[ref('A')]] },
      { name: 'A', alts: [[ref('B')]] },
      { name: 'B', alts: [[term('a')]] },
    ]), false)
  })


  it('sees rules defined after their use', () => {
    // A single pass over the productions in order answers `false` here.
    assert.equal(emptyOf([
      { name: 'S', alts: [[ref('A'), ref('B')]] },
      { name: 'A', alts: [[{ kind: 'opt', inner: term('x') }]] },
      { name: 'B', alts: [[{ kind: 'opt', inner: term('y') }]] },
    ]), true)
  })


  it('recursion is not nullability', () => {
    // Every alternative consumes an `a` before reaching the recursion, so
    // the rule that reaches itself stays at the bottom of the fixed point.
    assert.equal(emptyOf([
      { name: 'S', alts: [[term('a'), ref('S')], [term('a')]] },
    ]), false)
  })


  it('asks it of the start rule, whichever that is', () => {
    const prods = [
      { name: 'S', alts: [[term('a')]] },
      { name: 'T', alts: [[{ kind: 'star', inner: term('b') }]] },
    ]
    assert.equal(emptyOf(prods), false)
    assert.equal(emptyOf(prods, { start: 'T' }), true)
  })

})
