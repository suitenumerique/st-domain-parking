package s3

import (
	"bytes"
	"strings"
	"testing"

	"github.com/aws/smithy-go/logging"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// certmagic logs the storage value at INFO on every storage-cleaning pass,
// as zap.Any("storage", storage). Without an ObjectMarshaler, zap reflects
// over the struct's JSON tags and writes the credentials out in clear text.
// This reproduces that call and asserts nothing sensitive survives it.
func TestMarshalLogObjectLeaksNothing(t *testing.T) {
	secrets := map[string]string{
		"secret key":     "SuperSecretKey123",
		"access key":     "AKIAEXAMPLEACCESS",
		"encryption key": strings.Repeat("e", 32),
	}

	storage := &S3{
		Bucket:        "certificates",
		Endpoint:      "https://s3.example.invalid",
		Region:        "eu-west-3",
		Prefix:        "caddy",
		AccessKey:     secrets["access key"],
		SecretKey:     secrets["secret key"],
		EncryptionKey: secrets["encryption key"],
	}

	var buffer bytes.Buffer
	logger := zap.New(zapcore.NewCore(
		zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()),
		zapcore.AddSync(&buffer),
		zapcore.InfoLevel,
	))
	logger.Info("cleaning storage unit", zap.Any("storage", storage))

	logged := buffer.String()
	for name, secret := range secrets {
		if strings.Contains(logged, secret) {
			t.Errorf("%s was written to the log: %s", name, logged)
		}
	}

	// Guard against the marshaller being dropped: reflection would emit the
	// struct's fields, so an empty object is the signal that it ran.
	if !strings.Contains(logged, `"storage":{}`) {
		t.Errorf(`expected an empty "storage" object, got: %s`, logged)
	}
}

func newBufferLogger(level zapcore.Level) (*zap.Logger, *bytes.Buffer) {
	var buffer bytes.Buffer
	logger := zap.New(zapcore.NewCore(
		zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()),
		zapcore.AddSync(&buffer),
		level,
	))
	return logger, &buffer
}

// sdkLogger sends the SDK's log calls through zap so Caddy's log level
// applies. A Debug call must stay below Caddy's default level, and both
// levels must come through when the core accepts them.
func TestSDKLoggerRespectsLevel(t *testing.T) {
	logger, buffer := newBufferLogger(zapcore.InfoLevel)
	sdkLog := sdkLogger{logger}

	sdkLog.Logf(logging.Debug, "response has no supported checksum")
	if buffer.Len() != 0 {
		t.Errorf("expected no output for a Debug message at InfoLevel, got: %s", buffer.String())
	}

	sdkLog.Logf(logging.Warn, "retrying request %d of %d", 2, 3)
	logged := buffer.String()
	if !strings.Contains(logged, "retrying request 2 of 3") {
		t.Errorf("expected the formatted message in the log, got: %s", logged)
	}
	if !strings.Contains(logged, `"level":"warn"`) {
		t.Errorf(`expected "level":"warn" in the log, got: %s`, logged)
	}

	logger, buffer = newBufferLogger(zapcore.DebugLevel)
	sdkLogger{logger}.Logf(logging.Debug, "response has no supported checksum")
	logged = buffer.String()
	if !strings.Contains(logged, "response has no supported checksum") || !strings.Contains(logged, `"level":"debug"`) {
		t.Errorf(`expected the Debug message at "level":"debug", got: %s`, logged)
	}
}

// Guards against the bridge being dropped from buildS3Client on a rebase:
// without it the SDK falls back to its own stderr logger.
func TestBuildS3ClientInstallsTheLogBridge(t *testing.T) {
	storage := &S3{Region: "eu-west-3", Logger: zap.NewNop()}
	client, err := storage.buildS3Client()
	if err != nil {
		t.Fatalf("buildS3Client: %v", err)
	}
	if _, ok := client.Options().Logger.(sdkLogger); !ok {
		t.Fatalf("expected sdkLogger, got %T", client.Options().Logger)
	}
}
