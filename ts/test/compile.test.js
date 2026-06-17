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
  bnf: bnfPlugin,
  bnfConvert,
  bnfCompile,
  toRecognitionSpec,
  toPureSpec,
  toJsonic,
  attachActions,
  attachActionSlots,
  markListing,
  BnfCompileError,
  BnfActionError,
} = require('..')


// Parse `input` with the live plugin (closures) — the reference tree.
function liveTree(src, input) {
  const tn = new Tabnas({ plugins: [bnfPlugin] })
  tn.bnf(src)
  return tn.parse(input)
}

// Parse `input` with the serialized full pure-data grammar (tree
// `$`-builtins reconstructed by the engine).
function pureTree(src, input) {
  const spec = resolveFuncRefs(JSON.parse(
    bnfCompile(src, { recognition: false, strict: true })))
  const tn = new Tabnas()
  tn.grammar(spec)
  return tn.parse(input)
}


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

  it('emits eager RegExp match tokens as @~/source/flags', () => {
    // "hi" is case-insensitive -> an eager regex match token.
    const text = bnfCompile('greet = "hi"')
    assert.match(text, /'@~\/\^hi\/i'/)
  })
})


// [ A "@" ] A — optional-prefix ambiguity that compiles to a probe
// dispatcher.
const PROBE_SRC = 'R = [ A "@" ] A\nA = 1*ALPHA'

describe('compile: full pure-data AST mode', () => {
  const CASES = [
    ['greet', 'greet = "hi" / "hello"', 'hello'],
    ['pair', 'pair = "a" "b"', 'ab'],
    ['arith',
      'expr = term *("+" term)\nterm = "(" expr ")" / number\nnumber = 1*DIGIT',
      '(1+2)+3'],
    ['probe', 'R = [ A "@" ] A\nA = 1*ALPHA', 'a@b'],
  ]

  for (const [name, src, input] of CASES) {
    it(`${name}: pure-data tree equals the live plugin tree`, () => {
      assert.deepStrictEqual(pureTree(src, input), liveTree(src, input))
    })

    it(`${name}: builtins conversion leaves no closures`, () => {
      const spec = bnfConvert(src, { builtins: true })
      assert.deepStrictEqual(Object.keys(spec.ref || {}), [],
        'ref map must be empty — every action is a $-builtin')
    })
  }

  it('full mode keeps tree builtins; recognition mode drops them', () => {
    const src = 'pair = "a" "b"'
    const full = bnfCompile(src, { recognition: false })
    assert.match(full, /@node\$/, 'full mode retains @node$')

    const recog = bnfCompile(src, { recognition: true })
    assert.doesNotMatch(recog, /@node\$|@capture\$|@bubble\$/,
      'recognition mode drops all tree builtins')
    assert.doesNotMatch(recog, /node\$|capture\$/,
      'recognition mode drops orphaned k config too')
  })

  it('toPureSpec rejects a closure spec (needs builtins:true)', () => {
    assert.throws(
      () => toPureSpec(bnfConvert('greet = "hi"')),
      (e) => e instanceof BnfCompileError,
    )
  })
})


describe('compile: eager match tokens', () => {
  it('preserves eager$ across serialization round-trip', () => {
    const spec = resolveFuncRefs(JSON.parse(
      bnfCompile('greet = "hi" / "hello"', { recognition: false, strict: true })))
    const hi = spec.options.match.token['#HI']
    assert.ok(hi instanceof RegExp, 'reconstructed as RegExp')
    assert.strictEqual(hi.eager$, true, 'eager$ flag survives')
    // and it still recognises
    const tn = new Tabnas()
    tn.grammar(spec)
    assert.doesNotThrow(() => tn.parse('hello'))
  })
})


