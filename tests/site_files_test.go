package tests

// The website is embedded whole, so a file that nothing serves still ships in
// the binary. When the site was moved onto /css, /js and /img, four copies were
// left behind at the website root - models.css, marked.min.js, og-image.png and
// tinypixel.svg, ~280KB. Nothing served them: router has no wildcard route for
// the site root, only the explicit /favicon.ico one. They have been deleted,
// and these tests are what keeps them deleted.
//
// The pair of tests is the point. One says every file in the tree is served at
// the URL its path implies, so nothing is missing; the other says nothing is
// stranded where no route can reach it, so nothing is dead. A file cannot
// satisfy one and fail the other.
//
// One thing deleting the leftovers does *not* do, measured rather than assumed:
// it does not make a stray request 404. There never was a route for a root
// asset, so GET /models.css reached handleForward and was proxied to the
// provider before the deletion and still does. The win is the dead bytes and
// the confusing duplicate, not the request. Refusing unknown paths instead of
// forwarding them is the opposite of what this router is for - the passthrough
// contract is the product - so it is left alone on purpose.

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// servedFolders are the directories router.Handler mounts, and so the only
// places an asset can live and still be reachable (see the /css, /js, /img,
// /fonts and /favicon routes).
var servedFolders = map[string]bool{
	"css": true, "js": true, "img": true, "fonts": true, "favicon": true,
}

// shells are the only files allowed at the website root. They are read by name
// by serveModelsPage and serveDocsPage rather than served at their own URL, so
// they are reachable even though no route maps a path onto them.
var shells = map[string]bool{
	"index.html":  true,
	"models.html": true,
	"docs.html":   true,
	"status.html": true,
}

// walkWebsite calls fn for every file //go:embed actually carries from
// website/, named relative to that directory ("css/site.css", "models.html").
//
// Dotfiles and underscore files are skipped to match //go:embed, which leaves
// them out of a directory embed: flagging one would report a file that is not
// in the binary as dead weight in it.
func walkWebsite(t *testing.T, fn func(name string)) {
	t.Helper()
	dir := filepath.Join("..", "website")
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		if strings.HasPrefix(path.Base(name), ".") || strings.HasPrefix(path.Base(name), "_") {
			return nil
		}
		fn(name)
		return nil
	})
	if err != nil {
		t.Fatalf("walk website: %v", err)
	}
}

// TestNoStrandedAssetsAtTheSiteRoot is the guard for the four leftovers, and
// for the next set of them.
//
// A root file is unreachable unless it is one of the two shells, so a root
// asset is dead weight in the embed and nothing more: the request for it is
// forwarded upstream whether the file is on disk or not.
func TestNoStrandedAssetsAtTheSiteRoot(t *testing.T) {
	var stranded []string
	walkWebsite(t, func(name string) {
		if strings.Contains(name, "/") || shells[name] {
			return
		}
		stranded = append(stranded, name)
	})
	for _, name := range stranded {
		t.Errorf("website/%s sits at the site root, where no route can serve it, so it is "+
			"dead weight in the embed. Move it under /css, /js, /img, /fonts or /favicon, "+
			"or delete it if it is a leftover", name)
	}
}

// TestModelsPageHooksTheScriptNeeds pins the contract between the models page
// and the script that renders it. models.html is static markup: the kind pills,
// the cards, the stats and the modal are all built by models.js from
// GET /v1/models. So a kind is only as supported as the hooks the shell hands
// the script, and a missing one fails quietly - the pill row or the modal comes
// up half-empty and no error path says why.
//
// The video kind is the case that made this worth pinning: it renders through
// exactly the same handful of ids as image and OCR, so it needed nothing new in
// the shell - which is precisely what is easy to break by accident. The check
// is the whole contract rather than the video ids: every id models.js looks up
// has to be named either by the shell or by the markup models.js injects
// itself, and the two directions are checked against each other here so the
// next kind added to the KINDS table cannot miss a hook silently.
func TestModelsPageHooksTheScriptNeeds(t *testing.T) {
	html, err := os.ReadFile(filepath.Join("..", "website", "models.html"))
	if err != nil {
		t.Fatal(err)
	}
	js, err := os.ReadFile(filepath.Join("..", "website", "js", "models.js"))
	if err != nil {
		t.Fatal(err)
	}

	shell := map[string]bool{}
	for _, m := range regexp.MustCompile(`id="([^"]+)"`).FindAllStringSubmatch(string(html), -1) {
		shell[m[1]] = true
	}
	// The two states models.js renders into #panel - the error card's retry
	// button and the empty result's clear button - name their own ids.
	injected := map[string]bool{}
	for _, m := range regexp.MustCompile(`id="([^"]+)"`).FindAllStringSubmatch(string(js), -1) {
		injected[m[1]] = true
	}

	queries := regexp.MustCompile(`\$\('([^']+)'\)`)
	if !queries.Match(js) {
		t.Fatal("no $('id') lookups parsed from models.js; the page now addresses elements some other way")
	}
	for _, m := range queries.FindAllStringSubmatch(string(js), -1) {
		id := m[1]
		if shell[id] || injected[id] {
			continue
		}
		t.Errorf("models.js looks up #%s, which neither models.html nor models.js defines, so that part of the page renders into nothing", id)
	}
}

