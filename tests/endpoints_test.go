package tests

// A provider used to have exactly one upstream: options.baseURL. It now has a
// pool, spelled and behaving the way apiKeys spells and behaves its own pool -
// same accepted forms, same three strategies, same "a retry moves on" rule -
// so a gateway with several mirrors load-spreads across them and fails over
// when one of them is having a moment.

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// endpointServer wires a router in front of the given upstream URLs,
// declared as one provider's endpoint pool, with the proxy pool off so only
// the endpoint path is under test. tune runs before the defaults are applied,
// so a test can change the strategy, the retry budget or the endpoint health
// policy.
func endpointServer(t *testing.T, strategy string, urls []string, tune func(*Config)) (*httptest.Server, *Server) {
	t.Helper()
	cfg := &Config{
		Server: ServerConfig{MaxBodyMB: 1, RequestSecs: 5},
		Proxy:  ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt"), Enabled: boolPtr(false)},
		Retry:  RetryConfig{MaxAttempts: 10, BackoffMS: -1}, // backoff off: the test is about which endpoint, not when
		Provider: map[string]*Provider{"P": {
			Options: ProviderOptions{BaseURLs: keyList(urls), EndpointStrategy: strategy},
			Models:  map[string]*Model{"m": {}},
		}},
	}
	if tune != nil {
		tune(cfg)
	}
	cfg.ApplyDefaults()
	srv := NewServer(cfg, NewPool(cfg.Proxy.File, "", "rotate", false), newLogger(false))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, srv
}

// endpointRouter is the common case: a pool of the given upstreams under one
// strategy, with the retry budget the endpoint tests use.
func endpointRouter(t *testing.T, strategy string, handlers ...http.HandlerFunc) *httptest.Server {
	t.Helper()
	urls := make([]string, 0, len(handlers))
	for _, h := range handlers {
		u := httptest.NewServer(h)
		t.Cleanup(u.Close)
		urls = append(urls, u.URL)
	}
	ts, _ := endpointServer(t, strategy, urls, nil)
	return ts
}

// toggleableUpstream is an endpoint that can be switched off and on again at
// the same address: the listener goes away while it is down - so connections
// are refused, the way a dead endpoint refuses them - and comes back on the
// same port when it is up again, which is what lets a benched endpoint be
// probed back into the pool.
type toggleableUpstream struct {
	t    *testing.T
	addr string
	h    http.Handler

	mu  sync.Mutex
	srv *http.Server
}

func newToggleableUpstream(t *testing.T, h http.Handler) *toggleableUpstream {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	u := &toggleableUpstream{t: t, addr: ln.Addr().String(), h: h}
	u.serve(ln)
	t.Cleanup(u.down)
	return u
}

func (u *toggleableUpstream) serve(ln net.Listener) {
	u.mu.Lock()
	defer u.mu.Unlock()
	srv := &http.Server{Handler: u.h}
	u.srv = srv
	// Captured, not read off u: down() nils the field, and a goroutine that
	// dereferenced it would be a nil pointer rather than a closed listener.
	go func() { _ = srv.Serve(ln) }() // down() closes the listener; that error is the point
}

func (u *toggleableUpstream) url() string { return "http://" + u.addr + "/v1" }

func (u *toggleableUpstream) down() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.srv != nil {
		_ = u.srv.Close() // the listener and every connection on it
		u.srv = nil
	}
}

func (u *toggleableUpstream) up() {
	u.mu.Lock()
	if u.srv != nil {
		u.mu.Unlock()
		return
	}
	u.mu.Unlock()
	ln, err := net.Listen("tcp", u.addr)
	if err != nil {
		u.t.Fatalf("re-listen on %s: %v", u.addr, err)
	}
	u.serve(ln)
}

// statusJSON reads /noderouter/status the way an operator does.
func statusJSON(t *testing.T, ts *httptest.Server, token string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/noderouter/status", nil)
	if token != "" {
		req.Header.Set("X-Noderouter-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	return out
}

// providerRow finds one provider's row in a status answer.
func providerRow(t *testing.T, status map[string]any, name string) map[string]any {
	t.Helper()
	rows, _ := status["providers"].([]any)
	for _, row := range rows {
		if r, ok := row.(map[string]any); ok && r["name"] == name {
			return r
		}
	}
	t.Fatalf("no provider %q in %v", name, status["providers"])
	return nil
}

// countingHandler answers 200, recording every hit, except that failFirst
// turns the first hit on that endpoint into a 503 - the shape of an endpoint
// that is down for a moment and then recovers.
func countingHandler(hits *atomic.Int64, failFirst bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if failFirst && n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `{"error":{"message":"endpoint down"}}`)
			return
		}
		io.WriteString(w, `{"ok":true}`)
	}
}

