// Copyright (c) 2026 Richard Rodger and other contributors, MIT License

// A chain of rules, each one alternative that begins with the next, is
// what Paull's substitution copies most: every rule ends up holding the
// whole of the rule after it in front of its own tail, so a chain of n
// rules holds about n²/2 element occurrences between them.
// MAX_LEFT_RECURSION_ELEMENTS refuses the chain once the occurrences it
// adds would pass 32,768 (tests/leftrec_limit_test.rs), and the passes
// share an element between every sequence that holds it, as the
// TypeScript and Go compilers do (src/shared.rs). So for the longest chain
// the bound admits the compiler holds a pointer or two per occurrence,
// never an element: an owned element is `size_of::<Element>()` bytes before
// the strings it holds. Copying owned elements and with no bound, the
// compiler held 1.7 GB for a chain of 2,000 rules, and 6 GB was not enough
// for 5,000.
//
// This file is a test binary of its own because it counts what the whole
// binary allocates.

mod common;

use std::alloc::{GlobalAlloc, Layout, System};
use std::io::Write;
use std::sync::atomic::{AtomicUsize, Ordering};

use common::{prod, reference, sens_term};
use tabnas_bnf::{emit_grammar_spec, ConvertOptions, Element, Grammar};

/// The rules in the chain: the most the element bound admits, whose
/// substitutions add 1 + 2 + … + 255 = 32,640 occurrences.
const N: usize = 257;

/// The element occurrences the chain's rules hold once the substitution
/// has run: the last rule is lifted into a token, and each rule before it
/// holds that token and the literals of every rule after it, `N - i`
/// elements for rule `i`.
const OCCURRENCES: usize = N * (N + 1) / 2 - 1;

/// Less than one owned element per occurrence: what the compiler may have
/// live at its peak, the spec it writes included.
const BUDGET: usize = OCCURRENCES * std::mem::size_of::<Element>();

/// The bytes this binary has live, and the most it has had.
static LIVE: AtomicUsize = AtomicUsize::new(0);
static PEAK: AtomicUsize = AtomicUsize::new(0);

/// Counts every allocation, and refuses one that would take the live
/// total past [`BUDGET`]: a compiler that copies elements then fails this
/// test in seconds, with the process aborted on the refused allocation,
/// rather than exhausting the machine it runs on.
struct Counting;

impl Counting {
    /// Count `size` bytes in, or refuse them when they pass the budget.
    fn take(size: usize) -> bool {
        let live = LIVE.fetch_add(size, Ordering::Relaxed) + size;
        if live > BUDGET {
            LIVE.fetch_sub(size, Ordering::Relaxed);
            // Nothing that allocates: this runs inside the allocator.
            let _ = std::io::stderr().write_all(
                b"leftrec_chain_test: the compiler passed one owned element per occurrence \
                  of the chain; refusing the allocation\n",
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

/// root = r0 ; r(i) = r(i+1) "x(i)" ; r(n-1) = "end"
fn chain(n: usize) -> Grammar {
    let mut prods = vec![prod("root", vec![vec![reference("r0")]])];
    for i in 0..n - 1 {
        prods.push(prod(
            &format!("r{i}"),
            vec![vec![
                reference(&format!("r{}", i + 1)),
                sens_term(&format!("x{i}")),
            ]],
        ));
    }
    prods.push(prod(&format!("r{}", n - 1), vec![vec![sens_term("end")]]));
    Grammar::new(prods)
}

#[test]
fn the_longest_chain_the_element_limit_admits_holds_no_element_per_occurrence() {
    let grammar = chain(N);
    let before = LIVE.load(Ordering::Relaxed);
    PEAK.store(before, Ordering::Relaxed);

    let spec = emit_grammar_spec(&grammar, &ConvertOptions::tag("chain")).expect("the chain");
    let held = PEAK.load(Ordering::Relaxed) - before;

    // The first rule matches the whole chain: the lifted last rule, then
    // every literal from the last rule's caller back to its own.
    let r0 = spec.rule["r0"].as_ref().expect("r0 is emitted");
    let s = r0.open[0].s().expect("r0 matches tokens");
    assert_eq!(s.split(' ').count(), N);
    assert!(
        s.starts_with(&format!("#r{} #X{} ", N - 1, N - 2)),
        "{}",
        &s[..40]
    );
    assert!(s.ends_with(" #X1 #X0"), "{}", &s[s.len() - 40..]);

    assert!(
        held < BUDGET,
        "the compiler held {held} bytes for {OCCURRENCES} element occurrences, {} per \
         occurrence: an owned element is {}",
        held / OCCURRENCES,
        std::mem::size_of::<Element>()
    );
}
