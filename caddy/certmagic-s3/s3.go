package s3

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	s3sdk "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/certmagic"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var ErrInvalidKey = errors.New("invalid key")

type S3 struct {
	Logger *zap.Logger

	// S3
	Client       *s3sdk.Client
	Host         string `json:"host"`
	Endpoint     string `json:"endpoint"`
	Insecure     bool   `json:"insecure"`
	Bucket       string `json:"bucket"`
	Region       string `json:"region"`
	AccessKey    string `json:"access_key"`
	SecretKey    string `json:"secret_key"`
	Profile      string `json:"profile"`
	RoleARN      string `json:"role_arn"`
	Prefix       string `json:"prefix"`
	UsePathStyle bool   `json:"use_path_style,omitempty"`

	// EncryptionKey is optional. If you do not wish to encrypt your certficates and key inside the S3 bucket, leave it empty.
	EncryptionKey string `json:"encryption_key"`

	iowrap IO

	// Locks held by this instance, by key, each with the goroutine that keeps
	// it fresh. LOCAL CHANGE: upstream holds no state between Lock and Unlock.
	locksMu sync.Mutex
	locks   map[string]*lockHandle
}

// lockHandle is one held lock: the refresher's off switch, and the ETag of the
// lock file as this instance last wrote it. The ETag is what makes every later
// write conditional, so an instance can never overwrite a lock it no longer
// owns.
type lockHandle struct {
	cancel context.CancelFunc
	done   chan struct{}

	mu   sync.Mutex
	etag string
}

func (h *lockHandle) currentETag() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.etag
}

func (h *lockHandle) setETag(etag string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.etag = etag
}

// stop cancels the refresher and waits for it to exit, so that by the time
// Unlock deletes the file no further writes can be in flight.
func (h *lockHandle) stop() {
	h.cancel()
	<-h.done
}

func init() {
	caddy.RegisterModule(new(S3))
}

func (s3 *S3) Provision(ctx caddy.Context) error {
	s3.Logger = ctx.Logger(s3)

	if s3.Host != "" {
		s3.Logger.Info("Using deprecated 'host' option, consider switching to 'endpoint'",
			zap.String("host", s3.Host),
			zap.String("endpoint", s3.Endpoint),
		)
	}

	client, err := s3.buildS3Client()
	if err != nil {
		return fmt.Errorf("failed to create S3 client: %w", err)
	}

	s3.Client = client
	return s3.setupEncryption()
}

func (s3 *S3) buildS3Client() (*s3sdk.Client, error) {
	configOptions := []func(*config.LoadOptions) error{
		config.WithRegion(s3.Region),
	}

	if s3.Endpoint != "" {
		// some non-AWS providers do not implement automatic checksums
		// see https://github.com/aws/aws-sdk-go-v2/discussions/2960 for more details
		configOptions = append(configOptions, config.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired))
	}

	if s3.Insecure {
		s3.Logger.Warn("TLS certificate verification is disabled - this is insecure and should only be used for testing")
		httpClient := &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true, // #nosec G402
				},
			},
		}
		configOptions = append(configOptions, config.WithHTTPClient(httpClient))
	}

	if s3.AccessKey != "" && s3.SecretKey != "" {
		configOptions = append(configOptions, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(s3.AccessKey, s3.SecretKey, "")))
	} else if s3.Profile != "" {
		configOptions = append(configOptions, config.WithSharedConfigProfile(s3.Profile))
	}

	cfg, err := config.LoadDefaultConfig(context.Background(), configOptions...)
	if err != nil {
		return nil, err
	}

	if s3.RoleARN != "" {
		stsClient := sts.NewFromConfig(cfg)
		provider := stscreds.NewAssumeRoleProvider(stsClient, s3.RoleARN)
		cfg.Credentials = aws.NewCredentialsCache(provider)
	}

	var s3Options []func(*s3sdk.Options)

	if s3.Endpoint != "" {
		s3Options = append(s3Options, func(o *s3sdk.Options) {
			o.BaseEndpoint = aws.String(s3.Endpoint)
		})
	}

	if s3.UsePathStyle {
		s3Options = append(s3Options, func(o *s3sdk.Options) {
			o.UsePathStyle = true
		})
	}

	return s3sdk.NewFromConfig(cfg, s3Options...), nil
}

