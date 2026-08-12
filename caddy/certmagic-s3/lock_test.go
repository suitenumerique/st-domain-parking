package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	s3sdk "github.com/aws/aws-sdk-go-v2/service/s3"
	"go.uber.org/zap"
)

// The tests below run against a real S3 client pointed at the fake bucket in
// this file, rather than a mocked-out client. What is being tested is whether
// the conditional headers reach the bucket and are respected, so the request
// has to be built and signed by the SDK exactly as it would be in production.

// fakeBucket is a minimal object store that enforces If-None-Match and
// If-Match, which is all the locking depends on.
type fakeBucket struct {
	mu      sync.Mutex
	objects map[string]fakeObject
	version int
	puts    []http.Header

	// failWith, when non-zero, makes every request fail with that status.
	failWith int
	// dropETag omits the ETag from PutObject responses, as some
	// implementations do.
	dropETag bool
}

type fakeObject struct {
	body []byte
	etag string
}

func newFakeBucket() *fakeBucket {
	return &fakeBucket{objects: make(map[string]fakeObject)}
}

func (b *fakeBucket) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.failWith != 0 {
		writeS3Error(w, b.failWith, "InternalError")
		return
	}

	key := strings.TrimPrefix(r.URL.Path, "/")
	object, exists := b.objects[key]

	switch r.Method {
	case http.MethodPut:
		b.puts = append(b.puts, r.Header.Clone())

		// The two conditions the lock relies on. Note that a bucket ignoring
		// these headers would silently make acquisition racy again, which is
		// why TestLockUsesConditionalWrites asserts they are actually sent.
		if r.Header.Get("If-None-Match") == "*" && exists {
			writeS3Error(w, http.StatusPreconditionFailed, "PreconditionFailed")
			return
		}
		if match := r.Header.Get("If-Match"); match != "" && (!exists || match != object.etag) {
			writeS3Error(w, http.StatusPreconditionFailed, "PreconditionFailed")
			return
		}

		body, _ := io.ReadAll(r.Body)
		b.version++
		etag := fmt.Sprintf("%q", fmt.Sprintf("v%d", b.version))
		b.objects[key] = fakeObject{body: body, etag: etag}

		if !b.dropETag {
			w.Header().Set("ETag", etag)
		}
		w.WriteHeader(http.StatusOK)

	case http.MethodGet, http.MethodHead:
		if !exists {
			writeS3Error(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		w.Header().Set("ETag", object.etag)
		w.Header().Set("Content-Length", fmt.Sprint(len(object.body)))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(object.body)
		}

	case http.MethodDelete:
		delete(b.objects, key)
		w.WriteHeader(http.StatusNoContent)

	default:
		writeS3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}

func writeS3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "<Error><Code>%s</Code></Error>", code)
}

func (b *fakeBucket) put(key string, body []byte) string {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.version++
	etag := fmt.Sprintf("%q", fmt.Sprintf("v%d", b.version))
	b.objects[key] = fakeObject{body: body, etag: etag}
	return etag
}

func (b *fakeBucket) get(key string) (fakeObject, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	object, ok := b.objects[key]
	return object, ok
}

func (b *fakeBucket) putHeaders() []http.Header {
	b.mu.Lock()
	defer b.mu.Unlock()

	return append([]http.Header(nil), b.puts...)
}

func (b *fakeBucket) setFailWith(status int) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.failWith = status
}

// newTestStorage returns a storage backed by bucket, as a distinct "instance"
// — call it twice to model two Caddys sharing one bucket.
func newTestStorage(t *testing.T, bucket *fakeBucket) *S3 {
	t.Helper()

	server := httptest.NewServer(bucket)
	t.Cleanup(server.Close)

	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider("test", "test", "")),
		config.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
		// The fake bucket sends no checksums, and the warning it draws for
		// every response drowns the test output.
		config.WithResponseChecksumValidation(aws.ResponseChecksumValidationWhenRequired),
	)
	if err != nil {
		t.Fatalf("loading config: %v", err)
	}

	client := s3sdk.NewFromConfig(cfg, func(o *s3sdk.Options) {
		o.BaseEndpoint = aws.String(server.URL)
		o.UsePathStyle = true
		// What is under test is how the lock reacts to a failing bucket, not
		// how patiently the SDK retries on its way there.
		o.RetryMaxAttempts = 1
	})

	storage := &S3{
		Client: client,
		Bucket: "certs",
		Prefix: "caddy",
		Logger: zap.NewNop(),
	}

	// No refresher may outlive the test that started it. One that does keeps
	// writing to the bucket while the next test is using it, and reads the
	// package-level timings while withLockTimings is putting them back.
	t.Cleanup(func() { _ = storage.Cleanup() })

	return storage
}

