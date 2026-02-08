#!/usr/bin/env bash
set -euo pipefail

# Benchmark regression gate for CLIProxyAPI.
# Compares key hot-path benchmarks against a committed baseline.

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BASELINE_FILE="${BENCH_BASELINE_FILE:-$ROOT_DIR/test/perf_baseline.env}"
BENCH_COUNT="${BENCH_COUNT:-3}"
BENCH_TIME="${BENCH_TIME:-1s}"
UPDATE_BASELINE="${BENCH_UPDATE_BASELINE:-0}"

NS_REGRESSION_PCT="${BENCH_NS_REGRESSION_PCT:-15}"
B_REGRESSION_PCT="${BENCH_B_REGRESSION_PCT:-20}"
ALLOCS_REGRESSION_PCT="${BENCH_ALLOCS_REGRESSION_PCT:-10}"

BENCHMARKS=(
  "BenchmarkUnifiedModelsCacheHitOpenAI"
  "BenchmarkUnifiedModelsCacheHitClaude"
  "BenchmarkMetricsHandler"
)

fail() {
  echo "BENCH_REGRESSION_FAIL: $*" >&2
  exit 1
}

median() {
  if [[ $# -eq 0 ]]; then
    echo "0"
    return 0
  fi
  printf '%s\n' "$@" | sort -g | awk '
    { values[++n] = $1 }
    END {
      if (n == 0) { print "0"; exit }
      mid = int((n + 1) / 2)
      if (n % 2 == 1) {
        print values[mid]
      } else {
        printf "%.6f\n", (values[mid] + values[mid + 1]) / 2
      }
    }
  '
}

extract_metric_values() {
  local output="$1"
  local metric="$2"
  # Benchmark output can be interleaved with proxy logs; parse only canonical metric lines.
  awk -v target="$metric" '
    /ns\/op/ && /B\/op/ && /allocs\/op/ {
      for (i = 1; i <= NF; i++) {
        if ($i == target && i > 1) {
          print $(i - 1)
        }
      }
    }
  ' <<<"$output"
}

read_metric_from_baseline() {
  local key="$1"
  if [[ ! -f "$BASELINE_FILE" ]]; then
    fail "baseline file not found: $BASELINE_FILE (use BENCH_UPDATE_BASELINE=1 to create)"
  fi
  local value
  if ! value="$(awk -F= -v k="$key" '$1==k { print $2; found=1; exit } END { if (!found) exit 1 }' "$BASELINE_FILE")"; then
    fail "missing baseline key: ${key} in $BASELINE_FILE"
  fi
  echo "$value"
}

is_regression() {
  local observed="$1"
  local baseline="$2"
  local pct="$3"
  awk -v obs="$observed" -v base="$baseline" -v pct="$pct" '
    BEGIN {
      if (base <= 0) { exit 1 }
      limit = base * (1.0 + pct / 100.0)
      if (obs > limit) exit 0
      exit 1
    }
  '
}

format_float() {
  awk -v x="$1" 'BEGIN { printf "%.6f", x }'
}

run_benchmark() {
  local benchmark_name="$1"
  local output
  output="$(
    cd "$ROOT_DIR"
    go test ./internal/api -run '^$' -bench "^${benchmark_name}$" -benchmem -count "$BENCH_COUNT" -benchtime "$BENCH_TIME"
  )"

  mapfile -t ns_values < <(extract_metric_values "$output" "ns/op")
  mapfile -t b_values < <(extract_metric_values "$output" "B/op")
  mapfile -t alloc_values < <(extract_metric_values "$output" "allocs/op")

  if [[ "${#ns_values[@]}" -eq 0 || "${#b_values[@]}" -eq 0 || "${#alloc_values[@]}" -eq 0 ]]; then
    fail "failed to parse benchmark output for $benchmark_name"
  fi

  local ns_median b_median alloc_median
  ns_median="$(median "${ns_values[@]}")"
  b_median="$(median "${b_values[@]}")"
  alloc_median="$(median "${alloc_values[@]}")"

  echo "${benchmark_name}|${ns_median}|${b_median}|${alloc_median}"
}

echo "BENCH_REGRESSION: running benchmarks (count=$BENCH_COUNT benchtime=$BENCH_TIME)"
results=()
for benchmark_name in "${BENCHMARKS[@]}"; do
  results+=("$(run_benchmark "$benchmark_name")")
done

if [[ "$UPDATE_BASELINE" == "1" ]]; then
  mkdir -p "$(dirname "$BASELINE_FILE")"
  {
    echo "# CLIProxy benchmark baseline (median of count=$BENCH_COUNT, benchtime=$BENCH_TIME)"
    echo "# Update with: BENCH_UPDATE_BASELINE=1 ./test/bench_regression.sh"
    for row in "${results[@]}"; do
      IFS='|' read -r name ns b alloc <<<"$row"
      echo "${name}.ns_per_op=$(format_float "$ns")"
      echo "${name}.b_per_op=$(format_float "$b")"
      echo "${name}.allocs_per_op=$(format_float "$alloc")"
    done
  } > "$BASELINE_FILE"
  echo "BENCH_REGRESSION: baseline updated at $BASELINE_FILE"
  exit 0
fi

echo "BENCH_REGRESSION_RESULT:"
has_regression=0
for row in "${results[@]}"; do
  IFS='|' read -r name ns b alloc <<<"$row"

  base_ns="$(read_metric_from_baseline "${name}.ns_per_op")"
  base_b="$(read_metric_from_baseline "${name}.b_per_op")"
  base_alloc="$(read_metric_from_baseline "${name}.allocs_per_op")"

  ns_status="OK"
  b_status="OK"
  alloc_status="OK"

  if is_regression "$ns" "$base_ns" "$NS_REGRESSION_PCT"; then
    ns_status="REGRESSION"
    has_regression=1
  fi
  if is_regression "$b" "$base_b" "$B_REGRESSION_PCT"; then
    b_status="REGRESSION"
    has_regression=1
  fi
  if is_regression "$alloc" "$base_alloc" "$ALLOCS_REGRESSION_PCT"; then
    alloc_status="REGRESSION"
    has_regression=1
  fi

  echo "  ${name} ns/op=${ns} (base=${base_ns}, tol=+${NS_REGRESSION_PCT}%, ${ns_status}) b/op=${b} (base=${base_b}, tol=+${B_REGRESSION_PCT}%, ${b_status}) allocs/op=${alloc} (base=${base_alloc}, tol=+${ALLOCS_REGRESSION_PCT}%, ${alloc_status})"
done

if [[ "$has_regression" -ne 0 ]]; then
  fail "regression detected against baseline ($BASELINE_FILE)"
fi

echo "BENCH_REGRESSION_PASS"
