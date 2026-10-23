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
| Caddy version | `caddy/go.mod`, the `github.com/caddyserver/caddy/v2` require | GitHub releases of `caddyserver/caddy`, then `go mod tidy` in `caddy/` |
| RustFS image | `compose.yaml`, `image: rustfs/rustfs:` | GitHub releases of `rustfs/rustfs`; digest from Docker Hub |
| Go image | `Makefile` `GO_IMAGE`, `builder/Dockerfile` and `caddy/Dockerfile` `ARG GO_IMAGE`, and the two `TESTING.md` examples | latest `golang` release on Docker Hub, at least the `go` directive of Caddy's own `go.mod`; digest from Docker Hub |
| Builder module | `builder/go.mod`, the `go` directive and every require | `go get -u ./... && go mod tidy` in the Go image; the `go` directive follows the Go image |
| Vendored module | `caddy/certmagic-s3/go.mod`, the `go` directive and the direct requires | `https://proxy.golang.org/<module>/@latest`; the `go` directive follows the Go image |
| Caddy module | `caddy/go.mod`, the `go` directive | follows the Go image; the direct requires follow "Caddy version" and "certmagic-s3 fork point", then `go mod tidy` |
| certmagic-s3 fork point | `caddy/certmagic-s3/VENDORED.md`, "Forked at", and the `techknowlogick/certmagic-s3` require in `caddy/go.mod` | `git ls-remote --tags https://github.com/techknowlogick/certmagic-s3` |

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
  `VENDORED.md`, the certmagic-s3 require in `caddy/go.mod`, the
  certmagic-s3 paragraph of `README.md`, and any test name quoted in
  `TESTING.md`.
- `caddy/modules.txt`: after a Caddy upgrade the image build fails if the
  set of registered modules changed. Do not just regenerate it. Tell the user
  which modules appeared or went away, and why (`go mod why`), first.
- `TESTING.md` quotes the Go image twice. Update both.

## Tests and the CVE gate

`TESTING.md` is the reference. The full sweep is `make install`,
`make lint-check`, `make test`, `make build`, `make vendor-test`,
`make test-caddy`, `make test-e2e`. Run it on the clean tree first, then
after each group of changes, so a failure points at one change.

The grype gate is Tier 4 of `TESTING.md`: `grype docker:<image> --only-fixed --fail-on low`
on both built images. A finding that has a fix is a finding to fix. Report
the leftovers to the user with their reason: a fix only in a release line
the project cannot use yet, or a version range grype matches too widely
(check the CVE record). The user decides what to do with them.

Tell the user before editing: a pin this table does not list, and an upstream
certmagic-s3 tag newer than the fork point.
