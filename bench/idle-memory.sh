#!/bin/sh
# The M2 idle-memory check (plan 0003, final notes from round 2). After the last
# sign-in of a warmed single-node server (scenario 2's flags, plus a debug socket),
# RssAnon is sampled every INTERVAL seconds for WINDOW seconds from outside the
# container, and at +60 s and at the end the server's memstats are read from the
# debug socket (HeapInuse, HeapIdle minus HeapReleased, NumGC).
#
#   bench/idle-memory.sh        (needs docker, curl and python3; Linux with cgroup v2 for
#                                a direct reading, Docker Desktop gives an informational one)
#
# Verdict, from the RssAnon series:
#   flat      range (max - min) <= 1 MB
#   zig-zag   range > 1 MB but the last minute's mean is within 1 MB of the first minute's
#   growing   the last minute's mean is more than 1 MB above the first minute's
#   step      like growing, but the first minute and the last two minutes are each flat (<= 0.3 MB):
#             one rise and then nothing, not a trend. On a kernel with transparent huge pages
#             set to "always" (Docker Desktop's VM) RssAnon moves in 2 MB steps when khugepaged
#             collapses pages, so a step like this is expected there (check AnonHugePages in
#             /proc/<pid>/smaps_rollup).
# "growing" with a rising HeapInuse (or NumGC steady) means: take a heap profile and add a
# Background work row. Growth that stops in HeapIdle minus HeapReleased is the runtime
# holding freed memory, not a leak. Under ADR 0011 this is report-only: nothing is gated and
# the exit status is 0 whatever the verdict.
#
# The debug-socket reads are themselves traffic: the first one wakes the debug server
# (its listener and handlers), so run once with NO_DEBUG_READ=1 as a control. The script
# prints at which sample each read happened.
#
# Environment: WINDOW=300 INTERVAL=5 SKIP_BUILD=1 NO_DEBUG_READ=1 IMAGE=serverdock-bench:local
set -eu
cd "$(dirname "$0")/.."
IMAGE=${IMAGE:-serverdock-bench:local}
WINDOW=${WINDOW:-300}
INTERVAL=${INTERVAL:-5}
NAME=sdbench-idle
VOL=sdbench-idle-debug
CURL=curlimages/curl:8.15.0@sha256:4026b29997dc7c823b51c164b71e2b51e0fd95cce4601f78202c513d97da2922
WORK=$(mktemp -d "${TMPDIR:-/tmp}/sdidle.XXXXXX")
cleanup() { docker rm -f "$NAME" >/dev/null 2>&1 || true; docker volume rm "$VOL" >/dev/null 2>&1 || true; rm -rf "$WORK"; }
trap cleanup EXIT
log() { printf '[idle-memory] %s\n' "$*" >&2; }

[ -n "${SKIP_BUILD:-}" ] || { log "building $IMAGE"; docker build -q -f bench/Dockerfile -t "$IMAGE" . >/dev/null; }
docker rm -f "$NAME" >/dev/null 2>&1 || true
docker volume rm "$VOL" >/dev/null 2>&1 || true
docker volume create "$VOL" >/dev/null
# The volume root is root-owned; hand it to the image's nonroot uid before the server runs.
docker run --rm -v "$VOL":/debug --user 0:0 --entrypoint /bin/sh busybox:1 -c 'chown 65532:65532 /debug && chmod 700 /debug' >/dev/null 2>&1 || true
docker run -d --name "$NAME" --memory 1g --cpus 1 -p 127.0.0.1::443 -v "$VOL":/debug "$IMAGE" \
  server --data /data --proxy=true --listen :80 --debug-socket /debug/sd.sock >/dev/null
ADDR=$(docker port "$NAME" 443/tcp | head -n1)
i=0
until curl -fsSk -o /dev/null "https://$ADDR/healthz" 2>/dev/null; do
  i=$((i + 1)); [ "$i" -lt 60 ] || { log "server never became ready"; exit 1; }; sleep 0.5
done

