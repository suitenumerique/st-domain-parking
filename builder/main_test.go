package main

// The rebuild loop and its signals. These run the builder as a real child
// process: what is being tested is signal disposition and exit codes.

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// runAsBuilder makes the test binary act as the builder when it is started
// by one of the tests below.
const runAsBuilder = "BUILDER_TEST_RUN_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(runAsBuilder) == "1" {
		os.Exit(run(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// never is long enough that nothing rebuilds on its own: a second build
// proves the signal caused it.
const never = "3600"

// lockedBuffer collects the child's output while it is still running.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type builder struct {
	t       *testing.T
	domains string
	output  string
	log     *lockedBuffer
	cmd     *exec.Cmd
	done    chan struct{}
}

// command prepares the builder with env on top of a valid default setup.
func command(t *testing.T, env map[string]string, args ...string) (*exec.Cmd, *lockedBuffer) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(self, args...)
	cmd.Env = append(os.Environ(), runAsBuilder+"=1")
	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	log := &lockedBuffer{}
	cmd.Stdout, cmd.Stderr = log, log
	return cmd, log
}

func start(t *testing.T, interval string) *builder {
	t.Helper()
	dir := t.TempDir()
	b := &builder{
		t:       t,
		domains: filepath.Join(dir, "domains.json"),
		output:  filepath.Join(dir, "out"),
		done:    make(chan struct{}),
	}
	b.write(`[` + entryJSON("brigny.fr") + `]`)

	b.cmd, b.log = command(t, map[string]string{
		"DOMAINS_URL":      b.domains,
		"OUTPUT_DIR":       b.output,
		"REBUILD_INTERVAL": interval,
	})
	if err := b.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = b.cmd.Wait()
		close(b.done)
	}()
	t.Cleanup(func() {
		if !b.exited() {
			_ = b.cmd.Process.Kill()
			<-b.done
		}
	})
	return b
}

func entryJSON(domain string) string {
	return `{"domain": "` + domain + `", "commune_name": "Brigny", "commune_zipcode": "87200",
		"email": "contact@brigny.fr", "service_public_url": "https://lannuaire.service-public.fr/mairie-87030-01",
		"siret": "21510372200017"}`
}

func (b *builder) write(content string) {
	b.t.Helper()
	if err := os.WriteFile(b.domains, []byte(content), 0o644); err != nil {
		b.t.Fatal(err)
	}
}

func (b *builder) builds() int {
	return strings.Count(b.log.String(), "Build complete")
}

func (b *builder) exited() bool {
	select {
	case <-b.done:
		return true
	default:
		return false
	}
}

func (b *builder) signal(sig os.Signal) {
	b.t.Helper()
	if err := b.cmd.Process.Signal(sig); err != nil {
		b.t.Fatal(err)
	}
}

// waitForBuilds waits for count completed builds, or explains what happened.
func (b *builder) waitForBuilds(count int) {
	b.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if b.builds() >= count {
			return
		}
		// Distinguishing "died" from "never rebuilt" is the difference
		// between a signal being fatal and a signal being ignored.
		if b.exited() {
			b.t.Fatalf("the builder exited after %d builds, waiting for %d\n--- its log ---\n%s",
				b.builds(), count, b.log)
		}
		time.Sleep(20 * time.Millisecond)
	}
	b.t.Fatalf("timed out waiting for build %d, saw %d\n--- its log ---\n%s", count, b.builds(), b.log)
}

func TestSIGHUPRebuildsWithoutQuitting(t *testing.T) {
	b := start(t, never)
	b.waitForBuilds(1)

	b.write(`[` + entryJSON("brigny.fr") + `,` + entryJSON("sainte-anne-sur-vilaine.fr") + `]`)
	b.signal(syscall.SIGHUP)

	b.waitForBuilds(2)
	if !exists(filepath.Join(b.output, "www.sainte-anne-sur-vilaine.fr", page)) {
		t.Error("the rebuild did not pick up the new domain")
	}
	// The point of handling SIGHUP at all: its default disposition is to
	// kill the process, so asking for a refresh used to stop the builder.
	if b.exited() {
		t.Error("SIGHUP stopped the builder")
	}
}

func TestSIGHUPCanBeSentRepeatedly(t *testing.T) {
	b := start(t, never)
	b.waitForBuilds(1)

	for expected := 2; expected <= 4; expected++ {
		b.signal(syscall.SIGHUP)
		b.waitForBuilds(expected)
	}
	if b.exited() {
		t.Error("the builder stopped")
	}
}

func TestABrokenDomainListDoesNotStopTheBuilder(t *testing.T) {
	b := start(t, never)
	b.waitForBuilds(1)

	b.write("{ not json")
	b.signal(syscall.SIGHUP)

	// The previous build stays on disk and the process keeps running, so
	// the next SIGHUP can put it right.
	time.Sleep(500 * time.Millisecond)
	if b.exited() {
		t.Fatalf("a bad refresh killed the builder\n%s", b.log)
	}
	if !exists(filepath.Join(b.output, "www.brigny.fr", page)) {
		t.Error("a bad refresh removed the page")
	}

	b.write(`[` + entryJSON("brigny.fr") + `]`)
	b.signal(syscall.SIGHUP)
	b.waitForBuilds(2)
}

