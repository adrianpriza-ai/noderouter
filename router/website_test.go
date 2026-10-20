package router

// website.go serves the models site, its assets and the guides. Two contracts
// matter and are easy to break: the same URL hands a browser HTML and everyone
// else the raw file (so Accept has to drive the choice, and caches have to know
// it), and the embedded paths are cleaned before use, so a traversal attempt
// misses the filesystem instead of escaping it.
//
// The filesystems are package state installed by SetSiteFS, so these tests use
// an in-memory tree rather than the real website and restore whatever was there.

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"

	"noderouter/config"
	"noderouter/logx"
	"noderouter/proxy"
)

// testSite builds a stand-in website/docs tree and installs it for the test.
func testSite(t *testing.T) {
	t.Helper()

	site := fstest.MapFS{
		"index.html":            {Data: []byte(`<html><body id="home"></body></html>`)},
		"models.html":           {Data: []byte(`<html><body id="panel"></body></html>`)},
		"docs.html":             {Data: []byte(`<html><body id="content"></body></html>`)},
		"css/site.css":          {Data: []byte("body{}")},
		"css/fonts.css":         {Data: []byte("@font-face{}")},
		"js/models.js":          {Data: []byte("// models")},
		"js/marked.min.js":      {Data: []byte("// marked")},
		"fonts/logo.woff2":      {Data: []byte("woff2")},
		"favicon/favicon.ico":   {Data: []byte("ico")},
		"favicon/favicon-32x32": {Data: []byte("png")},
		"favicon/favicon.svg":   {Data: []byte("<svg/>")},
	}
	docs := fstest.MapFS{
		"configuration.md": {Data: []byte("# Configuration\n")},
		"proxies.md":       {Data: []byte("# Proxies\n")},
		"operations.md":    {Data: []byte("# Operations\n")},
	}

	restoreSiteFS(t, site, docs)
}

// restoreSiteFS installs the given filesystems and puts the previous ones back.
func restoreSiteFS(t *testing.T, site, docs fs.FS) {
	t.Helper()
	prevSite, prevDocs := SiteFS, DocsFS
	SetSiteFS(site, docs)
	t.Cleanup(func() { SetSiteFS(prevSite, prevDocs) })
}

// siteGet performs a request against the real mux, so routing is exercised
// rather than stubbed.
func siteGet(t *testing.T, path, accept string) *httptest.ResponseRecorder {
	t.Helper()
	srv := NewServer(websiteTestConfig(), proxy.NewPool("", "", "rotate", false), logx.New(false))
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// followRedirect re-requests a 30x target the way a browser or curl would, so
// a redirect is asserted on where it lands and not only that it was issued.
// The httptest recorder does not follow on its own.
func followRedirect(t *testing.T, accept string, rec *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	if rec.Code < 300 || rec.Code > 399 {
		return rec
	}
	loc := rec.Header().Get("Location")
	if loc == "" {
		t.Fatalf("status %d with no Location header", rec.Code)
	}
	path, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("Location %q: %v", loc, err)
	}
	// The client re-sends its own Accept on the way; a redirect response
	// carries none of its own.
	return siteGet(t, path.RequestURI(), accept)
}

// websiteTestConfig is a minimal valid config: the site routes need none of it,
// but NewServer reads the model table to report id clashes.
func websiteTestConfig() *config.Config {
	cfg := &config.Config{}
	cfg.ApplyDefaults()
	return cfg
}

// ------------------------------------------------------------- assets ---

