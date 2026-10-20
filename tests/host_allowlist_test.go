package tests

// Regression tests for the DNS-rebinding variant of the audit finding F2.
// The Origin gate in crossSiteRefusal turns away a request whose Origin names
// another site, but DNS rebinding defeats exactly that: the attacker points
// their own domain at the router, so the browser's Origin and Host are the
// attacker's name, identical to each other, and the request is same-origin -
// readable response and provider key included. The host allowlist closes it:
// a forwarded request is answered only for loopback, the configured listen
// host, and the hostnames listed in server.allowedHosts. The page chooses
// none of those, and the Host header is not one it is allowed to write.

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// postWithHost fires a POST with the Host header set the way a browser or a
// front proxy sets it - req.Host, not a header map - plus the given extra
// headers. It is the same request shape as simplePost otherwise.
func postWithHost(t *testing.T, base, host string, headers map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "text/plain")
	req.Host = host
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

// okChat is the upstream every test here points the provider at: a plain
// answer, so a status that is not 200 is the router refusing, not the hop.
func okChat(w http.ResponseWriter, r *http.Request) {
	io.WriteString(w, `{"id":"chatcmpl-1","choices":[]}`)
}

// TestDNSRebindSameOriginPostIsRefused is the finding: a hostile page whose
// DNS name resolves to the router makes a same-origin POST - Origin and Host
// both its own rebound name - which the Origin gate passes and the host
// allowlist must refuse, with the upstream never seeing the request.
func TestDNSRebindSameOriginPostIsRefused(t *testing.T) {
	base, hits := newForwardRouter(t, okChat)

	code, body := postWithHost(t, base, "rebind.attacker.test", map[string]string{
		"Origin":         "https://rebind.attacker.test",
		"Sec-Fetch-Site": "same-origin", // what a rebound page's fetch really carries
	})
	if code != http.StatusForbidden {
		t.Errorf("rebound same-origin POST: status %d, want 403 (body: %s)", code, body)
	}
	if !strings.Contains(body, "allowedHosts") {
		t.Errorf("refusal does not tell the operator how to fix it: %s", body)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("upstream was reached %d times by a refused request, want 0", n)
	}
}

// TestLoopbackHostsPassWithoutConfig: the zero-config default is loopback, so
// every loopback spelling a client might send - localhost, any 127.0.0.0/8
// address, the IPv6 one - stays answerable with nothing configured.
func TestLoopbackHostsPassWithoutConfig(t *testing.T) {
	base, hits := newForwardRouter(t, okChat)

	// The port matches the loopback name the same way a browser would pair
	// them, so this exercises port stripping on both sides of the compare.
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(base, "http://"))
	for _, host := range []string{"127.0.0.1:" + port, "localhost:" + port, "127.0.0.7:" + port, "[::1]:" + port, "LOCALHOST"} {
		code, body := postWithHost(t, base, host, map[string]string{"Origin": "http://" + host})
		if code != http.StatusOK {
			t.Errorf("loopback host %q: status %d, want 200 (body: %s)", host, code, body)
		}
	}
	if n := hits.Load(); n != 5 {
		t.Errorf("upstream was reached %d times, want 5", n)
	}
}

// TestAllowedHostsEntryPasses: a name listed in server.allowedHosts is
// answered, spelled with any casing and with or without a port - Host headers
// reach the router as the client typed them, and the port is not part of the
// name.
func TestAllowedHostsEntryPasses(t *testing.T) {
	base, hits := newForwardRouter(t, okChat, "llm.example.com")

	code, body := postWithHost(t, base, "llm.example.com", map[string]string{
		"Origin": "https://llm.example.com",
	})
	if code != http.StatusOK {
		t.Errorf("declared host: status %d, want 200 (body: %s)", code, body)
	}

	code, body = postWithHost(t, base, "LLM.Example.COM:8443", map[string]string{
		"Origin": "https://llm.example.com:8443",
	})
	if code != http.StatusOK {
		t.Errorf("declared host with port and casing: status %d, want 200 (body: %s)", code, body)
	}

	// A name the operator never declared is still refused, and a forged
	// X-Forwarded-Host naming a declared one does not vouch for it: the
	// allowlist, like the Origin check, reads Host only.
	code, body = postWithHost(t, base, "other.example.com", map[string]string{
		"Origin":           "https://other.example.com",
		"X-Forwarded-Host": "llm.example.com",
	})
	if code != http.StatusForbidden {
		t.Errorf("undeclared host with spoofed X-Forwarded-Host: status %d, want 403 (body: %s)", code, body)
	}

	if n := hits.Load(); n != 2 {
		t.Errorf("upstream was reached %d times, want 2", n)
	}
}

// TestAllowedHostsFrontProxyNamePasses: the documented front-proxy shape -
// the proxy passes the original Host through, as the shipped nginx example
// does - answers as long as the public name is declared.
func TestAllowedHostsFrontProxyNamePasses(t *testing.T) {
	base, hits := newForwardRouter(t, okChat, "llm.example.com")

	code, body := postWithHost(t, base, "llm.example.com", map[string]string{
		"Origin":           "https://llm.example.com",
		"X-Forwarded-Host": "llm.example.com",
	})
	if code != http.StatusOK {
		t.Errorf("front-proxied public name: status %d, want 200 (body: %s)", code, body)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("upstream was reached %d times, want 1", n)
	}
}

