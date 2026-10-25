#!/usr/bin/env bash
# Usage: hack/test-count.sh <go test -json output file>
# Counts top-level tests and subtests by outcome.
set -euo pipefail
f=$1
count() { grep -c "\"Action\":\"$1\".*\"Test\":\"$2" "$f" || true; }
top_pass=$(grep '"Action":"pass"' "$f" | grep '"Test":' | grep -vc '"Test":"[^"]*/' || true)
sub_pass=$(grep '"Action":"pass"' "$f" | grep -c '"Test":"[^"]*/' || true)
fail=$(grep '"Action":"fail"' "$f" | grep -c '"Test":' || true)
skip=$(grep '"Action":"skip"' "$f" | grep -c '"Test":' || true)
echo "${top_pass} tests + ${sub_pass} subtests passed, ${fail} failed, ${skip} skipped"
[ "$fail" -eq 0 ]
