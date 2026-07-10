#!/usr/bin/env bash
set -euo pipefail

# 24/7 soak test for CLIProxyAPI.
# Defaults to a 24h run, but accepts env overrides for shorter validation windows.

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN_PATH="${SOAK_BINARY:-$ROOT_DIR/.tmp/soak-cli-proxy-api}"
CODEX_FAKE_PORT="${SOAK_CODEX_FAKE_PORT:-0}"
CODEX_FAKE_URL=""

SOAK_DURATION_SEC="${SOAK_DURATION_SEC:-86400}"
REQUEST_INTERVAL_SEC="${SOAK_REQUEST_INTERVAL_SEC:-0.05}"
AUTH_CHURN_INTERVAL_SEC="${SOAK_AUTH_CHURN_INTERVAL_SEC:-0.20}"
SOAK_ENABLE_PPROF_DIFF="${SOAK_ENABLE_PPROF_DIFF:-1}"
SOAK_MAX_HEAP_DELTA_BYTES="${SOAK_MAX_HEAP_DELTA_BYTES:-67108864}"
SOAK_PPROF_DIFF_ARTIFACT="${SOAK_PPROF_DIFF_ARTIFACT:-$ROOT_DIR/.tmp/soak_pprof_diff.txt}"

MAX_MODELS_P95_MS="${SOAK_MAX_MODELS_P95_MS:-400}"
MAX_CODEX_P95_MS="${SOAK_MAX_CODEX_P95_MS:-600}"
MAX_ERROR_RATE_PCT="${SOAK_MAX_ERROR_RATE_PCT:-1.0}"
MAX_GOROUTINE_DELTA="${SOAK_MAX_GOROUTINE_DELTA:-300}"
MAX_OPEN_FDS_DELTA="${SOAK_MAX_OPEN_FDS_DELTA:-400}"

SOAK_KEY="sk-soak-local"

fail() {
  echo "SOAK_FAIL: $*" >&2
  exit 1
}

is_truthy() {
  case "${1:-}" in
    1|true|TRUE|True|yes|YES|on|ON)
      return 0
      ;;
    *)
      return 1
      ;;
  esac
}

pick_port() {
  local candidate
  for _ in $(seq 1 200); do
    candidate=$((22000 + RANDOM % 18000))
    if ! ss -ltn | grep -qE ":${candidate}\\b"; then
      echo "$candidate"
      return 0
    fi
  done
  fail "could not find free TCP port"
}

metric_value() {
  local metrics_url="$1"
  local metric_name="$2"
  curl -fsS "$metrics_url" | awk -v name="$metric_name" '$1==name {print $2; found=1; exit} END{if(!found) print 0}'
}

calc_p95_ms() {
  local file="$1"
  local n idx
  n="$(wc -l < "$file" | tr -d ' ')"
  if [[ -z "$n" || "$n" -eq 0 ]]; then
    echo 0
    return 0
  fi
  idx=$(( (95 * n + 99) / 100 ))
  sort -n "$file" | awk -v target="$idx" 'NR==target {print; found=1; exit} END {if (!found) print 0}'
}

calc_avg_ms() {
  local file="$1"
  awk '{sum+=$1; n++} END {if (n==0) {print 0} else {printf "%.0f", sum/n}}' "$file"
}

wait_pprof_ready() {
  local pprof_url="$1"
  for _ in $(seq 1 200); do
    if curl -fsS -o /dev/null "${pprof_url}/debug/pprof/heap?gc=1" 2>/dev/null; then
      return 0
    fi
    sleep 0.05
  done
  return 1
}

capture_heap_profile() {
  local pprof_url="$1"
  local out_file="$2"
  curl -fsS "${pprof_url}/debug/pprof/heap?gc=1" -o "$out_file"
}

extract_pprof_total_human() {
  local top_file="$1"
  awk '
    / total$/ {
      for (i = 1; i <= NF; i++) {
        if ($i == "of" && (i + 1) <= NF) {
          value = $(i + 1)
          gsub(",", "", value)
          print value
          exit
        }
      }
    }
  ' "$top_file"
}

