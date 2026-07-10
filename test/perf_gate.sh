#!/usr/bin/env bash
set -euo pipefail

# P0 performance gate for CLIProxyAPI.
# This script is self-contained and does not require upstream providers.

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN_PATH="${PERF_GATE_BINARY:-$ROOT_DIR/.tmp/perf-gate-cli-proxy-api}"
CODEX_FAKE_PORT="${PERF_GATE_CODEX_FAKE_PORT:-0}"
CODEX_FAKE_URL=""

IDLE_WINDOW_SEC="${PERF_GATE_IDLE_WINDOW_SEC:-3}"
BURST_WRITES="${PERF_GATE_BURST_WRITES:-20}"
BURST_WRITE_INTERVAL_SEC="${PERF_GATE_BURST_WRITE_INTERVAL_SEC:-0.03}"
BURST_SETTLE_SEC="${PERF_GATE_BURST_SETTLE_SEC:-2}"
LATENCY_SAMPLE_COUNT="${PERF_GATE_LATENCY_SAMPLE_COUNT:-120}"
LATENCY_CHURN_SEC="${PERF_GATE_LATENCY_CHURN_SEC:-4}"
LATENCY_CHURN_INTERVAL_SEC="${PERF_GATE_LATENCY_CHURN_INTERVAL_SEC:-0.03}"

MAX_IDLE_UPDATES="${PERF_GATE_MAX_IDLE_UPDATES:-2}"
MAX_BURST_UPDATES="${PERF_GATE_MAX_BURST_UPDATES:-3}"
MAX_MODELS_AVG_MS="${PERF_GATE_MAX_MODELS_AVG_MS:-150}"
MAX_MODELS_P95_MS="${PERF_GATE_MAX_MODELS_P95_MS:-300}"
MAX_CODEX_P95_MS="${PERF_GATE_MAX_CODEX_P95_MS:-500}"

PERF_KEY="sk-perf-gate-local"
SUMMARY_PATTERN="server clients and configuration updated"

fail() {
  echo "PERF_GATE_FAIL: $*" >&2
  exit 1
}

pick_port() {
  local candidate
  for _ in $(seq 1 100); do
    candidate=$((20000 + RANDOM % 20000))
    if ! ss -ltn | grep -qE ":${candidate}\\b"; then
      echo "$candidate"
      return 0
    fi
  done
  fail "could not find a free TCP port"
}

count_updates() {
  local file="$1"
  if [[ ! -f "$file" ]]; then
    echo 0
    return 0
  fi
  local value
  value="$(grep -cE "$SUMMARY_PATTERN" "$file" 2>/dev/null || true)"
  value="${value//$'\n'/}"
  if [[ -z "$value" ]]; then
    value=0
  fi
  echo "$value"
}

wait_ready() {
  local url="$1"
  for _ in $(seq 1 160); do
    if curl -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer ${PERF_KEY}" "$url" 2>/dev/null | grep -qE '^200$'; then
      return 0
    fi
    sleep 0.05
  done
  return 1
}

measure_models_latency_ms() {
  local url="$1"
  local out_file="$2"
  : > "$out_file"
  local start_ms end_ms elapsed_ms code
  for _ in $(seq 1 "$LATENCY_SAMPLE_COUNT"); do
    start_ms="$(date +%s%3N)"
    code="$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer ${PERF_KEY}" "$url" 2>/dev/null || echo 000)"
    end_ms="$(date +%s%3N)"
    if [[ "$code" != "200" ]]; then
      fail "/v1/models returned HTTP $code during latency sampling"
    fi
    elapsed_ms=$((end_ms - start_ms))
    echo "$elapsed_ms" >> "$out_file"
  done
}

measure_codex_latency_ms() {
  local url="$1"
  local out_file="$2"
  : > "$out_file"
  local start_ms end_ms elapsed_ms code
  local body='{"model":"gpt-5","input":"hi"}'
  for _ in $(seq 1 "$LATENCY_SAMPLE_COUNT"); do
    start_ms="$(date +%s%3N)"
    code="$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer ${PERF_KEY}" -H "Content-Type: application/json" -X POST "$url" --data "$body" 2>/dev/null || echo 000)"
    end_ms="$(date +%s%3N)"
    if [[ "$code" != "200" ]]; then
      fail "Codex /v1/responses returned HTTP $code during latency sampling"
    fi
    elapsed_ms=$((end_ms - start_ms))
    echo "$elapsed_ms" >> "$out_file"
  done
}