func (s3 *S3) setupEncryption() error {
	if len(s3.EncryptionKey) == 0 {
		s3.Logger.Info("Clear text certificate storage active")
		s3.iowrap = &CleartextIO{}
	} else if len(s3.EncryptionKey) != 32 {
		s3.Logger.Error("encryption key must have exactly 32 bytes")
		return errors.New("encryption key must have exactly 32 bytes")
	} else {
		s3.Logger.Info("Encrypted certificate storage active")
		sb := &SecretBoxIO{}
		copy(sb.SecretKey[:], []byte(s3.EncryptionKey))
		s3.iowrap = sb
	}

	return nil
}

func (s3 *S3) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID: "caddy.storage.s3",
		New: func() caddy.Module {
			return new(S3)
		},
	}
}

var (
	// LockExpiration is how long a lock file may go unrefreshed before another
	// instance treats it as abandoned.
	//
	// LOCAL CHANGE: upstream declares this, never reads it, and instead ages
	// locks out after LockTimeout (15s). Nothing refreshed the lock while ACME
	// issuance ran, so a slow issuance had its lock stolen out from under it —
	// exactly what the lock exists to prevent. The holder now heartbeats (see
	// refreshLock), which is what makes a short expiration safe: a live holder
	// keeps its lock for as long as it needs, and a dead one is reclaimed
	// seconds after it stops, instead of blocking everyone for two minutes.
	LockExpiration = 15 * time.Second
	// LockRefreshInterval is how often a holder rewrites its lock file, at a
	// third of the expiration so two refreshes can fail before it looks dead.
	// Matches the cadence of certmagic's own FileStorage.
	LockRefreshInterval = 5 * time.Second
	LockPollInterval    = 1 * time.Second
	// LockTimeout bounds how long we keep retrying when the *bucket* is
	// misbehaving — unreachable, throttling, refusing us.
	//
	// LOCAL CHANGE: upstream applies this to the whole acquisition, so an
	// instance gave up 15s into someone else's perfectly healthy issuance and
	// certmagic failed the handshake. Waiting on a live lock is now bounded by
	// LockExpiration instead: either the holder finishes, or it stops
	// refreshing and we take the lock over.
	LockTimeout = 15 * time.Second
)

