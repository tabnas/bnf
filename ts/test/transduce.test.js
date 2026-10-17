/* Copyright (c) 2026 Richard Rodger and other contributors, MIT License */
'use strict'

// The transducer part an emitted grammar's value annotations write
// (src/transduce.ts): its text, its refusals, and what it writes over the
// grammar's own rule passes.
//
// alchemy's `transduce-tree` runs a part, and alchemy is no dependency of
// this package, so `tree` below models it, pass for pass as alchemy's
// stdlib/transduce.alc defines it, over the passes the engine reports as
// alchemy's parser-events source relays them (its tokens less those it
// gave back, a probe's dropped). The events it makes of a grammar's
// passes are held to the walk of the value the same grammar builds.
// alchemy-cli's differential holds the real transduce-tree, natively and
// interpreted, to the same walk.

const { describe, it } = require('node:test')
const assert = require('node:assert')

const { emitGrammarSpec, transduceAlc } = require('..')
const { Tabnas } = require('@tabnas/parser')

const ref = (name) => ({ kind: 'ref', name })
const term = (literal) => ({ kind: 'term', literal })
const digits = () => ({ kind: 'plus', inner: { kind: 'regex', pattern: '[0-9]', flags: '' } })
const letters = () => ({ kind: 'plus', inner: { kind: 'regex', pattern: '[a-z]', flags: '' } })
const star = (inner) => ({ kind: 'star', inner })
const group = (...alts) => ({ kind: 'group', alts })

const emit = (productions, start, builtins = true) =>
  emitGrammarSpec({ productions }, { tag: 'tst', start, builtins })

// The spec a part's text gives transduce-tree, rule by rule.
function specOf(source) {
  const spec = {}
  for (const line of source.split('\n')) {
    const m = /^ {6}entry :(\S+) \(record (.*)\)$/.exec(line)
    if (!m) continue
    const entry = {}
    for (const f of m[2].matchAll(/\(entry :(\w+) ([^)]*)\)/g)) {
      if ('container' === f[1]) entry.container = f[2].slice(1)
      else if ('member' === f[1]) entry.member = JSON.parse(f[2])
      else if ('text' === f[1]) entry.text = 'true' === f[2]
    }
    spec[m[1]] = entry
  }
  return spec
}

// The grammar's passes over `src`, as a parser-events source relays them,
// and the value the same parse builds.
function passesOf(spec, src) {
  const passes = []
  const parser = new Tabnas()
  parser.grammar(spec)
  parser.sub({
    ruleDone: (r, _ctx, done) => {
      if (true === done.probe) return
      const open = 'o' === done.state
      const recorded = open ? r.o.slice(0, r.oN) : r.c.slice(0, r.cN)
      const b = null == done.alt ? 0 : done.alt.b
      const tokens = recorded.slice(0, Math.max(0, recorded.length - b)).map((t) => String(t.src))
      passes.push({ open, name: r.name, r: null == done.alt ? '' : done.alt.r, tokens })
    },
  })
  const value = parser.parse(src)
  return { passes, value }
}

// alchemy's transduce-tree over the passes, as stdlib/transduce.alc's
// transduce-pass makes its events.
function tree(spec, passes) {
  const out = []
  let open = 0
  let carry = false
  const texts = []
  const spell = (p) => {
    if (0 < texts.length) texts[texts.length - 1] += p.tokens.join('')
  }
  for (const p of passes) {
    const e = spec[p.name]
    if (p.open) {
      spell(p)
      if (undefined === e) continue
      if (undefined !== e.container) {
        if (carry) carry = false
        else {
          open++
          out.push(['start', e.container])
        }
      }
      if (undefined !== e.member) out.push(['key', e.member])
      if (e.text) texts.push('')
      continue
    }
    if (undefined === e) {
      spell(p)
      continue
    }
    if (e.text) {
      const text = texts.pop()
      if (0 < texts.length) texts[texts.length - 1] += text
      out.push(['scalar', text])
    }
    spell(p)
    if (undefined !== e.container) {
      if ('' !== p.r && undefined !== spec[p.r]?.container) carry = true
      else {
        open--
        carry = false
        out.push(['end', e.container])
      }
    }
  }
  assert.equal(open, 0, 'the passes end inside a container')
  return out
}

