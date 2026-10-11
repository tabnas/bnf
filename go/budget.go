// Copyright (c) 2025-2026 Richard Rodger and other contributors, MIT License

package bnf

// budget.go — the emit limit (MaxEmitUnits) and the memo limit
// (maxMemoEntries). Mirrors MAX_EMIT_UNITS and MAX_MEMO_ENTRIES in
// ts/src/compiler.ts, which is canonical.

import (
	"fmt"
	"strings"

	tabnas "github.com/tabnas/parser/go"
)

// MaxEmitUnits bounds what compiling one grammar may write, counted in
// units.
//
// The compiled grammar holds rules, and each rule holds alternates. A rule
// counts 1. An alternate counts 1, plus 1 for each token its s names, plus
// 1 for the rule it names in p or r. A token set counts 1 for each of its
// members, and each name a grammar removes (<remove>) counts 2, for the
// rule and the fixed token it writes in their place. Three tables the
// emitter builds on the way count an entry a unit as well: the tokens each
// probe helper lists (rewriteProbeDispatches), the tokens in each rule's
// FIRST and FOLLOW sets, and the pairs of tokens in each rule's FOLLOW₂
// set. A grammar of R rules and A alternates that name N tokens and rules
// in all, with M token-set members, D removed names, P probe entries, F
// FIRST and L FOLLOW entries and L₂ FOLLOW₂ pairs, writes
//
//	written = R + A + N + M + 2·D + P + F + L + L₂
//
// units. Each one is charged at the point it is generated, before the
// allocation that would hold it, so a grammar that would pass the limit is
// refused before the step that would pass it allocates.
//
// One thing more is counted while it lasts: the token paths a choice
// enumerates to tell its alternatives apart (altPrefixesRaw). While one
// alternative's enumeration runs, every path it makes counts 1 plus 1 for
// each token it holds, at every level of the enumeration; once it returns,
// only the paths it returned stay counted, and once the choice is decided,
// only the prefixes it dispatches on, until they have been written as its
// alternates. Written and held together never pass the limit:
//
//	written + held ≤ MaxEmitUnits
//
// MAX_EMIT_UNITS in ts/src/compiler.ts records the measurements behind the
// value.
const MaxEmitUnits = 3500000

// emitStep is what a step of the emitter was doing, for a refusal's
// wording: the clause after "while", and what the author can do about it.
type emitStep struct {
	doing  string
	advice string
}

var (
	emitStepProbe = emitStep{
		doing: "writing the probe for one of its optional prefixes",
		advice: "Write that optional prefix and what follows it as two " +
			"alternatives, one with the prefix and one without.",
	}
	emitStepTokenSet = emitStep{
		doing:  "listing the members of its token set",
		advice: "Write fewer character classes that overlap it.",
	}
	emitStepTokenClass = emitStep{
		doing:  "listing the members of its token class",
		advice: "Give it fewer alternatives.",
	}
	emitStepFirst = emitStep{
		doing:  "collecting the tokens it can begin with",
		advice: "Reduce the tokens it can begin with.",
	}
	emitStepFollow = emitStep{
		doing:  "collecting the tokens that can follow it",
		advice: "Reduce the tokens that can follow it.",
	}
	emitStepPairs = emitStep{
		doing:  "collecting the pairs of tokens that can follow it",
		advice: "Reduce the tokens that can follow it.",
	}
	emitStepAlternates = emitStep{
		doing:  "writing its alternates",
		advice: "Factor what its alternatives share into rules of their own.",
	}
	emitStepLookahead = emitStep{
		doing: "enumerating the token sequences that tell its alternatives apart",
		advice: "Give its alternatives first tokens that tell them apart, or " +
			"factor what they begin with into a rule of its own.",
	}
	emitStepRemove = emitStep{
		doing:  "writing the rules and tokens it removes",
		advice: "Remove fewer names.",
	}
)

// emitBudget is what one compile has written, and what the decision under
// way holds, against MaxEmitUnits. A refusal names the step under way, and
// counts from what had been written when that step began. A refusal is a
// panic with an *EmitError, which emitGrammarSpec recovers into its error
// return.
type emitBudget struct {
	written int
	held    int
	subject string
	rule    string
	sp      *SrcSpan
	step    emitStep
	start   int
}

func newEmitBudget() *emitBudget {
	return &emitBudget{step: emitStepAlternates}
}

// begin begins a step. subject is what a refusal names (`rule 'x'`), rule
// and sp what it carries.
func (b *emitBudget) begin(subject, rule string, sp *SrcSpan, step emitStep) {
	b.subject = subject
	b.rule = rule
	b.sp = sp
	b.step = step
	b.start = b.written
}

// charge charges n units the compiled grammar will hold, before they are
// made.
func (b *emitBudget) charge(n int) {
	if n > MaxEmitUnits-b.written-b.held {
		b.refuse(false)
	}
	b.written += n
}

