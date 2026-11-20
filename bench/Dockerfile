# Bench image: the shipped binary on distroless, run as nonroot (uid 65532).
# Build context is the repository root:  docker build -f bench/Dockerfile .
FROM golang:1.27@sha256:205c7636d74e0ffbe3d007162df7d74f859f5a8c8322dfb9f907977686c126cf AS build
WORKDIR /src
COPY . .
ARG VERSION=bench
# No separate `go mod download`: it fetches every module in the graph, while
# `go build` only fetches the modules the binary imports. Cache mounts keep
# repeat builds fast.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/serverdock ./cmd/serverdock \
 && mkdir -p /out/data /out/debug && chmod 700 /out/debug

FROM gcr.io/distroless/static:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931
COPY --from=build /out/serverdock /serverdock
# Writable data dir for the nonroot user (distroless has no usable HOME).
# ServerDock refuses to start unless DATA is 0700 and owned by the running uid.
COPY --from=build --chown=65532:65532 --chmod=0700 /out/data /data
# Mount point for the debug unix socket's directory (0700, nonroot-owned): a
# named volume mounted here inherits these permissions.
COPY --from=build --chown=65532:65532 --chmod=0700 /out/debug /debug
USER 65532:65532
ENTRYPOINT ["/serverdock"]
CMD ["server", "--data", "/data"]
