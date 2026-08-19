// Copyright (c) 2026 tabnas, MIT License

// Package bnf is the Go port of @tabnas/bnf: the notation-neutral
// compiler shared by the BNF-family grammar front-ends (abnf, gbnf,
// ebnf). It compiles a grammar IR into a tabnas GrammarSpec and parses
// no syntax itself.
//
// PORT STATUS: not yet implemented. The TypeScript implementation is
// canonical and lands first by design; this package currently exposes
// only VERSION so the module builds and the release tooling has
// something to check. The compiler itself — desugaring, left-recursion
// elimination, tail repeats, probe dispatch, literal lifting, token
// allocation, first sets and chain emission — is ported in a later
// change, mirroring ts/src/compiler.ts.
//
// Until then, use github.com/tabnas/abnf/go, which still carries its own
// copy of that pipeline.
package bnf

// VERSION is this module's version. It MUST equal ts/package.json
// "version": the release orchestrator rewrites both, and the version
// test fails the build if they drift.
const VERSION = "0.1.8"