describe('user actions (m-marks)', () => {
  it('binds alt actions by mark; compiler action runs first', () => {
    const tn = new Tabnas({ plugins: [bnfPlugin] })
    const log = []
    tn.bnf('op = "inc" / "dec"', {
      actions: {
        '@op:o:INC': (r) => { log.push('inc'); r.node.delta = 1 },
        '@op:o:DEC': (r) => { log.push('dec'); r.node.delta = -1 },
      },
    })
    const inc = tn.parse('inc')
    const dec = tn.parse('dec')
    assert.strictEqual(inc.delta, 1)
    assert.strictEqual(dec.delta, -1)
    // r.node existed (compiler's tree action ran before the user action)
    assert.strictEqual(inc.rule, 'op')
    assert.deepStrictEqual(log, ['inc', 'dec'])
  })

  it('multiple actions on one alt run in attachment order', () => {
    const tn = new Tabnas({ plugins: [bnfPlugin] })
    const log = []
    tn.bnf('op = "inc"', {
      actions: { '@op:o:INC': [() => log.push('a'), () => log.push('b')] },
    })
    tn.parse('inc')
    assert.deepStrictEqual(log, ['a', 'b'])
  })

  it('binds rule-phase hooks (bo)', () => {
    const tn = new Tabnas({ plugins: [bnfPlugin] })
    const log = []
    tn.bnf('g = "x"', { actions: { '@g:bo': () => log.push('enter') } })
    tn.parse('x')
    assert.deepStrictEqual(log, ['enter'])
  })

  it('rejects refs that match no rule/alt/hook', () => {
    const tn = new Tabnas({ plugins: [bnfPlugin] })
    for (const bad of ['@op:o:NOPE', '@nope:o:INC', '@op:zz']) {
      assert.throws(
        () => tn.bnf('op = "inc" / "dec"', { actions: { [bad]: () => {} } }),
        (e) => e instanceof BnfActionError,
        `should reject ${bad}`)
    }
  })

  it('markListing reports the assigned marks', () => {
    const listing = markListing(bnfConvert('op = "inc" / "dec"', { marks: true }))
    assert.match(listing, /op\s+o:INC/)
    assert.match(listing, /op\s+o:DEC/)
  })

  it('marks are opt-in (default conversion is unchanged)', () => {
    const spec = bnfConvert('op = "inc" / "dec"')
    const hasMark = JSON.stringify(spec.rule).includes('"m":')
    assert.strictEqual(hasMark, false)
  })
})


describe('user actions composed with $-builtins (pure data)', () => {
  it('live builtins-mode: user action runs after the @node$ tree builtin', () => {
    const spec = bnfConvert('op = "inc" / "dec"', { builtins: true, marks: true })
    attachActions(spec, { '@op:o:INC': (r) => { r.node.delta = 1 } })
    // no closures except the injected user fn; the tree action is @node$
    const tn = new Tabnas()
    tn.grammar(spec)
    const t = tn.parse('inc')
    assert.strictEqual(t.rule, 'op', 'tree built by @node$')
    assert.strictEqual(t.delta, 1, 'user action ran')
  })

  it('serialized slot bound at load time', () => {
    const spec = bnfConvert('op = "inc" / "dec"', { builtins: true, marks: true })
    attachActionSlots(spec, ['@op:o:INC'])
    const text = toJsonic(toPureSpec(spec), { strict: true })
    assert.match(text, /@op:o:INC/, 'slot serialized by name')
    assert.doesNotMatch(text, /@bnf_/, 'no closures in the wire format')

    // Consumer binds the slot at load.
    const obj = JSON.parse(text)
    obj.ref = { '@op:o:INC': (r) => { r.node.delta = 7 } }
    const tn = new Tabnas()
    tn.grammar(obj)
    const t = tn.parse('inc')
    assert.strictEqual(t.rule, 'op', 'tree still built by @node$')
    assert.strictEqual(t.delta, 7, 'load-bound user action ran')
  })

  it('engine array-a runs each action in order', () => {
    const log = []
    const tn = new Tabnas()
    tn.grammar({
      options: { rule: { start: 'g' }, match: { token: { '#A': /^a/ } } },
      ref: {
        '@one': () => log.push(1),
        '@two': () => log.push(2),
      },
      rule: { g: { open: [{ s: '#A', a: ['@one', '@two'] }] } },
    })
    tn.parse('a')
    assert.deepStrictEqual(log, [1, 2])
  })
})


describe('compile: probe grammars', () => {
  it('closure-mode probe is refused by toRecognitionSpec', () => {
    // Without `builtins`, the dispatcher uses registered closures for
    // its control logic, which cannot be emitted as pure data.
    const spec = bnfConvert(PROBE_SRC)
    assert.throws(
      () => toRecognitionSpec(spec),
      (e) => {
        assert.ok(e instanceof BnfCompileError, 'is BnfCompileError')
        assert.ok(e.rules.length > 0, 'lists offending rules')
        assert.match(e.message, /probe|lookahead/)
        return true
      },
    )
  })

  it('builtin-mode probe compiles to pure data', () => {
    const text = bnfCompile(PROBE_SRC)
    assert.doesNotMatch(text, /@bnf_/, 'no registered closures remain')
    assert.match(text, /@probeInit\$/)
    assert.match(text, /@probeDecide\$/)
    assert.match(text, /@probePhase0\$/)
  })

  it('builtin-mode probe round-trips into a working grammar', () => {
    const spec = resolveFuncRefs(JSON.parse(bnfCompile(PROBE_SRC, { strict: true })))
    const tn = new Tabnas()
    tn.grammar(spec)
    const acc = (s) => {
      try { tn.parse(s); return true } catch (e) { return false }
    }
    // With disambiguator: A "@" A. Without: A A (single A is the Y branch).
    assert.ok(acc('ab'), 'A A')
    assert.ok(acc('a@b'), 'A @ A')
    assert.ok(acc('a'), 'single A')
    assert.ok(!acc('a@'), 'A @ with no trailing A is rejected')
    assert.ok(!acc('@'), 'bare disambiguator rejected')
  })
})