func (s3 *S3) Lock(ctx context.Context, key string) error {
	name := s3.objLockName(key)
	s3.Logger.Debug("acquiring lock", zap.String("key", name))

	// When the bucket first started failing. Reset on every answer we can
	// interpret, so only an unbroken run of failures counts against
	// LockTimeout.
	var failingSince time.Time

	for {
		// LOCAL CHANGE: honour context cancellation. Upstream can only be
		// interrupted by its own timeout, so a shutdown mid-lock waits it out.
		if err := ctx.Err(); err != nil {
			return err
		}

		// Timed from before the call, not after it: a bucket that hangs until
		// the SDK gives up burns the whole budget inside a single attempt, and
		// timing from the return would restart the clock every time.
		attemptedAt := time.Now()

		// LOCAL CHANGE: the heart of it. Upstream read the lock and then wrote
		// it as two separate calls, so two instances could both see no lock
		// and both create one. A conditional create is refused by the bucket
		// unless the object really does not exist yet, which is the only way
		// to make acquisition atomic.

		etag, err := s3.writeLockFile(ctx, key, "")
		switch {
		case err == nil:
			s3.hold(key, etag)
			return nil
		case isPreconditionFailed(err):
			// Someone holds it. Fall through and see whether they are alive.
			failingSince = time.Time{}
		default:
			if failingSince.IsZero() {
				failingSince = attemptedAt
			}
			if time.Since(failingSince) > LockTimeout {
				return fmt.Errorf("acquiring lock %s: %w", name, err)
			}
			// LOCAL CHANGE: upstream `continue`s here with neither a sleep nor
			// a deadline check, so any non-404 error — a network blip, a 403,
			// throttling — becomes an unbounded tight loop hammering S3.
			s3.Logger.Warn("lock write failed, retrying",
				zap.String("key", name), zap.Error(err))
			if err := sleep(ctx, LockPollInterval); err != nil {
				return err
			}
			continue
		}

		attemptedAt = time.Now()

		written, holderETag, err := s3.readLockFile(ctx, key)
		if err != nil {
			if isNotFound(err) {
				// Released between our write and our read. Go straight back
				// and try to claim it.
				continue
			}
			if failingSince.IsZero() {
				failingSince = attemptedAt
			}
			if time.Since(failingSince) > LockTimeout {
				return fmt.Errorf("reading lock %s: %w", name, err)
			}
			s3.Logger.Warn("lock read failed, retrying",
				zap.String("key", name), zap.Error(err))
			if err := sleep(ctx, LockPollInterval); err != nil {
				return err
			}
			continue
		}

		// A lock file we cannot parse has a zero timestamp, so it reads as
		// long expired and is taken over below — upstream's behaviour, minus
		// the blind overwrite.
		if time.Since(written) > LockExpiration {
			// Conditional on the ETag we just read: if the holder refreshed it
			// in the meantime, or another waiter got there first, the write is
			// refused and we go round again rather than trampling a live lock.
			etag, err := s3.writeLockFile(ctx, key, holderETag)
			if err == nil {
				s3.Logger.Info("took over an abandoned lock",
					zap.String("key", name),
					zap.Duration("stale_for", time.Since(written)),
				)
				s3.hold(key, etag)
				return nil
			}
			if !isPreconditionFailed(err) {
				s3.Logger.Warn("taking over lock failed, retrying",
					zap.String("key", name), zap.Error(err))
			}
		}

		if err := sleep(ctx, LockPollInterval); err != nil {
			return err
		}
	}
}

// hold records a newly acquired lock and starts the goroutine that keeps it
// fresh for as long as this instance holds it.
func (s3 *S3) hold(key, etag string) {
	ctx, cancel := context.WithCancel(context.Background())
	handle := &lockHandle{cancel: cancel, done: make(chan struct{}), etag: etag}

	s3.locksMu.Lock()
	if s3.locks == nil {
		s3.locks = make(map[string]*lockHandle)
	}
	previous := s3.locks[key]
	s3.locks[key] = handle
	s3.locksMu.Unlock()

	// certmagic serialises its own Lock/Unlock per key, so this is a
	// belt-and-braces guard against leaking a refresher.
	if previous != nil {
		previous.stop()
	}

	go s3.refreshLock(ctx, key, handle)
}

// refreshLock rewrites the lock file until the lock is released. Every write is
// conditional on the ETag of the previous one, so once the lock has been taken
// over this instance can no longer write to it.
func (s3 *S3) refreshLock(ctx context.Context, key string, handle *lockHandle) {
	defer close(handle.done)

	ticker := time.NewTicker(LockRefreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		etag, err := s3.writeLockFile(ctx, key, handle.currentETag())
		switch {
		case err == nil:
			handle.setETag(etag)
		case errors.Is(err, context.Canceled):
			return
		case isPreconditionFailed(err):
			// Taken over while we still believed we held it. Writing again
			// would stamp our timestamp onto someone else's lock and keep it
			// alive on their behalf, so stop refreshing.
			s3.Logger.Warn("lock was taken over by another instance",
				zap.String("key", s3.objLockName(key)))
			return
		default:
			// Transient: LockExpiration leaves room for two of these before
			// anyone considers the lock abandoned.
			s3.Logger.Warn("refreshing lock failed",
				zap.String("key", s3.objLockName(key)), zap.Error(err))
		}
	}
}

