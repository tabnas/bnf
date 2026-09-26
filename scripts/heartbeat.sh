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
# workflow run their long steps, clones and installs included, through
# this.
#
# The script becomes the command. It starts the heartbeat in the
# background and then execs COMMAND in its own place, so a signal sent to
# it reaches the command itself, with no shell in between to defer it
# until the command returns, and the exit status is the command's own. A
# `timeout` or a CI cancel that signals only this process still stops the
# command.
#
# The heartbeat watches its parent, which after the exec is the command.
# When the command ends, the heartbeat is handed to another parent,
# notices within a second, and stops, letting go of the output it shares:
# a caller reading that output through a pipe is not kept waiting, and
# nothing is left behind. A reparent is also what a reused PID cannot
# fake. Where `ps` cannot answer, it falls back to asking whether the
# command's PID still exists.

set -uo pipefail

if [ $# -lt 2 ]; then
  echo "usage: heartbeat.sh LABEL COMMAND [ARG...]" >&2
  exit 2
fi
label=$1
shift
parent=$$
start=$(date +%s)

(
  exec </dev/null
  me=$(exec sh -c 'echo $PPID')
  running() {
    local pp
    pp=$(ps -o ppid= -p "$me" 2>/dev/null | tr -d ' ')
    if [ -n "$pp" ]; then
      [ "$pp" = "$parent" ]
    else
      kill -0 "$parent" 2>/dev/null
    fi
  }
  last=$start
  while running; do
    sleep 1
    now=$(date +%s)
    if [ $((now - last)) -ge 30 ] && running; then
      echo "==> $label: still running, $((now - start))s, percentage unknown"
      last=$now
    fi
  done
) &

exec "$@"
