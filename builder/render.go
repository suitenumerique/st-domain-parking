package main

// Rendering of the parking pages onto disk, in the layout Caddy expects.
//
// For every site, two directories are produced under the output root:
//
//	www.brigny.fr/index.html      the page
//	www.brigny.fr/index.html.br   pre-compressed variants, served as-is
//	www.brigny.fr/index.html.gz
//	www.brigny.fr/.parked         allowlist marker for on-demand TLS
//	brigny.fr/.parked             apex: allowlisted so it can be redirected
//
// A rebuild that changes nothing touches no file, so mtimes stay stable and
// Caddy's file cache is not invalidated for no reason.

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"

	"github.com/andybalholm/brotli"
	"github.com/tdewolff/minify/v2"
	"github.com/tdewolff/minify/v2/html"
)

const (
	page = "index.html"
	// marker is what Caddy's on-demand TLS allowlist matches on, so it is
	// the single source of truth for whether a host is served at all.
	marker = ".parked"

	// brotliQuality trades a few bytes for build time. Quality 11 saves
	// about 140 bytes on a 1.6 kB response and is about 30 times slower,
	// which over tens of thousands of pages is minutes of CPU per build.
	brotliQuality = 5
)

//go:embed index.html.tmpl
var templateSource []byte

// pageTemplate is minified once, here, rather than every rendered page: the
// values it receives are short and gain nothing from minification.
var pageTemplate = template.Must(template.New(page).Parse(minifyTemplate(templateSource)))

// minifyTemplate strips the whitespace from the template source, leaving the
// template actions alone.
func minifyTemplate(source []byte) string {
	m := minify.New()
	m.Add("text/html", &html.Minifier{
		KeepDocumentTags: true,
		KeepEndTags:      true,
		TemplateDelims:   html.GoTemplateDelims,
	})
	m.AddFunc("text/css", stripCSS)

	minified, err := m.Bytes("text/html", source)
	if err != nil {
		panic(fmt.Sprintf("minifying the template: %v", err))
	}
	return string(minified)
}

var (
	cssSpaces      = regexp.MustCompile(`\s+`)
	cssPunctuation = regexp.MustCompile(` ?([{};:,>]) ?`)
)

// stripCSS removes the whitespace from the stylesheet and the last semicolon
// of each block, and nothing else. A real CSS minifier also rewrites values
// to their shortest modern spelling (`rgba(…, 5%)`, eight-digit hex, range
// media queries), which older browsers ignore.
//
// It is only meant for the stylesheet in the template, whose strings and
// selectors have no whitespace this would change.
func stripCSS(_ *minify.M, w io.Writer, r io.Reader, _ map[string]string) error {
	css, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	css = cssSpaces.ReplaceAll(bytes.TrimSpace(css), []byte(" "))
	css = cssPunctuation.ReplaceAll(css, []byte("$1"))
	css = bytes.ReplaceAll(css, []byte(";}"), []byte("}"))
	_, err = w.Write(css)
	return err
}

// build renders every site into outputDir and prunes anything stale.
//
// It refuses, before writing anything, a list that would unpark more than
// maxUnpark percent of the hosts parked now: a feed that suddenly drops most
// of its entries is far more likely broken than right, and acting on it
// means re-issuing every certificate once it is fixed.
//
// Rendering, compressing and writing a page are independent of every other
// page, so the work is spread over one goroutine per CPU; the pruning waits
// for all of them, because it is the one step that needs the whole list. The
// Go runtime sizes GOMAXPROCS from the container's CPU limit.
func build(sites []Site, outputDir string, maxUnpark int) error {
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return err
	}

	expected := make(map[string]bool, 2*len(sites))
	for _, site := range sites {
		for _, host := range site.Hosts() {
			expected[host] = true
		}
	}

	stale, parked, err := staleHosts(outputDir, expected)
	if err != nil {
		return err
	}
	if len(stale)*100 > parked*maxUnpark {
		return fmt.Errorf("the domain list would unpark %d of the %d parked hosts, more than MAX_UNPARK_PERCENT (%d%%)",
			len(stale), parked, maxUnpark)
	}

	jobs := make(chan Site)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		written  int
		firstErr error
	)
	for range runtime.GOMAXPROCS(0) {
		wg.Go(func() {
			for site := range jobs {
				mu.Lock()
				failed := firstErr != nil
				mu.Unlock()
				if failed {
					continue
				}

				changed, err := buildSite(site, outputDir)
				mu.Lock()
				if changed {
					written++
				}
				if err != nil && firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
		})
	}
	for _, site := range sites {
		jobs <- site
	}
	close(jobs)
	wg.Wait()

	// A failed build stops short of pruning, so it changes as little as
	// possible of the tree being served.
	if firstErr != nil {
		return firstErr
	}

	for _, name := range stale {
		if err := os.RemoveAll(filepath.Join(outputDir, name)); err != nil {
			return err
		}
		slog.Info("Pruned " + name)
	}

	slog.Info(fmt.Sprintf("Build complete: %d domains, %d written, %d pruned", len(sites), written, len(stale)))
	return nil
}

