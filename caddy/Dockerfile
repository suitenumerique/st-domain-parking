# The server: Caddy with only the modules main.go lists, our vendored
# certmagic-s3 storage backend and our config adapter, on a distroless base.
#
# Context is this directory: a Go module (go.mod, main.go, adapter/) with
# certmagic-s3/ substituted by a replace directive, so what ships is what is
# in the repo. See certmagic-s3/VENDORED.md for the local changes on top of
# upstream.

# Digests resolved on 2026-10-21. GO_IMAGE is also pinned in the Makefile and
# builder/Dockerfile.
ARG GO_IMAGE=golang:1.27.3@sha256:f20d83a83e18dbd2a4714b8ffd4b30b287b6243962dd75608e86abcc63ba4efc
ARG DISTROLESS_STATIC_IMAGE=gcr.io/distroless/static-debian13:nonroot@sha256:d62067879de890a1def9da0e7766b7d20810a5e5555cef8506b752437d7a3e88

FROM ${GO_IMAGE} AS build

WORKDIR /src
COPY go.mod go.sum ./
COPY certmagic-s3/go.mod certmagic-s3/go.sum ./certmagic-s3/
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . ./
# CGO stays off: the result has to run on distroless. The tags leave out code
# nothing here can reach:
# - nobadger, nomysql, nopgx, nobbolt: smallstep's database drivers, linked
#   through the internal CA but used only by an ACME server we do not build.
# - grpcnotrace: x/net/trace, which gRPC (via OpenTelemetry) would link.
# - nethttpomithttp2: the standard library's HTTP/2. The server speaks HTTP/1.1
#   only, and the S3 and ACME clients fall back to it. Any config that enables
#   h2 now makes Caddy panic at startup, `caddy file-server` included.
ENV CGO_ENABLED=0
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags='-s -w' \
    -tags nobadger,nomysql,nopgx,nobbolt,grpcnotrace,nethttpomithttp2 -o /out/caddy .

# Fail the build rather than the deployment if the module set is not exactly
# modules.txt: a module that did not make it in, or one that came in with a
# dependency upgrade. Regenerate it with the same pipeline after reviewing.
RUN /out/caddy list-modules | grep '^[a-z]' | sort | diff modules.txt -

# distroless has no shell, so the data directory is created and owned here.
RUN mkdir -p /state/data && chown -R 65532:65532 /state

FROM ${DISTROLESS_STATIC_IMAGE}

COPY --from=build /out/caddy /usr/bin/caddy
COPY Caddyfile /etc/caddy/Caddyfile
COPY --from=build --chown=65532:65532 /state/data /data

# Certificates live in S3 (see the `storage` block in the Caddyfile) and the
# config is not persisted, so /data holds only the instance ID.
ENV XDG_DATA_HOME=/data

# Unprivileged, so the low ports are out of reach: publish them from the host
# instead (`-p 80:8080 -p 443:8443`). Caddy emits port-less redirects, so the
# public URLs stay clean.
ENV HTTP_PORT=8080
ENV HTTPS_PORT=8443
EXPOSE 8080 8443

ENTRYPOINT ["/usr/bin/caddy"]
# The Caddyfile adapter plus the certificate cache capacity, see adapter/.
CMD ["run", "--config", "/etc/caddy/Caddyfile", "--adapter", "parking"]
