---
name: upgrade
description: Upgrades and re-pins everything in st-domain-parking (base images, the GitHub Action, Go dependencies, the vendored certmagic-s3 plugin), then runs the test suite and a grype scan. Use when the user asks to upgrade, re-pin, bump dependencies, rebase certmagic-s3, or scan for CVEs.
---

# Upgrade

Re-fetch every value at run time. Do not trust a version written in a doc.

## Where each pin lives, and where to look up the latest value

| What | File | Latest value |
| --- | --- | --- |
| `actions/checkout` SHA + `# vX.Y.Z` comment | `.github/workflows/ci.yml`, every `uses:` | `https://api.github.com/repos/actions/checkout/git/ref/tags/<tag>` (dereference an annotated tag through `git/tags/<sha>`) |
| Debian and distroless bases | `builder/Dockerfile` and `caddy/Dockerfile`, the `ARG *_IMAGE=` lines | registry digests (see below) |
| Caddy version and builder digest | `caddy/Dockerfile`, `ARG CADDY_VERSION`, `ARG CADDY_BUILDER_DIGEST` | GitHub releases of `caddyserver/caddy`; digest from Docker Hub |
| RustFS image | `compose.yaml`, `image: rustfs/rustfs:` | GitHub releases of `rustfs/rustfs`; digest from Docker Hub |
| Go image | `Makefile` `GO_IMAGE`, `builder/Dockerfile` `ARG GO_IMAGE`, and the two `TESTING.md` examples | the `GOLANG_VERSION` env in the Caddy builder image config; digest from Docker Hub |
| Builder module | `builder/go.mod`, the `go` directive and every require | `go get -u ./... && go mod tidy` in the Go image; the `go` directive follows the Caddy builder's Go |
| Vendored module | `caddy/certmagic-s3/go.mod`, the `go` directive and the direct requires | `https://proxy.golang.org/<module>/@latest`; the `go` directive follows the Caddy builder's Go |
| Config adapter module | `caddy/adapter/go.mod`, the `go` directive and the `caddy/v2` require | the `caddy/v2` require follows `CADDY_VERSION`, then `go mod tidy`; the `go` directive follows the Caddy builder's Go |
| certmagic-s3 fork point | `caddy/certmagic-s3/VENDORED.md`, "Forked at", and `caddy/Dockerfile` `ARG CERTMAGIC_S3_VERSION` | `git ls-remote --tags https://github.com/techknowlogick/certmagic-s3` |

Image digest: `HEAD /v2/<repo>/manifests/<tag>` on the registry with
`Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json`,
read `docker-content-digest`. Docker Hub needs a token from
`auth.docker.io/token?service=registry.docker.io&scope=repository:<repo>:pull`
(`library/<name>` for official images). ghcr.io needs a token from
`ghcr.io/token?scope=repository:<repo>:pull`. gcr.io is anonymous.
A registry can re-push a tag; a digest that moved is a legitimate update.

## Rules that are easy to miss

- The builder's stylesheet is only whitespace-stripped (`stripCSS` in
  `builder/render.go`). A `tdewolff/minify` upgrade must not bring a CSS
  minifier back in: it rewrites values to syntax older browsers drop. The
  stylesheet tests catch that.
- The vendored module: read `caddy/certmagic-s3/VENDORED.md` first. Diff the
  upstream fork commit against the target tag, decide which `LOCAL CHANGE`
  hunks upstream absorbed, re-apply the rest with their tag, then
  `make vendor-update` (it runs `go get -u`, `go mod tidy`, vet and tests).
  Do not run `go mod download all`: it inflates `go.sum`. Update
  `VENDORED.md`, the `caddy/Dockerfile` header, the certmagic-s3 paragraph
  of `README.md`, and any test name quoted in `TESTING.md`.
- `TESTING.md` quotes the Go image twice. Update both.

## Tests and the CVE gate

`TESTING.md` is the reference. The full sweep is `make install`,
`make lint-check`, `make test`, `make build`, `make vendor-test`,
`make test-adapter`, `make test-e2e`. Run it on the clean tree first, then
after each group of changes, so a failure points at one change.

The grype gate is Tier 4 of `TESTING.md`: `grype docker:<image> --only-fixed --fail-on low`
on both built images. A finding that has a fix is a finding to fix. Report
the leftovers to the user with their reason: a fix only in a release line
the project cannot use yet, or a version range grype matches too widely
(check the CVE record). The user decides what to do with them.

Tell the user before editing: a pin this table does not list, and an upstream
certmagic-s3 tag newer than the fork point.