// contentTypeFor pins the asset types so woff2 and friends never depend on the
// host's mime.types being populated.
func TestContentTypeFor(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"site.css", "text/css; charset=utf-8"},
		{"models.js", "text/javascript; charset=utf-8"},
		{"app.mjs", "text/javascript; charset=utf-8"},
		{"data.json", "application/json; charset=utf-8"},
		{"app.js.map", "application/json; charset=utf-8"},
		{"configuration.md", "text/markdown; charset=utf-8"},
		{"index.html", "text/html; charset=utf-8"},
		{"robots.txt", "text/plain; charset=utf-8"},
		{"site.webmanifest", "application/manifest+json; charset=utf-8"},
		{"f.woff2", "font/woff2"},
		{"f.woff", "font/woff"},
		{"f.otf", "font/otf"},
		{"f.ttf", "font/ttf"},
		{"og.png", "image/png"},
		{"p.jpg", "image/jpeg"},
		{"p.jpeg", "image/jpeg"},
		{"p.webp", "image/webp"},
		{"p.gif", "image/gif"},
		{"favicon.ico", "image/x-icon"},
		{"mark.svg", "image/svg+xml"},
		// The extension is matched case-insensitively.
		{"UPPER.CSS", "text/css; charset=utf-8"},
		{"Mixed.PnG", "image/png"},
		// Anything unknown, and anything with no extension at all.
		{"mystery.xyz", "application/octet-stream"},
		{"LICENSE", "application/octet-stream"},
		{"archive.tar.gz", "application/octet-stream"},
	} {
		if got := contentTypeFor(tc.name); got != tc.want {
			t.Errorf("contentTypeFor(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// An asset is served with its own content type and a cacheable response: the
// fonts and favicons are immutable, so re-fetching them per page view would be
// pure waste.
func TestServeSiteAsset(t *testing.T) {
	testSite(t)

	for _, tc := range []struct{ path, wantCT, wantBody string }{
		{"/css/site.css", "text/css; charset=utf-8", "body{}"},
		{"/css/fonts.css", "text/css; charset=utf-8", "@font-face{}"},
		{"/js/models.js", "text/javascript; charset=utf-8", "// models"},
		{"/js/marked.min.js", "text/javascript; charset=utf-8", "// marked"},
		{"/fonts/logo.woff2", "font/woff2", "woff2"},
	} {
		rec := siteGet(t, tc.path, "")
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", tc.path, rec.Code)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); ct != tc.wantCT {
			t.Errorf("GET %s Content-Type = %q, want %q", tc.path, ct, tc.wantCT)
		}
		if got := rec.Body.String(); got != tc.wantBody {
			t.Errorf("GET %s body = %q, want %q", tc.path, got, tc.wantBody)
		}
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age") {
			t.Errorf("GET %s Cache-Control = %q, want it cacheable", tc.path, cc)
		}
	}
}

// A path that is not in the embed is a 404, not a 500 and not an empty 200.
func TestServeSiteAssetNotFound(t *testing.T) {
	testSite(t)
	for _, path := range []string{
		"/css/missing.css",
		"/js/missing.js",
		"/img/missing.png",
		"/fonts/missing.woff2",
		"/favicon/missing.ico",
	} {
		rec := siteGet(t, path, "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
}

// A traversal attempt must miss the embed. The path is cleaned before the
// lookup, so ../ escapes the site directory rather than the process.
func TestServeSiteAssetRejectsTraversal(t *testing.T) {
	testSite(t)
	for _, path := range []string{
		"/css/../../etc/passwd",
		"/js/../../../etc/hosts",
		"/../../etc/passwd",
	} {
		rec := siteGet(t, path, "")
		if rec.Code == http.StatusOK {
			t.Errorf("GET %s = 200 and served %q; a traversal must not succeed",
				path, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "root:") {
			t.Errorf("GET %s leaked file contents", path)
		}
	}
}

// --------------------------------------------------------------- docs ---

// One URL, two representations: a browser gets the docs website, everything
// else gets the raw markdown. This is why the handler looks at Accept at all.
func TestServeDocsServesRawToNonBrowsers(t *testing.T) {
	testSite(t)

	for _, accept := range []string{"", "*/*", "application/json", "text/plain"} {
		rec := siteGet(t, "/docs/configuration.md", accept)
		if rec.Code != http.StatusOK {
			t.Errorf("GET with Accept %q = %d, want 200", accept, rec.Code)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
			t.Errorf("GET with Accept %q Content-Type = %q, want markdown", accept, ct)
		}
		if got := rec.Body.String(); got != "# Configuration\n" {
			t.Errorf("GET with Accept %q body = %q, want the raw guide", accept, got)
		}
	}
}

// A browser navigating to a guide gets the docs shell, which renders the guide
// client-side. Without the Vary the cached markdown would be replayed to the
// navigation and the browser would show raw text.
func TestServeDocsServesShellToBrowsers(t *testing.T) {
	testSite(t)

	for _, accept := range []string{
		"text/html",
		"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		"text/html;q=0.9",
	} {
		rec := siteGet(t, "/docs/configuration.md", accept)
		if rec.Code != http.StatusOK {
			t.Errorf("GET with Accept %q = %d, want 200", accept, rec.Code)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("GET with Accept %q Content-Type = %q, want html", accept, ct)
		}
		if got := rec.Body.String(); !strings.Contains(got, `id="content"`) {
			t.Errorf("GET with Accept %q body = %q, want the docs shell", accept, got)
		}
		if v := rec.Header().Get("Vary"); v != "Accept" {
			t.Errorf("GET with Accept %q Vary = %q, want Accept", accept, v)
		}
		// The shell is a thin wrapper over the guide, so it must never be
		// served stale after a restart.
		if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("GET with Accept %q Cache-Control = %q, want no-cache", accept, cc)
		}
	}
}

// Both representations carry Vary, or a cache would mix them up.
func TestServeDocsVariesOnAcceptForBoth(t *testing.T) {
	testSite(t)
	for _, accept := range []string{"", "text/html"} {
		rec := siteGet(t, "/docs/proxies.md", accept)
		if v := rec.Header().Get("Vary"); v != "Accept" {
			t.Errorf("GET with Accept %q Vary = %q, want Accept", accept, v)
		}
	}
}

// Every guide in the tree has to be reachable, and a name that is not there is
// a 404.
func TestServeDocsCoversTheTree(t *testing.T) {
	testSite(t)

	for name, want := range map[string]string{
		"configuration.md": "# Configuration\n",
		"proxies.md":       "# Proxies\n",
		"operations.md":    "# Operations\n",
	} {
		rec := siteGet(t, "/docs/"+name, "")
		if rec.Code != http.StatusOK {
			t.Errorf("GET /docs/%s = %d, want 200", name, rec.Code)
			continue
		}
		if got := rec.Body.String(); got != want {
			t.Errorf("GET /docs/%s body = %q, want %q", name, got, want)
		}
	}

	for _, path := range []string{"/docs/nosuch.md", "/docs/missing/configuration.md"} {
		if rec := siteGet(t, path, ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
}

// The guide path is cleaned too, so a traversal misses the embed.
func TestServeDocsRejectsTraversal(t *testing.T) {
	testSite(t)
	for _, path := range []string{
		"/docs/../../etc/passwd",
		"/docs/../configuration.md",
		"/docs/../../../../etc/hosts",
	} {
		rec := siteGet(t, path, "")
		if strings.Contains(rec.Body.String(), "root:") {
			t.Errorf("GET %s leaked file contents", path)
		}
	}
}

// A bare /docs or /docs/ has no guide of its own to serve, and the embed has
// no index page, so it redirects to the front guide instead of 404ing a URL
// someone typed by hand. Both spellings must land, and both must survive the
// ?format= switch, or "curl /docs" gets an empty answer.
func TestServeDocsBarePathRedirectsToTheIndex(t *testing.T) {
	testSite(t)

	for _, path := range []string{"/docs", "/docs/"} {
		rec := siteGet(t, path, "")
		// /docs is redirected to /docs/ by the mux before the handler ever
		// sees it, so either hop is fine; what matters is where it ends up.
		loc := rec.Header().Get("Location")
		if rec.Code != http.StatusMovedPermanently || loc != "/docs/"+docsIndex {
			t.Errorf("GET %s = %d Location %q, want 301 to /docs/%s",
				path, rec.Code, loc, docsIndex)
		}
	}

	// Following the redirect must actually produce the guide, for a browser
	// (the shell) and for curl (the raw markdown).
	for _, accept := range []string{"", "text/html"} {
		rec := followRedirect(t, accept, siteGet(t, "/docs/", accept))
		if rec.Code != http.StatusOK {
			t.Fatalf("following /docs/ with Accept %q = %d, want 200", accept, rec.Code)
		}
		if accept == "text/html" {
			if !strings.Contains(rec.Body.String(), `id="content"`) {
				t.Errorf("browser body = %q, want the docs shell", rec.Body.String())
			}
			continue
		}
		if got := rec.Body.String(); got != "# Configuration\n" {
			t.Errorf("body = %q, want the raw index guide", got)
		}
	}
}

// The redirect has to carry the query through, or /docs?format=json (what a
// client sends to ask for markdown) lands on the page instead of the guide.
func TestServeDocsBareRedirectKeepsTheQuery(t *testing.T) {
	testSite(t)

	rec := siteGet(t, "/docs/?format=json", "")
	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("GET /docs/?format=json = %d, want 301", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/docs/"+docsIndex+"?format=json" {
		t.Errorf("Location = %q, want the query preserved", got)
	}
	rec = followRedirect(t, "", siteGet(t, "/docs/?format=json", ""))
	if got := rec.Body.String(); got != "# Configuration\n" {
		t.Errorf("body = %q, want the raw guide after following", got)
	}
}

// A name that cleans back out of the docs directory is a 404, not a redirect:
// escaping the guide folder must never be answered with a guide.
func TestServeDocsBarePathDoesNotMaskTraversal(t *testing.T) {
	testSite(t)
	for _, path := range []string{
		"/docs/../nosuch.md",
		"/docs/subdir/../../etc/passwd",
	} {
		rec := siteGet(t, path, "")
		if rec.Code == http.StatusMovedPermanently {
			t.Errorf("GET %s redirected to the index instead of being refused", path)
		}
		if strings.Contains(rec.Body.String(), "root:") {
			t.Errorf("GET %s leaked file contents", path)
		}
	}
}

// The docs shell is a separate file from the guide, so a missing one is
// reported as such rather than serving an empty page.
func TestServeDocsPageWithoutTheShell(t *testing.T) {
	restoreSiteFS(t, fstest.MapFS{"models.html": {Data: []byte("<html/>")}},
		fstest.MapFS{"configuration.md": {Data: []byte("# Configuration\n")}})

	rec := siteGet(t, "/docs/configuration.md", "text/html")
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /docs/configuration.md = %d without docs.html, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "docs page not found") {
		t.Errorf("body = %q, want it to say the docs page is missing", rec.Body.String())
	}
}

// The docs shell carries no guide of its own: the page reads its own URL and
// fetches the markdown back. So the shell must not embed one.
func TestServeDocsPageCarriesNoGuide(t *testing.T) {
	testSite(t)
	rec := siteGet(t, "/docs/configuration.md", "text/html")
	body := rec.Body.String()
	if strings.Contains(body, "# Configuration") {
		t.Error("the docs shell embedded the guide; it must fetch it client-side")
	}
	if !strings.Contains(body, `id="content"`) {
		t.Errorf("body = %q, want the empty container the page fills", body)
	}
}

// ------------------------------------------------------- site root file ---

// The one file left at the site root is the browser's default favicon request,
// which maps onto the favicon folder.
func TestServeSiteRootFileMapsFavicon(t *testing.T) {
	testSite(t)

	rec := siteGet(t, "/favicon.ico", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /favicon.ico = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/x-icon" {
		t.Errorf("Content-Type = %q, want image/x-icon", ct)
	}
	if got := rec.Body.String(); got != "ico" {
		t.Errorf("body = %q, want the contents of favicon/favicon.ico", got)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age") {
		t.Errorf("Cache-Control = %q, want it cacheable", cc)
	}
}

// Without this route a browser's /favicon.ico would fall through to the proxy
// and be forwarded upstream.
func TestServeSiteRootFileNotFound(t *testing.T) {
	restoreSiteFS(t, fstest.MapFS{"models.html": {Data: []byte("<html/>")}},
		fstest.MapFS{"configuration.md": {Data: []byte("# Configuration\n")}})

	rec := siteGet(t, "/favicon.ico", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /favicon.ico = %d without the favicon, want 404", rec.Code)
	}
}

// --------------------------------------------------------- wantsPage ---

// wantsPage decides HTML vs JSON. An explicit format wins, because that is the
// client stating its intent; otherwise Accept decides.
func TestWantsPage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		url    string
		accept string
		want   bool
	}{
		{"browser gets the page", "/models", "text/html,application/xhtml+xml", true},
		{"sdk gets json", "/v1/models", "application/json", false},
		{"curl gets json", "/v1/models", "*/*", false},
		{"no accept header gets json", "/v1/models", "", false},
		{"json accept does not ask for html", "/models", "application/json", false},

		// ?format= wins over Accept, in both directions.
		{"format=html beats json accept", "/v1/models?format=html", "application/json", true},
		{"format=html beats no accept", "/v1/models?format=html", "", true},
		{"format=html beats wildcard", "/v1/models?format=html", "*/*", true},
		{"format=json beats html accept", "/models?format=json", "text/html", false},
		{"format=json beats browser accept", "/v1/models?format=json", "text/html,application/xhtml+xml", false},
		{"format is case-insensitive in value only", "/models?format=HTML", "application/json", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.url, nil)
			if tc.accept != "" {
				req.Header.Set("Accept", tc.accept)
			}
			if got := wantsPage(req); got != tc.want {
				t.Errorf("wantsPage(%q, Accept %q) = %v, want %v", tc.url, tc.accept, got, tc.want)
			}
		})
	}
}

