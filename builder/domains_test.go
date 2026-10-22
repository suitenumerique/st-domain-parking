package main

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func valid() map[string]any {
	return map[string]any{
		"domain":             "brigny.fr",
		"commune_name":       "Brigny",
		"commune_zipcode":    "87200",
		"email":              "contact@brigny.fr",
		"service_public_url": "https://lannuaire.service-public.fr/mairie-87030-01",
		"siret":              "21510372200017",
	}
}

func with(changes map[string]any) map[string]any {
	entry := valid()
	maps.Copy(entry, changes)
	return entry
}

// write stores payload as a domain list and returns its path.
func write(t *testing.T, payload any) string {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "domains.json")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustLoad(t *testing.T, url string) []Site {
	t.Helper()
	sites, err := load(context.Background(), url)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return sites
}

// loadFails checks that loading url fails with an error that mentions want.
func loadFails(t *testing.T, url, want string) {
	t.Helper()
	_, err := load(context.Background(), url)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("load: got %v, want an error mentioning %q", err, want)
	}
}

func TestAValidEntryRoundTrips(t *testing.T) {
	sites := mustLoad(t, write(t, map[string]any{"domains": []any{valid()}}))

	site := sites[0]
	if len(sites) != 1 || site.Domain != "brigny.fr" || site.CommuneName != "Brigny" || site.Siret != "21510372200017" {
		t.Fatalf("got %+v", sites)
	}
	if site.ServeHost() != "www.brigny.fr" {
		t.Errorf("ServeHost = %q", site.ServeHost())
	}
	if !slices.Equal(site.Hosts(), []string{"www.brigny.fr", "brigny.fr"}) {
		t.Errorf("Hosts = %v", site.Hosts())
	}
}

func TestAGouvFrServicePublicURLIsAccepted(t *testing.T) {
	url := "https://lannuaire.service-public.gouv.fr/mairie-87030-01"
	sites := mustLoad(t, write(t, []any{with(map[string]any{"service_public_url": url})}))

	if sites[0].ServicePublicURL != url {
		t.Errorf("got %q", sites[0].ServicePublicURL)
	}
}

// Acting on an empty list unparks every domain at once, and the markers it
// deletes are what let Caddy hold certificates for them. Treated like any
// other unusable answer, so the previous build keeps being served.
func TestAnEmptyListIsRefused(t *testing.T) {
	for _, payload := range []any{[]any{}, map[string]any{"domains": []any{}}} {
		loadFails(t, write(t, payload), "empty")
	}
}

func TestABareListIsAccepted(t *testing.T) {
	sites := mustLoad(t, write(t, []any{valid(), with(map[string]any{"domain": "other.fr"})}))

	if len(sites) != 2 || sites[0].Domain != "brigny.fr" || sites[1].Domain != "other.fr" {
		t.Errorf("got %+v", sites)
	}
}

func TestTheWWWPrefixIsStrippedAndTheDomainNormalised(t *testing.T) {
	sites := mustLoad(t, write(t, []any{with(map[string]any{"domain": "  WWW.Brigny.FR.  "})}))

	if sites[0].Domain != "brigny.fr" || sites[0].ServeHost() != "www.brigny.fr" {
		t.Errorf("got %+v", sites[0])
	}
}

func TestADomainListedTwiceIsRejected(t *testing.T) {
	loadFails(t, write(t, []any{valid(), with(map[string]any{"domain": "www.brigny.fr"})}), "more than once")
}

func TestEveryFieldIsMandatory(t *testing.T) {
	for _, field := range fields {
		entry := valid()
		delete(entry, field)
		loadFails(t, write(t, []any{entry}), "expected exactly")
	}
}

func TestUnknownFieldsAreRejected(t *testing.T) {
	loadFails(t, write(t, []any{with(map[string]any{"accent": "#ff0000"})}), "expected exactly")
}

