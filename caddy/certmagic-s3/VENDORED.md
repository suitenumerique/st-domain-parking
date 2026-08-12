# Vendored: certmagic-s3

CertMagic storage backend that keeps Caddy's certificates in S3 instead of on
local disk. Vendored here rather than pulled by version so that we build what
we have read.

- Upstream: <https://github.com/techknowlogick/certmagic-s3>
- Forked at: `4d17998f812b1afcfe8613247cb70e0b849da04c` (2026-05-24)
- Licence: Apache 2.0 (see `LICENSE`)

The module path is left as `github.com/techknowlogick/certmagic-s3`; the build
substitutes this directory with an xcaddy `--with …=<dir>` replacement, so
rebasing on upstream stays a plain diff.

## Why not a released version

`v1.4.0`, the latest tag, carries two serious bugs that are fixed on `master`
but unreleased:

- **[#21](https://github.com/techknowlogick/certmagic-s3/issues/21) — OCSP
  cleanup deletes the entire bucket.** `List()` ignored its prefix argument, so
  certmagic's `List(ctx, "ocsp", false)` got back *every* object in the bucket,
  decided they were not valid staples, and deleted them. With a prefix
  configured the mirror-image bug applied the prefix twice and nothing was ever
  deleted. Fixed by [#20](https://github.com/techknowlogick/certmagic-s3/pull/20),
  which is in the commit above.
- **[#19](https://github.com/techknowlogick/certmagic-s3/issues/19) — the
  Caddyfile block does not parse.** Also fixed on master.

[#16](https://github.com/techknowlogick/certmagic-s3/issues/16) and
[#18](https://github.com/techknowlogick/certmagic-s3/issues/18) are downstream
reports of the same OCSP cleanup fault.

## Local changes

Every one is marked `LOCAL CHANGE` in the source.

1. **`Lock()` no longer spins.** On any error from `GetObject` that was not a
   404 — a network blip, a 403, throttling — upstream `continue`d with neither
   a sleep nor a deadline check, turning an S3 problem into an unbounded loop
   hammering S3 at full speed. The retry now backs off by `LockPollInterval`
   and the acquisition deadline is checked at the top of every iteration.

2. **`Lock()` honours context cancellation.** Upstream could only be
   interrupted by its own timeout, so shutting down mid-lock meant waiting it
   out.

3. **Acquisition is atomic.** Upstream read the lock file and then wrote it as
   two separate operations, so two instances could both find no lock and both
   create one — and then both order a certificate for the same name, against
   Let's Encrypt's five-per-week duplicate limit. The creating write now
   carries `If-None-Match: *`, so it is the bucket that decides who wins.

   This makes conditional writes a **requirement of the storage backend**. A
   store that ignored the header would accept both writes and silently be as
   racy as before. Verified on the two that matter here: RustFS answers `412
   PreconditionFailed` and leaves the object untouched, and
   [Scaleway documents both `If-None-Match` and `If-Match`](https://www.scaleway.com/en/docs/object-storage/api-cli/using-conditional-writes/).
   AWS additionally answers `409` when a competing conditional write is in
   flight, which is treated the same way.

4. **The holder heartbeats its lock.** Upstream declared `LockExpiration = 2m`,
   never read it, and expired locks after `LockTimeout` (15s) instead. Nothing
   refreshed the lock while ACME issuance ran, so a slow order had its lock
   stolen mid-flight. A goroutine now rewrites the lock file every
   `LockRefreshInterval` (5s, matching certmagic's own `FileStorage`), which is
   what makes a short `LockExpiration` (15s) safe: a live holder keeps its lock
   for as long as it needs, and a dead one is reclaimed seconds after it stops
   rather than blocking every other instance for two minutes.

   Every refresh is conditional on the ETag of the previous one, so once a lock
   has been taken over the previous holder physically cannot write to it again.

5. **`LockTimeout` bounds a broken bucket, not a busy one.** Upstream applied
   it to the whole acquisition, so an instance gave up 15s into another's
   perfectly healthy issuance and certmagic failed the handshake. Waiting on a
   live lock is now bounded by `LockExpiration` instead — either the holder
   finishes, or it stops refreshing and we take over — while `LockTimeout`
   applies only to an unbroken run of failures from the bucket itself.

6. **Releasing checks the lock is still ours.** S3 has no conditional delete,
   so `Unlock` stops the refresher, then compares the stored ETag with the one
   it wrote before deleting. Losing that race merely leaves a lock file to
   expire on its own; deleting one that another instance now holds would hand
   its certificate order to a third.

7. **`Cleanup()` stops the refreshers.** Caddy calls it when the module goes
   away, which on a config reload happens while the replacement is already
   running. Without it the outgoing instance would keep heartbeating locks it
   has no intention of releasing, and they would never expire.

8. **`Stat()` and `Load()` recognise every flavour of "not found".** Upstream
   only matched `*types.NoSuchKey`, which `GetObject` returns but `HeadObject`
   does not — `HeadObject` answers a bare 404 that the SDK surfaces as
   `*types.NotFound`. `Stat()` on a missing key therefore returned a generic
   error rather than `fs.ErrNotExist`, the sentinel certmagic keys its "must I
   issue a certificate?" decision on. `isNotFound()` now also covers
   `*types.NotFound` and any response carrying HTTP 404.

9. **Credentials stay out of the logs.** certmagic logs the storage value at
   INFO on every storage-cleaning pass, as `zap.Any("storage", storage)`. With
   nothing to tell zap otherwise it reflected over the struct's JSON tags and
   wrote the access key, the secret key and the encryption key out in clear
   text. `MarshalLogObject` covers that path and `String()` covers `%v`/`%s`,
   so neither spelling can print a credential.

10. **`host` is validated.** It is a bare hostname that gets `https://` put in
    front of it, so a value carrying a scheme silently became
    `https://https://…` and surfaced much later as an unresolvable endpoint.
    A scheme, a path or a non-numeric port is now refused at config-parse time,
    where the message can say what to do instead.

## Deliberately left alone

- **`UnmarshalCaddyfile`'s `if !d.Args(&value) { continue }`** looks like it is
  swallowing malformed input, and it is — but it is also what skips the `s3`
  module-name token from `storage s3 {`. Turning it into an error reproduces
  issue #19 exactly. It now carries a comment saying so.
- **`List()` ignores its `recursive` argument** and always lists everything
  under the prefix. Correct for how certmagic calls it; noted here because it
  is not what the signature promises.

## Updating

```sh
make vendor-update    # go get -u, go mod tidy, vet, test — in a container
```

Re-apply the `LOCAL CHANGE` hunks if upstream has moved under them.
