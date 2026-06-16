/* Copyright (c) 2026 Richard Rodger, MIT License */

/*  compile.test.js
 *  Compilation mode: ABNF -> pure recognition grammar -> jsonic text.
 *
 *  Verifies that dropping the AST-building functions preserves
 *  *recognition* (the grammar still accepts/rejects correctly on a
 *  bare engine), that the output carries no function references, that
 *  it round-trips back into a working grammar, and that grammars
 *  needing control functions (probe / unbounded lookahead) are
 *  refused.
 */
'use strict'

const { describe, it } = require('node:test')
const assert = require('node:assert')

const { Tabnas } = require('@tabnas/parser')
const { resolveFuncRefs } = require('@tabnas/parser/utility')
const {
  bnfConvert,
  bnfCompile,
  toRecognitionSpec,
  toJsonic,
  BnfCompileError,
} = require('..')


// Walk a value; collect every string that is a key of the original
// spec's ref map (i.e. a surviving function reference).
function findRefs(value, refKeys) {
  const found = []
  const walk = (v) => {
    if (Array.isArray(v)) v.forEach(walk)
    else if (v && 'object' === typeof v && !(v instanceof RegExp)) {
      for (const k of Object.keys(v)) walk(v[k])
    } else if ('string' === typeof v && refKeys.has(v)) {
      found.push(v)
    }
  }
  walk(value)
  return found
}


// Install a (function-free) spec on a bare engine and report whether
// it accepts an input. Match-token `@/.../` strings are reconstructed
// to RegExp via the engine's own resolver, mirroring a real load.
function recognises(spec, input) {
  const loaded = resolveFuncRefs(JSON.parse(JSON.stringify(spec, (k, v) =>
    v instanceof RegExp ? '@/' + v.source + '/' + v.flags : v)))
  const tn = new Tabnas()
  tn.grammar(loaded)
  try {
    tn.parse(input)
    return true
  } catch (e) {
    return false
  }
}


const RECOGNITION_CASES = [
  {
    name: 'greet',
    src: 'greet = "hi" / "hello"',
    accept: ['hi', 'hello'],
    reject: ['nope', 'h'],
  },
  {
    name: 'pair',
    src: 'pair = "a" "b"',
    accept: ['ab'],
    reject: ['a', 'ba'],
  },
  {
    name: 'arith',
    src: 'expr = term *("+" term)\nterm = "(" expr ")" / number\nnumber = 1*DIGIT',
    accept: ['1', '1+2', '(1+2)+3'],
    reject: ['1+', '(1'],
  },
]


describe('compile: pure recognition grammar', () => {
  for (const tc of RECOGNITION_CASES) {
    it(`${tc.name}: spec is function-free and still recognises`, () => {
      const spec = bnfConvert(tc.src)
      const refKeys = new Set(Object.keys(spec.ref || {}))
      assert.ok(refKeys.size > 0, 'sanity: converted spec has refs')

      const rspec = toRecognitionSpec(spec)

      // No function references survive, and no live functions either.
      assert.deepStrictEqual(findRefs(rspec, refKeys), [],
        'recognition spec must carry no function references')
      assert.ok(!('ref' in rspec), 'ref map dropped')
      assert.strictEqual(
        JSON.stringify(rspec, (k, v) => 'function' === typeof v ? '<FN>' : v)
          .includes('<FN>'), false, 'no live functions remain')

      // Recognition is preserved on a bare engine.
      for (const ok of tc.accept) {
        assert.ok(recognises(rspec, ok), `should accept ${JSON.stringify(ok)}`)
      }
      for (const bad of tc.reject) {
        assert.ok(!recognises(rspec, bad),
          `should reject ${JSON.stringify(bad)}`)
      }
    })
  }
})


describe('compile: jsonic serialisation', () => {
  it('strict mode round-trips into a working grammar', () => {
    const spec = bnfConvert(RECOGNITION_CASES[0].src)
    const rspec = toRecognitionSpec(spec)
    const text = toJsonic(rspec, { strict: true })

    // Strict output is valid JSON.
    const reparsed = JSON.parse(text)
    const reloaded = resolveFuncRefs(reparsed)

    const tn = new Tabnas()
    tn.grammar(reloaded)
    assert.doesNotThrow(() => tn.parse('hi'))
    assert.doesNotThrow(() => tn.parse('hello'))
    assert.throws(() => tn.parse('nope'))
  })

  it('relaxed mode uses bare identifier keys and single quotes', () => {
    const text = bnfCompile(RECOGNITION_CASES[0].src)
    assert.match(text, /\n\s*open:/, 'bare identifier key (open:)')
    assert.match(text, /'#HI'|'#HELLO'/, 'single-quoted token strings')
    assert.doesNotMatch(text, /"open"/, 'keys are not double-quoted')
  })

  it('emits RegExp match tokens as @/source/flags', () => {
    // "hi" is case-insensitive -> a regex match token.
    const text = bnfCompile('greet = "hi"')
    assert.match(text, /'@\/\^hi\/i'/)
  })
})


describe('compile: probe grammars are refused', () => {
  it('throws BnfCompileError naming the offending rules', () => {
    // [ A "@" ] A — optional-prefix ambiguity needing the probe.
    const src = 'R = [ A "@" ] A\nA = 1*ALPHA'
    assert.throws(
      () => bnfCompile(src),
      (e) => {
        assert.ok(e instanceof BnfCompileError, 'is BnfCompileError')
        assert.ok(e.rules.length > 0, 'lists offending rules')
        assert.match(e.message, /probe|lookahead/)
        return true
      },
    )
  })
})