func TestEndpointPoolTakesEveryKeyPoolShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want []string
	}{
		{"array", `{"baseURLs": ["http://a/v1", "http://b/v1"]}`,
			[]string{"http://a/v1", "http://b/v1"}},
		{"one string, spaces", `{"baseURLs": "http://a/v1   http://b/v1"}`,
			[]string{"http://a/v1", "http://b/v1"}},
		{"one string, lines and comments", "{\"baseURLs\": \"# mirrors\\nhttp://a/v1\\n\\nhttp://b/v1\\n\"}",
			[]string{"http://a/v1", "http://b/v1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var opts ProviderOptions
			if err := json.Unmarshal([]byte(tc.raw), &opts); err != nil {
				t.Fatalf("unmarshal %s: %v", tc.raw, err)
			}
			cfg := &Config{Provider: map[string]*Provider{"P": {Options: opts}}}
			cfg.ApplyDefaults()
			pool := cfg.Provider["P"].EndpointPool()
			if len(pool) != len(tc.want) {
				t.Fatalf("pool = %v, want %v", pool, tc.want)
			}
			for i := range pool {
				if pool[i] != tc.want[i] {
					t.Errorf("pool[%d] = %q, want %q", i, pool[i], tc.want[i])
				}
			}
		})
	}
}

func TestEndpointPoolReadsAFileOfEndpoints(t *testing.T) {
	path := filepath.Join(t.TempDir(), "endpoints.txt")
	if err := os.WriteFile(path, []byte("# mirrors\nhttp://a/v1\n\nhttp://b/v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Provider: map[string]*Provider{"P": {
		Options: ProviderOptions{BaseURLs: keyList{"file:" + path}},
	}}}
	cfg.ApplyDefaults()
	p := cfg.Provider["P"]

	pool := p.EndpointPool()
	if len(pool) != 2 || pool[0] != "http://a/v1" || pool[1] != "http://b/v1" {
		t.Fatalf("pool = %v, want the two endpoints from the file", pool)
	}
	if got := p.BaseURLFor(0, 0); got != "http://a/v1" {
		t.Errorf("BaseURLFor(0, 0) = %q, want the file's first endpoint", got)
	}
}

func TestEndpointPoolPrefersBaseURLsAndFallsBackWhenEmpty(t *testing.T) {
	// baseURLs wins over baseURL, the way apiKeys wins over apiKey.
	wins := &Config{Provider: map[string]*Provider{"P": {
		Options: ProviderOptions{BaseURL: "http://single/v1", BaseURLs: keyList{"http://pool/v1"}},
	}}}
	wins.ApplyDefaults()
	if pool := wins.Provider["P"].EndpointPool(); len(pool) != 1 || pool[0] != "http://pool/v1" {
		t.Errorf("pool = %v, want the baseURLs entry to win", pool)
	}

	// A pool that resolves to nothing falls back to baseURL rather than
	// leaving the provider with nowhere to send a request.
	missing := &Config{Provider: map[string]*Provider{"P": {
		Options: ProviderOptions{BaseURL: "http://single/v1",
			BaseURLs: keyList{"file:" + filepath.Join(t.TempDir(), "absent.txt")}},
	}}}
	missing.ApplyDefaults()
	if pool := missing.Provider["P"].EndpointPool(); len(pool) != 1 || pool[0] != "http://single/v1" {
		t.Errorf("pool = %v, want the baseURL fallback", pool)
	}

	// And a provider configured with nothing at all answers "" so the
	// request is refused instead of forwarded nowhere.
	none := &Config{Provider: map[string]*Provider{"P": {}}}
	none.ApplyDefaults()
	if got := none.Provider["P"].BaseURLFor(0, 0); got != "" {
		t.Errorf("BaseURLFor(0, 0) = %q, want empty for a provider with no endpoint", got)
	}
}

