# st-domain-parking

Parking pages for communes that do not have a website yet.

A JSON file lists the domains and six fields for each. A small Go builder
turns every entry into one self-contained HTML page, and Caddy serves it over
HTTPS.

## How it works

Two containers share one volume. The builder writes into it, Caddy reads from
it, and they never talk to each other — the directory tree is the whole
interface:

```
/srv/sites/
  www.brigny.fr/
    index.html        the page: inline CSS, no scripts, no external requests
    index.html.br     pre-compressed variants, served as-is by Caddy
    index.html.gz
    .parked           allowlist marker
  brigny.fr/
    .parked           apex, redirected to www
```

Caddy's configuration is **static** and never reloaded. The list of domains it
will serve is the directory tree itself:

- **TLS** is issued on demand, gated by a permission module in our Caddy
  build (`caddy/permission/`). It allows a certificate only when
  `<host>/.parked` exists, so a stranger pointing DNS at the server cannot
  make it request certificates: the TLS handshake is simply refused. With
  `PUBLIC_IPS` set, it also checks that every A/AAAA record of the host
  points here. A parked host whose DNS points elsewhere would otherwise
  make each handshake order a certificate that fails validation, and enough
  of those exhaust the ACME account's rate limits for every other host. The
  check only runs for a parked host whose certificate is not already in
  memory, so about once per host after a restart, and never on rebuilds.
- **Apex → www** is a single global rule.
- **Any path other than `/`** redirects back to `/`.
- **`/favicon.ico`** answers 204: the icon is an inline data URI, and a 204
  stops browsers that ask anyway from retrying.
- **`/robots.txt`** is served, not redirected — a redirected robots.txt reads
  as absent to crawlers.
- **Unknown hosts** get a 404, on HTTP too: only parked and helpcenter hosts
  are redirected to HTTPS.

Adding or removing a domain is therefore a pure file operation: no reload, no
restart, no config regeneration. Every redirect is temporary (302) on purpose —
parking is a placeholder that owners are expected to replace with a real site,
and a 301 would linger in browser caches long after that.

The page is ~4.2 kB of HTML, ~1.6 kB over the wire, in a single request: no
fonts, no stylesheets, no scripts, no images. The favicon is an inline data
URI.

### Search engines

The pages are `noindex`. They are meant to be reached by typing the domain;
the authoritative record for a commune is its Service-Public listing, and
these pages should not compete with it in search results.

`robots.txt` nonetheless **allows** crawling, which is not a contradiction:
`noindex` only works if crawlers are allowed to fetch the page and read it.
A URL blocked in robots.txt can still be indexed bare, from inbound links
alone — the opposite of the intent.

## Requirements

**Docker**, with Compose v2. That is the entire list, and containers are the
only supported way to work on this repo: the tests, the linters, the page
builds and the server all run in them, against the same images that ship. There
is no host toolchain to install, and none to drift out of step.

```sh
make install      # builds the images and pulls the Go image the other targets run in
```

## Running

```sh
make run          # docker compose up --build -d
make logs
make down
```

The example domain list is served by default. Point `DOMAINS_URL` elsewhere for
anything real.

