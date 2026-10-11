// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

//! The emit limit ([`MAX_EMIT_UNITS`]) and the memo limit
//! (`MAX_MEMO_ENTRIES`). Mirrors `MAX_EMIT_UNITS` and `MAX_MEMO_ENTRIES` in
//! `ts/src/compiler.ts`, which is canonical.

use std::cell::{Cell, RefCell};

use crate::analysis::PrefixPath;
use crate::ir::{diag_name, EmitError, SrcSpan};
use crate::spec::{AltSpec, RuleSpec};

/// What compiling one grammar may write, counted in units.
///
/// The compiled grammar holds rules, and each rule holds alternates. A rule
/// counts 1. An alternate counts 1, plus 1 for each token its `s` names,
/// plus 1 for the rule it names in `p` or `r`. A token set counts 1 for
/// each of its members. Three tables the emitter builds on the way count an
/// entry a unit as well: the tokens each probe helper lists, the tokens in
/// each rule's FIRST and FOLLOW sets, and the pairs of tokens in each rule's
/// FOLLOW₂ set. A grammar of R rules and A alternates that name N tokens and
/// rules in all, with M token-set members, P probe entries, F FIRST and L
/// FOLLOW entries and L₂ FOLLOW₂ pairs, writes
/// `R + A + N + M + P + F + L + L₂` units. Each one is charged at the point
/// it is generated, before the allocation that would hold it, so a grammar
/// that would pass the limit is refused before the step that would pass it
/// allocates.
///
/// One thing more is counted while it lasts: the token paths a choice
/// enumerates to tell its alternatives apart. While one alternative's
/// enumeration runs, every path it makes counts 1 plus 1 for each token it
/// holds, at every level of the enumeration; once it returns, only the
/// paths it returned stay counted, until the choice is decided. Written and
/// held together never pass the limit. `MAX_EMIT_UNITS` in
/// `ts/src/compiler.ts` records the measurements behind the value.
pub const MAX_EMIT_UNITS: usize = 3_500_000;

/// What a step of the emitter was doing, for a refusal's wording: the
/// clause after "while", and what the author can do about it.
pub(crate) struct EmitStep {
    doing: &'static str,
    advice: &'static str,
}

pub(crate) const STEP_PROBE: EmitStep = EmitStep {
    doing: "writing the probe for one of its optional prefixes",
    advice: "Write that optional prefix and what follows it as two \
             alternatives, one with the prefix and one without.",
};
pub(crate) const STEP_TOKEN_SET: EmitStep = EmitStep {
    doing: "listing the members of its token set",
    advice: "Write fewer character classes that overlap it.",
};
pub(crate) const STEP_TOKEN_CLASS: EmitStep = EmitStep {
    doing: "listing the members of its token class",
    advice: "Give it fewer alternatives.",
};
pub(crate) const STEP_FIRST: EmitStep = EmitStep {
    doing: "collecting the tokens it can begin with",
    advice: "Reduce the tokens it can begin with.",
};
pub(crate) const STEP_FOLLOW: EmitStep = EmitStep {
    doing: "collecting the tokens that can follow it",
    advice: "Reduce the tokens that can follow it.",
};
pub(crate) const STEP_PAIRS: EmitStep = EmitStep {
    doing: "collecting the pairs of tokens that can follow it",
    advice: "Reduce the tokens that can follow it.",
};
pub(crate) const STEP_ALTERNATES: EmitStep = EmitStep {
    doing: "writing its alternates",
    advice: "Factor what its alternatives share into rules of their own.",
};
const STEP_LOOKAHEAD: EmitStep = EmitStep {
    doing: "enumerating the token sequences that tell its alternatives apart",
    advice: "Give its alternatives first tokens that tell them apart, or \
             factor what they begin with into a rule of its own.",
};

/// What a refusal names.
#[derive(Clone)]
pub(crate) enum Subject {
    /// `rule 'x'`, carrying the rule.
    Rule(String),
    /// `the character class /p/`, carrying no rule.
    Class(String),
}

struct Step {
    subject: Subject,
    sp: Option<SrcSpan>,
    step: &'static EmitStep,
    start: usize,
}

/// What one compile has written, and what the decision under way holds,
/// against [`MAX_EMIT_UNITS`]. A refusal names the step under way, and
/// counts from what had been written when that step began. The counts are
/// cells, so that a pass holding the budget by shared reference can charge
/// it.
pub(crate) struct EmitBudget {
    written: Cell<usize>,
    held: Cell<usize>,
    step: RefCell<Step>,
}

impl EmitBudget {
    pub fn new() -> Self {
        Self {
            written: Cell::new(0),
            held: Cell::new(0),
            step: RefCell::new(Step {
                subject: Subject::Rule(String::new()),
                sp: None,
                step: &STEP_ALTERNATES,
                start: 0,
            }),
        }
    }

    /// What is held now, for [`EmitBudget::settle`].
    pub fn held(&self) -> usize {
        self.held.get()
    }