// sleep waits, unless the context ends first.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// isNotFound reports whether err means "this object does not exist".
//
// LOCAL CHANGE: upstream only checks for *types.NoSuchKey, which GetObject
// returns but HeadObject does not — Head answers a bare 404 that the SDK
// surfaces as *types.NotFound. Without this, Stat on a missing key returned a
// generic error instead of fs.ErrNotExist, which is the sentinel certmagic
// keys its "do I need to issue a certificate?" logic on.
func isNotFound(err error) bool {
	var noSuchKey *types.NoSuchKey
	if errors.As(err, &noSuchKey) {
		return true
	}

	var notFound *types.NotFound
	if errors.As(err, &notFound) {
		return true
	}

	var responseError *awshttp.ResponseError
	return errors.As(err, &responseError) &&
		responseError.HTTPStatusCode() == http.StatusNotFound
}

// isPreconditionFailed reports whether err is the bucket refusing a conditional
// write. 412 is the documented answer; AWS also answers 409 when a competing
// conditional write is still in flight, which means the same thing to us:
// somebody else has it.
func isPreconditionFailed(err error) bool {
	var responseError *awshttp.ResponseError
	if !errors.As(err, &responseError) {
		return false
	}

	status := responseError.HTTPStatusCode()
	return status == http.StatusPreconditionFailed || status == http.StatusConflict
}

// writeLockFile stamps the lock file with the current time and returns its new
// ETag. An empty ifMatch writes only if the object does not exist yet;
// otherwise the write lands only if the object is still exactly the one that
// ETag came from. Either way it is the bucket, not this process, that decides
// who wins — which is the whole point.
//
// LOCAL CHANGE: upstream writes unconditionally.
func (s3 *S3) writeLockFile(ctx context.Context, key, ifMatch string) (string, error) {
	// Nanoseconds, where upstream wrote whole seconds: the expiration is now
	// counted in seconds, so rounding to one is a large error. time.Parse with
	// the RFC3339 layout still reads both, including lock files left behind by
	// an older build.
	body := []byte(time.Now().Format(time.RFC3339Nano))

	input := &s3sdk.PutObjectInput{
		Bucket:        aws.String(s3.Bucket),
		Key:           aws.String(s3.objLockName(key)),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
	}
	if ifMatch == "" {
		input.IfNoneMatch = aws.String("*")
	} else {
		input.IfMatch = aws.String(ifMatch)
	}

	output, err := s3.Client.PutObject(ctx, input)
	if err != nil {
		return "", err
	}

	if etag := aws.ToString(output.ETag); etag != "" {
		return etag, nil
	}

	// Every S3 implementation is supposed to return the ETag on a write. One
	// that does not would leave us unable to make the next write conditional,
	// so pay for a HEAD rather than silently dropping back to blind writes.
	head, err := s3.Client.HeadObject(ctx, &s3sdk.HeadObjectInput{
		Bucket: aws.String(s3.Bucket),
		Key:    aws.String(s3.objLockName(key)),
	})
	if err != nil {
		return "", fmt.Errorf("lock written but its ETag is unknown: %w", err)
	}

	return aws.ToString(head.ETag), nil
}

// readLockFile returns when the lock was last written and the ETag it carries.
func (s3 *S3) readLockFile(ctx context.Context, key string) (time.Time, string, error) {
	output, err := s3.Client.GetObject(ctx, &s3sdk.GetObjectInput{
		Bucket: aws.String(s3.Bucket),
		Key:    aws.String(s3.objLockName(key)),
	})
	if err != nil {
		return time.Time{}, "", err
	}
	defer func() { _ = output.Body.Close() }()

	body, err := io.ReadAll(output.Body)
	if err != nil {
		return time.Time{}, "", err
	}

	etag := aws.ToString(output.ETag)

	written, err := time.Parse(time.RFC3339, string(body))
	if err != nil {
		// Not a timestamp at all. The zero time reads as long expired, so the
		// caller takes it over — but still conditionally, on the ETag.
		return time.Time{}, etag, nil
	}

	return written, etag, nil
}

