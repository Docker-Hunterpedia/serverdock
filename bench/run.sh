#!/bin/sh
# ServerDock footprint bench (plan 0001, scenarios 1-2).
#
#   bench/run.sh
#
# Scenarios (every container: --memory 1g --cpus 1, nonroot distroless image):
#   1  (report only) server --proxy=false; the control plane has only its unix socket, so
#      readiness and the burst run in the curl helper container on the shared volume
#      (every request is a `docker run` curl, with its own unix-socket connect). From M2 step 2
#      on, S1 is therefore NOT comparable with M1's S1 (a TCP listener and the host's curl).
#   2  GATE  server --proxy=true (control plane + in-process local agent + Caddy), single node <= 80 MB.
#      From M2 step 3 the burst goes over HTTPS on :443 (curl -k, self-signed panel leaf), so its
#      TLS handshakes are part of the measurement: not like-for-like with earlier S2 numbers.
#   3  (report only until the Phase 2 slim agent) remote `agent` connected to a scenario-2 server
#      through its proxy (measured: the agent)
#   4  GATE  remote `agent` plus its own embedded Caddy (--proxy-listen), remote node <= 50 MB
#   Gates: docs/decisions/0008-footprint-gates.md
# Scenarios 3-4 also start a server container (agent TLS listener on the bench network, debug
# unix socket on a shared volume) and a busybox logger; one log stream is opened and closed
# during the burst. The agent connects over wss with a pinned CA fingerprint and a generated
# 43-character token.
#
# Per run: start -> warm-up -> burst (500 requests from the host) -> idle window,
# sampled from OUTSIDE the container by bench/measure.sh. Gate = median of the
# per-run medians (bench/report). The debug socket is NOT enabled on measured
# containers, so the binary is measured exactly as shipped and nothing talks to
# it during idle.
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
  docker volume rm "$DBG_VOL" >/dev/null 2>&1 || true
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
# Helper for the debug socket: curl, run as the server's uid on the shared volume.
CURL=curlimages/curl:8.15.0@sha256:4026b29997dc7c823b51c164b71e2b51e0fd95cce4601f78202c513d97da2922
DBG_VOL=sdbench-debug
# A generated token: 32 random bytes as 43 base64url characters (the server refuses shorter).
TOKEN=$(head -c 32 /dev/urandom | base64 | tr '+/' '-_' | tr -d '=\n')

# GET a path on the server's debug unix socket. The socket is 0600 inside a 0700
# directory owned by uid 65532, so a helper container runs as the same uid on the
# shared volume; the measured binary needs no debug flag beyond the socket itself.
dbg_get() { # path [curl args...]
  p=$1; shift
  docker run --rm --label sdbench=1 --user 65532:65532 -v "$DBG_VOL":/debug "$CURL" \
    -sS --unix-socket /debug/sd.sock "$@" "http://debug$p"
}

# Scenarios 3-4 need a server to connect to. It listens for agents over TLS 1.3 on
# the bench network (SAN sdbench-srv) and serves a debug socket on a shared volume
# so a helper can open one log stream; the socket is used only during the burst,
# never in the idle window.
start_server_stack() { # -> sets SRV_HTTP (host:port of the server's HTTPS panel) and FP (hub CA fingerprint)
  docker network create "$NET" >/dev/null 2>&1 || true
  docker volume create --label sdbench=1 "$DBG_VOL" >/dev/null
  docker run -d --label sdbench=1 --name sdbench-srv --network "$NET" --memory 1g --cpus 1 \
    -e SD_AGENT_TOKEN="$TOKEN" -v "$DBG_VOL":/debug -p 127.0.0.1::443 "$IMAGE" \
    server --data /data --proxy=true --listen :80 \
    --debug-socket /debug/sd.sock --agent-listen :7443 --agent-san sdbench-srv >/dev/null
  SRV_HTTP=$(docker port sdbench-srv 443/tcp | head -n1) # the panel's HTTPS port; :80 only redirects
  wait_ready "https://$SRV_HTTP/healthz"
  # The CA fingerprint is public; read it from the server's data dir.
  docker cp sdbench-srv:/data/pki/ca.fingerprint "$WORK/ca.fp" >/dev/null
  FP=$(tr -d '\n' <"$WORK/ca.fp")
  # A container to read logs from.
  docker run -d --label sdbench=1 --name sdbench-logger --network "$NET" "$BUSYBOX" \
    sh -c 'while true; do echo bench-log-line; sleep 1; done' >/dev/null
}

wait_agent() { # waits until the server lists the bench agent
  i=0
  while [ "$i" -lt 60 ]; do
    dbg_get /debug/agents 2>/dev/null | grep -q '"bench"' && return 0
    i=$((i + 1)); sleep 0.5
  done
  log "agent never connected"; return 1
}

# One log stream opened and closed (curl -m closes the client side).
log_stream() {
  out=$(dbg_get "/debug/agents/bench/logs/sdbench-logger?tail=5" -m 4 || true)
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
    curl -fsSk -o /dev/null "$1" 2>/dev/null && return 0
    i=$((i + 1)); sleep 0.5
  done
  log "service never became ready: $1"; return 1
}

