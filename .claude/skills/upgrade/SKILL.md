---
name: upgrade
description: Upgrades and re-pins everything in st-domain-parking (base images, the GitHub Action, Python and Go dependencies, the vendored certmagic-s3 plugin), then runs the test suite and a grype scan. Use when the user asks to upgrade, re-pin, bump dependencies, rebase certmagic-s3, or scan for CVEs.
---

# Upgrade

Re-fetch every value at run time. Do not trust a version written in a doc.

## Where each pin lives, and where to look up the latest value

| What | File | Latest value |
| --- | --- | --- |
| `actions/checkout` SHA + `# vX.Y.Z` comment | `.github/workflows/ci.yml`, every `uses:` | `https://api.github.com/repos/actions/checkout/git/ref/tags/<tag>` (dereference an annotated tag through `git/tags/<sha>`) |
| uv version and digest | `Dockerfile`, `ARG UV_VERSION`, `ARG UV_DIGEST` | GitHub releases of `astral-sh/uv`; digest from `ghcr.io` |
| Python version | `Dockerfile`, `ARG PYTHON_VERSION` | newest `cpython-3.X.Y` asset in the latest `astral-sh/python-build-standalone` release |
| Debian and distroless bases | `Dockerfile` and `caddy/Dockerfile`, the `ARG *_IMAGE=` lines | registry digests (see below) |
| Caddy version and builder digest | `caddy/Dockerfile`, `ARG CADDY_VERSION`, `ARG CADDY_BUILDER_DIGEST` | GitHub releases of `caddyserver/caddy`; digest from Docker Hub |
| RustFS image | `compose.yaml`, `image: rustfs/rustfs:` | GitHub releases of `rustfs/rustfs`; digest from Docker Hub |
| Go test image | `Makefile`, `GO_IMAGE`, repeated in the `TESTING.md` stress-test example | the `GOLANG_VERSION` env in the Caddy builder image config; digest from Docker Hub |
| Python packages | `pyproject.toml`, every `==` pin, `requires-python`, the classifier, `target-version`, the `uv_build` bound | `https://pypi.org/pypi/<name>/json` `.info.version` |
| Go module | `caddy/certmagic-s3/go.mod`, the `go` directive and the direct requires | `https://proxy.golang.org/<module>/@latest`; the `go` directive follows the Caddy builder's Go |
| certmagic-s3 fork point | `caddy/certmagic-s3/VENDORED.md`, "Forked at" | `git ls-remote --tags https://github.com/techknowlogick/certmagic-s3` |

Image digest: `HEAD /v2/<repo>/manifests/<tag>` on the registry with
`Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json`,
read `docker-content-digest`. Docker Hub needs a token from
`auth.docker.io/token?service=registry.docker.io&scope=repository:<repo>:pull`
(`library/<name>` for official images). ghcr.io needs a token from
`ghcr.io/token?scope=repository:<repo>:pull`. gcr.io is anonymous.
A registry can re-push a tag; a digest that moved is a legitimate update.

## Rules that are easy to miss

- Before you move the Python minor, check every dependency that ships no
  pure-Python wheel, transitive ones included. List them from `uv.lock`: a
  package whose `wheels` hold no `none-any.whl` file. For each one,
  `https://pypi.org/pypi/<name>/json` `.urls[].filename` must contain
  `cp3XY-cp3XY-manylinux` and `x86_64` for the new minor. Stay on the newest
  minor that has them all. A package with a pure-Python fallback in its
  sdist, such as `markupsafe`, and a package with `py3-none` wheels, such as
  `ruff`, do not block the move.
- `uv.lock` must be regenerated on the new interpreter: build the `uv` stage
  (`docker build --target uv -t uv-stage .`) and run `uv lock --upgrade` in
  it with the repo mounted at `/app`. `make lock` cannot do it: `builder-dev`
  syncs `--frozen` from the old lock first.
- The `python-stripped` stage of `Dockerfile` derives `python3.X` from
  `PYTHON_VERSION` and selects the `cpython-*` directory by name. Keep both.
- Raise the `uv_build` upper bound in `pyproject.toml` when the uv minor moves.
- The vendored module: read `caddy/certmagic-s3/VENDORED.md` first. Diff the
  upstream fork commit against the target tag, decide which `LOCAL CHANGE`
  hunks upstream absorbed, re-apply the rest with their tag, then
  `make vendor-update` (it runs `go get -u`, `go mod tidy`, vet and tests).
  Do not run `go mod download all`: it inflates `go.sum`. Update
  `VENDORED.md`, the `caddy/Dockerfile` header, the certmagic-s3 paragraph
  of `README.md`, and any test name quoted in `TESTING.md`.
- `TESTING.md` quotes the unit-test count in two places and the Go image
  once. Update every one.

## Tests and the CVE gate

`TESTING.md` is the reference. The full sweep is `make install`,
`make lint-check`, `make test`, `make build`, `make vendor-test`,
`make test-e2e`. Run it on the clean tree first, then after each group of
changes, so a failure points at one change.

The grype gate is Tier 4 of `TESTING.md`: `grype docker:<image> --only-fixed --fail-on low`
on both built images. A finding that has a fix is a finding to fix. Report
the leftovers to the user with their reason: a fix only in a release line
the project cannot use yet, or a version range grype matches too widely
(check the CVE record). The user decides what to do with them.

Tell the user before editing: a pin this table does not list, a Python minor
without wheels, and an upstream certmagic-s3 tag newer than the fork point.