// --------------------------------------------------------- page split ---

// GET / answers both audiences from one URL: a browser gets the landing page,
// and everything else keeps the JSON index it has always had. Both carry Vary,
// or a cache would hand one to the other.
func TestHomePageForBrowsers(t *testing.T) {
	testSite(t)

	rec := siteGet(t, "/", "text/html")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / as a browser = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want html", ct)
	}
	if !strings.Contains(rec.Body.String(), `id="home"`) {
		t.Errorf("body = %q, want the landing page shell", rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", cc)
	}
	if v := rec.Header().Get("Vary"); v != "Accept" {
		t.Errorf("Vary = %q, want Accept", v)
	}
}

// The JSON index is what curl, the operator's health checks and any script that
// reads the root still get, so it must survive the landing page's arrival.
func TestHomeJSONForNonBrowsers(t *testing.T) {
	testSite(t)

	for _, accept := range []string{"", "*/*", "application/json"} {
		rec := siteGet(t, "/", accept)
		if rec.Code != http.StatusOK {
			t.Errorf("GET / with Accept %q = %d, want 200", accept, rec.Code)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("GET / with Accept %q Content-Type = %q, want json", accept, ct)
		}
		if !strings.Contains(rec.Body.String(), `"routes"`) {
			t.Errorf("GET / with Accept %q body = %q, want the JSON index", accept, rec.Body.String())
		}
	}

	// ?format= wins over Accept, the same as it does for the models list.
	rec := siteGet(t, "/?format=json", "text/html")
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("GET /?format=json Content-Type = %q, want json", ct)
	}
}

