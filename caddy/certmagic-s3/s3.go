package s3

import (
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
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	s3sdk "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go/logging"
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

	locksMu sync.Mutex
	locks   map[string]*heldLock
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
		config.WithLogger(sdkLogger{s3.Logger}),
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
		// LOCAL CHANGE: upstream matches only *types.NoSuchKey, which
		// GetObject returns but HeadObject does not. isNotFound (lock.go)
		// also covers *types.NotFound and a bare HTTP 404, so Load and Stat
		// agree on fs.ErrNotExist, the sentinel certmagic keys its
		// "must I issue a certificate?" decision on.
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

// List returns the keys under prefix.
//
// LOCAL CHANGE: certmagic's storage contract is hierarchical. List(prefix,
// false) must return the immediate children of prefix, directories included,
// so that callers can walk the tree one level at a time; List(prefix, true)
// returns every descendant. The previous implementation ignored `recursive`
// and always returned every descendant, which broke certmagic's storage
// cleaning: it walked the returned keys as if they were directories, so it
// issued one listing per returned key and never found the per-site assets it
// looks for.
//
// S3 has no directories, but it emulates them: a listing with a "/" delimiter
// returns the objects directly under the prefix and the shared prefixes of
// its subdirectories, which is exactly the immediate-children semantics
// certmagic expects. Directory keys come back without their trailing slash,
// as they do in certmagic's own file storage.
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
	if !recursive {
		input.Delimiter = aws.String("/")
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

		// The subdirectories of the prefix, as opposed to the files in it.
		// Only a non-recursive listing produces them.
		for _, common := range result.CommonPrefixes {
			key := strings.TrimPrefix(aws.ToString(common.Prefix), s3.objName(""))
			keys = append(keys, strings.TrimSuffix(key, "/"))
		}
	}

	return keys, nil
}

// Stat reports what is known about a key.
//
// LOCAL CHANGE: S3 has no directories, so a key that is a prefix of other
// keys has no object of its own. certmagic's own file storage reports those
// keys as directories (KeyInfo.IsTerminal false) rather than as missing, and
// certmagic's account discovery relies on that: it lists the account
// folders, stats them, and keeps the ones that are not files. Without this,
// every stat of a folder came back as fs.ErrNotExist and the account was
// never found.
func (s3 *S3) Stat(ctx context.Context, key string) (certmagic.KeyInfo, error) {
	s3.Logger.Debug(fmt.Sprintf("Stat: %v", s3.objName(key)))
	var ki certmagic.KeyInfo

	input := &s3sdk.HeadObjectInput{
		Bucket: aws.String(s3.Bucket),
		Key:    aws.String(s3.objName(key)),
	}

	result, err := s3.Client.HeadObject(ctx, input)
	if err != nil {
		// LOCAL CHANGE: upstream matches only *types.NoSuchKey, which
		// HeadObject never returns — it answers a bare 404, surfaced by the
		// SDK as *types.NotFound. Without isNotFound (lock.go), Stat on a
		// missing key returned a generic error instead of fs.ErrNotExist.
		if isNotFound(err) {
			return s3.statDirectory(ctx, key, ki)
		}
		return ki, err
	}

	ki.Key = key
	ki.Size = aws.ToInt64(result.ContentLength)
	ki.Modified = aws.ToTime(result.LastModified)
	ki.IsTerminal = true
	return ki, nil
}

