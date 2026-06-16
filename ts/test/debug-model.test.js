/* Copyright (c) 2026 Richard Rodger and other contributors, MIT License */
'use strict'

// Composition test: a BNF-compiled grammar layered with the official
// @tabnas/debug plugin, asserting its structured `debug.model()` output.
// @tabnas/debug is a devDependency, but this resolves it dynamically and
// SKIPS when it is absent so the suite stays runnable outside the package;
// the `compose-debug` CI job can also point TABNAS_DEBUG_PATH at a sibling
// checkout's built plugin.

const { describe, it } = require('node:test')
const assert = require('node:assert')

const { Tabnas } = require('@tabnas/parser')
const { bnf: bnfPlugin } = require('..')

function loadDebug() {
  const candidates = [process.env.TABNAS_DEBUG_PATH, '@tabnas/debug'].filter(
    Boolean,
  )
  for (const c of candidates) {
    try {
      return require(c).Debug
    } catch {
      /* try next */
    }
  }
  return null
}

const Debug = loadDebug()
const skip = Debug ? false : '@tabnas/debug not available (set TABNAS_DEBUG_PATH)'

// A small self-contained ABNF grammar compiled via j.bnf(...), the same
// way this repo's own tests build a grammar instance (see probe.test.js).
//   greeting = hello / hi
//   hello    = "hello" name
//   hi       = "hi" name
//   name     = "world" / "there"
const GRAMMAR = `
greeting = hello / hi
hello    = "hello" name
hi       = "hi" name
name     = "world" / "there"
`

const makeParser = () => {
  const tn = new Tabnas({ plugins: [bnfPlugin] })
  const j = tn.make({ rewind: { history: 4096 } })
  j.bnf(GRAMMAR)
  j.use(Debug, { print: false, trace: false })
  return j
}

describe('compose: bnf + @tabnas/debug', () => {
  it('parses normally with the debug plugin installed', { skip }, () => {
    const j = makeParser()
    assert.doesNotThrow(() => j.parse('hello world'))
    assert.doesNotThrow(() => j.parse('hi there'))
  })

  it('debug.model() returns the structured BNF-compiled grammar', { skip }, () => {
    const j = makeParser()
    const m = j.debug.model()

    // The structured rule set: the four user productions plus the
    // synthetic `__start__` wrapper the BNF converter injects to ensure
    // end-of-source is consumed.
    assert.deepStrictEqual(
      m.rules.map((r) => r.name).sort(),
      ['__start__', 'greeting', 'hello', 'hi', 'name'],
    )

    // The entry rule is the synthetic wrapper; it pushes the real start
    // production (`greeting`).
    assert.equal(m.config.start, '__start__')
    const start = m.rules.find((r) => r.name === '__start__')
    assert.ok(
      start.open.some((a) => a.push === 'greeting'),
      '__start__ should push greeting',
    )
    // Its close consumes end-of-source (#ZZ).
    assert.ok(
      start.close.some((a) => a.seq.includes('#ZZ')),
      '__start__ close should match #ZZ (end-of-source)',
    )

    // Both plugins are listed.
    assert.ok(m.plugins.some((p) => p.name === 'bnf'), 'plugins should list bnf')
    assert.ok(
      m.plugins.some((p) => p.name === 'Debug'),
      'plugins should list Debug',
    )

    // greeting is a choice; its two open alts begin with the #HELLO /
    // #HI literals and both push the shared `name` tail rule.
    const greeting = m.rules.find((r) => r.name === 'greeting')
    assert.equal(greeting.open.length, 2, 'greeting has two open alts')
    assert.ok(
      greeting.open.some((a) => a.seq.includes('#HELLO')),
      'greeting should match #HELLO',
    )
    assert.ok(
      greeting.open.some((a) => a.seq.includes('#HI')),
      'greeting should match #HI',
    )
    assert.ok(
      greeting.open.every((a) => a.push === 'name'),
      'both greeting alts push name',
    )

    // The rule-reference graph agrees: greeting's open alts push only
    // `name`, and __start__'s push only `greeting`.
    const greetingEdges = m.graph.find((g) => g.name === 'greeting')
    assert.deepStrictEqual(greetingEdges.openPush, ['name'])
    const startEdges = m.graph.find((g) => g.name === '__start__')
    assert.deepStrictEqual(startEdges.openPush, ['greeting'])

    // name is a terminal choice (#WORLD / #THERE) with no rule pushes.
    const name = m.rules.find((r) => r.name === 'name')
    assert.equal(name.open.length, 2, 'name has two open alts')
    assert.ok(
      name.open.every((a) => a.push === undefined),
      'name alts push no rule',
    )

    // The model is JSON-serialisable and the rules round-trip.
    const grammar = {
      tokens: m.tokens,
      rules: m.rules,
      graph: m.graph,
      config: m.config,
      abnf: m.abnf,
    }
    assert.deepStrictEqual(JSON.parse(JSON.stringify(grammar)).rules, m.rules)
  })
})
