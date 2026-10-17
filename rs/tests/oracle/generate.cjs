#!/usr/bin/env node
/* Copyright (c) 2026 Richard Rodger and other contributors, MIT License */
'use strict'

// The TypeScript oracle for the Rust port: writes the fixtures
// tests/oracle_test.rs replays. Each fixture is
//
//   { name, grammar, ir, opts, pure, recognition, recognitionError, cases,
//     transduce }
//
// where `ir` is the IR EXACTLY as a front-end handed it to the canonical
// `emitGrammarSpec` (captured by hooking the compiler module as each
// front-end resolves it), `pure` and `recognition` are the strict-jsonic
// texts TypeScript emits for that IR, each case records the TypeScript
// engine's verdict on one source, and `transduce` is the transducer part
// `transduceAlc` writes for the emitted spec under the fixture's name, or
// why the grammar is read whole.
//
// Needs the sibling checkouts built: ../../../../parser/ts,
// ../../../../abnf/ts and (optionally) ../../../../ebnf/ts, plus this
// repository's own ts/. See rs/AGENTS.md.
//
//   node rs/tests/oracle/generate.cjs corpus rs/tests/oracle
//   node rs/tests/oracle/generate.cjs file <grammar.abnf> <out.json>
//   node rs/tests/oracle/generate.cjs limits rs/tests/oracle
//   node rs/tests/oracle/generate.cjs transduce rs/tests/oracle
//   node rs/tests/oracle/generate.cjs regen <fixture.json>
//
// regen, limits and transduce need no front-end, only this repository's
// ts/ and the engine; corpus and file need the front-ends they convert
// with.

const Fs = require('node:fs')
const Path = require('node:path')

const REPO = Path.resolve(__dirname, '..', '..', '..')
const SIBLINGS = Path.resolve(REPO, '..')
const paths = {
  parser: Path.join(SIBLINGS, 'parser', 'ts', 'dist', 'tabnas'),
  bnf: Path.join(REPO, 'ts', 'dist', 'bnf'),
  compiler: Path.join(REPO, 'ts', 'dist', 'compiler'),
  abnf: Path.join(SIBLINGS, 'abnf', 'ts', 'dist', 'abnf'),
  ebnf: Path.join(SIBLINGS, 'ebnf', 'ts', 'dist', 'ebnf'),
}
function prerequisite(name) {
  if (!Fs.existsSync(paths[name] + '.js')) {
    throw new Error(`oracle prerequisite is missing: ${paths[name]}.js (build ${name}'s ts/ first)`)
  }
  return require(paths[name])
}
for (const name of ['parser', 'bnf', 'compiler']) prerequisite(name)

const bnf = require(paths.bnf)
const { Tabnas } = require(paths.parser)
const origEmit = require(paths.compiler).emitGrammarSpec

// Capture the exact IR a front-end hands to the compiler, whichever copy
// of @tabnas/bnf it resolved.
let captured = null
const hooked = new Set()
function hook(from) {
  let file
  try { file = require.resolve('@tabnas/bnf/dist/compiler', { paths: [from] }) }
  catch (e) { return }
  file = Fs.realpathSync(file)
  if (hooked.has(file)) return
  hooked.add(file)
  const mod = require(file)
  const orig = mod.emitGrammarSpec
  mod.emitGrammarSpec = (g, o) => {
    captured = { ir: JSON.parse(JSON.stringify(g)), opts: o ? { ...o } : {} }
    return orig(g, o)
  }
}
hook(Path.join(REPO, 'ts'))
hook(Path.join(SIBLINGS, 'abnf', 'ts'))
hook(Path.join(SIBLINGS, 'ebnf', 'ts'))

// The IR is captured through a JSON round-trip, which turns an unbounded
// repetition's `max: Infinity` into `null`. The fixture keeps `null` (it
// is what any JSON-carried IR holds, and what the Rust side reads as
// unbounded); TypeScript is handed the IR with `Infinity` restored, so
// the text it emits is the text a front-end's live IR produces.
function restoreInfinity(v) {
  if (Array.isArray(v)) return v.map(restoreInfinity)
  if (v && 'object' === typeof v) {
    const o = {}
    for (const k of Object.keys(v)) o[k] = restoreInfinity(v[k])
    if ('rep' === o.kind && null == o.max) o.max = Infinity
    return o
  }
  return v
}

