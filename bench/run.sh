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
#   2  GATE  server --proxy=true (control plane + in-process local agent + Caddy), single node: ceiling 200 MB, tripwire 80 MB (ADR 0011).
#      From M2 step 3 the burst goes over HTTPS on :443 (curl -k, self-signed panel leaf), so its
#      TLS handshakes are part of the measurement: not like-for-like with earlier S2 numbers.
#   3  (report only until the Phase 2 slim agent) remote `agent` connected to a scenario-2 server
#      through its proxy (measured: the agent)
#   4  GATE  remote `agent` plus its own embedded Caddy (--proxy-listen), remote node: ceiling 100 MB, tripwire 50 MB (ADR 0011)
#   Gates: docs/decisions/0008-footprint-gates.md
#   5  GATE (tripwire)  "S2 with one exposed service" (plan 0004 step 15): the live-watcher S2 server plus ONE app
#      container of the test image behind a public port (created and deployed through the API, serving 2xx before
#      the warm-up). Same ceiling and 80 MB tripwire as S2.
#   6  (report only) "S2 with 10 exposed idle services": the same with ten services. RSS and idle CPU, reported
#      against the 80 MB tripwire; nothing fails.
#   7  (report only) RSS around ONE deploy: the S2 server with 2 exposed services, sampled every second, 20 s before
#      one service is redeployed, during it and for 60 s after it finishes. Reports the idle baseline, the peak,
#      and the value 60 s after.
#   8  (report only) the same with TWO concurrent deploys (both services redeployed at once): peak RSS during them.
#   9  (report only) the BASELINE of 5 and 6: the same server, the same first-run setup, sign-in and API session
#      (argon2, the first deploy history) but ZERO services. "S2 with N exposed services" minus S2 is not the
#      cost of N services (the setup path alone adds to it); minus scenario 9 is.
#   Scenarios 5-9 need Docker, the host's registry on 127.0.0.1:5000 holding the test image (APP_IMAGE, default
#   127.0.0.1:5000/sd-testapp:1: see the CI integration job), python-free: bench/drive (Go) signs in and drives the
#   API. The server runs with --network host (the app network's addresses are not reachable from another bridge)
#   and --allow-loopback-registry, on two free loopback ports (127.0.0.1 only; the apps' public ports are the product's own: all interfaces); both are bench-only choices, the binary is the shipped one.
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
#   BURST=500     requests in the burst  SCENARIOS="1 2 3 4" (default); add 5 6 7 8 9 for step 15
#   IMAGE=serverdock-bench:local         RESULTS_DIR=bench/results
#   BUILD_CONTEXT=.   the directory the image is built from (a git worktree of another revision, for an A/B run).
#                     The Dockerfile is always this checkout's bench/Dockerfile; the context needs web/build.
#   RESULTS_SUFFIX=   appended to the result file name (-step13, -final ...), so two runs never overwrite each other
#   APP_IMAGE=127.0.0.1:5000/sd-testapp:1   the app image of scenarios 5-8 (pre-pushed to the local registry)
#   DEPLOY_BASE=20 DEPLOY_MAX=60 DEPLOY_POST=60   scenarios 7-8: seconds sampled before the deploy, the longest
#                     a deploy may take, and the seconds sampled after it finishes
#   S2_DOCKER=1   (default) scenario 2 is "S2 live watcher": the server container gets the host's
#                 Docker socket and its group (--group-add <socket gid> -v /var/run/docker.sock), so
#                 its boot runs network_ensure and the reconciler's first resync (events stream,
#                 container list, rules, first route apply) opens the job scheduler's start gate
#                 against REAL Docker, and the watcher stays connected during the idle window, as
#                 on a deployed server (plan 0004 step 10). This is THE gated S2 number: ceiling
#                 and 80 MB tripwire. Labelled "S2 live watcher" in the report (a ".docker" marker
#                 next to each run's samples).
#   S2_DOCKER=0   scenario 2 as "S2 no Docker", REPORT ONLY (not gated; a ".nodocker" marker): no
#                 socket, so the watcher never connects. It is the pre-step-10 shape, kept for
#                 comparison with older numbers.
#                 The live-watcher server creates an app network (label serverdock.instance) on the
#                 host's Docker, and may create containers (serverdock.managed): the bench removes
#                 every such network and container that did not exist before the run, after each
#                 run and in the exit trap (also when interrupted), and leaves every other
#                 resource alone. Scenarios 3 and 4 never create any.
#                   SCENARIOS=2 bench/run.sh                  # S2 live watcher (gated)
#                   S2_DOCKER=0 SCENARIOS=2 bench/run.sh      # S2 no Docker (report only)
#                   SCENARIOS=4 bench/run.sh                  # S4 (gated)
#                   SCENARIOS="9 5 6 7 8" bench/run.sh        # step 15: apps behind public ports, RSS around deploys
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
BUILD_CONTEXT=${BUILD_CONTEXT:-.}
RESULTS_SUFFIX=${RESULTS_SUFFIX:-}
APP_IMAGE=${APP_IMAGE:-127.0.0.1:5000/sd-testapp:1}
DEPLOY_BASE=${DEPLOY_BASE:-20}; DEPLOY_MAX=${DEPLOY_MAX:-60}; DEPLOY_POST=${DEPLOY_POST:-60}

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
S2_DOCKER=${S2_DOCKER:-1}
case "$S2_DOCKER" in 0|1) ;; *) echo "S2_DOCKER must be 0 or 1" >&2; exit 2 ;; esac
for id in $SCENARIOS; do case "$id" in 1|2|3|4|5|6|7|8|9) ;; *) echo "unknown scenario $id" >&2; exit 2 ;; esac; done
# Does this run create things on the host's Docker (the S2 live watcher's network; apps in 5-8)?
HOST_DOCKER_USED=0
case " $SCENARIOS " in *" 5 "*|*" 6 "*|*" 7 "*|*" 8 "*|*" 9 "*) HOST_DOCKER_USED=1 ;; esac
case " $SCENARIOS " in *" 2 "*) [ "$S2_DOCKER" = 1 ] && HOST_DOCKER_USED=1 ;; esac