func TestEndpointStrategy(t *testing.T) {
	pool := keyList{"http://a/v1", "http://b/v1", "http://c/v1"}
	newProvider := func(strategy string) *Provider {
		cfg := &Config{Provider: map[string]*Provider{"P": {
			Options: ProviderOptions{BaseURLs: pool, EndpointStrategy: strategy},
		}}}
		cfg.ApplyDefaults()
		return cfg.Provider["P"]
	}

	// Rotate walks the pool in order and wraps, one place per request, with
	// the retry taking the one after it.
	p := newProvider("")
	if got, want := p.Options.EndpointStrategy, endpointStrategyRotate; got != want {
		t.Errorf("unset strategy settled as %q, want %q", got, want)
	}
	for i, want := range []string{"http://a/v1", "http://b/v1", "http://c/v1", "http://a/v1"} {
		if got := p.BaseURLFor(i, 0); got != want {
			t.Errorf("rotate BaseURLFor(%d, 0) = %q, want %q", i, got, want)
		}
	}
	if got, want := p.BaseURLFor(1, 1), "http://c/v1"; got != want {
		t.Errorf("rotate retry BaseURLFor(1, 1) = %q, want %q (the retry walks on)", got, want)
	}

	// "roundrobin" is accepted as a spelling of rotate, the way proxy.mode
	// accepts it; anything else unrecognised falls back to rotate instead of
	// failing the config.
	if got := newProvider("roundrobin").Options.EndpointStrategy; got != endpointStrategyRotate {
		t.Errorf("roundrobin settled as %q, want %q", got, endpointStrategyRotate)
	}
	if got := newProvider("sideways").Options.EndpointStrategy; got != endpointStrategyRotate {
		t.Errorf("unrecognised strategy settled as %q, want %q", got, endpointStrategyRotate)
	}

	// primary always starts at the first endpoint and only moves when an
	// attempt fails: the request sequence number does not shift it.
	primary := newProvider("primary")
	for attempt, want := range []string{"http://a/v1", "http://b/v1", "http://c/v1", "http://a/v1"} {
		if got := primary.BaseURLFor(99, attempt); got != want {
			t.Errorf("primary BaseURLFor(99, %d) = %q, want %q", attempt, got, want)
		}
	}

	// first pins every request - and every retry - to the first endpoint.
	first := newProvider("first")
	for seq := 0; seq < 6; seq++ {
		if got := first.BaseURLFor(seq, seq); got != "http://a/v1" {
			t.Fatalf("first BaseURLFor(%d, %d) = %q, want the first endpoint", seq, seq, got)
		}
	}

	// random stays inside the pool.
	random := newProvider("random")
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		got := random.BaseURLFor(i, 0)
		if !strings.HasPrefix(got, "http://") {
			t.Fatalf("random BaseURLFor(%d, 0) = %q, not an endpoint", i, got)
		}
		seen[got] = true
	}
	if len(seen) == 0 {
		t.Error("random produced nothing")
	}
}

func TestRouterRotatesRequestsAcrossEndpoints(t *testing.T) {
	var a, b atomic.Int64
	ts := endpointRouter(t, "",
		countingHandler(&a, false),
		countingHandler(&b, false),
	)

	for i := 0; i < 2; i++ {
		if code, body := postForward(t, ts); code != http.StatusOK || !strings.Contains(body, `"ok":true`) {
			t.Fatalf("request %d: status %d body %s, want 200 from an endpoint", i+1, code, body)
		}
	}
	if a.Load() != 1 || b.Load() != 1 {
		t.Errorf("endpoint hits = %d/%d, want 1/1: two requests spread over two endpoints", a.Load(), b.Load())
	}
}

func TestRouterRetriesOnTheNextEndpoint(t *testing.T) {
	// Every endpoint refuses its own first hit and recovers after that, so
	// the only way the request can be answered is for the retry to leave the
	// endpoint that just failed and come back to it later.
	var a, b atomic.Int64
	ts := endpointRouter(t, "",
		countingHandler(&a, true),
		countingHandler(&b, true),
	)

	code, body := postForward(t, ts)
	if code != http.StatusOK || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("status %d body %s, want the request recovered by another endpoint", code, body)
	}
	if a.Load() == 0 || b.Load() == 0 {
		t.Errorf("endpoint hits = %d/%d, want both: a retry must move to the other endpoint", a.Load(), b.Load())
	}
}

