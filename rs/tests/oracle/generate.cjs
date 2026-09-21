#!/usr/bin/env node
/* Copyright (c) 2026 Richard Rodger and other contributors, MIT License */
'use strict'

// The TypeScript oracle for the Rust port: writes the fixtures
// tests/oracle_test.rs replays. Each fixture is
//
//   { name, grammar, ir, opts, pure, recognition, recognitionError, cases }
//
// where `ir` is the IR EXACTLY as a front-end handed it to the canonical
// `emitGrammarSpec` (captured by hooking the compiler module as each
// front-end resolves it), `pure` and `recognition` are the strict-jsonic
// texts TypeScript emits for that IR, and each case records the
// TypeScript engine's verdict on one source.
//
// Needs the sibling checkouts built: ../../../../parser/ts,
// ../../../../abnf/ts and (optionally) ../../../../ebnf/ts, plus this
// repository's own ts/. See rs/AGENTS.md.
//
//   node rs/tests/oracle/generate.cjs corpus rs/tests/oracle
//   node rs/tests/oracle/generate.cjs file <grammar.abnf> <out.json>

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
for (const [name, file] of Object.entries(paths)) {
  if (name !== 'ebnf' && !Fs.existsSync(file + '.js')) {
    throw new Error(`oracle prerequisite is missing: ${file}.js (build ${name}'s ts/ first)`)
  }
}

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
  const live = origEmit(ir, { ...opts, builtins: true })
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
  return { name, ir, opts, pure, recognition, recognitionError, cases }
}

// The grammars the parser repository's ci/rust/notation-corpus.js proves
// run on the Rust engine when compiled by TypeScript.
function suites() {
  const abnf = require(paths.abnf)
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
  const abnf = require(paths.abnf)
  const grammar = Fs.readFileSync(grammarFile, 'utf8')
  captured = null
  abnf.abnfConvert(grammar, { builtins: true })
  if (!captured) throw new Error('emitGrammarSpec was not called')
  const name = Path.basename(out).replace(/\.json$/, '')
  const fx = fixtureFor(name, captured.ir, { ...captured.opts, builtins: undefined }, [])
  fx.grammar = grammar
  Fs.writeFileSync(out, JSON.stringify(fx, null, 1))
}

const [mode, arg, arg2] = process.argv.slice(2)
if (mode === 'corpus') corpus(arg)
else if (mode === 'file') file(arg, arg2)
else {
  console.error('usage: generate.cjs corpus <outdir> | file <grammar.abnf> <out.json>')
  process.exit(2)
}
