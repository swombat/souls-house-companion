#!/bin/sh
# Decide how a release run may package its build (used by .github/workflows/release.yml).
#   signed  : all six Apple signing secrets are present
#   unsigned: none of them is present
# Anything in between is a misconfiguration and fails. Asking for an early
# unsigned publication is only allowed when no secret is present at all.
# Inputs (environment): S1..S6 = the six secrets, EARLY_UNSIGNED = true|false.
# Output: prints signed or unsigned; exits 1 with a reason otherwise.
set -eu
present=0
for v in "${S1:-}" "${S2:-}" "${S3:-}" "${S4:-}" "${S5:-}" "${S6:-}"; do
  [ -n "$v" ] && present=$((present + 1))
done
early=${EARLY_UNSIGNED:-false}
case "$early" in true | false) ;; *) echo "EARLY_UNSIGNED must be true or false" >&2; exit 1 ;; esac
if [ "$present" -eq 6 ]; then
  if [ "$early" = true ]; then
    echo "early_unsigned was asked for, but the signing secrets exist: refusing to publish an unsigned build." >&2
    exit 1
  fi
  echo signed
elif [ "$present" -eq 0 ]; then
  echo unsigned
else
  echo "only $present of the 6 Apple signing secrets are set: refusing to build (set all six, or none)." >&2
  exit 1
fi
