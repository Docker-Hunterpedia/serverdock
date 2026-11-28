VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
GOVULNCHECK_VERSION := v1.10.0

.PHONY: build test lint vuln fmt bench openapi web dev web-types e2e screens
# build embeds the real app when web/build exists (run `make web` first); without
# it the binary has the placeholder page and `serverdock version` says ui=placeholder.
build:
	CGO_ENABLED=0 go build $(if $(wildcard web/build/index.html),-tags spa) -trimpath -ldflags "$(LDFLAGS)" -o bin/serverdock ./cmd/serverdock
	@bin/serverdock version

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

# Regenerate web/src/lib/api-types.ts from the OpenAPI golden.
web-types:
	cd web && npm run types

# Playwright + axe against bin/serverdock (run `make web build` first; first time:
# cd web && npx playwright install chromium webkit).
e2e:
	cd web && npx playwright test

# Preview the website's screenshots (plan 0014): builds the web UI and the binary, then runs the capture
# into a scratch folder (SCREENS_OUT, default /tmp/serverdock-screens), never into site/public/screens/.
# Local captures differ from CI in fonts and rendering: only the CI artifact `site-screens` may be committed.
SCREENS_OUT ?= /tmp/serverdock-screens
screens: web build
	cd web && npx playwright install chromium && SCREENS_OUT=$(SCREENS_OUT) npm run screens
	@echo "preview written to $(SCREENS_OUT); commit only the CI artifact (see CONTRIBUTING.md, Refresh screenshots)"
