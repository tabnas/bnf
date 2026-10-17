// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

package bnf

// transduce.go: the transducer part of an emitted grammar, the Go port of
// ts/src/transduce.ts, which says what the part is and when a grammar is
// read whole instead. The text is the same in every runtime: the entries
// in byte order of their rule names, a member name quoted alike, every
// refusal checked in the same order.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	tabnas "github.com/tabnas/parser/go"
)

// TransducePart is the transducer part of an emitted grammar, or why its
// documents are read whole. A part sets Entry, the definition it makes
// (`<name>-transduce`, a function of the input's parser events), and
// Source, its alchemy text; a grammar read whole sets Whole alone.
type TransducePart struct {
	Entry  string
	Source string
	Whole  string
}

// transduceEntry is what one rule's value actions do.
type transduceEntry struct {
	container string // "object", "array" or ""
	// continues: the container is the one a rule it replaced handed on.
	continues bool
	member    string
	hasMember bool
	// fills: what the member its close alternates fill holds, "text" (the
	// text of the rule it pushed) or "value" (that rule's own value), or
	// "" when they fill none.
	fills string
}

func (e *transduceEntry) any() bool {
	return e.container != "" || e.hasMember || e.fills != ""
}

// transduceValueActions are the engine's value builders. The compiler
// emits the first five; the other two are a refusal here.
var transduceValueActions = map[string]bool{
	"@object$": true, "@array$": true, "@key$": true, "@setval$": true,
	"@push$": true, "@value$": true, "@reset$": true,
}

