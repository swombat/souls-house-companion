#!/bin/sh
# none / partial / all secrets x early off / on, against scripts/release-mode.sh.
set -eu
here=$(dirname "$0")
fail=0
check() { # expected-output-or-FAIL count early
  expected=$1 count=$2 early=$3
  env -i PATH="$PATH" EARLY_UNSIGNED="$early" \
    $(i=1; while [ "$i" -le 6 ]; do if [ "$i" -le "$count" ]; then printf 'S%s=x ' "$i"; fi; i=$((i + 1)); done) \
    sh "$here/release-mode.sh" > /tmp/release-mode.out 2>/dev/null && got=$(cat /tmp/release-mode.out) || got=FAIL
  if [ "$got" != "$expected" ]; then
    echo "secrets=$count early=$early: expected $expected, got $got" >&2; fail=1
  fi
}
check unsigned 0 false
check unsigned 0 true
for n in 1 2 3 4 5; do check FAIL "$n" false; check FAIL "$n" true; done
check signed 6 false
check FAIL 6 true
[ "$fail" -eq 0 ] && echo "release-mode: 14 cases ok"
exit "$fail"
