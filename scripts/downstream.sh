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
# Usage:
#   scripts/downstream.sh              # abnf ebnf gbnf
#   scripts/downstream.sh gbnf ebnf    # just those
#
# A sibling that is not checked out FAILS the run rather than being
# skipped: a gate that passes because it found nothing to run is worse
# than no gate.

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
  echo "==> $p (ts)"
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