// A missing landing page is reported rather than served empty.
func TestHomePageWithoutTheShell(t *testing.T) {
	restoreSiteFS(t, fstest.MapFS{"models.html": {Data: []byte("<html/>")}}, fstest.MapFS{})

	rec := siteGet(t, "/", "text/html")
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET / = %d without index.html, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "home page not found") {
		t.Errorf("body = %q, want it to say the landing page is missing", rec.Body.String())
	}
}

// /models and /v1/models are the same list, split by Accept. Both spellings
// have to work, since clients pick either.
func TestModelsPageSplitBothSpellings(t *testing.T) {
	testSite(t)

	for _, path := range []string{"/models", "/v1/models"} {
		rec := siteGet(t, path, "text/html")
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("GET %s as a browser Content-Type = %q, want html", path, ct)
		}
		if !strings.Contains(rec.Body.String(), `id="panel"`) {
			t.Errorf("GET %s as a browser did not return the shell", path)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("GET %s as a browser Cache-Control = %q, want no-cache", path, cc)
		}
	}
}

// Both representations and both URL spellings share one cache key: otherwise
// a cached model list could be replayed as the website, or the other way round.
func TestServeModelsVariesOnAcceptForBoth(t *testing.T) {
	testSite(t)

	for _, path := range []string{"/models", "/v1/models"} {
		for _, accept := range []string{"", "text/html"} {
			rec := siteGet(t, path, accept)
			if v := rec.Header().Get("Vary"); v != "Accept" {
				t.Errorf("GET %s with Accept %q Vary = %q, want Accept", path, accept, v)
			}
		}
	}
}

