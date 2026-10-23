#!/usr/bin/env bash
#
# End-to-end test: brings the whole stack up against RustFS and asserts the
# behaviour that only exists once the pieces are wired together — routing,
# on-demand TLS gated by the allowlist, and certificates landing in S3.
#
# Caddy's local CA stands in for Let's Encrypt (CADDY_TLS_ISSUER=internal),
# and e2e.compose.yaml pins the DNS records the permission checks, since no
# public DNS points at a laptop. Everything else is the real thing.

set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
COMPOSE=(docker compose -f "$ROOT/compose.yaml" -f "$ROOT/scripts/e2e.compose.yaml")
HTTP=http://localhost:${HTTP_BIND:-8080}
HTTPS_PORT=${HTTPS_BIND:-8443}

PARKED=www.brigny.fr
APEX=brigny.fr
# Parked, but its DNS points elsewhere (see e2e.compose.yaml).
PARKED_ELSEWHERE=www.sainte-anne-sur-vilaine.fr
UNKNOWN=not-parked.example
HELPCENTER=help.example
HELPCENTER_ALT=help-alt.example
HELPCENTER_TARGET=https://helpcenter.example/parking

failures=0

pass() { printf '  \033[32mok\033[0m   %s\n' "$1"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; failures=$((failures + 1)); }

check() {
	local what=$1 expected=$2 actual=$3
	if [[ $expected == "$actual" ]]; then
		pass "$what"
	else
		fail "$what — expected [$expected], got [$actual]"
	fi
}

# Status code and Location for a host, over the internal-CA HTTPS listener.
probe() {
	local host=$1 path=$2 fmt=${3:-'%{http_code}'}
	curl -sk -o /dev/null -w "$fmt" \
		--resolve "$host:$HTTPS_PORT:127.0.0.1" \
		"https://$host:$HTTPS_PORT$path" 2>/dev/null || true
}

# The bucket, through s3dev (builder/cmd/s3dev): `s3 keys`, `s3 put KEY BODY`.
s3() {
	"${COMPOSE[@]}" run --rm --no-deps bucket-init "$@"
}

cleanup() {
	if [[ ${KEEP_UP:-0} != 1 ]]; then
		"${COMPOSE[@]}" down -v >/dev/null 2>&1 || true
	fi
}
trap cleanup EXIT

echo "==> Starting the stack (RustFS standing in for S3)"
CADDY_TLS_ISSUER=internal \
	HELPCENTER_REDIRECT_HOSTS="$HELPCENTER $HELPCENTER_ALT" \
	HELPCENTER_REDIRECT_TARGET="$HELPCENTER_TARGET" \
	"${COMPOSE[@]}" up -d --build >/dev/null

echo "==> Waiting for the first build"
# Matched in the shell rather than through `| grep -q`: grep closes the pipe on
# the first match, and the SIGPIPE that kills `compose logs` becomes the
# pipeline's status under pipefail.
built=0
for _ in $(seq 1 60); do
	if [[ $("${COMPOSE[@]}" logs builder 2>&1) == *"Build complete"* ]]; then
		built=1
		break
	fi
	sleep 1
done
((built)) ||
	{ echo "builder never completed a build"; "${COMPOSE[@]}" logs builder; exit 1; }

echo "==> Waiting for Caddy"
for _ in $(seq 1 60); do
	[[ $(curl -s -o /dev/null -w '%{http_code}' "$HTTP/" -H "Host: $PARKED") != 000 ]] && break
	sleep 1
done

echo
echo "Routing"
check "http is redirected to https"      "302"  "$(curl -s -o /dev/null -w '%{http_code}' "$HTTP/" -H "Host: $PARKED")"
check "http to an unknown host is a 404" "404"  "$(curl -s -o /dev/null -w '%{http_code}' "$HTTP/" -H "Host: $UNKNOWN")"
check "http helpcenter goes to the target" "$HELPCENTER_TARGET" \
	"$(curl -s -o /dev/null -w '%{redirect_url}' "$HTTP/" -H "Host: $HELPCENTER")"
# On the apex: file_server would refuse a POST to the page by itself.
check "POST is refused on https"         "405"  "$(curl -sk -o /dev/null -w '%{http_code}' -X POST -d x \
	--resolve "$APEX:$HTTPS_PORT:127.0.0.1" "https://$APEX:$HTTPS_PORT/")"
check "POST is refused on http"          "405"  "$(curl -s -o /dev/null -w '%{http_code}' -X POST -d x "$HTTP/" -H "Host: $PARKED")"
check "parked host serves the page"      "200"  "$(probe "$PARKED" /)"
check "sub-path redirects to the root"   "302"  "$(probe "$PARKED" /deep/link)"
check "the marker file is not served"    "302"  "$(probe "$PARKED" /.parked)"
check "favicon.ico answers no-content"   "204"  "$(probe "$PARKED" /favicon.ico)"
check "robots.txt is served"             "200"  "$(probe "$PARKED" /robots.txt)"
check "apex redirects to www"            "https://$PARKED/" "$(probe "$APEX" / '%{redirect_url}')"
check "apex sub-path redirects to www"   "https://$PARKED/" "$(probe "$APEX" /deep '%{redirect_url}')"
check "heartbeat answers on http"        "200" "$(curl -s -o /dev/null -w '%{http_code}' "$HTTP/__lbheartbeat__")"
check "heartbeat answers on https"       "200" "$(probe "$PARKED" /__lbheartbeat__)"
check "heartbeat with slash answers too" "200" "$(probe "$PARKED" /__lbheartbeat__/)"

echo
echo "Content"
page=$(curl -sk --resolve "$PARKED:$HTTPS_PORT:127.0.0.1" "https://$PARKED:$HTTPS_PORT/")
grep -Eq '<meta name=robots content="?noindex' <<<"$page" &&
	pass "the page is noindex" || fail "the page is noindex"