human_size_to_bytes() {
  local human="$1"
  awk -v v="$human" '
    BEGIN {
      if (v == "") {
        print 0
        exit
      }
      unit = "B"
      num = v
      if (v ~ /[kMGT]B$/) {
        unit = substr(v, length(v)-1, 2)
        num = substr(v, 1, length(v)-2)
      } else if (v ~ /B$/) {
        unit = "B"
        num = substr(v, 1, length(v)-1)
      } else {
        print 0
        exit
      }
      num = num + 0.0
      factor = 1
      if (unit == "kB") factor = 1024
      else if (unit == "MB") factor = 1024 * 1024
      else if (unit == "GB") factor = 1024 * 1024 * 1024
      else if (unit == "TB") factor = 1024 * 1024 * 1024 * 1024
      printf "%.0f\n", num * factor
    }
  '
}

mkdir -p "$ROOT_DIR/.tmp"
echo "SOAK: building binary at $BIN_PATH"
go build -o "$BIN_PATH" "$ROOT_DIR/cmd/server"

PORT="$(pick_port)"
PPROF_PORT="$(pick_port)"
if [[ "$CODEX_FAKE_PORT" == "0" ]]; then
  CODEX_FAKE_PORT="$(pick_port)"
fi
CODEX_FAKE_URL="http://127.0.0.1:${CODEX_FAKE_PORT}"
PPROF_URL="http://127.0.0.1:${PPROF_PORT}"
PPROF_ENABLED_BOOL=false
if is_truthy "$SOAK_ENABLE_PPROF_DIFF"; then
  PPROF_ENABLED_BOOL=true
fi

TMP_DIR="$(mktemp -d /tmp/cliproxy-soak.XXXXXX)"
AUTH_DIR="$TMP_DIR/auth"
CONFIG_FILE="$TMP_DIR/config.yaml"
LOG_FILE="$TMP_DIR/run.log"
LAT_FILE="$TMP_DIR/models_latency_ms.txt"
CODEX_LAT_FILE="$TMP_DIR/codex_latency_ms.txt"
PPROF_START="$TMP_DIR/heap_start.pb.gz"
PPROF_END="$TMP_DIR/heap_end.pb.gz"
PPROF_START_TOP="$TMP_DIR/heap_start.top.txt"
PPROF_END_TOP="$TMP_DIR/heap_end.top.txt"
PPROF_DIFF_TOP="$TMP_DIR/heap_diff.top.txt"
MODELS_URL="http://127.0.0.1:${PORT}/v1/models"
CODEX_URL="http://127.0.0.1:${PORT}/v1/responses"
METRICS_URL="http://127.0.0.1:${PORT}/metrics"

PID=""
CHURN_PID=""
FAKE_PID=""
cleanup() {
  if [[ -n "$CHURN_PID" ]]; then
    kill "$CHURN_PID" >/dev/null 2>&1 || true
    wait "$CHURN_PID" >/dev/null 2>&1 || true
  fi
  if [[ -n "$FAKE_PID" ]]; then
    kill "$FAKE_PID" >/dev/null 2>&1 || true
    wait "$FAKE_PID" >/dev/null 2>&1 || true
  fi
  if [[ -n "$PID" ]]; then
    kill "$PID" >/dev/null 2>&1 || true
    wait "$PID" >/dev/null 2>&1 || true
  fi
  rm -rf "$TMP_DIR"
}
trap cleanup EXIT

mkdir -p "$AUTH_DIR"
echo '{"type":"demo","seq":0}' > "$AUTH_DIR/soak.json"
python3 "$ROOT_DIR/test/codex_fake_server.py" "$CODEX_FAKE_PORT" > "$TMP_DIR/codex_fake.log" 2>&1 &
FAKE_PID=$!

cat > "$CONFIG_FILE" <<YAML
port: ${PORT}
host: 127.0.0.1
auth-dir: ${AUTH_DIR}
api-keys:
  - ${SOAK_KEY}
remote-management:
  allow-remote: false
  secret-key: ''
  disable-control-panel: true
debug: false
commercial-mode: true
logging-to-file: false
usage-statistics-enabled: false
request-log: false
pprof:
  enable: ${PPROF_ENABLED_BOOL}
  addr: 127.0.0.1:${PPROF_PORT}
models-cache:
  strategy: versioned
upstream-http:
  max-idle-conns: 256
  max-idle-conns-per-host: 64
  max-conns-per-host: 128
codex-api-key:
  - api-key: codex-soak
    base-url: ${CODEX_FAKE_URL}
    non-stream-strategy: compact_auto
    models:
      - name: gpt-5
        alias: gpt-5
YAML

