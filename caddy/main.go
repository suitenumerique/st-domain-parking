// Command caddy is Caddy with only the modules the Caddyfile uses, plus the
// vendored S3 storage, the config adapter and the on-demand TLS permission.
//
// It replaces modules/standard, which registers every standard module:
// reverse_proxy, fastcgi, acme_server, basic auth, tracing and so on. A module
// that is not linked cannot be configured, by mistake or otherwise.
//
// The HTTP and TLS apps and rewrite come in through the adapter and
// file_server, so they are not listed here.
package main

import (
	caddycmd "github.com/caddyserver/caddy/v2/cmd"

	// `precompressed br gzip` in file_server.
	_ "github.com/caddyserver/caddy/v2/modules/caddyhttp/encode/brotli"
	_ "github.com/caddyserver/caddy/v2/modules/caddyhttp/encode/gzip"
	// file_server and the file matcher.
	_ "github.com/caddyserver/caddy/v2/modules/caddyhttp/fileserver"
	_ "github.com/caddyserver/caddy/v2/modules/caddyhttp/headers"
	// The proxy_protocol listener wrapper.
	_ "github.com/caddyserver/caddy/v2/modules/caddyhttp/proxyprotocol"
	// The json log encoder.
	_ "github.com/caddyserver/caddy/v2/modules/logging"

	_ "github.com/suitenumerique/st-domain-parking/caddy/adapter"
	_ "github.com/suitenumerique/st-domain-parking/caddy/permission"
	_ "github.com/techknowlogick/certmagic-s3"
)

func main() {
	caddycmd.Main()
}
