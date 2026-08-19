// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

package bnf

// actions.go — user semantic actions (the `m`-mark feature). The Go
// port of the action-attachment half of ts/src/compile.ts.
//
// Action refs: `@<rule>:<phase>` (bo/ao/bc/ac) or `@<rule>:o|c:<mark>`.
// Values are AltAction closures run after the compiler's own action.
// Marks are stored on the typed alt's U["m$"] by the emitter.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	tabnas "github.com/tabnas/parser/go"
)

// ActionError is raised for a malformed or unresolvable action ref.
type ActionError struct{ Message string }

func (e *ActionError) Error() string { return e.Message }

// ActionFn is a user semantic action.
type ActionFn = tabnas.AltAction

// ActionsMap maps action refs to a function or list of functions.
type ActionsMap map[string][]ActionFn

var phaseSet = map[string]bool{"bo": true, "ao": true, "bc": true, "ac": true}

var actionRefRe = regexp.MustCompile(`^@([^:]+):(.+)$`)
var ocRe = regexp.MustCompile(`^([oc]):(.+)$`)

// seqActions collapses a function list into one action.
func seqActions(fns []ActionFn) tabnas.AltAction {
	if len(fns) == 1 {
		return fns[0]
	}
	return func(r *tabnas.Rule, ctx *tabnas.Context) {
		for _, fn := range fns {
			fn(r, ctx)
		}
	}
}

// composeActions wraps a previous state action with the user's.
func composeActions(prev any, fns []ActionFn) tabnas.StateAction {
	var prevFn tabnas.StateAction
	if p, ok := prev.(tabnas.StateAction); ok {
		prevFn = p
	}
	return func(r *tabnas.Rule, ctx *tabnas.Context) {
		if prevFn != nil {
			prevFn(r, ctx)
		}
		for _, fn := range fns {
			fn(r, ctx)
		}
	}
}

type targetResult struct {
	phase string
	alts  []*tabnas.GrammarAltSpec
	rule  string
}

func resolveTarget(spec *tabnas.GrammarSpec, key string) (*targetResult, error) {
	if strings.Contains(key, "$") {
		return nil, &ActionError{Message: fmt.Sprintf(
			diagName()+": '$' is reserved for engine builtins; user action ref '%s' may not contain '$'", key)}
	}
	m := actionRefRe.FindStringSubmatch(key)
	if m == nil {
		return nil, &ActionError{Message: fmt.Sprintf(
			diagName()+": malformed action ref '%s' (expected @rule:phase or @rule:o|c:mark)", key)}
	}
	rule := m[1]
	sel := m[2]
	rspec, ok := spec.Rule[rule]
	if !ok || rspec == nil {
		return nil, &ActionError{Message: fmt.Sprintf(
			diagName()+": action ref '%s' targets unknown rule '%s'", key, rule)}
	}
	if phaseSet[sel] {
		return &targetResult{phase: sel}, nil
	}
	pm := ocRe.FindStringSubmatch(sel)
	if pm == nil {
		return nil, &ActionError{Message: fmt.Sprintf(diagName()+": malformed action ref '%s'", key)}
	}
	phase := "close"
	var alts any
	if pm[1] == "o" {
		phase = "open"
		alts = rspec.Open
	} else {
		alts = rspec.Close
	}
	mark := pm[2]
	matched := []*tabnas.GrammarAltSpec{}
	for _, a := range altListOf(alts) {
		if a != nil && altMark(a) == mark {
			matched = append(matched, a)
		}
	}
	if len(matched) == 0 {
		return nil, &ActionError{Message: fmt.Sprintf(
			diagName()+": action ref '%s' matches no %s alt with mark '%s' in rule '%s'", key, phase, mark, rule)}
	}
	return &targetResult{alts: matched, rule: rule}, nil
}

func altListOf(field any) []*tabnas.GrammarAltSpec {
	if gas, ok := field.([]*tabnas.GrammarAltSpec); ok {
		return gas
	}
	if gls, ok := field.(*tabnas.GrammarAltListSpec); ok {
		return gls.Alts
	}
	return nil
}