echo "SOAK: starting proxy on 127.0.0.1:${PORT}"
"$BIN_PATH" -config "$CONFIG_FILE" > "$LOG_FILE" 2>&1 &
PID=$!

for _ in $(seq 1 200); do
  code="$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer ${SOAK_KEY}" "$MODELS_URL" 2>/dev/null || echo 000)"
  if [[ "$code" == "200" ]]; then
    break
  fi
  sleep 0.05
done
if [[ "${code:-000}" != "200" ]]; then
  tail -n 120 "$LOG_FILE" >&2 || true
  fail "proxy did not become ready"
fi

if [[ "$PPROF_ENABLED_BOOL" == "true" ]]; then
  wait_pprof_ready "$PPROF_URL" || fail "pprof did not become ready at ${PPROF_URL}"
  capture_heap_profile "$PPROF_URL" "$PPROF_START" || fail "failed to capture initial heap profile"
fi

start_goroutines="$(metric_value "$METRICS_URL" "cliproxy_process_goroutines_current")"
start_open_fds="$(metric_value "$METRICS_URL" "cliproxy_process_open_fds_current")"

(
  seq_id=1000
  while true; do
    seq_id=$((seq_id + 1))
    printf '{"type":"demo","seq":%d}\n' "$seq_id" > "$AUTH_DIR/soak.json"
    sleep "$AUTH_CHURN_INTERVAL_SEC"
  done
) &
CHURN_PID=$!

: > "$LAT_FILE"
: > "$CODEX_LAT_FILE"
total_requests=0
codex_requests=0
error_requests=0
end_ts=$((SECONDS + SOAK_DURATION_SEC))
while [[ $SECONDS -lt $end_ts ]]; do
  total_requests=$((total_requests + 1))
  start_ms="$(date +%s%3N)"
  code="$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer ${SOAK_KEY}" "$MODELS_URL" 2>/dev/null || echo 000)"
  end_ms="$(date +%s%3N)"
  if [[ "$code" == "200" ]]; then
    echo $((end_ms - start_ms)) >> "$LAT_FILE"
  else
    error_requests=$((error_requests + 1))
  fi
  if (( total_requests % 10 == 0 )); then
    codex_requests=$((codex_requests + 1))
    codex_start_ms="$(date +%s%3N)"
    codex_code="$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer ${SOAK_KEY}" -H "Content-Type: application/json" -X POST "$CODEX_URL" --data '{"model":"gpt-5","input":"hi"}' 2>/dev/null || echo 000)"
    codex_end_ms="$(date +%s%3N)"
    if [[ "$codex_code" == "200" ]]; then
      echo $((codex_end_ms - codex_start_ms)) >> "$CODEX_LAT_FILE"
    else
      error_requests=$((error_requests + 1))
    fi
  fi
  sleep "$REQUEST_INTERVAL_SEC"
done

end_goroutines="$(metric_value "$METRICS_URL" "cliproxy_process_goroutines_current")"
end_open_fds="$(metric_value "$METRICS_URL" "cliproxy_process_open_fds_current")"

avg_ms="$(calc_avg_ms "$LAT_FILE")"
p95_ms="$(calc_p95_ms "$LAT_FILE")"
[[ -s "$CODEX_LAT_FILE" ]] || fail "codex latency sample file is empty"
codex_p95_ms="$(calc_p95_ms "$CODEX_LAT_FILE")"
total_probe_requests=$((total_requests + codex_requests))
error_rate_pct="$(awk -v e="$error_requests" -v t="$total_probe_requests" 'BEGIN {if (t==0) print "0.00"; else printf "%.2f", (e*100.0)/t}')"
goroutine_delta="$(awk -v s="$start_goroutines" -v e="$end_goroutines" 'BEGIN {printf "%.0f", e-s}')"
open_fds_delta="$(awk -v s="$start_open_fds" -v e="$end_open_fds" 'BEGIN {printf "%.0f", e-s}')"
heap_start_bytes=0
heap_end_bytes=0
heap_delta_bytes=0

