package s3

import (
	"crypto/md5" // #nosec G501 -- S3 computes ETags this way; nothing here is security
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	s3sdk "github.com/aws/aws-sdk-go-v2/service/s3"
	"go.uber.org/zap"
)

// fakeS3 is an in-memory, path-style S3 endpoint holding single objects. It
// applies each request atomically and honours If-None-Match: * and If-Match
// on PUT the way S3 and S3-compatible services with conditional writes do.
type fakeS3 struct {
	t *testing.T

	mu      sync.Mutex
	objects map[string]*fakeObject

	// ignoreDeleteIfMatch deletes regardless of If-Match, as services that
	// do not support conditional deletes do.
	ignoreDeleteIfMatch bool
}

type fakeObject struct {
	body     []byte
	etag     string
	modified time.Time
}

func newFakeS3(t *testing.T) (*fakeS3, *httptest.Server) {
	t.Helper()
	f := &fakeS3{t: t, objects: make(map[string]*fakeObject)}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

// instance returns a new S3 storage talking to the fake, as a separate
// process would.
func (f *fakeS3) instance(srv *httptest.Server) *S3 {
	client := s3sdk.New(s3sdk.Options{
		BaseEndpoint:               aws.String(srv.URL),
		Region:                     "us-east-1",
		UsePathStyle:               true,
		Credentials:                credentials.NewStaticCredentialsProvider("id", "secret", ""),
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
	return &S3{Logger: zap.NewNop(), Client: client, Bucket: "test"}
}

// seed stores an object last written at modified.
func (f *fakeS3) seed(key string, modified time.Time) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.write(key, []byte(modified.UTC().Format(time.RFC3339)), modified)
}

func (f *fakeS3) get(key string) (fakeObject, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	obj, ok := f.objects[key]
	if !ok {
		return fakeObject{}, false
	}
	return *obj, true
}

// write stores an object. Its ETag is the MD5 of its content, as S3 and
// S3-compatible services compute it for single-part uploads.
func (f *fakeS3) write(key string, body []byte, modified time.Time) string {
	etag := fmt.Sprintf("%q", fmt.Sprintf("%x", md5.Sum(body))) // #nosec G401 -- as above
	f.objects[key] = &fakeObject{body: body, etag: etag, modified: modified}
	return etag
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2" {
		f.list(w, r)
		return
	}

	key := strings.TrimPrefix(r.URL.Path, "/test/")

	f.mu.Lock()
	defer f.mu.Unlock()
	obj, exists := f.objects[key]

	switch r.Method {
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			f.t.Errorf("fake s3: reading body: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if r.Header.Get("If-None-Match") == "*" && exists {
			writeError(w, http.StatusPreconditionFailed, "PreconditionFailed")
			return
		}
		if m := r.Header.Get("If-Match"); m != "" && (!exists || obj.etag != m) {
			if !exists {
				writeError(w, http.StatusNotFound, "NoSuchKey")
				return
			}
			writeError(w, http.StatusPreconditionFailed, "PreconditionFailed")
			return
		}
		w.Header().Set("ETag", f.write(key, body, time.Now()))
		w.WriteHeader(http.StatusOK)

	case http.MethodGet, http.MethodHead:
		if !exists {
			if r.Method == http.MethodHead {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			writeError(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		w.Header().Set("ETag", obj.etag)
		w.Header().Set("Last-Modified", obj.modified.UTC().Format(http.TimeFormat))
		w.Header().Set("Content-Length", fmt.Sprint(len(obj.body)))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(obj.body)
		}

	case http.MethodDelete:
		if m := r.Header.Get("If-Match"); m != "" && !f.ignoreDeleteIfMatch && exists && obj.etag != m {
			writeError(w, http.StatusPreconditionFailed, "PreconditionFailed")
			return
		}
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func writeError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, code)
}

// list implements ListObjectsV2, including the delimiter S3 uses to emulate
// directories: with a delimiter, the keys that share a prefix up to the next
// delimiter come back as common prefixes rather than as contents. As in S3,
// each common prefix counts once against max-keys, and pages run through
// contents and common prefixes together, in key order.
func (f *fakeS3) list(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	prefix := query.Get("prefix")
	delimiter := query.Get("delimiter")
	after := query.Get("continuation-token")
	maxKeys := 1000
	if raw := query.Get("max-keys"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			maxKeys = parsed
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	keys := make([]string, 0, len(f.objects))
	for key := range f.objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	// Every entry of the listing, keyed by what it returns: the key itself
	// for contents, the shared prefix for common prefixes.
	entries := make(map[string]bool) // true for a common prefix
	for _, key := range keys {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		entry, isPrefix := key, false
		if delimiter != "" {
			rest := strings.TrimPrefix(key, prefix)
			if i := strings.Index(rest, delimiter); i >= 0 {
				entry, isPrefix = prefix+rest[:i+len(delimiter)], true
			}
		}
		if after != "" && entry <= after {
			continue
		}
		entries[entry] = isPrefix
	}
	ordered := make([]string, 0, len(entries))
	for entry := range entries {
		ordered = append(ordered, entry)
	}
	sort.Strings(ordered)

	truncated := false
	var nextToken string
	if len(ordered) > maxKeys {
		ordered = ordered[:maxKeys]
		truncated = true
		nextToken = ordered[len(ordered)-1]
	}

	var contents, prefixes []string
	for _, entry := range ordered {
		if entries[entry] {
			prefixes = append(prefixes, entry)
		} else {
			contents = append(contents, entry)
		}
	}

	var body strings.Builder
	body.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	body.WriteString(`<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
	body.WriteString(`<Name>test</Name>`)
	fmt.Fprintf(&body, `<Prefix>%s</Prefix>`, prefix)
	fmt.Fprintf(&body, `<KeyCount>%d</KeyCount>`, len(ordered))
	fmt.Fprintf(&body, `<MaxKeys>%d</MaxKeys>`, maxKeys)
	fmt.Fprintf(&body, `<IsTruncated>%t</IsTruncated>`, truncated)
	if truncated {
		fmt.Fprintf(&body, `<NextContinuationToken>%s</NextContinuationToken>`, nextToken)
	}
	for _, key := range contents {
		obj := f.objects[key]
		body.WriteString(`<Contents>`)
		fmt.Fprintf(&body, `<Key>%s</Key>`, key)
		fmt.Fprintf(&body, `<LastModified>%s</LastModified>`, obj.modified.UTC().Format(time.RFC3339))
		fmt.Fprintf(&body, `<ETag>%s</ETag>`, obj.etag)
		fmt.Fprintf(&body, `<Size>%d</Size>`, len(obj.body))
		body.WriteString(`</Contents>`)
	}
	for _, key := range prefixes {
		fmt.Fprintf(&body, `<CommonPrefixes><Prefix>%s</Prefix></CommonPrefixes>`, key)
	}
	body.WriteString(`</ListBucketResult>`)

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body.String()))
}