// buildSite renders and writes one site. True if the page was written.
func buildSite(site Site, outputDir string) (bool, error) {
	var rendered bytes.Buffer
	if err := pageTemplate.Execute(&rendered, site); err != nil {
		return false, fmt.Errorf("rendering %s: %w", site.Domain, err)
	}

	written, err := writePage(filepath.Join(outputDir, site.ServeHost()), rendered.Bytes())
	if err != nil {
		return false, err
	}
	// The apex only needs to exist so Caddy will negotiate a certificate for
	// it; the redirect itself is handled by the static config.
	return written, mark(filepath.Join(outputDir, site.Domain))
}

// writePage writes the page and its compressed variants. True if anything
// changed.
func writePage(dir string, content []byte) (bool, error) {
	if err := mark(dir); err != nil {
		return false, err
	}

	target := filepath.Join(dir, page)
	if current, err := os.ReadFile(target); err == nil && bytes.Equal(current, content) {
		return false, nil
	}

	var br bytes.Buffer
	writer := brotli.NewWriterLevel(&br, brotliQuality)
	if _, err := writer.Write(content); err != nil {
		return false, err
	}
	if err := writer.Close(); err != nil {
		return false, err
	}

	var gz bytes.Buffer
	gzipWriter, err := gzip.NewWriterLevel(&gz, gzip.BestCompression)
	if err != nil {
		return false, err
	}
	if _, err := gzipWriter.Write(content); err != nil {
		return false, err
	}
	if err := gzipWriter.Close(); err != nil {
		return false, err
	}

	// The page last: it is what the next build compares against, so an
	// interrupted write leaves a page that differs and is rewritten in full.
	for _, file := range []struct {
		name    string
		content []byte
	}{
		{page + ".br", br.Bytes()},
		{page + ".gz", gz.Bytes()},
		{page, content},
	} {
		if err := replace(filepath.Join(dir, file.name), file.content); err != nil {
			return false, err
		}
	}

	slog.Info(fmt.Sprintf("Built %s (%d bytes)", filepath.Base(dir), len(content)))
	return true, nil
}

// replace writes via a rename, so Caddy never serves a half-written file.
func replace(target string, content []byte) error {
	temporary := filepath.Join(filepath.Dir(target), ".tmp-"+filepath.Base(target))
	if err := os.WriteFile(temporary, content, 0o644); err != nil {
		return err
	}
	return os.Rename(temporary, target)
}

func mark(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(dir, marker), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	return file.Close()
}

// staleHosts lists the host directories that are no longer in the domain
// list, and counts the hosts parked now: the stale ones plus the expected
// ones already on disk.
//
// Only directories carrying the marker are stale, so an operator poking
// around in the output directory cannot have unrelated files deleted.
func staleHosts(outputDir string, expected map[string]bool) ([]string, int, error) {
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		return nil, 0, err
	}

	var stale []string
	parked := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if expected[entry.Name()] {
			parked++
			continue
		}
		if _, err := os.Stat(filepath.Join(outputDir, entry.Name(), marker)); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, 0, err
		}
		stale = append(stale, entry.Name())
		parked++
	}
	return stale, parked, nil
}
