/* Copyright (c) 2026 Richard Rodger and other contributors, MIT License */
'use strict'

// Value annotations, driven through the IR rather than a notation.
//
// A front-end says WHAT a rule builds; nothing here knows how the
// notation spelled it. ABNF carries it in a trailing comment, but this
// compiler must not know that — so these drive `Production.value`
// directly, which is also the only way to reach the cases no front-end
// has syntax for yet.

const { describe, it } = require('node:test')
const assert = require('node:assert')

const { emitGrammarSpec } = require('..')
const { Tabnas } = require('@tabnas/parser')

const ref = (name) => ({ kind: 'ref', name })
const term = (literal) => ({ kind: 'term', literal })
// `1*DIGIT`, not a bare `[0-9]+` terminal. The difference decides whether
// a LEADING member survives: left-recursion elimination folds a leading
// reference into this rule, and a rule whose whole body is one terminal
// becomes literals here — which stops it being a member at all. A
// repetition becomes a reference to a generated helper, so it still
// pushes. Real ABNF writes `1*DIGIT`, so that is what these use.
const digits = () => ({ kind: 'plus', inner: { kind: 'regex', pattern: '[0-9]', flags: '' } })
const letters = () => ({ kind: 'plus', inner: { kind: 'regex', pattern: '[a-z]', flags: '' } })

const emit = (productions, start) =>
  emitGrammarSpec({ productions }, { tag: 'tst', start, builtins: true })

const build = (productions, start, src) => {
  const j = new Tabnas()
  j.grammar(emit(productions, start))
  return j.parse(src)
}

