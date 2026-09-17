"""Fetching and validating the domain list."""

import json

import pytest

from builder.domains import DomainsError, load

VALID = {
    "domain": "brigny.fr",
    "commune_name": "Brigny",
    "commune_zipcode": "87200",
    "email": "contact@brigny.fr",
    "service_public_url": "https://lannuaire.service-public.fr/mairie-87030-01",
    "siret": "21510372200017",
}


def write(tmp_path, payload):
    """Write ``payload`` as a domain list and return its path as a URL."""
    path = tmp_path / "domains.json"
    path.write_text(json.dumps(payload), encoding="utf-8")
    return str(path)


def test_a_valid_entry_round_trips(tmp_path):
    (site,) = load(write(tmp_path, {"domains": [VALID]}))

    assert site.domain == "brigny.fr"
    assert site.serve_host == "www.brigny.fr"
    assert site.hosts == ["www.brigny.fr", "brigny.fr"]
    assert site.commune_name == "Brigny"
    assert site.siret == "21510372200017"


def test_a_gouv_fr_service_public_url_is_accepted(tmp_path):
    url = "https://lannuaire.service-public.gouv.fr/mairie-87030-01"
    (site,) = load(write(tmp_path, [{**VALID, "service_public_url": url}]))

    assert site.service_public_url == url


@pytest.mark.parametrize("payload", [[], {"domains": []}])
def test_an_empty_list_is_refused(tmp_path, payload):
    """Acting on an empty list unparks every domain at once, and the markers
    it deletes are what let Caddy hold certificates for them. Treated like any
    other unusable answer, so the previous build keeps being served.
    """
    with pytest.raises(DomainsError, match="empty"):
        load(write(tmp_path, payload))


def test_a_bare_list_is_accepted(tmp_path):
    sites = load(write(tmp_path, [VALID, {**VALID, "domain": "other.fr"}]))

    assert [site.domain for site in sites] == ["brigny.fr", "other.fr"]


def test_the_www_prefix_is_stripped_and_the_domain_normalised(tmp_path):
    (site,) = load(write(tmp_path, [{**VALID, "domain": "  WWW.Brigny.FR.  "}]))

    assert site.domain == "brigny.fr"
    assert site.serve_host == "www.brigny.fr"


def test_a_domain_listed_twice_is_rejected(tmp_path):
    url = write(tmp_path, [VALID, {**VALID, "domain": "www.brigny.fr"}])

    with pytest.raises(DomainsError, match="more than once"):
        load(url)


@pytest.mark.parametrize("field", sorted(VALID))
def test_every_field_is_mandatory(tmp_path, field):
    url = write(
        tmp_path, [{key: value for key, value in VALID.items() if key != field}]
    )

    with pytest.raises(DomainsError, match="expected exactly"):
        load(url)


def test_unknown_fields_are_rejected(tmp_path):
    url = write(tmp_path, [{**VALID, "accent": "#ff0000"}])

    with pytest.raises(DomainsError, match="expected exactly"):
        load(url)


@pytest.mark.parametrize("value", ["", "   ", None, 42, ["x"]])
def test_fields_must_be_non_empty_strings(tmp_path, value):
    url = write(tmp_path, [{**VALID, "commune_name": value}])

    with pytest.raises(DomainsError, match="non-empty string"):
        load(url)


@pytest.mark.parametrize(
    ("field", "value"),
    [
        ("domain", "not a domain"),
        ("domain", "localhost"),
        ("domain", "-brigny.fr"),
        ("domain", "../etc/passwd"),
        ("domain", "brigny.fr/x"),
        ("commune_zipcode", "8720"),
        ("commune_zipcode", "87200x"),
        ("email", "nope"),
        ("email", "a@b"),
        ("siret", "123"),
        ("siret", "2151037220001x"),
        ("service_public_url", "https://evil.example/x"),
        ("service_public_url", "javascript:alert(1)"),
        ("service_public_url", "http://lannuaire.service-public.fr/x"),
    ],
)
def test_malformed_values_are_rejected(tmp_path, field, value):
    url = write(tmp_path, [{**VALID, field: value}])

    with pytest.raises(DomainsError, match=f"`{field}` must be"):
        load(url)


@pytest.mark.parametrize(
    "payload", ["a string", {"domains": "not a list"}, {}, 42, [["not an object"]]]
)
def test_malformed_payloads_are_rejected(tmp_path, payload):
    with pytest.raises(DomainsError):
        load(write(tmp_path, payload))


def test_invalid_json_is_reported(tmp_path):
    path = tmp_path / "domains.json"
    path.write_text("{not json", encoding="utf-8")

    with pytest.raises(DomainsError, match="not valid JSON"):
        load(str(path))


def test_a_missing_file_is_reported(tmp_path):
    with pytest.raises(DomainsError, match="could not fetch"):
        load(str(tmp_path / "absent.json"))


def test_an_unsupported_scheme_is_reported():
    with pytest.raises(DomainsError, match="unsupported scheme"):
        load("ftp://example.com/domains.json")
