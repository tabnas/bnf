#!/usr/bin/env bash
#
# heartbeat.sh — run a command, and say every 30 s that it is still running.
#
#   scripts/heartbeat.sh LABEL COMMAND [ARG...]
#
# The command's own output streams as usual, and the exit status is the
# command's. While it runs, one line goes out every 30 s:
#
#   ==> LABEL: still running, 90s, percentage unknown
#
# A step that is quiet for minutes reads as a hung one (AGENTS.md, the
# progress principle), so scripts/downstream.sh and the Downstream
# workflow run their long steps, installs included, through this.
#
# The heartbeat's sleep runs in the background and is waited on, so the
# signal that stops the heartbeat interrupts the wait, and its trap kills
# the sleep too. A sleep left behind would hold this script's output open
# after the command had ended: a caller reading that output through a pipe
# would wait up to 30 s more for nothing, and every step would leave one
# more stray process. The trap finds the sleep through `jobs -p` rather
# than a saved PID, because a command that ends at once can stop the
# heartbeat between starting the sleep and saving its PID.

set -uo pipefail

if [ $# -lt 2 ]; then
  echo "usage: heartbeat.sh LABEL COMMAND [ARG...]" >&2
  exit 2
fi
label=$1
shift
start=$(date +%s)

(
  trap 'kill $(jobs -p) 2>/dev/null; exit 0' TERM
  while :; do
    sleep 30 </dev/null >/dev/null 2>&1 &
    wait $!
    echo "==> $label: still running, $(( $(date +%s) - start ))s, percentage unknown"
  done
) &
beat=$!

stop() {
  kill "$beat" 2>/dev/null
  wait "$beat" 2>/dev/null
}
# An interrupted run stops the heartbeat as well, rather than leaving it
# looping on its own.
trap 'stop; exit 130' INT
trap 'stop; exit 143' TERM

rc=0
"$@" || rc=$?
stop
exit "$rc"
