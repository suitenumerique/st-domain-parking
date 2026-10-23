package main

// Fetching and validating the domain list.
//
// The domain list carries data only. Every piece of wording, every label and
// every fixed URL lives in the template, so a copy change never means touching
// the schema, and a domain entry stays six fields long. All six are mandatory:
// a half-filled entry would render a page with gaps in it, which is worse than
// a build that refuses to run.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// fields are the keys of a domain entry, all mandatory.
var fields = []string{
	"domain",
	"commune_name",
	"commune_zipcode",
	"email",
	"service_public_url",
	"siret",
}

// label is one DNS label. Deliberately strict: the domain becomes a directory
// name that Caddy resolves against the Host header, so anything exotic is
// rejected rather than sanitised.
const label = `[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?`

var patterns = []struct {
	field    string
	re       *regexp.Regexp
	expected string
}{
	{"domain", regexp.MustCompile(`^` + label + `(?:\.` + label + `)+$`), "a domain name"},
	{"commune_name", regexp.MustCompile(`^[^\x00-\x1f]{1,100}$`), "a commune name"},
	{"commune_zipcode", regexp.MustCompile(`^[0-9]{5}$`), "5 digits"},
	// Narrower than what RFC 5322 allows: the address goes into a mailto:
	// link, where `?`, `&`, `=` and `%` would add headers or a body to the
	// message the visitor sends. Accented letters stay allowed.
	{"email", regexp.MustCompile(`^[\pL\pN._+-]+@(?:[\pL\pN-]+\.)+[a-zA-Z]{2,}$`), "an email address"},
	{"siret", regexp.MustCompile(`^[0-9]{14}$`), "14 digits"},
	{
		"service_public_url",
		regexp.MustCompile(`^https://lannuaire\.service-public(\.gouv)?\.fr/[\w\-./]+$`),
		"a lannuaire.service-public[.gouv].fr URL",
	},
}

// maxDomain is the longest legal domain name. The per-label pattern does not
// bound the whole string, and the domain becomes a directory name: anything
// past 255 bytes fails mkdir with ENAMETOOLONG, which aborts the entire build
// rather than just that entry.
const maxDomain = 253

// fetchTimeout bounds a whole fetch: a stalled one would block the refresh
// loop, and with it SIGHUP and SIGTERM.
var fetchTimeout = 30 * time.Second

// httpClient follows redirects like Go's default client, except from https
// to plain http, where anyone on the path could rewrite the list.
var httpClient = &http.Client{CheckRedirect: refuseDowngrade}

func refuseDowngrade(request *http.Request, via []*http.Request) error {
	if via[0].URL.Scheme == "https" && request.URL.Scheme != "https" {
		return fmt.Errorf("refusing the redirect to %s, which leaves https", request.URL.Redacted())
	}
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	return nil
}

// Site is one parked domain. Every field comes straight from the domain list.
type Site struct {
	// Domain is the registrable domain, always stored without "www.".
	Domain         string
	CommuneName    string
	CommuneZipcode string
	Email          string
	// ServicePublicURL is the listing on lannuaire.service-public.fr or
	// lannuaire.service-public.gouv.fr.
	ServicePublicURL string
	Siret            string
}

// ServeHost is the host whose directory holds the generated page.
func (s Site) ServeHost() string {
	return "www." + s.Domain
}

// Hosts are every host to allowlist: the served one and the apex that
// redirects.
func (s Site) Hosts() []string {
	return []string{s.ServeHost(), s.Domain}
}