// TestEveryEmbeddedAssetIsServed fetches every file in the tree from a live
// server and expects a 200 with a real content type.
//
// The site keeps its URL structure and its file layout identical - /css/site.css
// is website/css/site.css - which is what lets one rule serve every asset. That
// identity is asserted here rather than assumed, because it is the assumption
// the whole asset route rests on.
func TestEveryEmbeddedAssetIsServed(t *testing.T) {
	cfg := &Config{
		Server:   ServerConfig{MaxBodyMB: 1, RequestSecs: 5},
		Proxy:    ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt")},
		Provider: map[string]*Provider{"P": {Options: ProviderOptions{BaseURL: "http://x/v1"}}},
	}
	cfg.ApplyDefaults()
	srv := NewServer(cfg, NewPool(cfg.Proxy.File, "", "rotate", true), newLogger(false))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	get := func(url, accept string) *http.Response {
		t.Helper()
		req, err := http.NewRequest("GET", ts.URL+url, nil)
		if err != nil {
			t.Fatal(err)
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
		resp.Body.Close()
		return resp
	}

	walkWebsite(t, func(name string) {
		// A shell is a page, served where its route lives rather than at its
		// own filename, so each is fetched where it is actually rendered.
		url := "/" + name
		switch name {
		case "index.html":
			url = "/"
		case "models.html":
			url = "/models?format=html"
		case "docs.html":
			url = "/docs/configuration.md?format=html"
		case "status.html":
			url = "/status"
		}
		if !shells[name] && !inServedFolder(name) {
			t.Errorf("website/%s is in no mounted folder, so /%s has no route and cannot be served",
				name, name)
			return
		}

		resp := get(url, "")
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s (website/%s): status %d, the file is embedded but not served",
				url, name, resp.StatusCode)
			return
		}
		// An extension the router has no pinned type for is served as
		// application/octet-stream, which a browser downloads rather than
		// renders: reachable, and still useless.
		if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "application/octet-stream") {
			t.Errorf("GET %s: content-type %q, so the browser downloads it instead of rendering it", url, ct)
		}
	})
}

// inServedFolder reports whether a website-relative path sits inside one of the
// folders router mounts. A file nested deeper than one level is included: the
// /{folder}/{file...} patterns are wildcards, so depth is fine, an unmounted
// folder is not.
func inServedFolder(name string) bool {
	dir, _, _ := strings.Cut(name, "/")
	return servedFolders[dir]
}

// TestEveryKindIsStyled is the guard for the moderation chip that shipped
// unstyled. models.js renders a class per kind - kind-<k> on the card chip and
// in the modal, dot-<k> on the filter pill - and colors it with an accent
// token --kind-<k>; site.css has to define all three, or that kind renders as a
// transparent chip and an invisible dot while everything else looks fine. The
// two files are checked against each other here, so the next kind added to the
// KINDS table cannot miss the stylesheet again.
func TestEveryKindIsStyled(t *testing.T) {
	js, err := os.ReadFile(filepath.Join("..", "website", "js", "models.js"))
	if err != nil {
		t.Fatal(err)
	}
	css, err := os.ReadFile(filepath.Join("..", "website", "css", "site.css"))
	if err != nil {
		t.Fatal(err)
	}
	// The KINDS table is the page's list of kinds, and each entry opens at a
	// fixed indent ("    chat: {") - which is how they are picked out here.
	// The name may hold an underscore (image_edit), so the class below allows one.
	re := regexp.MustCompile(`(?m)^    ([a-z][a-z0-9_]*): \{$`)
	var kinds []string
	seen := map[string]bool{}
	for _, m := range re.FindAllSubmatch(js, -1) {
		k := string(m[1])
		if !seen[k] {
			seen[k] = true
			kinds = append(kinds, k)
		}
	}
	if len(kinds) < 5 {
		t.Fatalf("parsed %d kinds from models.js (%v), want the whole KINDS table; the pattern above no longer matches the file",
			len(kinds), kinds)
	}
	for _, k := range kinds {
		for _, want := range []string{".kind-" + k + " ", ".dot-" + k + " ", "--kind-" + k + ":"} {
			if !strings.Contains(string(css), want) {
				t.Errorf("kind %q: site.css defines no %q, so its chip and pill dot render unstyled", k, want)
			}
		}
	}
}