# curl through the control socket, as our Caddy would reach it: from the helper container on
# the shared volume, with the single X-Forwarded-For value the control plane requires.
cp_curl() { # curl args...
  docker run --rm --label sdbench=1 --user 65532:65532 -v "$DBG_VOL":/debug "$CURL" \
    -sS --unix-socket /debug/cp.sock -H 'X-Forwarded-For: 127.0.0.1' "$@"
}

wait_ready_sock() { # waits for /healthz on the control socket
  i=0
  while [ "$i" -lt 60 ]; do
    cp_curl -f -o /dev/null http://127.0.0.1/healthz 2>/dev/null && return 0
    i=$((i + 1)); sleep 0.5
  done
  log "control socket never became ready"; return 1
}

# burst base_url [accepted status codes, default 200]. curl never follows redirects (no -L), so
# a 308 is counted as a 308. https URLs use -k: the panel's certificate is self-signed.
# BURST_VIA=sock sends it through the control socket instead of the host's curl.
burst() {
  accept=${2:-200}
  half=$((BURST / 2)); rest=$((BURST - half))
  if [ "${BURST_VIA:-}" = sock ]; then
    ok=$(cp_curl -o /dev/null -w '%{http_code}\n' --parallel --parallel-max 10 \
          "$1/healthz?n=[1-$half]" "$1/?n=[1-$rest]" | grep -Ec "^($accept)$" || true)
  else
    ok=$(curl -sk -o /dev/null -w '%{http_code}\n' --parallel --parallel-max 10 \
          "$1/healthz?n=[1-$half]" "$1/?n=[1-$rest]" | grep -Ec "^($accept)$" || true)
  fi
  log "burst: $ok/$BURST requests returned $accept"
  [ "$ok" -eq "$BURST" ]
}

for id in $SCENARIOS; do
  r=1
  while [ "$r" -le "$RUNS" ]; do
    log "scenario $id run $r/$RUNS"
    ids=$(docker ps -aq --filter label=sdbench=1); [ -z "$ids" ] || docker rm -f $ids >/dev/null
    TARGET="sdbench-s$id-r$r"
    BURST_VIA=""
    case "$id" in
      1)
        # The socket sits in the shared volume's directory (0700, uid 65532).
        docker volume create --label sdbench=1 "$DBG_VOL" >/dev/null
        docker run -d --label sdbench=1 --name "$TARGET" --memory 1g --cpus 1 -v "$DBG_VOL":/debug "$IMAGE" \
          server --data /data --proxy=false --control-socket /debug/cp.sock >/dev/null
        base="http://127.0.0.1"; BURST_VIA=sock
        wait_ready_sock ;;
      2)
        docker run -d --label sdbench=1 --name "$TARGET" --memory 1g --cpus 1 -p 127.0.0.1::443 "$IMAGE" \
          server --data /data --proxy=true --listen :80 >/dev/null
        base="https://$(docker port "$TARGET" 443/tcp | head -n1)"
        wait_ready "$base/healthz" ;;
      3|4)
        start_server_stack
        extra=""; base="https://$SRV_HTTP"
        if [ "$id" = 4 ]; then extra="--proxy-listen :8080 --proxy-upstream sdbench-srv:80"; fi
        # shellcheck disable=SC2086
        docker run -d --label sdbench=1 --name "$TARGET" --network "$NET" --memory 1g --cpus 1 \
          --group-add "$(socket_gid)" -v /var/run/docker.sock:/var/run/docker.sock \
          -e SD_AGENT_TOKEN="$TOKEN" $( [ "$id" = 4 ] && echo "-p 127.0.0.1::8080" ) "$IMAGE" \
          agent --server wss://sdbench-srv:7443/agent/connect --ca-fingerprint "$FP" --id bench --data /data $extra >/dev/null
        if [ "$id" = 4 ]; then
          base="http://$(docker port "$TARGET" 8080/tcp | head -n1)"
          wait_ready "$base/healthz"
        fi
        wait_agent ;;
      *) echo "unknown scenario $id" >&2; exit 2 ;;
    esac
    log "warm-up ${WARMUP}s"; sleep "$WARMUP"
    # S4's burst goes through the agent's own Caddy to the server's :80, which now only
    # redirects: the agent's proxy does the same work, so 200 or 308 both count.
    if [ "$id" = 4 ]; then burst "$base" "200|308"; else burst "$base"; fi
    case "$id" in 3|4) log_stream ;; esac
    log "idle window ${IDLE}s (sampling every ${INTERVAL}s)"
    bench/measure.sh "$TARGET" "$IDLE" "$INTERVAL" "$WORK/s$id-r$r.tsv"
    ids=$(docker ps -aq --filter label=sdbench=1); [ -z "$ids" ] || docker rm -f $ids >/dev/null
    docker network rm "$NET" >/dev/null 2>&1 || true
    docker volume rm "$DBG_VOL" >/dev/null 2>&1 || true
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