// hold holds n units of the decision under way, before they are made.
func (b *emitBudget) hold(n int) {
	if n > MaxEmitUnits-b.written-b.held {
		b.refuse(true)
	}
	b.held += n
}

// settle records that one alternative's enumeration has returned paths,
// having begun with base held: from now on only the paths it returned are
// held. Each of them was held as it was made, so this never holds more.
func (b *emitBudget) settle(base int, paths []prefixPath) {
	n := 0
	for _, p := range paths {
		n += 1 + len(p.tokens)
	}
	b.held = base + n
}

// keep records that a decision which began with base held has made what it
// returns, n units: from now on that alone stays held, until its caller has
// used it.
func (b *emitBudget) keep(base, n int) {
	b.held = base + n
}

// restore records that what a decision returned has been used: the budget
// holds again what it held before the decision began.
func (b *emitBudget) restore(base int) {
	b.held = base
}

func (b *emitBudget) refuse(holding bool) {
	step := b.step
	if holding {
		step = emitStepLookahead
	}
	room := MaxEmitUnits - b.start
	var count string
	if holding {
		count = fmt.Sprintf("hold more than %d units on top of the %d already written",
			room, b.start)
	} else {
		count = fmt.Sprintf("add more than %d units to the %d already written",
			room, b.start)
	}
	panic(&EmitError{
		Message: fmt.Sprintf("%s: %s exceeds the emit limit of %d units while %s, "+
			"which would %s. %s",
			diagName(), b.subject, MaxEmitUnits, step.doing, count, step.advice),
		Rule: b.rule,
		Sp:   b.sp,
	})
}

// probeRuleUnits is the units of the rule a probe helper listing n tokens
// becomes (MaxEmitUnits): 1 for the rule, an alternate of 3 for each token,
// and the fallback alternate of 1.
func probeRuleUnits(n int) int {
	return 3*n + 2
}

// ruleSubject is what a refusal names for a rule.
func ruleSubject(name string) string {
	return "rule '" + name + "'"
}

// removalSubject is what a refusal names for a grammar's <remove> list of
// n names.
func removalSubject(n int) string {
	if n == 1 {
		return "the removal list of 1 name"
	}
	return fmt.Sprintf("the removal list of %d names", n)
}

// altUnits is what an alternate holds (MaxEmitUnits): 1, 1 for each token
// its s names, and 1 for the rule it names in p or r.
func altUnits(m map[string]any) int {
	n := 1
	if s, ok := m["s"].(string); ok && s != "" {
		n += 1 + strings.Count(s, " ")
	}
	if p, ok := m["p"].(string); ok && p != "" {
		n++
	}
	if r, ok := m["r"].(string); ok && r != "" {
		n++
	}
	return n
}

// specAltUnits is altUnits for an alternate already converted.
func specAltUnits(a *tabnas.GrammarAltSpec) int {
	n := 1
	if s, ok := a.S.(string); ok && s != "" {
		n += 1 + strings.Count(s, " ")
	}
	if a.P != "" {
		n++
	}
	if a.R != "" {
		n++
	}
	return n
}

// ruleUnits is what a rule holds: 1, and its alternates'.
func ruleUnits(rs *tabnas.GrammarRuleSpec) int {
	n := 1
	for _, a := range altListOf(rs.Open) {
		n += specAltUnits(a)
	}
	for _, a := range altListOf(rs.Close) {
		n += specAltUnits(a)
	}
	return n
}

// chargeAlt charges an alternate the compiled grammar will hold, and
// returns its units.
func chargeAlt(budget *emitBudget, m map[string]any) int {
	n := altUnits(m)
	budget.charge(n)
	return n
}

// commitRule writes a rule into the compiled grammar, charging what it
// holds beyond the pre units already charged for its alternates as they
// were made.
func commitRule(ruleSpec map[string]*tabnas.GrammarRuleSpec, name string,
	rs *tabnas.GrammarRuleSpec, budget *emitBudget, pre int) {
	units := ruleUnits(rs)
	if units < pre {
		panic(fmt.Sprintf("%s: internal — rule '%s' holds %d units, fewer than "+
			"the %d charged for its alternates", diagName(), name, units, pre))
	}
	budget.charge(units - pre)
	ruleSpec[name] = rs
}

// maxMemoEntries is the most answers a memo of token pairs keeps:
// headsContest's and tokensOverlap's, each over a whole compile, and the
// keyword-shadow ordering's within one rule. A memo only saves asking
// again, so past the limit a pair is answered and not kept, which changes no
// answer and so no output. Without it, the contested-head scan of a rule
// with thousands of alternatives asks every pair of their heads and keeps
// every answer. MAX_MEMO_ENTRIES in ts/src/compiler.ts records the
// measurement.
const maxMemoEntries = 65536

// memoLimit is the limit in force: maxMemoEntries but for the tests, which
// lift it to show that it changes no output.
var memoLimit = maxMemoEntries