function fixtureFor(name, ir, opts, sources) {
  ir = restoreInfinity(ir)
  // An IR the compiler REFUSES is a fixture too: the refusal message is
  // what a front-end shows the author, so it is graded like the text.
  let live
  try {
    live = origEmit(ir, { ...opts, builtins: true })
  }
  catch (e) {
    return { name, ir, opts, pure: null, pureError: String(e.message),
      recognition: null, recognitionError: null, cases: [], transduce: null }
  }
  const pure = bnf.toJsonic(bnf.toPureSpec(live), { strict: true })
  const closure = origEmit(ir, { ...opts, builtins: false })
  let recognition = null
  let recognitionError = null
  try {
    recognition = bnf.toJsonic(bnf.toRecognitionSpec(closure), { strict: true })
  }
  catch (e) {
    recognitionError = String(e.message)
  }
  const cases = []
  for (const source of sources) {
    const parser = new Tabnas()
    parser.grammar(JSON.parse(pure))
    try {
      const value = parser.parse(source)
      cases.push({
        source,
        accepted: true,
        value: JSON.parse(JSON.stringify(value === undefined ? null : value)),
      })
    }
    catch (e) {
      cases.push({ source, accepted: false, code: String(e.code) })
    }
  }
  const transduce = bnf.transduceAlc(live, name)
  return { name, ir, opts, pure, pureError: null, recognition, recognitionError, cases, transduce }
}

// The grammars the parser repository's ci/rust/notation-corpus.js proves
// run on the Rust engine when compiled by TypeScript.
function suites() {
  const abnf = prerequisite('abnf')
  const out = [{
    name: 'abnf',
    convert: (g) => abnf.abnfConvert(g, { builtins: true }),
    cases: [
      { grammar: 'greet = "hi" / "hello"', sources: ['hi', 'hello', 'nope', 'h'] },
      { grammar: 'pair = "a" "b"', sources: ['ab', 'a', 'ba'] },
      { grammar: 'expr = term *("+" term)\nterm = "(" expr ")" / number\nnumber = 1*DIGIT',
        sources: ['1', '1+2', '(1+2)+3', '1+', '(1'] },
      { grammar: 'R = [ A "@" ] A\nA = 1*ALPHA', sources: ['ab', 'a@b', 'a', 'a@', '@'] },
    ],
  }]
  if (Fs.existsSync(paths.ebnf + '.js')) {
    const ebnf = require(paths.ebnf)
    out.push({
      name: 'ebnf',
      convert: (g) => ebnf.ebnfConvert(g, { builtins: true }),
      cases: [
        { grammar: 'Greet ::= "hi" | "hello"', sources: ['hi', 'hello', 'nope'] },
        { grammar: 'Pair ::= "a" "b" "c"', sources: ['abc', 'ab', 'bac'] },
        { grammar: 'A ::= "x"* "end"', sources: ['end', 'x end', 'x x x end', 'y end'] },
        { grammar: 'A ::= [0-9]+', sources: ['1', '1234', 'abc'] },
        { grammar: 'A ::= ( "a" | "b" ) "c"', sources: ['ac', 'bc', 'cc'] },
        { grammar: 'A ::= #x41 #x42', sources: ['AB', 'ab'] },
      ],
    })
  }
  return out
}

function corpus(outdir) {
  Fs.mkdirSync(outdir, { recursive: true })
  let n = 0
  for (const suite of suites()) {
    suite.cases.forEach((entry, i) => {
      captured = null
      suite.convert(entry.grammar)
      if (!captured) throw new Error('emitGrammarSpec was not called')
      const name = `${suite.name}-${i}`
      const fx = fixtureFor(name, captured.ir, { ...captured.opts, builtins: undefined }, entry.sources)
      fx.grammar = entry.grammar
      Fs.writeFileSync(Path.join(outdir, name + '.json'), JSON.stringify(fx, null, 1))
      n++
    })
  }
  console.log(`wrote ${n} fixtures to ${outdir}`)
}