Compose also starts a [RustFS](https://github.com/rustfs/rustfs) container as a
local stand-in for S3, so the stack works end to end — including the real
certificate-storage path — without any cloud account. In production point
`S3_ENDPOINT` at the actual store, or leave it empty for AWS, and drop the
`rustfs` and `bucket-init` services.

Both containers run unprivileged and listen on 8080/8443, so the host publishes
the real ports: `-p 80:8080 -p 443:8443` (see `HTTP_BIND` / `HTTPS_BIND`).
Caddy emits port-less redirects, so public URLs stay clean.

### Opening a page locally

**`https://localhost:8443` will not work, and that is the point.** Caddy issues
certificates on demand, and only for hosts the builder has written out. Nothing
parks `localhost`, so the allowlist refuses it and the handshake fails —
browsers report that as `ERR_SSL_PROTOCOL_ERROR`. (`https://localhost:8080` is
the same error for a duller reason: 8080 is the plain HTTP port.)

Reach a real domain instead. `make hosts` prints the line to add to
`/etc/hosts`:

```sh
make hosts
# 127.0.0.1 www.brigny.fr brigny.fr www.sainte-anne-sur-vilaine.fr sainte-anne-sur-vilaine.fr
sudo sh -c 'echo "127.0.0.1 www.brigny.fr brigny.fr" >> /etc/hosts'

make run
# then open https://www.brigny.fr:8443
```

`make run` uses Caddy's own CA (`CADDY_TLS_ISSUER=internal`), because no public
DNS points at a development machine and the real issuer could only fail — while
spending rate limit to do it. Your browser has no reason to trust that CA, so
expect a warning; `curl` needs `-k`.

Note the port. Redirects are deliberately port-less so public URLs stay clean,
which means a redirect from the apex lands you on `https://www.brigny.fr/`
— port 443, where nothing is listening locally. Go straight to the `www` host
on 8443.

To skip the server altogether and just look at the HTML, `make build` writes
the pages to `./build`, owned by you rather than by root. `make preview` builds
them and serves that directory over HTTP, so a template change can be looked
at in a browser:

```sh
make preview
# http://localhost:8081/www.brigny.fr/index.html
```

The tree is served as it is written, hence the host in the path. This is the
page and nothing else: no TLS, no redirect, no allowlist. `make run` is the
whole stack.

## Configuration

Builder:

| Variable | Default | Meaning |
| --- | --- | --- |
| `DOMAINS_URL` | *(required)* | Location of the JSON file: `https://`, `s3://`, `file://` or a path |
| `REBUILD_INTERVAL` | `300` | Seconds between refetches. `0` builds once and exits |
| `OUTPUT_DIR` | `/srv/sites` | Root of the generated tree |
| `LOG_LEVEL` | `INFO` | `DEBUG`, `INFO`, `WARN` or `ERROR` |

`s3://` URLs use the standard `AWS_*` variables; set `AWS_ENDPOINT_URL` to
point at any S3-compatible store, which is then addressed path-style.

Pages are built in parallel, one worker per CPU. Go sizes that from the
container's CPU limit; `GOMAXPROCS` overrides it.

The first build must succeed or the builder exits. Afterwards a failed refetch
is logged and the previous build keeps being served.

Send `SIGHUP` to refetch immediately instead of waiting out the interval:

```sh
docker compose kill -s HUP builder
```

`SIGTERM` and `SIGINT` stop it, cutting the wait short rather than sleeping out
the rest of the interval.

Caddy:

| Variable | Default | Meaning |
| --- | --- | --- |
| `ACME_EMAIL` | *(none)* | Contact address for Let's Encrypt |
| `CACHE_MAX_AGE` | `300` | `Cache-Control: max-age` on the pages |
| `HTTP_PORT` / `HTTPS_PORT` | `8080` / `8443` | Ports inside the container |
| `PUBLIC_IPS` | *(empty)* | Space-separated addresses the hosts' DNS must point at before a certificate is ordered (see below). Empty skips the check; set it in production |
| `CADDY_TLS_ISSUER` | *(empty)* | Empty is real ACME; `internal` uses Caddy's local CA |
| `HELPCENTER_REDIRECT_HOSTS` | *(empty)* | Space-separated, lowercase hostnames to redirect to the helpcenter |
| `HELPCENTER_REDIRECT_TARGET` | *(empty)* | Absolute URL of the helpcenter page |

`PUBLIC_IPS` is matched against every address the host resolves to:

- **List both families.** Every A *and* AAAA record must be in the list. A host
  with an AAAA record is refused unless our IPv6 address is listed too, since
  the CA may validate over IPv6.
- **CNAMEs work.** The resolver follows the chain and the final addresses are
  compared. Behind a load balancer whose IPs change (some cloud LBs), a fixed
  list goes stale, and every host not yet in the certificate cache is then
  refused.
- **A resolver failure allows.** A timeout or SERVFAIL is not an answer, and
  the check also runs before a certificate is loaded from storage, so refusing
  would stop serving certificates we already have. Only an answer refuses: an
  address elsewhere, no address at all, or NXDOMAIN.
- **A refusal is remembered for a minute**, so a host whose DNS was just fixed
  gets its certificate within a minute.


Certificate storage (see below):

| Variable | Default | Meaning |
| --- | --- | --- |
| `S3_BUCKET` | *(required)* | Bucket holding the certificates |
| `S3_REGION` | `us-east-1` | |
| `S3_ENDPOINT` | *(empty)* | Empty for real AWS; set for RustFS, MinIO, Scaleway, Garage… |
| `S3_ACCESS_KEY` / `S3_SECRET_KEY` | | Omit to use the ambient AWS credential chain |
| `S3_PREFIX` | `caddy` | Key prefix within the bucket |
| `S3_ENCRYPTION_KEY` | *(empty)* | Exactly 32 bytes to encrypt stored keys client-side |

## The Caddy build

`caddy/` is a Go module of our own: `main.go` links only the Caddy modules the
Caddyfile uses, plus the S3 storage, the config adapter below and the
on-demand TLS permission. The rest of
the standard distribution (reverse_proxy, fastcgi, the ACME server, basic auth,
tracing, …) is not in the binary, so no config can turn it on. `go.mod` and
`go.sum` pin every dependency, and the image is a plain `go build` on the same
Go image as the builder.

`caddy/modules.txt` lists every registered module, and the image build fails
if the binary registers a different set. A Caddy upgrade that adds a module
therefore needs a reviewed update of that file.

At runtime the server is cut down to what one 1.6 kB page needs:

- **HTTP/1.1 only.** A single request gains nothing from HTTP/2, and h2 and h3
  are where the recent HTTP DoS bugs were. There is no UDP listener and no
  `Alt-Svc`. The binary is built without the standard library's HTTP/2
  (`nethttpomithttp2`), so a config that enables h2 makes Caddy panic at
  startup rather than quietly serving it.
- **No keep-alive.** Every response carries `Connection: close`. A visit is
  one request, and the redirects change host or scheme, so they need a new
  connection anyway.
- **The Host header must match the SNI**, or the request gets a 421.
- **GET and HEAD only**, 405 otherwise. Headers are capped at 8 KiB, and a
  client has 30 s to send them. That limit also bounds the TLS handshake, which
  waits for the certificate on a host's first visit.
- **The config is not autosaved.** The adapted JSON carries the S3
  credentials.

`compose.yaml` runs the container with a read-only root filesystem, no
capabilities and `no-new-privileges`.

```sh
make test-caddy       # gofmt + go vet + go test on main.go, the adapter and the permission
```

## Certificate storage

Certificates live in S3, not on a container disk, so instances are
interchangeable and a rebuilt container does not re-issue everything and walk
into the Let's Encrypt rate limits.

That comes from [certmagic-s3](https://github.com/techknowlogick/certmagic-s3),
which is **vendored** into `caddy/certmagic-s3/` and compiled in from local
source, through a `replace` in `caddy/go.mod`, rather than fetched by version. The reasons and the local
changes are in [`caddy/certmagic-s3/VENDORED.md`](caddy/certmagic-s3/VENDORED.md).
Upstream v1.5.0 fixes the bucket deletion during OCSP cleanup
([#21](https://github.com/techknowlogick/certmagic-s3/issues/21)) and the
Caddyfile block that did not parse
([#19](https://github.com/techknowlogick/certmagic-s3/issues/19)). The vendored
copy is that release plus the local changes listed there. We build the code we
read.

```sh
make vendor-test      # gofmt + go vet + go test, in a container
make vendor-update    # go get -u && go mod tidy, then re-check
```

The module check in the Caddy build covers it too, so a broken vendor drop
cannot ship silently.

## Certificate cache

Caddy keeps at most 10,000 certificates in memory by default and evicts at
random beyond that, after which a handshake for an evicted certificate reloads
it from S3. Every parked domain has two certificates, www and apex, so the
estate is past that limit.

The capacity can only be set in Caddy's JSON config, so Caddy runs the
Caddyfile through a small config adapter, `caddy/adapter/`, which adapts it as
usual and then sets the capacity to 100,000. A cached certificate takes a few
kB, so a full cache is a few hundred MB; only the certificates requested since
startup are loaded. `make test-caddy` covers it.

## The domain list

```json
{
  "domains": [
    {
      "domain": "brigny.fr",
      "commune_name": "Brigny",
      "commune_zipcode": "87200",
      "email": "contact@brigny.fr",
      "service_public_url": "https://lannuaire.service-public.gouv.fr/nouvelle-aquitaine/haute-vienne/mairie-87030-01",
      "siret": "21870030000013"
    }
  ]
}
```

A bare list of domain objects is also accepted.

**All six fields are mandatory, and no others are allowed.** A half-filled
entry would render a page with gaps in it, which is worse than a build that
refuses to run. Every value is validated — the domain as a hostname, the
zipcode as 5 digits, the SIRET as 14, the email as an address, and
`service_public_url` as a `lannuaire.service-public.gouv.fr` URL (the
pre-move `lannuaire.service-public.fr` form is also accepted). One bad entry
fails the whole build.

**An empty list is refused too.** Removing the last domain would delete every
page *and* every `.parked` marker, and those markers are what allow Caddy to
hold certificates at all — so a feed that empties itself for a moment would
cost a full re-issuance of the estate, against the rate limits, long after the
feed was put right. An empty answer is treated like an unreachable one: logged,
skipped, previous build still served. Unparking everything on purpose means
stopping the service, not emptying the list.

The page is served from `www.<domain>`; a leading `www.` in the domain list is
stripped, so the two can never disagree. `siret` builds the *Présence
numérique* link to `suiteterritoriale.anct.gouv.fr/bienvenue/<siret>`.

Everything else — wording, labels, fixed URLs, colours, language — lives in
`builder/index.html.tmpl`, so a copy change never touches the schema. It is a
Go `html/template`, so every value is escaped for where it lands. The builder
minifies it once at startup, not every page. Its stylesheet only has its
whitespace stripped: a CSS minifier also rewrites values to newer syntax, such
as percentage alpha or range media queries, that older browsers drop.

## Development

```sh
make lint         # gofmt + go vet + caddy validate
make test         # the builder's unit tests
make test-e2e     # brings the whole stack up against RustFS and asserts
                  # routing, the TLS allowlist and certificates landing in S3
make test-caddy   # gofmt + go vet + go test on main.go, the adapter and the permission
make vendor-test  # gofmt + go vet + go test on the vendored plugin
```

`make test-e2e` runs Caddy with `CADDY_TLS_ISSUER=internal`, since no public
DNS points at a development machine. Everything else in it is the real thing,
including on-demand issuance gated by the allowlist.

[`TESTING.md`](TESTING.md) has the full protocol: what to run before pushing
and before deploying, the checks that are still manual, and what a green run
does not prove.

## Licence

MIT — see [`LICENSE`](LICENSE).

One exception: `caddy/certmagic-s3/` is third-party code vendored from
[techknowlogick/certmagic-s3](https://github.com/techknowlogick/certmagic-s3)
and stays under **Apache 2.0**, its original licence, which is reproduced at
`caddy/certmagic-s3/LICENSE`. Our modifications to it are marked
`LOCAL CHANGE` in the source and listed in
[`caddy/certmagic-s3/VENDORED.md`](caddy/certmagic-s3/VENDORED.md).
