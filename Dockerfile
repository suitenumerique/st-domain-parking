# The page builder. The server is a separate image, built from caddy/.
#
# Layout follows src/backend in suitenumerique/messages: uv-managed CPython
# under /opt/python, dependencies in /venv, and a distroless production
# runtime with pip/tests/headers stripped out.

ARG PYTHON_VERSION=3.14.8
ARG UV_VERSION=0.13.4

# Digests resolved on 2026-10-21.
#
# Python is pinned to the newest 3.14.x, not to 3.15: brotli and lightningcss
# ship no cp315 wheels yet, and the builder has to install them from a wheel.
ARG UV_DIGEST=sha256:9f130421e930bfbdc563c00740466603f8723158e90297d07957c2294640262b
ARG DEBIAN_IMAGE=debian:trixie-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f
ARG DISTROLESS_CC_IMAGE=gcr.io/distroless/cc-debian13:nonroot@sha256:e792ab3d241a468a4fd7519ddbbebe66b49b5f365771716ea688ad40b6c6f1c2

FROM ghcr.io/astral-sh/uv:${UV_VERSION}@${UV_DIGEST} AS uv-bin

# ---- uv + managed Python ----
FROM ${DEBIAN_IMAGE} AS uv
ARG PYTHON_VERSION

COPY --from=uv-bin /uv /usr/local/bin/uv

RUN <<EOR
apt-get update
DEBIAN_FRONTEND="noninteractive" apt-get install -y --no-install-recommends \
    ca-certificates
rm -rf /var/lib/apt/lists/*
EOR

# python-build-standalone, installed where the distroless stage can pick it up.
ENV UV_PYTHON_INSTALL_DIR=/opt/python
ENV UV_PYTHON_PREFERENCE=only-managed
ENV UV_PROJECT_ENVIRONMENT=/venv
RUN uv python install ${PYTHON_VERSION}

# ---- Production dependencies ----
FROM uv AS base-with-deps

WORKDIR /app
COPY pyproject.toml uv.lock ./

RUN --mount=type=cache,target=/root/.cache/uv \
    uv sync --frozen --no-install-project --no-editable --exact --no-dev

# ---- Development dependencies ----
FROM base-with-deps AS base-with-deps-dev

RUN --mount=type=cache,target=/root/.cache/uv \
    uv sync --frozen --no-install-project --no-editable --all-extras

# ---- Strip Python for the distroless image ----
# Hardening rather than size: the interpreter sits in an inherited layer either
# way, but pip, the test suite and the headers have no business in a running
# container.
FROM uv AS python-stripped
ARG PYTHON_VERSION

RUN <<EOR
set -eu
root=$(find /opt/python -maxdepth 1 -mindepth 1 -type d -name 'cpython-*')
lib="${root}/lib/python$(printf '%s' "${PYTHON_VERSION}" | cut -d. -f1,2)"
# Fail the build here: rm -rf below would pass silently on a wrong path.
[ -d "${lib}" ]
rm -rf \
    "${lib}/test" \
    "${lib}/idlelib" \
    "${lib}/tkinter" \
    "${lib}/lib2to3" \
    "${lib}/ensurepip" \
    "${lib}/site-packages/pip" \
    "${lib}/site-packages/pip-"* \
    "${root}/include" \
    "${root}/share"
find /opt/python -name '__pycache__' -type d -prune -exec rm -rf {} +
find /opt/python -name 'libpython*.a' -delete
EOR

# ---- Writable output directory, pre-owned ----
# distroless has no shell, so ownership has to be baked in here.
FROM ${DEBIAN_IMAGE} AS state
RUN mkdir -p /state/sites && chown -R 65532:65532 /state

# ---- Production application source ----
FROM ${DEBIAN_IMAGE} AS app-prod
COPY builder/ /app/builder/
RUN <<EOR
rm -rf /app/builder/tests
# Normalise permissions: COPY preserves whatever the build host had, and the
# runtime user is not the owner, so a restrictive umask or an ACL on the
# checkout would otherwise produce an image that cannot read its own code.
chmod -R a=rX,u+w /app
EOR

# ---- Development runtime ----
# Debian-based, with a shell and the dev extras. Handy for debugging; not what
# runs in production.
FROM ${DEBIAN_IMAGE} AS runtime-dev

COPY --from=python-stripped /opt/python /opt/python
COPY --from=base-with-deps-dev /venv /venv

ENV PATH="/venv/bin:$PATH"
ENV VIRTUAL_ENV=/venv
ENV PYTHONUNBUFFERED=1

WORKDIR /app
CMD ["python", "-m", "builder"]

# ---- Builder runtime ----
FROM ${DISTROLESS_CC_IMAGE} AS builder

COPY --from=python-stripped /opt/python /opt/python
COPY --from=base-with-deps /venv /venv
COPY --from=app-prod /app/ /app/
COPY --from=state --chown=65532:65532 /state/sites /srv/sites

ENV PATH="/venv/bin:$PATH"
ENV VIRTUAL_ENV=/venv
ENV PYTHONUNBUFFERED=1
ENV PYTHONDONTWRITEBYTECODE=1
ENV OUTPUT_DIR=/srv/sites

WORKDIR /app
CMD ["python", "-m", "builder"]