// statDirectory reports a key that has no object of its own but is a prefix
// of other keys as the directory it is.
func (s3 *S3) statDirectory(ctx context.Context, key string, ki certmagic.KeyInfo) (certmagic.KeyInfo, error) {
	prefix := strings.TrimSuffix(s3.objName(key), "/") + "/"

	input := &s3sdk.ListObjectsV2Input{
		Bucket:  aws.String(s3.Bucket),
		Prefix:  aws.String(prefix),
		MaxKeys: aws.Int32(1),
	}

	result, err := s3.Client.ListObjectsV2(ctx, input)
	if err != nil {
		return ki, err
	}
	if len(result.Contents) == 0 {
		return ki, fs.ErrNotExist
	}

	ki.Key = key
	ki.Modified = aws.ToTime(result.Contents[0].LastModified)
	ki.IsTerminal = false
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

// String keeps the credentials out of anything that formats this value.
//
// LOCAL CHANGE: MarshalLogObject below covers zap.Any, which is how
// certmagic logs the storage. This covers fmt's %v and %s, which would
// otherwise print every field of the struct, including the keys.
func (s3 *S3) String() string {
	return fmt.Sprintf("S3 storage: bucket=%s endpoint=%s prefix=%s",
		s3.Bucket, s3.Endpoint, s3.Prefix)
}

// MarshalLogObject implements zapcore.ObjectMarshaler, and deliberately
// emits nothing at all.
//
// LOCAL CHANGE: certmagic logs the whole storage value at INFO on every
// storage-cleaning pass ("cleaning storage unit"). Without this method zap
// falls back to reflection over the struct's JSON tags and writes
// secret_key and encryption_key out in clear text, putting the bucket
// credentials into every log aggregator the operator ships to.
//
// Nothing here is worth logging on a repeating INFO line: the bucket and
// endpoint are already visible in the config, and an empty object cannot
// leak a field somebody adds to the struct later.
func (s3 *S3) MarshalLogObject(zapcore.ObjectEncoder) error {
	return nil
}

// LOCAL CHANGE: the SDK logs to stderr by default, outside Caddy's structured
// log and without a level filter. This bridge sends the SDK log calls to
// zap, so Caddy's level applies.
type sdkLogger struct{ logger *zap.Logger }

func (l sdkLogger) Logf(classification logging.Classification, format string, v ...any) {
	level := zapcore.InfoLevel
	switch classification {
	case logging.Debug:
		level = zapcore.DebugLevel
	case logging.Warn:
		level = zapcore.WarnLevel
	}
	if !l.logger.Core().Enabled(level) {
		return
	}
	l.logger.Log(level, fmt.Sprintf(format, v...))
}

// validateHost reports whether host is a bare hostname, optionally with a
// numeric port. Anything else, such as a scheme or a path, would be
// concatenated into a nonsense endpoint that fails only at connect time.
//
// LOCAL CHANGE: host gets `https://` put in front of it below. Without this
// check, a value that already carried a scheme silently became
// `https://https://…` and surfaced much later as an unresolvable endpoint.
func validateHost(host string) error {
	if host == "" {
		return errors.New("host must not be empty")
	}
	if strings.Contains(host, "://") {
		return fmt.Errorf("host %q must not carry a scheme; use `endpoint` for a URL", host)
	}

	hostname := host
	if split, port, err := net.SplitHostPort(host); err == nil {
		// Not just any trailing colon: `https:example.com` splits cleanly
		// here too, and is no more a hostname than the form above.
		if _, err := strconv.Atoi(port); err != nil {
			return fmt.Errorf("host %q has a non-numeric port %q", host, port)
		}
		hostname = split
	}

	if hostname == "" {
		return fmt.Errorf("host %q has no hostname", host)
	}
	// An IPv6 literal such as "::1" contains colons that are not a scheme
	// or a path, so the check below must not see it as one.
	if net.ParseIP(hostname) == nil && strings.ContainsAny(hostname, "/:") {
		return fmt.Errorf("host %q must be a bare hostname", host)
	}

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
		// dispenser yields is the module name `s3` from `storage s3 {`,
		// which has no argument of its own. Erroring here breaks the whole
		// block with "wrong argument count or unexpected line ending after
		// 's3'" — upstream issue #19, which is what v1.4.0 shipped.
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
		// LOCAL CHANGE: validate the bare hostname before building the
		// endpoint from it; see validateHost above for why.
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
// LOCAL CHANGE: Caddy calls this when the module goes away, which on a
// config reload happens while the replacement is already running. Without
// it the outgoing instance would keep heartbeating locks that it no longer
// has any intention of releasing, and the locks would never expire.
func (s3 *S3) Cleanup() error {
	s3.locksMu.Lock()
	handles := make([]*heldLock, 0, len(s3.locks))
	for name, h := range s3.locks {
		handles = append(handles, h)
		delete(s3.locks, name)
	}
	s3.locksMu.Unlock()

	// Same stop sequence as Unlock: a handle leaves s3.locks before its stop
	// channel closes, so no handle is closed twice. A Lock call that
	// completes after this returns starts a refresher nothing stops; Caddy
	// cancels the module's jobs before it calls Cleanup, so none is in flight.
	//
	// Close every stop before waiting on any done: a refresher can sit in
	// putLock for up to LockRefreshInterval, and Caddy blocks the reload here.
	for _, h := range handles {
		close(h.stop)
	}
	for _, h := range handles {
		<-h.done
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
