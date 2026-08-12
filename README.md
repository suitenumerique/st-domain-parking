# st-domain-parking

Parking pages for communes that do not have a website yet.

A JSON file lists the domains and six fields for each. A Python builder
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

- **TLS** is issued on demand, gated by an allowlist endpoint Caddy serves to
  itself on loopback. It answers 200 only when `<host>/.parked` exists, so a
  stranger pointing DNS at the server cannot make it request certificates —
  the TLS handshake is simply refused.
- **Apex → www** is a single global rule.
- **Any path other than `/`** redirects back to `/`.
- **`/favicon.ico`** answers 204: the icon is an inline data URI, and a 204
  stops browsers that ask anyway from retrying.
- **`/robots.txt`** is served, not redirected — a redirected robots.txt reads
  as absent to crawlers.
- **Unknown hosts** get a 404.

Adding or removing a domain is therefore a pure file operation: no reload, no
restart, no config regeneration. Every redirect is temporary (302) on purpose —
parking is a placeholder that owners are expected to replace with a real site,
and a 301 would linger in browser caches long after that.

The page is ~5.8 kB of HTML, ~1.6 kB over the wire, in a single request: no
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
make install      # builds the tooling image the other targets run in
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
the pages to `./build`, owned by you rather than by root.

## Configuration

Builder:

| Variable | Default | Meaning |
| --- | --- | --- |
| `DOMAINS_URL` | *(required)* | Location of the JSON file: `https://`, `s3://`, `file://` or a path |
| `REBUILD_INTERVAL` | `300` | Seconds between refetches. `0` builds once and exits |
| `OUTPUT_DIR` | `/srv/sites` | Root of the generated tree |
| `LOG_LEVEL` | `INFO` | Python log level |

`s3://` URLs use the standard `AWS_*` variables; set `AWS_ENDPOINT_URL` to
point at any S3-compatible store.

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
| `ASK_PORT` | `9000` | Loopback port for the TLS allowlist endpoint |
| `CADDY_TLS_ISSUER` | *(empty)* | Empty is real ACME; `internal` uses Caddy's local CA |

Certificate storage (see below):

| Variable | Default | Meaning |
| --- | --- | --- |
| `S3_BUCKET` | *(required)* | Bucket holding the certificates |
| `S3_REGION` | `us-east-1` | |
| `S3_ENDPOINT` | *(empty)* | Empty for real AWS; set for RustFS, MinIO, Scaleway, Garage… |
| `S3_ACCESS_KEY` / `S3_SECRET_KEY` | | Omit to use the ambient AWS credential chain |
| `S3_PREFIX` | `caddy` | Key prefix within the bucket |
| `S3_ENCRYPTION_KEY` | *(empty)* | Exactly 32 bytes to encrypt stored keys client-side |

## Certificate storage

Certificates live in S3, not on a container disk, so instances are
interchangeable and a rebuilt container does not re-issue everything and walk
into the Let's Encrypt rate limits.

That comes from [certmagic-s3](https://github.com/techknowlogick/certmagic-s3),
which is **vendored** into `caddy/certmagic-s3/` and compiled in with `xcaddy`
from local source rather than fetched by version. The reasons, the upstream
bugs that motivated it, and the local fixes are all in
[`caddy/certmagic-s3/VENDORED.md`](caddy/certmagic-s3/VENDORED.md) — briefly:
the latest release deletes the entire bucket during OCSP cleanup
([#21](https://github.com/techknowlogick/certmagic-s3/issues/21)), and its
Caddyfile block does not parse
([#19](https://github.com/techknowlogick/certmagic-s3/issues/19)). Both are
fixed on master but unreleased, so we build the commit we read.

```sh
make vendor-test      # gofmt + go vet + go test, in a container
make vendor-update    # go get -u && go mod tidy, then re-check
```

The Caddy build fails if the module does not end up registered, so a broken
vendor drop cannot ship silently.

## The domain list

```json
{
  "domains": [
    {
      "domain": "brigny.fr",
      "commune_name": "Brigny",
      "commune_zipcode": "87200",
      "email": "contact@brigny.fr",
      "service_public_url": "https://lannuaire.service-public.fr/nouvelle-aquitaine/haute-vienne/mairie-87030-01",
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
`service_public_url` as a `lannuaire.service-public.fr` URL. One bad entry
fails the whole build.

The page is served from `www.<domain>`; a leading `www.` in the domain list is
stripped, so the two can never disagree. `siret` builds the *Présence
numérique* link to `suiteterritoriale.anct.gouv.fr/bienvenue/<siret>`.

Everything else — wording, labels, fixed URLs, colours, language — lives in
`builder/templates/index.html.j2`, so a copy change never touches the schema.

## Development

```sh
make lint         # ruff format + ruff check + pylint + caddy validate
make test         # unit tests
make test-e2e     # brings the whole stack up against RustFS and asserts
                  # routing, the TLS allowlist and certificates landing in S3
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