// TestAllowedHostsEntryMaySpellAURL: an entry copied out of a whole URL -
// scheme and all - still declares the host that names, so a deployment does
// not 403 its own traffic over a copy-paste.
func TestAllowedHostsEntryMaySpellAURL(t *testing.T) {
	base, hits := newForwardRouter(t, okChat, "https://llm.example.com")

	code, body := postWithHost(t, base, "llm.example.com", map[string]string{
		"Origin": "https://llm.example.com",
	})
	if code != http.StatusOK {
		t.Errorf("declared URL entry: status %d, want 200 (body: %s)", code, body)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("upstream was reached %d times, want 1", n)
	}
}

// TestListenHostIsAnsweredFor: a listen host that names an address rather
// than a wildcard interface is answered for on its own - the operator told
// the router where to live. 0.0.0.0 and :: say "every interface", which
// declares nothing.
func TestListenHostIsAnsweredFor(t *testing.T) {
	var counter atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counter.Add(1)
		okChat(w, r)
	}))
	t.Cleanup(upstream.Close)

	cfg := &Config{
		Server:   ServerConfig{Host: "192.0.2.10", MaxBodyMB: 1, RequestSecs: 5},
		Proxy:    ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt"), Enabled: boolPtr(false)},
		Provider: map[string]*Provider{"P": {Options: ProviderOptions{BaseURL: upstream.URL, APIKey: "sk-providerkey"}, Models: map[string]*Model{}}},
	}
	cfg.ApplyDefaults()
	rt := httptest.NewServer(NewServer(cfg, NewPool(cfg.Proxy.File, "", "rotate", false), newLogger(false)).Handler())
	t.Cleanup(rt.Close)

	code, body := postWithHost(t, rt.URL, "192.0.2.10", map[string]string{"Origin": "http://192.0.2.10"})
	if code != http.StatusOK {
		t.Errorf("listen host: status %d, want 200 (body: %s)", code, body)
	}

	// Bound to every interface instead: the address is not declared by the
	// bind, and the address on the wire is 198.51.100.9.
	wildcard := &Config{
		Server:   ServerConfig{Host: "0.0.0.0", MaxBodyMB: 1, RequestSecs: 5},
		Proxy:    ProxyConfig{File: cfg.Proxy.File, Enabled: boolPtr(false)},
		Provider: cfg.Provider,
	}
	wildcard.ApplyDefaults()
	rt2 := httptest.NewServer(NewServer(wildcard, NewPool(wildcard.Proxy.File, "", "rotate", false), newLogger(false)).Handler())
	t.Cleanup(rt2.Close)

	code, body = postWithHost(t, rt2.URL, "198.51.100.9", map[string]string{"Origin": "http://198.51.100.9"})
	if code != http.StatusForbidden {
		t.Errorf("undeclared address behind a wildcard bind: status %d, want 403 (body: %s)", code, body)
	}

	if n := counter.Load(); n != 1 {
		t.Errorf("upstream was reached %d times, want 1", n)
	}
}

// TestHostlessRequestPasses: a client that sends no Host at all (HTTP/1.0,
// which does not carry one) is not a browser - browsers always send it, and
// a rebind carries the attacker's name in the one they always send - so it is
// left alone rather than refused.
func TestHostlessRequestPasses(t *testing.T) {
	base, hits := newForwardRouter(t, okChat)

	conn, err := net.Dial("tcp", strings.TrimPrefix(base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	req := "POST /v1/chat/completions HTTP/1.0\r\n" +
		"Content-Type: text/plain\r\nContent-Length: 27\r\n\r\n" +
		`{"model":"m","messages":[]}`
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("hostless HTTP/1.0 POST: status %d, want 200", resp.StatusCode)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("upstream was reached %d times, want 1", n)
	}
}

// TestKeyedConfigStillChecksHost: the host allowlist does not depend on the
// key gate. A request that presents a valid client key but is addressed to a
// host the operator never declared is still refused - a rebind against a
// keyed deployment is no more welcome than one against a keyless one.
func TestKeyedConfigStillChecksHost(t *testing.T) {
	_, hits := newForwardRouter(t, okChat)
	var counter atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counter.Add(1)
		okChat(w, r)
	}))
	t.Cleanup(upstream.Close)

	keyed := &Config{
		Server:   ServerConfig{MaxBodyMB: 1, RequestSecs: 5},
		Auth:     AuthConfig{APIKeys: keyList{"sk-one"}},
		Proxy:    ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt"), Enabled: boolPtr(false)},
		Provider: map[string]*Provider{"P": {Options: ProviderOptions{BaseURL: upstream.URL, APIKey: "sk-providerkey"}, Models: map[string]*Model{}}},
	}
	keyed.ApplyDefaults()
	rt := httptest.NewServer(NewServer(keyed, NewPool(keyed.Proxy.File, "", "rotate", false), newLogger(false)).Handler())
	t.Cleanup(rt.Close)

	// The right key, the wrong host.
	code, body := postWithHost(t, rt.URL, "rebind.attacker.test", map[string]string{
		"Authorization": "Bearer sk-one",
		"Origin":        "https://rebind.attacker.test",
	})
	if code != http.StatusForbidden {
		t.Errorf("keyed request to an undeclared host: status %d, want 403 (body: %s)", code, body)
	}

	// The right key, a loopback host: unchanged behaviour.
	code, body = postWithHost(t, rt.URL, "127.0.0.1", map[string]string{
		"Authorization": "Bearer sk-one",
		"Origin":        "http://127.0.0.1",
	})
	if code != http.StatusOK {
		t.Errorf("keyed loopback request: status %d, want 200 (body: %s)", code, body)
	}

	if n := counter.Load(); n != 1 {
		t.Errorf("upstream was reached %d times, want 1", n)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the unkeyed router's upstream was reached %d times, want 0", n)
	}
}
