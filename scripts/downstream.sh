#!/usr/bin/env bash
#
# downstream.sh — run the front-end suites against THIS working tree.
#
# A green build here proves much less than usual. This package emits specs
# that other packages consume, and tabnas/bnf#41 is the worked example:
# 0.1.12 emitted correctly, two front-ends discarded part of what it
# emitted, and 185 gbnf tests plus 1 ebnf test went red while every test
# in this repo stayed green.
#
# "Verify your work" has always asked for the sibling suites before an
# emit-pipeline change is done. This makes that one command, so it cannot
# be half-done: each sibling gets this tree the way npm would deliver it
# (`npm pack`, so only what "files" publishes), and its Go module is
# pointed at go/ through a workspace file in a temp dir.
#
# The Rust half runs each sibling's own Rust gate, ci/rust/run.sh, the
# script that sibling's rust.yml runs. Nothing is swapped in for it: every
# front-end crate depends on this one by PATH,
# `tabnas-bnf = { path = "../../bnf/rs" }`, so a sibling beside this
# checkout already builds against it -- provided this checkout IS the
# ../bnf those paths name. Under any other directory name they would build
# some other bnf, or none, so that is refused rather than graded. The gates
# also need the other sibling crates they name (parser and support, and
# abnf for gbnf) checked out beside them, and each gate says which is
# missing. This half is what the other two cannot see: bnf 0.1.20 and
# 0.1.21 made the Rust compiles of ipv6.abnf, jid.abnf and jsonpath.abnf
# five to ten times slower, which put them past the 60 s budget of abnf's
# Rust conformance sweep on CI (tabnas/abnf#95), and both shipped while
# this script, which had no Rust half, stayed green.
#
# Nothing in any checkout is left modified. The sibling's installed
# @tabnas/bnf is moved aside and restored on exit, failure included, and
# no go.mod is touched -- the workspace file lives outside every repo, the
# way AGENTS.md requires. Each Rust gate puts back any Cargo.lock that
# cargo rewrote.
#
# Only the bnf dependency is swapped. Everything else resolves the way it
# normally does for that sibling, so a failure here is about this tree and
# not about which parser happened to be linked in.
#
# WHAT THIS DOES NOT PROVE, on the Go side: that a sibling can RESOLVE
# this package. A workspace takes the module from disk and never consults
# go.sum, so a `require` naming a version that is missing, unpublished or
# unsummed passes here and fails in CI with `missing go.sum entry`. It
# did, on tabnas/ebnf#20 and tabnas/gbnf#25. Resolvability is a separate
# check and needs the release to exist first:
#
#     (cd <sibling>/go && GOWORK=off go build ./...)
#
# which is why "Releasing" asks for GOWORK=off as well as this script,
# and why a sibling bump lands AFTER the tag rather than beside it.
#
# Usage:
#   scripts/downstream.sh                    # abnf ebnf gbnf, every runtime
#   scripts/downstream.sh gbnf ebnf          # just those
#   RUNTIMES="ts go" scripts/downstream.sh   # without the Rust half
#
# RUNTIMES names the halves to run, from ts, go and rs; all three when it
# is unset. The Rust half without the TypeScript one still BUILDS each
# sibling's TypeScript against this tree, because abnf's Rust gate runs
# node over that build to measure the canonical half of each DIVERGENCE.md
# entry, and a stale dist/ would measure some other bnf.
#
# A sibling that is not checked out FAILS the run rather than being
# skipped: a gate that passes because it found nothing to run is worse
# than no gate. So does a requested half that a named sibling does not
# have (no go/, or no rs/Cargo.toml and ci/rust/run.sh), and a Rust half
# with no cargo to run it.
#
# What each sibling is sitting on is printed, and a checkout behind its
# own tracking ref is called out. Grading a feature branch is legitimate,
# so that is a warning rather than a refusal -- but it must be VISIBLE.
# A stale checkout does not fail, it grades the wrong tree: a `gbnf` left
# on a pre-#23 commit reported 554/185, the exact signature of #41, from
# a bnf tree that was fine. (Read from the tracking ref, so it is only as
# fresh as the last fetch in that checkout.)
#
# Each step prints a line when it starts, with its place in the run, and
# runs through scripts/heartbeat.sh, which prints a line every 30 s while
# it runs: a Go conformance suite or a Rust gate can be quiet for minutes,
# and a quiet step reads as a hung one.