# What the server of "S2 live watcher" (and of scenarios 5-9) creates on the host's Docker: its app
# network and the containers of its deploys, all labelled serverdock.instance=<install id>. Clean-up
# removes ONLY what carries the bench server's own install id, learned right after the server is up:
# the id is the label of the one app network that did not exist before this run (the server creates it
# at boot). Anything else on this host, another ServerDock's resources included, is never touched, and
# when the id cannot be learned nothing is removed (a warning says so).
host_nets() { docker network ls -q --filter label=serverdock.instance 2>/dev/null || true; }
PRE_NETS=""; HOST_RECORDED=0; INSTANCE_IDS=""
record_host_docker() {
  [ "$HOST_DOCKER_USED" = 1 ] && [ "$HOST_RECORDED" = 0 ] || return 0
  PRE_NETS=$(host_nets | tr '\n' ' '); HOST_RECORDED=1
}
learn_instance() { # call once the server is up; waits up to 30 s for its app network
  [ "$HOST_DOCKER_USED" = 1 ] || return 0
  i=0
  while [ "$i" -lt 60 ]; do
    new=""
    for n in $(host_nets); do case " $PRE_NETS " in *" $n "*) ;; *) new="$new $n" ;; esac; done
    # shellcheck disable=SC2086
    set -- $new
    if [ "$#" -eq 1 ]; then
      id=$(docker network inspect -f '{{index .Labels "serverdock.instance"}}' "$1" 2>/dev/null || true)
      if printf '%s' "$id" | grep -Eq '^[0-9a-f]{32}$'; then INSTANCE_IDS="$INSTANCE_IDS $id"; return 0; fi
    fi
    i=$((i + 1)); sleep 0.5
  done
  log "warning: the bench server's install id could not be learned (no single new app network): its containers and network are NOT removed"
}
clean_host_docker() { # call after the bench's own containers are gone (a network with endpoints cannot be removed)
  for id in $INSTANCE_IDS; do
    for c in $(docker ps -aq --filter "label=serverdock.instance=$id" 2>/dev/null || true); do
      docker rm -f -v "$c" >/dev/null 2>&1 || true
    done
    for n in $(docker network ls -q --filter "label=serverdock.instance=$id" 2>/dev/null || true); do
      docker network rm "$n" >/dev/null 2>&1 || true
    done
  done
  INSTANCE_IDS=""
}

