package router

// One request is one entry in each rollup. The global rollup is counted by the
// middleware that wraps every handler; the model and provider split is counted
// by the forward path, the only place that knows them. When the forward path
// counted the global outcome too, every forwarded request appeared twice there
// while its model appeared once - so the totals disagreed with the split under
// them, and the uptime the page leads with was computed from a number that
// counted each forwarded request twice.

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"noderouter/config"
	"noderouter/logx"
	"noderouter/proxy"
)

// statusTestServer points one provider at a live upstream and one at an
// endpoint that accepts a connection and then says nothing, so both the
// answered and the failed exit from the forward path are reachable without a
// network. A closed port would be the shorter way to say "unreachable", but it
// is dead only by luck: the port can be handed to another listener before the
// request is made, and the test would pass or fail depending on what else is
// running. A silent listener is a failure on purpose.
func statusTestServer(t *testing.T, liveURL string) *Server {
	t.Helper()

	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { silent.Close() })
	go func() {
		// Accepted and held, never read or written: the client waits out the
		// timeout. They are kept referenced on purpose - a connection the
		// garbage collector can reach is a connection it will close.
		var held []net.Conn
		defer func() {
			for _, conn := range held {
				conn.Close()
			}
		}()
		for {
			conn, err := silent.Accept()
			if err != nil {
				return
			}
			held = append(held, conn)
		}
	}()

	cfg := &config.Config{}
	cfg.ApplyDefaults()
	cfg.Proxy.Enabled = config.BoolPtr(false)
	// One attempt and a one second upstream timeout: the failed case has to be
	// the router's own failure, not a retry sequence.
	cfg.Retry.MaxAttempts = 1
	cfg.Server.RequestSecs = 1
	cfg.Provider = map[string]*config.Provider{
		"P":      {Options: config.ProviderOptions{BaseURL: liveURL}, Models: map[string]*config.Model{"m": {}}},
		"Silent": {Options: config.ProviderOptions{BaseURL: "http://" + silent.Addr().String() + "/v1"}, Models: map[string]*config.Model{"s": {}}},
	}
	return NewServer(cfg, proxy.NewPool(filepath.Join(t.TempDir(), "none.txt"), "", "rotate", false), logx.New(false))
}