set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
PEERS=(${@:-abnf ebnf gbnf})
RUNTIMES=${RUNTIMES:-ts go rs}
HEARTBEAT="$ROOT/scripts/heartbeat.sh"
WORK=$(mktemp -d)
SAVED=()

restore() {
  for s in ${SAVED+"${SAVED[@]}"}; do
    rm -rf "${s%%:*}"
    [ -d "${s#*:}" ] && mv "${s#*:}" "${s%%:*}"
  done
  rm -rf "$WORK"
}
trap restore EXIT

wants() {
  case " $RUNTIMES " in *" $1 "*) return 0 ;; esac
  return 1
}

n=0
for r in $RUNTIMES; do
  case "$r" in
    ts|go|rs) n=$((n + 1)) ;;
    *) echo "downstream: unknown runtime '$r' in RUNTIMES; use ts, go and rs" >&2; exit 1 ;;
  esac
done
if [ "$n" = 0 ]; then
  echo "downstream: RUNTIMES names no runtime; use ts, go and rs" >&2
  exit 1
fi

# What a sibling is sitting on, and whether that is behind what it last
# fetched.
state() {
  local at behind
  at="$(git -C "$1" rev-parse --abbrev-ref HEAD 2>/dev/null || echo '?')"
  at="$at $(git -C "$1" rev-parse --short HEAD 2>/dev/null || echo '?')"
  behind=$(git -C "$1" rev-list --count 'HEAD..@{upstream}' 2>/dev/null || true)
  if [ -n "$behind" ] && [ "$behind" != 0 ]; then
    at="$at, $behind BEHIND $(git -C "$1" rev-parse --abbrev-ref '@{upstream}')"
  fi
  echo "[$at]"
}

missing=()
for p in "${PEERS[@]}"; do
  [ -d "$ROOT/../$p" ] || missing+=("$p")
