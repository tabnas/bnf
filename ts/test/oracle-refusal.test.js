/* Copyright (c) 2026 Richard Rodger and other contributors, MIT License */

// The shared fixtures under rs/tests/oracle/ that record a refusal: an IR
// this compiler refused, and the message it refused it with, written by
// rs/tests/oracle/generate.cjs. The Rust port's oracle test and the Go
// port's TestOracleRefusalsMatchTypeScript grade their own refusals
// against these messages byte for byte; this holds the canonical compiler
// to what it recorded, so a message that changes here without a
// regenerated fixture fails here first.

const Fs = require('node:fs')
const Path = require('node:path')
const { describe, it } = require('node:test')
const assert = require('node:assert')

const { emitGrammarSpec } = require('../dist/bnf')

const DIR = Path.join(__dirname, '..', '..', 'rs', 'tests', 'oracle')

// A fixture's IR is JSON, which writes a repetition's `max: Infinity` as
// null; generate.cjs reads it back as Infinity, and so does this.
const restoreInfinity = (v) => {
  if (Array.isArray(v)) return v.map(restoreInfinity)
  if (v && 'object' === typeof v) {
    const o = {}
    for (const k of Object.keys(v)) o[k] = restoreInfinity(v[k])
    if ('rep' === o.kind && null == o.max) o.max = Infinity
    return o
  }
  return v
}

describe('oracle refusals', () => {
  it('refuses each fixture\'s IR with the message it records', () => {
    const refusals = Fs.readdirSync(DIR)
      .filter((f) => f.startsWith('ir-') && f.endsWith('.json'))
      .map((f) => JSON.parse(Fs.readFileSync(Path.join(DIR, f), 'utf8')))
      .filter((fx) => null != fx.pureError)
    assert.ok(0 < refusals.length, 'no oracle fixture records a refusal')
    for (const fx of refusals) {
      assert.throws(
        () => emitGrammarSpec(restoreInfinity(fx.ir), { ...fx.opts, builtins: true }),
        { message: fx.pureError },
        fx.name,
      )
    }
  })
})