// A value's events, as a walk of it gives them.
function walk(v, out = []) {
  if (Array.isArray(v)) {
    out.push(['start', 'array'])
    for (const x of v) walk(x, out)
    out.push(['end', 'array'])
  }
  else if (null != v && 'object' === typeof v) {
    out.push(['start', 'object'])
    for (const k of Object.keys(v)) {
      out.push(['key', k])
      walk(v[k], out)
    }
    out.push(['end', 'object'])
  }
  else out.push(['scalar', v])
  return out
}

// The part's events over each source equal the walk of the value, in both
// emit modes, and the two modes write the same text.
function streams(productions, start, sources) {
  const part = transduceAlc(emit(productions, start), 'g')
  assert.ok(undefined !== part.source, part.whole)
  assert.deepEqual(transduceAlc(emit(productions, start, false), 'g'), part)
  const spec = specOf(part.source)
  for (const builtins of [true, false]) {
    for (const src of sources) {
      const { passes, value } = passesOf(emit(productions, start, builtins), src)
      assert.deepEqual(tree(spec, passes), walk(value), src)
    }
  }
  return part
}

const TRIPLE = [
  { name: 'top', value: { kind: 'object', members: ['a', 'b', 'c'] },
    alts: [[ref('a'), term('.'), ref('b'), term('.'), ref('c')]] },
  { name: 'a', alts: [[digits()]] },
  { name: 'b', alts: [[digits()]] },
  { name: 'c', alts: [[digits()]] },
]