done
if [ ${#missing[@]} -gt 0 ]; then
  echo "downstream: not checked out: ${missing[*]}" >&2
  echo "downstream: clone them beside $(basename "$ROOT")/ or name the ones you have" >&2
  exit 1
fi

# A requested half that a sibling lacks would otherwise be skipped, and
# the run would still report that half green. Refuse it up front instead.
absent=()
for p in "${PEERS[@]}"; do
  if wants go && [ ! -d "$ROOT/../$p/go" ]; then absent+=("$p (go)"); fi
  if wants rs && { [ ! -f "$ROOT/../$p/rs/Cargo.toml" ] || [ ! -f "$ROOT/../$p/ci/rust/run.sh" ]; }; then
    absent+=("$p (rs)")
  fi
done
if [ ${#absent[@]} -gt 0 ]; then
  echo "downstream: nothing to run for ${absent[*]}" >&2
  echo "downstream: name the siblings that have it, or leave it out of RUNTIMES" >&2
  exit 1
fi

if wants rs; then
  if ! command -v cargo >/dev/null 2>&1; then
    echo "downstream: the Rust half needs cargo, and there is none on PATH" >&2
    echo 'downstream: install Rust, or leave the Rust half out: RUNTIMES="ts go"' >&2
    exit 1
  fi
  # Every sibling crate names this one as ../../bnf/rs. The siblings sit
  # at $ROOT/.., so that path reaches this tree only when this tree is
  # $ROOT/../bnf -- the same directory, not merely one with the name.
  if ! [ "$ROOT/../bnf" -ef "$ROOT" ]; then
    echo "downstream: the Rust half needs this tree at $(dirname "$ROOT")/bnf" >&2
    echo "downstream: the sibling crates depend on ../../bnf/rs by path, so from" >&2
    echo "downstream: $ROOT they would build some other bnf, or none" >&2
    exit 1
  fi
  for s in parser support; do
    if [ -d "$ROOT/../$s" ]; then
      echo "==> $s, a sibling crate of the Rust half $(state "$ROOT/../$s")"
    fi
  done
fi

# Every step this run will take, for the "step i of N" in each header.
total=0
for p in "${PEERS[@]}"; do
  if wants ts || wants rs; then total=$((total + 1)); fi
  if wants go; then total=$((total + 1)); fi
  if wants rs; then total=$((total + 1)); fi
done
if [ "$total" = 0 ]; then
  echo "downstream: nothing to run for ${PEERS[*]} in RUNTIMES=\"$RUNTIMES\"" >&2
  exit 1
fi
step=0
header() {
  step=$((step + 1))
  echo
  echo "==> $1, step $step of $total ($(( 100 * (step - 1) / total ))%)"
}

# Only the TypeScript half, and the TypeScript build the Rust half makes,
# need this tree packed. The Go half reads go/ through a workspace, so a
# Go-only run needs no Node at all.
if wants ts || wants rs; then
  echo "==> packing $(basename "$ROOT") as npm would publish it"
  (cd "$ROOT/ts" && npm run --silent build)
  TARBALL="$WORK/$(cd "$ROOT/ts" && npm pack --silent --pack-destination "$WORK")"
fi

# Every sibling runs even after one goes red. Stopping at the first
# failure would let a pre-existing break in an early sibling hide a real
# one in a later sibling -- which is the shape of the problem this script
# exists for.
failed=()

for p in "${PEERS[@]}"; do
  peer=$(cd "$ROOT/../$p" && pwd)
  # What the sibling is sitting on, shown in its first step's header.
  at=" $(state "$peer")"

  if wants ts || wants rs; then
    dest="$peer/ts/node_modules/@tabnas/bnf"
    if [ -d "$dest" ]; then
      mv "$dest" "$WORK/$p-bnf"
      SAVED+=("$dest:$WORK/$p-bnf")
    else
      SAVED+=("$dest:")
    fi
    mkdir -p "$dest"
    tar xzf "$TARBALL" --strip-components=1 -C "$dest"
  fi

  # Build explicitly. Most siblings rebuild in `pretest`, but abnf's
  # pretest fetches its conformance corpus instead, so `npm test` there
  # grades whatever dist/ was left lying around -- which looks exactly
  # like a failure in this tree.
  if wants ts; then
    header "$p (ts)$at"; at=""
    "$HEARTBEAT" "$p (ts)" bash -c 'cd "$1/ts" && npm run --silent build && npm test' _ "$peer" \
      || failed+=("$p (ts)")
  elif wants rs; then
    header "$p (ts build, for the Rust half)$at"; at=""
    "$HEARTBEAT" "$p (ts build)" bash -c 'cd "$1/ts" && npm run --silent build' _ "$peer" \
      || failed+=("$p (ts build)")
  fi

  if wants go; then
    header "$p (go)$at"; at=""
    printf 'go 1.24.7\n\nuse (\n\t%s/go\n\t%s/go\n)\n' "$ROOT" "$peer" > "$WORK/go.work"
    "$HEARTBEAT" "$p (go)" bash -c 'cd "$1/go" && GOWORK="$2" go test ./...' _ "$peer" "$WORK/go.work" \
      || failed+=("$p (go)")
  fi

  if wants rs; then
    header "$p (rs)$at"; at=""
    "$HEARTBEAT" "$p (rs)" bash "$peer/ci/rust/run.sh" || failed+=("$p (rs)")
  fi
done

echo
if [ ${#failed[@]} -gt 0 ]; then
  echo "==> downstream RED: ${failed[*]}" >&2
  exit 1
fi
echo "==> downstream green: ${PEERS[*]} ($RUNTIMES)"
