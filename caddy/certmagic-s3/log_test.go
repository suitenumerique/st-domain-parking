package s3

import (
	"bytes"
	"strings"
	"testing"

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
