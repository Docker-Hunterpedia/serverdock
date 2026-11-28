#!/bin/sh
# Checks the dev loop (plan 0003, step 11a): a POST through Vite's HTTPS dev
# proxy reaches the server and is answered by it, and the server's Origin rule
# is NOT loosened (a foreign Origin is still refused). Needs Go, Node (with
# web/node_modules installed), curl and openssl. Exit 0 when both hold.
set -eu
cd "$(dirname "$0")/../.."
ROOT=$(pwd)
D=$(mktemp -d /tmp/sdwebchk.XXXXXX)
chmod 700 "$D"
PIDS=""
cleanup() {
	for p in $PIDS; do kill "$p" 2>/dev/null || true; done
	wait 2>/dev/null || true
	rm -rf "$D"
}
trap cleanup EXIT INT TERM
fail() { echo "dev-proxy-check: $*" >&2; [ -f "$D/vite.log" ] && tail -5 "$D/vite.log" >&2; [ -f "$D/server.log" ] && tail -5 "$D/server.log" >&2; exit 1; }

# Free high ports, so two runs (or CI) cannot collide.
port() { python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])'; }
HTTP=$(port); HTTPS=$(port); VITE=$(port)

go build -o "$D/serverdock" ./cmd/serverdock
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 1 \
	-keyout "$D/key.pem" -out "$D/cert.pem" -subj "/CN=localhost" \
	-addext "subjectAltName=DNS:localhost,IP:127.0.0.1" >/dev/null 2>&1
# --docker-socket names a path that does not exist: the server creates its app
# network in Docker at start, and this check must never do that on the machine's
# real Docker (it would leak one network per run).
"$D/serverdock" server --data "$D/data" --control-socket "$D/c.sock" \
	--listen "127.0.0.1:$HTTP" --https-listen "127.0.0.1:$HTTPS" \
	--docker-socket "$D/no-docker.sock" 2>"$D/server.log" &
PIDS="$!"
cd "$ROOT/web"
VITE_DEV_KEY="$D/key.pem" VITE_DEV_CERT="$D/cert.pem" VITE_DEV_API="https://127.0.0.1:$HTTPS" \
	npx vite dev --host 127.0.0.1 --port "$VITE" --strictPort >"$D/vite.log" 2>&1 &
PIDS="$PIDS $!"

i=0
until curl -fsSk -o /dev/null "https://127.0.0.1:$VITE/" 2>/dev/null && curl -fsSk -o /dev/null "https://127.0.0.1:$HTTPS/healthz" 2>/dev/null; do
	i=$((i + 1)); [ "$i" -lt 120 ] || fail "the dev servers did not come up"; sleep 0.5
done

BASE="https://localhost:$VITE"
RES="--resolve localhost:$VITE:127.0.0.1"
# 1. A POST with the browser's own Origin: it reaches the server (401 from the login handler).
code=$(curl -sk $RES -o "$D/body1" -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" \
	-H 'Content-Type: application/json' -H "Origin: $BASE" -d '{"username":"admin","password":"x"}')
[ "$code" = 401 ] || fail "POST through the proxy answered $code (want 401 from the server): $(cat "$D/body1")"
grep -q '"invalid_credentials"' "$D/body1" || fail "the answer did not come from the sign-in handler: $(cat "$D/body1")"
# 2. The Origin rule is intact: a foreign Origin is refused by the server itself.
code=$(curl -sk $RES -o "$D/body2" -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" \
	-H 'Content-Type: application/json' -H 'Origin: https://evil.test' -d '{"username":"admin","password":"x"}')
[ "$code" = 403 ] || fail "a foreign Origin answered $code (want 403)"
grep -q '"origin"' "$D/body2" || fail "the refusal was not the Origin rule: $(cat "$D/body2")"
# 3. The Host allowlist is intact too.
code=$(curl -sk $RES -o /dev/null -w '%{http_code}' -H 'Host: evil.test' "https://127.0.0.1:$VITE/api/v1/setup/status")
[ "$code" = 421 ] || [ "$code" = 404 ] || [ "$code" = 403 ] || echo "dev-proxy-check: note: a foreign Host through Vite answered $code" >&2
echo "dev-proxy-check: ok (POST through the dev proxy answered by the server; the Origin rule is not loosened)"
