/* Copyright (c) 2026 Richard Rodger and other contributors, MIT License */

// PRODUCTION NAMES ARE UNTRUSTED KEYS.
//
// A grammar is text, and this package compiles it — so production names come
// from input, not from a developer's fingers. Every table keyed by one must
// therefore be allocated without a prototype: on a plain `{}` the name
// __proto__ never becomes a key (the write runs the Object.prototype setter)
// and a lookup of any inherited name (`constructor`, `toString`) returns an
// Object.prototype member instead of undefined.
//
// The visible symptom was a production silently disappearing: BUILTIN_TOKENS
// is consulted as `BUILTIN_TOKENS[prod.name]`, which for __proto__ is
// Object.prototype — truthy — so the production was skipped as though the
// lexer already defined it, and the generated parser then failed with
// "unknown rule: __proto__".

const { describe, it } = require('node:test')
const assert = require('node:assert')

const { emitGrammarSpec } = require('..')

// Minimal Grammar IR: one production per entry. Two elements per alt so the
// production is not lifted into a lexer token (a single-literal production is
// deliberately lifted, which would take it out of the rule spec for reasons
// unrelated to its name).
const term = (literal) => ({ kind: 'term', literal })

function grammarOf(...names) {
  return {
    productions: names.map((name) => ({
      name,
      alts: [[term('x'), term('y')]],
    })),
  }
}

function ruleKeys(spec) {
  return Object.keys(spec.rule || {}).sort()
}

describe('prototype-shaped production names', () => {

  it('a production named __proto__ survives into the rule spec', () => {
    const spec = emitGrammarSpec(grammarOf('__proto__', 'other'))
    assert.equal(
      Object.prototype.hasOwnProperty.call(spec.rule, '__proto__'), true,
      'rule keys were: ' + ruleKeys(spec))
  })

  it('productions named after Object.prototype members survive', () => {
    for (const name of ['constructor', 'toString', 'hasOwnProperty', 'valueOf']) {
      const spec = emitGrammarSpec(grammarOf(name, 'other'))
      assert.equal(
        Object.prototype.hasOwnProperty.call(spec.rule, name), true,
        name + ' was dropped; rule keys were: ' + ruleKeys(spec))
    }
  })

  it('the emitted rule spec carries no prototype', () => {
    const spec = emitGrammarSpec(grammarOf('a', 'b'))
    assert.equal(Object.getPrototypeOf(spec.rule), null)
  })

  it('compiling a grammar does not touch Object.prototype', () => {
    const before = Object.getOwnPropertyNames(Object.prototype).slice()
    emitGrammarSpec(grammarOf('__proto__', 'constructor', 'other'))
    const added = Object.getOwnPropertyNames(Object.prototype)
      .filter((k) => !before.includes(k))
    for (const k of added) delete Object.prototype[k]
    assert.deepEqual(added, [])
  })

})