func (s3 *S3) Unlock(ctx context.Context, key string) error {
	name := s3.objLockName(key)
	s3.Logger.Debug("releasing lock", zap.String("key", name))

	s3.locksMu.Lock()
	handle := s3.locks[key]
	delete(s3.locks, key)
	s3.locksMu.Unlock()

	if handle != nil {
		// Stop the refresher first and wait for it, so no write can land after
		// the delete below and resurrect a lock nobody holds.
		handle.stop()

		// LOCAL CHANGE: S3 has no conditional delete, so an ETag check is the
		// closest thing available to "delete only if it is still mine". It is
		// not atomic; losing the race merely leaves a lock file to expire on
		// its own, whereas deleting one that another instance now holds would
		// hand its certificate order to a third.
		head, err := s3.Client.HeadObject(ctx, &s3sdk.HeadObjectInput{
			Bucket: aws.String(s3.Bucket),
			Key:    aws.String(name),
		})
		switch {
		case isNotFound(err):
			return nil
		case err == nil && aws.ToString(head.ETag) != handle.currentETag():
			s3.Logger.Warn("not releasing a lock that is no longer ours",
				zap.String("key", name))
			return nil
		}
	}

	_, err := s3.Client.DeleteObject(ctx, &s3sdk.DeleteObjectInput{
		Bucket: aws.String(s3.Bucket),
		Key:    aws.String(name),
	})
	return err
}

func (s3 *S3) Store(ctx context.Context, key string, value []byte) error {
	start := time.Now()
	objName := s3.objName(key)

	if len(value) == 0 {
		return fmt.Errorf("%w: cannot store empty value", ErrInvalidKey)
	}

	s3.Logger.Info("storing object",
		zap.String("key", objName),
		zap.Int("size", len(value)),
		zap.String("bucket", s3.Bucket),
	)

	defer func() {
		s3.Logger.Debug("store completed",
			zap.String("key", objName),
			zap.Duration("duration", time.Since(start)),
		)
	}()

	r := s3.iowrap.ByteReader(value)

	input := &s3sdk.PutObjectInput{
		Bucket:        aws.String(s3.Bucket),
		Key:           aws.String(objName),
		Body:          &r,
		ContentLength: aws.Int64(r.Len()),
	}

	_, err := s3.Client.PutObject(ctx, input)
	if err != nil {
		return fmt.Errorf("failed to store key %s: %w", key, err)
	}
	return nil
}

func (s3 *S3) Load(ctx context.Context, key string) ([]byte, error) {
	start := time.Now()
	objName := s3.objName(key)

	s3.Logger.Info("loading object",
		zap.String("key", objName),
		zap.String("bucket", s3.Bucket),
	)

	defer func() {
		s3.Logger.Debug("load completed",
			zap.String("key", objName),
			zap.Duration("duration", time.Since(start)),
		)
	}()

	input := &s3sdk.GetObjectInput{
		Bucket: aws.String(s3.Bucket),
		Key:    aws.String(objName),
	}

	result, err := s3.Client.GetObject(ctx, input)
	if err != nil {
		if isNotFound(err) {
			return nil, fs.ErrNotExist
		}
		return nil, fmt.Errorf("failed to load key %s: %w", key, err)
	}
	defer func() { _ = result.Body.Close() }()

	buf, err := io.ReadAll(s3.iowrap.WrapReader(result.Body))
	if err != nil {
		return nil, fmt.Errorf("failed to read/decrypt data for key %s: %w", key, err)
	}
	return buf, nil
}

func (s3 *S3) Delete(ctx context.Context, key string) error {
	start := time.Now()
	objName := s3.objName(key)

	s3.Logger.Info("deleting object",
		zap.String("key", objName),
		zap.String("bucket", s3.Bucket),
	)

	defer func() {
		s3.Logger.Debug("delete completed",
			zap.String("key", objName),
			zap.Duration("duration", time.Since(start)),
		)
	}()

	input := &s3sdk.DeleteObjectInput{
		Bucket: aws.String(s3.Bucket),
		Key:    aws.String(objName),
	}

	_, err := s3.Client.DeleteObject(ctx, input)
	if err != nil {
		return fmt.Errorf("failed to delete key %s: %w", key, err)
	}
	return nil
}

