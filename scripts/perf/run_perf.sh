#!/usr/bin/env bash
# GenixSearch perf test runner.
#
# Builds a release genixsearch binary, boots it against a clean data dir, and
# runs three progressive PUSHI/QUERY phases against a sample of Spanish
# product names (shipped in-repo at scripts/perf/data/productos.csv). All
# logs (server log + per-phase ingest/query traces) land under
# data/perf-logs/.
#
# Usage:
#   scripts/perf/run_perf.sh                 # default 1k / 10k / 50k phases
#   N1=2000 N2=20000 N3=80000 scripts/perf/run_perf.sh  # custom sizes
#   CSV=/path/to/products.csv scripts/perf/run_perf.sh  # use an external CSV
#   KEEP_DATA=1 scripts/perf/run_perf.sh     # preserve data/perf-store across runs

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
cd "$ROOT_DIR"

CSV="${CSV:-$SCRIPT_DIR/data/productos.csv}"
N1="${N1:-1000}"
N2="${N2:-10000}"
N3="${N3:-50000}"
PASSWORD="${PASSWORD:-SecretPassword}"
HOST="${HOST:-::1}"
PORT="${PORT:-1491}"
COLLECTION="${COLLECTION:-products}"
BUCKET="${BUCKET:-all}"
KEEP_DATA="${KEEP_DATA:-0}"

DATA_DIR="$ROOT_DIR/data/perf-store"
LOG_DIR="$ROOT_DIR/data/perf-logs"
CONFIG="$SCRIPT_DIR/genixsearch.cfg"
CLIENT="$SCRIPT_DIR/perf_client.py"
BIN="$ROOT_DIR/target/release/genixsearch"

bold() { printf '\033[1m%s\033[0m\n' "$*"; }
hr()   { printf '%.0s-' {1..78}; echo; }
fail() { echo "ERROR: $*" >&2; exit 1; }

[ -f "$CSV" ]    || fail "CSV not found: $CSV"
[ -f "$CONFIG" ] || fail "config not found: $CONFIG"
[ -f "$CLIENT" ] || fail "client not found: $CLIENT"
command -v python3 >/dev/null   || fail "python3 missing"
command -v cargo   >/dev/null   || fail "cargo missing"

mkdir -p "$LOG_DIR"
if [ "$KEEP_DATA" != "1" ] && [ -d "$DATA_DIR" ]; then
  bold "==> Wiping previous data dir: $DATA_DIR"
  rm -rf "$DATA_DIR"
fi
mkdir -p "$DATA_DIR/kv"

bold "==> CSV:     $CSV ($(wc -l <"$CSV") lines)"
bold "==> Phases:  $N1 -> $N2 -> $N3 rows"
bold "==> Logs:    $LOG_DIR"
hr

bold "==> Building genixsearch (release, no default features)..."
BUILD_LOG="$LOG_DIR/build.log"
if ! BINDGEN_EXTRA_CLANG_ARGS="${BINDGEN_EXTRA_CLANG_ARGS:--I/usr/lib/clang/21/include}" \
     cargo build --release --bin genixsearch --no-default-features >"$BUILD_LOG" 2>&1; then
  echo "build failed; tail of $BUILD_LOG:"
  tail -50 "$BUILD_LOG"
  exit 1
fi
[ -x "$BIN" ] || fail "binary missing after build: $BIN"
bold "    binary: $BIN"
hr

bold "==> Starting genixsearch..."
GENIX_LOG="$LOG_DIR/genixsearch.log"
: >"$GENIX_LOG"
"$BIN" -c "$CONFIG" >"$GENIX_LOG" 2>&1 &
GENIX_PID=$!

cleanup() {
  if kill -0 "$GENIX_PID" 2>/dev/null; then
    bold "==> Stopping genixsearch (pid $GENIX_PID)..."
    kill -INT "$GENIX_PID" 2>/dev/null || true
    for _ in $(seq 1 20); do
      kill -0 "$GENIX_PID" 2>/dev/null || break
      sleep 0.25
    done
    kill -KILL "$GENIX_PID" 2>/dev/null || true
    wait "$GENIX_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT INT TERM

bold "==> Waiting for genixsearch to accept TCP on [$HOST]:$PORT ..."
READY=0
for attempt in $(seq 1 60); do
  if ncat -z -6 "$HOST" "$PORT" 2>/dev/null \
     || nc -z "$HOST" "$PORT" 2>/dev/null; then
    READY=1
    break
  fi
  sleep 0.25
done
[ "$READY" = "1" ] || { tail -50 "$GENIX_LOG"; fail "genixsearch did not come up in time"; }
bold "    ready in ~$((attempt * 250))ms"
hr

run_phase() {
  local n="$1"
  local label="$2"
  local phase_log="$LOG_DIR/phase-${label}.log"

  : >"$phase_log"
  bold "==> PHASE ${label}: ingest up to $n rows"
  hr
  python3 "$CLIENT" \
    --host "$HOST" --port "$PORT" --password "$PASSWORD" \
    --csv "$CSV" --n "$n" \
    --collection "$COLLECTION" --bucket "$BUCKET" \
    --report-every 500 \
    ingest 2>&1 | tee -a "$phase_log"
  hr

  bold "==> PHASE ${label}: query batch"
  hr
  python3 "$CLIENT" \
    --host "$HOST" --port "$PORT" --password "$PASSWORD" \
    --csv "$CSV" --n "$n" \
    --collection "$COLLECTION" --bucket "$BUCKET" \
    --limit 10 \
    query 2>&1 | tee -a "$phase_log"
  hr

  bold "    phase ${label} log saved: $phase_log"
  echo
}

run_phase "$N1" "1k"
run_phase "$N2" "10k"
run_phase "$N3" "50k"

bold "==> final genixsearch server log tail:"
tail -20 "$GENIX_LOG"
hr
bold "==> Done. All logs under $LOG_DIR"