totp() {
  python3 -I - "$1" <<'PY'
import base64, hmac, hashlib, struct, sys, time
s = sys.argv[1].upper(); s += "=" * (-len(s) % 8)
key = base64.b32decode(s)
c = int(time.time()) // 30
h = hmac.new(key, struct.pack(">Q", c), hashlib.sha1).digest()
o = h[-1] & 15
print("%06d" % ((struct.unpack(">I", h[o:o+4])[0] & 0x7fffffff) % 1000000))
PY
}
json() { python3 -I -c "import json,sys; print(json.load(sys.stdin)$1)"; }
post() { curl -sSk -b "$WORK/jar" -c "$WORK/jar" -H 'Content-Type: application/json' -H "Origin: https://$ADDR" -X POST "https://$ADDR$1" -d "$2"; }
dbg_mem() { docker run --rm --user 65532:65532 -v "$VOL":/debug "$CURL" -sS --unix-socket /debug/sd.sock http://debug/debug/mem; }

log "warm-up 20 s"; sleep 20
docker cp "$NAME:/data/setup-token" "$WORK/token" >/dev/null
TOKEN=$(tr -d '\n' <"$WORK/token")
PASS='correct horse battery staple'
EN=$(post /api/v1/setup/enroll "{\"token\":\"$TOKEN\",\"username\":\"admin\"}")
SECRET=$(printf '%s' "$EN" | json "['secret']"); EID=$(printf '%s' "$EN" | json "['enrollment_id']")
post /api/v1/setup/complete "{\"token\":\"$TOKEN\",\"enrollment_id\":\"$EID\",\"username\":\"admin\",\"password\":\"$PASS\",\"code\":\"$(totp "$SECRET")\"}" >/dev/null
log "first run done; three sign-ins, then the idle window"
for _ in 1 2 3; do
  sleep 31 # a TOTP code is never accepted twice
  rm -f "$WORK/jar"
  post /api/v1/auth/login "{\"username\":\"admin\",\"password\":\"$PASS\"}" >/dev/null
  [ "$(post /api/v1/auth/2fa "{\"code\":\"$(totp "$SECRET")\"}" | json "['username']")" = admin ] || { log "sign-in failed"; exit 1; }
done
log "last sign-in done: sampling RssAnon every ${INTERVAL}s for ${WINDOW}s (no traffic from here on)"
bench/measure.sh "$NAME" "$WINDOW" "$INTERVAL" "$WORK/rss.tsv" >/dev/null 2>&1 &
MEASURE=$!
T0=$(date +%s)
sleep 60
if [ -z "${NO_DEBUG_READ:-}" ]; then
  dbg_mem >"$WORK/mem60.json" 2>/dev/null || echo '{}' >"$WORK/mem60.json"
  echo $(( ($(date +%s) - T0) / INTERVAL )) >"$WORK/read60.idx"
else echo '{}' >"$WORK/mem60.json"; echo - >"$WORK/read60.idx"; fi
wait "$MEASURE"
if [ -z "${NO_DEBUG_READ:-}" ]; then
  dbg_mem >"$WORK/memend.json" 2>/dev/null || echo '{}' >"$WORK/memend.json"
else echo '{}' >"$WORK/memend.json"; fi

python3 -I - "$WORK/rss.tsv" "$WORK/rss.tsv.mode" "$WORK/mem60.json" "$WORK/memend.json" "$INTERVAL" "$WORK/read60.idx" <<'PY'
import json, sys
tsv, mode, m60, mend, interval, ridx = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4], int(sys.argv[5]), open(sys.argv[6]).read().strip()
rows = [l.rstrip("\n").split("\t") for l in open(tsv)][1:]
anon = [int(r[3]) / 1e6 for r in rows]
per_min = max(1, 60 // interval)
first, last = sum(anon[:per_min]) / len(anon[:per_min]), sum(anon[-per_min:]) / len(anon[-per_min:])
rng = max(anon) - min(anon)
head, tail = anon[:per_min], anon[-2 * per_min:]
flat = lambda xs: max(xs) - min(xs) <= 0.3
if last - first > 1.0:
    verdict = "step (one rise, then flat)" if flat(head) and flat(tail) else "growing"
else:
    verdict = "flat" if rng <= 1.0 else "zig-zag"
print(f"idle RssAnon, {len(anon)} samples every {interval}s after the last sign-in (measured {open(mode).read().strip()}):")
print(f"  min {min(anon):.1f} MB  max {max(anon):.1f} MB  range {rng:.1f} MB  first minute {first:.1f} MB  last minute {last:.1f} MB")
print("  series (MB): " + " ".join(f"{a:.1f}" for a in anon[::max(1, len(anon)//30)]))
print(f"  verdict: {verdict}")
print(f"  the +60 s debug read finished at sample {ridx} ('-' = control run, no debug reads)")
for label, f in (("+60 s", m60), ("end", mend)):
    try:
        m = json.load(open(f))
        print(f"  memstats {label}: HeapInuse {m['heap_inuse']/1e6:.1f} MB, HeapIdle-HeapReleased {(m['heap_idle']-m['heap_released'])/1e6:.1f} MB, NumGC {m['num_gc']}, goroutines {m['goroutines']}")
    except Exception as e:
        print(f"  memstats {label}: not read ({'control run' if ridx == '-' else e})")
PY
