// Copyright (c) 2026 tabnas, MIT License

// rules.go — the schema grammar-rules: a grammar's rules as its text
// writes them, uncompiled and in source order, which the ABNF, EBNF and
// GBNF front ends read their documents as and write them from (alchemy's
// docs/streaming-plan.md, section 13.1; docs/grammar-rules.md here).
//
// Go port of ts/src/rules.ts. A tree is held in order where its objects
// are *tabnas.OrderedMap, as a front end builds them and as
// OrderedMap.UnmarshalJSON reads one; a plain map[string]any has no order
// to check, so only its members and their values are.

package bnf

import (
	"fmt"
	"math"
	"sort"
	"strconv"

	tabnas "github.com/tabnas/parser/go"
)

// MaxCodePoint is the largest code point.
const MaxCodePoint = 0x10ffff

// RulesError is a tree the schema refuses: Code names what was wrong
// (rules_root, rules_type, rules_member, rules_value) and Path where.
type RulesError struct {
	Code    string
	Path    string
	Message string
}

func (e *RulesError) Error() string {
	return "grammar-rules: " + e.Path + ": " + e.Message
}

func rulesFail(code, path, message string) *RulesError {
	return &RulesError{Code: code, Path: path, Message: message}
}

// rulesObject is an object's members in order, or false where v is none.
// A plain map has no order, so its keys are taken in the order the
// schema lists them, the others after, sorted: its members and their
// values are held, and their order is not.
func rulesObject(v any, list []string) ([]string, map[string]any, bool) {
	switch o := v.(type) {
	case *tabnas.OrderedMap:
		return o.Keys, o.Vals, true
	case map[string]any:
		keys := make([]string, 0, len(o))
		for _, name := range list {
			if _, has := o[name]; has {
				keys = append(keys, name)
			}
		}
		var rest []string
		for k := range o {
			if !contains(list, k) {
				rest = append(rest, k)
			}
		}
		sort.Strings(rest)
		return append(keys, rest...), o, true
	}
	return nil, nil, false
}

func contains(list []string, name string) bool {
	for _, n := range list {
		if n == name {
			return true
		}
	}
	return false
}

func names(list []rulesMember) []string {
	out := make([]string, len(list))
	for i, m := range list {
		out[i] = m.name
	}
	return out
}

type rulesMember struct {
	name     string
	required bool
	check    func(v any, at string) error
}

func rulesText(v any, at string) error {
	if _, ok := v.(string); !ok {
		return rulesFail("rules_value", at, "is not text")
	}
	return nil
}

func rulesName(v any, at string) error {
	if s, ok := v.(string); !ok || s == "" {
		if !ok {
			return rulesFail("rules_value", at, "is not text")
		}
		return rulesFail("rules_value", at, "is empty")
	}
	return nil
}

func rulesBool(v any, at string) error {
	if _, ok := v.(bool); !ok {
		return rulesFail("rules_value", at, "is not true or false")
	}
	return nil
}

func rulesYes(v any, at string) error {
	if b, ok := v.(bool); !ok || !b {
		return rulesFail("rules_value", at, "is true where it is written")
	}
	return nil
}

// rulesWhole is v as a whole number, or false.
func rulesWhole(v any) (float64, bool) {
	var f float64
	switch n := v.(type) {
	case float64:
		f = n
	case int:
		f = float64(n)
	case int64:
		f = float64(n)
	default:
		return 0, false
	}
	return f, f >= 0 && f == math.Trunc(f) && !math.IsInf(f, 0)
}

func rulesCount(v any, at string) error {
	if _, ok := rulesWhole(v); !ok {
		return rulesFail("rules_value", at, "is not a whole number")
	}
	return nil
}

func rulesCodePoint(v any, at string) error {
	if f, ok := rulesWhole(v); !ok || f > MaxCodePoint {
		return rulesFail("rules_value", at, "is not a code point")
	}
	return nil
}

// rulesMembers holds an object's members to the kind's list: each in the
// list's order, none twice, every required one there, and no other.
func rulesMembers(keys []string, vals map[string]any, list []rulesMember, at string) error {
	next := 0
	for _, key := range keys {
		found := -1
		for i := next; i < len(list); i++ {
			if list[i].name == key {
				found = i
				break
			}
		}
		if found < 0 {
			for _, m := range list {
				if m.name == key {
					return rulesFail("rules_member", at+"."+key, "comes out of its place, or twice")
				}
			}
			return rulesFail("rules_member", at+"."+key, "is no member of this node")
		}
		for i := next; i < found; i++ {
			if list[i].required {
				return rulesFail("rules_member", at+"."+list[i].name, "is missing")
			}
		}
		if err := list[found].check(vals[key], at+"."+key); err != nil {
			return err
		}
		next = found + 1
	}
	for i := next; i < len(list); i++ {
		if list[i].required {
			return rulesFail("rules_member", at+"."+list[i].name, "is missing")
		}
	}
	return nil
}

func rulesIndex(at string, i int) string { return at + "[" + strconv.Itoa(i) + "]" }