// The modal stays hidden until it is opened, but must remain rendered while
// its closing opacity transition runs. This static contract catches the two
// same-frame hidden/class changes that make both fades disappear.
func TestModelsModalTransitionRetainsHiddenState(t *testing.T) {
	js, err := os.ReadFile(filepath.Join("..", "website", "js", "models.js"))
	if err != nil {
		t.Fatal(err)
	}
	css, err := os.ReadFile(filepath.Join("..", "website", "css", "site.css"))
	if err != nil {
		t.Fatal(err)
	}

	jsText := string(js)
	openHidden := strings.Index(jsText, "overlay.hidden = false;")
	openReflow := strings.Index(jsText, "void overlay.offsetWidth;")
	openClass := strings.Index(jsText, "overlay.classList.add('open');")
	if openHidden < 0 || openReflow < openHidden || openClass < openReflow {
		t.Fatalf("modal open must reveal, render, then add .open for the opening transition")
	}

	closeStart := strings.Index(jsText, "  function closeModal() {")
	if closeStart < 0 {
		t.Fatal("could not find closeModal")
	}
	closeEndOffset := strings.Index(jsText[closeStart:], "  $('modal-close').addEventListener")
	if closeEndOffset < 0 {
		t.Fatal("could not find the end of closeModal")
	}
	closeEnd := closeStart + closeEndOffset
	closeBody := jsText[closeStart:closeEnd]
	if strings.Contains(closeBody, "overlay.hidden = true;") {
		t.Fatal("closeModal hides the overlay before its fade can finish")
	}
	for _, want := range []string{
		"overlay.addEventListener('transitionend', modalTransitionEnd);",
		"modalHideTimer = setTimeout(finishModalClose, 220);",
	} {
		if !strings.Contains(closeBody, want) {
			t.Errorf("closeModal is missing %q", want)
		}
	}

	cssText := string(css)
	for _, want := range []string{
		".modal-overlay[hidden] { display: none; }",
		"transition: opacity .18s ease, visibility 0s linear .18s;",
		"opacity: 1; visibility: visible; pointer-events: auto;",
	} {
		if !strings.Contains(cssText, want) {
			t.Errorf("site.css is missing modal transition contract %q", want)
		}
	}
}

// The landing page parks its nav above the fold, which is only acceptable
// because three fallbacks keep it reachable: without JavaScript the .js class
// is never set, so the parked rule applies to nothing and the bar is a plain
// sticky nav; with the keyboard, focusing inside the parked bar (:focus-within)
// pulls it back into view; and screen readers read the header and its links
// regardless of a transform. Delete the first two and the bar becomes an
// invisible strip that tabbing walks through blind, so the contract is pinned
// here rather than left to a manual pass.
func TestHomeNavStaysReachableWhileParkedAboveTheFold(t *testing.T) {
	css, err := os.ReadFile(filepath.Join("..", "website", "css", "site.css"))
	if err != nil {
		t.Fatal(err)
	}
	html, err := os.ReadFile(filepath.Join("..", "website", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		".js .page-home.nav-visible .nav",
		".js .page-home .nav:focus-within",
	} {
		if !strings.Contains(string(css), want) {
			t.Errorf("site.css is missing %q, so the parked nav has no reveal path and its links become unreachable", want)
		}
	}
	if !strings.Contains(string(html), `class="page-home"`) {
		t.Error("index.html no longer marks <body> page-home, so the parked-nav rules apply nowhere and every reader keeps a plain bar")
	}
	if !strings.Contains(string(html), "classList.add('js')") {
		t.Error("index.html no longer sets the .js class before first paint, so the parked-nav rule hides the bar with no scripting fallback")
	}
}
