"""Rendering onto disk, in the layout the static Caddyfile expects."""

import gzip
import re
from html.parser import HTMLParser

import brotli
import pytest

from builder import render
from builder.domains import Site
from builder.render import MARKER, PAGE, build


@pytest.fixture(name="site")
def fixture_site() -> Site:
    return Site(
        domain="brigny.fr",
        commune_name="Brigny",
        commune_zipcode="87200",
        email="contact@brigny.fr",
        service_public_url="https://lannuaire.service-public.fr/mairie-87030-01",
        siret="21510372200017",
    )


def page_of(output_dir, host="www.brigny.fr"):
    return (output_dir / host / PAGE).read_text("utf-8")


def test_layout_matches_what_caddy_looks_for(tmp_path, site):
    build([site], tmp_path)

    # The apex is allowlisted so a certificate can be issued for the redirect,
    # but it holds no page.
    assert (tmp_path / "brigny.fr" / MARKER).exists()
    assert not (tmp_path / "brigny.fr" / PAGE).exists()

    served = tmp_path / "www.brigny.fr"
    assert (served / MARKER).exists()
    assert (served / PAGE).exists()
    assert (served / f"{PAGE}.br").exists()
    assert (served / f"{PAGE}.gz").exists()


def test_page_is_self_contained(tmp_path, site):
    build([site], tmp_path)
    page = page_of(tmp_path)

    assert "<script" not in page
    for forbidden in ("fonts.googleapis.com", "rel=stylesheet", ".css", ".js"):
        assert forbidden not in page


def test_page_content(tmp_path, site):
    build([site], tmp_path)
    page = page_of(tmp_path)

    assert "Ce domaine appartient à la mairie de Brigny (87200)." in page
    assert "mailto:contact@brigny.fr" in page
    assert "https://lannuaire.service-public.fr/mairie-87030-01" in page
    assert "https://suiteterritoriale.anct.gouv.fr/bienvenue/21510372200017" in page
    assert "href=https://www.brigny.fr/ rel=canonical" in page
    # The footer sends the municipal team somewhere it can edit the entry, and
    # that is a different destination from the per-SIRET row above.
    assert "href=https://collectivite.fr/>Espace Commune</a>" in page


def test_the_page_is_not_indexable(tmp_path, site):
    """Reached by typing the domain, not from search: the authoritative
    record is the Service-Public listing, so the page stays out of the index.
    Crawling is still allowed (in the Caddyfile) or the noindex is never read.
    """
    build([site], tmp_path)

    assert "content=noindex name=robots" in page_of(tmp_path)


def test_the_description_has_no_stray_whitespace(tmp_path, site):
    build([site], tmp_path)

    assert (
        'content="Ce domaine appartient à la mairie de Brigny (87200)." name=description'
        in page_of(tmp_path)
    )


def test_compressed_variants_decode_to_the_page(tmp_path, site):
    build([site], tmp_path)
    served = tmp_path / "www.brigny.fr"
    page = (served / PAGE).read_bytes()

    assert brotli.decompress((served / f"{PAGE}.br").read_bytes()) == page
    assert gzip.decompress((served / f"{PAGE}.gz").read_bytes()) == page
    # Compression has to be worth its while.
    assert len((served / f"{PAGE}.br").read_bytes()) < len(page) / 2


@pytest.mark.parametrize(
    "payload",
    [
        "<script>alert(1)</script>",
        '"><script>alert(1)</script>',
        '"\' onload=alert(1) x="',
        "</title><script>alert(1)</script>",
    ],
)
def test_domain_list_values_cannot_inject_markup(tmp_path, site, payload):
    """Values reach both element text and attribute values, so parse the
    result rather than grepping it: the minifier legitimately decodes ``&lt;``
    back to ``<`` inside quoted attributes, where it cannot open a tag.
    """
    build([Site(**{**vars(site), "commune_name": payload})], tmp_path)

    parsed = _Parser()
    parsed.feed(page_of(tmp_path))

    assert "script" not in parsed.tags
    assert not [name for name, _ in parsed.attributes if name.startswith("on")]
    assert payload in "".join(parsed.text)


def test_an_unchanged_rebuild_writes_nothing(tmp_path, site):
    build([site], tmp_path)
    served = tmp_path / "www.brigny.fr" / PAGE
    before = served.stat().st_mtime_ns

    build([site], tmp_path)

    assert served.stat().st_mtime_ns == before


def test_removed_domains_are_pruned(tmp_path, site):
    gone = Site(**{**vars(site), "domain": "gone.fr"})
    build([site, gone], tmp_path)
    assert (tmp_path / "www.gone.fr").exists()

    build([site], tmp_path)

    assert not (tmp_path / "www.gone.fr").exists()
    assert not (tmp_path / "gone.fr").exists()
    assert (tmp_path / "www.brigny.fr" / PAGE).exists()


def test_media_queries_stay_in_level_3_syntax(tmp_path, site):
    build([site], tmp_path)
    page = page_of(tmp_path)

    # Minifying the stylesheet normalises `(max-width: 560px)` into Level 4
    # range syntax, which browsers older than ~2023 drop along with the whole
    # rule — taking the mobile layout with it.
    assert "@media (max-width:560px)" in page
    assert "width<=" not in page
    assert "width>=" not in page

    # The rule itself still has to be there, and intact.
    media = page[page.index("@media") : page.index("</style>")]
    assert ".card-body" in media
    assert "flex-direction:column" in media


def test_the_stylesheet_is_minified(tmp_path, site):
    build([site], tmp_path)
    style = page_of(tmp_path)
    style = style[style.index("<style>") : style.index("</style>")]

    # Cheap proxies for "the CSS pass ran": it strips the spaces after colons
    # and the last semicolon of each block.
    assert ": " not in style
    assert ";}" not in style


def test_the_stylesheet_avoids_syntax_older_browsers_would_drop(tmp_path, site):
    build([site], tmp_path)
    style = page_of(tmp_path)
    style = style[style.index("<style>") : style.index("</style>")]

    # Eight-digit hex (#rrggbbaa) is the other rewrite a minifier reaches for
    # when it is not told what it has to run on.
    assert not re.search(r"#[0-9a-f]{8}\b", style), "eight-digit hex reached the page"
    assert "rgba(" in style


def test_a_stylesheet_carrying_template_syntax_is_refused(tmp_path, monkeypatch):
    # The stylesheet is minified once at import because it is the same for
    # every site. That stops being true the moment it interpolates anything.
    templates = tmp_path / "templates"
    templates.mkdir()
    (templates / render.TEMPLATE_NAME).write_text(
        "<html><style>body{color:{{ site.domain }}}</style></html>", encoding="utf-8"
    )
    monkeypatch.setattr(render, "TEMPLATES", templates)

    with pytest.raises(ValueError, match="template syntax"):
        render._stylesheet()


def test_unmanaged_directories_are_left_alone(tmp_path, site):
    stray = tmp_path / "not-ours"
    stray.mkdir(parents=True)
    (stray / "keep.txt").write_text("keep")

    build([site], tmp_path)

    assert (stray / "keep.txt").exists()


class _Parser(HTMLParser):
    """Collects what a browser would actually build out of the page."""

    def __init__(self) -> None:
        super().__init__(convert_charrefs=True)
        self.tags: list[str] = []
        self.attributes: list[tuple[str, str | None]] = []
        self.text: list[str] = []

    def handle_starttag(self, tag, attrs):
        self.tags.append(tag)
        self.attributes.extend(attrs)

    def handle_data(self, data):
        self.text.append(data)
