#!/usr/bin/env bash
#
# End-to-end test: brings the whole stack up against RustFS and asserts the
# behaviour that only exists once the pieces are wired together — routing,
# on-demand TLS gated by the allowlist, and certificates landing in S3.
#
# Caddy's local CA stands in for Let's Encrypt (CADDY_TLS_ISSUER=internal),
# since no public DNS points at a laptop. Everything else is the real thing.

set -euo pipefail

COMPOSE=(docker compose)
HTTP=http://localhost:${HTTP_BIND:-8080}
HTTPS_PORT=${HTTPS_BIND:-8443}
BUCKET=${S3_BUCKET:-caddy-certs}

PARKED=www.brigny.fr
APEX=brigny.fr
UNKNOWN=not-parked.example

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

s3() {
	"${COMPOSE[@]}" run --rm --no-deps --entrypoint python bucket-init -c "$1"
}

cleanup() {
	if [[ ${KEEP_UP:-0} != 1 ]]; then
		"${COMPOSE[@]}" down -v >/dev/null 2>&1 || true
	fi
}
trap cleanup EXIT

echo "==> Starting the stack (RustFS standing in for S3)"
CADDY_TLS_ISSUER=internal "${COMPOSE[@]}" up -d --build >/dev/null

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
check "parked host serves the page"      "200"  "$(probe "$PARKED" /)"
check "sub-path redirects to the root"   "302"  "$(probe "$PARKED" /deep/link)"
check "the marker file is not served"    "302"  "$(probe "$PARKED" /.parked)"
check "favicon.ico answers no-content"   "204"  "$(probe "$PARKED" /favicon.ico)"
check "robots.txt is served"             "200"  "$(probe "$PARKED" /robots.txt)"
check "apex redirects to www"            "https://$PARKED/" "$(probe "$APEX" / '%{redirect_url}')"
check "apex sub-path redirects to www"   "https://$PARKED/" "$(probe "$APEX" /deep '%{redirect_url}')"

echo
echo "Content"
page=$(curl -sk --resolve "$PARKED:$HTTPS_PORT:127.0.0.1" "https://$PARKED:$HTTPS_PORT/")
grep -q 'content=noindex name=robots' <<<"$page" &&
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
# An unknown host has no .parked marker, so the ask endpoint refuses issuance
# and the handshake fails outright — curl reports 000, never a page.
check "unknown host is refused a certificate" "000" "$(probe "$UNKNOWN" /)"

echo
echo "Certificate storage in S3"
s3 "
import boto3, sys
c = boto3.client('s3', endpoint_url='http://rustfs:9000',
                 aws_access_key_id='rustfsadmin', aws_secret_access_key='rustfsadmin',
                 region_name='us-east-1')
c.put_object(Bucket='$BUCKET', Key='unrelated/keepme.txt', Body=b'outside the prefix')
keys = [o['Key'] for o in c.list_objects_v2(Bucket='$BUCKET').get('Contents', [])]
sys.exit(0 if any(k.endswith('.crt') for k in keys) and any(k.endswith('.key') for k in keys) else 1)
" >/dev/null 2>&1 && pass "certificates and keys are stored in S3" ||
	fail "certificates and keys are stored in S3"

# Upstream issue #21: storage cleaning listed the whole bucket and deleted
# everything that did not look like an OCSP staple. Anything outside the
# configured prefix must survive.
"${COMPOSE[@]}" restart caddy >/dev/null 2>&1
sleep 8
s3 "
import boto3, sys
c = boto3.client('s3', endpoint_url='http://rustfs:9000',
                 aws_access_key_id='rustfsadmin', aws_secret_access_key='rustfsadmin',
                 region_name='us-east-1')
keys = [o['Key'] for o in c.list_objects_v2(Bucket='$BUCKET').get('Contents', [])]
sys.exit(0 if 'unrelated/keepme.txt' in keys else 1)
" >/dev/null 2>&1 && pass "storage cleaning spares objects outside the prefix (issue #21)" ||
	fail "storage cleaning spares objects outside the prefix (issue #21)"

echo
if ((failures)); then
	echo "$failures check(s) failed"
	exit 1
fi
echo "All end-to-end checks passed"
