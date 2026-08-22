/* Copyright (c) 2026 Richard Rodger and other contributors, MIT License */

/*  bnf.ts
 *  @tabnas/bnf — the shared compiler behind the BNF-family grammar
 *  front-ends.
 *
 *  This package holds no notation of its own. It defines an
 *  intermediate representation (`Grammar`, a list of `Production`s over
 *  `Element`s) and compiles that IR into a tabnas `GrammarSpec`. A
 *  front-end parses one concrete syntax into the IR:
 *
 *    @tabnas/abnf   RFC 5234 ABNF
 *    @tabnas/gbnf   llama.cpp GBNF
 *    @tabnas/ebnf   EBNF (best effort — see that package)
 *
 *    ABNF/GBNF/EBNF text ──front-end──▶ Grammar ──emitGrammarSpec──▶ GrammarSpec
 *
 *  Everything hard about that second arrow lives here: desugaring
 *  repetition into helper rules, eliminating left recursion, rewriting
 *  tail repeats into same-depth loops, the probe/rewind dispatcher for
 *  prefixes that exceed the engine's bounded lookahead, lifting literals
 *  into named lexer tokens, allocating token names, first-set analysis,
 *  and chaining multi-reference alternatives through `$stepN` rules.
 *
 *  Writing a new front-end therefore means writing a parser for your
 *  notation that produces `Production[]`, and nothing else.
 */

export {
  EmitError,
  emitGrammarSpec,
  eliminateLeftRecursion,
  refsIn,
  escapeRegExp,
  termKey,
  isEffectivelyCaseSensitive,
  BUILTIN_TOKENS,
  REMOVE_PROSE,
  REMOVE_ALL,
  isProseName,
} from './compiler'

export type {
  ConvertOptions,
  SrcSpan,
  Element,
  Sequence,
  Production,
  Grammar,
  ProbeDispatchSpec,
  AmbiguityReport,
} from './compiler'

export {
  compileSpec,
  toRecognitionSpec,
  toPureSpec,
  toJsonic,
  attachActions,
  attachActionSlots,
  markListing,
  CompileError,
  ActionError,
} from './spec'

export type { CompileOptions, JsonicOptions, ActionsMap } from './spec'

// VERSION is this package's version. It MUST equal package.json
// "version": the release orchestrator rewrites both, and the version
// test fails the build if they drift. Mirrors `const VERSION` in
// go/bnf.go.
export const VERSION = '0.1.10'
