// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

// Left factoring and the specificity ordering ask how many tokens a
// sequence can span (`seq_token_span` in src/factor.rs), and only up to
// LOOKAHEAD_K. The walk that answers keeps its own stack and one set of the
// rules on its way, and stops as soon as a sequence passes what the window
// leaves it. The walk it replaced recursed once per rule it followed and
// copied the path at every one: on a chain of rules each ending in the
// next, at the head of two alternatives, it followed the chain to its end,
// holding 3.9 GB at 10,000 rules and 8.5 GB at 15,000; at 30,000 the
// process was killed for memory, and a test thread's 2 MB stack runs out
// long before that.
//
// This file is a test binary of its own because it counts what the whole
// binary allocates.

mod common;

use std::alloc::{GlobalAlloc, Layout, System};
use std::io::Write;
use std::sync::atomic::{AtomicUsize, Ordering};

use common::{prod, reference, sens_term, star};
use tabnas_bnf::{emit_grammar_spec, ConvertOptions, Element, Grammar};

/// More than this binary ever has live while it compiles the chain of
/// 30,000 rules, which holds about 140 MB at its peak: a walk that grows
/// with the square of the chain passes it within the first few thousand
/// rules, and the refused allocation then aborts the binary instead of
/// exhausting the machine.
const BUDGET: usize = 512 << 20;

/// The bytes this binary has live, and the most it has had.
static LIVE: AtomicUsize = AtomicUsize::new(0);
static PEAK: AtomicUsize = AtomicUsize::new(0);

/// Counts every allocation, and refuses one that would take the live
/// total past [`BUDGET`].
struct Counting;

impl Counting {
    /// Count `size` bytes in, or refuse them when they pass the budget.
    fn take(size: usize) -> bool {
        let live = LIVE.fetch_add(size, Ordering::Relaxed) + size;
        if live > BUDGET {
            LIVE.fetch_sub(size, Ordering::Relaxed);
            // Nothing that allocates: this runs inside the allocator.
            let _ = std::io::stderr().write_all(
                b"token_span_test: the compiler passed the budget on the chain; \
                  refusing the allocation\n",
            );
            return false;
        }
        PEAK.fetch_max(live, Ordering::Relaxed);
        true
    }
}

unsafe impl GlobalAlloc for Counting {
    unsafe fn alloc(&self, layout: Layout) -> *mut u8 {
        if !Self::take(layout.size()) {
            return std::ptr::null_mut();
        }
        let ptr = System.alloc(layout);
        if ptr.is_null() {
            LIVE.fetch_sub(layout.size(), Ordering::Relaxed);
        }
        ptr
    }

    unsafe fn dealloc(&self, ptr: *mut u8, layout: Layout) {
        System.dealloc(ptr, layout);
        LIVE.fetch_sub(layout.size(), Ordering::Relaxed);
    }

    unsafe fn realloc(&self, ptr: *mut u8, layout: Layout, new_size: usize) -> *mut u8 {
        let grows = new_size.saturating_sub(layout.size());
        if !Self::take(grows) {
            return std::ptr::null_mut();
        }
        let out = System.realloc(ptr, layout, new_size);
        if out.is_null() {
            LIVE.fetch_sub(grows, Ordering::Relaxed);
        } else {
            LIVE.fetch_sub(layout.size().saturating_sub(new_size), Ordering::Relaxed);
        }
        out
    }
}

#[global_allocator]
static ALLOCATOR: Counting = Counting;

/// top = c0 "f0" / c0 "f1" ; c(i) = "g(i)" c(i+1) / … ; c(n-1) = "g(n-1)".
/// The two alternatives of top share their first element, so left
/// factoring asks how far c0 reaches. With `two`, each rule of the chain
/// but the last has a second alternative, "h(i)" c(i+1).
fn chain(n: usize, two: bool) -> Grammar {
    let mut prods = vec![prod(
        "top",
        vec![
            vec![reference("c0"), sens_term("f0")],
            vec![reference("c0"), sens_term("f1")],
        ],
    )];
    for i in 0..n {
        let alts = if i + 1 < n {
            let next = format!("c{}", i + 1);
            let mut alts = vec![vec![sens_term(&format!("g{i}")), reference(&next)]];
            if two {
                alts.push(vec![sens_term(&format!("h{i}")), reference(&next)]);
            }
            alts
        } else {
            vec![vec![sens_term(&format!("g{i}"))]]
        };
        prods.push(prod(&format!("c{i}"), alts));
    }
    Grammar::new(prods)
}

/// What compiling the chain of `n` rules has live at its peak, beyond what
/// was live before.
fn peak_compiling(n: usize) -> usize {
    let grammar = chain(n, false);
    let before = LIVE.load(Ordering::Relaxed);
    PEAK.store(before, Ordering::Relaxed);
    let spec = emit_grammar_spec(&grammar, &ConvertOptions::tag("x")).expect("the chain");
    let peak = PEAK.load(Ordering::Relaxed) - before;
    // The last rule, one literal, is lifted into a token; the one before it
    // is the deepest rule left.
    assert!(spec.rule.contains_key(&format!("c{}", n - 2)));
    peak
}

#[test]
fn a_chain_of_thirty_thousand_rules_holds_what_the_chain_does() {
    // Doubling the chain about doubles what the compile holds at its peak:
    // the walk adds nothing that grows with the chain. The walk this
    // replaced held a copy of the path at every rule it followed, n²/2
    // names, which takes the larger chain past the budget.
    let half = peak_compiling(15_000);
    let whole = peak_compiling(30_000);
    eprintln!("token_span_test: peak {half} bytes at 15,000 rules, {whole} at 30,000");
    assert!(
        whole < 3 * half,
        "30,000 rules held {whole} bytes, 15,000 held {half}"
    );
}

#[test]
fn each_alternative_of_a_rule_is_followed_no_further_than_the_lookahead() {
    // 30 rules of two alternatives each: 2³⁰ paths to the end of the chain,
    // and at most 2⁵ within the window.
    let start = std::time::Instant::now();
    let spec = emit_grammar_spec(&chain(30, true), &ConvertOptions::tag("x")).expect("the chain");
    let elapsed = start.elapsed();
    assert!(spec.rule.contains_key("top"));
    assert!(
        elapsed < std::time::Duration::from_secs(5),
        "compiled after {elapsed:?}"
    );
}

#[test]
fn a_repetition_of_at_most_none_spans_no_tokens() {
    // Two alternatives share a repetition of at most none, of an unbounded
    // element, and then n literals. The repetition spans no tokens, so the
    // prefix spans n: within the lookahead at 4, and factored at 5. The walk
    // this replaced read the repetition as unbounded, which always passed
    // the lookahead; TypeScript's and Go's read it as NaN, which TypeScript
    // took as within the lookahead and Go as past it.
    let shared = |n: usize| {
        let alts = ["y", "z"]
            .iter()
            .map(|last| {
                let mut alt = vec![Element::rep(0, Some(0), star(sens_term("x")))];
                alt.extend((0..n).map(|i| sens_term(&format!("a{i}"))));
                alt.push(sens_term(last));
                alt
            })
            .collect();
        Grammar::new(vec![prod("top", alts)])
    };
    let factored = |n: usize| -> Vec<String> {
        let spec = emit_grammar_spec(&shared(n), &ConvertOptions::tag("x")).expect("the prefix");
        spec.rule
            .keys()
            .filter(|name| name.contains("$fact"))
            .cloned()
            .collect()
    };
    assert_eq!(factored(4), Vec::<String>::new());
    assert_eq!(factored(5), vec!["top$fact0".to_string()]);
}