func rulesRepeats(v any, at string) error {
	list, ok := v.([]any)
	if !ok || len(list) == 0 {
		return rulesFail("rules_value", at, "is not a list of repetitions")
	}
	for i, r := range list {
		rat := rulesIndex(at, i)
		list := []rulesMember{{"min", true, rulesCount}, {"max", false, rulesCount}}
		keys, vals, ok := rulesObject(r, names(list))
		if !ok {
			return rulesFail("rules_value", rat, "is not a repetition")
		}
		if err := rulesMembers(keys, vals, list, rat); err != nil {
			return err
		}
		if max, has := vals["max"]; has {
			hi, _ := rulesWhole(max)
			lo, _ := rulesWhole(vals["min"])
			if hi < lo {
				return rulesFail("rules_value", rat+".max", "is less than min")
			}
		}
	}
	return nil
}

func rulesSpan(vals map[string]any, at string) error {
	from, _ := rulesWhole(vals["from"])
	to, _ := rulesWhole(vals["to"])
	if to < from {
		return rulesFail("rules_value", at+".to", "is less than from")
	}
	return nil
}

func rulesClassMembers(v any, at string) error {
	list, ok := v.([]any)
	if !ok {
		return rulesFail("rules_value", at, "is not a list")
	}
	for i, m := range list {
		mat := rulesIndex(at, i)
		list := []rulesMember{{"from", true, rulesCodePoint}, {"to", true, rulesCodePoint}}
		keys, vals, ok := rulesObject(m, names(list))
		if !ok {
			return rulesFail("rules_value", mat, "is not a span of code points")
		}
		if err := rulesMembers(keys, vals, list, mat); err != nil {
			return err
		}
		if err := rulesSpan(vals, mat); err != nil {
			return err
		}
	}
	return nil
}

func rulesElements() map[string][]rulesMember {
	return map[string][]rulesMember{
		"reference": {{"name", true, rulesName}},
		"literal":   {{"text", true, rulesText}, {"caseSensitive", true, rulesBool}},
		"char":      {{"value", true, rulesCodePoint}},
		"range":     {{"from", true, rulesCodePoint}, {"to", true, rulesCodePoint}},
		"class":     {{"negated", true, rulesBool}, {"members", true, rulesClassMembers}},
		"any":       {},
		"token":     {{"negated", true, rulesBool}, {"text", true, rulesText}},
		"prose":     {{"text", true, rulesText}},
		"group":     {{"alternatives", true, rulesAlternatives}},
	}
}

func rulesAny(any, string) error { return nil }

// rulesTyped is an object whose first member is its type, one of the
// kinds given, with that kind's members after it.
func rulesTyped(v any, at string, kinds map[string][]rulesMember, code, what string) error {
	var kind string
	if keys, vals, ok := rulesObject(v, []string{"type"}); ok {
		kind, _ = vals["type"].(string)
		list, known := kinds[kind]
		if known && len(keys) > 0 && keys[0] == "type" {
			full := append([]rulesMember{{"type", true, rulesAny}}, list...)
			keys, vals, _ = rulesObject(v, names(full))
			return rulesMembers(keys, vals, full, at)
		}
	}
	return rulesFail(code, at, fmt.Sprintf("is no %s: its first member is not a type it has (%q)", what, kind))
}

func rulesElement(v any, at string) error {
	if _, _, ok := rulesObject(v, nil); !ok {
		return rulesFail("rules_value", at, "is not an element")
	}
	kinds := rulesElements()
	for kind, list := range kinds {
		kinds[kind] = append(list, rulesMember{"repeat", false, rulesRepeats})
	}
	if err := rulesTyped(v, at, kinds, "rules_type", "element"); err != nil {
		return err
	}
	_, vals, _ := rulesObject(v, nil)
	if vals["type"] == "range" {
		return rulesSpan(vals, at)
	}
	return nil
}

func rulesAlternatives(v any, at string) error {
	list, ok := v.([]any)
	if !ok || len(list) == 0 {
		return rulesFail("rules_value", at, "is not a list of alternatives")
	}
	for i, alt := range list {
		aat := rulesIndex(at, i)
		elements, ok := alt.([]any)
		if !ok {
			return rulesFail("rules_value", aat, "is not a list of elements")
		}
		for j, el := range elements {
			if err := rulesElement(el, rulesIndex(aat, j)); err != nil {
				return err
			}
		}
	}
	return nil
}

func rulesRule(v any, at string) error {
	return rulesTyped(v, at, map[string][]rulesMember{"rule": {
		{"name", true, rulesName},
		{"incremental", false, rulesYes},
		{"alternatives", true, rulesAlternatives},
	}}, "rules_type", "rule")
}

// CheckRules holds a value to the schema grammar-rules, or answers a
// *RulesError naming what was wrong and where.
func CheckRules(tree any) error {
	err := rulesTyped(tree, "$", map[string][]rulesMember{"grammar": {
		{"rules", true, func(v any, at string) error {
			list, ok := v.([]any)
			if !ok {
				return rulesFail("rules_value", at, "is not a list of rules")
			}
			for i, r := range list {
				if err := rulesRule(r, rulesIndex(at, i)); err != nil {
					return err
				}
			}
			return nil
		}},
	}}, "rules_root", "grammar")
	return err
}
