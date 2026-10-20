package router

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// SiteFS holds the website directory (models.html, docs.html, css/, js/,
// img/, fonts/, favicon/) and DocsFS holds the guides directory. Both are set
// once at startup by SetSiteFS: the binary passes the embedded copies from the
// root package, and tests pass a directory FS. They are package state because
// they are build-time assets, not per-server data.
var (
	SiteFS     fs.FS
	DocsFS     fs.FS
	ReadmeData []byte
)

// SetSiteFS installs the embedded site, docs filesystems and README content.
func SetSiteFS(site, docs fs.FS, readme ...[]byte) {
	SiteFS, DocsFS = site, docs
	if len(readme) > 0 {
		ReadmeData = readme[0]
	}
}

// siteAssetTypes pins the content types of the embedded assets so woff2
// and friends never depend on the host's mime.types being populated.
var siteAssetTypes = map[string]string{
	".css":         "text/css; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".mjs":         "text/javascript; charset=utf-8",
	".json":        "application/json; charset=utf-8",
	".map":         "application/json; charset=utf-8",
	".md":          "text/markdown; charset=utf-8",
	".html":        "text/html; charset=utf-8",
	".txt":         "text/plain; charset=utf-8",
	".webmanifest": "application/manifest+json; charset=utf-8",
	".woff2":       "font/woff2",
	".woff":        "font/woff",
	".otf":         "font/otf",
	".ttf":         "font/ttf",
	".png":         "image/png",
	".jpg":         "image/jpeg",
	".jpeg":        "image/jpeg",
	".webp":        "image/webp",
	".gif":         "image/gif",
	".ico":         "image/x-icon",
	".svg":         "image/svg+xml",
}

func contentTypeFor(name string) string {
	if t, ok := siteAssetTypes[strings.ToLower(path.Ext(name))]; ok {
		return t
	}
	return "application/octet-stream"
}

// serveSiteAsset serves one file from the website directory (fonts,
// favicons) straight from the embed. The path is cleaned before it is
// used, so a traversal attempt just misses the embed and 404s.
func (s *Server) serveSiteAsset(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	data, err := fs.ReadFile(SiteFS, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentTypeFor(name))
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(data)
}

// docsIndex is the guide a bare /docs lands on. The route is a
// /docs/{file...} wildcard, so "/docs" and "/docs/" both arrive here with an
// empty name; there is no index page in the embed, so without this they would
// try to read DocsFS[""] and 404 a URL a person typed by hand.
const docsIndex = "configuration.md"

