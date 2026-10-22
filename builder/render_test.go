package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	stdhtml "html"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/tdewolff/parse/v2"
	"github.com/tdewolff/parse/v2/html"
)

var brigny = Site{
	Domain:           "brigny.fr",
	CommuneName:      "Brigny",
	CommuneZipcode:   "87200",
	Email:            "contact@brigny.fr",
	ServicePublicURL: "https://lannuaire.service-public.fr/mairie-87030-01",
	Siret:            "21510372200017",
}

func mustBuild(t *testing.T, sites []Site, dir string) {
	t.Helper()
	if err := build(sites, dir); err != nil {
		t.Fatalf("build: %v", err)
	}
}

func pageOf(t *testing.T, dir, host string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(dir, host, page))
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// built renders brigny into a fresh directory and returns its page.
func built(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustBuild(t, []Site{brigny}, dir)
	return pageOf(t, dir, "www.brigny.fr")
}

func stylesheet(t *testing.T, content string) string {
	t.Helper()
	start, end := strings.Index(content, "<style>"), strings.Index(content, "</style>")
	if start < 0 || end < start {
		t.Fatal("no stylesheet in the page")
	}
	return content[start:end]
}

func TestLayoutMatchesWhatCaddyLooksFor(t *testing.T) {
	dir := t.TempDir()
	mustBuild(t, []Site{brigny}, dir)

	// The apex is allowlisted so a certificate can be issued for the
	// redirect, but it holds no page.
	if !exists(filepath.Join(dir, "brigny.fr", marker)) || exists(filepath.Join(dir, "brigny.fr", page)) {
		t.Error("the apex should hold the marker and nothing else")
	}
	for _, name := range []string{marker, page, page + ".br", page + ".gz"} {
		if !exists(filepath.Join(dir, "www.brigny.fr", name)) {
			t.Errorf("www.brigny.fr/%s is missing", name)
		}
	}
}

func TestThePageIsSelfContained(t *testing.T) {
	content := built(t)

	for _, forbidden := range []string{"<script", "fonts.googleapis.com", "rel=stylesheet", ".css", ".js"} {
		if strings.Contains(content, forbidden) {
			t.Errorf("the page contains %q", forbidden)
		}
	}
}

func TestThePageContent(t *testing.T) {
	content := built(t)

	for _, want := range []string{
		"<h1>Ce domaine appartient à la mairie de Brigny (87200).</h1>",
		`href="mailto:contact@brigny.fr"`,
		`href="https://lannuaire.service-public.fr/mairie-87030-01"`,
		`href="https://suiteterritoriale.anct.gouv.fr/bienvenue/21510372200017"`,
		`<link rel=canonical href="https://www.brigny.fr/">`,
		// The footer sends the municipal team somewhere it can edit the
		// entry, a different destination from the per-SIRET row above.
		`href=https://collectivite.fr/>Espace Commune</a>`,
	} {
		if !strings.Contains(content, want) {
			t.Errorf("the page lacks %s", want)
		}
	}
}

// Reached by typing the domain, not from search: the authoritative record is
// the Service-Public listing, so the page stays out of the index. Crawling is
// still allowed (in the Caddyfile) or the noindex is never read.
func TestThePageIsNotIndexable(t *testing.T) {
	if content := built(t); !strings.Contains(content, `<meta name=robots content="noindex">`) {
		t.Error("the page is indexable")
	}
}

func TestTheDescriptionHasNoStrayWhitespace(t *testing.T) {
	want := `<meta name=description content="Ce domaine appartient à la mairie de Brigny (87200).">`
	if content := built(t); !strings.Contains(content, want) {
		t.Errorf("the page lacks %s", want)
	}
}

func TestTheTemplateLeavesNoStrayWhitespace(t *testing.T) {
	content := built(t)
	if !strings.HasPrefix(content, "<!doctype html><html lang=fr><head>") {
		t.Errorf("the page starts with %q", content[:min(60, len(content))])
	}
	if strings.Contains(content, ">\n<meta") || strings.Contains(content, ">\n<link") {
		t.Error("a template comment left a newline between head elements")
	}
}

