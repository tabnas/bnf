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
	"encoding/json"
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

// Output-building actions dropped by recognition mode. The VALUE builders
// belong here for the same reason the tree builders do: a recognition-only
// grammar must recognise and build nothing. Leaving them in made
// ToRecognitionSpec return a spec that still constructed values.
//
// `@fold$`/`fold$` were missing here while TypeScript dropped them — a
// pre-existing divergence in this same list, found while adding the value
// builders, and repaired the same way: TypeScript defines the language.
var treeBuiltins = map[string]bool{
	"@node$": true, "@capture$": true, "@bubble$": true, "@fold$": true,
	"@object$": true, "@array$": true, "@reset$": true, "@key$": true,
	"@setval$": true, "@push$": true, "@value$": true,
}
var treeConfigKeys = []string{
	"node$", "capture$", "fold$",
	"object$", "array$", "key$", "setval$", "push$", "value$",
}

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
// data tree: a map[string]any at the root, *tabnas.OrderedMap for every
// object inside it, []any / scalars / regexHolder below. This is the
// canonical form the clone/serialise functions operate on.
//
// The objects are ordered as TypeScript writes them (spec_order.go): the
// rules in RuleOrder, the order the emitter committed them in, the match
// tokens in TokenOrder, and the fixed tokens, token sets and each
// alternate's keys in the order the emitter recorded for this spec. What
// has no recorded order is written in name order, as everything was.
//
// Returns the offending rule names if any control closures remain (i.e.
// a probe dispatcher compiled without builtins) — those have non-string
// A/C fields.
func specToData(spec *tabnas.GrammarSpec) (map[string]any, []string, error) {
	offenders := map[string]bool{}
	rec := recordOf(spec)

	// The options block carries the WHOLE of the front-end's typed
	// Options, not just the three keys the converter sets itself. See
	// options_data.go: a front-end's lexing configuration IS part of the
	// accepted language, and a spec emitted without it produced a
	// grammar that parsed differently from a natively installed one.
	options, err := optionsToData(spec.Options, rec)
	if err != nil {
		return nil, nil, err
	}
	// Map-form options fill in under the typed ones, so a spec carrying
	// both keeps the typed side authoritative.
	for _, k := range inOrder(mapKeys(spec.OptionsMap), nil) {
		if !options.Has(k) {
			options.Set(k, spec.OptionsMap[k])
		}
	}

	rules := tabnas.NewOrderedMap()
	for _, name := range inOrder(mapKeys(spec.Rule), spec.RuleOrder) {
		rspec := spec.Rule[name]
		// A nil entry is the engine's documented representation of a REMOVAL —
		// grammarspec.go: "a nil entry removes that rule" — so it must survive
		// serialisation AS one. TypeScript carries it through as null (its
		// cloneData never dereferences the entry), and dereferencing it here
		// panicked every serialisation entry point for any grammar that
		// carried a removal: SpecToDataErr, ToRecognitionSpec and ToPureSpec
		// alike.
		if nil == rspec {
			rules.Set(name, nil)
			continue
		}
		rm := tabnas.NewOrderedMap()
		if rspec.Open != nil {
			rm.Set("open", altsToData(rspec.Open, name, offenders, rec))
		}
		if rspec.Close != nil {
			rm.Set("close", altsToData(rspec.Close, name, offenders, rec))
		}
		rules.Set(name, rm)
	}

	data := map[string]any{"options": options, "rule": rules}
	carryMeta(spec, data)
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

// carryMeta carries the engine-ignored `meta` block across a spec
// transform.
//
// Every transform here rebuilds the spec from `{options, rule}` alone,
// which is the right default — everything else on a converted spec is
// either closures or compiler-internal. `meta` is the exception: it is
// pure data that describes the grammar rather than defining it, and the
// consumer that needs it most (a language server resolving a generated
// rule name back to the author's rule) loads exactly these compiled
// specs. Dropping it would mean provenance survived only in the
// in-memory spec, which no tool ever sees.
//
// Done once here, in specToData, rather than at each exported transform:
// ToRecognitionSpec, ToPureSpec and SpecToData all funnel through it, so
// this is the single point every serialised shape passes. Mirrors the TS
// `carryMeta`, which the two TS transforms call individually because
// they do not share a builder.
func carryMeta(from *tabnas.GrammarSpec, to map[string]any) {
	if from == nil || from.Meta == nil {
		return
	}
	// Normalise through JSON before cloning. Meta is JSON-serialisable
	// BY CONTRACT, but a caller can perfectly reasonably hold ordinary
	// typed Go containers in it — `map[string]string`, `[]string` — and
	// neither cloneData nor ToJsonic recognises those: they handle only
	// the generic `map[string]any` / `[]any` forms and everything else
	// falls through to `null`. The metadata would then be silently
	// replaced by nothing on the way out, which is the worst of the
	// available outcomes. A round-trip converts every JSON-compatible
	// value to the generic forms.
	//
	// A value that cannot be marshalled at all is dropped rather than
	// written out: a missing key is honest, a null one is not.
	raw, err := json.Marshal(from.Meta)
	if err != nil {
		return
	}
	var norm map[string]any
	if err := json.Unmarshal(raw, &norm); err != nil {
		return
	}
	// Ordered as TypeScript writes it: the provenance the emitter makes
	// first, its names sorted as JavaScript sorts them (the emitter's own
	// order), then anything else a caller keeps in meta, in name order.
	meta := tabnas.NewOrderedMap()
	for _, k := range inOrder(mapKeys(norm), []string{"provenance"}) {
		v := cloneData(norm[k])
		if pm, ok := norm[k].(map[string]any); ok && k == "provenance" {
			names := mapKeys(pm)
			sort.Slice(names, func(i, j int) bool { return jsLess(names[i], names[j]) })
			prov := tabnas.NewOrderedMap()
			for _, n := range names {
				prov.Set(n, cloneData(pm[n]))
			}
			v = prov
		}
		meta.Set(k, v)
	}
	to["meta"] = meta
}

// regexHolder wraps a match token's regexp for serialisation, with its
// eager flag and, where the emitter recorded it, the matcher as the
// canonical compiler writes it.
type regexHolder struct {
	re    *regexp.Regexp
	eager bool
	js    *jsRegex
}

// jsRegex is a match token's matcher as the canonical compiler writes it,
// in JavaScript's dialect: its source, and its flags in the order
// JavaScript reports them. RE2 compiles a class from its own spelling of
// a code point (`\x{1F600}`) and keeps no flag but `i`, so the compiled
// regexp gives back neither. Dropping `u` changes what JavaScript matches:
// without it a negated class takes half of an astral character, and a
// braced escape is not an escape at all.
type jsRegex struct {
	// re is the compiled matcher the record describes. A matcher put in
	// its place after the emit is written from its own source instead.
	re *regexp.Regexp
	// source is the whole source TypeScript writes, `^` included, or ""
	// when it is the compiled one's, respelled (jsRegexSource).
	source string
	flags  string
}

// SpecToData converts a *GrammarSpec into a plain data tree: a
// map[string]any at the root, *tabnas.OrderedMap for every object inside
// it (in the order TypeScript writes it; see spec_order.go), and
// []any / scalars / regexHolder below. Action/condition refs are emitted as
// their `@`-name strings. Closures in spec.Ref are dropped — like the TS
// CLI's JSON output, which serialises actions as FuncRef strings. Used by
// the CLI's default (spec-dump) mode.
func SpecToData(spec *tabnas.GrammarSpec) map[string]any {
	data, _ := SpecToDataErr(spec)
	return data
}

// SpecToDataErr is SpecToData with the failure surfaced.
//
// specToData can now refuse a spec outright — an option holding a
// function cannot be emitted as data — and a dump API that swallowed
// that returned an EMPTY spec while reporting success, which is the
// silent-wrong-grammar failure this package exists to avoid. It also
// panicked outright on `data["ref"]` when the nil map met a spec with
// refs.
//
// The signatures of SpecToData / SpecToJSON are kept so callers do not
// break; on failure they now yield nil / "" rather than a plausible
// lie, and this function is how a caller learns why.
func SpecToDataErr(spec *tabnas.GrammarSpec) (map[string]any, error) {
	data, _, err := specToData(spec)
	if err != nil {
		return nil, err
	}
	if spec.Ref != nil && len(spec.Ref) > 0 {
		// List the ref names (closures can't serialise) for parity with
		// the TS shape where `ref` maps names to functions.
		refs := map[string]any{}
		for name := range spec.Ref {
			refs[name] = "@fn"
		}
		data["ref"] = refs
	}
	return data, nil
}

// SpecToJSON renders a spec as JSON text (the CLI default output).
// Returns "" for a spec that cannot be represented; use SpecToJSONErr
// to learn why rather than emitting an empty grammar as if it were one.
func SpecToJSON(spec *tabnas.GrammarSpec, indent int) string {
	out, _ := SpecToJSONErr(spec, indent)
	return out
}

// SpecToJSONErr is SpecToJSON with the failure surfaced.
func SpecToJSONErr(spec *tabnas.GrammarSpec, indent int) (string, error) {
	data, err := SpecToDataErr(spec)
	if err != nil {
		return "", err
	}
	return ToJsonic(data, true, indent), nil
}

func altsToData(alts any, rule string, offenders map[string]bool, rec *specRecord) []any {
	gas, ok := alts.([]*tabnas.GrammarAltSpec)
	if !ok {
		return []any{}
	}
	out := make([]any, 0, len(gas))
	for _, ga := range gas {
		var keys []string
		if rec != nil {
			keys = rec.alts[ga]
		}
		out = append(out, altToData(ga, rule, offenders, keys))
	}
	return out
}

// altToData writes one alternate, its keys in the order given (the order
// the emitter set them in), and any it does not name in the order an
// alternate was always written in.
func altToData(ga *tabnas.GrammarAltSpec, rule string, offenders map[string]bool, keys []string) *tabnas.OrderedMap {
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
			m["c"] = orderedCopy(c, nil)
		default:
			offenders[rule] = true
		}
	}
	if ga.K != nil {
		k := tabnas.NewOrderedMap()
		for _, name := range inOrder(mapKeys(ga.K), nil) {
			v := ga.K[name]
			// An engine builtin's config, in the order TypeScript's
			// emitter builds it in.
			if cfg, ok := v.(map[string]any); ok {
				if order, known := builtinConfigOrder[name]; known {
					v = orderedCopy(cfg, order)
				}
			}
			k.Set(name, v)
		}
		m["k"] = k
	}
	if ga.N != nil {
		nm := tabnas.NewOrderedMap()
		for _, k := range inOrder(mapKeys(ga.N), nil) {
			nm.Set(k, ga.N[k])
		}
		m["n"] = nm
	}
	if ga.U != nil {
		// Drop the mark. The emitter stashes it under "m$" so
		// attachActions and markListing can find it on the in-memory
		// spec, but it is compiler-internal: the engine's alt contract
		// has no `m`, its loader would discard one, and the grammar JSON
		// Schema sets additionalProperties:false over the twelve keys it
		// defines -- so emitting it makes the whole grammar FAIL
		// `tabnas validate`, which is the first thing the docs tell an
		// author to run. It used to be promoted to a top-level "m" here.
		um := copyAnyMap(ga.U)
		delete(um, "m$")
		if len(um) > 0 {
			m["u"] = orderedCopy(um, nil)
		}
	}
	if ga.G != "" {
		m["g"] = ga.G
	}
	out := tabnas.NewOrderedMap()
	first := keys
	if first == nil {
		first = legacyAltOrder
	}
	for _, k := range inOrder(mapKeys(m), first) {
		out.Set(k, m[k])
	}
	return out
}

