// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

package bnf

// compile.go — compilation mode: turn ABNF source into a *pure-data*
// tabnas grammar and serialise it as jsonic text. The Go port of
// ts/src/compile.ts.
//
// A GrammarSpec emitted with builtins:true carries only string refs
// (`@…$` builtins) — no closures. ToRecognitionSpec drops the
// tree-building builtins (recognition is structural); ToPureSpec keeps
// them (still pure data). Both refuse a spec that still needs control
// closures.

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	tabnas "github.com/tabnas/parser/go"
)

// CompileError is raised when a grammar can't be compiled to a
// pure-data spec.
type CompileError struct {
	Message string
	Rules   []string
}

func (e *CompileError) Error() string { return e.Message }

// Ref fields whose string value is an AST-building action dropped by
// recognition mode.
var refFields = map[string]bool{"a": true, "bo": true, "bc": true}

// Tree-building builtins dropped by recognition mode.
var treeBuiltins = map[string]bool{"@node$": true, "@capture$": true, "@bubble$": true}
var treeConfigKeys = []string{"node$", "capture$"}

// CompileOptions controls compilation. Mirrors CompileOptions.
type CompileOptions struct {
	Start       string
	Tag         string
	Strict      bool
	Indent      int
	Recognition *bool // default true
}

// specToData converts a typed *GrammarSpec (built with builtins:true,
// so all action/cond fields are `@…$` strings) into a generic nested
// data tree: map[string]any / []any / scalars / *regexp.Regexp. This is
// the canonical form the clone/serialise functions operate on.
//
// Returns the offending rule names if any control closures remain (i.e.
// a probe dispatcher compiled without builtins) — those have non-string
// A/C fields.
func specToData(spec *tabnas.GrammarSpec) (map[string]any, []string, error) {
	offenders := map[string]bool{}

	// The options block carries the WHOLE of the front-end's typed
	// Options, not just the three keys the converter sets itself. See
	// options_data.go: a front-end's lexing configuration IS part of the
	// accepted language, and a spec emitted without it produced a
	// grammar that parsed differently from a natively installed one.
	options, err := optionsToData(spec.Options)
	if err != nil {
		return nil, nil, err
	}
	// Map-form options fill in under the typed ones, so a spec carrying
	// both keeps the typed side authoritative.
	for k, v := range spec.OptionsMap {
		if _, taken := options[k]; !taken {
			options[k] = v
		}
	}

	rules := map[string]any{}
	for name, rspec := range spec.Rule {
		rm := map[string]any{}
		if rspec.Open != nil {
			rm["open"] = altsToData(rspec.Open, name, offenders)
		}
		if rspec.Close != nil {
			rm["close"] = altsToData(rspec.Close, name, offenders)
		}
		rules[name] = rm
	}

	data := map[string]any{"options": options, "rule": rules}
	if len(offenders) > 0 {
		off := make([]string, 0, len(offenders))
		for r := range offenders {
			off = append(off, r)
		}
		sort.Strings(off)
		return data, off, nil
	}
	return data, nil, nil
}

// regexHolder wraps a regexp with its eager flag for serialisation.
type regexHolder struct {
	re    *regexp.Regexp
	eager bool
}

// SpecToData converts a *GrammarSpec into a plain data tree
// (map/slice/scalar/regexHolder). Action/condition refs are emitted as
// their `@`-name strings. Closures in spec.Ref are dropped — like the TS
// CLI's JSON output, which serialises actions as FuncRef strings. Used by
// the CLI's default (spec-dump) mode.
func SpecToData(spec *tabnas.GrammarSpec) map[string]any {
	data, _, _ := specToData(spec)
	if spec.Ref != nil && len(spec.Ref) > 0 {
		// List the ref names (closures can't serialise) for parity with
		// the TS shape where `ref` maps names to functions.
		refs := map[string]any{}
		for name := range spec.Ref {
			refs[name] = "@fn"
		}
		data["ref"] = refs
	}
	return data
}

// SpecToJSON renders a spec as JSON text (the CLI default output).
func SpecToJSON(spec *tabnas.GrammarSpec, indent int) string {
	return ToJsonic(SpecToData(spec), true, indent)
}

func altsToData(alts any, rule string, offenders map[string]bool) []any {
	gas, ok := alts.([]*tabnas.GrammarAltSpec)
	if !ok {
		return []any{}
	}
	out := make([]any, 0, len(gas))
	for _, ga := range gas {
		out = append(out, altToData(ga, rule, offenders))
	}
	return out
}

