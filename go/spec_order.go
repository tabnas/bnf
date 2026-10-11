// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

package bnf

// spec_order.go — the order a compiled spec is written in.
//
// TypeScript writes a spec in the order its emitter makes it: the rules in
// the order they are committed, each token in the order it is allocated,
// each alternate's keys in the order they are set. Part of that order is
// meaning, not layout. The rules' order is the grammar's, and it ranks the
// match tokens: the compiler allocates them as it walks the rules, an
// engine numbers them in the order the spec lists them, and of two tokens
// a place expects the lower number is tried first. A notation's render
// (abnf's, ebnf's and gbnf's, each an alchemy program) writes the rules
// back in the spec's order, so the text compiles back to the same ranks.
//
// A Go map keeps no order, so this port wrote every map of a spec in name
// order and gave the match tokens' order as a list of its own,
// `options.match.tokenOrder`. The renders refuse such a spec, since the
// rules' order is gone, and the Go spec matched neither of the other two
// runtimes', which are byte for byte alike. This file keeps the order
// instead: the emitter records it as it writes, and the serialisers walk
// it.
//
//   - The rules' order is the engine's own field, GrammarSpec.RuleOrder,
//     which the emitter fills in commit order; the engine installs the
//     rules in it, as it does a spec loaded from JSON.
//   - The match tokens' order is MatchOptions.TokenOrder, which the
//     emitter has always filled.
//   - The fixed tokens', the token sets' and each alternate's key order
//     have no field in the engine's spec, so the emitter records them per
//     spec here (specOrders), and the serialisers look them up.
//
// What a serialiser writes in that order is a *tabnas.OrderedMap, the
// engine's ordered object, which ToJsonic writes in key order and
// encoding/json writes in key order too.

import (
	"runtime"
	"sort"
	"strings"
	"sync"
	"unicode/utf16"
	"weak"

	tabnas "github.com/tabnas/parser/go"
)

// ---- an alternate's keys, as the emitter sets them --------------------

// altKeysKey is the entry, in the map the emitter builds an alternate in,
// that holds the order the alternate's keys were set in. TypeScript builds
// an alternate as an object and writes its keys in the order they were
// set; Go builds one as a map, which keeps none. The NUL that opens the
// name keeps it from ever being a key an alternate has, and mapToAlt,
// which reads the fields it knows by name, never copies it into the spec.
const altKeysKey = "\x00keys"

// altKeys is the order m's keys were set in, as setKey recorded it.
func altKeys(m map[string]any) []string {
	ks, _ := m[altKeysKey].([]string)
	return ks
}

// setKey sets one key of an alternate, as TypeScript's `o.k = v` does: a
// key already present keeps its place, and a new one goes last.
func setKey(m map[string]any, k string, v any) {
	if _, ok := m[k]; !ok {
		ks := altKeys(m)
		// A full slice expression, so the append copies: a copy of this
		// map (copyMap) may share the array, and must not see the key.
		m[altKeysKey] = append(ks[:len(ks):len(ks)], k)
	}
	m[k] = v
}

// delKey deletes one key, as `delete o.k` does: set again later, it goes
// last.
func delKey(m map[string]any, k string) {
	if _, ok := m[k]; !ok {
		return
	}
	delete(m, k)
	ks := altKeys(m)
	out := make([]string, 0, len(ks))
	for _, x := range ks {
		if x != k {
			out = append(out, x)
		}
	}
	m[altKeysKey] = out
}

// altOf builds an alternate, or a fragment of one, from key and value
// pairs in the order given: a TypeScript object literal.
func altOf(kv ...any) map[string]any {
	m := make(map[string]any, len(kv)/2+1)
	for i := 0; i+1 < len(kv); i += 2 {
		setKey(m, kv[i].(string), kv[i+1])
	}
	return m
}