cleanup() {
  ids=$(docker ps -aq --filter label=sdbench=1 2>/dev/null || true)
  [ -z "$ids" ] || docker rm -f $ids >/dev/null 2>&1 || true
  clean_host_docker
  docker network rm "$NET" >/dev/null 2>&1 || true
  docker volume rm "$DBG_VOL" >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

log() { printf '[bench] %s\n' "$*" >&2; }

log "building $IMAGE from $BUILD_CONTEXT"
docker build -q -f bench/Dockerfile -t "$IMAGE" "$BUILD_CONTEXT" >/dev/null

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
# One -o per URL: with a single -o the second URL's bodies went to stdout and could glue onto
# a status line ("...</html>200"), so a good response was counted as a failure.
# BURST_VIA=sock sends it through the control socket instead of the host's curl.
burst() {
  accept=${2:-200}
  half=$((BURST / 2)); rest=$((BURST - half))
  if [ "${BURST_VIA:-}" = sock ]; then
    codes=$(cp_curl -o /dev/null -o /dev/null -w '%{http_code} %{url} %{errormsg}\n' --parallel --parallel-max 10 \
          "$1/healthz?n=[1-$half]" "$1/?n=[1-$rest]" || true)
  else
    codes=$(curl -sk -o /dev/null -o /dev/null -w '%{http_code} %{url} %{errormsg}\n' --parallel --parallel-max 10 \
          "$1/healthz?n=[1-$half]" "$1/?n=[1-$rest]" || true)
  fi
  ok=$(printf '%s\n' "$codes" | grep -Ec "^($accept) " || true)
  log "burst: $ok/$BURST requests returned $accept"
  # Name what was not accepted (code 000 = no HTTP response; errormsg says why).
  printf '%s\n' "$codes" | grep -Ev "^($accept) " | grep -v '^$' | head -n 10 | sed 's/^/[bench]   not accepted: /' >&2 || true
  [ "$ok" -eq "$BURST" ]
}

# ---- scenarios 5-8: the S2 live-watcher server with real apps behind public ports ----
DRIVE=""
app_registry_check() {
  curl -fsS -o /dev/null "http://127.0.0.1:5000/v2/" 2>/dev/null || { log "scenarios 5-8 need the local registry on 127.0.0.1:5000 holding $APP_IMAGE"; return 1; }
  docker image inspect "$APP_IMAGE" >/dev/null 2>&1 || docker pull -q "$APP_IMAGE" >/dev/null 2>&1 || { log "cannot pull $APP_IMAGE"; return 1; }
}

# start_apps_server <container> <napps>: the S2 live-watcher server (host network, two free ports), signed in
# and with <napps> exposed services serving 2xx. Sets base.
start_apps_server() {
  [ -n "$DRIVE" ] || { DRIVE="$WORK/drive"; go build -o "$DRIVE" ./bench/drive; }
  app_registry_check
  ports=$("$DRIVE" -addr x -state x freeports); HTTP_PORT=$(echo "$ports" | sed -n 1p); HTTPS_PORT=$(echo "$ports" | sed -n 2p)
  docker run -d --label sdbench=1 --name "$1" --network host --memory 1g --cpus 1 \
    --group-add "$(socket_gid)" -v /var/run/docker.sock:/var/run/docker.sock "$IMAGE" \
    server --data /data --proxy=true --listen "127.0.0.1:$HTTP_PORT" --https-listen "127.0.0.1:$HTTPS_PORT" --allow-loopback-registry >/dev/null
  base="https://127.0.0.1:$HTTPS_PORT"
  wait_ready "$base/healthz"
  docker cp "$1:/data/setup-token" "$WORK/setup-token" >/dev/null
  "$DRIVE" -addr "127.0.0.1:$HTTPS_PORT" -state "$WORK/session" setup -token-file "$WORK/setup-token" >/dev/null
  rm -f "$WORK/setup-token"
  learn_instance
  if [ "$2" -gt 0 ]; then
    log "creating $2 exposed service(s) from $APP_IMAGE"
    "$DRIVE" -addr "127.0.0.1:$HTTPS_PORT" -state "$WORK/session" expose -n "$2" -image "$APP_IMAGE" >&2
  fi
}

# uptime_s: this host's uptime in seconds (/proc/uptime; with decimals). Only differences, or (direct
# path) the same clock as measure.sh's samples, are used. Where there is no /proc/uptime (a macOS host
# with Docker Desktop: the helper path, informational only) it is whole seconds of the wall clock, so the
# deploy window is known to a second, and the absolute values are not used for alignment.
uptime_s() { if [ -r /proc/uptime ]; then cut -d' ' -f1 /proc/uptime; else date +%s; fi; }

record_host_docker

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
        sock_args=""
        if [ "$S2_DOCKER" = 1 ]; then
          sock_args="--group-add $(socket_gid) -v /var/run/docker.sock:/var/run/docker.sock"
          log "scenario 2: S2 live watcher (Docker socket and its group)"
        else
          log "scenario 2: S2 no Docker (report only)"
        fi
        # shellcheck disable=SC2086 # sock_args is deliberately split into words
        docker run -d --label sdbench=1 --name "$TARGET" --memory 1g --cpus 1 $sock_args -p 127.0.0.1::443 "$IMAGE" \
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
      5|6|7|8)
        case "$id" in 5) napps=1 ;; 6) napps=10 ;; *) napps=2 ;; esac
        log "scenario $id: S2 live watcher with $napps exposed service(s)"
        start_apps_server "$TARGET" "$napps" ;;
      *) echo "unknown scenario $id" >&2; exit 2 ;;
    esac
    # Scenarios 7-8: the burst comes BEFORE the warm-up so the 20 s baseline is not the tail of it.
    if [ "$id" = 7 ] || [ "$id" = 8 ]; then burst "$base"; fi
    log "warm-up ${WARMUP}s"; sleep "$WARMUP"
    if [ "$id" = 7 ] || [ "$id" = 8 ]; then
      # Sample every second from before the deploy to DEPLOY_POST seconds after it finished. The events
      # file holds the offsets from the start of sampling (so the helper path works too), the seconds
      # sampled after, whether VmHWM was reset, and the ABSOLUTE host uptimes of the deploy's start and
      # end (the report aligns on them on the direct path, where they are the samples' own clock).
      total=$((DEPLOY_BASE + DEPLOY_MAX + DEPLOY_POST + 5))
      svcs=bench/app-1; [ "$id" = 8 ] && svcs=bench/app-1,bench/app-2
      log "deploy window: ${DEPLOY_BASE}s before, then deploy of $svcs, then ${DEPLOY_POST}s after (sampling every 1 s)"
      bench/measure.sh "$TARGET" "$total" 1 "$WORK/s$id-r$r.tsv" &
      mpid=$!
      m0=$(uptime_s); sleep "$DEPLOY_BASE"
      # Best effort (direct path, root): reset the peak-RSS mark so VmHWM afterwards is the deploy's own peak.
      pid=$(docker inspect -f '{{.State.Pid}}' "$TARGET"); reset=0
      if [ -w "/proc/$pid/clear_refs" ] && echo 5 >"/proc/$pid/clear_refs" 2>/dev/null; then reset=1; fi
      d0=$(uptime_s)
      "$DRIVE" -addr "127.0.0.1:$HTTPS_PORT" -state "$WORK/session" deploy -services "$svcs" -timeout "${DEPLOY_MAX}s" >&2
      d1=$(uptime_s)
      sleep "$DEPLOY_POST"
      wait "$mpid"
      echo "$(awk -v a="$m0" -v b="$d0" 'BEGIN{printf "%.2f", b-a}') $(awk -v a="$m0" -v b="$d1" 'BEGIN{printf "%.2f", b-a}') $DEPLOY_POST $reset $d0 $d1" >"$WORK/s$id-r$r.tsv.events"
      ids=$(docker ps -aq --filter label=sdbench=1); [ -z "$ids" ] || docker rm -f $ids >/dev/null
      clean_host_docker
      r=$((r + 1)); continue
    fi
    # S4's burst goes through the agent's own Caddy to the server's :80, which now only
    # redirects: the agent's proxy does the same work, so 200 or 308 both count.
    if [ "$id" = 4 ]; then burst "$base" "200|308"; else burst "$base"; fi
    case "$id" in 3|4) log_stream ;; esac
    log "idle window ${IDLE}s (sampling every ${INTERVAL}s)"
    bench/measure.sh "$TARGET" "$IDLE" "$INTERVAL" "$WORK/s$id-r$r.tsv"
    if [ "$id" = 2 ]; then
      if [ "$S2_DOCKER" = 1 ]; then echo docker >"$WORK/s$id-r$r.tsv.docker"; else echo nodocker >"$WORK/s$id-r$r.tsv.nodocker"; fi
    fi
    ids=$(docker ps -aq --filter label=sdbench=1); [ -z "$ids" ] || docker rm -f $ids >/dev/null
    clean_host_docker
    docker network rm "$NET" >/dev/null 2>&1 || true
    docker volume rm "$DBG_VOL" >/dev/null 2>&1 || true
    r=$((r + 1))
  done
done

mkdir -p "$RESULTS_DIR"
QFLAG=""; [ "$QUICK" = 1 ] && QFLAG="-quick"
status=0
go run ./bench/report -dir "$WORK" -out "$RESULTS_DIR/$DATE-$ARCH$SUFFIX$RESULTS_SUFFIX" -arch "$ARCH" -date "$DATE" \
  -binary-bytes "$BIN_BYTES" -binary-gzip-bytes "$GZ_BYTES" $QFLAG >&2 || status=$?
log "results: $RESULTS_DIR/$DATE-$ARCH$SUFFIX$RESULTS_SUFFIX.md / .json"
exit "$status"