func (s3 *S3) Exists(ctx context.Context, key string) bool {
	objName := s3.objName(key)

	s3.Logger.Debug("checking object existence",
		zap.String("key", objName),
		zap.String("bucket", s3.Bucket),
	)

	input := &s3sdk.HeadObjectInput{
		Bucket: aws.String(s3.Bucket),
		Key:    aws.String(objName),
	}

	_, err := s3.Client.HeadObject(ctx, input)
	exists := err == nil

	s3.Logger.Debug("existence check completed",
		zap.String("key", objName),
		zap.Bool("exists", exists),
	)

	return exists
}

func (s3 *S3) List(ctx context.Context, prefix string, recursive bool) ([]string, error) {
	var keys []string

	fullPrefix := s3.objName(prefix)
	if prefix != "" {
		fullPrefix += "/"
	}
	input := &s3sdk.ListObjectsV2Input{
		Bucket: aws.String(s3.Bucket),
		Prefix: aws.String(fullPrefix),
	}

	paginator := s3sdk.NewListObjectsV2Paginator(s3.Client, input)
	for paginator.HasMorePages() {
		result, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}

		for _, obj := range result.Contents {
			key := aws.ToString(obj.Key)
			// Strip the configured prefix from the key before appending
			keys = append(keys, strings.TrimPrefix(key, s3.objName("")))
		}
	}

	return keys, nil
}

func (s3 *S3) Stat(ctx context.Context, key string) (certmagic.KeyInfo, error) {
	s3.Logger.Info(fmt.Sprintf("Stat: %v", s3.objName(key)))
	var ki certmagic.KeyInfo

	input := &s3sdk.HeadObjectInput{
		Bucket: aws.String(s3.Bucket),
		Key:    aws.String(s3.objName(key)),
	}

	result, err := s3.Client.HeadObject(ctx, input)
	if err != nil {
		if isNotFound(err) {
			return ki, fs.ErrNotExist
		}
		return ki, err
	}

	ki.Key = key
	ki.Size = aws.ToInt64(result.ContentLength)
	ki.Modified = aws.ToTime(result.LastModified)
	ki.IsTerminal = true
	return ki, nil
}

func (s3 *S3) objName(key string) string {
	prefix := strings.Trim(s3.Prefix, "/")
	key = strings.TrimLeft(key, "/")

	if prefix == "" {
		return key
	}
	return prefix + "/" + key
}

func (s3 *S3) objLockName(key string) string {
	return s3.objName(key) + ".lock"
}

// CertMagicStorage converts s to a certmagic.Storage instance.
func (s3 *S3) CertMagicStorage() (certmagic.Storage, error) {
	return s3, nil
}

// MarshalLogObject implements zapcore.ObjectMarshaler, and deliberately emits
// nothing at all.
//
// LOCAL CHANGE: certmagic logs the whole storage value at INFO on every
// storage-cleaning pass ("cleaning storage unit"). Without this method zap
// falls back to reflection over the struct's JSON tags and writes secret_key
// and encryption_key out in clear text, putting the bucket credentials into
// every log aggregator the operator ships to.
//
// Nothing here is worth logging on a repeating INFO line — the bucket and
// endpoint are already visible in the config — and an empty object cannot
// leak a field somebody adds to the struct later.
// validateHost reports whether host is a bare hostname, optionally with a
// numeric port. Anything else — a scheme, a path, a stray slash — would be
// concatenated into a nonsense endpoint that fails only at connect time.
func validateHost(host string) error {
	if host == "" {
		return errors.New("host must not be empty")
	}
	if strings.Contains(host, "://") {
		return fmt.Errorf("host %q must not carry a scheme; use `endpoint` for a URL", host)
	}

	hostname := host
	if split, port, err := net.SplitHostPort(host); err == nil {
		// Not just any trailing colon: `https:example.com` splits cleanly here
		// too, and is no more a hostname than the form above.
		if _, err := strconv.Atoi(port); err != nil {
			return fmt.Errorf("host %q has a non-numeric port %q", host, port)
		}
		hostname = split
	}

	if hostname == "" {
		return fmt.Errorf("host %q has no hostname", host)
	}
	if strings.ContainsAny(hostname, "/:") {
		return fmt.Errorf("host %q must be a bare hostname", host)
	}

	return nil
}