// transduceSymbol is a name alchemy reads as a keyword's, and as a
// definition's: a symbol that is not a number, and not one of the value
// words.
var transduceSymbol = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.$-]*$`)

func isTransduceSymbol(name string) bool {
	return transduceSymbol.MatchString(name) && name != "true" && name != "false" && name != "null"
}

// transduceNamed is the rule an alternate pushes (p) or replaces its rule
// with (r), if it names one.
func transduceNamed(alt *tabnas.GrammarAltSpec, field byte) string {
	if alt == nil {
		return ""
	}
	if field == 'p' {
		return alt.P
	}
	return alt.R
}

// transduceAct is one value action of an alternate, with the config the
// engine binds it.
type transduceAct struct {
	act string
	cfg any
}

// transduceActs is an alternate's value actions, each with its config,
// and the same as a comparable signature.
func transduceActs(alt *tabnas.GrammarAltSpec) ([]transduceAct, string) {
	var refs []string
	switch a := alt.A; a := a.(type) {
	case string:
		refs = []string{a}
	case []any:
		for _, x := range a {
			if s, ok := x.(string); ok {
				refs = append(refs, s)
			}
		}
	case []string:
		refs = a
	}
	var acts []transduceAct
	var sig strings.Builder
	for _, ref := range refs {
		if !transduceValueActions[ref] {
			continue
		}
		var cfg any
		if alt.K != nil {
			cfg = alt.K[ref[1:]]
		}
		acts = append(acts, transduceAct{act: ref, cfg: cfg})
		data, _ := json.Marshal(cfg)
		sig.WriteString(ref)
		sig.WriteByte(' ')
		sig.Write(data)
		sig.WriteByte(';')
	}
	return acts, sig.String()
}

// transduceQuoted is an alchemy string: double quotes, a backslash before
// a quote or a backslash, a control character as \u00XX, and every other
// character as it is.
func transduceQuoted(text string) string {
	var out strings.Builder
	out.WriteByte('"')
	for _, ch := range text {
		switch {
		case ch == '"' || ch == '\\':
			out.WriteByte('\\')
			out.WriteRune(ch)
		case ch < 0x20:
			fmt.Fprintf(&out, "\\u%04x", ch)
		default:
			out.WriteRune(ch)
		}
	}
	out.WriteByte('"')
	return out.String()
}

// transduceStart is the rule a spec starts from: its options' rule.start.
func transduceStart(spec *tabnas.GrammarSpec) string {
	if spec.Options != nil && spec.Options.Rule != nil && spec.Options.Rule.Start != "" {
		return spec.Options.Rule.Start
	}
	if rule, ok := spec.OptionsMap["rule"].(map[string]any); ok {
		if start, ok := rule["start"].(string); ok {
			return start
		}
	}
	return ""
}

func transduceWhole(why string) TransducePart { return TransducePart{Whole: why} }

// TransduceAlc is the transducer part of an emitted grammar, defining
// `<name>-transduce`, or why its documents are read whole
// (ts/src/transduce.ts).
func TransduceAlc(spec *tabnas.GrammarSpec, name string) TransducePart {
	if !isTransduceSymbol(name) {
		return transduceWhole(fmt.Sprintf("the name %s is not one alchemy can give a definition", transduceQuoted(name)))
	}
	rules := spec.Rule
	names := make([]string, 0, len(rules))
	for rule, rs := range rules {
		if rs != nil {
			names = append(names, rule)
		}
	}
	sort.Strings(names)
	states := func(rule string) map[string][]*tabnas.GrammarAltSpec {
		rs := rules[rule]
		return map[string][]*tabnas.GrammarAltSpec{"open": altListOf(rs.Open), "close": altListOf(rs.Close)}
	}

	// What each rule's value actions do.
	entries := map[string]*transduceEntry{}
	for _, rule := range names {
		entry := &transduceEntry{}
		for _, side := range []string{"open", "close"} {
			alts := states(rule)[side]
			var first []transduceAct
			signature := ""
			for i, alt := range alts {
				acts, sig := transduceActs(alt)
				if i == 0 {
					first, signature = acts, sig
				} else if sig != signature {
					return transduceWhole(fmt.Sprintf("rule %s's %s alternates build its value differently", rule, side))
				}
			}
			for _, a := range first {
				cfg, _ := a.cfg.(map[string]any)
				switch {
				case side == "open" && (a.act == "@object$" || a.act == "@array$") && entry.container == "":
					entry.container = "array"
					if a.act == "@object$" {
						entry.container = "object"
					}
				case side == "open" && a.act == "@key$" && isString(cfg["lit"]) && !entry.hasMember:
					entry.member, entry.hasMember = cfg["lit"].(string), true
				case side == "close" && (a.act == "@setval$" || a.act == "@push$") && entry.fills == "":
					entry.fills = "value"
					if src, _ := cfg["src"].(bool); src {
						entry.fills = "text"
					}
				default:
					return transduceWhole(fmt.Sprintf("rule %s's %s alternates use %s as no transducer part reads it", rule, side, a.act))
				}
			}
		}
		if entry.any() {
			entries[rule] = entry
		}
	}

	// The rules a container rule's close replaces it with carry the
	// container on, and the rules theirs replace them with, to a fixed
	// point.
	for changed := true; changed; {
		changed = false
		for _, rule := range names {
			entry := entries[rule]
			if entry == nil || entry.container == "" {
				continue
			}
			for _, alt := range states(rule)["close"] {
				next := transduceNamed(alt, 'r')
				if next == "" {
					continue
				}
				if rules[next] == nil {
					return transduceWhole(fmt.Sprintf("rule %s hands its %s on to rule %s, which the grammar does not define", rule, entry.container, next))
				}
				target := entries[next]
				if target == nil {
					target = &transduceEntry{}
				}
				switch {
				case target.container == "":
					target.container = entry.container
					target.continues = true
					entries[next] = target
					changed = true
				case target.container != entry.container:
					article := "an array"
					if target.container == "object" {
						article = "an object"
					}
					return transduceWhole(fmt.Sprintf("rule %s hands its %s on to rule %s, a rule of %s", rule, entry.container, next, article))
				case !target.continues:
					return transduceWhole(fmt.Sprintf("rule %s hands its %s on to rule %s, which begins one of its own", rule, entry.container, next))
				}
			}
		}
	}

	sorted := make([]string, 0, len(entries))
	for rule := range entries {
		sorted = append(sorted, rule)
	}
	sort.Strings(sorted)
	for _, rule := range sorted {
		if !isTransduceSymbol(rule) {
			return transduceWhole(fmt.Sprintf("rule %s has no name alchemy can write as a keyword", transduceQuoted(rule)))
		}
	}

	// A container ends, and a text is yielded, on its rule's close, which
	// an open alternate that replaces the rule skips.
	for _, rule := range sorted {
		entry := entries[rule]
		if entry.container == "" && entry.fills != "text" {
			continue
		}
		for _, alt := range states(rule)["open"] {
			if transduceNamed(alt, 'r') != "" {
				return transduceWhole(fmt.Sprintf("rule %s's open alternate replaces it, so its close never runs", rule))
			}
		}
	}

	// A member that is a rule's text holds no value of its own: a value
	// rule beneath it builds what the text does not hold.
	beneath := func(start string) string {
		seen := map[string]bool{}
		stack := []string{start}
		for len(stack) > 0 {
			rule := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[rule] || rules[rule] == nil {
				continue
			}
			seen[rule] = true
			if entries[rule] != nil {
				return rule
			}
			var next []string
			for _, side := range []string{"open", "close"} {
				for _, alt := range states(rule)[side] {
					for _, field := range []byte{'p', 'r'} {
						if to := transduceNamed(alt, field); to != "" {
							next = append(next, to)
						}
					}
				}
			}
			for i := len(next) - 1; i >= 0; i-- {
				stack = append(stack, next[i])
			}
		}
		return ""
	}
	for _, rule := range sorted {
		if entries[rule].fills != "text" {
			continue
		}
		for _, alt := range states(rule)["open"] {
			pushed := transduceNamed(alt, 'p')
			if pushed == "" {
				continue
			}
			if found := beneath(pushed); found != "" {
				return transduceWhole(fmt.Sprintf("rule %s's member is the text of rule %s, beneath which rule %s builds a value of its own", rule, pushed, found))
			}
		}
	}

	// The value is the start rule's, which the compiler's start wrapper
	// pushes.
	start := ""
	if wrapper := transduceStart(spec); wrapper != "" && rules[wrapper] != nil {
		for _, alt := range states(wrapper)["open"] {
			if p := transduceNamed(alt, 'p'); p != "" {
				start = p
				break
			}
		}
	}
	if start == "" {
		return transduceWhole("the grammar has no start rule a wrapper pushes")
	}
	if root := entries[start]; root == nil || root.container == "" || root.continues {
		return transduceWhole(fmt.Sprintf("the start rule %s has no annotation, so the value is the {rule, src, kids} tree, "+
			"whose src comes before its kids and holds the text of every node beneath it: "+
			"no stream writes the root before the document has ended", start))
	}

	entry := name + "-transduce"
	lines := []string{
		fmt.Sprintf("; The transducer part of the grammar %s: the JSON events of the value", name),
		"; its @object and @array annotations build, from its rule passes, as",
		"; tabnas-bnf wrote them from the value actions those annotations attach.",
		fmt.Sprintf("def %s [input]", entry),
		"  transduce-tree",
		"    record",
	}
	for _, rule := range sorted {
		e := entries[rule]
		var fields []string
		if e.container != "" {
			fields = append(fields, "(entry :container :"+e.container+")")
		}
		if e.hasMember {
			fields = append(fields, "(entry :member "+transduceQuoted(e.member)+")")
		}
		if e.fills == "text" {
			fields = append(fields, "(entry :text true)")
		}
		if len(fields) > 0 {
			lines = append(lines, fmt.Sprintf("      entry :%s (record %s)", rule, strings.Join(fields, " ")))
		}
	}
	lines = append(lines, "    input", "")
	return TransducePart{Entry: entry, Source: strings.Join(lines, "\n")}
}

func isString(v any) bool {
	_, ok := v.(string)
	return ok
}
