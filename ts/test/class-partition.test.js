/* Copyright (c) 2026 Richard Rodger and other contributors, MIT License */
'use strict'

// The limits of the overlapping-class partition, driven through the IR
// rather than a notation.
//
// These cases exist because the first cut of the partition got them
// wrong, and no front-end in the fleet can express them: ABNF's `%x`
// ranges are always single-code-point and case-sensitive, so its suite —
// the usual oracle for this compiler — cannot reach a multi-character
// regex terminal or a case-insensitive class at all. A notation-neutral
// compiler has to be safe for the front-ends that can.

const { describe, it } = require('node:test')
const assert = require('node:assert')

const { emitGrammarSpec } = require('..')
const { Tabnas } = require('@tabnas/parser')

const rx = (pattern, flags = '') => ({ kind: 'regex', pattern, flags })
const term = (literal) => ({ kind: 'term', literal })
const lit = (literal) => ({ kind: 'term', literal, caseSensitive: true })
const ref = (name) => ({ kind: 'ref', name })

const emit = (productions, opts) =>
  emitGrammarSpec({ productions }, Object.assign({ tag: 't' }, opts))

const matchers = (spec) =>
  Object.fromEntries(
    Object.entries(spec.options.match?.token ?? {}).map(([n, re]) => [n, String(re)]))

describe('overlapping-class partition: what it may touch', () => {
  // patternCharRanges answers "what can this pattern's FIRST character
  // be?" — right for contest detection, wrong for deciding a class's
  // full coverage. Partitioning REPLACES the matcher with one-character
  // atoms, so a pattern that matches more than one code point would lose
  // the rest of itself. Each of these three lost something real.
  const MULTI = [
    ['top-level alternation', 'a|bc'],
    ['quantifier', '[a-z]+'],
    ['two classes in sequence', '[aA][bB]'],
  ]

  for (const [label, pattern] of MULTI) {
    it(`leaves a multi-character regex alone: ${label}`, () => {
      // `[a]` overlaps the first character of every pattern above, so
      // without the guard each one becomes "contested" and is replaced.
      const spec = emit([
        { name: 'top', alts: [[rx(pattern)], [rx('[a]')]] },
      ])
      const src = Object.values(matchers(spec))
      assert.ok(
        src.some((s) => s.includes(pattern)),
        `${pattern} lost its matcher; emitted ${JSON.stringify(src)}`,
      )
      assert.deepEqual(
        spec.options.tokenSet ?? {}, {},
        'a pattern that can match more than one code point must not be partitioned',
      )
    })
  }

  it('leaves a case-insensitive class alone', () => {
    // foldCaseRanges folds ASCII A-Z/a-z and nothing else, so atoms
    // derived from `[é]/i` would cover `é` but not `É` — the matcher
    // would say one thing and the ranges another.
    const spec = emit([
      { name: 'top', alts: [[rx('[\\u00e9]', 'i')], [rx('[\\u00e9]')]] },
    ])
    const insensitive = Object.values(spec.options.match.token)
      .filter((re) => re.flags.includes('i'))
    assert.equal(insensitive.length, 1, 'the /i matcher must survive')
    assert.ok(insensitive[0].test('É'), 'and must still cover É')
    assert.deepEqual(spec.options.tokenSet ?? {}, {})
  })

  it('leaves out a class without u or v that reaches past U+FFFF', () => {
    // Without `u` or `v` a matcher takes one UTF-16 code unit at a time,
    // so `[^\ud800]` takes an emoji as two characters, its lead and
    // trail surrogates. Laid over the partition beside the overlapping
    // `[b-c]`, it became a set whose astral atom was compiled with `u` and
    // took the emoji whole: `c c ";"` refused `\u{1F600};`, which the
    // class takes alone, and took `\u{1F600}x;`, which it does not. A
    // negation, `[\s\S]` and an astral literal each read past U+FFFF
    // (tabnas/bnf#75 review).
    const grammar = (pattern, flags) => emit([
      { name: 'doc', alts: [
        [rx(pattern, flags), rx(pattern, flags), lit(';')],
        [rx('[b-c]'), lit('!')],
      ] },
    ], { tag: 'cp', start: 'doc' })
    const parses = (spec, src) => {
      try {
        return 'doc' === new Tabnas().grammar(spec).parse(src).rule
      } catch (e) {
        return false
      }
    }
    for (const pattern of ['[^\\ud800]', '[^a]', '[\\s\\S]', '[b\u{1F600}]']) {
      const spec = grammar(pattern, '')
      assert.deepEqual(spec.options.tokenSet ?? {}, {}, pattern)
      assert.ok(parses(spec, '\u{1F600};'), pattern)
      assert.ok(!parses(spec, '\u{1F600}x;'), pattern)
    }
    // Under `u` the class reads code points, as its atoms do, so the
    // partition still takes it.
    const unicode = grammar('[^a]', 'u')
    assert.equal(Object.keys(unicode.options.tokenSet ?? {}).length, 2)
    assert.ok(parses(unicode, '\u{1F600}x;'))
    assert.ok(!parses(unicode, '\u{1F600};'))
  })

  it('refuses a pattern whether or not another class overlaps it', () => {
    // Partitioning replaces a class's matcher with atoms, and the class's
    // own matcher went unbuilt: `[z-a]` beside `[a-z]` became an empty set
    // and the grammar was accepted, where alone it is `Range out of
    // order`. A flag string the constructor refuses went the same way
    // (tabnas/bnf#75 review).
    for (const [pattern, flags] of [['[z-a]', ''], ['[\\u-a]', ''], ['[a-z]', 'q']]) {
      const refusal = (alts) => {
        try {
          emit([{ name: 'top', alts }])
        } catch (e) {
          return e.message
        }
        return null
      }
      const alone = refusal([[rx(pattern, flags)]])
      assert.ok(null != alone, pattern + ' alone')
      assert.equal(refusal([[rx(pattern, flags)], [rx('[a-z]')]]), alone, pattern)
    }
  })

  it('still partitions two plain single-code-point classes', () => {
    // The guard above must not have disarmed the fix itself.
    const spec = emit([
      { name: 'top', alts: [[rx('[\\u0030-\\u0039]')], [rx('[\\u0031-\\u0039]')]] },
    ])
    const sets = spec.options.tokenSet ?? {}
    assert.deepEqual(
      Object.keys(sets).sort(),
      ['RX___U0030__U0039', 'RX___U0031__U0039'],
    )
  })
})

