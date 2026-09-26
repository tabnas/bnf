/* Copyright (c) 2026 Richard Rodger and other contributors, MIT License */

// The size of what the emitter produces is a contract (tabnas/bnf#71).
//
// A choice is dispatched on the first token of each alternative, and the
// lookahead deepens only under a head two alternatives share, so the
// emitted table grows with the number of decisions rather than with the
// product of the tokens that can fill four positions. Every count below
// is exact and pinned, so the product cannot come back unnoticed.

const { describe, it } = require('node:test')
const assert = require('node:assert')

const { emitGrammarSpec } = require('../dist/bnf')
const { Tabnas } = require('@tabnas/parser')

const lit = (s) => ({ kind: 'term', literal: s, caseSensitive: true })
const ref = (name) => ({ kind: 'ref', name })
const prod = (name, ...alts) => ({ name, alts })
const kw = (N) =>
  prod('kw', ...Array.from({ length: N }, (_, i) => [lit('k' + (i + 1))]))

// doc = "{" *entry "}" ; entry = kw kw ; kw = "k1" / ... / "kN"
const starred = (N) => [
  prod('doc', [lit('{'), { kind: 'star', inner: ref('entry') }, lit('}')]),
  prod('entry', [ref('kw'), ref('kw')]),
  kw(N),
]
// doc = "{" x "}" ; x = entry entry / "z" ; entry = kw kw ; kw = ...
const choice = (N) => [
  prod('doc', [lit('{'), ref('x'), lit('}')]),
  prod('x', [ref('entry'), ref('entry')], [lit('z')]),
  prod('entry', [ref('kw'), ref('kw')]),
  kw(N),
]

const opens = (spec, name) => (spec.rule[name].open || []).length
const totalOpens = (spec) =>
  Object.values(spec.rule).reduce((a, r) => a + (r.open || []).length, 0)
const largest = (spec) =>
  Object.entries(spec.rule)
    .map(([n, r]) => [n, (r.open || []).length])
    .sort((a, b) => b[1] - a[1])[0]

