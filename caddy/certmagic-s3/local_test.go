package s3

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"time"
)

// `host` is a bare hostname that gets https:// prepended, so a URL here used
// to become https://https://… and only failed much later, at connect time.
func TestValidateHost(t *testing.T) {
	tests := []struct {
		name     string
		host     string
		accepted bool
	}{
		{name: "bare hostname", host: "s3.example.com", accepted: true},
		{name: "with a port", host: "s3.example.com:9000", accepted: true},
		{name: "localhost with a port", host: "localhost:9000", accepted: true},
		{name: "https URL", host: "https://s3.example.com", accepted: false},
		{name: "http URL", host: "http://s3.example.com", accepted: false},
		{name: "URL with a port", host: "https://s3.example.com:9000", accepted: false},
		// Bracketed IPv6 with a port: net.SplitHostPort yields the bare
		// literal "::1", which net.ParseIP recognises, so the colons in it
		// are not mistaken for a scheme or a path.
		{name: "bracketed IPv6 with a port", host: "[::1]:9000", accepted: true},
		// A bare IPv6 literal has no port to split off, so hostname stays
		// the whole string; net.ParseIP still recognises it as an IP, so
		// it is accepted on the same basis as the bracketed form above.
		{name: "bare IPv6 literal", host: "::1", accepted: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateHost(tt.host)
			if tt.accepted && err != nil {
				t.Errorf("validateHost(%q) = %v, want it accepted", tt.host, err)
			}
			if !tt.accepted && err == nil {
				t.Errorf("validateHost(%q) was accepted, want it rejected", tt.host)
			}
		})
	}
}

// MarshalLogObject (log_test.go) covers zap.Any, which is how certmagic logs
// the storage. Anything that reaches for %v or %s instead must be just as safe.
func TestStringHidesCredentials(t *testing.T) {
	storage := &S3{
		Bucket:        "certificates",
		Endpoint:      "https://s3.example.invalid",
		Prefix:        "caddy",
		AccessKey:     "AKIAEXAMPLEACCESS",
		SecretKey:     "SuperSecretKey123",
		EncryptionKey: strings.Repeat("e", 32),
	}

	for _, formatted := range []string{
		fmt.Sprintf("%v", storage),
		fmt.Sprintf("%s", storage),
		fmt.Sprint(storage),
	} {
		for _, secret := range []string{
			storage.SecretKey, storage.AccessKey, storage.EncryptionKey,
		} {
			if strings.Contains(formatted, secret) {
				t.Errorf("a credential was formatted into %q", formatted)
			}
		}
		if !strings.Contains(formatted, "certificates") {
			t.Errorf("expected the bucket in %q", formatted)
		}
	}
}

// A module that goes away, such as on a config reload, must stop
// heartbeating the locks it was holding. If it does not, the lock object
// never goes stale, and the instance that replaced it can never take it over.
func TestCleanupStopsRefreshing(t *testing.T) {
	setLockTimings(t, 60*time.Millisecond, 10*time.Millisecond, 5*time.Millisecond, time.Second)

	f, srv := newFakeS3(t)
	holder := f.instance(srv)

	if err := holder.Lock(context.Background(), "k"); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if err := holder.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	first, ok := f.get("k.lock")
	if !ok {
		t.Fatal("lock object disappeared after Cleanup")
	}

	// Several refresh intervals, and past LockExpiration: long enough that a
	// running refresher would have rewritten the lock, and long enough that
	// a stopped one leaves it looking abandoned.
	time.Sleep(100 * time.Millisecond)

	second, ok := f.get("k.lock")
	if !ok {
		t.Fatal("lock object disappeared after Cleanup")
	}
	if second.etag != first.etag {
		t.Error("Cleanup did not stop the refresher: the lock was rewritten after it ran")
	}

	waiter := f.instance(srv)
	// Both Last-Modified and Date carry whole seconds, so the takeover waits
	// for the next second boundary, not for LockExpiration.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := waiter.Lock(ctx, "k"); err != nil {
		t.Fatalf("a second instance could not take over the stale lock: %v", err)
	}
	_ = waiter.Unlock(context.Background(), "k")
}

// certmagic reads fs.ErrNotExist to decide whether it must issue a
// certificate. HeadObject answers a bare 404, which a NoSuchKey-only match
// misses.
func TestStatAndLoadReportNotExist(t *testing.T) {
	f, srv := newFakeS3(t)
	s := f.instance(srv)

	if _, err := s.Stat(context.Background(), "missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Stat on a missing key: got %v, want fs.ErrNotExist", err)
	}
	if _, err := s.Load(context.Background(), "missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Load on a missing key: got %v, want fs.ErrNotExist", err)
	}
}
