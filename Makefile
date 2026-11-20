VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
GOVULNCHECK_VERSION := v1.10.0

.PHONY: build test lint vuln fmt bench openapi web dev web-types
# build embeds the real app when web/build exists (run `make web` first); without
# it the binary has the placeholder page and `serverdock version` says ui=placeholder.
build:
	CGO_ENABLED=0 go build $(if $(wildcard web/build/index.html),-tags spa) -trimpath -ldflags "$(LDFLAGS)" -o bin/serverdock ./cmd/serverdock

test:
	go test -race ./...

lint:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)
	go vet ./...

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

fmt:
	gofmt -w .

bench:
	bench/run.sh

# Regenerate docs/api/openapi.json (the golden file TestOpenAPIGolden compares against).
openapi:
	go test ./internal/api -run TestOpenAPIGolden -update

# The single-page app (needs Node, see web/.nvmrc): web/build, gzip-twinned, size-checked.
web:
	cd web && npm ci --ignore-scripts && npm run check && npm run build

# Server plus Vite over HTTPS with a throwaway certificate (web/scripts/dev.sh).
dev:
	web/scripts/dev.sh

# Regenerate web/src/lib/api/types.ts from the OpenAPI golden.
web-types:
	cd web && npm run types