// orderedCopy copies a map as an ordered object, the keys in first first,
// then the rest in name order. The values are shared.
func orderedCopy(m map[string]any, first []string) *tabnas.OrderedMap {
	out := tabnas.NewOrderedMap()
	for _, k := range inOrder(mapKeys(m), first) {
		out.Set(k, m[k])
	}
	return out
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
func controlRefRules(rules any, isRef func(string) bool) []string {
	offenders := map[string]bool{}
	var scan func(v any, rule string)
	field := func(k string, val any, rule string) {
		if s, ok := val.(string); ok && !refFields[k] && isRef(s) {
			offenders[rule] = true
		} else {
			scan(val, rule)
		}
	}
	scan = func(v any, rule string) {
		switch x := v.(type) {
		case []any:
			for _, e := range x {
				scan(e, rule)
			}
		case map[string]any:
			for k, val := range x {
				field(k, val, rule)
			}
		case *tabnas.OrderedMap:
			for _, k := range x.Keys {
				field(k, x.Vals[k], rule)
			}
		}
	}
	switch x := rules.(type) {
	case map[string]any:
		for name, rd := range x {
			scan(rd, name)
		}
	case *tabnas.OrderedMap:
		for _, name := range x.Keys {
			scan(x.Vals[name], name)
		}
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
// (a map[string]any root whose objects are *tabnas.OrderedMap, in
// TypeScript's order; []any / scalars — see SpecToData / ToJsonic).
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
	offenders = mergeSortedRules(offenders, controlRefRules(data["rule"], isRef))
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
// (a map[string]any root whose objects are *tabnas.OrderedMap, in
// TypeScript's order; []any / scalars — see SpecToData / ToJsonic).
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
	case *tabnas.OrderedMap:
		o := tabnas.NewOrderedMap()
		for _, k := range x.Keys {
			o.Set(k, cloneData(x.Vals[k]))
		}
		return o
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
			if kept, keep := cloneRecognitionField(k, val, isDropped); keep {
				o[k] = kept
			}
		}
		return o
	case *tabnas.OrderedMap:
		o := tabnas.NewOrderedMap()
		for _, k := range x.Keys {
			if kept, keep := cloneRecognitionField(k, x.Vals[k], isDropped); keep {
				o.Set(k, kept)
			}
		}
		return o
	default:
		return v
	}
}