describe('overlapping-class partition: names stay put', () => {
  it('a contested class keeps its own token name', () => {
    // Marks come from altDiscriminator, which reads the token name out
    // of regexTokens. Pointing a one-atom class at the atom renamed it,
    // so a user action bound to `@top:o:<mark>` silently detached the
    // moment some OTHER production mentioned an overlapping class.
    const alts = [[rx('[123456789]'), term('x')], [term('y')]]
    const alone = emit([{ name: 'top', alts }], { marks: true })
    const contested = emit([
      { name: 'top', alts },
      { name: 'other', alts: [[rx('[0-9]')]] },
    ], { marks: true })

    assert.deepEqual(
      contested.rule.top.open.map((a) => a.m ?? null),
      alone.rule.top.open.map((a) => a.m ?? null),
      'an unrelated overlapping class must not move this rule’s marks',
    )
    assert.deepEqual(
      contested.rule.top.open.map((a) => a.s ?? null),
      alone.rule.top.open.map((a) => a.s ?? null),
    )
  })

  it('atoms are named apart from classes, so neither displaces the other', () => {
    // An atom minted as `rx_<pattern>` collides with the natural name of
    // any class spelling the same span: `%x31-39`'s atom took
    // `#RX___U0031__U0039` first and pushed the class to a suffixed name.
    const spec = emit([
      { name: 'top', alts: [[rx('[\\u0030-\\u0039]')], [rx('[\\u0031-\\u0039]')]] },
    ])
    assert.deepEqual(
      spec.rule.top.open.map((a) => a.s),
      ['#RX___U0030__U0039', '#RX___U0031__U0039'],
      'both classes keep the name they would have had unpartitioned',
    )
    for (const n of Object.keys(spec.options.match.token)) {
      assert.ok(n.startsWith('#RXA'), `${n} should be an atom token`)
    }
  })
})