describe('transduce', () => {
  it('writes an object of three texts as transduce-tree over its chain', () => {
    const part = streams(TRIPLE, 'top', ['1.2.30', '7.8.9'])
    assert.equal(part.entry, 'g-transduce')
    assert.equal(part.source, [
      '; The transducer part of the grammar g: the JSON events of the value',
      '; its @object and @array annotations build, from its rule passes, as',
      '; tabnas-bnf wrote them from the value actions those annotations attach.',
      'def g-transduce [input]',
      '  transduce-tree',
      '    record',
      '      entry :top (record (entry :container :object) (entry :member "a") (entry :text true))',
      '      entry :top$step1 (record (entry :container :object) (entry :member "b") (entry :text true))',
      '      entry :top$step2 (record (entry :container :object) (entry :member "c") (entry :text true))',
      '    input',
      '',
    ].join('\n'))
  })

  it('nests a member that builds its own value', () => {
    streams([
      { name: 'top', value: { kind: 'object', members: ['name', 'inner'] },
        alts: [[ref('name'), term('='), ref('inner')]] },
      { name: 'name', alts: [[letters()]] },
      { name: 'inner', value: { kind: 'object', members: ['maj', 'min'] },
        alts: [[ref('maj'), term('.'), ref('min')]] },
      { name: 'maj', alts: [[digits()]] },
      { name: 'min', alts: [[digits()]] },
    ], 'top', ['ab=1.2'])
  })

  it('writes arrays of texts and of objects, and the empty ones', () => {
    streams([
      { name: 'top', value: { kind: 'array' }, alts: [[ref('a'), term(','), ref('b')]] },
      { name: 'a', alts: [[digits()]] },
      { name: 'b', alts: [[digits()]] },
    ], 'top', ['1,2'])
    streams([
      { name: 'top', value: { kind: 'array' },
        alts: [[ref('plain'), term(','), ref('one')]] },
      { name: 'plain', alts: [[digits()]] },
      { name: 'one', value: { kind: 'object', members: ['p', 'q'] },
        alts: [[ref('p'), term(':'), ref('q')]] },
      { name: 'p', alts: [[digits()]] },
      { name: 'q', alts: [[digits()]] },
    ], 'top', ['9,1:2'])
    streams([
      { name: 'list', value: { kind: 'array' },
        alts: [[ref('item'), star(group([term(','), ref('item')]))]] },
      { name: 'item', alts: [[digits()]] },
    ], 'list', ['1', '1,2,3', '10,200'])
    streams([
      { name: 'list', value: { kind: 'array' }, alts: [[star(ref('item'))]] },
      { name: 'item', alts: [[{ kind: 'regex', pattern: '[0-9]', flags: '' }]] },
    ], 'list', ['1', '123', ' '])
  })

  it('writes lines of records, each an object whose last member is an array', () => {
    // /etc/hosts as aless's grammar library writes it: an address and the
    // names it answers to, a line each.
    streams([
      { name: 'hosts', value: { kind: 'array' },
        alts: [[star(group([ref('entry'), term('\n')], [term('\n')])), { kind: 'opt', inner: ref('entry') }]] },
      { name: 'entry', value: { kind: 'object', members: ['address', 'names'] },
        alts: [[ref('address'), ref('names')]] },
      { name: 'address', alts: [[ref('word')]] },
      { name: 'names', value: { kind: 'array' }, alts: [[{ kind: 'plus', inner: ref('word') }]] },
      { name: 'word', alts: [[group([{ kind: 'regex', pattern: '[a-z0-9.:-]+', flags: '' }])]] },
    ], 'hosts', ['127.0.0.1 localhost\n::1 ip6-localhost ip6-loopback\n', '\n10.0.0.1 a b c\n'])
  })

  it('reads a grammar whole whose start rule has no annotation', () => {
    const part = transduceAlc(emit([
      { name: 'top', alts: [[ref('a'), term('.'), ref('b')]] },
      { name: 'a', alts: [[digits()]] },
      { name: 'b', alts: [[digits()]] },
    ], 'top'), 'g')
    assert.equal(part.source, undefined)
    assert.match(part.whole, /^the start rule top has no annotation, so the value is the \{rule, src, kids\} tree/)
  })

  it('reads a grammar whole whose text member holds a value rule', () => {
    // The compiler refuses such a grammar itself; a spec it did not emit
    // is held to the same rule. Here `b`, `top`'s second member and its
    // text, reaches `top` again.
    const spec = JSON.parse(JSON.stringify(emit(TRIPLE, 'top')))
    spec.rule.b.open[0].p = 'top'
    const part = transduceAlc(spec, 'g')
    assert.equal(part.whole, "rule top$step1's member is the text of rule b, beneath which rule top builds a value of its own")
  })

  it('writes nothing for an empty source, where the engine makes no passes', () => {
    // The engine returns at once on an empty source that the grammar
    // accepts, with no value and no rule pass, so no part has anything to
    // write: a host reads that document as the engine leaves it.
    const productions = [
      { name: 'list', value: { kind: 'array' }, alts: [[star(ref('item'))]] },
      { name: 'item', alts: [[{ kind: 'regex', pattern: '[0-9]', flags: '' }]] },
    ]
    const { passes, value } = passesOf(emit(productions, 'list'), '')
    assert.equal(value, undefined)
    assert.deepEqual(passes, [])
    assert.deepEqual(tree(specOf(transduceAlc(emit(productions, 'list'), 'g').source), passes), [])
  })

  it('refuses a name alchemy cannot give a definition', () => {
    for (const name of ['', '1g', 'true', 'a b', 'é']) {
      const part = transduceAlc(emit(TRIPLE, 'top'), name)
      assert.match(part.whole, /is not one alchemy can give a definition$/, name)
    }
    assert.equal(transduceAlc(emit(TRIPLE, 'top'), 'resolv.conf').entry, 'resolv.conf-transduce')
  })

  it('refuses value actions it does not read, and alternates that disagree', () => {
    const spec = emit(TRIPLE, 'top')
    const reset = JSON.parse(JSON.stringify(spec))
    reset.rule.top.open[0].a = ['@object$', '@reset$']
    assert.equal(transduceAlc(reset, 'g').whole, "rule top's open alternates use @reset$ as no transducer part reads it")
    const split = JSON.parse(JSON.stringify(spec))
    split.rule.top.open.push({ s: '#ZZ', g: 'tst' })
    assert.equal(transduceAlc(split, 'g').whole, "rule top's open alternates build its value differently")
  })
})
