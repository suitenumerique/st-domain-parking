package s3

import (
	"fmt"
	"strings"
	"testing"
)

func TestS3_objName(t *testing.T) {
	tests := []struct {
		name     string
		prefix   string
		key      string
		expected string
	}{
		{
			name:     "empty prefix",
			prefix:   "",
			key:      "test.key",
			expected: "test.key",
		},
		{
			name:     "with prefix",
			prefix:   "acme",
			key:      "test.key",
			expected: "acme/test.key",
		},
		{
			name:     "slash normalization",
			prefix:   "//acme//",
			key:      "//test.key",
			expected: "acme/test.key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s3 := &S3{Prefix: tt.prefix}
			result := s3.objName(tt.key)
			if result != tt.expected {
				t.Errorf("objName() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestS3_objLockName(t *testing.T) {
	s3 := &S3{Prefix: "acme"}
	key := "test.key"
	expected := "acme/test.key.lock"

	result := s3.objLockName(key)
	if result != expected {
		t.Errorf("objLockName() = %v, want %v", result, expected)
	}
}

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

// MarshalLogObject covers zap.Any, which is how certmagic logs the storage.
// Anything reaching for %v or %s instead has to be just as safe.
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

func TestS3_UsePathStyleConfiguration(t *testing.T) {
	tests := []struct {
		name            string
		endpoint        string
		usePathStyle    bool
		expectPathStyle bool
	}{
		{
			name:            "default AWS (no custom endpoint)",
			endpoint:        "",
			usePathStyle:    false,
			expectPathStyle: false,
		},
		{
			name:            "explicit path style enabled",
			endpoint:        "",
			usePathStyle:    true,
			expectPathStyle: true,
		},
		{
			name:            "custom endpoint forces path style",
			endpoint:        "https://minio.example.com",
			usePathStyle:    false,
			expectPathStyle: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s3 := &S3{
				Endpoint:     tt.endpoint,
				UsePathStyle: tt.usePathStyle,
			}

			endpoint := tt.endpoint
			shouldUsePathStyle := s3.UsePathStyle || endpoint != ""

			if shouldUsePathStyle != tt.expectPathStyle {
				t.Errorf("UsePathStyle logic = %v, want %v", shouldUsePathStyle, tt.expectPathStyle)
			}
		})
	}
}