    /// Begin a step: a refusal names `subject` and carries `sp`.
    pub fn begin(&self, subject: Subject, sp: Option<SrcSpan>, step: &'static EmitStep) {
        *self.step.borrow_mut() = Step {
            subject,
            sp,
            step,
            start: self.written.get(),
        };
    }

    /// Begin a step that names a rule.
    pub fn begin_rule(&self, rule: &str, sp: Option<SrcSpan>, step: &'static EmitStep) {
        self.begin(Subject::Rule(rule.to_string()), sp, step);
    }

    fn room(&self) -> usize {
        MAX_EMIT_UNITS.saturating_sub(self.written.get() + self.held.get())
    }

    /// Charge `n` units the compiled grammar will hold, before they are
    /// made.
    pub fn charge(&self, n: usize) -> Result<(), EmitError> {
        if n > self.room() {
            return Err(self.refusal(false));
        }
        self.written.set(self.written.get() + n);
        Ok(())
    }

    /// Hold `n` units of the decision under way, before they are made.
    pub fn hold(&self, n: usize) -> Result<(), EmitError> {
        if n > self.room() {
            return Err(self.refusal(true));
        }
        self.held.set(self.held.get() + n);
        Ok(())
    }

    /// One alternative's enumeration has returned `paths`, having begun
    /// with `base` held: from now on only the paths it returned are held.
    /// Each of them was held as it was made, so this never holds more.
    pub fn settle(&self, base: usize, paths: &[PrefixPath]) {
        let n: usize = paths.iter().map(|p| 1 + p.tokens.len()).sum();
        self.held.set(base + n);
    }

    /// The decision is made: what it held is free again.
    pub fn release(&self) {
        self.held.set(0);
    }

    fn refusal(&self, holding: bool) -> EmitError {
        let step = self.step.borrow();
        let doing = if holding { &STEP_LOOKAHEAD } else { step.step };
        let room = MAX_EMIT_UNITS - step.start;
        let count = if holding {
            format!(
                "hold more than {room} units on top of the {} already written",
                step.start
            )
        } else {
            format!(
                "add more than {room} units to the {} already written",
                step.start
            )
        };
        let (subject, rule) = match &step.subject {
            Subject::Rule(name) => (format!("rule '{name}'"), Some(name.clone())),
            Subject::Class(pattern) => (format!("the character class /{pattern}/"), None),
        };
        EmitError {
            message: format!(
                "{}: {subject} exceeds the emit limit of {MAX_EMIT_UNITS} units while {}, \
                 which would {count}. {}",
                diag_name(),
                doing.doing,
                doing.advice
            ),
            rule,
            sp: step.sp,
        }
    }
}

/// The units an alternate holds ([`MAX_EMIT_UNITS`]): 1, 1 for each token
/// its `s` names, and 1 for the rule it names in `p` or `r`.
pub(crate) fn alt_units(o: &AltSpec) -> usize {
    let mut n = 1;
    if let Some(s) = o.s().filter(|s| !s.is_empty()) {
        n += 1 + s.matches(' ').count();
    }
    if o.p().is_some() {
        n += 1;
    }
    if o.r().is_some() {
        n += 1;
    }
    n
}

/// The units a rule holds: 1, and its alternates'.
pub(crate) fn rule_units(rs: &RuleSpec) -> usize {
    1 + rs.open.iter().map(alt_units).sum::<usize>()
        + rs.close.iter().flatten().map(alt_units).sum::<usize>()
}

/// The units of the rule a probe helper listing `n` tokens becomes
/// ([`MAX_EMIT_UNITS`]): 1 for the rule, an alternate of 3 for each token,
/// and the fallback alternate of 1.
pub(crate) fn probe_rule_units(n: usize) -> usize {
    3 * n + 2
}

/// The most answers a memo of token pairs keeps: `heads_contest`'s and
/// `tokens_overlap`'s, each over a whole compile, and the keyword-shadow
/// ordering's within one rule. A memo only saves asking again, so past the
/// limit a pair is answered and not kept, which changes no answer and so no
/// output. Without it, the contested-head scan of a rule with thousands of
/// alternatives asks every pair of their heads and keeps every answer.
/// `MAX_MEMO_ENTRIES` in `ts/src/compiler.ts` records the measurement.
pub(crate) const MAX_MEMO_ENTRIES: usize = 65_536;

thread_local! {
    /// The limit in force: `MAX_MEMO_ENTRIES` but for the tests, which lift
    /// it to show that it changes no output.
    static MEMO_LIMIT: Cell<usize> = const { Cell::new(MAX_MEMO_ENTRIES) };
}

/// The memo limit in force on this thread.
pub(crate) fn memo_limit() -> usize {
    MEMO_LIMIT.with(Cell::get)
}

/// Set the memo limit on this thread, returning the one it replaces. For
/// the tests alone.
#[cfg(test)]
pub(crate) fn set_memo_limit(n: usize) -> usize {
    MEMO_LIMIT.with(|limit| limit.replace(n))
}