// String keeps the credentials out of anything that formats this value.
//
// LOCAL CHANGE: MarshalLogObject below covers zap.Any, which is how certmagic
// logs the storage. This covers fmt's %v and %s, which would otherwise print
// every field of the struct — including the keys.
func (s3 *S3) String() string {
	return fmt.Sprintf("S3 storage: bucket=%s endpoint=%s prefix=%s",
		s3.Bucket, s3.Endpoint, s3.Prefix)
}

func (s3 *S3) MarshalLogObject(zapcore.ObjectEncoder) error {
	return nil
}

func parseBool(value string) (bool, error) {
	return strconv.ParseBool(value)
}

func (s3 *S3) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		key := d.Val()
		var value string

		// Load-bearing, do not turn this into an error: the first token the
		// dispenser yields is the module name `s3` from `storage s3 {`, which
		// has no argument of its own. Erroring here breaks the whole block
		// with "wrong argument count or unexpected line ending after 's3'" —
		// upstream issue #19, which is what v1.4.0 shipped.
		if !d.Args(&value) {
			continue
		}

		switch key {
		case "host":
			s3.Host = value
		case "endpoint":
			s3.Endpoint = value
		case "insecure":
			parsed, err := parseBool(value)
			if err != nil {
				return d.Errf("invalid boolean value for 'insecure': %v", err)
			}
			s3.Insecure = parsed
		case "bucket":
			s3.Bucket = value
		case "region":
			s3.Region = value
		case "access_key":
			s3.AccessKey = value
		case "secret_key":
			s3.SecretKey = value
		case "profile":
			s3.Profile = value
		case "role_arn":
			s3.RoleARN = value
		case "prefix":
			s3.Prefix = value
		case "encryption_key":
			if value != "" && len(value) != 32 {
				return d.Errf("encryption_key must be exactly 32 bytes, got %d", len(value))
			}
			s3.EncryptionKey = value
		case "use_path_style":
			parsed, err := parseBool(value)
			if err != nil {
				return d.Errf("invalid boolean value for 'use_path_style': %v", err)
			}
			s3.UsePathStyle = parsed
		default:
			return d.Errf("unknown configuration option: %s", key)
		}
	}

	if s3.Region == "" {
		s3.Region = "us-east-1"
	}
	if s3.Prefix == "" {
		s3.Prefix = "acme"
	}

	if s3.Bucket == "" {
		return d.Err("bucket is required")
	}

	if s3.Host != "" && s3.Endpoint != "" {
		return d.Err("cannot specify both 'host' and 'endpoint' options")
	}
	if s3.Host != "" && s3.Endpoint == "" {
		// LOCAL CHANGE: `host` is a bare
		// hostname that gets https:// put in front of it. Without this, a URL
		// here silently became `https://https://…` and surfaced much later as
		// an unresolvable endpoint.
		if err := validateHost(s3.Host); err != nil {
			return d.Errf("%v", err)
		}
		s3.Endpoint = "https://" + s3.Host
	}
	if s3.Endpoint != "" && !s3.UsePathStyle {
		s3.UsePathStyle = true
	}

	return nil
}

// Cleanup stops every refresher this instance is running.
//
// LOCAL CHANGE: Caddy calls this when the module goes away, which on a config
// reload happens while the replacement is already running. Without it the
// outgoing instance would keep heartbeating locks that it no longer has any
// intention of releasing, and the locks would never expire.
func (s3 *S3) Cleanup() error {
	s3.locksMu.Lock()
	handles := make([]*lockHandle, 0, len(s3.locks))
	for key, handle := range s3.locks {
		handles = append(handles, handle)
		delete(s3.locks, key)
	}
	s3.locksMu.Unlock()

	for _, handle := range handles {
		handle.stop()
	}

	return nil
}

var (
	_ caddy.Provisioner       = (*S3)(nil)
	_ caddy.StorageConverter  = (*S3)(nil)
	_ caddy.CleanerUpper      = (*S3)(nil)
	_ caddyfile.Unmarshaler   = (*S3)(nil)
	_ zapcore.ObjectMarshaler = (*S3)(nil)
	_ fmt.Stringer            = (*S3)(nil)
)
