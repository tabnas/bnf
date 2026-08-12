// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

// Package main builds the C-ABI shared library: libbnf.
//
//	go build -buildmode=c-shared -o libbnf.so ./clib
//
// WHAT THIS IS FOR. @tabnas/bnf is the shared compiler behind the
// BNF-family front-ends (GBNF, ABNF, EBNF) — it is a library those
// front-ends call, not something an end user drives directly. So the C
// surface is deliberately narrow: it exposes the one capability that is
// useful WITHOUT a front-end, which is reducing an already-serialized
// GrammarSpec to pure data.
//
// Two reductions, and the difference matters:
//
//   - RECOGNITION drops the AST-building hooks. What is left answers
//     only "is this input in the language" — which is what a validator
//     needs, and is smaller.
//   - PURE keeps the tree `$`-builtins, so a reloaded grammar still
//     builds {rule, src, kids}.
//
// The output is a spec `libtabnas` can load, so the pipeline a caller
// without Go or Node can assemble is:
//
//	GBNF text --libgbnf--> spec --libbnf--> recognition spec
//	                                              |
//	                                        libtabnas --> verdicts
//
// NOT A COMPILER ENTRY POINT. There is no "notation text in" function
// here, because this package parses no notation — a front-end does. Use
// libgbnf (tabnas/gbnf) for GBNF.
//
// EVERY CALL RETURNS JSON. A C ABI has one return value and no
// exceptions, so each entry point returns a malloc'd JSON document and a
// binding in any language is call-and-decode.
//
// OWNERSHIP. Every char* returned here is the caller's and must be
// released with bnf_free. Nothing else crosses.
//
// LENGTHS ARE EXPLICIT. Spec arguments take a byte length rather than
// being read as NUL-terminated C strings.
//
// This file is the marshalling shim ONLY. The behaviour lives in core.go
// so that it can be unit-tested: Go does not support cgo in _test.go
// files, so nothing beside `import "C"` is reachable from a test.
package main

/*
#include <stdlib.h>
*/
import "C"

import "unsafe"

// goBytes copies a (pointer, length) pair into Go memory. The C memory
// belongs to the caller and may be freed the moment this returns.
//
// (NULL, 0) is accepted as the empty buffer, which is how C conveys one;
// it then fails as an invalid spec rather than as a usage error, which
// is the honest description of an empty spec.
func goBytes(src *C.char, n C.int) (string, bool) {
	if n < 0 {
		return "", false
	}
	if src == nil {
		return "", n == 0
	}
	return C.GoStringN(src, n), true
}

//export bnf_version
func bnf_version() *C.char {
	return C.CString(versionDoc())
}

// bnf_recognition_spec reduces a serialized spec to a function-free
// RECOGNITION grammar — AST-building hooks dropped.
//
//export bnf_recognition_spec
func bnf_recognition_spec(spec *C.char, specLen C.int) *C.char {
	src, ok := goBytes(spec, specLen)
	if !ok {
		return C.CString(failDoc("usage", "spec pointer or length is invalid"))
	}
	return C.CString(reduce(src, true))
}

// bnf_pure_spec reduces a serialized spec to pure data while KEEPING the
// tree-building builtins.
//
//export bnf_pure_spec
func bnf_pure_spec(spec *C.char, specLen C.int) *C.char {
	src, ok := goBytes(spec, specLen)
	if !ok {
		return C.CString(failDoc("usage", "spec pointer or length is invalid"))
	}
	return C.CString(reduce(src, false))
}

// bnf_free releases a string returned by any function here. C.CString
// allocates with malloc, so this is free(3); callers must not use their
// own allocator's free.
//
//export bnf_free
func bnf_free(s *C.char) {
	if s != nil {
		C.free(unsafe.Pointer(s))
	}
}

func main() {}