describe('size', () => {
  it('a repeated N-way entry dispatches on N heads, not N^4 paths', () => {
    // The star helper: one entry per keyword head, one FOLLOW peek for
    // the `}` that ends the loop, and the bare fallback.
    for (const N of [2, 8, 26]) {
      const spec = emitGrammarSpec(
        { productions: starred(N) }, { tag: 'rp', start: 'doc', wordKeywords: true })
      const [name, count] = largest(spec)
      assert.match(name, /star_entry/)
      assert.equal(count, N + 2, `N=${N}: ${name} has ${count} open alternates`)
    }
  })

  it('an uncontested choice dispatches on one token per head', () => {
    const spec = emitGrammarSpec(
      { productions: choice(26) }, { tag: 'rp', start: 'doc', wordKeywords: true })
    // 26 keyword heads for the first alternative, one for "z".
    assert.equal(opens(spec, 'x'), 27)
  })

  it('a token class is one lookahead token (tokenClasses)', () => {
    const spec = emitGrammarSpec(
      { productions: starred(26) },
      { tag: 'rp', start: 'doc', wordKeywords: true, tokenClasses: true })
    // The class is a set; the helper peeks it once. The class rule
    // itself keeps its 26 alternates, one per member: it is the rule
    // that builds the node.
    assert.deepEqual(Object.keys(spec.options.tokenSet), ['kw'])
    assert.equal(spec.options.tokenSet.kw.length, 26)
    const helper = Object.keys(spec.rule).find((n) => /star_entry$/.test(n))
    assert.equal(opens(spec, helper), 3, `${helper} has ${opens(spec, helper)} open alternates`)
    assert.equal(opens(spec, 'kw'), 26)
    const x = emitGrammarSpec(
      { productions: choice(26) },
      { tag: 'rp', start: 'doc', wordKeywords: true, tokenClasses: true })
    assert.equal(opens(x, 'x'), 2)
    // The whole grammar stays small, and installs and parses.
    assert.ok(totalOpens(spec) < 40, `${totalOpens(spec)} open alternates`)
    const tn = new Tabnas().grammar(spec)
    const tree = tn.parse('{k1 k2 k26 k1}')
    assert.equal(tree.rule, 'doc')
    // The class keeps its node: it is a rule of its own, not inlined.
    const kids = JSON.stringify(tree)
    assert.match(kids, /"rule":"kw"/)
    assert.throws(() => tn.parse('{k1}'))
  })

  it('a token class leaves the tree exactly as the option-off compile does', () => {
    // A leading reference to the class is consumed as its one token where
    // the plain compile inlines the class's alternatives, and stays a
    // node where the plain compile keeps the reference: the same parse
    // result, node for node, with the option on or off.
    const grammar = { productions: starred(26) }
    const off = new Tabnas().grammar(
      emitGrammarSpec(grammar, { tag: 'rp', start: 'doc', wordKeywords: true }))
    const on = new Tabnas().grammar(
      emitGrammarSpec(grammar, { tag: 'rp', start: 'doc', wordKeywords: true, tokenClasses: true }))
    for (const src of ['{k1 k2 k26 k1}', '{}', '{k3 k3}']) {
      assert.deepEqual(
        JSON.parse(JSON.stringify(on.parse(src))),
        JSON.parse(JSON.stringify(off.parse(src))), src)
    }
  })

  it('the emitted grammar installs and parses at N=26 either way', () => {
    const spec = emitGrammarSpec(
      { productions: starred(26) }, { tag: 'rp', start: 'doc', wordKeywords: true })
    const tn = new Tabnas().grammar(spec)
    assert.equal(tn.parse('{k1 k2 k2 k1}').rule, 'doc')
    assert.throws(() => tn.parse('{k1}'))
  })

  it('a contested head deepens only as far as the decision needs', () => {
    // Two alternatives share `a`; they part at the second token, so the
    // dispatcher peeks two tokens under `a` and one under `z`. (Each
    // alternative holds two references, so this is a dispatcher, not
    // the single-segment path.)
    const spec = emitGrammarSpec({
      productions: [
        prod('doc', [ref('x')]),
        prod('x',
          [lit('a'), ref('t'), ref('u')],
          [lit('a'), ref('u'), ref('t')],
          [lit('z'), ref('t'), ref('u')]),
        prod('t', [lit('.'), lit('.')]),
        prod('u', [lit(','), lit(',')]),
      ],
    }, { tag: 'rp', start: 'doc' })
    const s = spec.rule.x.open.map((o) => [o.s, o.b])
    assert.deepEqual(s, [['#A #T', 2], ['#A #T1', 2], ['#Z', 1]])
    const tn = new Tabnas().grammar(spec)
    for (const src of ['a..,,', 'a,,..', 'z..,,']) assert.equal(tn.parse(src).rule, 'doc')
    assert.throws(() => tn.parse('a.,.,'))
  })

  it('a repetition contested by what follows it still looks across the boundary', () => {
    // start = *( "a" t ) "a" "b" with t = "x" "x": at an `a` the loop
    // may continue (`a x`) or exit (`a b`). The exit is an open path
    // (anything the enclosing rule accepts may follow), so the continue
    // entries keep the full window here, as they always did; what the
    // change buys is that an UNCONTESTED head no longer pays for it.
    const spec = emitGrammarSpec({
      productions: [
        prod('start', [{ kind: 'star', inner: { kind: 'group', alts: [[lit('a'), ref('t')]] } }, lit('a'), lit('b')]),
        prod('t', [lit('x'), lit('x')]),
      ],
    }, { tag: 'rp', start: 'start' })
    const helper = Object.keys(spec.rule).find((n) => /star.*group$/.test(n))
    const s = spec.rule[helper].open.map((o) => [o.s, o.b])
    assert.ok(s[0][0].startsWith('#A #X') && 2 <= s[0][1], JSON.stringify(s))
    const tn = new Tabnas().grammar(spec)
    for (const src of ['ab', 'axxab', 'axxaxxab']) assert.equal(tn.parse(src).rule, 'start')
    assert.throws(() => tn.parse('b'))
    assert.throws(() => tn.parse('axx'))
  })
})