if [[ "$PPROF_ENABLED_BOOL" == "true" ]]; then
  capture_heap_profile "$PPROF_URL" "$PPROF_END" || fail "failed to capture final heap profile"
  go tool pprof -top -sample_index=inuse_space "$PPROF_START" > "$PPROF_START_TOP"
  go tool pprof -top -sample_index=inuse_space "$PPROF_END" > "$PPROF_END_TOP"
  go tool pprof -top -sample_index=inuse_space -diff_base="$PPROF_START" "$PPROF_END" > "$PPROF_DIFF_TOP"

  start_total_human="$(extract_pprof_total_human "$PPROF_START_TOP")"
  end_total_human="$(extract_pprof_total_human "$PPROF_END_TOP")"
  [[ -n "$start_total_human" ]] || fail "failed to parse start heap total from pprof output"
  [[ -n "$end_total_human" ]] || fail "failed to parse end heap total from pprof output"
  heap_start_bytes="$(human_size_to_bytes "$start_total_human")"
  heap_end_bytes="$(human_size_to_bytes "$end_total_human")"
  heap_delta_bytes="$(awk -v s="$heap_start_bytes" -v e="$heap_end_bytes" 'BEGIN {printf "%.0f", e-s}')"

  mkdir -p "$(dirname "$SOAK_PPROF_DIFF_ARTIFACT")"
  {
    echo "# soak pprof diff"
    echo "start_total_human=${start_total_human}"
    echo "end_total_human=${end_total_human}"
    echo "heap_start_bytes=${heap_start_bytes}"
    echo "heap_end_bytes=${heap_end_bytes}"
    echo "heap_delta_bytes=${heap_delta_bytes}"
    echo "---"
    cat "$PPROF_DIFF_TOP"
  } > "$SOAK_PPROF_DIFF_ARTIFACT"
fi

echo "SOAK_RESULT duration_sec=${SOAK_DURATION_SEC} requests=${total_requests} errors=${error_requests} error_rate_pct=${error_rate_pct} models_avg_ms=${avg_ms} models_p95_ms=${p95_ms} codex_p95_ms=${codex_p95_ms} goroutine_delta=${goroutine_delta} open_fds_delta=${open_fds_delta} heap_delta_bytes=${heap_delta_bytes}"
echo "SOAK_LIMITS models_p95<=${MAX_MODELS_P95_MS} codex_p95<=${MAX_CODEX_P95_MS} err_rate<=${MAX_ERROR_RATE_PCT}% goroutine_delta<=${MAX_GOROUTINE_DELTA} open_fds_delta<=${MAX_OPEN_FDS_DELTA} heap_delta<=${SOAK_MAX_HEAP_DELTA_BYTES}"
if [[ "$PPROF_ENABLED_BOOL" == "true" ]]; then
  echo "SOAK_PPROF_DIFF artifact=${SOAK_PPROF_DIFF_ARTIFACT}"
fi

awk -v p95="$p95_ms" -v max="$MAX_MODELS_P95_MS" 'BEGIN{if (p95<=max) exit 0; exit 1}' || fail "models p95 too high: ${p95_ms}ms > ${MAX_MODELS_P95_MS}ms"
awk -v p95="$codex_p95_ms" -v max="$MAX_CODEX_P95_MS" 'BEGIN{if (p95<=max) exit 0; exit 1}' || fail "codex p95 too high: ${codex_p95_ms}ms > ${MAX_CODEX_P95_MS}ms"
awk -v err="$error_rate_pct" -v max="$MAX_ERROR_RATE_PCT" 'BEGIN{if (err<=max) exit 0; exit 1}' || fail "error rate too high: ${error_rate_pct}% > ${MAX_ERROR_RATE_PCT}%"
awk -v d="$goroutine_delta" -v max="$MAX_GOROUTINE_DELTA" 'BEGIN{if (d<=max) exit 0; exit 1}' || fail "goroutine delta too high: ${goroutine_delta} > ${MAX_GOROUTINE_DELTA}"
awk -v d="$open_fds_delta" -v max="$MAX_OPEN_FDS_DELTA" 'BEGIN{if (d<=max) exit 0; exit 1}' || fail "open fds delta too high: ${open_fds_delta} > ${MAX_OPEN_FDS_DELTA}"
if [[ "$PPROF_ENABLED_BOOL" == "true" ]]; then
  awk -v d="$heap_delta_bytes" -v max="$SOAK_MAX_HEAP_DELTA_BYTES" 'BEGIN{if (d<=max) exit 0; exit 1}' || fail "heap delta too high: ${heap_delta_bytes} > ${SOAK_MAX_HEAP_DELTA_BYTES}"
fi

echo "SOAK_PASS"
