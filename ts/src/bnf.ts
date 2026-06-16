/* Copyright (c) 2025-2026 Richard Rodger and other contributors, MIT License */

/*  bnf.ts
 *  BNF plugin — adds `tn.bnf(src)` (install) and `tn.bnf.toSpec(src)`
 *  (build without installing) to an Tabnas instance.
 *
 *  The conversion logic itself lives in ./converter.ts; this file
 *  exposes it both as a Plugin (for `tn.use(bnf)`) and as bare
 *  exports (for code that wants to convert without an instance).
 */

import type {
  Tabnas,
  GrammarSpec,
  Plugin,
} from '@tabnas/parser'

import {
  bnf as bnfConvert,
  parseBnf,
  emitGrammarSpec,
  eliminateLeftRecursion,
  bnfRules,
  BnfParseError,
  BnfConvertOptions,
} from './converter'

import {
  bnfCompile,
  toRecognitionSpec,
  toPureSpec,
  toJsonic,
  BnfCompileError,
  BnfCompileOptions,
} from './compile'


// Plugin entry point. Decorates the instance with a callable `bnf`
// member that converts and installs a grammar, plus `bnf.toSpec` for
// callers that just want the spec.
const bnf: Plugin = function bnf(tn: Tabnas, _options?: any): void {
  const fn = ((src: string, opts?: BnfConvertOptions): GrammarSpec => {
    const spec = bnfConvert(src, opts)
    tn.grammar(spec)
    return spec
  }) as ((src: string, opts?: BnfConvertOptions) => GrammarSpec) & {
    toSpec: (src: string, opts?: BnfConvertOptions) => GrammarSpec
  }
  fn.toSpec = (src: string, opts?: BnfConvertOptions): GrammarSpec =>
    bnfConvert(src, opts)
  tn.bnf = fn
}


export {
  bnf,
  bnfConvert,
  parseBnf,
  emitGrammarSpec,
  eliminateLeftRecursion,
  bnfRules,
  BnfParseError,
  bnfCompile,
  toRecognitionSpec,
  toPureSpec,
  toJsonic,
  BnfCompileError,
}

export type { BnfConvertOptions, BnfCompileOptions }
