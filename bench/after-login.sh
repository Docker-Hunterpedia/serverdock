#!/bin/sh
# Report-only "S2 after one login" (plan 0003, M2 step 6b): the single-node server
# (scenario 2's flags), first run and one full sign-in over HTTPS, then VmRSS
# before, 10 s after and 60 s after. argon2 allocates ~19 MiB for a moment, so
# this shows what the Go runtime gives back (expected: S2 + at most 1 MB).
#
#   bench/after-login.sh            (needs docker, curl and python3)
#
# Informational only: nothing is gated and nothing is written to bench/results.
set -eu
cd "$(dirname "$0")/.."
IMAGE=${IMAGE:-serverdock-bench:local}
NAME=sdbench-login
WORK=$(mktemp -d "${TMPDIR:-/tmp}/sdlogin.XXXXXX")
cleanup() { docker rm -f "$NAME" >/dev/null 2>&1 || true; rm -rf "$WORK"; }
trap cleanup EXIT
log() { printf '[after-login] %s\n' "$*" >&2; }

[ -n "${SKIP_BUILD:-}" ] || { log "building $IMAGE"; docker build -q -f bench/Dockerfile -t "$IMAGE" . >/dev/null; }
docker rm -f "$NAME" >/dev/null 2>&1 || true
docker run -d --name "$NAME" --memory 1g --cpus 1 -p 127.0.0.1::443 "$IMAGE" \
  server --data /data --proxy=true --listen :80 >/dev/null
ADDR=$(docker port "$NAME" 443/tcp | head -n1)
i=0
until curl -fsSk -o /dev/null "https://$ADDR/healthz" 2>/dev/null; do
  i=$((i + 1)); [ "$i" -lt 60 ] || { log "server never became ready"; exit 1; }; sleep 0.5
done

totp() { # base32 secret -> current 6-digit code
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
post() { # path body -> response body (cookies kept in the jar)
  curl -sSk -b "$WORK/jar" -c "$WORK/jar" -H 'Content-Type: application/json' -H "Origin: https://$ADDR" -X POST "https://$ADDR$1" -d "$2"
}
rss() { # sample VmRSS (bytes) of the container, from outside
  bench/measure.sh "$NAME" 0 1 "$WORK/rss.tsv" >/dev/null 2>&1 && awk 'NR==2{printf "%.1f", $2/1000000}' "$WORK/rss.tsv"
}

log "warm-up 20 s"; sleep 20
BEFORE=$(rss)
docker cp "$NAME:/data/setup-token" "$WORK/token" >/dev/null
TOKEN=$(tr -d '\n' <"$WORK/token")
PASS='correct horse battery staple'
EN=$(post /api/v1/setup/enroll "{\"token\":\"$TOKEN\",\"username\":\"admin\"}")
SECRET=$(printf '%s' "$EN" | json "['secret']"); EID=$(printf '%s' "$EN" | json "['enrollment_id']")
post /api/v1/setup/complete "{\"token\":\"$TOKEN\",\"enrollment_id\":\"$EID\",\"username\":\"admin\",\"password\":\"$PASS\",\"code\":\"$(totp "$SECRET")\"}" >/dev/null
log "first run done; waiting for the next TOTP step, then one sign-in"
sleep 31
rm -f "$WORK/jar"
post /api/v1/auth/login "{\"username\":\"admin\",\"password\":\"$PASS\"}" >/dev/null
CODE=$(post /api/v1/auth/2fa "{\"code\":\"$(totp "$SECRET")\"}" | json "['username']")
[ "$CODE" = admin ] || { log "sign-in failed"; exit 1; }
log "signed in; sampling"
sleep 10; AFTER10=$(rss)
sleep 50; AFTER60=$(rss)
echo "S2 after one login (VmRSS MB): before=$BEFORE  +10s=$AFTER10  +60s=$AFTER60  (report only)"