func TestEndpointStrategyFirstNeverLeavesTheFirstEndpoint(t *testing.T) {
	// "first" is the strategy for a primary endpoint with spares nobody is
	// meant to touch, so a failing primary is retried in place rather than
	// spending the spares.
	var a, b atomic.Int64
	ts := endpointRouter(t, "first",
		countingHandler(&a, true),
		countingHandler(&b, false),
	)

	code, body := postForward(t, ts)
	if code != http.StatusOK || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("status %d body %s, want the primary to recover on a retry", code, body)
	}
	if a.Load() != 2 {
		t.Errorf("primary hits = %d, want 2: the retry stays on it", a.Load())
	}
	if b.Load() != 0 {
		t.Errorf("spare hits = %d, want 0: \"first\" must not spend its spares", b.Load())
	}
}

func TestStatusReportsTheEndpointPool(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{MaxBodyMB: 1, RequestSecs: 5},
		Proxy:  ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt")},
		Auth:   AuthConfig{AdminToken: "secret"},
		Provider: map[string]*Provider{
			"Pooled": {Options: ProviderOptions{
				BaseURLs:         keyList{"http://a/v1", "http://b/v1"},
				EndpointStrategy: "first",
			}, Models: map[string]*Model{"m": {}}},
			"Single": {Options: ProviderOptions{BaseURL: "http://c/v1"}, Models: map[string]*Model{"m": {}}},
		},
	}
	cfg.ApplyDefaults()
	srv := NewServer(cfg, NewPool(cfg.Proxy.File, "", "rotate", false), newLogger(false))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/noderouter/status", nil)
	req.Header.Set("X-Noderouter-Token", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Providers []map[string]any `json:"providers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode status: %v", err)
	}

	byName := map[string]map[string]any{}
	for _, row := range out.Providers {
		name, _ := row["name"].(string)
		byName[name] = row
	}

	pooled := byName["Pooled"]
	if pooled == nil {
		t.Fatalf("providers = %+v, want a Pooled row", out.Providers)
	}
	if got, want := pooled["baseURL"], "http://a/v1"; got != want {
		t.Errorf("baseURL = %v, want %v (the endpoint a request starts on)", got, want)
	}
	urls, ok := pooled["baseURLs"].([]any)
	if !ok {
		t.Fatalf("baseURLs = %v, want a list of every endpoint", pooled["baseURLs"])
	}
	if len(urls) != 2 || urls[0] != "http://a/v1" || urls[1] != "http://b/v1" {
		t.Errorf("baseURLs = %v, want both endpoints in pool order", urls)
	}
	if got, want := pooled["endpointStrategy"], "first"; got != want {
		t.Errorf("endpointStrategy = %v, want %v", got, want)
	}

	single := byName["Single"]
	if single == nil {
		t.Fatalf("providers = %+v, want a Single row", out.Providers)
	}
	if got, want := single["baseURL"], "http://c/v1"; got != want {
		t.Errorf("baseURL = %v, want %v", got, want)
	}
	if _, ok := single["baseURLs"]; ok {
		t.Errorf("a single-endpoint provider reported a pool: %v", single)
	}
}

// okHandler answers 200 and counts the hits, the shape of an endpoint that is
// up and staying up.
func okHandler(hits *atomic.Int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.WriteString(w, `{"ok":true}`)
	}
}

// A dead endpoint is benched out of the pool after three failures, so the
// requests stop meeting it - and a probe brings it back as soon as it answers
// again, without waiting out the cooldown.
func TestDeadEndpointIsBenchedAndProbedBack(t *testing.T) {
	var aHits, bHits atomic.Int64
	a := newToggleableUpstream(t, okHandler(&aHits))
	b := newToggleableUpstream(t, okHandler(&bHits))

	// primary + one attempt per request: every request starts at the first
	// endpoint, so the dead one is exactly what each request meets until it
	// is benched. That makes the switch to the spare visible.
	ts, srv := endpointServer(t, "primary", []string{a.url(), b.url()}, func(c *Config) {
		c.Retry.MaxAttempts = 1
	})

	if code, body := postForward(t, ts); code != http.StatusOK {
		t.Fatalf("first request: status %d body %s, want 200 while both endpoints are up", code, body)
	}
	if aHits.Load() != 1 || bHits.Load() != 0 {
		t.Fatalf("hits = %d/%d, want the primary to have served the first request", aHits.Load(), bHits.Load())
	}

	a.down()
	for i := 0; i < 3; i++ { // endpoints.failThreshold defaults to 3
		if code, _ := postForward(t, ts); code != http.StatusBadGateway {
			t.Fatalf("request %d against the dead endpoint: status %d, want 502", i+1, code)
		}
	}

	code, body := postForward(t, ts)
	if code != http.StatusOK || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("after benching: status %d body %s, want the spare to serve it", code, body)
	}
	if bHits.Load() == 0 {
		t.Fatal("the spare never saw a request, so the benched primary is still being tried")
	}

	// The status answer says what is in force and which endpoint is out.
	status := statusJSON(t, ts, srv.DynamicAdminToken())
	health, _ := status["endpoints"].(map[string]any)
	if health == nil {
		t.Fatalf("status carries no endpoint health: %v", status)
	}
	for _, tc := range []struct {
		key  string
		want float64
	}{{"failThreshold", 3}, {"benchSecs", 60}, {"recheckSecs", 60}, {"benched", 1}} {
		if got := health[tc.key]; got != tc.want {
			t.Errorf("endpoints.%s = %v, want %v", tc.key, got, tc.want)
		}
	}
	if benched := providerRow(t, status, "P")["benchedEndpoints"]; benched == nil {
		t.Error("provider row does not name its benched endpoint")
	} else if urls, _ := benched.([]any); len(urls) != 1 || urls[0] != a.url() {
		t.Errorf("benchedEndpoints = %v, want just %s", urls, a.url())
	}

	// The endpoint comes back the moment a probe finds it answering, and the
	// primary strategy starts at it again.
	a.up()
	if restored := srv.RecheckEndpoints(context.Background()); restored != 1 {
		t.Fatalf("RecheckEndpoints restored %d endpoints, want 1 now that the endpoint answers", restored)
	}
	hitsBefore := aHits.Load()
	if code, body := postForward(t, ts); code != http.StatusOK {
		t.Fatalf("after the probe: status %d body %s, want 200 from the restored endpoint", code, body)
	}
	if aHits.Load() == hitsBefore {
		t.Error("the restored endpoint served no request, so it never went back in the pool")
	}
}

// A bench is not forever: with no probe to lift it, it expires on its own and
// the endpoint is tried again.
func TestBenchedEndpointReturnsWhenTheBenchExpires(t *testing.T) {
	var aHits, bHits atomic.Int64
	a := newToggleableUpstream(t, okHandler(&aHits))
	b := newToggleableUpstream(t, okHandler(&bHits))

	ts, _ := endpointServer(t, "primary", []string{a.url(), b.url()}, func(c *Config) {
		c.Retry.MaxAttempts = 1
		c.Endpoints.BenchSecs = 1
	})

	a.down()
	for i := 0; i < 3; i++ {
		if code, _ := postForward(t, ts); code != http.StatusBadGateway {
			t.Fatalf("request %d: status %d, want 502 while the primary is dead", i+1, code)
		}
	}
	if code, _ := postForward(t, ts); code != http.StatusOK {
		t.Fatalf("benched primary: status %d, want the spare to serve it", code)
	}

	time.Sleep(1300 * time.Millisecond) // outlast the one-second bench

	// The primary is back in the pool - and still dead - so the request fails
	// again. That failure is the proof it was tried.
	if code, _ := postForward(t, ts); code != http.StatusBadGateway {
		t.Fatalf("after the bench expired: status %d, want the endpoint to be tried again (and fail)", code)
	}
}

// endpoints.failThreshold below zero turns benching off: the dead endpoint
// stays in the pool, and every request on the primary strategy meets it.
func TestEndpointBenchingCanBeDisabled(t *testing.T) {
	a := newToggleableUpstream(t, okHandler(&atomic.Int64{}))
	b := newToggleableUpstream(t, okHandler(&atomic.Int64{}))

	ts, srv := endpointServer(t, "primary", []string{a.url(), b.url()}, func(c *Config) {
		c.Retry.MaxAttempts = 1
		c.Endpoints.FailThreshold = -1
	})

	a.down()
	for i := 0; i < 6; i++ {
		if code, _ := postForward(t, ts); code != http.StatusBadGateway {
			t.Fatalf("request %d: status %d, want 502: with benching off the dead endpoint is never skipped", i+1, code)
		}
	}
	// The status answer reports the policy as off rather than hiding it.
	health, _ := statusJSON(t, ts, srv.DynamicAdminToken())["endpoints"].(map[string]any)
	if health == nil {
		t.Fatal("status carries no endpoint health")
	}
	if got := health["failThreshold"]; got != float64(0) {
		t.Errorf("endpoints.failThreshold = %v, want 0 (benching off)", got)
	}
	if got := health["benched"]; got != float64(0) {
		t.Errorf("endpoints.benched = %v, want 0: nothing is benched when the feature is off", got)
	}
}

// When every endpoint of a provider is benched the pool is handed back whole:
// a request that is refused outright can never be served, while trying is the
// only way to learn that one of them came back.
func TestAllEndpointsBenchedStillRoutes(t *testing.T) {
	a := newToggleableUpstream(t, okHandler(&atomic.Int64{}))
	b := newToggleableUpstream(t, okHandler(&atomic.Int64{}))

	ts, srv := endpointServer(t, "primary", []string{a.url(), b.url()}, func(c *Config) {
		c.Retry.MaxAttempts = 1
		c.Endpoints.FailThreshold = 1 // bench on the first failure
	})

	a.down()
	b.down()

	// First request benches A, second benches B, third finds both benched -
	// and still goes out, refusing the connection rather than the request.
	for i := 0; i < 3; i++ {
		code, body := postForward(t, ts)
		if code != http.StatusBadGateway {
			t.Fatalf("request %d: status %d body %s, want 502", i+1, code, body)
		}
		if !strings.Contains(body, "connection refused") {
			t.Fatalf("request %d: body %s, want the router to have tried an endpoint", i+1, body)
		}
		if strings.Contains(body, "has no baseURL") {
			t.Fatalf("request %d: body %s, want the benched pool to still be used", i+1, body)
		}
	}

	status := statusJSON(t, ts, srv.DynamicAdminToken())
	if benched, _ := status["endpoints"].(map[string]any); benched == nil || benched["benched"] != float64(2) {
		t.Errorf("endpoints.benched = %v, want 2: both endpoints are out", benched)
	}
}

// POST /noderouter/refresh re-checks the benched endpoints too, so an operator
// who fixed an upstream does not have to wait out the probe interval.
func TestRefreshProbesBenchedEndpoints(t *testing.T) {
	var aHits, bHits atomic.Int64
	a := newToggleableUpstream(t, okHandler(&aHits))
	b := newToggleableUpstream(t, okHandler(&bHits))

	ts, _ := endpointServer(t, "primary", []string{a.url(), b.url()}, func(c *Config) {
		c.Retry.MaxAttempts = 1
		c.Auth.AdminToken = "secret"
	}) // OnRefresh stays nil: nothing proxies here, the endpoint probe is all the refreshing there is

	a.down()
	for i := 0; i < 3; i++ {
		if code, _ := postForward(t, ts); code != http.StatusBadGateway {
			t.Fatalf("request %d: status %d, want 502 while the primary is dead", i+1, code)
		}
	}

	// The endpoint is fixed. The refresh route must find that out for itself.
	a.up()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/noderouter/refresh", nil)
	req.Header.Set("X-Noderouter-Token", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("refresh = %d, want 200: the endpoint probe is refresh work even without proxies", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode refresh: %v", err)
	}
	if got := out["endpointsRestored"]; got != float64(1) {
		t.Errorf("endpointsRestored = %v, want 1", got)
	}

	if code, body := postForward(t, ts); code != http.StatusOK {
		t.Fatalf("after refresh: status %d body %s, want the restored endpoint to serve it", code, body)
	}
	if aHits.Load() == 0 {
		t.Error("the restored endpoint served no request, so the refresh did not put it back")
	}
}

// The endpoint health policy takes the shapes the rest of the config takes:
// unset is the default, negative turns a layer off.
func TestEndpointHealthConfigSettles(t *testing.T) {
	defaults := EndpointHealthConfig{}
	if got, want := defaults.Threshold(), 3; got != want {
		t.Errorf("default threshold = %d, want %d", got, want)
	}
	if got, want := defaults.Bench(), 60*time.Second; got != want {
		t.Errorf("default bench = %s, want %s", got, want)
	}
	if got, want := defaults.RecheckEvery(), 60*time.Second; got != want {
		t.Errorf("default recheck = %s, want %s", got, want)
	}

	off := EndpointHealthConfig{FailThreshold: -1, RecheckSecs: -1}
	if got := off.Threshold(); got != 0 {
		t.Errorf("negative threshold = %d, want 0 (benching off)", got)
	}
	if got := off.RecheckEvery(); got != 0 {
		t.Errorf("negative recheckSecs = %s, want 0 (no probes)", got)
	}
	if got, want := off.Bench(), 60*time.Second; got != want {
		t.Errorf("negative benchSecs = %s, want the default %s", got, want)
	}
}
