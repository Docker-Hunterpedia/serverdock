#!/bin/sh
# ServerDock footprint bench (plan 0001, scenarios 1-2).
#
#   bench/run.sh
#
# Scenarios (every container: --memory 1g --cpus 1, nonroot distroless image):
#   1  server --proxy=false
#   2  server --proxy=true   (control plane + in-process local agent + Caddy)
#   3  remote `agent` connected to a scenario-2 server through its proxy (measured: the agent)
#   4  (report only) remote `agent` plus its own embedded Caddy (--proxy-listen), previewing the
#      remote-node <= 50 MB budget
# Scenarios 3-4 also start a server container (debug listener on the bench network only) and a
# busybox logger; one log stream is opened and closed during the burst.
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
SCENARIOS=${SCENARIOS:-"1 2 3 4"}
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
NET=sdbench-net
SOCK_GID=""
cleanup() {
  ids=$(docker ps -aq --filter label=sdbench=1 2>/dev/null || true)
  [ -z "$ids" ] || docker rm -f $ids >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
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

ALPINE=alpine:3@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf
BUSYBOX=busybox:1.37@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec
TOKEN=bench-token-not-a-secret

# Scenarios 3-4 need a server to connect to. It runs with the debug listener on
# the bench network only (--debug-allow-remote) so the host can open one log
# stream; the debug listener is used only during the burst, never in the idle window.
start_server_stack() { # -> sets SRV_HTTP (host:port of the server's proxy) and SRV_DEBUG
  docker network create "$NET" >/dev/null 2>&1 || true
  docker run -d --label sdbench=1 --name sdbench-srv --network "$NET" --memory 1g --cpus 1 \
    -e SD_AGENT_TOKEN="$TOKEN" -p 127.0.0.1::80 -p 127.0.0.1::7071 "$IMAGE" \
    server --data /data --proxy=true --listen :80 --internal 127.0.0.1:7070 \
    --debug-listen 0.0.0.0:7071 --debug-allow-remote >/dev/null
  SRV_HTTP=$(docker port sdbench-srv 80/tcp | head -n1)
  SRV_DEBUG=$(docker port sdbench-srv 7071/tcp | head -n1)
  wait_ready "http://$SRV_HTTP/healthz"
  # A container to read logs from.
  docker run -d --label sdbench=1 --name sdbench-logger --network "$NET" "$BUSYBOX" \
    sh -c 'while true; do echo bench-log-line; sleep 1; done' >/dev/null
}

wait_agent() { # waits until the server lists the bench agent
  i=0
  while [ "$i" -lt 60 ]; do
    curl -fsS "http://$SRV_DEBUG/debug/agents" 2>/dev/null | grep -q '"bench"' && return 0
    i=$((i + 1)); sleep 0.5
  done
  log "agent never connected"; return 1
}

# One log stream opened and closed (curl -m closes the client side).
log_stream() {
  out=$(curl -s -m 3 "http://$SRV_DEBUG/debug/agents/bench/logs/sdbench-logger?tail=5" || true)
  case "$out" in *bench-log-line*) log "log stream: ok" ;; *) log "log stream returned no data"; return 1 ;; esac
}

socket_gid() {
  # The image runs as nonroot (uid 65532) and cannot open /var/run/docker.sock by
  # default; add the socket's group (works on Linux hosts and Docker Desktop).
  [ -n "$SOCK_GID" ] || SOCK_GID=$(docker run --rm -v /var/run/docker.sock:/s "$ALPINE" stat -c %g /s)
  echo "$SOCK_GID"
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
  r=1
  while [ "$r" -le "$RUNS" ]; do
    log "scenario $id run $r/$RUNS"
    ids=$(docker ps -aq --filter label=sdbench=1); [ -z "$ids" ] || docker rm -f $ids >/dev/null
    TARGET="sdbench-s$id-r$r"
    case "$id" in
      1)
        docker run -d --label sdbench=1 --name "$TARGET" --memory 1g --cpus 1 -p 127.0.0.1::7070 "$IMAGE" \
          server --data /data --proxy=false --internal 0.0.0.0:7070 >/dev/null
        base="http://$(docker port "$TARGET" 7070/tcp | head -n1)"
        wait_ready "$base/healthz" ;;
      2)
        docker run -d --label sdbench=1 --name "$TARGET" --memory 1g --cpus 1 -p 127.0.0.1::80 "$IMAGE" \
          server --data /data --proxy=true --listen :80 --internal 127.0.0.1:7070 >/dev/null
        base="http://$(docker port "$TARGET" 80/tcp | head -n1)"
        wait_ready "$base/healthz" ;;
      3|4)
        start_server_stack
        extra=""; base="http://$SRV_HTTP"
        if [ "$id" = 4 ]; then extra="--proxy-listen :8080 --proxy-upstream sdbench-srv:80"; fi
        # shellcheck disable=SC2086
        docker run -d --label sdbench=1 --name "$TARGET" --network "$NET" --memory 1g --cpus 1 \
          --group-add "$(socket_gid)" -v /var/run/docker.sock:/var/run/docker.sock \
          -e SD_AGENT_TOKEN="$TOKEN" $( [ "$id" = 4 ] && echo "-p 127.0.0.1::8080" ) "$IMAGE" \
          agent --server ws://sdbench-srv:80/agent/connect --id bench --data /data $extra >/dev/null
        if [ "$id" = 4 ]; then
          base="http://$(docker port "$TARGET" 8080/tcp | head -n1)"
          wait_ready "$base/healthz"
        fi
        wait_agent ;;
      *) echo "unknown scenario $id" >&2; exit 2 ;;
    esac
    log "warm-up ${WARMUP}s"; sleep "$WARMUP"
    burst "$base"
    case "$id" in 3|4) log_stream ;; esac
    log "idle window ${IDLE}s (sampling every ${INTERVAL}s)"
    bench/measure.sh "$TARGET" "$IDLE" "$INTERVAL" "$WORK/s$id-r$r.tsv"
    ids=$(docker ps -aq --filter label=sdbench=1); [ -z "$ids" ] || docker rm -f $ids >/dev/null
    docker network rm "$NET" >/dev/null 2>&1 || true
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