calc_avg_ms() {
  local file="$1"
  awk '{sum+=$1; n++} END {if (n==0) {print 0} else {printf "%.0f", sum/n}}' "$file"
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

mkdir -p "$ROOT_DIR/.tmp"
echo "PERF_GATE: building binary at $BIN_PATH"
go build -o "$BIN_PATH" "$ROOT_DIR/cmd/server"

PORT="$(pick_port)"
if [[ "$CODEX_FAKE_PORT" == "0" ]]; then
  CODEX_FAKE_PORT="$(pick_port)"
fi
CODEX_FAKE_URL="http://127.0.0.1:${CODEX_FAKE_PORT}"
TMP_DIR="$(mktemp -d /tmp/cliproxy-perf-gate.XXXXXX)"
AUTH_DIR="$TMP_DIR/auth"
CONFIG_FILE="$TMP_DIR/config.yaml"
LOG_FILE="$TMP_DIR/run.log"
LAT_FILE="$TMP_DIR/models_latency_ms.txt"
CODEX_LAT_FILE="$TMP_DIR/codex_latency_ms.txt"
MODELS_URL="http://127.0.0.1:${PORT}/v1/models"
CODEX_URL="http://127.0.0.1:${PORT}/v1/responses"

PID=""
FAKE_PID=""
cleanup() {
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
echo '{"type":"demo","seq":0}' > "$AUTH_DIR/perf.json"
python3 "$ROOT_DIR/test/codex_fake_server.py" "$CODEX_FAKE_PORT" > "$TMP_DIR/codex_fake.log" 2>&1 &
FAKE_PID=$!

cat > "$CONFIG_FILE" <<YAML
port: ${PORT}
host: 127.0.0.1
auth-dir: ${AUTH_DIR}
api-keys:
  - ${PERF_KEY}
remote-management:
  allow-remote: false
  secret-key: ''
  disable-control-panel: true
debug: false
commercial-mode: true
logging-to-file: false
usage-statistics-enabled: false
request-log: false
codex-api-key:
  - api-key: codex-perf
    base-url: ${CODEX_FAKE_URL}
    non-stream-strategy: compact_auto
    models:
      - name: gpt-5
        alias: gpt-5
YAML

echo "PERF_GATE: starting proxy on 127.0.0.1:${PORT}"
"$BIN_PATH" -config "$CONFIG_FILE" > "$LOG_FILE" 2>&1 &
PID=$!

wait_ready "$MODELS_URL" || {
  tail -n 120 "$LOG_FILE" >&2 || true
  fail "proxy did not become ready"
}

# Warmup request.
curl -sS -o /dev/null -H "Authorization: Bearer ${PERF_KEY}" "$MODELS_URL"

U0="$(count_updates "$LOG_FILE")"
sleep "$IDLE_WINDOW_SEC"
U1="$(count_updates "$LOG_FILE")"
IDLE_DELTA=$((U1 - U0))

for i in $(seq 1 "$BURST_WRITES"); do
  printf '{"type":"demo","seq":%d}\n' "$i" > "$AUTH_DIR/perf.json"
  sleep "$BURST_WRITE_INTERVAL_SEC"
done
sleep "$BURST_SETTLE_SEC"
U2="$(count_updates "$LOG_FILE")"
BURST_DELTA=$((U2 - U1))

(
  end_ts=$((SECONDS + LATENCY_CHURN_SEC))
  seq_id=10000
  while [[ $SECONDS -lt $end_ts ]]; do
    seq_id=$((seq_id + 1))
    printf '{"type":"demo","seq":%d}\n' "$seq_id" > "$AUTH_DIR/perf.json"
    sleep "$LATENCY_CHURN_INTERVAL_SEC"
  done
) &
WRITER_PID=$!

measure_models_latency_ms "$MODELS_URL" "$LAT_FILE"
measure_codex_latency_ms "$CODEX_URL" "$CODEX_LAT_FILE"
wait "$WRITER_PID" || true

MODELS_AVG_MS="$(calc_avg_ms "$LAT_FILE")"
MODELS_P95_MS="$(calc_p95_ms "$LAT_FILE")"
CODEX_P95_MS="$(calc_p95_ms "$CODEX_LAT_FILE")"

echo "PERF_GATE_RESULT idle_delta=${IDLE_DELTA} burst_delta=${BURST_DELTA} models_avg_ms=${MODELS_AVG_MS} models_p95_ms=${MODELS_P95_MS} codex_p95_ms=${CODEX_P95_MS}"
echo "PERF_GATE_LIMITS idle<=${MAX_IDLE_UPDATES} burst<=${MAX_BURST_UPDATES} avg<=${MAX_MODELS_AVG_MS} models_p95<=${MAX_MODELS_P95_MS} codex_p95<=${MAX_CODEX_P95_MS}"

[[ "$IDLE_DELTA" -le "$MAX_IDLE_UPDATES" ]] || fail "idle update churn too high: ${IDLE_DELTA} > ${MAX_IDLE_UPDATES}"
[[ "$BURST_DELTA" -le "$MAX_BURST_UPDATES" ]] || fail "burst update churn too high: ${BURST_DELTA} > ${MAX_BURST_UPDATES}"
[[ "$MODELS_AVG_MS" -le "$MAX_MODELS_AVG_MS" ]] || fail "models avg latency too high: ${MODELS_AVG_MS}ms > ${MAX_MODELS_AVG_MS}ms"
[[ "$MODELS_P95_MS" -le "$MAX_MODELS_P95_MS" ]] || fail "models p95 latency too high: ${MODELS_P95_MS}ms > ${MAX_MODELS_P95_MS}ms"
[[ "$CODEX_P95_MS" -le "$MAX_CODEX_P95_MS" ]] || fail "codex p95 latency too high: ${CODEX_P95_MS}ms > ${MAX_CODEX_P95_MS}ms"

echo "PERF_GATE_PASS"