describe('overlapping-class partition: the escapes it reads', () => {
  // A class the coverage reader cannot read stays out of the partition.
  // When another class overlaps it, both claim the shared characters, the
  // lexer's first cut picks one, and every alternative keyed on the other
  // is unreachable for them. So an escape that has one meaning must be
  // read as that meaning, and one read wrongly is worse still.

  // doc = x ; x = <a> t u / <b> u t ; t = ";" ";" ; u = "!" "!"
  const semi = (a, b) => emit([
    { name: 'doc', alts: [[ref('x')]] },
    { name: 'x', alts: [[a, ref('t'), ref('u')], [b, ref('u'), ref('t')]] },
    { name: 't', alts: [[lit(';'), lit(';')]] },
    { name: 'u', alts: [[lit('!'), lit('!')]] },
  ], { tag: 'cp', start: 'doc' })
  const parses = (spec, src) => {
    try {
      return 'doc' === new Tabnas().grammar(spec).parse(src).rule
    } catch (e) {
      return false
    }
  }

  it('reads a surrogate escape as the code unit it spells', () => {
    // ABNF `%x41-D800` lowers to `[A-\ud800]`. Without `u` the
    // matcher works in UTF-16 code units and `\ud800` is one of them.
    // Read as unknown, the class was left out of the partition beside
    // `[A-Z]`, and `A!!;;` was refused.
    const spec = semi(rx('[\\u0041-\\ud800]'), rx('[\\u0041-\\u005a]'))
    assert.deepEqual(spec.options.tokenSet, {
      RX___U0041__UD800: ['#RXA___U0041__U005A', '#RXA___U005B__UD800'],
      RX___U0041__U005A: ['#RXA___U0041__U005A'],
    })
    for (const src of ['A;;!!', 'A!!;;', 'b;;!!', 'Z!!;;']) {
      assert.ok(parses(spec, src), src)
    }
  })

  it('reads a surrogate pair written as two escapes as one code point under u', () => {
    // Under `u` (or `v`) `\uD83D\uDE00` is U+1F600, the one code point the
    // pair encodes, and `[\uD83D\uDE00]` holds it alone.
    const spec = semi(rx('[\\uD83D\\uDE00]', 'u'), rx('[\\u{1F600}-\\u{1F64F}]', 'u'))
    assert.deepEqual(Object.keys(spec.options.tokenSet ?? {}).sort(),
      ['RX___UD83D_UDE00', 'RX___U_1F600___U_1F64F'])
    for (const src of ['\u{1F600};;!!', '\u{1F600}!!;;', '\u{1F601}!!;;']) {
      assert.ok(parses(spec, src), src)
    }
    assert.ok(!parses(spec, '\u{1F601};;!!'), 'U+1F601 is not in the pair class')
  })

  it('reads a brace escape by the flags', () => {
    // Without `u` or `v`, `\u{61}` is `u` and then a quantifier, and
    // `[\u{61}]` holds `u`, `{`, `6`, `1` and `}`, never `a`. Read as
    // U+0061 regardless, the class was laid over the `a` atom of
    // `[a-z]`: `a` reached the branch whose matcher refuses it, and the
    // `u` the matcher takes was refused.
    const cls = semi(rx('[\\u{61}]'), rx('[a-z]'))
    for (const src of ['u;;!!', '{;;!!', 'a!!;;', 'u!!;;']) {
      assert.ok(parses(cls, src), src)
    }
    assert.ok(!parses(cls, 'a;;!!'), '[\\u{61}] does not take a without u')
    // The bare escape is `u` sixty-one times: not one code point, so it
    // keeps its own matcher rather than becoming the `a` atom.
    const bare = semi(rx('\\u{61}'), rx('[a-z]'))
    assert.deepEqual(bare.options.tokenSet ?? {}, {})
    assert.ok(parses(bare, 'u'.repeat(61) + ';;!!'))
    assert.ok(!parses(bare, 'a;;!!'))
    // Under `u` both are the code point a.
    const unicode = semi(rx('[\\u{61}]', 'u'), rx('[a-z]'))
    assert.ok(parses(unicode, 'a;;!!'))
    assert.ok(parses(unicode, 'a!!;;'))
  })

  it('compiles the XML Char production as it did before the escape reader', () => {
    // W3C XML's `Char ::= #x9 | #xA | #xD | [#x20-#xD7FF] | [#xE000-#xFFFD]
    // | [#x10000-#x10FFFF]`, as the EBNF front-end lowers it, beside a
    // negated class. The partition mints the atom `[\uD800-\uDFFF]` for
    // the surrogate block the negated class covers; read as unknown, every
    // set holding it contested every head, the content loop deepened from
    // 92 alternates to 103, and without negotiated lexing `<a>hi</a>` was
    // refused at its first character.
    const star = (inner) => ({ kind: 'star', inner })
    const plus = (inner) => ({ kind: 'plus', inner })
    const group = (...alts) => ({ kind: 'group', alts })
    const spec = emit([
      { name: 'document', alts: [[ref('content')]] },
      { name: 'content', alts: [[star(group([ref('element')], [ref('CharData')], [ref('Comment')]))]] },
      { name: 'element', alts: [[lit('<'), ref('Name'), lit('>'), ref('content'), lit('</'), ref('Name'), lit('>')]] },
      { name: 'CharData', alts: [[plus(rx('[^\\u003c\\u0026]', 'u'))]] },
      { name: 'Name', alts: [[rx('[\\u0061-\\u007a\\u0041-\\u005a]'),
        star(rx('[\\u0061-\\u007a\\u0041-\\u005a\\u0030-\\u0039]'))]] },
      { name: 'Comment', alts: [[lit('<!--'), star(ref('Char')), lit('-->')]] },
      { name: 'Char', alts: [[lit('\t')], [lit('\n')], [lit('\r')], [rx('[\\u0020-\\ud7ff]')],
        [rx('[\\ue000-\\ufffd]')], [rx('[\\u{10000}-\\u{10ffff}]', 'u')]] },
    ], { tag: 'cp', start: 'document' })
    const alternates = Object.values(spec.rule)
      .reduce((n, r) => n + (r.open ?? []).length + (r.close ?? []).length, 0)
    assert.equal(alternates, 92)
    assert.equal(Object.keys(spec.options.tokenSet).length, 6)
    assert.equal(Object.keys(spec.options.match.token).length, 16)
    // The negated class reads code points, so its surrogate atom does too:
    // compiled without `u`, it took the first half of U+1F600, which the
    // class takes whole (tabnas/bnf#75 review).
    assert.ok(Object.values(spec.options.match.token).map(String)
      .includes('/^[\\u{D800}-\\u{DFFF}]/u'), 'the surrogate atom is minted')
    for (const src of ['<a>hi</a>', '<a></a>', '<a><b>c</b></a>', 'hi', '<a>\u{1F600}</a>', '<a>\uD800</a>']) {
      assert.equal(new Tabnas().grammar(spec).parse(src).rule, 'document', src)
    }
  })

  it('compiles an atom naming a lead surrogate as its classes read it', () => {
    // Under `u` a lead surrogate is one standing alone: `[\uD800]/u` does
    // not take U+10000, whose first half it is. Laid over the partition
    // beside `[\uD800-\uDBFF]/u`, its atom was compiled without `u` and
    // took that first half, and `doc = [\uD800] [\uDC00-\uDFFF] / ...`
    // accepted U+10000, which it refuses with the class alone (tabnas/bnf#75
    // review).
    const lone = rx('[\\uD800]', 'u')
    const trail = rx('[\\uDC00-\\uDFFF]', 'u')
    const alone = emit([{ name: 'doc', alts: [[lone, trail]] }], { tag: 'cp', start: 'doc' })
    const spec = emit([
      { name: 'doc', alts: [[lone, trail], [rx('[\\uD800-\\uDBFF]', 'u'), lit('!')]] },
    ], { tag: 'cp', start: 'doc' })
    assert.deepEqual(matchers(spec), {
      '#RXA___U_D800___U_D800': '/^[\\u{D800}-\\u{D800}]/u',
      '#RX___UDC00__UDFFF': '/^[\\uDC00-\\uDFFF]/u',
      '#RXA___U_D801___U_DBFF': '/^[\\u{D801}-\\u{DBFF}]/u',
    })
    assert.ok(!parses(alone, '\u{10000}'))
    assert.ok(!parses(spec, '\u{10000}'))
    assert.ok(parses(spec, '\uD801!'))
  })

  it('leaves out a class read in code units naming a lead surrogate a code-point class names', () => {
    // `[A-\uD800]` without `u` takes U+D800 as the first half of U+10000
    // too; `[^a]/u` takes it only standing alone. No one atom reads it
    // both ways, so the class read in code units keeps its own matcher
    // and the rest are laid over the partition without it.
    const spec = emit([{ name: 'top', alts: [
      [rx('[\\u0041-\\ud800]')], [rx('[^a]', 'u')], [rx('[\\u0041-\\u005a]')],
    ] }])
    assert.deepEqual(Object.keys(spec.options.tokenSet).sort(), ['RX___A', 'RX___U0041__U005A'])
    assert.equal(matchers(spec)['#RX___U0041__UD800'], '/^[\\u0041-\\ud800]/')
    // A class read in code units that stops short of the lead surrogates
    // reads the same either way, and stays in.
    const short = emit([{ name: 'top', alts: [[rx('[\\u0041-\\ud7ff]')], [rx('[^a]', 'u')]] }])
    assert.deepEqual(Object.keys(short.options.tokenSet).sort(), ['RX___A', 'RX___U0041__UD7FF'])
  })
})
