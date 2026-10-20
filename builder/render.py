"""Rendering of the parking pages onto disk, in the layout Caddy expects.

For every site, two directories are produced under the output root::

    www.brigny.fr/index.html      the page
    www.brigny.fr/index.html.br   pre-compressed variants, served as-is
    www.brigny.fr/index.html.gz
    www.brigny.fr/.parked         allowlist marker for on-demand TLS
    brigny.fr/.parked             apex: allowlisted so it can be redirected

A rebuild that changes nothing touches no file, so mtimes stay stable and
Caddy's file cache is not invalidated for no reason.
"""

import gzip
import logging
import multiprocessing
import os
import re
import shutil
from concurrent.futures import ProcessPoolExecutor
from pathlib import Path

import brotli
import minify_html
from jinja2 import Environment, FileSystemLoader, StrictUndefined
from lightningcss import process_stylesheet

from builder.domains import Site

logger = logging.getLogger(__name__)

PAGE = "index.html"
#: Caddy's on-demand TLS allowlist matches on this file, so it is the single
#: source of truth for whether a host is served at all.
MARKER = ".parked"

#: Compression level for the pre-compressed variant.
#:
#: Brotli's quality 11 buys 138 bytes over quality 5 — 1.48 kB instead of
#: 1.62 kB on a 4.2 kB page — and costs about fifty times the CPU: 9.4 ms
#: against 0.17 ms per page, which over a 36,000-domain estate is the
#: difference between six seconds and five minutes. The page is read by the
#: handful of people who typed the domain in, so the bytes are worth nothing
#: next to the build time. Quality 9 is no smaller than 5 and ten times
#: slower, which is where the trade-off stops being worth taking.
BROTLI_QUALITY = 5

#: Gzip is the fallback for the clients that do not speak brotli. A page this
#: small compresses in the same time at any level, so the best one costs
#: nothing.
GZIP_LEVEL = 9

#: Browsers the stylesheet has to keep working on.
#:
#: Minifying CSS without saying what it has to run on rewrites it to whatever
#: syntax is current: `@media (max-width: 560px)` becomes Level 4 range syntax
#: and `rgba(...)` becomes an eight-digit hex. A browser that predates either
#: does not merely ignore the new spelling — it drops the whole rule, so the
#: mobile layout silently disappears on exactly the devices least likely to be
#: up to date.
#:
#: Pinned rather than browserslist's `defaults`, which drifts as browsers age
#: out and would quietly reintroduce the problem years from now. This floor is
#: the one the page already sets by being built on custom properties, and
#: asking for it is free: the only prefix it adds is one the source carries.
BROWSERS = ["chrome 49", "firefox 31", "safari 9.1", "edge 15", "ios_saf 9.3"]

TEMPLATES = Path(__file__).parent / "templates"
TEMPLATE_NAME = "index.html.j2"

TEMPLATE = Environment(
    loader=FileSystemLoader(TEMPLATES),
    # Unconditional, not select_autoescape(): that helper keys off the file
    # extension, and `index.html.j2` ends in `.j2`, so it would quietly leave
    # escaping off and let domain list values inject markup.
    autoescape=True,
    undefined=StrictUndefined,
    trim_blocks=True,
    lstrip_blocks=True,
).get_template(TEMPLATE_NAME)

#: The stylesheet, so it can be swapped for the minified one below.
STYLE = re.compile(r"(?<=<style>).*?(?=</style>)", re.DOTALL)


def _stylesheet() -> tuple[str, str]:
    """The template's stylesheet, as written and minified for ``BROWSERS``."""
    blocks = STYLE.findall((TEMPLATES / TEMPLATE_NAME).read_text("utf-8"))
    if len(blocks) != 1:
        raise ValueError(
            f"expected one <style> block in the template, got {len(blocks)}"
        )

    (raw,) = blocks
    if "{{" in raw or "{%" in raw:
        raise ValueError(
            "the stylesheet carries template syntax, so it cannot be "
            "minified once for every site"
        )

    return raw, process_stylesheet(raw, minify=True, browsers_list=BROWSERS)


#: Every page carries the same stylesheet, so it is minified once here rather
#: than once per site on every rebuild.
RAW_CSS, CSS = _stylesheet()

#: The workers are forked rather than spawned: a fork inherits the compiled
#: template and the logging configuration, so a worker costs nothing to
#: start. The builder is single-threaded, which is what makes forking safe
#: here; Python 3.14 would default to forkserver, whose workers would
#: re-import this module and lose the logging setup.
try:
    _CONTEXT = multiprocessing.get_context("fork")
