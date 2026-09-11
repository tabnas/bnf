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

const emit = (productions, start, opts) =>
  emitGrammarSpec({ productions }, { tag: 'tst', start, builtins: true, ...opts })

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

  // ---- The plan and the emitter must count the SAME parts ----------
  //
  // `planValueAnnotations` runs on the authored grammar and hands the
  // emitter one nesting flag per member; the emitter walks segments of
  // the REWRITTEN alternative. Every test below is a way those two
  // sequences came apart, and each one produced a wrong value in
  // silence rather than an error.

  it('nests by position when a group precedes the reference', () => {
    // The plan counted only `ref` elements, so a leading group was not
    // a member to it — and `inner`'s nesting flag landed on the GROUP,
    // pushing `{src:'ab',kids:[]}` (an internal tree node) as element 0.
    const prods = [
      { name: 'top', value: { kind: 'array' },
        alts: [[{ kind: 'group', alts: [[letters()]] }, term(','), ref('inner')]] },
      { name: 'inner', value: { kind: 'object', members: ['y'] },
        alts: [[ref('y')]] },
      { name: 'y', alts: [[digits()]] },
    ]
    assert.deepEqual(build(prods, 'top', 'ab,3'), ['ab', { y: '3' }])
  })

  it('counts a group as a member of an object', () => {
    // Same miscount on the object side, where it showed up as the
    // emitter's own count check firing with a number the author could
    // not relate to what they wrote. The refusal is now at annotation
    // time and says what is actually wrong.
    const prods = [
      { name: 'top', value: { kind: 'object', members: ['inner'] },
        alts: [[{ kind: 'group', alts: [[letters()]] }, term(','), ref('inner')]] },
      { name: 'inner', alts: [[digits()]] },
    ]
    assert.throws(() => emit(prods, 'top'),
      /names 1 member but has 2 parts that produce a value/)
  })

  it('keeps an annotated rule out of the literal-token lift', () => {
    // `liftLiteralTokens` turns a single-literal production into a named
    // lexer token and deletes the rule. Doing that to an ANNOTATED rule
    // discarded its builders with no diagnostic, and removed it from its
    // caller's member list at the same time — so the caller's remaining
    // flags shifted onto the wrong parts and nested an internal node.
    const prods = [
      { name: 'top', value: { kind: 'array' },
        alts: [[ref('n'), ref('s'), ref('m')]] },
      { name: 'n', alts: [[digits()]] },
      { name: 's', value: { kind: 'object', members: [] }, alts: [[term('+')]] },
      { name: 'm', alts: [[digits()]] },
    ]
    assert.deepEqual(build(prods, 'top', '12+34'), ['12', {}, '34'])
  })

  it('refuses an annotation whose part count a rewrite changed', () => {
    // The array side had nothing checking the plan against the segments:
    // an object at least compared its NAMES. A member whose own rule is
    // one literal becomes a lexer token, so it stops pushing — and every
    // later flag then sits one place too early.
    const prods = [
      { name: 'top', value: { kind: 'array' },
        alts: [[ref('n'), ref('s'), ref('m')]] },
      { name: 'n', alts: [[digits()]] },
      { name: 's', alts: [[term('+')]] },
      { name: 'm', alts: [[digits()]] },
    ]
    assert.throws(() => emit(prods, 'top'),
      /annotation for 3 parts but builds 2/)
  })

  // ---- The leading fold, followed the whole way -------------------

  it('follows an alias chain when checking the leading member', () => {
    // `a = b` is a single part, so the check passed — but the pass
    // inlines `a` into `top` and then `b` into that, so what landed was
    // `b`'s two-part body and the member silently lost its ':'.
    const prods = [
      { name: 'top', value: { kind: 'object', members: ['a', 'c'] },
        alts: [[ref('a'), term(','), ref('c')]] },
      { name: 'a', alts: [[ref('b')]] },
      { name: 'b', alts: [[digits(), term(':')]] },
      { name: 'c', alts: [[digits()]] },
    ]
    assert.throws(() => emit(prods, 'top'), /not a single part/)
  })

  it('refuses a leading member whose own rule builds a value', () => {
    // Inlining erases that rule's builders, so the member held an
    // internal tree node — `{src:'1',kids:[]}` — where the author had
    // asked for the object `a` is annotated to build. The refusal comes
    // from the whole-grammar scan, which asks this of every caller, not
    // only of an annotated one.
    const prods = [
      { name: 'top', value: { kind: 'object', members: ['a', 'c'] },
        alts: [[ref('a'), term(','), ref('c')]] },
      { name: 'a', value: { kind: 'object', members: ['x'] },
        alts: [[ref('x')]] },
      { name: 'x', alts: [[digits()]] },
      { name: 'c', alts: [[digits()]] },
    ]
    assert.throws(() => emit(prods, 'top'),
      /erases the value 'a' is annotated to build/)
  })

  it('nests a leading member once a literal guards it', () => {
    // The escape hatch the diagnostic above offers has to actually
    // work: a literal in front means the reference is no longer leading,
    // so nothing is inlined and the member nests whole.
    const prods = [
      { name: 'top', value: { kind: 'object', members: ['a', 'c'] },
        alts: [[term('v'), ref('a'), term(','), ref('c')]] },
      { name: 'a', value: { kind: 'object', members: ['x'] },
        alts: [[ref('x')]] },
      { name: 'x', alts: [[digits()]] },
      { name: 'c', alts: [[digits()]] },
    ]
    assert.deepEqual(build(prods, 'top', 'v1,2'), { a: { x: '1' }, c: '2' })
  })

  it('terminates on an alias cycle', () => {
    // Chasing the chain has to have a stop. `a = b`, `b = a` is left
    // recursion reached through aliases; the fold check must refuse
    // rather than loop.
    const prods = [
      { name: 'top', value: { kind: 'object', members: ['a'] },
        alts: [[ref('a')]] },
      { name: 'a', alts: [[ref('b')]] },
      { name: 'b', alts: [[ref('a')]] },
    ]
    assert.throws(() => emit(prods, 'top'), /not a single part/)
  })

  // ---- `members` is data, not a type promise ----------------------

  it('refuses members that are not a list', () => {
    // `'ab'.length` is 2, so a string passed the count check and was
    // indexed as two one-character member names.
    const prods = [
      { name: 'top', value: { kind: 'object', members: 'ab' },
        alts: [[ref('a'), ref('b')]] },
      { name: 'a', alts: [[letters()]] },
      { name: 'b', alts: [[digits()]] },
    ]
    assert.throws(() => emit(prods, 'top'), /members are not a list/)
  })

  it('refuses a member that is not a name', () => {
    // A null entry suppressed its `@key$`, so `@setval$` wrote into
    // whatever key the previous part left behind — or the literal key
    // 'undefined' when there was none. An empty string was worse: it
    // built the key '' in TypeScript and was skipped entirely in Go,
    // which is the two ports disagreeing about a value.
    for (const bad of [null, '', 7]) {
      const prods = [
        { name: 'top', value: { kind: 'object', members: [bad, 'b'] },
          alts: [[ref('a'), ref('b')]] },
        { name: 'a', alts: [[letters()]] },
        { name: 'b', alts: [[digits()]] },
      ]
      assert.throws(() => emit(prods, 'top'), /is not a name/,
        `members: [${JSON.stringify(bad)}]`)
    }
  })

  it('refuses an array that names members', () => {
    // An array's parts are positional. Ignoring the names hid the real
    // mistake, which is that the author meant `object`.
    const prods = [
      { name: 'top', value: { kind: 'array', members: ['a'] },
        alts: [[ref('a')]] },
      { name: 'a', alts: [[digits()]] },
    ]
    assert.throws(() => emit(prods, 'top'), /positional and are not named/)
  })

  // ---- Diagnostics ------------------------------------------------

  it('names the notation in an annotation diagnostic', () => {
    // The plan runs before any rewrite — and used to run before the
    // diagnostic prefix was set, so the first bad grammar in a process
    // reported `bnf:` and every later one inherited the PREVIOUS
    // conversion's tag.
    const bad = [{ name: 'top', value: { kind: 'nope' }, alts: [[digits()]] }]
    for (const tag of ['gbnf', 'ebnf']) {
      // On `e.message`, not the stringified error: `assert.throws` tests
      // a RegExp against `'EmitError: ' + message`, which would match a
      // leaked prefix anywhere in the text.
      assert.throws(
        () => emitGrammarSpec({ productions: bad }, { tag, start: 'top', builtins: true }),
        (e) => e.message.startsWith(tag + ': '),
        `tag ${tag}`)
    }
  })

  it('carries the span of the offending rule', () => {
    // Every other diagnostic in this compiler can say WHERE. These four
    // are the ones an author is most likely to hit, and they were the
    // ones with nothing to underline.
    const sp = { s: 12, e: 20, r: 3, c: 7 }
    const bad = [{ name: 'top', value: { kind: 'nope' }, sp, alts: [[digits()]] }]
    assert.throws(() => emit(bad, 'top'), (e) => {
      assert.deepEqual(e.sp, sp)
      assert.equal(e.rule, 'top')
      return true
    })
  })

  // ---- Who gets inlined, and who does not ------------------------

  it('refuses an UNANNOTATED caller that inlines an annotated rule', () => {
    // The erasure does not need an annotated caller — it needs a
    // LEADING reference. Nothing was looking at `top`, because the
    // planner only ever walked productions that named members, so this
    // returned an ordinary AST with the requested value nowhere in it.
    const prods = [
      { name: 'top', alts: [[ref('leaf'), term(',')]] },
      { name: 'leaf', value: { kind: 'object', members: ['d'] },
        alts: [[ref('d')]] },
      { name: 'd', alts: [[digits()]] },
    ]
    assert.throws(() => emit(prods, 'top'),
      /erases the value 'leaf' is annotated to build/)
  })

  it('nests through an annotated pure alias', () => {
    // A pure alias is the one caller left-recursion elimination does NOT
    // substitute into, so `top = child` keeps its reference and an
    // annotated `child` nests whole. Refusing it was a refusal of a
    // shape that works — and a one-member wrapper is the most natural
    // way to reach for it.
    const prods = [
      { name: 'top', value: { kind: 'object', members: ['child'] },
        alts: [[ref('child')]] },
      { name: 'child', value: { kind: 'object', members: ['d'] },
        alts: [[ref('d')]] },
      { name: 'd', alts: [[digits()]] },
    ]
    assert.deepEqual(build(prods, 'top', '7'), { child: { d: '7' } })
  })

  it('refuses an annotation on a same-depth repeat, with its span', () => {
    // `add = [0-9]+ [ "+" add ]` compiles to a close-phase loop, so the
    // parts the annotation named are no longer separate pushes. The
    // prefix and separator have to be BARE terminals for the rewrite to
    // fire — `1*DIGIT` is a repetition and is not one.
    const rx = { kind: 'regex', pattern: '[0-9]+', flags: '' }
    const sp = { s: 5, e: 25 }
    const prods = [
      { name: 'top', alts: [[ref('add')]] },
      { name: 'add', value: { kind: 'array' }, sp,
        alts: [[rx, { kind: 'opt', inner: { kind: 'group',
          alts: [[term('+'), ref('add')]] } }]] },
    ]
    assert.throws(() => emit(prods, 'top'), (e) => {
      assert.match(e.message, /same-depth repeat/)
      assert.deepEqual(e.sp, sp, 'ranged like every other annotation refusal')
      return true
    })
  })

  it('keeps a composed action flat when a slot is attached', () => {
    // The canonical shape the Go port has to match: attaching a user
    // action or slot to an alt that already carries value builders must
    // extend the list, not nest inside it. Go stored the builders as
    // []string, which its `appendAction` type switch did not recognise,
    // so it produced [["@object$","@key$"], ref]. Pinned on both sides.
    const { attachActionSlots } = require('..')
    const spec = emit(TRIPLE, 'top', { marks: true })
    const mark = spec.rule.top.open[0].m
    const slot = '@top:o:' + mark
    attachActionSlots(spec, [slot])
    assert.deepEqual(spec.rule.top.open[0].a, ['@object$', '@key$', slot])
  })
})