func TestCompressedVariantsDecodeToThePage(t *testing.T) {
	dir := t.TempDir()
	mustBuild(t, []Site{brigny}, dir)
	served := filepath.Join(dir, "www.brigny.fr")

	content, _ := os.ReadFile(filepath.Join(served, page))
	br, _ := os.ReadFile(filepath.Join(served, page+".br"))
	gz, _ := os.ReadFile(filepath.Join(served, page+".gz"))

	if decoded, err := io.ReadAll(brotli.NewReader(bytes.NewReader(br))); err != nil || !bytes.Equal(decoded, content) {
		t.Errorf("the brotli variant does not decode to the page: %v", err)
	}
	gzipReader, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatal(err)
	}
	if decoded, err := io.ReadAll(gzipReader); err != nil || !bytes.Equal(decoded, content) {
		t.Errorf("the gzip variant does not decode to the page: %v", err)
	}
	// Compression has to be worth its while.
	if len(br) >= len(content)/2 {
		t.Errorf("brotli: %d bytes for a %d-byte page", len(br), len(content))
	}
}

// Values reach both element text and attribute values, so the page is lexed
// rather than grepped.
func TestDomainListValuesCannotInjectMarkup(t *testing.T) {
	for _, payload := range []string{
		"<script>alert(1)</script>",
		`"><script>alert(1)</script>`,
		`"' onload=alert(1) x="`,
		"</title><script>alert(1)</script>",
	} {
		t.Run(payload, func(t *testing.T) {
			site := brigny
			site.CommuneName = payload
			dir := t.TempDir()
			mustBuild(t, []Site{site}, dir)

			var text strings.Builder
			lexer := html.NewLexer(parse.NewInputString(pageOf(t, dir, "www.brigny.fr")))
			for {
				token, data := lexer.Next()
				switch token {
				case html.ErrorToken:
					if lexer.Err() != io.EOF {
						t.Fatal(lexer.Err())
					}
					if !strings.Contains(text.String(), payload) {
						t.Error("the payload is not in the page text")
					}
					return
				case html.StartTagToken:
					if strings.EqualFold(string(lexer.Text()), "script") {
						t.Error("the payload opened a script tag")
					}
				case html.AttributeToken:
					if strings.HasPrefix(strings.ToLower(string(lexer.Text())), "on") {
						t.Errorf("the payload added an attribute: %s", data)
					}
				case html.TextToken:
					text.WriteString(stdhtml.UnescapeString(string(data)))
				}
			}
		})
	}
}

func TestAnUnchangedRebuildWritesNothing(t *testing.T) {
	dir := t.TempDir()
	mustBuild(t, []Site{brigny}, dir)
	served := filepath.Join(dir, "www.brigny.fr")

	past := time.Now().Add(-time.Hour)
	for _, name := range []string{page, page + ".br", page + ".gz", marker} {
		if err := os.Chtimes(filepath.Join(served, name), past, past); err != nil {
			t.Fatal(err)
		}
	}

	mustBuild(t, []Site{brigny}, dir)

	for _, name := range []string{page, page + ".br", page + ".gz", marker} {
		info, err := os.Stat(filepath.Join(served, name))
		if err != nil {
			t.Fatal(err)
		}
		if !info.ModTime().Equal(past) {
			t.Errorf("%s was rewritten", name)
		}
	}
}

func TestAChangedSiteIsRewritten(t *testing.T) {
	dir := t.TempDir()
	mustBuild(t, []Site{brigny}, dir)

	renamed := brigny
	renamed.CommuneName = "Brigny-le-Neuf"
	mustBuild(t, []Site{renamed}, dir)

	if !strings.Contains(pageOf(t, dir, "www.brigny.fr"), "Brigny-le-Neuf") {
		t.Error("the page was not rewritten")
	}
	br, _ := os.ReadFile(filepath.Join(dir, "www.brigny.fr", page+".br"))
	decoded, _ := io.ReadAll(brotli.NewReader(bytes.NewReader(br)))
	if !bytes.Contains(decoded, []byte("Brigny-le-Neuf")) {
		t.Error("the brotli variant was not rewritten")
	}
}

