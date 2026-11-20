#!/bin/sh
# ServerDock footprint bench (plan 0001, scenarios 1-2).
#
#   bench/run.sh
#
# Scenarios (every container: --memory 1g --cpus 1, nonroot distroless image):
#   1  server --proxy=false
#   2  server --proxy=true   (control plane + in-process local agent + Caddy)
# Scenarios 3-4 (remote agent) arrive with plan step 7.
#
# Per run: start -> warm-up -> burst (500 requests from the host) -> idle window,
# sampled from OUTSIDE the container by bench/measure.sh. Gate = median of the
# per-run medians (bench/report). The debug listener is NOT enabled here, so the
# binary is measured exactly as shipped and nothing talks to it during idle.
#
# Environment:
#   QUICK=1       shortened warm-up 20 s, idle 60 s, 1 run; gates evaluated but labelled "quick"
#   WARMUP=120    warm-up seconds        IDLE=300   idle window seconds
#   INTERVAL=10   sample interval (s)    RUNS=3     runs per scenario
#   BURST=500     requests in the burst  SCENARIOS="1 2"
#   IMAGE=serverdock-bench:local         RESULTS_DIR=bench/results
#
# Exit status: 1 when a gate fails, unless the measurement ran through the
# Docker Desktop helper container (informational only).
#
# Docker socket (scenarios 3-4 and the busybox integration test, later steps):
# the image runs as nonroot (uid 65532) and cannot open /var/run/docker.sock by
# default. Run the agent with `--group-add <gid of docker.sock>` (or as root in
# tests), e.g.  docker run --group-add "$(stat -c %g /var/run/docker.sock)" \
#   -v /var/run/docker.sock:/var/run/docker.sock ...
set -eu

cd "$(dirname "$0")/.."
QUICK=${QUICK:-0}
if [ "$QUICK" = 1 ]; then
  WARMUP=${WARMUP:-20}; IDLE=${IDLE:-60}; RUNS=${RUNS:-1}
else
  WARMUP=${WARMUP:-120}; IDLE=${IDLE:-300}; RUNS=${RUNS:-3}
fi
INTERVAL=${INTERVAL:-10}
BURST=${BURST:-500}
SCENARIOS=${SCENARIOS:-"1 2"}
IMAGE=${IMAGE:-serverdock-bench:local}
RESULTS_DIR=${RESULTS_DIR:-bench/results}

case "$(docker info --format '{{.Architecture}}')" in
  aarch64|arm64) ARCH=arm64 ;;
  x86_64|amd64) ARCH=amd64 ;;
  *) ARCH=$(docker info --format '{{.Architecture}}') ;;
esac
DATE=$(date -u +%Y-%m-%d)
SUFFIX=""; [ "$QUICK" = 1 ] && SUFFIX="-quick"
WORK=$(mktemp -d "${TMPDIR:-/tmp}/sdbench.XXXXXX")
CURRENT=""
cleanup() {
  [ -n "$CURRENT" ] && docker rm -f "$CURRENT" >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

log() { printf '[bench] %s\n' "$*" >&2; }

log "building $IMAGE"
docker build -q -f bench/Dockerfile -t "$IMAGE" . >/dev/null

# Binary sizes (raw and gzip -9) from the image that is measured.
cid=$(docker create "$IMAGE")
docker cp "$cid:/serverdock" "$WORK/serverdock" >/dev/null
docker rm -f "$cid" >/dev/null
BIN_BYTES=$(wc -c <"$WORK/serverdock" | tr -d ' ')
GZ_BYTES=$(gzip -9 -c "$WORK/serverdock" | wc -c | tr -d ' ')
log "binary $BIN_BYTES bytes, gzip -9 $GZ_BYTES bytes"

scenario_args() { # id -> container port and args
  case "$1" in
    1) PORT=7070; ARGS="server --data /data --proxy=false --internal 0.0.0.0:7070" ;;
    2) PORT=80;   ARGS="server --data /data --proxy=true --listen :80 --internal 127.0.0.1:7070" ;;
    *) echo "unknown scenario $1" >&2; exit 2 ;;
  esac
}

wait_ready() { # url
  i=0
  while [ "$i" -lt 60 ]; do
    curl -fsS -o /dev/null "$1" 2>/dev/null && return 0
    i=$((i + 1)); sleep 0.5
  done
  log "service never became ready: $1"; return 1
}

burst() { # base_url
  half=$((BURST / 2)); rest=$((BURST - half))
  ok=$(curl -s -o /dev/null -w '%{http_code}\n' --parallel --parallel-max 10 \
        "$1/healthz?n=[1-$half]" "$1/?n=[1-$rest]" | grep -c '^200$' || true)
  log "burst: $ok/$BURST requests returned 200"
  [ "$ok" -eq "$BURST" ]
}

for id in $SCENARIOS; do
  scenario_args "$id"
  r=1
  while [ "$r" -le "$RUNS" ]; do
    CURRENT="sdbench-s$id-r$r"
    log "scenario $id run $r/$RUNS"
    docker rm -f "$CURRENT" >/dev/null 2>&1 || true
    # shellcheck disable=SC2086
    docker run -d --name "$CURRENT" --memory 1g --cpus 1 -p "127.0.0.1::$PORT" "$IMAGE" $ARGS >/dev/null
    hostport=$(docker port "$CURRENT" "$PORT/tcp" | head -n1)
    base="http://$hostport"
    wait_ready "$base/healthz"
    log "warm-up ${WARMUP}s"; sleep "$WARMUP"
    burst "$base"
    log "idle window ${IDLE}s (sampling every ${INTERVAL}s)"
    bench/measure.sh "$CURRENT" "$IDLE" "$INTERVAL" "$WORK/s$id-r$r.tsv"
    docker rm -f "$CURRENT" >/dev/null; CURRENT=""
    r=$((r + 1))
  done
done

mkdir -p "$RESULTS_DIR"
QFLAG=""; [ "$QUICK" = 1 ] && QFLAG="-quick"
status=0
go run ./bench/report -dir "$WORK" -out "$RESULTS_DIR/$DATE-$ARCH$SUFFIX" -arch "$ARCH" -date "$DATE" \
  -binary-bytes "$BIN_BYTES" -binary-gzip-bytes "$GZ_BYTES" $QFLAG >&2 || status=$?
log "results: $RESULTS_DIR/$DATE-$ARCH$SUFFIX.md / .json"
exit "$status"