// withLockTimings shortens the package-level timings so the tests do not have
// to wait out production intervals, and restores them afterwards.
func withLockTimings(t *testing.T, expiration, refresh, poll time.Duration) {
	t.Helper()

	expirationWas, refreshWas, pollWas := LockExpiration, LockRefreshInterval, LockPollInterval
	LockExpiration, LockRefreshInterval, LockPollInterval = expiration, refresh, poll

	t.Cleanup(func() {
		LockExpiration, LockRefreshInterval, LockPollInterval = expirationWas, refreshWas, pollWas
	})
}

// The bug this whole change exists for: upstream read the lock and then wrote
// it as two separate operations, so two instances could both find no lock and
// both create one. Only one of these acquisitions may succeed.
func TestOnlyOneInstanceAcquiresALock(t *testing.T) {
	withLockTimings(t, time.Minute, time.Minute, 10*time.Millisecond)

	bucket := newFakeBucket()
	const instances = 8

	var (
		start     sync.WaitGroup
		finished  sync.WaitGroup
		mu        sync.Mutex
		acquired  int
		contended int
	)
	start.Add(1)

	for i := 0; i < instances; i++ {
		storage := newTestStorage(t, bucket)
		finished.Add(1)

		go func() {
			defer finished.Done()
			start.Wait() // release them all at once

			// Short deadline: whoever does not get it must block, not acquire.
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()

			if err := storage.Lock(ctx, "example.com"); err != nil {
				mu.Lock()
				contended++
				mu.Unlock()
				return
			}
			mu.Lock()
			acquired++
			mu.Unlock()
		}()
	}

	start.Done()
	finished.Wait()

	if acquired != 1 {
		t.Errorf("%d of %d instances acquired the lock at once, want exactly 1", acquired, instances)
	}
	if contended != instances-1 {
		t.Errorf("%d instances were kept waiting, want %d", contended, instances-1)
	}
}

// A bucket that ignored the conditional headers would make acquisition racy
// again while every test above still passed, so assert they go out on the wire.
func TestLockUsesConditionalWrites(t *testing.T) {
	withLockTimings(t, time.Minute, time.Minute, 10*time.Millisecond)

	bucket := newFakeBucket()
	storage := newTestStorage(t, bucket)

	if err := storage.Lock(context.Background(), "example.com"); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	t.Cleanup(func() { _ = storage.Unlock(context.Background(), "example.com") })

	headers := bucket.putHeaders()
	if len(headers) != 1 {
		t.Fatalf("got %d writes, want 1", len(headers))
	}
	if got := headers[0].Get("If-None-Match"); got != "*" {
		t.Errorf("If-None-Match on the creating write = %q, want %q", got, "*")
	}
}

