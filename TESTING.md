# Local test protocol

Four layers, cheapest first. Each one is worth running on its own; the point of
the split is that you should not need the slow ones to find out you have a typo.

Every command here has been run against this tree. Timings are from a warm
cache — a cold run is dominated by image builds, not by the tests.

## Before you start

```sh
make install          # builds the images and pulls the Go image the other targets run in
```

- **Docker** with Compose v2. Nothing else: every target below runs in a
  container, including the tests and the linters. If a command here works only
  after you install something on the host, that is a bug in the Makefile.
- **~10 GB of free disk.** The Caddy image compiles Caddy from source. A disk that fills mid-build fails late and blames the wrong thing —
  if a build dies confusingly, check `df -h` first.
- **Ports 8080, 8443 and 9000 free**, or set `HTTP_BIND`, `HTTPS_BIND` and
  `S3_BIND`.

## Tier 1 — on every save (~15 s)

```sh
make test             # the builder's unit tests, with -race
make lint-check       # gofmt, go vet, and the Caddyfile
```

Covers the page builder: domain-list validation and its error paths,
rendering, escaping, the stylesheet's syntax, compression, pruning, and the
signal handling and exit codes (which run the builder as a real child process).

Does **not** touch Caddy or S3.

Narrow it while you work:

```sh
docker run --rm -v "$PWD/builder":/src -w /src \
  golang:1.27.3@sha256:f20d83a83e18dbd2a4714b8ffd4b30b287b6243962dd75608e86abcc63ba4efc \
  go test -run SIGHUP .
```

## Tier 2 — before pushing changes under caddy/ (~60 s)

```sh
make vendor-test      # gofmt, go vet, go test -race
make test-caddy       # the same, for main.go, the config adapter and the TLS permission
```

The `-race` matters: the storage plugin runs a refresher goroutine per held
lock. The lock tests drive a real AWS SDK client against an in-process fake
bucket that enforces `If-None-Match` and `If-Match`, so they prove the
conditional headers reach the wire — not merely that a mock was called.

Concurrency bugs hide from single runs. Before changing anything about
locking, hammer it:

```sh
docker run --rm -v "$PWD/caddy/certmagic-s3":/src -w /src \
  golang:1.27.3@sha256:f20d83a83e18dbd2a4714b8ffd4b30b287b6243962dd75608e86abcc63ba4efc \
  go test -race -count=50 -run TestLockIsExclusiveAcrossInstances ./...
```

Keep this image the same as `GO_IMAGE` in the Makefile and
`builder/Dockerfile`.

## Tier 3 — before deploying (~13 s warm)

```sh
make test-e2e         # the whole stack against RustFS
```

`test-e2e` is the only layer that proves the pieces fit: routing, the on-demand
TLS allowlist, pre-compressed variants being served, certificates landing in
S3, and that storage cleaning spares objects outside the prefix.

It also checks the helpcenter redirect: two listed hosts get a certificate
and redirect to the configured target, on the root path and on a sub-path.
It checks that `/__lbheartbeat__` answers 200 on both listeners, with and
without the trailing slash, and also on a helpcenter host.
It checks the Caddy logs: they contain access lines, no heartbeat path, and
no SDK debug line. It checks the hardening: HTTP/1.1 only and no `Alt-Svc`,
no keep-alive, 421 when the Host does not match the SNI, 405 for a POST, a
404 rather than a redirect for an unknown host on HTTP, and no config
autosave.

The image build itself checks that the binary registers exactly the modules
in `caddy/modules.txt`.

It uses Caddy's internal CA (`CADDY_TLS_ISSUER=internal`), since no public DNS
points at a laptop. `scripts/e2e.compose.yaml` pins the DNS records instead,
so the permission's DNS check runs for real: a parked host whose record
points elsewhere is refused a certificate. Everything else is the real thing.

To keep the stack up afterwards and poke at it:

```sh
KEEP_UP=1 ./scripts/e2e.sh
docker compose down -v          # when you are done
```

