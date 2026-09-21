// Copyright (c) 2026 tabnas, MIT License

// Package bnf is the Go port of @tabnas/bnf: the notation-neutral
// compiler shared by the BNF-family grammar front-ends (abnf, gbnf,
// ebnf). It compiles a grammar IR into a tabnas GrammarSpec and parses
// no syntax itself.
//
// Lower your notation into a *Grammar, then call EmitGrammarSpec. That
// is the whole surface a front-end needs. Behind it the pipeline mirrors
// ts/src/compiler.ts: desugaring, left-recursion elimination, tail
// repeats, probe dispatch, literal lifting, token allocation, first sets
// and chain emission.
//
// The TypeScript implementation stays canonical. Where the two disagree,
// TypeScript wins, and DIVERGENCE.md records what cannot be repaired
// yet.
package bnf

// VERSION is this module's version. It MUST equal ts/package.json
// "version": the release orchestrator rewrites both, and the version
// test fails the build if they drift.
const VERSION = "0.1.16"