describe('value annotations', () => {
  // `top = a "." b "." c` with the parts named. The FIRST member is the
  // one that matters: a production's leading reference is inlined by the
  // left-recursion pass, so `top` pushes a generated helper rather than
  // `a`. The annotation naming its own parts is what survives that.
  const TRIPLE = [
    { name: 'top', value: { kind: 'object', members: ['a', 'b', 'c'] },
      alts: [[ref('a'), term('.'), ref('b'), term('.'), ref('c')]] },
    { name: 'a', alts: [[digits()]] },
    { name: 'b', alts: [[digits()]] },
    { name: 'c', alts: [[digits()]] },
  ]

  it('builds an object whose keys the input never spells', () => {
    assert.deepEqual(build(TRIPLE, 'top', '1.2.30'),
      { a: '1', b: '2', c: '30' })
  })

  it('names the first member even though its rule was inlined away', () => {
    // Not a restatement of the test above: it asserts the CAUSE. `top`'s
    // first segment pushes a generated helper, not `a` — so the key can
    // only have come from the annotation, never from the chain.
    const spec = emit(TRIPLE, 'top')
    const firstPush = spec.rule.top.open[0]
    assert.notEqual(firstPush.p, 'a',
      'this test is pointless if the leading ref survives — ' +
      'the inlining it guards against has stopped happening')
    assert.deepEqual(firstPush.k.key$, { lit: 'a' })
  })

  it('takes a scalar member from its matched text, via setval src', () => {
    const spec = emit(TRIPLE, 'top')
    assert.deepEqual(spec.rule.top.close[0].k.setval$, { src: true })
  })

  it('nests a member that builds its own value, and only that one', () => {
    // `inner` is annotated, so it is assigned WHOLE — omitting `src` is
    // what makes nesting work, rather than any special case. `name` is
    // not, so it still resolves to its text.
    const prods = [
      { name: 'top', value: { kind: 'object', members: ['name', 'inner'] },
        alts: [[ref('name'), term('='), ref('inner')]] },
      { name: 'name', alts: [[letters()]] },
      { name: 'inner', value: { kind: 'object', members: ['maj', 'min'] },
        alts: [[ref('maj'), term('.'), ref('min')]] },
      { name: 'maj', alts: [[digits()]] },
      { name: 'min', alts: [[digits()]] },
    ]
    assert.deepEqual(build(prods, 'top', 'ab=1.2'),
      { name: 'ab', inner: { maj: '1', min: '2' } })

    const spec = emit(prods, 'top')
    // The nested member's close carries NO src; the scalar one does.
    assert.equal(spec.rule['top$step1'].close[0].k, undefined,
      'a nested member must be assigned whole, not flattened to text')
  })

  it('builds an array, with each element taken from its text', () => {
    const prods = [
      { name: 'top', value: { kind: 'array' },
        alts: [[ref('a'), term(','), ref('b')]] },
      { name: 'a', alts: [[digits()]] },
      { name: 'b', alts: [[digits()]] },
    ]
    assert.deepEqual(build(prods, 'top', '1,2'), ['1', '2'])
  })

  it('drops the tree builders on a rule that builds a value', () => {
    // They cannot both run: both own `r.node`. Appending `@object$`
    // after `@node$` clobbers the node just allocated, and the matching
    // `@capture$` then finds no `kids` and throws.
    const spec = emit(TRIPLE, 'top')
    const acts = JSON.stringify(spec.rule.top)
    assert.ok(!acts.includes('@node$'), '@node$ must not survive on a value rule')
    assert.ok(!acts.includes('@capture$'), '@capture$ must not survive either')
    // ...but the MEMBERS keep theirs: `src` reads the node.src they build.
    assert.ok(JSON.stringify(spec.rule.b).includes('@node$'),
      'a member still needs its tree builders to accumulate src')
  })

  it('refuses an annotation that names the wrong number of members', () => {
    // A part made only of literals pushes nothing and produces no value,
    // so the names would land on the wrong members. The rewrite passes
    // are why this is checked at emit time: inlining a leading reference
    // can change how many parts push.
    const prods = [
      { name: 'top', value: { kind: 'object', members: ['a', 'b'] },
        alts: [[ref('a'), term('x')]] },
      { name: 'a', alts: [[digits()]] },
    ]
    assert.throws(() => emit(prods, 'top'),
      /names 2 members but has 1 part that produces a value/)
  })

  it('refuses a member whose rule would lose its shape to inlining', () => {
    // `a = x ":"` is folded into `top` by left-recursion elimination, so
    // the member would capture `x` and silently drop the `":"` that
    // belonged to `a`. A rule whose body pushes twice would silently
    // become two members. Both are wrong VALUES, not errors, so refuse.
    const prods = [
      { name: 'top', value: { kind: 'object', members: ['a', 'b'] },
        alts: [[ref('a'), term(','), ref('b')]] },
      { name: 'a', alts: [[ref('x'), term(':')]] },
      { name: 'x', alts: [[digits()]] },
      { name: 'b', alts: [[digits()]] },
    ]
    assert.throws(() => emit(prods, 'top'), /folded into this rule/)
  })

  it('refuses an unknown annotation kind', () => {
    // The TS union is a compile-time promise only: a JS caller, or a
    // grammar deserialized from JSON, reaches this with anything.
    const prods = [
      { name: 'top', value: { kind: 'arry' }, alts: [[ref('a')]] },
      { name: 'a', alts: [[digits()]] },
    ]
    assert.throws(() => emit(prods, 'top'), /unknown kind 'arry'/)
  })

  it('annotates a rule whose alternative is a single reference', () => {
    // `top = child` takes the all-simple shortcut, which emitted tree
    // builders and returned before the annotation was ever consulted —
    // silently handing back an AST instead of the requested value.
    const prods = [
      { name: 'top', value: { kind: 'object', members: ['child'] },
        alts: [[ref('child')]] },
      { name: 'child', alts: [[digits()]] },
    ]
    assert.deepEqual(build(prods, 'top', '7'), { child: '7' })
  })

  it('nests an array element whose own rule is annotated', () => {
    // Nesting is decided by POSITION, not by a member name — an array
    // names nothing, so a name-based rule could never nest one.
    const prods = [
      { name: 'top', value: { kind: 'array' },
        alts: [[ref('plain'), term(','), ref('one')]] },
      { name: 'plain', alts: [[digits()]] },
      { name: 'one', value: { kind: 'object', members: ['p', 'q'] },
        alts: [[ref('p'), term(':'), ref('q')]] },
      { name: 'p', alts: [[digits()]] },
      { name: 'q', alts: [[digits()]] },
    ]
    assert.deepEqual(build(prods, 'top', '9,1:2'),
      ['9', { p: '1', q: '2' }])
  })

  it('keeps the value builders out of a recognition-only spec', () => {
    // `toRecognitionSpec` drops the output-building actions. The value
    // builders belong in that set for the same reason the tree builders
    // do — a recognition grammar must recognise and build nothing.
    const { toRecognitionSpec } = require('..')
    const spec = toRecognitionSpec(emit(TRIPLE, 'top'))
    const text = JSON.stringify(spec)
    for (const act of ['@object$', '@key$', '@setval$', '@push$', '@array$']) {
      assert.ok(!text.includes(act), `${act} must not survive recognition mode`)
    }
  })

  it('refuses an annotation on a rule with alternatives', () => {
    // One list of names cannot describe two alternatives' parts, and
    // building a different shape depending on which matched is worse
    // than refusing.
    // Distinct leading refs, so left factoring cannot merge these into
    // one alternative behind the guard's back.
    const prods = [
      { name: 'top', value: { kind: 'object', members: ['a'] },
        alts: [[ref('a'), term('x')], [ref('b'), term('y')]] },
      { name: 'a', alts: [[digits()]] },
      { name: 'b', alts: [[letters()]] },
    ]
    assert.throws(() => emit(prods, 'top'), /alternatives/)
  })
})