// The page's own fetch gets JSON even though it is the same page's script.
func TestModelsJSONForTheFetch(t *testing.T) {
	testSite(t)
	for _, accept := range []string{"application/json", "*/*", ""} {
		rec := siteGet(t, "/v1/models", accept)
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("GET /v1/models with Accept %q Content-Type = %q, want json", accept, ct)
		}
	}
}

// The models page is a thin shell over live data, so it must never be cached.
func TestModelsPageIsNotCached(t *testing.T) {
	testSite(t)
	rec := siteGet(t, "/models", "text/html")
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache so a stale catalog is never served", cc)
	}
}

// A missing shell is reported rather than serving an empty page.
func TestModelsPageWithoutTheShell(t *testing.T) {
	restoreSiteFS(t, fstest.MapFS{"docs.html": {Data: []byte("<html/>")}},
		fstest.MapFS{"configuration.md": {Data: []byte("# Configuration\n")}})

	rec := siteGet(t, "/models", "text/html")
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /models = %d without models.html, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "models page not found") {
		t.Errorf("body = %q, want it to say the page is missing", rec.Body.String())
	}
}

// --------------------------------------------------------------- wiring ---

// SetSiteFS is package state, so it has to replace both halves together: a
// site without docs (or the reverse) would 404 half the site.
func TestSetSiteFSInstallsBothHalves(t *testing.T) {
	site := fstest.MapFS{"models.html": {Data: []byte("<html/>")}}
	docs := fstest.MapFS{"configuration.md": {Data: []byte("# Configuration\n")}}

	restoreSiteFS(t, site, docs)

	if SiteFS == nil || DocsFS == nil {
		t.Fatal("SetSiteFS left one of the two filesystems unset")
	}
	if _, err := fs.Stat(SiteFS, "models.html"); err != nil {
		t.Errorf("the site tree is not readable: %v", err)
	}
	if _, err := fs.Stat(DocsFS, "configuration.md"); err != nil {
		t.Errorf("the docs tree is not readable: %v", err)
	}
}

// With no filesystem installed at all the site routes must not panic: a 404 is
// the right answer, since the binary always installs one but a test or a future
// caller might not.
func TestSiteRoutesWithoutAnInstalledFS(t *testing.T) {
	restoreSiteFS(t, nil, nil)

	for _, path := range []string{"/", "/models", "/css/site.css", "/docs/configuration.md", "/favicon.ico"} {
		rec := siteGet(t, path, "text/html")
		if rec.Code == http.StatusOK {
			t.Errorf("GET %s = 200 with no filesystem installed", path)
		}
	}
}