// Upstream aged locks out after 15s with nothing refreshing them, so an ACME
// order that ran longer had its lock stolen mid-flight.
func TestALiveLockIsNotStolen(t *testing.T) {
	withLockTimings(t, 150*time.Millisecond, 25*time.Millisecond, 10*time.Millisecond)

	bucket := newFakeBucket()
	holder := newTestStorage(t, bucket)
	waiter := newTestStorage(t, bucket)

	if err := holder.Lock(context.Background(), "example.com"); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	defer func() { _ = holder.Unlock(context.Background(), "example.com") }()

	// Wait several expirations. The holder is doing nothing but heartbeating,
	// which is what a slow certificate order looks like from here.
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()

	if err := waiter.Lock(ctx, "example.com"); err == nil {
		t.Fatal("the waiter took over a lock that was still being refreshed")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiter failed with %v, want the context deadline", err)
	}

	// And the holder still owns it: releasing must actually remove the file.
	if err := holder.Unlock(context.Background(), "example.com"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if _, exists := bucket.get("caddy/example.com.lock"); exists {
		t.Error("the lock file outlived the holder that released it")
	}
}

// The other half: an instance that dies holding a lock must not block the rest
// for longer than the expiration.
func TestAnAbandonedLockIsTakenOver(t *testing.T) {
	withLockTimings(t, 100*time.Millisecond, time.Minute, 10*time.Millisecond)

	bucket := newFakeBucket()
	storage := newTestStorage(t, bucket)

	// A lock file with nobody behind it, stamped well in the past.
	abandoned := time.Now().Add(-time.Hour).Format(time.RFC3339Nano)
	bucket.put("caddy/example.com.lock", []byte(abandoned))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := storage.Lock(ctx, "example.com"); err != nil {
		t.Fatalf("Lock did not take over an abandoned lock: %v", err)
	}
	_ = storage.Unlock(context.Background(), "example.com")
}

// A lock file that is not a timestamp must not wedge every instance forever.
func TestAnUnparsableLockIsTakenOver(t *testing.T) {
	withLockTimings(t, time.Minute, time.Minute, 10*time.Millisecond)

	bucket := newFakeBucket()
	storage := newTestStorage(t, bucket)

	bucket.put("caddy/example.com.lock", []byte("this is not a timestamp"))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := storage.Lock(ctx, "example.com"); err != nil {
		t.Fatalf("Lock did not take over an unparsable lock: %v", err)
	}
	_ = storage.Unlock(context.Background(), "example.com")
}

// Once a lock has been taken over, the previous holder's heartbeat must not
// keep writing to it — that would hold the new owner's lock open on their
// behalf and, worse, could outlive them.
func TestARefresherStopsAfterLosingItsLock(t *testing.T) {
	withLockTimings(t, 40*time.Millisecond, 10*time.Millisecond, 10*time.Millisecond)

	bucket := newFakeBucket()
	loser := newTestStorage(t, bucket)

	if err := loser.Lock(context.Background(), "example.com"); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	// Another instance takes it over, which changes the ETag.
	stolen := bucket.put("caddy/example.com.lock", []byte(time.Now().Format(time.RFC3339Nano)))

	// Several refresh intervals later, the loser must not have written over it.
	time.Sleep(100 * time.Millisecond)

	object, exists := bucket.get("caddy/example.com.lock")
	if !exists {
		t.Fatal("the lock file disappeared")
	}
	if object.etag != stolen {
		t.Errorf("the previous holder wrote to a lock it no longer owned (etag %s, want %s)",
			object.etag, stolen)
	}

	// And releasing must leave the new owner's lock alone.
	if err := loser.Unlock(context.Background(), "example.com"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if _, exists := bucket.get("caddy/example.com.lock"); !exists {
		t.Error("Unlock deleted a lock belonging to another instance")
	}
}

// A module that goes away — a config reload, say — has to stop heartbeating
// the locks it was holding. If it does not, they never expire and the instance
// that replaced it can never take them over.
func TestCleanupStopsRefreshing(t *testing.T) {
	withLockTimings(t, 40*time.Millisecond, 10*time.Millisecond, 10*time.Millisecond)

	bucket := newFakeBucket()
	storage := newTestStorage(t, bucket)

	if err := storage.Lock(context.Background(), "example.com"); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if err := storage.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	before := len(bucket.putHeaders())
	time.Sleep(60 * time.Millisecond) // several refresh intervals

	if after := len(bucket.putHeaders()); after != before {
		t.Errorf("%d writes after Cleanup, want none", after-before)
	}
}

// Contention is bounded by the expiration, but a broken bucket has to surface
// as an error rather than an indefinite wait.
func TestLockGivesUpOnABrokenBucket(t *testing.T) {
	withLockTimings(t, time.Minute, time.Minute, 10*time.Millisecond)

	timeoutWas := LockTimeout
	LockTimeout = 100 * time.Millisecond
	t.Cleanup(func() { LockTimeout = timeoutWas })

	bucket := newFakeBucket()
	bucket.setFailWith(http.StatusInternalServerError)
	storage := newTestStorage(t, bucket)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := storage.Lock(ctx, "example.com")
	if err == nil {
		t.Fatal("Lock succeeded against a bucket returning 500")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("Lock waited for the context instead of giving up on the bucket")
	}
}

// Some implementations omit the ETag on a write. Without it no later write can
// be conditional, so the lock has to go and fetch it.
func TestLockRecoversAMissingETag(t *testing.T) {
	withLockTimings(t, time.Minute, time.Minute, 10*time.Millisecond)

	bucket := newFakeBucket()
	bucket.dropETag = true
	storage := newTestStorage(t, bucket)

	if err := storage.Lock(context.Background(), "example.com"); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	storage.locksMu.Lock()
	handle := storage.locks["example.com"]
	storage.locksMu.Unlock()

	if handle == nil {
		t.Fatal("no lock was recorded")
	}
	if handle.currentETag() == "" {
		t.Error("the lock was taken without an ETag, so it can never be refreshed safely")
	}

	_ = storage.Unlock(context.Background(), "example.com")
}
