// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

//! The shared compiler behind the BNF-family grammar front-ends for the
//! `tabnas` parsing engine.
//!
//! This crate holds no notation of its own. It defines an intermediate
//! representation (a [`Grammar`] of [`Production`]s over [`Element`]s)
//! and compiles that IR into a tabnas grammar document. A front-end
//! parses one concrete syntax (ABNF, GBNF, EBNF) into the IR and calls
//! [`emit_grammar_spec`]:
//!
//! ```text
//! ABNF / GBNF / EBNF text ──front-end──▶ Grammar ──emit_grammar_spec──▶ GrammarSpec
//! ```
//!
//! Everything hard about that second arrow lives here, and is shared:
//! desugaring repetition into helper rules, left-recursion elimination
//! (including the recursion hidden behind nullable sugar and the
//! suffix-debt counters that keep the generated tail loop from eating a
//! token the enclosing alternative still owes), tail-repeat rewriting,
//! probe dispatch for optional prefixes beyond the engine's bounded
//! lookahead, literal lifting into named lexer tokens, token allocation,
//! first-set analysis, and chain emission through synthetic `$stepN`
//! continuation rules.
//!
//! This is the Rust port of the canonical TypeScript implementation; the
//! TypeScript compiler is authoritative and this crate tracks it.
//!
//! ```
//! use tabnas_bnf::{emit_grammar_spec, ConvertOptions, Element, Grammar, Production};
//!
//! let grammar = Grammar::new(vec![
//!     Production::new("val", vec![vec![Element::reference("add")]]),
//!     Production::new("add", vec![vec![Element::token("#NR")]]),
//! ]);
//! let spec = emit_grammar_spec(&grammar, &ConvertOptions::tag("demo")).unwrap();
//! assert!(spec.rule.contains_key("val"));
//! ```

mod analysis;
mod annotate;
mod desugar;
mod emit;
mod factor;
mod ir;
mod leftrec;
mod probe;
mod prose;
mod ranges;
mod spec;

/// The README's Rust examples run as doctests, so a stale one fails the
/// gate rather than misleading the reader. Its `toml` and `bash` fences
/// are skipped; rustdoc runs only the `rust` ones.
#[cfg(doctest)]
#[doc = include_str!("../README.md")]
mod readme_examples {}

/// This crate's version. It MUST equal `ts/package.json` "version": the
/// release orchestrator rewrites both, and `tests/version_test.rs` fails
/// the build if they drift. Mirrors `VERSION` in `ts/src/bnf.ts` and
/// `const VERSION` in `go/bnf.go`.
pub const VERSION: &str = "0.1.16";

pub use emit::emit_grammar_spec;
pub use ir::{
    builtin_token, diag_name, escape_regexp, is_effectively_case_sensitive, is_prose_name,
    origin_of, refs_in, term_key, AmbiguityReport, ConvertOptions, Element, EmitError, Grammar,
    Kind, NodeKind, ProbeDispatchSpec, ProbeHelperSpec, Production, Sequence, SrcSpan,
    TailRepeatSpec, ValueAnnotation, BUILTIN_TOKENS, MAX_ELEMENT_DEPTH, REMOVE_ALL, REMOVE_PROSE,
};
pub use leftrec::eliminate_left_recursion;
pub use spec::{
    attach_action_slots, attach_actions, compile_spec, mark_listing, to_jsonic, to_pure_spec,
    to_recognition_spec, ActionError, ActionFn, ActionsMap, AltSpec, CompileError, CompileOptions,
    GrammarSpec, JsonicOptions, RefAction, RuleSpec,
};
