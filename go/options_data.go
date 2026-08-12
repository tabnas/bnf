// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

package bnf

// options_data.go — the typed Options a front-end leaves on a spec,
// converted into the map form the engine reads back.
//
// WHY THIS EXISTS. A GrammarSpec in TypeScript has ONE options field, a
// plain object, so `toRecognitionSpec` carries it through by writing
// `{options: spec.options, rule: spec.rule}` and serialisation is
// automatic. Go split that field in two — `Options` (typed struct) and
// `OptionsMap` (map) — and specToData only ever emitted three keys from
// the typed side: fixed.token, match.token and rule.start.
//
// That was true of the converter's own output and false of a
// front-end's. GBNF is scannerless: it sets an empty IGNORE token set
// and switches off every default matcher, because anything the lexer
// does on its own initiative — skipping space, claiming numbers or
// quoted strings — changes which inputs are in the language. Dropping
// those options did not lose a setting, it silently produced a grammar
// that accepts a DIFFERENT language: a serialized `arithmetic.gbnf`
// lexed "a+b" as one text token and rejected "a+b=c", which a natively
// installed copy of the same grammar accepts.
//
// So this is not a feature, it is the repair of a silent divergence
// from the canonical runtime, and the reason the emitted set is now
// derived from the Options struct rather than hand-picked.
//
// WHAT IT REFUSES. Some option fields hold functions (`Check`,
// `Exclude`, `Modify`, `TokenFn`). A function cannot cross a data
// boundary, and a serialized grammar that quietly lost one would be the
// same class of bug all over again — so a spec carrying one is refused
// by name. A gap is an assertion, not an omission.

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	tabnas "github.com/tabnas/parser/go"
)

// optionsHandled lists the Options fields optionsToData knows how to
// emit. Anything set outside this set is refused rather than dropped —
// which is what makes a future engine option a loud failure here instead
// of a quiet change of language downstream.
var optionsHandled = map[string]bool{
	"Fixed": true, "Match": true, "Rule": true, "TokenSet": true,
	"Space": true, "Line": true, "Text": true, "Number": true,
	"Comment": true, "String": true, "Value": true, "Lex": true,
	"Ender": true, "Tag": true,
	// Diagnostics. These do not change the accepted language, but they
	// ARE plain data and TS's cloneData carries them through, so
	// refusing them would be a Go-only failure for a spec the canonical
	// runtime serialises happily.
	"Error": true, "Hint": true, "Safe": true,
}

