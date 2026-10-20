package tests

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// auth.apiKeys is a pool of client keys: a forwarded request may present any
// one of them, in any of the three spellings a client uses. auth.token is the
// single-key spelling of the same gate, and with neither configured there is
// no gate at all - any bearer, or no header at all, is accepted.
func TestRouterAPIKeyPool(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	newRouter := func(auth AuthConfig) string {
		cfg := &Config{
			Server:   ServerConfig{MaxBodyMB: 1, RequestSecs: 5},
			Auth:     auth,
			Proxy:    ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt"), Enabled: boolPtr(false)},
			Provider: map[string]*Provider{"P": {Options: ProviderOptions{BaseURL: upstream.URL, APIKey: "sk-upstream"}, Models: map[string]*Model{}}},
		}
		cfg.ApplyDefaults()
		ts := httptest.NewServer(NewServer(cfg, NewPool(cfg.Proxy.File, "", "rotate", false), newLogger(false)).Handler())
		t.Cleanup(ts.Close)
		return ts.URL
	}

	send := func(url, header, value string) int {
		req, err := http.NewRequest(http.MethodPost, url+"/v1/chat/completions",
			strings.NewReader(`{"model":"x","messages":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		if header != "" {
			req.Header.Set(header, value)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	url := newRouter(AuthConfig{APIKeys: keyList{"sk-one", "sk-two"}})

	// Every key in the pool opens the gate, whichever header carries it:
	// Bearer for the OpenAI SDKs, x-api-key for the Anthropic ones, and the
	// header the admin routes already use.
	if code := send(url, "Authorization", "Bearer sk-one"); code != http.StatusOK {
		t.Errorf("sk-one via Bearer: status %d, want 200", code)
	}
	if code := send(url, "Authorization", "Bearer sk-two"); code != http.StatusOK {
		t.Errorf("sk-two via Bearer: status %d, want 200", code)
	}
	if code := send(url, "x-api-key", "sk-one"); code != http.StatusOK {
		t.Errorf("sk-one via x-api-key: status %d, want 200", code)
	}
	if code := send(url, "X-Noderouter-Token", "sk-two"); code != http.StatusOK {
		t.Errorf("sk-two via X-Noderouter-Token: status %d, want 200", code)
	}

	// A key outside the pool, and no credential at all, are refused.
	if code := send(url, "Authorization", "Bearer sk-wrong"); code != http.StatusUnauthorized {
		t.Errorf("wrong key: status %d, want 401", code)
	}
	if code := send(url, "", ""); code != http.StatusUnauthorized {
		t.Errorf("no credential: status %d, want 401", code)
	}

	// auth.token still gates on its one key.
	one := newRouter(AuthConfig{Token: "letmein"})
	if code := send(one, "Authorization", "Bearer letmein"); code != http.StatusOK {
		t.Errorf("auth.token: status %d, want 200", code)
	}
	if code := send(one, "Authorization", "Bearer sk-one"); code != http.StatusUnauthorized {
		t.Errorf("auth.token with a key from another setup: status %d, want 401", code)
	}

	// Nothing configured means nothing checked: an arbitrary bearer passes,
	// and so does a client with no credential at all.
	open := newRouter(AuthConfig{})
	if code := send(open, "Authorization", "Bearer literally-anything"); code != http.StatusOK {
		t.Errorf("no auth configured, arbitrary bearer: status %d, want 200", code)
	}
	if code := send(open, "", ""); code != http.StatusOK {
		t.Errorf("no auth configured, no credential: status %d, want 200", code)
	}
}

// A "file:" entry holds the pool, so client keys rotate without a restart. A
// file the router cannot read contributes no keys - and must not open the
// gate, because the gate was asked for.
func TestRouterAPIKeyFromFile(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	newRouter := func(entry string) string {
		cfg := &Config{
			Server:   ServerConfig{MaxBodyMB: 1, RequestSecs: 5},
			Auth:     AuthConfig{APIKeys: keyList{entry}},
			Proxy:    ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt"), Enabled: boolPtr(false)},
			Provider: map[string]*Provider{"P": {Options: ProviderOptions{BaseURL: upstream.URL, APIKey: "sk-upstream"}, Models: map[string]*Model{}}},
		}
		cfg.ApplyDefaults()
		ts := httptest.NewServer(NewServer(cfg, NewPool(cfg.Proxy.File, "", "rotate", false), newLogger(false)).Handler())
		t.Cleanup(ts.Close)
		return ts.URL
	}

	bearer := func(url, key string) int {
		req, _ := http.NewRequest(http.MethodPost, url+"/v1/chat/completions",
			strings.NewReader(`{"model":"x","messages":[]}`))
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "client-keys.txt")
	if err := os.WriteFile(path, []byte("# one key per line, comments dropped\nsk-file-one\n\nsk-file-two\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	url := newRouter("file:" + path)
	if code := bearer(url, "sk-file-one"); code != http.StatusOK {
		t.Errorf("key from file: status %d, want 200", code)
	}
	if code := bearer(url, "sk-file-two"); code != http.StatusOK {
		t.Errorf("second key from file: status %d, want 200", code)
	}
	if code := bearer(url, "sk-not-in-file"); code != http.StatusUnauthorized {
		t.Errorf("key outside the file: status %d, want 401", code)
	}

	// Missing file: no keys resolve, and the gate stays closed.
	gone := newRouter("file:" + filepath.Join(dir, "missing.txt"))
	if code := bearer(gone, "sk-file-one"); code != http.StatusUnauthorized {
		t.Errorf("unreadable key file: status %d, want 401 (a configured gate fails closed)", code)
	}
}

// The key that opens the router is for this hop only, and no client credential
// is the provider's business: a keyless provider sees a request with no
// credential at all, and X-Noderouter-Token never leaves the router at all.
func TestRouterAPIKeyNeverReachesUpstream(t *testing.T) {
	type seen struct{ auth, apiKey, admin string }
	got := make(chan seen, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- seen{auth: r.Header.Get("Authorization"), apiKey: r.Header.Get("X-Api-Key"), admin: r.Header.Get("X-Noderouter-Token")}
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	newRouter := func(auth AuthConfig) string {
		// APIKey "" is a keyless provider: no credential of any kind is
		// supposed to reach the upstream.
		cfg := &Config{
			Server:   ServerConfig{MaxBodyMB: 1, RequestSecs: 5},
			Auth:     auth,
			Proxy:    ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt"), Enabled: boolPtr(false)},
			Provider: map[string]*Provider{"P": {Options: ProviderOptions{BaseURL: upstream.URL, APIKey: ""}, Models: map[string]*Model{}}},
		}
		cfg.ApplyDefaults()
		ts := httptest.NewServer(NewServer(cfg, NewPool(cfg.Proxy.File, "", "rotate", false), newLogger(false)).Handler())
		t.Cleanup(ts.Close)
		return ts.URL
	}

	// newRouterWithKey is the same router pointed at a provider that holds a
	// credential of its own.
	newRouterWithKey := func(auth AuthConfig, providerKey string) string {
		cfg := &Config{
			Server:   ServerConfig{MaxBodyMB: 1, RequestSecs: 5},
			Auth:     auth,
			Proxy:    ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt"), Enabled: boolPtr(false)},
			Provider: map[string]*Provider{"P": {Options: ProviderOptions{BaseURL: upstream.URL, APIKey: providerKey}, Models: map[string]*Model{}}},
		}
		cfg.ApplyDefaults()
		ts := httptest.NewServer(NewServer(cfg, NewPool(cfg.Proxy.File, "", "rotate", false), newLogger(false)).Handler())
		t.Cleanup(ts.Close)
		return ts.URL
	}

	send := func(url string, headers map[string]string) int {
		req, _ := http.NewRequest(http.MethodPost, url+"/v1/chat/completions",
			strings.NewReader(`{"model":"x","messages":[]}`))
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	// Bearer: the router key opened the gate and is dropped before the hop.
	url := newRouter(AuthConfig{APIKeys: keyList{"sk-router"}})
	if code := send(url, map[string]string{"Authorization": "Bearer sk-router"}); code != http.StatusOK {
		t.Fatalf("router key via Bearer: status %d, want 200", code)
	}
	if h := <-got; h.auth != "" || h.apiKey != "" || h.admin != "" {
		t.Errorf("router key leaked upstream: auth %q x-api-key %q X-Noderouter-Token %q", h.auth, h.apiKey, h.admin)
	}

	// x-api-key, the Anthropic spelling, is dropped the same way.
	if code := send(url, map[string]string{"x-api-key": "sk-router"}); code != http.StatusOK {
		t.Fatalf("router key via x-api-key: status %d, want 200", code)
	}
	if h := <-got; h.auth != "" || h.apiKey != "" {
		t.Errorf("router key leaked upstream as auth %q / x-api-key %q", h.auth, h.apiKey)
	}

	// With no gate configured the client's key is still not the provider's
	// to judge, and the admin header does not leave either.
	open := newRouter(AuthConfig{})
	if code := send(open, map[string]string{"Authorization": "Bearer sk-client", "X-Noderouter-Token": "whatever"}); code != http.StatusOK {
		t.Fatalf("open gate with a client key: status %d, want 200", code)
	}
	if h := <-got; h.auth != "" || h.admin != "" {
		t.Errorf("client credential left the router: auth %q X-Noderouter-Token %q", h.auth, h.admin)
	}

	// x-api-key is the Anthropic spelling of the same non-credential.
	if code := send(open, map[string]string{"x-api-key": "sk-client"}); code != http.StatusOK {
		t.Fatalf("open gate with x-api-key: status %d, want 200", code)
	}
	if h := <-got; h.auth != "" || h.apiKey != "" {
		t.Errorf("client x-api-key left the router: auth %q x-api-key %q", h.auth, h.apiKey)
	}

	// A provider with a key of its own: the router's credential replaces the
	// client's in full, whichever dialect the client spoke. Setting the
	// bearer used to leave a client's x-api-key riding along beside it.
	keyed := newRouterWithKey(AuthConfig{}, "sk-provider")
	if code := send(keyed, map[string]string{"Authorization": "Bearer example"}); code != http.StatusOK {
		t.Fatalf("keyed provider, client bearer: status %d, want 200", code)
	}
	if h := <-got; h.auth != "Bearer sk-provider" || h.apiKey != "" {
		t.Errorf("keyed provider saw auth %q x-api-key %q, want only the provider key", h.auth, h.apiKey)
	}
	if code := send(keyed, map[string]string{"x-api-key": "example"}); code != http.StatusOK {
		t.Fatalf("keyed provider, client x-api-key: status %d, want 200", code)
	}
	if h := <-got; h.auth != "Bearer sk-provider" || h.apiKey != "" {
		t.Errorf("client x-api-key rode along to a keyed provider: auth %q x-api-key %q", h.auth, h.apiKey)
	}
}

// The /noderouter/* routes take the admin token - configured, or generated at
// startup when it is unset - and any key from auth.apiKeys as well, in the
// same three spellings a forwarded request accepts. An operator who only
// configured client keys therefore does not need a second credential to read
// status.
func TestAdminAcceptsAPIKeys(t *testing.T) {
	newRouter := func(auth AuthConfig) (*httptest.Server, string) {
		cfg := &Config{
			Server:   ServerConfig{MaxBodyMB: 1, RequestSecs: 5},
			Auth:     auth,
			Proxy:    ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt")},
			Provider: map[string]*Provider{"P": {Options: ProviderOptions{BaseURL: "http://x"}, Models: map[string]*Model{}}},
		}
		cfg.ApplyDefaults()
		srv := NewServer(cfg, NewPool(cfg.Proxy.File, "", "rotate", true), newLogger(false))
		ts := httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)
		return ts, srv.DynamicAdminToken()
	}

	get := func(url, header, value string) int {
		req, err := http.NewRequest(http.MethodGet, url+"/noderouter/status", nil)
		if err != nil {
			t.Fatal(err)
		}
		if header != "" {
			req.Header.Set(header, value)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	// Only apiKeys configured: the pool opens the admin routes, and the
	// generated admin token keeps working beside it.
	ts, dynamic := newRouter(AuthConfig{APIKeys: keyList{"sk-admin"}})
	if code := get(ts.URL, "", ""); code != http.StatusUnauthorized {
		t.Errorf("no credential: status %d, want 401", code)
	}
	if code := get(ts.URL, "X-Noderouter-Token", "sk-admin"); code != http.StatusOK {
		t.Errorf("pool key via X-Noderouter-Token: status %d, want 200", code)
	}
	if code := get(ts.URL, "Authorization", "Bearer sk-admin"); code != http.StatusOK {
		t.Errorf("pool key via Bearer: status %d, want 200", code)
	}
	if code := get(ts.URL, "x-api-key", "sk-admin"); code != http.StatusOK {
		t.Errorf("pool key via x-api-key: status %d, want 200", code)
	}
	if code := get(ts.URL, "X-Noderouter-Token", dynamic); code != http.StatusOK {
		t.Errorf("generated admin token: status %d, want 200", code)
	}
	if code := get(ts.URL, "Authorization", "Bearer sk-wrong"); code != http.StatusUnauthorized {
		t.Errorf("key outside the pool: status %d, want 401", code)
	}

	// With auth.adminToken set too, both credentials are accepted.
	both, _ := newRouter(AuthConfig{AdminToken: "secret", APIKeys: keyList{"sk-admin"}})
	if code := get(both.URL, "X-Noderouter-Token", "secret"); code != http.StatusOK {
		t.Errorf("auth.adminToken: status %d, want 200", code)
	}
	if code := get(both.URL, "Authorization", "Bearer sk-admin"); code != http.StatusOK {
		t.Errorf("pool key beside adminToken: status %d, want 200", code)
	}
	if code := get(both.URL, "Authorization", "Bearer secret"); code != http.StatusOK {
		t.Errorf("adminToken via Bearer: status %d, want 200", code)
	}
}

// The client key pool takes every spelling a provider's pool takes: an array,
// one whitespace- or newline-separated string, or a "file:" entry.
func TestAuthAPIKeysParsing(t *testing.T) {
	var cfg Config
	err := json.Unmarshal([]byte(`{"auth":{"apiKeys":"sk-one sk-two\nsk-three","token":"ignored-when-the-pool-is-set"}}`), &cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ApplyDefaults()
	keys, required := cfg.Auth.ClientKeys()
	if !required {
		t.Fatal("apiKeys set but ClientKeys reports no gate")
	}
	want := map[string]bool{"sk-one": true, "sk-two": true, "sk-three": true}
	if len(keys) != len(want) {
		t.Errorf("ClientKeys = %v, want %v", keys, want)
	}
	for _, k := range keys {
		if !want[k] {
			t.Errorf("unexpected key %q", k)
		}
	}

	// With neither token nor apiKeys there is no gate to speak of.
	var open Config
	if err := json.Unmarshal([]byte(`{"auth":{}}`), &open); err != nil {
		t.Fatal(err)
	}
	open.ApplyDefaults()
	if keys, required := open.Auth.ClientKeys(); required || keys != nil {
		t.Errorf("ClientKeys = %v,%v want no gate at all", keys, required)
	}
}
