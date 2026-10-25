#!/usr/bin/env bash
# Usage: hack/coverage.sh <profile> <threshold-percent>
# Merges a multi-binary -coverpkg profile (a block counts as covered if any
# test binary covered it), prints per-package statement coverage for the core
# packages (everything except cmd/ and the test-only kubeenv/scenario
# helpers) and fails if the total is below the threshold.
set -euo pipefail
profile=$1
threshold=$2
module=$(go list -m)
awk -v module="$module/" -v threshold="$threshold" '
NR == 1 { next }                          # "mode:" line
{
  split($1, a, ":"); file = a[1]
  key = $1; stmts[key] = $2
  if ($3 > 0) hit[key] = 1
}
END {
  for (k in stmts) {
    split(k, a, ":"); file = a[1]
    pkg = file; sub(/\/[^\/]+$/, "", pkg); sub(module, "", pkg)
    if (pkg ~ /^cmd\// || pkg == "internal/kubeenv" || pkg == "internal/scenario") continue
    total[pkg] += stmts[k]; all += stmts[k]
    if (k in hit) { cov[pkg] += stmts[k]; allcov += stmts[k] }
  }
  n = 0
  for (p in total) names[++n] = p
  # simple insertion sort for stable output
  for (i = 2; i <= n; i++) { v = names[i]; j = i - 1; while (j > 0 && names[j] > v) { names[j+1] = names[j]; j-- } names[j+1] = v }
  for (i = 1; i <= n; i++) { p = names[i]; printf "  %-20s %6.1f%%  (%d/%d statements)\n", p, 100*cov[p]/total[p], cov[p], total[p] }
  pct = 100 * allcov / all
  printf "core coverage: %.1f%% (%d/%d statements), threshold %d%%\n", pct, allcov, all, threshold
  if (pct < threshold) { print "coverage below threshold"; exit 1 }
}' "$profile"