## Full sweep

```sh
make install && make lint-check && make test && make vendor-test && make test-caddy && make test-e2e
```

Expect: `Valid configuration`, `ok  …builder`, `ok  …certmagic-s3`,
`ok  …caddy/adapter`, `All end-to-end checks passed`.

## Tier 4 — vulnerability scan (~1 min)

Run [grype](https://github.com/anchore/grype) on the two images the stack
builds. The gate fails on any finding that has a fix.

```sh
docker compose build builder caddy
grype db update
grype docker:st-domain-parking-builder:latest --only-fixed --fail-on low
grype docker:st-domain-parking-caddy:latest --only-fixed --fail-on low
```

Expect: no finding, or only findings whose fix the project cannot take
yet. Decide what to do with each one, and check its CVE record: grype can
match a version range too widely.

The Debian packages in the distroless base can carry findings with no fix in
Debian 13. `--only-fixed` leaves them out of the gate; the full report
`grype docker:<image>` still lists them.

## Checks nothing automates

**Look at the page.** The tests assert on syntax; only a browser tells you it
looks right.

```sh
make build            # writes ./build, owned by you rather than by root
# then open build/www.brigny.fr/index.html
```

Narrow the window under 560 px. The mobile layout collapsing is what
`stripCSS` in `builder/render.go` protects by only stripping whitespace: a CSS
minifier that rewrites the media query to Level 4 range syntax makes it
silently vanish on older browsers, and no test in this repo renders anything.

**SIGHUP refetches without stopping the builder.**

```sh
make run
docker compose logs builder | grep -c "Build complete"    # note the number
docker compose kill -s HUP builder
docker compose logs builder | grep -c "Build complete"    # one more
docker compose ps builder                                 # still Up
```

**Credentials stay out of the logs.** certmagic logs the storage value on every
cleaning pass; the plugin has a `MarshalLogObject` and a `String()` to keep the
keys out of both spellings.

```sh
docker compose logs caddy --no-color | grep -c rustfsadmin      # must be 0
```

**No lock files are left behind.** A leaked `.lock` object blocks the next
issuance until it expires.

```sh
docker compose run --rm --no-deps bucket-init keys | grep '\.lock$'   # must be empty
```

**Two instances on one bucket** — a smoke test, see the gap below.

```sh
CADDY_TLS_ISSUER=internal docker compose up -d
CADDY_TLS_ISSUER=internal docker compose run -d --no-deps --name caddy2 caddy
docker logs caddy2 | grep "serving initial configuration"
docker rm -f caddy2 && docker compose down -v
```

`failed to install root certificate … "tee": executable file not found` is
expected and harmless: the image is distroless, and the local CA root only
matters inside the container.

## Known gaps

Worth knowing before you trust a green run.

- **Nothing races two Caddys for the same certificate.** The locking is unit-
  tested against the fake bucket and verified by hand against RustFS, and the
  procedure above only shows two instances can share a bucket. Nothing asserts
  that two simultaneous issuances of the same name produce one ACME order.
- **No real ACME server is exercised.** The e2e uses the internal CA, so
  rate limits, challenge types and issuer quirks are untested here.
- **Nothing renders the page.** Every visual property is checked as a string.
- **The domain list is only ever read from a file locally.** The `https://`
  and `s3://` paths in `builder/domains.go` are unit-tested against local
  fakes, not against a real server or store.

## Troubleshooting

| Symptom | Cause |
| --- | --- |
| The Caddy build dies with an odd error | Disk. `df -h`, then `docker system prune -af` |
| `port is already allocated` | Set `HTTP_BIND` / `HTTPS_BIND` / `S3_BIND` |
| e2e passes but leaves containers | It ran with `KEEP_UP=1`; `docker compose down -v` |
| `make lint-caddy` rebuilds every time | Expected: it runs `compose run --build` |
| A Go test fails only in the full run | Suspect a leaked goroutine, not the test. Re-run with `-race` |
| A target needs something installed on the host | A bug: everything runs in a container |
