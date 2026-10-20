package tests

// Regression tests for the audit finding: on the zero-config default (no
// auth.token, no auth.apiKeys) any web page the operator visited could drive
// the router with a browser "simple" POST - JSON labelled text/plain, no
// credentials, foreign Origin - which is delivered without a CORS preflight.
// The response stayed unreadable (no CORS headers), but the router presented
// the real provider key upstream and burned quota. The gate now refuses a
// request the browser marks as belonging to another origin, by comparing the
// Origin header against Host - the header a page cannot forge.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// newForwardRouter stands up a router with one keyless provider pointing at
// the given handler and no client key gate - the documented zero-config
// default the finding is about. allowedHosts are extra hostnames the router
// answers forwarded requests for, the way a front-proxy deployment declares
// its public name (see server.allowedHosts). The returned counter is bumped
// once per request that reaches the upstream.
func newForwardRouter(t *testing.T, upstream http.HandlerFunc, allowedHosts ...string) (base string, hits *atomic.Int64) {
	t.Helper()
	var counter atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counter.Add(1)
		upstream(w, r)
	}))
	t.Cleanup(ts.Close)

	cfg := &Config{
		Server:   ServerConfig{MaxBodyMB: 1, RequestSecs: 5, AllowedHosts: allowedHosts},
		Proxy:    ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt"), Enabled: boolPtr(false)},
		Provider: map[string]*Provider{"P": {Options: ProviderOptions{BaseURL: ts.URL, APIKey: "sk-providerkey"}, Models: map[string]*Model{}}},
	}
	cfg.ApplyDefaults()
	rt := httptest.NewServer(NewServer(cfg, NewPool(cfg.Proxy.File, "", "rotate", false), newLogger(false)).Handler())
	t.Cleanup(rt.Close)
	return rt.URL, &counter
}

// simplePost fires the request a hostile page's no-cors fetch produces: a
// plain-text POST carrying a JSON body, an Origin header (browser-set) and no
// client credential. headers are extra request headers the test controls.
func simplePost(t *testing.T, base string, headers map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "text/plain") // a simple request content type
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// TestCrossOriginSimplePostIsRefused is the finding itself: a foreign Origin
// on a simple POST must be turned away at the gate, with the upstream never
// seeing the request - no provider key presented, no quota spent.
func TestCrossOriginSimplePostIsRefused(t *testing.T) {
	okUpstream := func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":"chatcmpl-1","choices":[]}`)
	}
	base, hits := newForwardRouter(t, okUpstream)

	code, body := simplePost(t, base, map[string]string{"Origin": "https://evil.example"})
	if code != http.StatusForbidden {
		t.Errorf("cross-origin simple POST: status %d, want 403 (body: %s)", code, body)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("upstream was reached %d times by a refused request, want 0", n)
	}
}

// TestCrossOriginGateKeepsSDKClients: the same request with no Origin header -
// curl, an SDK, any non-browser client - is forwarded as before.
func TestCrossOriginGateKeepsSDKClients(t *testing.T) {
	okUpstream := func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":"chatcmpl-1","choices":[]}`)
	}
	base, hits := newForwardRouter(t, okUpstream)

	code, body := simplePost(t, base, nil)
	if code != http.StatusOK {
		t.Errorf("no-Origin POST: status %d, want 200 (body: %s)", code, body)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("upstream was reached %d times, want 1", n)
	}
}

// TestSameOriginIsAllowed: a browser page served by the router itself (or a
// front-proxied deployment where the proxy passes the original Host through,
// as the shipped nginx example does) names this host in Origin and keeps
// working. The front-proxy public name has to be declared in
// server.allowedHosts as well - the host allowlist runs alongside the Origin
// check, and TestAllowedHostsFrontProxyNamePasses covers that pairing.
func TestSameOriginIsAllowed(t *testing.T) {
	okUpstream := func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":"chatcmpl-1","choices":[]}`)
	}
	base, hits := newForwardRouter(t, okUpstream)

	// Same-origin page on the router's own port.
	code, body := simplePost(t, base, map[string]string{"Origin": base})
	if code != http.StatusOK {
		t.Errorf("same-origin POST: status %d, want 200 (body: %s)", code, body)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("upstream was reached %d times, want 1", n)
	}

	// Behind a front proxy the browser talks to the public name: the proxy
	// sets Host (and X-Forwarded-Host) to it, so Origin matches Host, and
	// the deployment lists that name in server.allowedHosts.
	base, hits = newForwardRouter(t, okUpstream, "llm.example.com")
	req, err := http.NewRequest(http.MethodPost, base+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "llm.example.com"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://llm.example.com")
	req.Header.Set("X-Forwarded-Host", "llm.example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("front-proxied same-origin POST: status %d, want 200", resp.StatusCode)
	}

	if n := hits.Load(); n != 1 {
		t.Errorf("upstream was reached %d times, want 1", n)
	}
}