// keyOrder is m's keys in the order they were set, followed by any set
// without setKey, in name order. The log entry is never one of them.
func keyOrder(m map[string]any) []string {
	ks := altKeys(m)
	out := make([]string, 0, len(m))
	seen := make(map[string]bool, len(m))
	for _, k := range ks {
		if _, ok := m[k]; ok && !seen[k] {
			out = append(out, k)
			seen[k] = true
		}
	}
	var rest []string
	for k := range m {
		if k != altKeysKey && !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// merge sets src's keys on dst, in the order they were set on src: a
// spread, `{...dst, ...src}`.
func merge(dst, src map[string]any) {
	for _, k := range keyOrder(src) {
		setKey(dst, k, src[k])
	}
}

// copyMap copies an alternate with its key order: `{...m}`.
func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	if ks := altKeys(m); ks != nil {
		out[altKeysKey] = append([]string(nil), ks...)
	}
	return out
}

// ---- what one emit records ----------------------------------------------

// emitTrace is what one emit records of the order it writes in, for the
// spec it returns. Package-level and set for the length of one emit, under
// emitMu, as diagPrefix is: the recording sites (commitRule, mapToAlt, the
// token allocation) are reached through twenty functions that have no
// other business with it.
type emitTrace struct {
	rules    []string
	ruleSeen map[string]bool
	fixed    []string
	sets     []string
	setSeen  map[string]bool
	alts     map[*tabnas.GrammarAltSpec][]string
	// Interned key orders: an emit makes many alternates and few orders.
	orders map[string][]string
}

// trace is the emit under way's, nil outside one.
var trace *emitTrace

func newEmitTrace() *emitTrace {
	return &emitTrace{
		ruleSeen: map[string]bool{},
		setSeen:  map[string]bool{},
		alts:     map[*tabnas.GrammarAltSpec][]string{},
		orders:   map[string][]string{},
	}
}

// noteRule records a rule name the first time the spec holds it, which is
// where an object keeps a key.
func (t *emitTrace) noteRule(name string) {
	if t == nil || t.ruleSeen[name] {
		return
	}
	t.ruleSeen[name] = true
	t.rules = append(t.rules, name)
}

// noteFixed records a fixed token as it is allocated, or removed.
func (t *emitTrace) noteFixed(name string) {
	if t != nil {
		t.fixed = append(t.fixed, name)
	}
}

// noteSet records a token set the first time it is written.
func (t *emitTrace) noteSet(name string) {
	if t == nil || t.setSeen[name] {
		return
	}
	t.setSeen[name] = true
	t.sets = append(t.sets, name)
}

// noteAlt records the order of an alternate's keys as mapToAlt converts it.
func (t *emitTrace) noteAlt(alt *tabnas.GrammarAltSpec, m map[string]any) {
	if t == nil {
		return
	}
	ks := make([]string, 0, len(m))
	for _, k := range keyOrder(m) {
		if altFieldOrder[k] {
			ks = append(ks, k)
		}
	}
	id := strings.Join(ks, ",")
	if shared, ok := t.orders[id]; ok {
		ks = shared
	} else {
		t.orders[id] = ks
	}
	t.alts[alt] = ks
}

// altFieldOrder holds the keys an alternate map can carry into the spec.
var altFieldOrder = map[string]bool{
	"s": true, "b": true, "p": true, "r": true, "a": true, "c": true,
	"k": true, "n": true, "u": true, "g": true, "m": true,
}

// finish hands what the emit recorded to the spec it built: the rules'
// order to its RuleOrder, and the rest to specOrders. Only the alternates
// the spec holds are kept; one converted and then replaced is not.
func (t *emitTrace) finish(spec *tabnas.GrammarSpec) {
	spec.RuleOrder = t.rules
	o := &specOrder{fixed: t.fixed, sets: t.sets, alts: map[*tabnas.GrammarAltSpec][]string{}}
	for _, rs := range spec.Rule {
		if rs == nil {
			continue
		}
		for _, list := range [][]*tabnas.GrammarAltSpec{altListOf(rs.Open), altListOf(rs.Close)} {
			for _, a := range list {
				if ks, ok := t.alts[a]; ok {
					o.alts[a] = ks
				}
			}
		}
	}
	rememberOrder(spec, o)
}

// ---- what a spec keeps ---------------------------------------------------

// specOrder is the order of what a spec holds that the engine's
// GrammarSpec has no field for: its fixed tokens, its token sets and each
// alternate's keys.
type specOrder struct {
	fixed []string
	sets  []string
	alts  map[*tabnas.GrammarAltSpec][]string
}

// specOrders holds each emitted spec's specOrder, for as long as the spec
// itself lives. The key is a weak pointer, so holding an entry does not
// keep its spec alive, and a cleanup removes the entry once the spec is
// collected. A spec built by hand, loaded from JSON or copied has none,
// and is written as it always was: what it records in its own fields in
// that order, and the rest in name order.
var specOrders sync.Map // weak.Pointer[tabnas.GrammarSpec] -> *specOrder

func rememberOrder(spec *tabnas.GrammarSpec, o *specOrder) {
	key := weak.Make(spec)
	specOrders.Store(key, o)
	runtime.AddCleanup(spec, func(k weak.Pointer[tabnas.GrammarSpec]) {
		specOrders.Delete(k)
	}, key)
}

// orderOf is the specOrder recorded for spec, or nil.
func orderOf(spec *tabnas.GrammarSpec) *specOrder {
	if spec == nil {
		return nil
	}
	if v, ok := specOrders.Load(weak.Make(spec)); ok {
		return v.(*specOrder)
	}
	return nil
}

// ---- writing in order ----------------------------------------------------

// inOrder lists names: those in first that are present, in that order,
// then the rest of them in name order. It is how a map is written when an
// order is known for some or all of its keys. A key set after the order
// was recorded comes after every recorded one, which is where TypeScript
// puts a key it sets late: an `a` that AttachActions or AttachActionSlots
// gives an alternate that had none goes last in both.
func inOrder(names []string, first []string) []string {
	present := make(map[string]bool, len(names))
	for _, n := range names {
		present[n] = true
	}
	out := make([]string, 0, len(names))
	used := make(map[string]bool, len(names))
	for _, n := range first {
		if present[n] && !used[n] {
			out = append(out, n)
			used[n] = true
		}
	}
	var rest []string
	for _, n := range names {
		if !used[n] {
			rest = append(rest, n)
			used[n] = true
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// jsLess orders two strings as JavaScript's default sort does, by UTF-16
// code unit, which is the order TypeScript writes provenance in. It
// differs from Go's byte order only between a character past U+FFFF and
// one from U+E000 to U+FFFF.
func jsLess(a, b string) bool {
	ua, ub := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(ua) && i < len(ub); i++ {
		if ua[i] != ub[i] {
			return ua[i] < ub[i]
		}
	}
	return len(ua) < len(ub)
}

// builtinConfigOrder is the order TypeScript writes the config of each
// engine builtin the emitter uses in, which is the order its emitter
// builds them in.
var builtinConfigOrder = map[string][]string{
	"node$":    {"init", "rule", "kind", "nterms"},
	"capture$": {"rule", "kind"},
	"fold$":    {"cN"},
	"key$":     {"lit"},
	"push$":    {"src"},
	"setval$":  {"src"},
}

// legacyAltOrder is the order an alternate is written in when the emitter
// recorded none: a spec this compiler did not emit.
var legacyAltOrder = []string{"s", "b", "c", "p", "r", "a", "k", "n", "u", "g"}
