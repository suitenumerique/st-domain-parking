# Vendored: certmagic-s3

CertMagic storage backend that keeps Caddy's certificates in S3 instead of on
local disk.

- Upstream: <https://github.com/techknowlogick/certmagic-s3>
- Forked at: `6c26852bb192f1f0ccfe36b0508b3b9f12116632` (tag `v1.5.0`, 2026-10-02)
- Licence: Apache 2.0 (see `LICENSE`)

The build keeps the module path as `github.com/techknowlogick/certmagic-s3`.
An xcaddy `--with …=<dir>` replacement substitutes this directory for it, so
rebasing on upstream stays a plain diff.

## Why vendored

We build the code we read: the module is compiled from this directory, not
fetched by tag at build time. The vendored copy also carries a small set of
local fixes that upstream does not have yet. The "Local changes" section
lists them.

## Now upstream (v1.5.0)

The previous fork carried six local fixes to locking, all now part of
upstream's own `lock.go`:

- `Lock` backs off by `LockPollInterval` between retries, instead of spinning
  on a storage error.
- `Lock` honours context cancellation and stops as soon as the caller's
  context ends.
- Acquisition is atomic: the lock object is created with `If-None-Match: *`,
  so the store, not this process, decides which of two racing instances wins.
- The holder heartbeats its lock every `LockRefreshInterval` while it holds
  it, so a slow certificate order no longer loses its lock during the wait.
- `LockTimeout` now bounds only an unbroken run of storage failures, not the
  whole wait for a live lock.
- Releasing checks the lock is still this instance's own, by ETag, before
  deleting it.

Upstream also went further than the fork. It judges a lock stale by
comparing its `LastModified` against the storage service's own `Date`
header. Holders and waiters therefore never need to agree on wall-clock
time. The fork parsed a timestamp out of the lock body instead.

`LockExpiration` is now 2 minutes, up from the fork's 15 seconds. A live
holder keeps its lock for as long as it needs, because it heartbeats
regardless of this value. The longer expiration only slows down taking over
a lock that is genuinely abandoned.

A reload pays for this: `Cleanup()` leaves the lock object in place, so the
incoming instance waits up to two minutes to reclaim it.

### What this needs from the store

The lock needs a store that honours `If-None-Match` and `If-Match` on
writes. A store that ignores these headers accepts both writes. The lock
then protects nothing, exactly as before this fix.

RustFS answers `412 PreconditionFailed` and leaves the object untouched.
[Scaleway documents both headers](https://www.scaleway.com/en/docs/object-storage/api-cli/using-conditional-writes/).
AWS also answers `409` for a competing conditional write in flight; `lock.go`
treats that the same way as a `412`.

## Local changes

Every one is marked `LOCAL CHANGE` in the source.

1. **`Cleanup()` stops the refreshers.** Caddy calls `Cleanup` when the
   module goes away. On a config reload, this happens while the replacement
   instance already runs. Without `Cleanup`, the outgoing instance keeps
   heartbeating locks it will never release, and those locks never expire.

2. **`Stat()` and `Load()` recognise every type of "not found".** Upstream
   matches only `*types.NoSuchKey`. `GetObject` returns that type, but
   `HeadObject` does not: it answers a bare 404, and the SDK surfaces that
   as `*types.NotFound`. Without a match for `*types.NotFound`, `Stat()` on
   a missing key returned a generic error, not `fs.ErrNotExist`. certmagic
   reads `fs.ErrNotExist` to decide whether it must issue a certificate.
   Both functions now call `isNotFound()`. This function, already defined in
   upstream's `lock.go`, covers `*types.NoSuchKey`, `*types.NotFound`, and
   any response that carries HTTP 404.

3. **Credentials stay out of the logs.** certmagic logs the storage value at
   INFO on every storage-cleaning pass, as `zap.Any("storage", storage)`.
   Without `MarshalLogObject`, zap reflects over the struct's JSON tags
   instead. It then writes the access key, the secret key, and the
   encryption key in clear text. `MarshalLogObject` covers that path.
   `String()` covers `%v` and `%s`. Neither format can print a credential.

4. **`host` is validated.** `Provision` puts `https://` in front of `host`.
   A value that already carried a scheme silently became
   `https://https://…` and failed much later, as an endpoint nothing could
   resolve. `validateHost` now refuses a scheme, a path, or a non-numeric
   port at config-parse time, and its message says what to do instead.
   `validateHost` also accepts an IP literal, including a bracketed or bare
   IPv6 address, because `net.ParseIP` recognises it before the colon check
   runs.

5. **SDK logs go through zap.** The SDK's default logger is
   `logging.NewStandardLogger(os.Stderr)`. It prints a line such as
   `SDK <time> DEBUG Response has no supported checksum.` on every
   successful `GetObject`. A store without checksum support, such as
   RustFS, triggers it. These lines sit outside Caddy's JSON log, and the
   default logger has no level filter. `sdkLogger` sends the SDK log calls
   to zap, so Caddy's log level applies. The checksum line is `Debug` in
   the pinned SDK, so it stays hidden unless Caddy runs with the `debug`
   global option. SDK warnings still show.

### Local-only files

- `VENDORED.md` — this file.
- `local_test.go` — tests for behaviour upstream does not have:
  `TestValidateHost`, `TestStringHidesCredentials`,
  `TestCleanupStopsRefreshing`, and `TestStatAndLoadReportNotExist`.
- `log_test.go` — tests the log-redaction change (`MarshalLogObject`) and the
  SDK log bridge (`sdkLogger`). Upstream has neither.

### Omitted upstream files

`Dockerfile`, `Makefile`, `README.md`, `.golangci.yml`, `.dockerignore`. The
vendored copy builds through `caddy/Dockerfile` and documents itself through
this file. The vendored copy is also gofmt-clean, which drops a stray
semicolon that upstream still carries in `UnmarshalCaddyfile`.

## Deliberately left alone

- **`UnmarshalCaddyfile`'s `if !d.Args(&value) { continue }`** silently
  skips malformed input. It also skips the `s3` module-name token from
  `storage s3 {`, which has no argument of its own. Turning the skip into an
  error reproduces upstream issue #19. The code carries a comment that
  explains this.
- **`List()` ignores its `recursive` argument.** It always lists everything
  under the prefix. This matches how certmagic calls `List()`. This note
  exists because the behaviour does not match what the signature promises.

## Updating

```sh
make vendor-update    # go get -u, go mod tidy, vet, test — in a container
```

Re-apply the `LOCAL CHANGE` hunks if upstream has moved under them.
