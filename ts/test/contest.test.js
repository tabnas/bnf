/* Copyright (c) 2026 Richard Rodger and other contributors, MIT License */
'use strict'

// Whether two dispatch heads CONTEST (can the lexer hand the same input
// to both) decides how deep the dispatcher looks. An answer of "no" that
// is wrong emits one-token entries for a decision that needs two, and
// the first branch commits on the first token and rejects input the
// other branch accepts. Each case here is a decision that was once
// declared uncontested and is not (tabnas/bnf#74 review). Go and Rust
// pin the same cases.

const { describe, it } = require('node:test')
const assert = require('node:assert')

const { emitGrammarSpec } = require('../dist/bnf')
const { Tabnas } = require('@tabnas/parser')

const lit = (s) => ({ kind: 'term', literal: s, caseSensitive: true })
const ilit = (s) => ({ kind: 'term', literal: s })
const rx = (pattern, flags) => ({ kind: 'regex', pattern, flags: flags || '' })
const tok = (name) => ({ kind: 'token', name })
const ref = (name) => ({ kind: 'ref', name })
const prod = (name, ...alts) => ({ name, alts })

// doc = x ; x = <a> t u / <b> u t ; t = "." "." ; u = "," ","
// The two branches part at the second token, so a one-token dispatch
// on contested heads commits to the first branch and cannot take the
// input the second accepts.
const grammar = (a, b) => ({
  productions: [
    prod('doc', [ref('x')]),
    prod('x', [a, ref('t'), ref('u')], [b, ref('u'), ref('t')]),
    prod('t', [lit('.'), lit('.')]),
    prod('u', [lit(','), lit(',')]),
  ],
})

// doc = x ; x = <a> t u / <b> u t ; t = ";" ";" ; u = "!" "!"
// The same shape with tails no engine matcher can take (a number would
// swallow the `.` of `1..`).
const semi = (a, b) => ({
  productions: [
    prod('doc', [ref('x')]),
    prod('x', [a, ref('t'), ref('u')], [b, ref('u'), ref('t')]),
    prod('t', [lit(';'), lit(';')]),
    prod('u', [lit('!'), lit('!')]),
  ],
})
const depthsOf = (spec) => spec.rule.x.open.map((o) => o.s.split(' ').length)

const parses = (spec, src, opts) => {
  const tn = new Tabnas(opts || {}).grammar(spec)
  return tn.parse(src).rule === 'doc'
}