// load fetches rawURL and turns it into validated sites.
//
// Everything that can go wrong (transport, JSON syntax, schema) is an error,
// because the caller reacts the same way to all of them: log it and keep
// serving the previous build.
func load(ctx context.Context, rawURL string) ([]Site, error) {
	body, err := fetch(ctx, rawURL)
	if err != nil {
		return nil, err
	}

	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", rawURL, err)
	}

	entries, ok := payload.([]any)
	if object, isObject := payload.(map[string]any); isObject {
		entries, ok = object["domains"].([]any)
	}
	if !ok {
		return nil, errors.New("expected a list of domains, or an object with `domains`")
	}

	// An empty list is indistinguishable from "every commune was
	// deprovisioned at once", and acting on it deletes the whole tree: every
	// page 404s, and with the `.parked` markers gone Caddy stops being allowed
	// to have certificates for any of them. Recovering means re-issuing the
	// lot, against the rate limits. A feed that empties itself is a bug
	// upstream far more often than it is an instruction, so it is refused
	// like any other unusable answer.
	if len(entries) == 0 {
		return nil, errors.New("the domain list is empty, refusing to unpark everything")
	}

	sites := make([]Site, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for index, entry := range entries {
		site, err := parseSite(entry, index)
		if err != nil {
			return nil, err
		}
		if seen[site.Domain] {
			return nil, fmt.Errorf("domains[%d]: %s is listed more than once", index, site.Domain)
		}
		seen[site.Domain] = true
		sites = append(sites, site)
	}
	return sites, nil
}

func parseSite(raw any, index int) (Site, error) {
	where := fmt.Sprintf("domains[%d]", index)
	object, ok := raw.(map[string]any)
	if !ok {
		return Site{}, fmt.Errorf("%s must be an object", where)
	}

	// One check covers both missing and unexpected keys, since all six
	// fields are mandatory and there are no optional ones.
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	expected := slices.Sorted(slices.Values(fields))
	if !slices.Equal(keys, expected) {
		return Site{}, fmt.Errorf("%s: expected exactly %v, got %v", where, fields, keys)
	}

	values := make(map[string]string, len(fields))
	for _, field := range fields {
		value, ok := object[field].(string)
		value = strings.TrimSpace(value)
		if !ok || value == "" {
			return Site{}, fmt.Errorf("%s: `%s` must be a non-empty string", where, field)
		}
		values[field] = value
	}

	// The www prefix is always derived, never stored, so the served host and
	// the redirected apex cannot disagree.
	domain := strings.TrimRight(strings.ToLower(values["domain"]), ".")
	values["domain"] = strings.TrimPrefix(domain, "www.")
	where = values["domain"]

	// Checked before the pattern, so a pathological value is discarded
	// without being matched against it.
	if len(values["domain"]) > maxDomain {
		return Site{}, fmt.Errorf("%s: `domain` is longer than %d characters", where, maxDomain)
	}

	for _, p := range patterns {
		if !p.re.MatchString(values[p.field]) {
			return Site{}, fmt.Errorf("%s: `%s` must be %s, got %q", where, p.field, p.expected, values[p.field])
		}
	}

	return Site{
		Domain:           values["domain"],
		CommuneName:      values["commune_name"],
		CommuneZipcode:   values["commune_zipcode"],
		Email:            values["email"],
		ServicePublicURL: values["service_public_url"],
		Siret:            values["siret"],
	}, nil
}

// fetch reads rawURL: http(s)://, s3://, file:// or a bare path.
func fetch(ctx context.Context, rawURL string) ([]byte, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("could not fetch %s: %w", rawURL, err)
	}

	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	var body []byte
	switch parsed.Scheme {
	case "http", "https":
		body, err = fetchHTTP(ctx, rawURL)
	case "s3":
		body, err = fetchS3(ctx, parsed.Host, strings.TrimPrefix(parsed.Path, "/"))
	case "file":
		body, err = os.ReadFile(parsed.Path)
	case "":
		body, err = os.ReadFile(rawURL)
	default:
		return nil, fmt.Errorf("unsupported scheme %q in %s", parsed.Scheme, rawURL)
	}
	if err != nil {
		return nil, fmt.Errorf("could not fetch %s: %w", rawURL, err)
	}
	return body, nil
}

func fetchHTTP(ctx context.Context, rawURL string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", response.Status)
	}
	return io.ReadAll(response.Body)
}

// fetchS3 reads an object with the standard AWS_* configuration.
// AWS_ENDPOINT_URL points it at any S3-compatible store, which is then
// addressed path-style, as those stores generally expect.
func fetchS3(ctx context.Context, bucket, key string) ([]byte, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = os.Getenv("AWS_ENDPOINT_URL") != ""
	})

	result, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = result.Body.Close() }()
	return io.ReadAll(result.Body)
}
