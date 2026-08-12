// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

// core.go — the library's behaviour, in plain Go.
//
// The cgo layer in bnf_c.go is a thin shim over this: it converts
// (pointer, length) pairs to Go strings and Go strings to malloc'd C
// strings, and does nothing else. The split is not decoration — Go does
// not support cgo in _test.go files, so anything living beside
// `import "C"` cannot be unit-tested at all.
package main

import (
	"encoding/json"
	"strings"

	bnf "github.com/tabnas/bnf/go"
	tabnas "github.com/tabnas/parser/go"
)

// reply marshals a result document. Marshalling cannot fail for the
// shapes built here, and there is nowhere to report it if it did, so a
// failure degrades to a fixed error document rather than something the
// caller cannot decode.
func reply(v map[string]any) string {
	out, err := json.Marshal(v)
	if err != nil {
		return `{"ok":false,"error":{"code":"internal",` +
			`"message":"result could not be encoded"}}`
	}
	return string(out)
}

func failDoc(code, message string) string {
	return reply(map[string]any{
		"ok":    false,
		"error": map[string]any{"code": code, "message": message},
	})
}

func versionDoc() string {
	return reply(map[string]any{
		"ok": true, "version": bnf.VERSION, "engine": tabnas.VERSION,
	})
}

// reduce loads a serialized GrammarSpec, reduces it, and returns the
// result as spec text.
//
// `recognition` chooses which of the two reductions to run:
//
//   - true  — drop the AST-building hooks. What survives decides only
//     whether input is IN the language. Smaller, and the form a
//     validator wants.
//   - false — keep the tree `$`-builtins, so the reloaded grammar still
//     builds {rule, src, kids}. Still pure data.
//
// Both refuse a spec whose control logic is still closures: those cannot
// be represented as data at all, and a reduction that dropped them
// silently would return a grammar that no longer does what it says.
func reduce(specJSON string, recognition bool) string {
	gs, err := tabnas.GrammarSpecFromJSON([]byte(specJSON))
	if err != nil {
		return failDoc("spec", "spec is not valid JSON: "+err.Error())
	}

	var data map[string]any
	if recognition {
		data, err = bnf.ToRecognitionSpec(gs)
	} else {
		data, err = bnf.ToPureSpec(gs)
	}
	if err != nil {
		return failDoc("compile", firstLine(err.Error()))
	}

	// ToJsonic, not encoding/json: a regex is emitted as an "@/src/flags"
	// sentinel that the engine decodes on load. encoding/json sees the
	// holder's unexported fields and writes {}, which would drop every
	// match token and leave a grammar that lexes nothing.
	return reply(map[string]any{"ok": true, "spec": bnf.ToJsonic(data, true, 0)})
}

// firstLine trims a diagnostic to its headline and strips the terminal
// colouring it carries for humans. A JSON field is neither.
func firstLine(msg string) string {
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	return stripANSI(msg)
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			if i < len(s) {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
