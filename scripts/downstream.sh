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
# Nothing in any checkout is left modified. The sibling's installed
# @tabnas/bnf is moved aside and restored on exit, failure included, and
# no go.mod is touched -- the workspace file lives outside every repo, the
# way AGENTS.md requires.
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
#   scripts/downstream.sh              # abnf ebnf gbnf
#   scripts/downstream.sh gbnf ebnf    # just those
#
# A sibling that is not checked out FAILS the run rather than being
# skipped: a gate that passes because it found nothing to run is worse
# than no gate.
#
# What each sibling is sitting on is printed, and a checkout behind its
# own tracking ref is called out. Grading a feature branch is legitimate,
# so that is a warning rather than a refusal -- but it must be VISIBLE.
# A stale checkout does not fail, it grades the wrong tree: a `gbnf` left
# on a pre-#23 commit reported 554/185, the exact signature of #41, from
# a bnf tree that was fine. (Read from the tracking ref, so it is only as
# fresh as the last fetch in that checkout.)

set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
PEERS=(${@:-abnf ebnf gbnf})
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

echo "==> packing $(basename "$ROOT") as npm would publish it"
(cd "$ROOT/ts" && npm run --silent build)
TARBALL="$WORK/$(cd "$ROOT/ts" && npm pack --silent --pack-destination "$WORK")"

# Every sibling runs even after one goes red. Stopping at the first
# failure would let a pre-existing break in an early sibling hide a real
# one in a later sibling -- which is the shape of the problem this script
# exists for.
failed=()

for p in "${PEERS[@]}"; do
  peer=$(cd "$ROOT/../$p" && pwd)

  dest="$peer/ts/node_modules/@tabnas/bnf"
  if [ -d "$dest" ]; then
    mv "$dest" "$WORK/$p-bnf"
    SAVED+=("$dest:$WORK/$p-bnf")
  else
    SAVED+=("$dest:")
  fi
  mkdir -p "$dest"
  tar xzf "$TARBALL" --strip-components=1 -C "$dest"

  echo
  echo "==> $p (ts) $(state "$peer")"
  # Build explicitly. Most siblings rebuild in `pretest`, but abnf's
  # pretest fetches its conformance corpus instead, so `npm test` there
  # grades whatever dist/ was left lying around -- which looks exactly
  # like a failure in this tree.
  (cd "$peer/ts" && npm run --silent build && npm test) || failed+=("$p (ts)")

  if [ -d "$peer/go" ]; then
    echo
    echo "==> $p (go)"
    printf 'go 1.24.7\n\nuse (\n\t%s/go\n\t%s/go\n)\n' "$ROOT" "$peer" > "$WORK/go.work"
    (cd "$peer/go" && GOWORK="$WORK/go.work" go test ./...) || failed+=("$p (go)")
  fi
done

echo
if [ ${#failed[@]} -gt 0 ]; then
  echo "==> downstream RED: ${failed[*]}" >&2
  exit 1
fi
echo "==> downstream green: ${PEERS[*]}"