// One ABNF file, no sources: the emitted text alone is the check.
function file(grammarFile, out) {
  const abnf = prerequisite('abnf')
  const grammar = Fs.readFileSync(grammarFile, 'utf8')
  captured = null
  abnf.abnfConvert(grammar, { builtins: true })
  if (!captured) throw new Error('emitGrammarSpec was not called')
  const name = Path.basename(out).replace(/\.json$/, '')
  const fx = fixtureFor(name, captured.ir, { ...captured.opts, builtins: undefined }, [])
  fx.grammar = grammar
  Fs.writeFileSync(out, JSON.stringify(fx, null, 1))
}

// Regenerate a fixture from its own IR. A fixture built from hand-written
// IR (the `ir-*` ones, which pin shapes no front-end emits) has no grammar
// text to go back to, so the fixture itself is the input: its `ir`, `opts`
// and the sources of its `cases` are replayed through TypeScript again.
// Needs no front-end, only this repository's ts/ and the engine.
function regen(fixtureFile) {
  const text = Fs.readFileSync(fixtureFile, 'utf8')
  const fx = JSON.parse(text)
  const sources = (fx.cases || []).map((c) => c.source)
  const out = fixtureFor(fx.name, fx.ir, fx.opts || {}, sources)
  if (null != fx.grammar) out.grammar = fx.grammar
  // A fixture written on one line (see `limits`) stays on one line.
  const compact = !text.trimEnd().includes('\n')
  Fs.writeFileSync(fixtureFile,
    compact ? JSON.stringify(out) : JSON.stringify(out, null, 1))
  console.log(`regenerated ${fixtureFile}`)
}

// The emit limit's refusal fixtures (MAX_EMIT_UNITS in ts/src/compiler.ts),
// built here rather than captured from a front-end, with their strings
// case-insensitive as abnf hands them over (no `caseSensitive` field). Their
// IRs are large, so they are written on one line.
//
//   ir-emit-units-probes   one rule of 500 alternatives, each an optional
//                          prefix ["a" "d"] before "a" v, over the 1,750
//                          tokens of v: 500 probes, the last refused
//   ir-emit-units-options  A = [x0] … [x39] "y" / [x1] … [x39] "y" / … /
//                          "z", refused while its lookahead is enumerated
//   ir-emit-units-remove   2,642 character classes nested inside one
//                          another, which write all but 664 units, and a
//                          removal list of 333 names, 2 units each
function limits(outdir) {
  const lit = (literal) => ({ kind: 'term', literal })
  const ref = (name) => ({ kind: 'ref', name })
  const opt = (inner) => ({ kind: 'opt', inner })
  const probes = (p, v) => ({
    productions: [
      {
        name: 't',
        alts: Array.from({ length: p }, () => [
          opt({ kind: 'group', alts: [[lit('a'), lit('d')]] }), lit('a'), ref('v'),
        ]),
      },
      { name: 'v', alts: Array.from({ length: v }, (_, i) => [lit('w' + i)]) },
    ],
  })
  const options = (k) => ({
    productions: [{
      name: 'A',
      alts: [
        ...Array.from({ length: k }, (_, j) => [
          ...Array.from({ length: k - j }, (_, i) => opt(lit('x' + (i + j)))),
          lit('y'),
        ]),
        [lit('z')],
      ],
    }],
  })
  const removing = (c, n) => ({
    productions: [{
      name: 'A',
      alts: Array.from({ length: c }, (_, i) => [{
        kind: 'regex',
        pattern: `[${String.fromCodePoint(0x100)}-${String.fromCodePoint(0x101 + i)}]`,
        flags: 'u',
      }]),
    }],
    remove: Array.from({ length: n }, (_, i) => 'gone' + i),
  })
  Fs.mkdirSync(outdir, { recursive: true })
  for (const [name, ir] of [
    ['ir-emit-units-probes', probes(500, 1750)],
    ['ir-emit-units-options', options(40)],
    ['ir-emit-units-remove', removing(2642, 333)],
  ]) {
    const fx = fixtureFor(name, ir, { tag: 'demo' }, [])
    if (null == fx.pureError) throw new Error(`${name} compiled`)
    Fs.writeFileSync(Path.join(outdir, name + '.json'), JSON.stringify(fx))
    console.log(`wrote ${name}: ${fx.pureError}`)
  }
}