// cloneRecognitionField is one key of an object as recognition keeps it,
// and whether it keeps the key at all.
func cloneRecognitionField(k string, val any, isDropped func(string) bool) (any, bool) {
	if refFields[k] {
		if s, ok := val.(string); ok && isDropped(s) {
			return nil, false
		}
		// Array-`a` composition: filter the dropped actions out of
		// the list rather than keeping the whole list because it is
		// not a string. Only checking the scalar case let every
		// composed action through — the value builders are emitted
		// that way (`a: ["@object$","@key$"]`), so a
		// "recognition-only" spec still built values.
		if list, ok := val.([]any); ok {
			kept := []any{}
			for _, e := range list {
				if es, isStr := e.(string); isStr && isDropped(es) {
					continue
				}
				kept = append(kept, e)
			}
			if len(kept) == 0 {
				return nil, false
			}
			if len(kept) == 1 {
				return kept[0], true
			}
			return kept, true
		}
	}
	if k == "k" {
		var kc any
		switch km := val.(type) {
		case map[string]any:
			c := cloneRecognitionVal(km, isDropped).(map[string]any)
			for _, tk := range treeConfigKeys {
				delete(c, tk)
			}
			if len(c) == 0 {
				return nil, false
			}
			kc = c
		case *tabnas.OrderedMap:
			c := cloneRecognitionVal(km, isDropped).(*tabnas.OrderedMap)
			for _, tk := range treeConfigKeys {
				c.Delete(tk)
			}
			if c.Len() == 0 {
				return nil, false
			}
			kc = c
		}
		if kc != nil {
			return kc, true
		}
	}
	return cloneRecognitionVal(val, isDropped), true
}

