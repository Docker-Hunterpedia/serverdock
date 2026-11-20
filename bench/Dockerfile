# Bench image: the shipped binary on distroless, run as nonroot (uid 65532).
# Build context is the repository root:  docker build -f bench/Dockerfile .
FROM golang:1.27 AS build
WORKDIR /src
COPY . .
ARG VERSION=bench
# No separate `go mod download`: it fetches every module in the graph, while
# `go build` only fetches the modules the binary imports. Cache mounts keep
# repeat builds fast.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/serverdock ./cmd/serverdock \
 && mkdir -p /out/data

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/serverdock /serverdock
# Writable data dir for the nonroot user (distroless has no usable HOME).
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
ENTRYPOINT ["/serverdock"]
CMD ["server", "--data", "/data"]
