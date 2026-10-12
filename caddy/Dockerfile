# The server: Caddy built with our vendored certmagic-s3 storage backend, on a
# distroless base.
#
# Context is this directory — the Caddyfile plus certmagic-s3/, which is built
# from local source rather than fetched by version. See certmagic-s3/VENDORED.md
# for why (the latest release deletes the whole bucket during OCSP cleanup) and
# for the local fixes on top.

ARG CADDY_VERSION=2.11.7

# Digests resolved on 2026-10-12.
ARG CADDY_BUILDER_DIGEST=sha256:b25f47453fa02f7e66c0828b4b1343658808b9000950b6825c4a1cb0c988f5f8
ARG DEBIAN_IMAGE=debian:trixie-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f
ARG DISTROLESS_STATIC_IMAGE=gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3

# ---- Build Caddy with the storage plugin ----
FROM caddy:${CADDY_VERSION}-builder@${CADDY_BUILDER_DIGEST} AS build

WORKDIR /src
COPY certmagic-s3/ ./certmagic-s3/

# `--with <module>=<dir>` rewrites the import to this directory, so what ships
# is what is in the repo. CGO stays off: the result has to run on distroless.
ENV CGO_ENABLED=0
# The builder image exports CADDY_VERSION=v<version>, which xcaddy reads by default.
RUN xcaddy build \
    --output /out/caddy \
    --with github.com/techknowlogick/certmagic-s3=/src/certmagic-s3

# Fail the build rather than the deployment if the module did not make it in.
RUN /out/caddy list-modules | grep -qx 'caddy.storage.s3'

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
CMD ["run", "--config", "/etc/caddy/Caddyfile", "--adapter", "caddyfile"]
