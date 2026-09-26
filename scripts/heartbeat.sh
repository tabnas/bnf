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
# Stopping it stops everything the command started. The command runs in a
# process group of its own, and an INT, TERM or HUP sent to this script,
# whether to its PID alone (a supervisor, a `kill`) or to its group
# (Ctrl-C), is passed on as a TERM to that whole group; the script then
# exits 130, 143 or 129. A signal to the wrapped `bash -c` alone would stop
# only that shell and leave the test run or install it started running.
# The wait below is what lets the signal in at once: bash runs a trap only
# after a foreground command returns, but a wait returns to it. perl's
# setpgrp makes the group, because setsid(1) is Linux only; without perl
# the command stays in this script's group, and a stop reaches the
# command and its direct children.
#
# The command, in its own group, is not in the terminal's foreground: it
# cannot read the terminal, and Ctrl-C reaches it through this script
# rather than directly. Nothing this runs reads the terminal.
#
# The heartbeat's sleep runs in the background and is waited on, so the
# signal that stops the heartbeat interrupts the wait, and its trap kills
# the sleep too: nothing is left holding this script's output open after
# the command has ended. The trap finds the sleep through `jobs -p` rather
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
cmd=""
beat=""
group=""

stop_beat() {
  if [ -n "$beat" ]; then
    kill "$beat" 2>/dev/null
    wait "$beat" 2>/dev/null
    beat=""
  fi
}
stop_all() {
  if [ -n "$cmd" ]; then
    if [ -n "$group" ]; then
      kill -TERM -- "-$cmd" 2>/dev/null
    else
      pkill -TERM -P "$cmd" 2>/dev/null
      kill -TERM "$cmd" 2>/dev/null
    fi
    wait "$cmd" 2>/dev/null
  fi
  stop_beat
  exit "$1"
}
trap 'stop_all 130' INT
trap 'stop_all 143' TERM
trap 'stop_all 129' HUP

(
  trap 'kill $(jobs -p) 2>/dev/null; exit 0' TERM
  while :; do
    sleep 30 </dev/null >/dev/null 2>&1 &
    wait $!
    echo "==> $label: still running, $(( $(date +%s) - start ))s, percentage unknown"
  done
) &
beat=$!

# `<&0` keeps the caller's stdin: a background command would otherwise get
# /dev/null.
if command -v perl >/dev/null 2>&1; then
  group=yes
  perl -e 'setpgrp(0, 0); exec { $ARGV[0] } @ARGV;
           print STDERR "heartbeat.sh: $ARGV[0]: $!\n"; exit 127' -- "$@" <&0 &
else
  "$@" <&0 &
fi
cmd=$!

rc=0
wait "$cmd" || rc=$?
stop_beat
exit "$rc"
