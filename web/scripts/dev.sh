#!/bin/sh
# `make dev`: the server (this checkout's code, high ports, a throwaway data
# directory) and Vite's dev server over HTTPS with a throwaway self-signed
# certificate. Open https://localhost:5173 (accept the certificate warning).
# Vite proxies /api to the server's HTTPS port without changing the Host header,
# so the server's Host allowlist and Origin rule (https:// + Host) apply exactly as
# in production: nothing is loosened for development.
#
# Docker: by default the server gets a --docker-socket that does not exist, so
# it never touches the machine's Docker (it would create one app network per run,
# leaked when the throwaway data directory is removed, and a browser open on this
# host can see one net::ERR_NETWORK_CHANGED when that network first appears). To
# deploy real containers from the dev server, opt in with
#     SD_DEV_DOCKER=1 make dev
# (uses the default socket) or SD_DEV_DOCKER=/path/to/docker.sock (a named one).
# Each opted-in run leaves its app network ("serverdock-<install id>") behind
# when it stops: remove it with `docker network rm`.
set -eu
cd "$(dirname "$0")/../.."
ROOT=$(pwd)
D=$(mktemp -d /tmp/sdweb.XXXXXX)
chmod 700 "$D"
PIDS=""
cleanup() {
	for p in $PIDS; do kill "$p" 2>/dev/null || true; done
	wait 2>/dev/null || true
	rm -rf "$D"
}
trap cleanup EXIT INT TERM

go build -o "$D/serverdock" ./cmd/serverdock
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 1 \
	-keyout "$D/key.pem" -out "$D/cert.pem" -subj "/CN=localhost" \
	-addext "subjectAltName=DNS:localhost,IP:127.0.0.1" >/dev/null 2>&1
chmod 600 "$D/key.pem"

case "${SD_DEV_DOCKER:-}" in
"") DOCKER_ARGS="--docker-socket $D/no-docker.sock" ;;
1) DOCKER_ARGS="" ;;
/*) DOCKER_ARGS="--docker-socket $SD_DEV_DOCKER" ;;
*) echo "SD_DEV_DOCKER must be empty, 1, or an absolute socket path" >&2; exit 2 ;;
esac
# shellcheck disable=SC2086 # DOCKER_ARGS is deliberately split into words
"$D/serverdock" server --data "$D/data" --control-socket "$D/c.sock" \
	--listen 127.0.0.1:8080 --https-listen 127.0.0.1:8443 $DOCKER_ARGS 2>"$D/server.log" &
PIDS="$!"
echo "server: https://localhost:8443 (log: $D/server.log); setup token: $D/data/setup-token"

cd "$ROOT/web"
[ -d node_modules ] || npm ci --ignore-scripts
VITE_DEV_KEY="$D/key.pem" VITE_DEV_CERT="$D/cert.pem" VITE_DEV_API="${VITE_DEV_API:-https://127.0.0.1:8443}" npm run dev &
PIDS="$PIDS $!"
wait