// ---- jsonic serialisation ------------------------------------------

var identRe = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)

// ToJsonic serialises a (function-free) data value as jsonic text. A
// *tabnas.OrderedMap is written in its own key order, which for a tree
// the serialisers here build is the order TypeScript writes the spec in,
// so the text is TypeScript's; a map[string]any has no order, and is
// written in a canonical one (sortedMapKeys).
func ToJsonic(value any, strict bool, indent int) string {
	if indent == 0 {
		indent = 2
	}
	sep := "\n"
	if strict {
		sep = ",\n"
	}
	pad := func(n int) string { return strings.Repeat(" ", indent*n) }

	// Every C0 control character is escaped, not just newline: JSON
	// forbids all of them raw inside a string, so a spec whose options
	// carried a tab (a plausible space.chars) serialised to text that
	// would not parse back. The TS serialiser escapes the whole range;
	// this now matches it.
	quote := func(s, ch string) string {
		var b strings.Builder
		b.WriteString(ch)
		for _, r := range s {
			switch {
			case string(r) == ch:
				b.WriteString("\\" + ch)
			case r == '\\':
				b.WriteString(`\\`)
			case r == '\n':
				b.WriteString(`\n`)
			case r == '\r':
				b.WriteString(`\r`)
			case r == '\t':
				b.WriteString(`\t`)
			case r == '\b':
				b.WriteString(`\b`)
			case r == '\f':
				b.WriteString(`\f`)
			case r < 0x20:
				fmt.Fprintf(&b, `\u%04x`, r)
			default:
				b.WriteRune(r)
			}
		}
		b.WriteString(ch)
		return b.String()
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
			source, flags := jsMatcher(x)
			return str(sentinel + source + "/" + flags)
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
		case *tabnas.OrderedMap:
			// Written in its own order: the order TypeScript writes the
			// object in, which the serialisers built it in.
			if len(x.Keys) == 0 {
				return "{}"
			}
			items := make([]string, len(x.Keys))
			for i, k := range x.Keys {
				items[i] = pad(depth+1) + key(k) + ": " + ser(x.Vals[k], depth+1)
			}
			return "{\n" + strings.Join(items, sep) + "\n" + pad(depth) + "}"
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

// sortedMapKeys returns a plain map's keys in a stable order: the
// canonical field order where recognised, else alpha. The objects the
// serialisers here build are ordered (*tabnas.OrderedMap); a plain map
// is what they carry over as it came (map-form options, meta beyond its
// provenance, a builtin's config they do not know) or what a caller
// hands ToJsonic.
var fieldOrder = map[string]int{
	"options": 0, "rule": 1, "v": 2,
	"fixed": 0, "match": 1, "token": 0, "start": 0,
	"open": 0, "close": 1,
	"s": 0, "b": 1, "c": 2, "p": 3, "r": 4, "a": 5, "k": 6, "n": 7, "u": 8, "g": 9,
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

// jsMatcher is the source and the flags a match token is written with:
// what the emitter recorded, or else the compiled regexp's own, respelled.
// A braced escape is read as one only under `u`, so a respelled source
// that has one carries the flag whatever the regexp was compiled with.
func jsMatcher(x regexHolder) (string, string) {
	if x.js != nil && x.js.re == x.re {
		source, flags := x.js.source, x.js.flags
		if source == "" {
			var braced bool
			source, braced = jsRegexSource(x.re)
			if braced && !strings.ContainsAny(flags, "uv") {
				flags = canonicalJSFlags(flags + "u")
			}
		}
		return source, flags
	}
	source, braced := jsRegexSource(x.re)
	flags := ""
	if strings.HasPrefix(x.re.String(), "(?i)") {
		flags = "i"
	}
	if braced {
		flags = canonicalJSFlags(flags + "u")
	}
	return source, flags
}

// jsRegexSource is a compiled regexp's source as JavaScript reads it:
// without the `(?i)` group the `i` flag stands for, and with each of RE2's
// code point escapes, `\x{…}`, spelled as JavaScript spells it. A source
// that names a code point past U+FFFF spells every one in braces,
// `\u{1F600}`, which JavaScript reads only under `u`, and reports that it
// did; any other is `\uHHHH`, read alike with `u` and without. `\uHHHH`
// cannot reach past U+FFFF: JavaScript reads `\u1F600` as U+1F60 and a
// `0`. Switching a whole source to braces is how the canonical compiler's
// class atoms are spelled, and the front-ends' classes but GBNF's, which
// switch a code point at a time.
func jsRegexSource(re *regexp.Regexp) (string, bool) {
	return jsCodePoints(strings.TrimPrefix(re.String(), "(?i)"))
}

// jsCodePoints respells the `\x{…}` escapes of an RE2 source as
// JavaScript's, as jsRegexSource describes, and reports whether it wrote
// them in braces. Any other escape is copied as it is, the escaped
// character with it, so `\\x{41}` stays a backslash and `x{41}`.
func jsCodePoints(s string) (string, bool) {
	type escape struct {
		start, end int
		hex        string
	}
	var escapes []escape
	astral := false
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			continue
		}
		if s[i+1] == 'x' && i+2 < len(s) && s[i+2] == '{' {
			if j := strings.IndexByte(s[i+3:], '}'); j > 0 && isHexDigits(s[i+3:i+3+j]) {
				hex := strings.TrimLeft(s[i+3:i+3+j], "0")
				if hex == "" {
					hex = "0"
				}
				if len(hex) > 4 {
					astral = true
				}
				escapes = append(escapes, escape{i, i + 4 + j, hex})
				i += 3 + j
				continue
			}
		}
		i++
	}
	if len(escapes) == 0 {
		return s, false
	}
	var b strings.Builder
	last := 0
	for _, e := range escapes {
		b.WriteString(s[last:e.start])
		if astral {
			b.WriteString(`\u{` + e.hex + `}`)
		} else {
			b.WriteString(`\u` + strings.Repeat("0", 4-len(e.hex)) + e.hex)
		}
		last = e.end
	}
	b.WriteString(s[last:])
	return b.String(), astral
}

func isHexDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
			return false
		}
	}
	return s != ""
}

// jsFlagOrder is the order JavaScript reports a RegExp's flags in, which
// is the order the canonical compiler writes them in.
const jsFlagOrder = "dgimsuvy"

// canonicalJSFlags is flags in JavaScript's order, each once. A letter
// that is not a JavaScript flag is left out, as it always was here; the
// canonical compiler refuses the IR that carries one.
func canonicalJSFlags(flags string) string {
	var b []byte
	for i := 0; i < len(jsFlagOrder); i++ {
		if strings.IndexByte(flags, jsFlagOrder[i]) >= 0 {
			b = append(b, jsFlagOrder[i])
		}
	}
	return string(b)
}
