# The server: Caddy built with our vendored certmagic-s3 storage backend and
# our config adapter, on a distroless base.
#
# Context is this directory — the Caddyfile plus certmagic-s3/ and adapter/,
# which are built from local source rather than fetched by version. See
# certmagic-s3/VENDORED.md for the local changes on top of upstream.

ARG CADDY_VERSION=2.11.7
# The upstream tag certmagic-s3/ is forked from (VENDORED.md, "Forked at").
# Only the version recorded in the binary: the code is the local directory.
ARG CERTMAGIC_S3_VERSION=v1.5.0

# Digests resolved on 2026-10-21.
ARG CADDY_BUILDER_DIGEST=sha256:8910fe662f448b860e8d16ef1c9a4f8946d3193bdc3e971bf0c55c9a2cf246f6
ARG DEBIAN_IMAGE=debian:trixie-slim@sha256:62bd423aeee912639ca8ee681c2b762cb3e25137e0a92b20be455898a9f034ad
ARG DISTROLESS_STATIC_IMAGE=gcr.io/distroless/static-debian13:nonroot@sha256:d62067879de890a1def9da0e7766b7d20810a5e5555cef8506b752437d7a3e88

# ---- Build Caddy with the storage plugin ----
FROM caddy:${CADDY_VERSION}-builder@${CADDY_BUILDER_DIGEST} AS build
ARG CERTMAGIC_S3_VERSION

WORKDIR /src
COPY certmagic-s3/ ./certmagic-s3/
COPY adapter/ ./adapter/

# `--with <module>=<dir>` rewrites the import to this directory, so what ships
# is what is in the repo. Without `@<version>`, xcaddy's `go get` would record
# whatever upstream's latest tag is at build time, and scanners match CVEs on
# that. CGO stays off: the result has to run on distroless.
ENV CGO_ENABLED=0
# The builder image exports CADDY_VERSION=v<version>, which xcaddy reads by default.
RUN xcaddy build \
    --output /out/caddy \
    --with github.com/techknowlogick/certmagic-s3@${CERTMAGIC_S3_VERSION}=/src/certmagic-s3 \
    --with github.com/suitenumerique/st-domain-parking/caddy/adapter=/src/adapter

# Fail the build rather than the deployment if a module did not make it in.
RUN /out/caddy list-modules | grep -qx 'caddy.storage.s3' \
    && /out/caddy list-modules | grep -qx 'caddy.adapters.parking'

# distroless has no shell, so writable directories have to be created and
# owned here.
FROM ${DEBIAN_IMAGE} AS state
RUN mkdir -p /state/data /state/config && chown -R 65532:65532 /state

FROM ${DISTROLESS_STATIC_IMAGE}

COPY --from=build /out/caddy /usr/bin/caddy
COPY Caddyfile /etc/caddy/Caddyfile
COPY --from=state --chown=65532:65532 /state/data /data
COPY --from=state --chown=65532:65532 /state/config /config

# Certificates live in S3 (see the `storage` block in the Caddyfile), so these
# hold only local scratch state. /config still receives the config autosave.
ENV XDG_DATA_HOME=/data
ENV XDG_CONFIG_HOME=/config

# Unprivileged, so the low ports are out of reach: publish them from the host
# instead (`-p 80:8080 -p 443:8443`). Caddy emits port-less redirects, so the
# public URLs stay clean.
ENV HTTP_PORT=8080
ENV HTTPS_PORT=8443
EXPOSE 8080 8443

ENTRYPOINT ["/usr/bin/caddy"]
# The Caddyfile adapter plus the certificate cache capacity, see adapter/.
CMD ["run", "--config", "/etc/caddy/Caddyfile", "--adapter", "parking"]