describe('contest', () => {

  it('a word keyword contests a longer literal that continues with punctuation', () => {
    // Under wordKeywords "a" is guarded by a word boundary, which "a-b"
    // satisfies at its hyphen: both heads can claim `a-b`.
    const spec = emitGrammarSpec(
      grammar(lit('a'), lit('a-b')), { tag: 'ct', start: 'doc', wordKeywords: true })
    const [first, second] = spec.rule.x.open.map((o) => o.s.split(' ').length)
    assert.equal(first, 2, 'the "a" branch peeks a second token')
    assert.equal(second, 2, 'the "a-b" branch peeks a second token')
    assert.ok(parses(spec, 'a-b,,..', { lex: { relex: true } }))
    assert.ok(parses(spec, 'a..,,', { lex: { relex: true } }))
    // Whereas a word continuation keeps the guard: "ab" never meets "a".
    const word = emitGrammarSpec(
      grammar(lit('a'), lit('ab')), { tag: 'ct', start: 'doc', wordKeywords: true })
    assert.deepEqual(word.rule.x.open.map((o) => o.s.split(' ').length), [1, 1])
  })

  it('case-insensitive literals contest by what their matchers fold', () => {
    // A literal with no ASCII letter compiles to an exact fixed token
    // (isEffectivelyCaseSensitive), so `Σ` and `ς` never meet at the
    // lexer, whatever Unicode folding would say, and the decision stays
    // one token deep. Both inputs parse either way.
    const spec = emitGrammarSpec(grammar(ilit('Σ'), ilit('ς')), { tag: 'ct', start: 'doc' })
    assert.deepEqual(Object.values(spec.options.fixed.token).slice(0, 2), ['Σ', 'ς'])
    assert.deepEqual(spec.rule.x.open.map((o) => o.s.split(' ').length), [1, 1])
    assert.ok(parses(spec, 'ς,,..'))
    assert.ok(parses(spec, 'Σ..,,'))
    // With ASCII letters the matcher folds case, and so does the
    // predicate: "A-b" begins as "a" does, in either case.
    const ascii = emitGrammarSpec(grammar(ilit('a'), ilit('A-b')), { tag: 'ct', start: 'doc' })
    assert.deepEqual(ascii.rule.x.open.map((o) => o.s.split(' ').length), [2, 2])
    assert.ok(parses(ascii, 'A-B,,..', { lex: { relex: true } }))
    assert.ok(parses(ascii, 'A..,,', { lex: { relex: true } }))
  })

  it('a regex whose first character is not one atom contests by what it can match', () => {
    // `a|b` can begin with b; its first textual atom alone says a.
    const spec = emitGrammarSpec(grammar(rx('a|b'), lit('b')), { tag: 'ct', start: 'doc' })
    assert.deepEqual(spec.rule.x.open.map((o) => o.s.split(' ').length), [2, 2])
    assert.ok(parses(spec, 'b,,..', { lex: { relex: true } }))
    assert.ok(parses(spec, 'a..,,', { lex: { relex: true } }))
  })

  it('the ANY token contests every head', () => {
    const spec = emitGrammarSpec(grammar(tok('#AA'), lit('b')), { tag: 'ct', start: 'doc' })
    assert.deepEqual(spec.rule.x.open.map((o) => o.s.split(' ').length), [2, 2])
    assert.ok(parses(spec, 'b,,..'))
    assert.ok(parses(spec, 'z..,,'))
  })


  it('a regex head whose first character the coverage cannot name contests', () => {
    // `\n`, `\t` and `\v` are one atom each, but patternCharRanges declines
    // to name what a control escape covers, so nothing says the literal
    // character is not what the pattern matches.
    for (const [pattern, literal] of [['\\n', '\n'], ['\\t', '\t'], ['\\v', '\v']]) {
      const spec = emitGrammarSpec(grammar(rx(pattern), lit(literal)), { tag: 'ct', start: 'doc' })
      assert.deepEqual(spec.rule.x.open.map((o) => o.s.split(' ').length), [2, 2], pattern)
    }
    const spec = emitGrammarSpec(grammar(rx('\\v'), lit('\v')), { tag: 'ct', start: 'doc' })
    assert.ok(parses(spec, '\v..,,', { lex: { relex: true } }))
    assert.ok(parses(spec, '\v,,..', { lex: { relex: true } }))
  })

  it('a case-insensitive head beyond ASCII contests what Unicode folding lets it meet', () => {
    // `/[Σ]/i` takes `ς`, and the coverage folds ASCII letters alone.
    const spec = emitGrammarSpec(grammar(rx('[Σ]', 'i'), lit('ς')), { tag: 'ct', start: 'doc' })
    assert.deepEqual(spec.rule.x.open.map((o) => o.s.split(' ').length), [2, 2])
    for (const src of ['ς..,,', 'Σ..,,', 'ς,,..']) {
      assert.ok(parses(spec, src, { lex: { relex: true } }), src)
    }
    // Within ASCII the folded coverage stays exact: `/[b]/i` meets no `a`.
    const ascii = emitGrammarSpec(grammar(rx('[b]', 'i'), lit('a')), { tag: 'ct', start: 'doc' })
    assert.deepEqual(ascii.rule.x.open.map((o) => o.s.split(' ').length), [1, 1])
  })


  it('an engine token contests what its matcher can take when the parser asks', () => {
    // Negotiated lexing runs only the matchers that can produce the
    // token an alternative wants: the number matcher takes a leading
    // digit, the string matcher a quote, and the text matcher any text
    // no fixed literal claims. The four-token dispatch this replaced kept
    // each of these pairs apart; a one-token dispatch did not.
    for (const [a, b, text] of [
      [tok('#NR'), lit('1'), '1'],
      [tok('#ST'), lit("'a'"), "'a'"],
      [tok('#TX'), ilit('let'), 'let'],
      [tok('#NR'), tok('#TX'), '1'],
      [tok('#NR'), rx('[0-9]'), '1'],
      [tok('#TX'), rx('[a-z]+'), 'let'],
    ]) {
      const spec = emitGrammarSpec(semi(a, b), { tag: 'ct', start: 'doc' })
      assert.deepEqual(depthsOf(spec), [2, 2], a.name + ' / ' + (b.name || b.literal || b.pattern))
      assert.ok(parses(spec, text + ';;!!', { lex: { relex: true } }), text + ';;!!')
      assert.ok(parses(spec, text + '!!;;', { lex: { relex: true } }), text + '!!;;')
    }
    // The text matcher defers to a fixed literal, and an emitted grammar
    // lexes no values: those stay one token deep.
    for (const [a, b] of [[tok('#TX'), lit('let')], [tok('#VL'), lit('true')]]) {
      const spec = emitGrammarSpec(semi(a, b), { tag: 'ct', start: 'doc' })
      assert.deepEqual(depthsOf(spec), [1, 1], a.name)
    }
  })

  it('a brace escape is a code point only under the u flag', () => {
    // Without `u` or `v`, `\u{1}` is `u` once, not U+0001.
    const legacy = emitGrammarSpec(semi(rx('\\u{1}'), lit('u')), { tag: 'ct', start: 'doc' })
    assert.deepEqual(depthsOf(legacy), [2, 2])
    assert.ok(parses(legacy, 'u;;!!', { lex: { relex: true } }))
    assert.ok(parses(legacy, 'u!!;;', { lex: { relex: true } }))
    const unicode = emitGrammarSpec(semi(rx('\\u{1}', 'u'), lit('u')), { tag: 'ct', start: 'doc' })
    assert.deepEqual(depthsOf(unicode), [1, 1])
  })

  it('an escape whose code point cannot be read is not an exact head', () => {
    // Without the `u` flag `\u1` is `u` then `1`, and `\x1` is `x` then
    // `1`: neither is U+0001. `\cA`, `\p{L}`, `\k` and a digit escape
    // name a control character, a property, a group or a backreference,
    // none of which is the letter after the backslash. A head the
    // coverage cannot read contests every head.
    for (const [pattern, text] of [['\\u1', 'u1'], ['\\x1', 'x1']]) {
      const spec = emitGrammarSpec(semi(rx(pattern), lit(text[0])), { tag: 'ct', start: 'doc' })
      assert.deepEqual(depthsOf(spec), [2, 2], pattern)
      assert.ok(parses(spec, text + ';;!!', { lex: { relex: true } }), text + ';;!!')
      assert.ok(parses(spec, text[0] + '!!;;', { lex: { relex: true } }), text[0] + '!!;;')
    }
    for (const [pattern, flags, other] of [
      ['\\cA', '', 'c'],
      ['[\\p{L}]', 'u', 'é'],
      ['[\\1]', '', '1'],
    ]) {
      const spec = emitGrammarSpec(semi(rx(pattern, flags), lit(other)), { tag: 'ct', start: 'doc' })
      assert.deepEqual(depthsOf(spec), [2, 2], pattern)
    }
    // Four hex digits, or two, are the code point they spell.
    const exact = emitGrammarSpec(semi(rx('\\u0041'), lit('u')), { tag: 'ct', start: 'doc' })
    assert.deepEqual(depthsOf(exact), [1, 1])
  })

  it('a repetition ending on a surrogate escape keeps its exit guard', () => {
    // doc = *[\u0041-\ud800] "A" ";" -- ABNF `*%x41-D800 %x41 %x3B`. The
    // class covers the `A` the tail needs, so the loop carries a two-token
    // exit guard that yields it. Coverage read as unknown meets nothing
    // (tokensOverlap), so the guard was dropped and the loop ate the `A`.
    const star = (inner) => ({ kind: 'star', inner })
    const spec = emitGrammarSpec({
      productions: [prod('doc', [star(rx('[\\u0041-\\ud800]')), lit('A'), lit(';')])],
    }, { tag: 'ct', start: 'doc' })
    const guards = Object.values(spec.rule)
      .flatMap((r) => r.open ?? []).filter((o) => '#A #T' === o.s && 2 === o.b)
    assert.equal(guards.length, 1, 'the loop yields on `A ;`')
    for (const src of ['A;', 'BA;', 'BBA;']) {
      assert.ok(parses(spec, src, { lex: { relex: true } }), src)
    }
  })

})