// serveDocs answers one guide from the docs directory (the
// configuration/proxies/operations pages the models site links to).
// A browser that navigated here gets the docs website, which fetches
// the guide back as markdown and renders it (see wantsPage); curl and
// other clients get the raw guide. The path is cleaned before it is
// used, so a traversal attempt just misses the embed and 404s.
func (s *Server) serveDocs(w http.ResponseWriter, r *http.Request) {
	// The guide name is whatever follows the /docs prefix, with the surrounding
	// slashes trimmed. Both spellings of the bare path ("/docs" and "/docs/")
	// leave it empty, and the embed has no index page, so that case redirects
	// instead of reading DocsFS[""] and 404ing a URL someone typed by hand.
	rel := strings.TrimPrefix(r.URL.Path, "/docs")
	name := strings.Trim(path.Clean("/"+rel), "/")
	if name == "" {
		// Permanent: the guide is always there, so caches may remember it.
		target := "/docs/" + docsIndex
		if q := r.URL.RawQuery; q != "" {
			target += "?" + q
		}
		http.Redirect(w, r, target, http.StatusMovedPermanently)
		return
	}
	// README.md is a special case: it lives at the project root, not in DocsFS.
	if name == "README.md" {
		if wantsPage(r) {
			s.serveDocsPage(w, r)
			return
		}
		if len(ReadmeData) == 0 {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Vary", "Accept")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = w.Write(ReadmeData)
		return
	}
	// path.Clean was applied to an absolute path, so name can never point out
	// of the docs directory (".." is resolved away before the embed is
	// touched); a traversal simply misses the lookup below and 404s.
	data, err := fs.ReadFile(DocsFS, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if wantsPage(r) {
		s.serveDocsPage(w, r)
		return
	}
	w.Header().Set("Content-Type", contentTypeFor(name))
	// The same URL also serves the website shell to browsers, so
	// caches must key on Accept: without it, this cached markdown
	// would be replayed to a browser navigation (raw text until
	// the page is reloaded).
	w.Header().Set("Vary", "Accept")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(data)
}

// serveDocsPage writes the docs website shell. It carries no guide of
// its own: the page's JavaScript reads the guide it was served for,
// as raw markdown, and renders it in the browser.
func (s *Server) serveDocsPage(w http.ResponseWriter, r *http.Request) {
	data, err := fs.ReadFile(SiteFS, "docs.html")
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "docs page not found")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Same URL, two representations: tell caches the answer depends
	// on the Accept header (see serveDocs).
	w.Header().Set("Vary", "Accept")
	// The page is a thin shell over the guide it renders; never let a
	// stale copy of the shell outlive a router restart.
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

// serveSiteRootFile serves a file that lives at the root of the website
// directory. The only one left there is the browser-default /favicon.ico,
// which is read from the favicon folder; every other asset now lives under
// /css, /js, /img, /fonts or /favicon and is served by serveSiteAsset.
func (s *Server) serveSiteRootFile(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "favicon.ico" {
		name = "favicon/favicon.ico"
	}
	data, err := fs.ReadFile(SiteFS, ""+name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentTypeFor(name))
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(data)
}

// serveModelsPage writes the /models website. It fetches nothing itself:
// the page's JavaScript reads /v1/models in the browser.
func (s *Server) serveModelsPage(w http.ResponseWriter, r *http.Request) {
	data, err := fs.ReadFile(SiteFS, "models.html")
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "models page not found")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The page is a thin shell over live data; never let a stale copy
	// of the shell outlive a router restart.
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

// serveModels answers the model list as JSON, or the models website
// when a browser navigated to the URL.
func (s *Server) serveModels(w http.ResponseWriter, r *http.Request) {
	// The same URL serves the model list and the website shell, so shared
	// caches must key the response on the client's requested representation.
	w.Header().Set("Vary", "Accept")
	if wantsPage(r) {
		s.serveModelsPage(w, r)
		return
	}
	s.handleModels(w, r)
}

// serveHome answers GET / for both audiences, the same split /v1/models uses:
// a browser navigating to the root gets the landing page, everything else keeps
// the JSON index it has always had. The JSON index is what curl and the
// operator's own health checks read; the page is the front door for a person.
func (s *Server) serveHome(w http.ResponseWriter, r *http.Request) {
	// Same URL, two representations: shared caches have to key on Accept, or a
	// client that asked for the JSON index could be handed the page shell.
	w.Header().Set("Vary", "Accept")
	if !wantsPage(r) {
		s.handleIndex(w, r)
		return
	}
	// The site filesystem is installed at startup but is nil in a bare test or
	// a future caller, and fs.ReadFile on a nil FS would panic rather than 404.
	if SiteFS == nil {
		writeJSONError(w, http.StatusNotFound, "home page not found")
		return
	}
	data, err := fs.ReadFile(SiteFS, "index.html")
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "home page not found")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The page is a shell over live data and a version-stamped binary; never
	// let a stale copy outlive a router restart.
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

// serveStatus answers the public status feed as JSON, or the status website
// when a browser navigated to the URL. It is the /v1/status twin of
// serveModels: one URL, two representations, and wantsPage picks between them.
func (s *Server) serveStatus(w http.ResponseWriter, r *http.Request) {
	// Same URL, two representations: shared caches have to key on Accept, or a
	// client that asked for the JSON feed could be handed the page shell.
	w.Header().Set("Vary", "Accept")
	if wantsPage(r) {
		s.handleStatusPage(w, r)
		return
	}
	s.handlePublicStatus(w, r)
}

// wantsPage reports whether the client asked for the /models website
// rather than the JSON list. An explicit ?format= wins; otherwise a
// browser's Accept header (which names text/html) decides, so curl and
// SDKs keep getting JSON.
func wantsPage(r *http.Request) bool {
	switch r.URL.Query().Get("format") {
	case "html":
		return true
	case "json":
		return false
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}
