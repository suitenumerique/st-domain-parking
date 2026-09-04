"""Fetching and validating the domain list.

The domain list carries data only. Every piece of wording, every label and every
fixed URL lives in the template, so a copy change never means touching the
schema — and a domain entry stays six fields long. All six are mandatory: a
half-filled entry would render a page with gaps in it, which is worse than a
build that refuses to run.
"""

import json
import logging
import re
import urllib.parse
import urllib.request
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import boto3

logger = logging.getLogger(__name__)

FIELDS = (
    "domain",
    "commune_name",
    "commune_zipcode",
    "email",
    "service_public_url",
    "siret",
)

# Deliberately strict: the domain becomes a directory name that Caddy resolves
# against the Host header, so anything exotic is rejected rather than sanitised.
LABEL = r"(?!-)[a-z0-9-]{1,63}(?<!-)"
PATTERNS = {
    "domain": (rf"^{LABEL}(\.{LABEL})+$", "a domain name"),
    "commune_name": (r"^[^\x00-\x1f]{1,100}$", "a commune name"),
    "commune_zipcode": (r"^\d{5}$", "5 digits"),
    "email": (r"^[^@\s<>\"']+@[^@\s<>\"']+\.[a-z]{2,}$", "an email address"),
    "siret": (r"^\d{14}$", "14 digits"),
    "service_public_url": (
        r"^https://lannuaire\.service-public(\.gouv)?\.fr/[\w\-./]+$",
        "a lannuaire.service-public[.gouv].fr URL",
    ),
}

#: Longest legal domain name. The per-label regex above does not bound the
#: whole string, and the domain becomes a directory name: anything past 255
#: bytes fails mkdir with ENAMETOOLONG, which aborts the entire build rather
#: than just that entry.
MAX_DOMAIN = 253

HTTP_TIMEOUT = 30


class DomainsError(Exception):
    """The domain list could not be fetched, parsed or validated."""


@dataclass(frozen=True)
class Site:
    """One parked domain. Every field comes straight from the domain list."""

    #: Registrable domain, always stored without the ``www.`` prefix.
    domain: str
    commune_name: str
    commune_zipcode: str
    email: str
    #: Listing on lannuaire.service-public.fr or lannuaire.service-public.gouv.fr.
    service_public_url: str
    siret: str

    @property
    def serve_host(self) -> str:
        """Host whose directory holds the generated page."""
        return f"www.{self.domain}"

    @property
    def hosts(self) -> list[str]:
        """Every host to allowlist: the served one and the apex that redirects."""
        return [self.serve_host, self.domain]


def load(url: str) -> list[Site]:
    """Fetch ``url`` and turn it into validated sites.

    Everything that can go wrong — transport, JSON syntax, schema — surfaces as
    :class:`DomainsError`, because the caller reacts the same way to all of
    them: log it and keep serving the previous build.
    """
    try:
        payload = json.loads(_fetch(url))
    except json.JSONDecodeError as exception:
        raise DomainsError(f"{url} is not valid JSON: {exception}") from exception

    entries = payload.get("domains") if isinstance(payload, dict) else payload
    if not isinstance(entries, list):
        raise DomainsError("expected a list of domains, or an object with `domains`")

    sites = [_site(entry, index) for index, entry in enumerate(entries)]

    duplicates = {site.domain for site in sites}
    if len(duplicates) != len(sites):
        raise DomainsError("the same domain is listed more than once")

    return sites


def _site(raw: Any, index: int) -> Site:
    where = f"domains[{index}]"
    if not isinstance(raw, dict):
        raise DomainsError(f"{where} must be an object")

    # One check covers both missing and unexpected keys, since all six fields
    # are mandatory and there are no optional ones.
    if set(raw) != set(FIELDS):
        raise DomainsError(
            f"{where}: expected exactly {list(FIELDS)}, got {sorted(raw)}"
        )

    values = {}
    for field in FIELDS:
        value = raw[field]
        if not isinstance(value, str) or not value.strip():
            raise DomainsError(f"{where}: `{field}` must be a non-empty string")
        values[field] = value.strip()

    # The www prefix is always derived, never stored, so the served host and
    # the redirected apex cannot disagree.
    values["domain"] = values["domain"].lower().rstrip(".").removeprefix("www.")
    where = values["domain"]

    # Checked before the regex, so a pathological value is discarded without
    # being matched against it.
    if len(values["domain"]) > MAX_DOMAIN:
        raise DomainsError(f"{where}: `domain` is longer than {MAX_DOMAIN} characters")

    for field, (pattern, expected) in PATTERNS.items():
        if not re.match(pattern, values[field]):
            raise DomainsError(
                f"{where}: `{field}` must be {expected}, got {values[field]!r}"
            )

    return Site(**values)


def _fetch(url: str) -> bytes:
    """Read ``url``. Supports http(s)://, s3://, file:// and bare paths."""
    parsed = urllib.parse.urlparse(url)
    try:
        if parsed.scheme in ("http", "https"):
            # The scheme is checked here, so this cannot reach file:// et al.
            with urllib.request.urlopen(url, timeout=HTTP_TIMEOUT) as response:  # noqa: S310
                return response.read()

        if parsed.scheme == "s3":
            # Credentials come from the usual AWS_* variables; AWS_ENDPOINT_URL
            # points this at any S3-compatible store.
            body = boto3.client("s3").get_object(
                Bucket=parsed.netloc, Key=parsed.path.lstrip("/")
            )
            return body["Body"].read()

        if parsed.scheme in ("", "file"):
            return Path(parsed.path if parsed.scheme else url).read_bytes()
    except Exception as exception:
        raise DomainsError(f"could not fetch {url}: {exception}") from exception

    raise DomainsError(f"unsupported scheme {parsed.scheme!r} in {url}")