func TestFieldsMustBeNonEmptyStrings(t *testing.T) {
	for _, value := range []any{"", "   ", nil, 42, []any{"x"}} {
		loadFails(t, write(t, []any{with(map[string]any{"commune_name": value})}), "non-empty string")
	}
}

func TestMalformedValuesAreRejected(t *testing.T) {
	for _, tt := range []struct{ field, value string }{
		{"domain", "not a domain"},
		{"domain", "localhost"},
		{"domain", "-brigny.fr"},
		{"domain", "brigny-.fr"},
		{"domain", "../etc/passwd"},
		{"domain", "brigny.fr/x"},
		{"domain", strings.Repeat("a", 64) + ".fr"},
		{"commune_name", "Brigny\x07"},
		{"commune_zipcode", "8720"},
		{"commune_zipcode", "87200x"},
		{"commune_zipcode", "٨٧٢٠٠"},
		{"email", "nope"},
		{"email", "a@b"},
		{"siret", "123"},
		{"siret", "2151037220001x"},
		{"service_public_url", "https://evil.example/x"},
		{"service_public_url", "javascript:alert(1)"},
		{"service_public_url", "http://lannuaire.service-public.fr/x"},
	} {
		t.Run(tt.field+"="+tt.value, func(t *testing.T) {
			loadFails(t, write(t, []any{with(map[string]any{tt.field: tt.value})}), "`"+tt.field+"` must be")
		})
	}
}

func TestADomainLongerThanDNSAllowsIsRejected(t *testing.T) {
	long := strings.Repeat(strings.Repeat("a", 60)+".", 5) + "fr"
	loadFails(t, write(t, []any{with(map[string]any{"domain": long})}), "longer than 253")
}

func TestMalformedPayloadsAreRejected(t *testing.T) {
	for _, payload := range []any{"a string", map[string]any{"domains": "not a list"}, map[string]any{}, 42, []any{[]any{"not an object"}}} {
		if _, err := load(context.Background(), write(t, payload)); err == nil {
			t.Errorf("load accepted %v", payload)
		}
	}
}

func TestInvalidJSONIsReported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "domains.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	loadFails(t, path, "not valid JSON")
}

func TestAMissingFileIsReported(t *testing.T) {
	loadFails(t, filepath.Join(t.TempDir(), "absent.json"), "could not fetch")
}

func TestAFileURLIsRead(t *testing.T) {
	sites := mustLoad(t, "file://"+write(t, []any{valid()}))
	if sites[0].Domain != "brigny.fr" {
		t.Errorf("got %+v", sites)
	}
}

func TestAnHTTPURLIsRead(t *testing.T) {
	body, _ := json.Marshal([]any{valid()})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/domains.json" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	if sites := mustLoad(t, srv.URL+"/domains.json"); sites[0].Domain != "brigny.fr" {
		t.Errorf("got %+v", sites)
	}
	// An error page is not a domain list, whatever it contains.
	loadFails(t, srv.URL+"/missing.json", "404")
}

// AWS_ENDPOINT_URL points s3:// at an S3-compatible store, addressed
// path-style: the fake below only answers at localhost/<bucket>/<key>. The
// endpoint is a hostname, not an IP, which the SDK would address path-style
// whatever it was told.
func TestAnS3URLIsRead(t *testing.T) {
	body, _ := json.Marshal([]any{valid()})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasPrefix(r.Host, "localhost:") || r.URL.Path != "/lists/prod/domains.json" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchKey</Code></Error>`))
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	t.Setenv("AWS_ENDPOINT_URL", strings.Replace(srv.URL, "127.0.0.1", "localhost", 1))
	t.Setenv("AWS_ACCESS_KEY_ID", "id")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "none"))

	if sites := mustLoad(t, "s3://lists/prod/domains.json"); sites[0].Domain != "brigny.fr" {
		t.Errorf("got %+v", sites)
	}
	loadFails(t, "s3://lists/prod/missing.json", "could not fetch")
}

func TestAnUnsupportedSchemeIsReported(t *testing.T) {
	loadFails(t, "ftp://example.com/domains.json", "unsupported scheme")
}