func altMark(a *tabnas.GrammarAltSpec) string {
	if a.U == nil {
		return ""
	}
	m, _ := a.U["m$"].(string)
	return m
}

// AttachActions attaches user semantic actions to a spec in place.
func AttachActions(spec *tabnas.GrammarSpec, actions ActionsMap) error {
	if spec.Ref == nil {
		spec.Ref = map[tabnas.FuncRef]any{}
	}
	counter := 0
	// Deterministic ordering over the action keys.
	keys := make([]string, 0, len(actions))
	for k := range actions {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		fns := actions[key]
		target, err := resolveTarget(spec, key)
		if err != nil {
			return err
		}
		if target.phase != "" {
			m := actionRefRe.FindStringSubmatch(key)
			rule := m[1]
			fkey := tabnas.FuncRef(fmt.Sprintf("@%s-%s", rule, target.phase))
			spec.Ref[fkey] = composeActions(spec.Ref[fkey], fns)
			continue
		}
		for _, alt := range target.alts {
			userRef := tabnas.FuncRef(fmt.Sprintf("@abnf_user%d", counter))
			counter++
			spec.Ref[userRef] = seqActions(fns)
			alt.A = appendAction(alt.A, string(userRef))
		}
	}
	return nil
}

// AttachActionSlots declares user-action slots by name on a pure-data
// spec, without supplying functions.
func AttachActionSlots(spec *tabnas.GrammarSpec, refNames []string) error {
	for _, name := range refNames {
		target, err := resolveTarget(spec, name)
		if err != nil {
			return err
		}
		if target.phase != "" {
			return &ActionError{Message: fmt.Sprintf(
				diagName()+": slot '%s' is a rule-phase ref; slots are for @rule:o|c:mark alt actions", name)}
		}
		for _, alt := range target.alts {
			alt.A = appendAction(alt.A, name)
		}
	}
	return nil
}

func appendAction(existing any, added string) any {
	if existing == nil {
		return added
	}
	switch e := existing.(type) {
	case []any:
		return append(append([]any{}, e...), added)
	default:
		return []any{existing, added}
	}
}

// MarkListing returns a human-readable listing of compiler-assigned
// marks.
func MarkListing(spec *tabnas.GrammarSpec) string {
	lines := []string{}
	ruleNames := make([]string, 0, len(spec.Rule))
	for r := range spec.Rule {
		ruleNames = append(ruleNames, r)
	}
	sort.Strings(ruleNames)
	for _, rule := range ruleNames {
		rspec := spec.Rule[rule]
		// A REMOVAL, not a rule. `spec.Rule["B"] = nil` is how a spec says "B
		// is removed" (Grammar.Remove), and a removed rule has no alternates
		// to mark, so it contributes no lines.
		//
		// This guard is load-bearing rather than defensive: before removals
		// survived cloneGrammar the entry was dropped entirely and this loop
		// never saw one, so MarkListing quietly returned "" for a grammar
		// whose removals TS listed. Preserving the field is what makes the
		// nil reachable — the fix and the guard have to land together.
		if nil == rspec {
			continue
		}
		for _, ph := range []struct{ field, sym string }{{"open", "o"}, {"close", "c"}} {
			var list []*tabnas.GrammarAltSpec
			if ph.field == "open" {
				list = altListOf(rspec.Open)
			} else {
				list = altListOf(rspec.Close)
			}
			for _, a := range list {
				if a == nil {
					continue
				}
				mk := altMark(a)
				if mk == "" {
					continue
				}
				what := "(empty)"
				if s, ok := a.S.(string); ok && s != "" {
					what = "s:" + s
				} else if a.P != "" {
					what = "p:" + a.P
				}
				lines = append(lines, fmt.Sprintf("%s  %s:%s  %s", rule, ph.sym, mk, what))
			}
		}
	}
	return strings.Join(lines, "\n")
}