func TestRemovedDomainsArePruned(t *testing.T) {
	dir := t.TempDir()
	gone := brigny
	gone.Domain = "gone.fr"
	mustBuild(t, []Site{brigny, gone}, dir)
	if !exists(filepath.Join(dir, "www.gone.fr")) {
		t.Fatal("www.gone.fr was not built")
	}

	mustBuild(t, []Site{brigny}, dir)

	if exists(filepath.Join(dir, "www.gone.fr")) || exists(filepath.Join(dir, "gone.fr")) {
		t.Error("gone.fr was not pruned")
	}
	if !exists(filepath.Join(dir, "www.brigny.fr", page)) {
		t.Error("www.brigny.fr was pruned")
	}
}

func TestUnmanagedContentIsLeftAlone(t *testing.T) {
	dir := t.TempDir()
	stray := filepath.Join(dir, "not-ours", "keep.txt")
	if err := os.MkdirAll(filepath.Dir(stray), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stray, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A file at the top level, which has no marker to look into.
	loose := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(loose, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	mustBuild(t, []Site{brigny}, dir)

	if !exists(stray) || !exists(loose) {
		t.Error("unmanaged content was deleted")
	}
}

func TestAFailedBuildPrunesNothing(t *testing.T) {
	dir := t.TempDir()
	gone := brigny
	gone.Domain = "gone.fr"
	mustBuild(t, []Site{brigny, gone}, dir)

	// A file where a host directory has to go makes that site fail.
	blocked := brigny
	blocked.Domain = "blocked.fr"
	if err := os.WriteFile(filepath.Join(dir, "www.blocked.fr"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := build([]Site{brigny, blocked}, dir); err == nil {
		t.Fatal("build succeeded")
	}
	if !exists(filepath.Join(dir, "www.gone.fr", page)) {
		t.Error("a failed build pruned gone.fr")
	}
}

// The sites are built concurrently; every one of them has to end up on disk
// with its own content.
func TestManySitesAreAllBuilt(t *testing.T) {
	dir := t.TempDir()
	var sites []Site
	for index := range 200 {
		site := brigny
		site.Domain = fmt.Sprintf("commune-%d.fr", index)
		sites = append(sites, site)
	}

	mustBuild(t, sites, dir)

	for _, site := range sites {
		if !strings.Contains(pageOf(t, dir, site.ServeHost()), "<title>"+site.Domain+"</title>") {
			t.Errorf("%s does not carry its own page", site.ServeHost())
		}
		if !exists(filepath.Join(dir, site.Domain, marker)) {
			t.Errorf("%s has no marker", site.Domain)
		}
	}
}

// Minifying the stylesheet with a CSS minifier normalises `(max-width: 560px)`
// into Level 4 range syntax, which browsers older than ~2023 drop along with
// the whole rule, taking the mobile layout with it.
func TestMediaQueriesStayInLevel3Syntax(t *testing.T) {
	style := stylesheet(t, built(t))

	if !strings.Contains(style, "@media (max-width:560px)") {
		t.Error("the media query is not in Level 3 syntax")
	}
	if strings.Contains(style, "width<=") || strings.Contains(style, "width>=") {
		t.Error("the media query uses range syntax")
	}
	media := style[strings.Index(style, "@media"):]
	if !strings.Contains(media, ".card-body") || !strings.Contains(media, "flex-direction:column") {
		t.Error("the media query lost its rules")
	}
}

func TestTheStylesheetIsStripped(t *testing.T) {
	style := stylesheet(t, built(t))

	for _, left := range []string{": ", ";}", "\n", "  "} {
		if strings.Contains(style, left) {
			t.Errorf("the stylesheet still contains %q", left)
		}
	}
}

// Eight-digit hex (#rrggbbaa) and percentage alpha are what a minifier
// reaches for when it is not told what the page has to run on.
func TestTheStylesheetAvoidsSyntaxOlderBrowsersWouldDrop(t *testing.T) {
	style := stylesheet(t, built(t))

	if regexp.MustCompile(`#[0-9a-fA-F]{8}\b`).MatchString(style) {
		t.Error("eight-digit hex reached the page")
	}
	if !strings.Contains(style, "rgba(30,40,50,0.05)") {
		t.Error("the rgba() colour was rewritten")
	}
}
