#!/usr/bin/env bash
# Rust port gate. Kept in one script so local and hosted validation cannot
# quietly drift apart: .github/workflows/rust.yml runs this file, and so can
# you. `make test-rs` is the fast inner loop; this is the full gate.
#
# The engine is a PATH DEPENDENCY on the sibling checkout
# (rs/Cargo.toml: `tabnas = { path = "../../parser/rs" }`), and the crate
# is unpublished, so there is no registry version to fall back on. Clone
# https://github.com/tabnas/parser next to this repo before running.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)

# Every path dependency of rs/Cargo.toml, checked before cargo gets a
# chance to fail on it with a less useful message.
for SIBLING in parser; do
  if [[ ! -f "$ROOT/../$SIBLING/rs/Cargo.toml" ]]; then
    echo "no $SIBLING checkout at $ROOT/../$SIBLING/rs" >&2
    echo "clone https://github.com/tabnas/$SIBLING as a sibling of $(basename "$ROOT")" >&2
    exit 1
  fi
done

cd "$ROOT/rs"

# Run through the MSRV toolchain when one is available. The workflow
# installs it explicitly, but a contributor running this script gets
# whatever `cargo` is on their PATH -- and a newer toolchain accepts code
# and formatting that the MSRV rejects, so the "local and hosted cannot
# drift" claim this script exists for would hold everywhere except the
# compiler version. Loud rather than silent when the toolchain is absent,
# because a quiet fallback is the drift.
MSRV=$(awk -F'"' '/^rust-version = /{print $2; exit}' Cargo.toml)
CARGO=(cargo)
if [[ -n "$MSRV" ]]; then
  if command -v rustup >/dev/null 2>&1 && rustup toolchain list | grep -q "^$MSRV"; then
    CARGO=(cargo "+$MSRV")
  else
    echo "warning: MSRV $MSRV is not installed; running on $(rustc --version 2>/dev/null)" >&2
    echo "         install it with: rustup toolchain install $MSRV" >&2
    echo "         a newer toolchain can accept what $MSRV rejects" >&2
  fi
fi

# Assert the lock's entry for THIS crate still matches the manifest,
# BEFORE anything runs cargo. Without `--locked` (see below) a cargo
# command silently rewrites Cargo.lock in the runner, so a version bump
# that updates rs/Cargo.toml and forgets rs/Cargo.lock passes every
# check and ships a stale lock. This has to come first -- after a cargo
# command the lock has already been fixed up and the check can never fail.
#
# Only this crate's entry is asserted. The engine's entry legitimately
# moves whenever the sibling checkout does, which is the same reason
# blanket `--locked` is wrong here.
CRATE=$(awk -F'"' '/^name = /{print $2; exit}' Cargo.toml)
WANT=$(awk -F'"' '/^version = /{print $2; exit}' Cargo.toml)
HAVE=$(awk -v c="$CRATE" -F'"' '
  $0 == "name = \"" c "\"" { f = 1; next }
  f && /^version = / { print $2; exit }
' Cargo.lock)

if [[ "$WANT" != "$HAVE" ]]; then
  echo "Cargo.lock records $CRATE ${HAVE:-<missing>}, but Cargo.toml says $WANT" >&2
  echo "run a cargo command and commit the updated rs/Cargo.lock" >&2
  exit 1
fi

# That version check is the common case stated clearly; it is NOT the whole
# check. A pull request that adds, removes or re-pins a DEPENDENCY leaves
# the crate's own version alone, so it sails past the comparison above while
# leaving the committed lock stale -- cargo then regenerates it in the
# runner and everything goes green.
#
# So the whole resolution is compared, before and after cargo runs, with one
# exemption per path dependency: the sibling crate's recorded version. That
# entry legitimately moves whenever the sibling checkout does, and exempting
# exactly it is what makes a full comparison usable here when blanket
# `--locked` is not.
lock_without_sibling_versions() {
  awk '
    /^\[\[package\]\]$/  { sib = 0 }
    /^name = "tabnas"$/  { sib = 1 }
    sib && /^version = / { print "version = \"<sibling>\""; next }
                         { print }
  ' "$1"
}

LOCK_BEFORE=$(mktemp)
cp Cargo.lock "$LOCK_BEFORE"
# On any exit, a red run included, put the lock back if a cargo command
# rewrote it, then drop the snapshot: the tree is left as it was found.
trap 'if [ -f "$LOCK_BEFORE" ] && ! cmp -s "$LOCK_BEFORE" Cargo.lock; then cp "$LOCK_BEFORE" Cargo.lock; fi; rm -f "$LOCK_BEFORE"' EXIT

# NOT `--locked`, deliberately, and this is the one place the plugin gate
# differs from the engine's own (parser ci/rust/run.sh does pass it).
#
# Cargo.lock records the engine by version, and the engine is resolved
# from a sibling checkout of MAIN. So the day parser bumps its crate
# version, `--locked` here fails with "cannot update the lock file" on
# every pull request in this repo, including ones that touch no Rust at
# all -- a red build caused by another repository's release.
#
# NOT `--all` on fmt either. cargo defines it as "all packages, and also
# their local path-based dependencies", and the engine IS such a
# dependency, so `--all` reaches into the sibling parser checkout: an
# unformatted file over there fails this gate even when every file here
# is clean.
"${CARGO[@]}" fmt --check
"${CARGO[@]}" build --all-targets
"${CARGO[@]}" test --all-targets
# `--all-targets` does NOT include doctests -- cargo documents the selector
# as "Test all targets (does not include doctests)" -- so a broken example
# in the crate docs (and the README, which is doctested) passes a gate
# that only runs it.
"${CARGO[@]}" test --doc
"${CARGO[@]}" clippy --all-targets --all-features -- -D warnings

# Now that cargo has had every chance to rewrite it, the lock must still
# describe the same resolution it did when committed.
if ! diff -q <(lock_without_sibling_versions "$LOCK_BEFORE") \
             <(lock_without_sibling_versions Cargo.lock) >/dev/null; then
  echo "rs/Cargo.lock does not match rs/Cargo.toml -- cargo rewrote it:" >&2
  diff <(lock_without_sibling_versions "$LOCK_BEFORE") \
       <(lock_without_sibling_versions Cargo.lock) >&2 || true
  echo >&2
  echo "run a cargo command and commit the updated rs/Cargo.lock" >&2
  cp "$LOCK_BEFORE" Cargo.lock   # leave the tree as it was found
  exit 1
fi
# The exempted sibling versions may still have moved, and cargo wrote them
# into the lock. Put the lock back so a green gate leaves the tree exactly
# as it found it, whatever the siblings did: updating the committed lock is
# a deliberate cargo run and commit, never a side effect of running the gate.
if ! cmp -s "$LOCK_BEFORE" Cargo.lock; then
  cp "$LOCK_BEFORE" Cargo.lock
fi

# A green run leaves the tree exactly as it found it too. The exempted
# sibling versions may have moved and cargo may have rewritten them;
# updating the committed lock is a deliberate cargo run and commit, never
# a gate side effect.
if ! cmp -s "$LOCK_BEFORE" Cargo.lock; then
  cp "$LOCK_BEFORE" Cargo.lock
fi
