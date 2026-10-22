# The page builder: one static Go binary on distroless. The server is a
# separate image, built from caddy/.
#
# The `s3dev` target is the S3 client for local development and the
# end-to-end test (see builder/cmd/s3dev). It is not in the builder image.

# Digests resolved on 2026-10-21. GO_IMAGE is also pinned in the Makefile.
ARG GO_IMAGE=golang:1.27.3@sha256:f20d83a83e18dbd2a4714b8ffd4b30b287b6243962dd75608e86abcc63ba4efc
ARG DISTROLESS_STATIC_IMAGE=gcr.io/distroless/static-debian13:nonroot@sha256:d62067879de890a1def9da0e7766b7d20810a5e5555cef8506b752437d7a3e88

FROM ${GO_IMAGE} AS build

WORKDIR /src
COPY builder/go.mod builder/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY builder/ ./
# CGO stays off: the binaries have to run on distroless static.
ENV CGO_ENABLED=0
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags='-s -w' -o /out/builder . \
    && go build -trimpath -ldflags='-s -w' -o /out/s3dev ./cmd/s3dev

# distroless has no shell, so the output directory is created and owned here.
RUN mkdir -p /state/sites && chown -R 65532:65532 /state

FROM ${DISTROLESS_STATIC_IMAGE} AS s3dev
COPY --from=build /out/s3dev /usr/local/bin/s3dev
ENTRYPOINT ["/usr/local/bin/s3dev"]

FROM ${DISTROLESS_STATIC_IMAGE} AS builder
COPY --from=build /out/builder /usr/local/bin/builder
COPY --from=build --chown=65532:65532 /state/sites /srv/sites
ENV OUTPUT_DIR=/srv/sites
ENTRYPOINT ["/usr/local/bin/builder"]