func altToData(ga *tabnas.GrammarAltSpec, rule string, offenders map[string]bool) map[string]any {
	m := map[string]any{}
	if ga.S != nil {
		m["s"] = ga.S
	}
	if ga.B != nil {
		switch n := ga.B.(type) {
		case int:
			m["b"] = n
		case float64:
			m["b"] = int(n)
		}
	}
	if ga.P != "" {
		m["p"] = ga.P
	}
	if ga.R != "" {
		m["r"] = ga.R
	}
	if ga.A != nil {
		// In builtins mode A is a string ref (or []string). Non-string =
		// a closure offender.
		switch a := ga.A.(type) {
		case string:
			m["a"] = a
		case []any:
			m["a"] = a
		case []string:
			arr := make([]any, len(a))
			for i, s := range a {
				arr[i] = s
			}
			m["a"] = arr
		default:
			offenders[rule] = true
		}
	}
	if ga.C != nil {
		switch c := ga.C.(type) {
		case string:
			// A non-ref-field string ref signals a control function.
			m["c"] = c
		case map[string]any:
			m["c"] = c
		default:
			offenders[rule] = true
		}
	}
	if ga.K != nil {
		m["k"] = copyAnyMap(ga.K)
	}
	if ga.N != nil {
		nm := map[string]any{}
		for k, v := range ga.N {
			nm[k] = v
		}
		m["n"] = nm
	}
	if ga.U != nil {
		// Recover the mark stashed under "m$" as a top-level "m".
		um := copyAnyMap(ga.U)
		if mv, ok := um["m$"]; ok {
			m["m"] = mv
			delete(um, "m$")
		}
		if len(um) > 0 {
			m["u"] = um
		}
	}
	if ga.G != "" {
		m["g"] = ga.G
	}
	return m
}

func copyAnyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// controlRefRules finds rules that reference the ref map (closures in
// spec.Ref) from a field *other than* the droppable AST hooks (a/bo/bc)
// — i.e. control functions (probe `c:` guards and dispatch actions).
// Their presence means the grammar can't be represented purely
// structurally. In builtins mode the probe phase guards are
// `@probePhase0$` etc. — engine builtins, NOT refs into spec.Ref — and
// so are not offenders. Mirrors the TS controlRefRules (compile.ts).
func controlRefRules(rules map[string]any, isRef func(string) bool) []string {
	offenders := map[string]bool{}
	var scan func(v any, rule string)
	scan = func(v any, rule string) {
		switch x := v.(type) {
		case []any:
			for _, e := range x {
				scan(e, rule)
			}
		case map[string]any:
			for k, val := range x {
				if s, ok := val.(string); ok && !refFields[k] && isRef(s) {
					offenders[rule] = true
				} else {
					scan(val, rule)
				}
			}
		}
	}
	for name, rd := range rules {
		scan(rd, name)
	}
	out := make([]string, 0, len(offenders))
	for r := range offenders {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// ToRecognitionSpec strips a converted spec down to a function-free
// recognition grammar: AST-building hooks (`a`/`bo`/`bc` refs into
// spec.Ref and the tree `$`-builtins, plus their `k.node$`/`k.capture$`
// config) are dropped, and the result carries `v` set to the engine's
// BUILTIN_SCHEMA_VERSION. It is the Go counterpart of the TS
// `toRecognitionSpec` export (ts/src/compile.ts); where TS returns a
// GrammarSpec object, Go returns the generic pure-data tree
// (map[string]any / []any / scalars — see SpecToData / ToJsonic).
//
// Grammars whose control logic is still closures (a probe dispatcher
// converted without `Builtins: true`) cannot be emitted as pure
// recognition data: those return a *CompileError listing the
// offending rules, matching the TS function's throw.
func ToRecognitionSpec(spec *tabnas.GrammarSpec) (map[string]any, error) {
	isRef := func(s string) bool {
		if spec.Ref == nil {
			return false
		}
		_, ok := spec.Ref[s]
		return ok
	}

	data, offenders, err := specToData(spec)
	if err != nil {
		return nil, err
	}
	if rules, ok := data["rule"].(map[string]any); ok {
		offenders = mergeSortedRules(offenders, controlRefRules(rules, isRef))
	}
	if len(offenders) > 0 {
		return nil, &CompileError{
			Message: diagName() + ": grammar needs control functions (probe / unbounded " +
				"lookahead) and cannot be emitted as a pure recognition grammar; " +
				"recompile with `builtins: true`. Offending rule(s): " +
				strings.Join(offenders, ", "),
			Rules: offenders,
		}
	}

	isDropped := func(s string) bool { return isRef(s) || treeBuiltins[s] }
	out := cloneRecognition(data, isDropped)
	out["v"] = tabnas.BUILTIN_SCHEMA_VERSION
	return out, nil
}

// mergeSortedRules unions two sorted offender lists, deduplicated.
func mergeSortedRules(a, b []string) []string {
	if len(b) == 0 {
		return a
	}
	seen := map[string]bool{}
	for _, r := range a {
		seen[r] = true
	}
	for _, r := range b {
		seen[r] = true
	}
	out := make([]string, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// ToPureSpec reduces a spec to a pure-data, function-free grammar that
// *keeps* the AST-building `$`-builtins (so the reloaded grammar still
// builds the full {rule, src, kids} tree), with `v` set to the engine's
// BUILTIN_SCHEMA_VERSION. It is the Go counterpart of the TS
// `toPureSpec` export (ts/src/compile.ts); where TS returns a
// GrammarSpec object, Go returns the generic pure-data tree
// (map[string]any / []any / scalars — see SpecToData / ToJsonic).
//
// Requires a `Builtins: true` conversion: if any closures remain in
// spec.Ref it returns a *CompileError, matching the TS throw.
func ToPureSpec(spec *tabnas.GrammarSpec) (map[string]any, error) {
	if len(spec.Ref) > 0 {
		keys := make([]string, 0, len(spec.Ref))
		for k := range spec.Ref {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if len(keys) > 3 {
			keys = keys[:3]
		}
		return nil, &CompileError{
			Message: diagName() + ": spec still contains closures; convert with `builtins: true` " +
				"for pure-data output. Stray ref(s): " + strings.Join(keys, ", "),
		}
	}
	data, offenders, err := specToData(spec)
	if err != nil {
		return nil, err
	}
	if len(offenders) > 0 {
		return nil, &CompileError{
			Message: diagName() + ": spec still contains closures; convert with `builtins: true` " +
				"for pure-data output.",
			Rules: offenders,
		}
	}
	out, _ := cloneData(data).(map[string]any)
	if out == nil {
		out = map[string]any{}
	}
	out["v"] = tabnas.BUILTIN_SCHEMA_VERSION
	return out, nil
}

// cloneData deep-copies, preserving regexHolder and dropping nothing
// (the data tree already has no functions).
func cloneData(v any) any {
	switch x := v.(type) {
	case regexHolder:
		return x
	case map[string]any:
		o := map[string]any{}
		for k, val := range x {
			o[k] = cloneData(val)
		}
		return mapOrSelf(o)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = cloneData(e)
		}
		return out
	default:
		return v
	}
}

func mapOrSelf(m map[string]any) map[string]any { return m }

// cloneRecognition drops AST-building hooks: a/bo/bc fields pointing at
// a dropped action (a spec.Ref closure or a tree builtin), and the
// now-orphaned k.node$/k.capture$ config.
func cloneRecognition(v any, isDropped func(string) bool) map[string]any {
	res := cloneRecognitionVal(v, isDropped)
	if m, ok := res.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func cloneRecognitionVal(v any, isDropped func(string) bool) any {
	switch x := v.(type) {
	case regexHolder:
		return x
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = cloneRecognitionVal(e, isDropped)
		}
		return out
	case map[string]any:
		o := map[string]any{}
		for k, val := range x {
			if refFields[k] {
				if s, ok := val.(string); ok && isDropped(s) {
					continue
				}
			}
			if k == "k" {
				if km, ok := val.(map[string]any); ok {
					kc := cloneRecognitionVal(km, isDropped).(map[string]any)
					for _, tk := range treeConfigKeys {
						delete(kc, tk)
					}
					if len(kc) == 0 {
						continue
					}
					o[k] = kc
					continue
				}
			}
			o[k] = cloneRecognitionVal(val, isDropped)
		}
		return o
	default:
		return v
	}
}

// ---- jsonic serialisation ------------------------------------------

var identRe = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)

// ToJsonic serialises a (function-free) data value as jsonic text.
func ToJsonic(value any, strict bool, indent int) string {
	if indent == 0 {
		indent = 2
	}
	sep := "\n"
	if strict {
		sep = ",\n"
	}
	pad := func(n int) string { return strings.Repeat(" ", indent*n) }

	quote := func(s, ch string) string {
		r := strings.ReplaceAll(s, "\\", "\\\\")
		r = strings.ReplaceAll(r, ch, "\\"+ch)
		r = strings.ReplaceAll(r, "\n", "\\n")
		return ch + r + ch
	}
	dq := func(s string) string { return quote(s, `"`) }
	str := func(s string) string {
		if strict {
			return dq(s)
		}
		return quote(s, "'")
	}
	key := func(k string) string {
		if !strict && identRe.MatchString(k) {
			return k
		}
		return dq(k)
	}

	var ser func(v any, depth int) string
	ser = func(v any, depth int) string {
		if v == nil {
			return "null"
		}
		switch x := v.(type) {
		case regexHolder:
			sentinel := "@/"
			if x.eager {
				sentinel = "@~/"
			}
			return str(sentinel + jsRegexSource(x.re) + "/" + jsRegexFlags(x.re))
		case bool:
			if x {
				return "true"
			}
			return "false"
		case int:
			return fmt.Sprintf("%d", x)
		case float64:
			return strconv.FormatFloat(x, 'g', -1, 64)
		case string:
			return str(x)
		case []any:
			if len(x) == 0 {
				return "[]"
			}
			items := make([]string, len(x))
			for i, e := range x {
				items[i] = pad(depth+1) + ser(e, depth+1)
			}
			return "[\n" + strings.Join(items, sep) + "\n" + pad(depth) + "]"
		case [][]int:
			// S field can arrive typed; convert to a string list form.
			return ser(tinMatrixToAny(x), depth)
		case map[string]any:
			keys := sortedMapKeys(x)
			if len(keys) == 0 {
				return "{}"
			}
			items := make([]string, len(keys))
			for i, k := range keys {
				items[i] = pad(depth+1) + key(k) + ": " + ser(x[k], depth+1)
			}
			return "{\n" + strings.Join(items, sep) + "\n" + pad(depth) + "}"
		}
		return "null"
	}

	return ser(value, 0)
}

func tinMatrixToAny(_ [][]int) any { return nil }

// sortedMapKeys returns map keys in a stable order. The converter emits
// alts as maps; jsonic key order only affects formatting, not meaning.
// Use the canonical field order where recognised, else alpha.
var fieldOrder = map[string]int{
	"options": 0, "rule": 1, "v": 2,
	"fixed": 0, "match": 1, "token": 0, "start": 0,
	"open": 0, "close": 1,
	"s": 0, "b": 1, "c": 2, "p": 3, "r": 4, "a": 5, "k": 6, "n": 7, "u": 8, "m": 9, "g": 10,
}

func sortedMapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		oi, iok := fieldOrder[keys[i]]
		oj, jok := fieldOrder[keys[j]]
		if iok && jok {
			if oi != oj {
				return oi < oj
			}
			return keys[i] < keys[j]
		}
		if iok != jok {
			return iok
		}
		return keys[i] < keys[j]
	})
	return keys
}

// jsRegexSource returns a JS-flavoured source for the Go regex (strips
// the leading (?i) flag group and anchor as needed). The converter's
// match-token regexes are `(?i)^…` or `^[\x{..}-\x{..}]`.
func jsRegexSource(re *regexp.Regexp) string {
	s := re.String()
	s = strings.TrimPrefix(s, "(?i)")
	// Keep the anchor; TS emitted `^` + pattern too.
	// Translate Go \x{HHHH} back to JS \uHHHH where 4 hex digits.
	s = goHexToJsUnicode(s)
	return s
}

func jsRegexFlags(re *regexp.Regexp) string {
	if strings.HasPrefix(re.String(), "(?i)") {
		return "i"
	}
	return ""
}

var goHexRe = regexp.MustCompile(`\\x\{([0-9a-fA-F]+)\}`)

func goHexToJsUnicode(s string) string {
	return goHexRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := goHexRe.FindStringSubmatch(m)
		h := sub[1]
		for len(h) < 4 {
			h = "0" + h
		}
		return "\\u" + h
	})
}
