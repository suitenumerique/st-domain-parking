// Command builder renders the parking pages, then re-renders them on a timer.
//
// It owns nothing but the output directory. Caddy runs beside it from its own
// image, with a static configuration it never reloads: the domain list lives
// entirely in the generated directory tree, so publishing a change is nothing
// more than a few file writes.
//
// `builder hosts` prints every host of the domain list on one line, for
// development.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// run runs the builder and returns the process exit code.
func run(args []string) int {
	if err := setupLogging(); err != nil {
		slog.Error(err.Error())
		return 2
	}

	url := os.Getenv("DOMAINS_URL")
	if url == "" {
		slog.Error("DOMAINS_URL is required (https://, s3://, file:// or a path)")
		return 2
	}

	switch {
	case len(args) == 1 && args[0] == "hosts":
		return printHosts(url)
	case len(args) > 0:
		slog.Error(fmt.Sprintf("unexpected arguments %q, the only command is `hosts`", args))
		return 2
	}

	outputDir := os.Getenv("OUTPUT_DIR")
	if outputDir == "" {
		outputDir = "/srv/sites"
	}
	interval, err := rebuildInterval()
	if err != nil {
		slog.Error(err.Error())
		return 2
	}

	// Before the first build, not after it: SIGHUP kills a process that is
	// not handling it, so any window where it is unhandled is a window where
	// asking for a refresh stops the builder instead. That window would open
	// exactly where an operator is most likely to aim: the moment the log
	// first says a build is complete.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, os.Interrupt)

	// The first build has to succeed: an empty tree would 404 every parked
	// domain and, worse, teach Caddy's TLS allowlist that none of them exist.
	if !rebuild(url, outputDir) {
		slog.Error("Initial build failed")
		return 1
	}

	if interval == 0 {
		slog.Info("REBUILD_INTERVAL is 0, built once")
		return 0
	}

	slog.Info(fmt.Sprintf("Rebuilding every %s, or on SIGHUP", interval))
	timer := time.NewTimer(interval)
	for {
		// A SIGHUP that lands mid-rebuild waits in its channel and triggers
		// the next pass rather than being lost: the list may well have
		// changed after the fetch it arrived during.
		select {
		case received := <-stop:
			name := "SIGTERM"
			if received == os.Interrupt {
				name = "SIGINT"
			}
			slog.Info("Received " + name + ", shutting down")
			return 0
		case <-hup:
			slog.Info("Received SIGHUP, rebuilding now")
		case <-timer.C:
		}

		rebuild(url, outputDir)
		timer.Reset(interval)
	}
}

// rebuild fetches, validates and renders. False if the build did not happen.
//
// A failed refresh is not fatal: whatever was built last is still on disk and
// Caddy keeps serving it.
func rebuild(url, outputDir string) bool {
	sites, err := load(context.Background(), url)
	if err == nil {
		err = build(sites, outputDir)
	}
	if err != nil {
		slog.Error("Build skipped: " + err.Error())
		return false
	}
	return true
}

func printHosts(url string) int {
	sites, err := load(context.Background(), url)
	if err != nil {
		slog.Error(err.Error())
		return 1
	}

	var hosts []string
	for _, site := range sites {
		hosts = append(hosts, site.Hosts()...)
	}
	fmt.Println(strings.Join(hosts, " "))
	return 0
}

// rebuildInterval reads REBUILD_INTERVAL, in seconds. 0 builds once.
func rebuildInterval() (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv("REBUILD_INTERVAL"))
	if raw == "" {
		raw = "300"
	}

	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds < 0 {
		return 0, fmt.Errorf("REBUILD_INTERVAL must be a number of seconds, got %q", raw)
	}
	return time.Duration(seconds) * time.Second, nil
}

func setupLogging() error {
	var level slog.Level
	raw := strings.TrimSpace(os.Getenv("LOG_LEVEL"))
	if raw == "" {
		raw = "INFO"
	}
	if strings.EqualFold(raw, "WARNING") {
		raw = "WARN"
	}
	if err := level.UnmarshalText([]byte(raw)); err != nil {
		return fmt.Errorf("LOG_LEVEL must be DEBUG, INFO, WARN or ERROR, got %q", raw)
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
	return nil
}