// TestSpoofedForwardedHostDoesNotOpenTheGate: a page may set X-Forwarded-Host
// itself, so a foreign origin smuggled in that header must not pass just
// because it matches its own Origin. Only Host - which the browser reserves -
// vouches for the origin.
func TestSpoofedForwardedHostDoesNotOpenTheGate(t *testing.T) {
	okUpstream := func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":"chatcmpl-1","choices":[]}`)
	}
	base, hits := newForwardRouter(t, okUpstream)

	code, _ := simplePost(t, base, map[string]string{
		"Origin":           "https://evil.example",
		"X-Forwarded-Host": "evil.example",
	})
	if code != http.StatusForbidden {
		t.Errorf("POST with spoofed X-Forwarded-Host: status %d, want 403", code)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("upstream was reached %d times by a refused request, want 0", n)
	}
}

// TestNullOriginIsRefused: "null" is what a sandboxed iframe or a file://
// page sends; it matches no host and is refused with the rest.
func TestNullOriginIsRefused(t *testing.T) {
	okUpstream := func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":"chatcmpl-1","choices":[]}`)
	}
	base, hits := newForwardRouter(t, okUpstream)

	code, _ := simplePost(t, base, map[string]string{"Origin": "null"})
	if code != http.StatusForbidden {
		t.Errorf("Origin: null POST: status %d, want 403", code)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("upstream was reached %d times by a refused request, want 0", n)
	}
}

// TestSecFetchSiteCrossSiteIsRefused: fetch metadata covers the requests that
// carry it but no Origin, such as a cross-site simple GET. A navigation - the
// browser setting a URL by hand or from a bookmark - is Sec-Fetch-Site: none
// and passes.
func TestSecFetchSiteCrossSiteIsRefused(t *testing.T) {
	okUpstream := func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":"chatcmpl-1","choices":[]}`)
	}
	base, hits := newForwardRouter(t, okUpstream)

	code, _ := simplePost(t, base, map[string]string{"Sec-Fetch-Site": "cross-site"})
	if code != http.StatusForbidden {
		t.Errorf("Sec-Fetch-Site: cross-site POST: status %d, want 403", code)
	}

	code, body := simplePost(t, base, map[string]string{"Sec-Fetch-Site": "none"})
	if code != http.StatusOK {
		t.Errorf("Sec-Fetch-Site: none POST: status %d, want 200 (body: %s)", code, body)
	}

	if n := hits.Load(); n != 1 {
		t.Errorf("upstream was reached %d times, want 1 (only the navigation passed)", n)
	}
}

// TestKeyedConfigKeepsIts401: with auth.apiKeys configured the vector was
// already closed by the key gate; the cross-origin request must still be
// answered 401 there, in the order clients already observe.
func TestKeyedConfigKeepsIts401(t *testing.T) {
	var counter atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counter.Add(1)
		io.WriteString(w, `{"id":"chatcmpl-1","choices":[]}`)
	}))
	t.Cleanup(upstream.Close)

	cfg := &Config{
		Server:   ServerConfig{MaxBodyMB: 1, RequestSecs: 5},
		Auth:     AuthConfig{APIKeys: keyList{"sk-one"}},
		Proxy:    ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt"), Enabled: boolPtr(false)},
		Provider: map[string]*Provider{"P": {Options: ProviderOptions{BaseURL: upstream.URL, APIKey: "sk-providerkey"}, Models: map[string]*Model{}}},
	}
	cfg.ApplyDefaults()
	rt := httptest.NewServer(NewServer(cfg, NewPool(cfg.Proxy.File, "", "rotate", false), newLogger(false)).Handler())
	t.Cleanup(rt.Close)

	code, _ := simplePost(t, rt.URL, map[string]string{"Origin": "https://evil.example"})
	if code != http.StatusUnauthorized {
		t.Errorf("cross-origin POST with a key gate configured: status %d, want 401", code)
	}
	if n := counter.Load(); n != 0 {
		t.Errorf("upstream was reached %d times by a refused request, want 0", n)
	}
}

// TestPlainTextJSONBodyIsForwardedAsIs pins the router-side half of the
// audit's "needs validation" item: the router is content-type agnostic on
// purpose - requests are forwarded path-through, so the upstream decides
// what it accepts. A non-browser client (no Origin) sending JSON labelled
// text/plain is forwarded with the provider credential attached and the
// body byte for byte. Whether an upstream charges such a request or answers
// 400 is provider-specific and outside this repo.
func TestPlainTextJSONBodyIsForwardedAsIs(t *testing.T) {
	var gotAuth, gotCT, gotBody string
	captureUpstream := func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		io.WriteString(w, `{"id":"chatcmpl-1","choices":[]}`)
	}
	base, hits := newForwardRouter(t, captureUpstream)

	code, body := simplePost(t, base, nil) // text/plain, no Origin: a curl/SDK client
	if code != http.StatusOK {
		t.Fatalf("text/plain JSON POST from a non-browser client: status %d, want 200 (body: %s)", code, body)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("upstream was reached %d times, want 1", n)
	}
	if gotAuth != "Bearer sk-providerkey" {
		t.Errorf("upstream Authorization = %q, want the provider key presented", gotAuth)
	}
	if gotCT != "text/plain" {
		t.Errorf("upstream Content-Type = %q, want the client's text/plain forwarded unchanged", gotCT)
	}
	if gotBody != `{"model":"m","messages":[]}` {
		t.Errorf("upstream body = %q, want the client's JSON intact", gotBody)
	}
}