// optionsToData converts typed Options into the map form
// tabnas.MapToOptions reads. Returns nil when nothing is set.
func optionsToData(opt *tabnas.Options) (map[string]any, error) {
	if opt == nil {
		return map[string]any{}, nil
	}
	if err := refuseUnhandled(opt); err != nil {
		return nil, err
	}

	out := map[string]any{}
	var funcs []string

	if opt.Tag != "" {
		out["tag"] = opt.Tag
	}
	if len(opt.Ender) > 0 {
		out["ender"] = strsToAny(opt.Ender)
	}
	if opt.TokenSet != nil {
		ts := map[string]any{}
		for name, tins := range opt.TokenSet {
			ts[name] = strsToAny(tins)
		}
		out["tokenSet"] = ts
	}
	if len(opt.Error) > 0 {
		out["error"] = strMapToAny(opt.Error)
	}
	if len(opt.Hint) > 0 {
		out["hint"] = strMapToAny(opt.Hint)
	}
	if o := opt.Safe; o != nil {
		m := map[string]any{}
		putBool(m, "key", o.Key)
		put(out, "safe", m)
	}

	if o := opt.Fixed; o != nil {
		m := map[string]any{}
		putBool(m, "lex", o.Lex)
		if o.Token != nil {
			tok := map[string]any{}
			for name, src := range o.Token {
				if src == nil {
					tok[name] = nil
				} else {
					tok[name] = *src
				}
			}
			m["token"] = tok
		}
		funcs = appendIfFunc(funcs, "fixed.check", o.Check)
		put(out, "fixed", m)
	}

	if o := opt.Match; o != nil {
		m := map[string]any{}
		putBool(m, "lex", o.Lex)
		if o.Token != nil {
			tok := map[string]any{}
			for name, re := range o.Token {
				tok[name] = regexHolder{
					re: re, eager: o.TokenEager != nil && o.TokenEager[name]}
			}
			m["token"] = tok
		}
		if len(o.TokenOrder) > 0 {
			m["tokenOrder"] = strsToAny(o.TokenOrder)
		}
		funcs = appendIfFunc(funcs, "match.check", o.Check)
		if len(o.TokenFn) > 0 {
			funcs = append(funcs, "match.tokenFn")
		}
		if len(o.Value) > 0 {
			funcs = append(funcs, "match.value")
		}
		put(out, "match", m)
	}

	if o := opt.Rule; o != nil {
		m := map[string]any{}
		putStr(m, "start", o.Start)
		putBool(m, "finish", o.Finish)
		putStr(m, "include", o.Include)
		putStr(m, "exclude", o.Exclude)
		if o.MaxMul != nil {
			m["maxmul"] = *o.MaxMul
		}
		put(out, "rule", m)
	}

	if o := opt.Space; o != nil {
		m := map[string]any{}
		putBool(m, "lex", o.Lex)
		putStr(m, "chars", o.Chars)
		funcs = appendIfFunc(funcs, "space.check", o.Check)
		put(out, "space", m)
	}

	if o := opt.Line; o != nil {
		m := map[string]any{}
		putBool(m, "lex", o.Lex)
		putStr(m, "chars", o.Chars)
		putStr(m, "rowChars", o.RowChars)
		putBool(m, "single", o.Single)
		funcs = appendIfFunc(funcs, "line.check", o.Check)
		put(out, "line", m)
	}

	if o := opt.Text; o != nil {
		m := map[string]any{}
		putBool(m, "lex", o.Lex)
		funcs = appendIfFunc(funcs, "text.check", o.Check)
		if len(o.Modify) > 0 {
			funcs = append(funcs, "text.modify")
		}
		put(out, "text", m)
	}

	if o := opt.Number; o != nil {
		m := map[string]any{}
		putBool(m, "lex", o.Lex)
		putBool(m, "hex", o.Hex)
		putBool(m, "oct", o.Oct)
		putBool(m, "bin", o.Bin)
		putStr(m, "sep", o.Sep)
		funcs = appendIfFunc(funcs, "number.check", o.Check)
		funcs = appendIfFunc(funcs, "number.exclude", o.Exclude)
		put(out, "number", m)
	}

	if o := opt.Comment; o != nil {
		m := map[string]any{}
		putBool(m, "lex", o.Lex)
		if o.Def != nil {
			def := map[string]any{}
			for name, d := range o.Def {
				if d == nil {
					def[name] = nil
					continue
				}
				dm := map[string]any{}
				putStr(dm, "start", d.Start)
				putStr(dm, "end", d.End)
				if d.Line {
					dm["line"] = true
				}
				def[name] = dm
			}
			m["def"] = def
		}
		funcs = appendIfFunc(funcs, "comment.check", o.Check)
		put(out, "comment", m)
	}

	if o := opt.String; o != nil {
		m := map[string]any{}
		putBool(m, "lex", o.Lex)
		putStr(m, "chars", o.Chars)
		putStr(m, "multiChars", o.MultiChars)
		putStr(m, "escapeChar", o.EscapeChar)
		putBool(m, "allowUnknown", o.AllowUnknown)
		putBool(m, "escapeStrict", o.EscapeStrict)
		putBool(m, "allowControl", o.AllowControl)
		putBool(m, "abandon", o.Abandon)
		if o.Escape != nil {
			esc := map[string]any{}
			for k, v := range o.Escape {
				esc[k] = v
			}
			m["escape"] = esc
		}
		if len(o.Replace) > 0 {
			rep := map[string]any{}
			for r, v := range o.Replace {
				rep[string(r)] = v
			}
			m["replace"] = rep
		}
		funcs = appendIfFunc(funcs, "string.check", o.Check)
		put(out, "string", m)
	}

	if o := opt.Value; o != nil {
		m := map[string]any{}
		putBool(m, "lex", o.Lex)
		if len(o.Def) > 0 {
			// A ValueDef carries the native value a keyword becomes,
			// which is data — but the engine's own defaults are
			// re-installed on load, and a front-end that needed custom
			// keywords would need them checked, not guessed.
			funcs = append(funcs, "value.def")
		}
		put(out, "value", m)
	}

	if o := opt.Lex; o != nil {
		m := map[string]any{}
		putBool(m, "empty", o.Empty)
		putBool(m, "relex", o.Relex)
		if o.EmptyResult != nil {
			m["emptyResult"] = o.EmptyResult
		}
		if len(o.Match) > 0 {
			funcs = append(funcs, "lex.match")
		}
		put(out, "lex", m)
	}

	if len(funcs) > 0 {
		sort.Strings(funcs)
		return nil, &CompileError{
			Message: diagName() + ": option(s) hold functions and cannot be " +
				"emitted as data: " + strings.Join(funcs, ", ") +
				". A serialized grammar that dropped them would accept a " +
				"different language than the one compiled.",
		}
	}
	return out, nil
}

// refuseUnhandled reports any Options field that is set but that this
// file does not know how to emit. Reflection rather than a checklist:
// a field added to the engine later shows up here as a failure instead
// of vanishing from every serialized grammar.
func refuseUnhandled(opt *tabnas.Options) error {
	v := reflect.ValueOf(*opt)
	t := v.Type()
	var unknown []string
	for i := 0; i < t.NumField(); i++ {
		name := t.Field(i).Name
		if optionsHandled[name] || v.Field(i).IsZero() {
			continue
		}
		unknown = append(unknown, name)
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return &CompileError{
		Message: fmt.Sprintf(
			"%s: option field(s) %s are set but are not serialisable as "+
				"grammar data; emitting the spec without them would change "+
				"the accepted language", diagName(), strings.Join(unknown, ", ")),
	}
}

func put(out map[string]any, key string, m map[string]any) {
	if len(m) > 0 {
		out[key] = m
	}
}

func putBool(m map[string]any, key string, v *bool) {
	if v != nil {
		m[key] = *v
	}
}

func putStr(m map[string]any, key, v string) {
	if v != "" {
		m[key] = v
	}
}

func strMapToAny(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func strsToAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// appendIfFunc records a field name when a function-valued option is
// set. reflect is used because the field types are distinct named func
// types with no common interface.
func appendIfFunc(dst []string, name string, fn any) []string {
	if fn == nil {
		return dst
	}
	if v := reflect.ValueOf(fn); v.Kind() == reflect.Func && !v.IsNil() {
		return append(dst, name)
	}
	return dst
}
