package tests

// Regression tests for the audit finding: the raw proxy entry URL used to be
// handed to the client on every proxied response (X-Noderouter-Proxy), inside
// 502 bodies ("via <url>: ...") and into log lines. Private proxies carry
// user:pass@ in their URL; the client may only ever see the hop.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// credentialed rewrites a proxy URL to carry gateway credentials, the way a
// private proxy entry looks in the list file.
func credentialed(proxyURL string) string {
	return strings.Replace(proxyURL, "http://", "http://gwuser:s3cr3t@", 1)
}

// TestRouterRedactsProxyCredentialsInHeader proxies a request through a
// credentialed hop and requires the answer header to name the hop without the
// credentials baked into its URL.
func TestRouterRedactsProxyCredentialsInHeader(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl-1","choices":[]}`)
	}))
	defer upstream.Close()

	proxyURL, hits := fakeProxy(t)
	withCreds := credentialed(proxyURL)

	dir := t.TempDir()
	list := filepath.Join(dir, "ports.txt")
	if err := os.WriteFile(list, []byte(withCreds+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{
		Server: ServerConfig{MaxBodyMB: 1, RequestSecs: 30},
		Proxy:  ProxyConfig{File: list, Mode: "rotate", MaxAttempts: 3},
		Provider: map[string]*Provider{
			"P": {Options: ProviderOptions{BaseURL: upstream.URL, APIKey: "sk-test", ReProxy: boolPtr(true)},
				Models: map[string]*Model{}},
		},
	}
	cfg.ApplyDefaults()

	pool := NewPool(cfg.Proxy.File, cfg.MetaPath(), cfg.Proxy.Mode, cfg.ProxyEnabled())
	if n, err := pool.Reload(); err != nil || n != 1 {
		t.Fatalf("pool reload: %d %v", n, err)
	}
	srv := NewServer(cfg, pool, newLogger(false))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"anything","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	got := resp.Header.Get("X-Noderouter-Proxy")
	if strings.Contains(got, "s3cr3t") || strings.Contains(got, "gwuser") {
		t.Errorf("X-Noderouter-Proxy leaks credentials: %q", got)
	}
	if got != proxyURL {
		t.Errorf("X-Noderouter-Proxy = %q, want the hop %q", got, proxyURL)
	}
	if hits.Load() == 0 {
		t.Error("the request did not go through the proxy")
	}
}

// TestRouterRedactsProxyCredentialsIn502 fails every attempt behind a
// credentialed dead proxy and requires the 502 body to name the hop without
// its credentials.
func TestRouterRedactsProxyCredentialsIn502(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // nothing is listening any more
	withCreds := credentialed(deadURL)
	deadHost := strings.TrimPrefix(deadURL, "http://")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	list := filepath.Join(dir, "ports.txt")
	if err := os.WriteFile(list, []byte(withCreds+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{
		Server:   ServerConfig{MaxBodyMB: 1, RequestSecs: 5},
		Proxy:    ProxyConfig{File: list, Mode: "rotate", MaxAttempts: 1},
		Provider: map[string]*Provider{"P": {Options: ProviderOptions{BaseURL: upstream.URL, ReProxy: boolPtr(true)}, Models: map[string]*Model{}}},
	}
	cfg.ApplyDefaults()
	pool := NewPool(cfg.Proxy.File, cfg.MetaPath(), "rotate", true)
	pool.Reload()
	srv := NewServer(cfg, pool, newLogger(false))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"anything","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d: want 502, body %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "s3cr3t") || strings.Contains(string(body), "gwuser") {
		t.Errorf("502 body leaks proxy credentials: %s", body)
	}
	if !strings.Contains(string(body), deadHost) {
		t.Errorf("502 body lost the hop %q: %s", deadHost, body)
	}
}