// The dangerous shape of a bad feed: valid JSON, fetched fine, and empty.
// Acting on it would delete every page and every `.parked` marker, so Caddy
// would stop being allowed to hold certificates for domains that are still
// very much parked.
func TestAnEmptyDomainListDoesNotUnparkEverything(t *testing.T) {
	b := start(t, never)
	b.waitForBuilds(1)

	b.write("[]")
	b.signal(syscall.SIGHUP)

	time.Sleep(500 * time.Millisecond)
	if !exists(filepath.Join(b.output, "www.brigny.fr", page)) || !exists(filepath.Join(b.output, "www.brigny.fr", marker)) {
		t.Error("an empty feed unparked the domain")
	}
	if b.exited() {
		t.Fatal("an empty feed stopped the builder")
	}

	// And it recovers, rather than needing a restart.
	b.write(`[` + entryJSON("brigny.fr") + `]`)
	b.signal(syscall.SIGHUP)
	b.waitForBuilds(2)
}

func TestSIGTERMStopsItPromptly(t *testing.T) {
	for _, sig := range []os.Signal{syscall.SIGTERM, os.Interrupt} {
		t.Run(sig.String(), func(t *testing.T) {
			b := start(t, never)
			b.waitForBuilds(1)

			// The interval is an hour: a builder that slept through the
			// signal would be killed by the runtime long before it noticed.
			b.signal(sig)
			select {
			case <-b.done:
			case <-time.After(5 * time.Second):
				t.Fatal("the signal was not acted on promptly")
			}
			if code := b.cmd.ProcessState.ExitCode(); code != 0 {
				t.Errorf("exit code %d, want 0", code)
			}
		})
	}
}

func TestAZeroIntervalBuildsOnceAndExits(t *testing.T) {
	b := start(t, "0")

	select {
	case <-b.done:
	case <-time.After(30 * time.Second):
		t.Fatal("the builder did not exit")
	}
	if code := b.cmd.ProcessState.ExitCode(); code != 0 {
		t.Errorf("exit code %d, want 0\n%s", code, b.log)
	}
	if b.builds() != 1 || !exists(filepath.Join(b.output, "www.brigny.fr", page)) {
		t.Errorf("expected exactly one build\n%s", b.log)
	}
}

// runToEnd runs the builder to completion and returns its exit code and
// output.
func runToEnd(t *testing.T, env map[string]string, args ...string) (int, string) {
	t.Helper()
	cmd, log := command(t, env, args...)
	err := cmd.Run()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		t.Fatal(err)
	}
	return cmd.ProcessState.ExitCode(), log.String()
}

func TestConfigurationErrorsExitWith2(t *testing.T) {
	domains := write(t, []any{valid()})
	for _, tt := range []struct {
		name string
		env  map[string]string
		args []string
		want string
	}{
		{"missing DOMAINS_URL", map[string]string{"DOMAINS_URL": ""}, nil, "DOMAINS_URL is required"},
		{"bad interval", map[string]string{"REBUILD_INTERVAL": "soon"}, nil, "REBUILD_INTERVAL must be"},
		{"negative interval", map[string]string{"REBUILD_INTERVAL": "-1"}, nil, "REBUILD_INTERVAL must be"},
		{"bad log level", map[string]string{"LOG_LEVEL": "LOUD"}, nil, "LOG_LEVEL must be"},
		{"unknown command", nil, []string{"serve"}, "the only command is `hosts`"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "out")
			env := map[string]string{"DOMAINS_URL": domains, "OUTPUT_DIR": output, "REBUILD_INTERVAL": "0"}
			for key, value := range tt.env {
				env[key] = value
			}

			code, log := runToEnd(t, env, tt.args...)
			if code != 2 || !strings.Contains(log, tt.want) {
				t.Errorf("exit code %d, want 2 and %q in:\n%s", code, tt.want, log)
			}
			if exists(output) {
				t.Error("a configuration error still built")
			}
		})
	}
}

func TestAnUnreachableDomainListFailsTheFirstBuild(t *testing.T) {
	code, log := runToEnd(t, map[string]string{
		"DOMAINS_URL":      filepath.Join(t.TempDir(), "nope.json"),
		"OUTPUT_DIR":       filepath.Join(t.TempDir(), "out"),
		"REBUILD_INTERVAL": never,
	})

	// An empty tree would 404 every parked domain and teach Caddy's
	// allowlist that none of them exist, so the first build has to be fatal.
	if code != 1 || !strings.Contains(log, "Initial build failed") {
		t.Errorf("exit code %d, want 1 and the failure in:\n%s", code, log)
	}
}

func TestHostsPrintsEveryHost(t *testing.T) {
	domains := write(t, []any{valid(), with(map[string]any{"domain": "other.fr"})})
	cmd, _ := command(t, map[string]string{"DOMAINS_URL": domains}, "hosts")
	cmd.Stdout, cmd.Stderr = nil, nil

	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != "www.brigny.fr brigny.fr www.other.fr other.fr" {
		t.Errorf("got %q", got)
	}
}
