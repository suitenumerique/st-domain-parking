"""The rebuild loop and its signals.

These run the module as a real process: what is being tested is signal
disposition, which does not exist inside the interpreter running the tests.
"""

import json
import os
import signal
import subprocess
import sys
import time
from pathlib import Path

import pytest

VALID = {
    "domain": "brigny.fr",
    "commune_name": "Brigny",
    "commune_zipcode": "87200",
    "email": "contact@brigny.fr",
    "service_public_url": "https://lannuaire.service-public.fr/mairie-87030-01",
    "siret": "21510372200017",
}

#: Long enough that nothing rebuilds on its own: a second build proves the
#: signal caused it.
NEVER = "3600"


def entry(domain):
    return {**VALID, "domain": domain}


class Builder:
    """The builder, running as a child process, with its log on disk."""

    def __init__(self, tmp_path, interval=NEVER):
        self.domains = tmp_path / "domains.json"
        self.output = tmp_path / "out"
        self.log = tmp_path / "builder.log"
        self.write([entry("brigny.fr")])

        self.handle = self.log.open("wb")
        self.process = subprocess.Popen(
            [sys.executable, "-m", "builder"],
            stdout=self.handle,
            stderr=subprocess.STDOUT,
            env={
                **os.environ,
                "DOMAINS_URL": str(self.domains),
                "OUTPUT_DIR": str(self.output),
                "REBUILD_INTERVAL": interval,
                "LOG_LEVEL": "INFO",
            },
        )

    def write(self, entries):
        self.domains.write_text(json.dumps(entries), encoding="utf-8")

    def builds(self):
        """How many builds have completed so far."""
        if not self.log.exists():
            return 0
        return self.log.read_text(encoding="utf-8", errors="replace").count(
            "Build complete"
        )

    def wait_for_builds(self, count, timeout=15.0):
        """Wait for ``count`` completed builds, or explain what happened."""
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if self.builds() >= count:
                return True
            # Distinguishing "died" from "never rebuilt" is the difference
            # between a signal being fatal and a signal being ignored.
            if self.process.poll() is not None:
                raise AssertionError(
                    f"the builder exited with {self.process.returncode} after "
                    f"{self.builds()} builds, waiting for {count}\n"
                    f"--- its log ---\n{self.log.read_text(errors='replace')}"
                )
            time.sleep(0.02)

        raise AssertionError(
            f"timed out waiting for build {count}, saw {self.builds()}\n"
            f"--- its log ---\n{self.log.read_text(errors='replace')}"
        )

    def stop(self):
        if self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                self.process.kill()
        self.handle.close()


@pytest.fixture
def builder(tmp_path):
    running = Builder(tmp_path)
    yield running
    running.stop()


def test_sighup_rebuilds_without_quitting(builder):
    assert builder.wait_for_builds(1), "the first build never completed"

    builder.write([entry("brigny.fr"), entry("sainte-anne-sur-vilaine.fr")])
    builder.process.send_signal(signal.SIGHUP)

    assert builder.wait_for_builds(2), "SIGHUP did not trigger a rebuild"
    assert (builder.output / "www.sainte-anne-sur-vilaine.fr" / "index.html").exists()

    # The point of handling SIGHUP at all: its default disposition is to kill
    # the process, so asking for a refresh used to stop the builder.
    assert builder.process.poll() is None, "SIGHUP stopped the builder"


def test_sighup_can_be_sent_repeatedly(builder):
    assert builder.wait_for_builds(1)

    for expected in (2, 3, 4):
        builder.process.send_signal(signal.SIGHUP)
        assert builder.wait_for_builds(expected), f"rebuild {expected} never happened"

    assert builder.process.poll() is None


def test_a_broken_domain_list_does_not_stop_the_builder(builder):
    assert builder.wait_for_builds(1)

    builder.domains.write_text("{ not json", encoding="utf-8")
    builder.process.send_signal(signal.SIGHUP)

    # The previous build stays on disk and the process keeps running, so the
    # next SIGHUP can put it right.
    time.sleep(0.5)
    assert builder.process.poll() is None, "a bad refresh killed the builder"
    assert (builder.output / "www.brigny.fr" / "index.html").exists()

    builder.write([entry("brigny.fr")])
    builder.process.send_signal(signal.SIGHUP)
    assert builder.wait_for_builds(2), "the builder did not recover"


def test_an_empty_domain_list_does_not_unpark_everything(builder):
    """The dangerous shape of a bad feed: valid JSON, fetched fine, and empty.

    Acting on it would delete every page and every `.parked` marker, so Caddy
    would stop being allowed to hold certificates for domains that are still
    very much parked.
    """
    assert builder.wait_for_builds(1)
    page = builder.output / "www.brigny.fr" / "index.html"
    assert page.exists()

    builder.write([])
    builder.process.send_signal(signal.SIGHUP)

    time.sleep(0.5)
    assert page.exists(), "an empty feed deleted the pages"
    assert (builder.output / "www.brigny.fr" / ".parked").exists()
    assert builder.process.poll() is None, "an empty feed stopped the builder"

    # And it recovers, rather than needing a restart.
    builder.write([entry("brigny.fr")])
    builder.process.send_signal(signal.SIGHUP)
    assert builder.wait_for_builds(2)


def test_sigterm_stops_it_promptly(builder):
    assert builder.wait_for_builds(1)

    # The interval is an hour: a builder that slept through SIGTERM would be
    # killed by the runtime long before it noticed.
    started = time.monotonic()
    builder.process.terminate()
    assert builder.process.wait(timeout=10) == 0
    assert time.monotonic() - started < 5, "SIGTERM was not acted on promptly"


def test_a_zero_interval_builds_once_and_exits(tmp_path):
    running = Builder(tmp_path, interval="0")
    try:
        assert running.process.wait(timeout=30) == 0
        assert running.builds() == 1
        assert (running.output / "www.brigny.fr" / "index.html").exists()
    finally:
        running.stop()


def test_a_missing_domains_url_is_an_error(tmp_path):
    process = subprocess.run(
        [sys.executable, "-m", "builder"],
        env={**os.environ, "DOMAINS_URL": "", "OUTPUT_DIR": str(tmp_path / "out")},
        capture_output=True,
        timeout=30,
        check=False,
    )

    assert process.returncode == 2
    assert b"DOMAINS_URL is required" in process.stdout + process.stderr


def test_an_unreachable_domain_list_fails_the_first_build(tmp_path):
    process = subprocess.run(
        [sys.executable, "-m", "builder"],
        env={
            **os.environ,
            "DOMAINS_URL": str(Path(tmp_path) / "nope.json"),
            "OUTPUT_DIR": str(tmp_path / "out"),
            "REBUILD_INTERVAL": NEVER,
        },
        capture_output=True,
        timeout=30,
        check=False,
    )

    # An empty tree would 404 every parked domain and teach Caddy's allowlist
    # that none of them exist, so the first build has to be fatal.
    assert process.returncode == 1
    assert b"Initial build failed" in process.stdout + process.stderr