except ValueError:
    _CONTEXT = multiprocessing.get_context()

#: Sites per task. Large enough for the per-task overhead to disappear,
#: small enough for the workers to stay evenly loaded.
_CHUNK = 64


def build(sites: list[Site], output_dir: Path, jobs: int | None = None) -> None:
    """Render every site into ``output_dir`` and prune anything stale.

    ``jobs`` is the number of worker processes; ``None`` means one per usable
    CPU. Rendering, compressing and writing a page are independent of every
    other page, so the work is spread over the workers; the pruning stays
    here, because it is the one step that needs to see the whole list.
    """
    output_dir.mkdir(parents=True, exist_ok=True)

    expected: set[str] = set()
    for site in sites:
        expected.update(site.hosts)

    workers = _workers(len(sites), jobs)
    if workers > 1:
        with ProcessPoolExecutor(max_workers=workers, mp_context=_CONTEXT) as pool:
            written = sum(
                pool.map(
                    _build_site,
                    ((output_dir, site) for site in sites),
                    chunksize=_CHUNK,
                )
            )
    else:
        written = sum(1 for site in sites if _build_site((output_dir, site)))

    pruned = _prune(output_dir, expected)

    logger.info(
        "Build complete: %d domains, %d written, %d pruned", len(sites), written, pruned
    )


def _build_site(job: tuple[Path, Site]) -> bool:
    """Render and write one site. True if the page was written.

    A module-level function, because the workers are separate processes and
    have to be able to pickle it.
    """
    output_dir, site = job

    written = _write(output_dir / site.serve_host, _page(site))
    # The apex only needs to exist so Caddy will negotiate a certificate
    # for it; the redirect itself is handled by the static config.
    _mark(output_dir / site.domain)

    return written


def _workers(count: int, jobs: int | None) -> int:
    """How many workers to build with.

    One per usable CPU, and never more than there are sites: a pool of
    workers with nothing to do is overhead rather than speed. A pool also
    has to be worth starting at all, which is why a handful of sites is
    built in one go however many CPUs the machine has.
    """
    if jobs is not None:
        if jobs < 1:
            raise ValueError("BUILD_JOBS must be at least 1")
        return min(jobs, count)

    # process_cpu_count() counts the CPUs this process may actually run on,
    # which is what a container limited to a few CPUs reports; cpu_count()
    # counts the ones the machine has.
    cpus = getattr(os, "process_cpu_count", os.cpu_count)() or os.cpu_count() or 1
    return max(1, min(cpus, count // 2))


def _page(site: Site) -> bytes:
    # The stylesheet is swapped for its minified form before the HTML pass,
    # which is then told to leave CSS alone: its own stylesheet pass takes no
    # browser targets and would undo the work above.
    return minify_html.minify(
        TEMPLATE.render(site=site).replace(RAW_CSS, CSS, 1),
        minify_css=False,
        keep_closing_tags=True,
        keep_html_and_head_opening_tags=True,
    ).encode("utf-8")


def _write(directory: Path, page: bytes) -> bool:
    """Write the page and its compressed variants. True if anything changed."""
    _mark(directory)

    target = directory / PAGE
    if target.exists() and target.read_bytes() == page:
        return False

    _replace(target, page)
    _replace(directory / f"{PAGE}.br", brotli.compress(page, quality=BROTLI_QUALITY))
    _replace(directory / f"{PAGE}.gz", gzip.compress(page, GZIP_LEVEL, mtime=0))

    logger.info("Built %s (%d bytes)", directory.name, len(page))
    return True


def _replace(target: Path, payload: bytes) -> None:
    """Write via a rename, so Caddy never serves a half-written file."""
    temporary = target.with_name(f".tmp-{target.name}")
    temporary.write_bytes(payload)
    temporary.replace(target)


def _mark(directory: Path) -> None:
    directory.mkdir(parents=True, exist_ok=True)
    (directory / MARKER).touch(exist_ok=True)


def _prune(output_dir: Path, expected: set[str]) -> int:
    """Remove host directories that are no longer in the domain list.

    Only directories carrying the marker are touched, so an operator poking
    around in the output directory cannot have unrelated files deleted.
    """
    stale = [
        entry
        for entry in output_dir.iterdir()
        if entry.name not in expected and (entry / MARKER).exists()
    ]
    for entry in stale:
        shutil.rmtree(entry)
        logger.info("Pruned %s", entry.name)

    return len(stale)