// The transducer part's fixtures: grammars whose value annotations make a
// part, built here from IR as tests/transduce.test.js builds them, so the
// ports are held to the part's text as to the spec's.
function transduce(outdir) {
  const ref = (name) => ({ kind: 'ref', name })
  const term = (literal) => ({ kind: 'term', literal })
  const re = (pattern) => ({ kind: 'regex', pattern, flags: '' })
  const digits = () => ({ kind: 'plus', inner: re('[0-9]') })
  const letters = () => ({ kind: 'plus', inner: re('[a-z]') })
  const group = (...alts) => ({ kind: 'group', alts })
  const fixtures = [
    ['ir-transduce-object', 'top', [
      { name: 'top', value: { kind: 'object', members: ['a', 'b', 'c'] },
        alts: [[ref('a'), term('.'), ref('b'), term('.'), ref('c')]] },
      { name: 'a', alts: [[digits()]] },
      { name: 'b', alts: [[digits()]] },
      { name: 'c', alts: [[digits()]] },
    ], ['1.2.30']],
    ['ir-transduce-nested', 'top', [
      { name: 'top', value: { kind: 'object', members: ['name', 'inner'] },
        alts: [[ref('name'), term('='), ref('inner')]] },
      { name: 'name', alts: [[letters()]] },
      { name: 'inner', value: { kind: 'object', members: ['maj', 'min'] },
        alts: [[ref('maj'), term('.'), ref('min')]] },
      { name: 'maj', alts: [[digits()]] },
      { name: 'min', alts: [[digits()]] },
    ], ['ab=1.2']],
    ['ir-transduce-list', 'list', [
      { name: 'list', value: { kind: 'array' },
        alts: [[ref('item'), { kind: 'star', inner: group([term(','), ref('item')]) }]] },
      { name: 'item', alts: [[digits()]] },
    ], ['1', '1,2,3']],
    ['ir-transduce-hosts', 'hosts', [
      { name: 'hosts', value: { kind: 'array' },
        alts: [[{ kind: 'star', inner: group([ref('entry'), term('\n')], [term('\n')]) },
          { kind: 'opt', inner: ref('entry') }]] },
      { name: 'entry', value: { kind: 'object', members: ['address', 'names'] },
        alts: [[ref('address'), ref('names')]] },
      { name: 'address', alts: [[ref('word')]] },
      { name: 'names', value: { kind: 'array' }, alts: [[{ kind: 'plus', inner: ref('word') }]] },
      { name: 'word', alts: [[group([re('[a-z0-9.:-]+')])]] },
    ], ['127.0.0.1 localhost\n::1 ip6-localhost ip6-loopback\n']],
  ]
  Fs.mkdirSync(outdir, { recursive: true })
  for (const [name, start, productions, sources] of fixtures) {
    const fx = fixtureFor(name, { productions }, { tag: 'tst', start }, sources)
    if (undefined === fx.transduce?.source) throw new Error(`${name} writes no part: ${fx.transduce?.whole}`)
    Fs.writeFileSync(Path.join(outdir, name + '.json'), JSON.stringify(fx, null, 1))
    console.log(`wrote ${name}`)
  }
}

const [mode, arg, arg2] = process.argv.slice(2)
if (mode === 'corpus') corpus(arg)
else if (mode === 'file') file(arg, arg2)
else if (mode === 'regen') regen(arg)
else if (mode === 'limits') limits(arg)
else if (mode === 'transduce') transduce(arg)
else {
  console.error('usage: generate.cjs corpus <outdir> | file <grammar.abnf> <out.json>' +
    ' | regen <fixture.json> | limits <outdir> | transduce <outdir>')
  process.exit(2)
}
