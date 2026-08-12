"""Entrypoint: render the pages, then re-render them on a timer.

This process owns nothing but the output directory. Caddy runs beside it from
its own image, with a static configuration it never reloads — the domain list
lives entirely in the generated directory tree (see :mod:`builder.render`), so
publishing a change is nothing more than a few file writes.
"""

import logging
import os
import signal
import sys
import threading
from pathlib import Path

from builder import render
from builder.domains import DomainsError, load

logger = logging.getLogger("builder")


def main() -> int:
    """Run the builder. Returns the process exit code."""
    logging.basicConfig(
        level=os.environ.get("LOG_LEVEL", "INFO").upper(),
        format="%(asctime)s %(levelname)-7s %(name)s %(message)s",
    )

    url = os.environ.get("DOMAINS_URL", "")
    if not url:
        logger.error("DOMAINS_URL is required (https://, s3://, file:// or a path)")
        return 2

    output_dir = Path(os.environ.get("OUTPUT_DIR", "/srv/sites"))
    interval = int(os.environ.get("REBUILD_INTERVAL", "300"))

    # No thread is started: the Events are only an interruptible sleep. A plain
    # time.sleep() is resumed with the time remaining when a signal arrives
    # (PEP 475), so SIGTERM would go unnoticed for up to REBUILD_INTERVAL —
    # well past the grace period, turning every restart into a SIGKILL.
    stop = threading.Event()
    wake = threading.Event()

    # Before the first build, not after it: SIGHUP kills a process that is not
    # handling it, so any window where it is unhandled is a window where asking
    # for a refresh stops the builder instead. That window would open exactly
    # where an operator is most likely to aim — the moment the log first says a
    # build is complete.
    _on_signal(stop, wake)

    # The first build has to succeed: an empty tree would 404 every parked
    # domain and, worse, teach Caddy's TLS allowlist that none of them exist.
    if not _rebuild(url, output_dir):
        logger.error("Initial build failed")
        return 1

    if not interval:
        logger.info("REBUILD_INTERVAL is 0, built once")
        return 0

    logger.info("Rebuilding every %ds, or on SIGHUP", interval)
    while not stop.is_set():
        # Returns as soon as a signal sets the event, otherwise after the
        # interval. A SIGHUP that lands mid-rebuild is honoured by the next
        # pass rather than lost: the list may well have changed after the
        # fetch it arrived during.
        wake.wait(interval)
        wake.clear()
        if stop.is_set():
            break
        _rebuild(url, output_dir)

    return 0


def _rebuild(url: str, output_dir: Path) -> bool:
    """Fetch, validate and render. False if the build did not happen.

    A failed refresh is not fatal: whatever was built last is still on disk and
    Caddy keeps serving it.
    """
    try:
        render.build(load(url), output_dir)
    except (DomainsError, OSError) as exception:
        logger.error("Build skipped: %s", exception)
        return False
    return True


def _on_signal(stop: threading.Event, wake: threading.Event) -> None:
    def shutdown(signum, _frame):
        logger.info("Received %s, shutting down", signal.Signals(signum).name)
        stop.set()
        wake.set()

    def rebuild(signum, _frame):
        logger.info("Received %s, rebuilding now", signal.Signals(signum).name)
        wake.set()

    signal.signal(signal.SIGTERM, shutdown)
    signal.signal(signal.SIGINT, shutdown)
    # Refetch on demand, without waiting out the interval — the conventional
    # meaning of SIGHUP for a daemon that reads its inputs at startup.
    #
    # Handling it is also what stops it being fatal: the default disposition of
    # SIGHUP is to terminate, so before this an operator asking for a refresh
    # would have killed the builder instead.
    signal.signal(signal.SIGHUP, rebuild)


if __name__ == "__main__":
    sys.exit(main())