grep -q 'mairie de Brigny (87200)' <<<"$page" &&
	pass "the page carries the commune" || fail "the page carries the commune"
grep -q '<script' <<<"$page" &&
	fail "the page has no scripts" || pass "the page has no scripts"
check "brotli is served pre-compressed" "br" \
	"$(curl -sk -H 'Accept-Encoding: br' -o /dev/null -w '%{content_type}' \
		--resolve "$PARKED:$HTTPS_PORT:127.0.0.1" "https://$PARKED:$HTTPS_PORT/" >/dev/null 2>&1
	   curl -sk -H 'Accept-Encoding: br' -D- -o /dev/null \
		--resolve "$PARKED:$HTTPS_PORT:127.0.0.1" "https://$PARKED:$HTTPS_PORT/" \
		| awk 'tolower($1)=="content-encoding:"{gsub(/\r/,"");print $2}')"

echo
echo "TLS allowlist"
# An unknown host has no .parked marker, so the permission refuses issuance
# and the handshake fails outright — curl reports 000, never a page.
check "unknown host is refused a certificate" "000" "$(probe "$UNKNOWN" /)"
check "parked host with DNS elsewhere is refused a certificate" "000" "$(probe "$PARKED_ELSEWHERE" /)"

# Helpcenter hosts have no marker either, but the permission allows them
# explicitly, so the handshake succeeds and every path redirects to the URL.
check "helpcenter host gets a certificate"  "302" "$(probe "$HELPCENTER" /)"
check "helpcenter redirects to the target"  "$HELPCENTER_TARGET" "$(probe "$HELPCENTER" / '%{redirect_url}')"
check "helpcenter sub-path redirects too"   "$HELPCENTER_TARGET" "$(probe "$HELPCENTER" /deep/link '%{redirect_url}')"
check "second helpcenter host works too"    "$HELPCENTER_TARGET" "$(probe "$HELPCENTER_ALT" / '%{redirect_url}')"
check "heartbeat on a helpcenter host is not redirected" "200" "$(probe "$HELPCENTER" /__lbheartbeat__)"

echo
echo "Logs"
# Matched in the shell rather than through `| grep -q`: grep closes the pipe
# early and the SIGPIPE becomes the pipeline's status under pipefail.
caddy_logs=$("${COMPOSE[@]}" logs caddy 2>&1 || true)
[[ $caddy_logs == *'"logger":"http.log.access'* ]] &&
	pass "access logs are emitted" || fail "access logs are emitted"
[[ $caddy_logs == *'__lbheartbeat__'* ]] &&
	fail "heartbeat stays out of the logs" || pass "heartbeat stays out of the logs"
# One line at a time: a single glob over the whole log would also match
# "SDK " and "DEBUG" on two unrelated lines.
sdk_debug_lines=$(printf '%s\n' "$caddy_logs" | grep -c 'SDK .*DEBUG' || true)
check "SDK debug lines stay out of the logs" "0" "$sdk_debug_lines"

echo
echo "Hardening"
# With HTTP/3 on, every response offers the QUIC listener in Alt-Svc.
alt_svc=$(curl -sk -D- -o /dev/null --resolve "$PARKED:$HTTPS_PORT:127.0.0.1" \
	"https://$PARKED:$HTTPS_PORT/" | awk 'tolower($1)=="alt-svc:"' || true)
check "HTTP/3 is not advertised" "" "$alt_svc"
check "HTTP/2 is not spoken" "1.1" \
	"$(curl -sk --http2 -o /dev/null -w '%{http_version}' \
		--resolve "$PARKED:$HTTPS_PORT:127.0.0.1" "https://$PARKED:$HTTPS_PORT/" || true)"
check "the Host must match the SNI" "421" \
	"$(curl -sk -o /dev/null -w '%{http_code}' -H "Host: $HELPCENTER" \
		--resolve "$PARKED:$HTTPS_PORT:127.0.0.1" "https://$PARKED:$HTTPS_PORT/" || true)"
# Two requests in one curl run reuse the connection whenever the server
# allows it: each then reports zero new connections.
check "connections are not kept alive" "1 1 " \
	"$(curl -sk -o /dev/null -o /dev/null -w '%{num_connects} ' \
		--resolve "$PARKED:$HTTPS_PORT:127.0.0.1" \
		"https://$PARKED:$HTTPS_PORT/" "https://$PARKED:$HTTPS_PORT/robots.txt" || true)"
# Caddy logs every autosave, and every failed one. The saved config would
# carry the S3 secret key.
[[ $caddy_logs == *autosave* ]] &&
	fail "the config is not saved to disk" || pass "the config is not saved to disk"

echo
echo "Certificate storage in S3"
s3 put unrelated/keepme.txt 'outside the prefix' >/dev/null 2>&1 ||
	fail "storing an object outside the prefix"
keys=$(s3 keys 2>/dev/null || true)
[[ $keys == *.crt* && $keys == *.key* ]] &&
	pass "certificates and keys are stored in S3" ||
	fail "certificates and keys are stored in S3"

# Upstream issue #21: storage cleaning listed the whole bucket and deleted
# everything that did not look like an OCSP staple. Anything outside the
# configured prefix must survive.
"${COMPOSE[@]}" restart caddy >/dev/null 2>&1
sleep 8
keys=$(s3 keys 2>/dev/null || true)
grep -qx 'unrelated/keepme.txt' <<<"$keys" &&
	pass "storage cleaning spares objects outside the prefix (issue #21)" ||
	fail "storage cleaning spares objects outside the prefix (issue #21)"

echo
if ((failures)); then
	echo "$failures check(s) failed"
	exit 1
fi
echo "All end-to-end checks passed"