// waitForTotal polls the snapshot until the global total has caught up. The
// middleware records a request *after* its handler has finished, so a client
// that has read the response can be looking at a total that has not counted it
// yet; reading once is how a test becomes flaky rather than wrong.
func waitForTotal(t *testing.T, srv *Server, want int) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		snap := srv.StatusSnapshot()
		if got, _ := snap["total"].(int); got == want {
			return snap
		}
		if time.Now().After(deadline) {
			t.Fatalf("global total = %v, want %d after 5s", snap["total"], want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func postChat(t *testing.T, url, body string) int {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func TestForwardedRequestIsCountedOnce(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"1","object":"chat.completion","choices":[]}`)
	}))
	defer upstream.Close()

	srv := statusTestServer(t, upstream.URL)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	chat := ts.URL + "/v1/chat/completions"

	// Every request adds exactly one to the global total, whether it was
	// answered, failed upstream, or was refused before a provider was chosen.
	// The count after each is the count of requests so far, not twice it.
	for i, tc := range []struct {
		what string
		body string
		want int
	}{
		{"forwarded to the live provider", `{"model":"m","messages":[]}`, http.StatusOK},
		{"forwarded to an endpoint that never answers", `{"model":"s","messages":[]}`, http.StatusBadGateway},
		{"refused with an unknown model", `{"model":"nope","messages":[]}`, http.StatusBadRequest},
	} {
		if code := postChat(t, chat, tc.body); code != tc.want {
			t.Errorf("%s: status %d, want %d", tc.what, code, tc.want)
		}
		// One request, one entry: the total after each is the count of
		// requests so far, not twice it.
		waitForTotal(t, srv, i+1)
	}

	snap := srv.StatusSnapshot()
	if got := statusInt(t, snap, "failed"); got != 2 {
		t.Errorf("global failed = %d, want 2 (the silent endpoint and the refused model)", got)
	}

	// The detail rollups are the other half of the same requests, counted once
	// each - and the split under a model still adds up to that model's row.
	models := snap["modelStats"].([]map[string]any)
	m := statusEntryByName(t, models, "m")
	if got := statusInt(t, m, "total"); got != 1 {
		t.Errorf("model m total = %d, want 1", got)
	}
	if got := statusInt(t, m, "failed"); got != 0 {
		t.Errorf("model m failed = %d, want 0", got)
	}
	s := statusEntryByName(t, models, "s")
	if got := statusInt(t, s, "total"); got != 1 {
		t.Errorf("model s total = %d, want 1 (the request the silent endpoint failed)", got)
	}
	if got := statusInt(t, s, "failed"); got != 1 {
		t.Errorf("model s failed = %d, want 1", got)
	}
	split := s["providerStats"].([]map[string]any)
	if got := statusInt(t, statusEntryByName(t, split, "Silent"), "total"); got != 1 {
		t.Errorf("model s through Silent = %d, want 1", got)
	}

	providers := snap["providerStats"].([]map[string]any)
	if got := statusInt(t, statusEntryByName(t, providers, "P"), "total"); got != 1 {
		t.Errorf("provider P total = %d, want 1", got)
	}
	if got := statusInt(t, statusEntryByName(t, providers, "Silent"), "total"); got != 1 {
		t.Errorf("provider Silent total = %d, want 1", got)
	}
}

// A local endpoint records nothing but the global outcome, because there is no
// provider behind it - and it still records exactly one request, not none.
func TestLocalEndpointIsCountedOnceWithoutAModel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()

	srv := statusTestServer(t, upstream.URL)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/models = %d, want 200", resp.StatusCode)
	}

	// Discovery is a local control endpoint, not an LLM API request, so it is
	// intentionally absent from the public status totals.
	snap := srv.StatusSnapshot()
	if got := snap["total"].(int); got != 0 {
		t.Errorf("global total = %d, want 0 for /v1/models", got)
	}
	if got := len(snap["modelStats"].([]map[string]any)); got != 0 {
		t.Errorf("modelStats = %d entries, want none for a request that named no model", got)
	}
}

func TestStatusTracksOnlyLLMAPIRequests(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	srv := statusTestServer(t, upstream.URL)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, path := range []string{"/", "/status", "/noderouter/status", "/v1/models", "/v1/models/m", "/v1/status", "/v1/status/"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	if got := srv.StatusSnapshot()["total"].(int); got != 0 {
		t.Fatalf("global total after non-LLM routes = %d, want 0", got)
	}

	if code := postChat(t, ts.URL+"/chat/completions", `{"model":"m","messages":[]}`); code != http.StatusOK {
		t.Fatalf("POST /chat/completions = %d, want 200", code)
	}
	snap := srv.StatusSnapshot()
	if got := snap["total"].(int); got != 0 {
		t.Fatalf("global total after non-/v1 API request = %d, want 0", got)
	}
	if got := len(snap["modelStats"].([]map[string]any)); got != 0 {
		t.Fatalf("modelStats after non-/v1 API request = %d entries, want none", got)
	}

	if code := postChat(t, ts.URL+"/v1/chat/completions", `{"model":"m","messages":[]}`); code != http.StatusOK {
		t.Fatalf("POST /v1/chat/completions = %d, want 200", code)
	}
	waitForTotal(t, srv, 1)
}

func TestStatusSubpathsDoNotBecomeLocalStatusRoutes(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{MaxBodyMB: 1, RequestSecs: 5},
		Proxy:  config.ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt")},
		Provider: map[string]*config.Provider{
			"P": {Options: config.ProviderOptions{BaseURL: upstream.URL}, Models: map[string]*config.Model{"m": {}}},
		},
	}
	cfg.ApplyDefaults()
	cfg.Proxy.Enabled = config.BoolPtr(false)
	srv := NewServer(cfg, proxy.NewPool(cfg.Proxy.File, "", "rotate", false), logx.New(false))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/status/health")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/status/health = %d, want upstream 200", resp.StatusCode)
	}
	if gotPath != "/v1/status/health" {
		t.Fatalf("upstream path = %q, want /v1/status/health", gotPath)
	}
	if got := srv.StatusSnapshot()["total"].(int); got != 0 {
		t.Fatalf("global total after /v1/status/health = %d, want 0", got)
	}
}
