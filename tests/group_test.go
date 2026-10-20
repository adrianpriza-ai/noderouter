package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// twoProviders returns a config where p1 and p2 both serve a model that
// joined the group "g", and the models they serve differ in kind so the
// representative entry stays deterministic.
func groupConfig(up1, up2 string) *Config {
	cfg := &Config{
		Server: ServerConfig{MaxBodyMB: 1, RequestSecs: 5},
		Proxy:  ProxyConfig{File: filepath.Join("/tmp", "none.txt"), Enabled: boolPtr(false)},
		Retry:  RetryConfig{MaxAttempts: 4, BackoffMS: 1, MaxBackoffMS: 5},
		Provider: map[string]*Provider{
			"p1": {Options: ProviderOptions{BaseURL: up1 + "/v1"}, Models: map[string]*Model{
				"model-a": {Group: "g"},
			}},
			"p2": {Options: ProviderOptions{BaseURL: up2 + "/v1"}, Models: map[string]*Model{
				"model-b": {Group: "g"},
			}},
		},
	}
	cfg.ApplyDefaults()
	return cfg
}

func TestModelTableUnifiesGroup(t *testing.T) {
	cfg := groupConfig("http://one", "http://two")
	table := cfg.ModelTable()
	if len(table) != 1 || table[0].Call != "g" {
		var calls []string
		for _, e := range table {
			calls = append(calls, e.Call)
		}
		t.Fatalf("ModelTable = %v, want one entry \"g\"", calls)
	}
	if len(table[0].Backends) != 2 {
		t.Fatalf("backends = %d, want 2", len(table[0].Backends))
	}
	backends, err := cfg.ResolveBackends("g")
	if err != nil {
		t.Fatal(err)
	}
	if len(backends) != 2 || backends[0].Upstream != "model-a" || backends[1].Upstream != "model-b" {
		t.Fatalf("ResolveBackends = %+v", backends)
	}
	p, upstream, err := cfg.ResolveModel("g")
	if err != nil || p == nil || upstream != "model-a" {
		t.Fatalf("ResolveModel = %v, %q, %v", p, upstream, err)
	}
	if got := cfg.ModelBackends("g"); fmt.Sprint(got) != "[p1 p2]" {
		t.Fatalf("ModelBackends = %v", got)
	}
}

func TestHiddenModelsAndProviders(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{MaxBodyMB: 1},
		Proxy:  ProxyConfig{File: filepath.Join("/tmp", "none.txt"), Enabled: boolPtr(false)},
		Provider: map[string]*Provider{
			"shown": {Options: ProviderOptions{BaseURL: "http://x/v1"}, Models: map[string]*Model{
				"visible": {},
				"secret":  {Hidden: true},
			}},
			"ghost": {Hidden: true, Options: ProviderOptions{BaseURL: "http://y/v1"}, Models: map[string]*Model{
				"owned": {Group: "g"},
			}},
		},
	}
	cfg.ApplyDefaults()
	for _, e := range cfg.ModelTable() {
		if e.Provider.Name == "ghost" || e.Key == "secret" {
			t.Fatalf("hidden entry leaked into table: %+v", e)
		}
	}
	if _, err := cfg.ResolveBackends("g"); err == nil {
		t.Fatal("group with only a hidden member should not resolve")
	}
	// Still reachable by exact key, just not advertised.
	if _, _, err := cfg.ResolveModel("owned"); err != nil {
		t.Fatalf("hidden model should still resolve by key: %v", err)
	}
}

// TestUnifiedRoundRobinAndFailover: two upstreams answer for the same
// group id. With both healthy the requests alternate between them; once
// one dies, the retry walks to the other.
func TestUnifiedRoundRobinAndFailover(t *testing.T) {
	var countA, countB atomic.Int32
	mux := func(name string, counter *atomic.Int32, status int) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			counter.Add(1)
			if status != 200 {
				w.WriteHeader(status)
				fmt.Fprint(w, `{"error":"down"}`)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"cmpl-1","object":"chat.completion","choices":[{"message":{"role":"assistant","content":%q}}]}`, name)
		})
	}
	upA := httptest.NewServer(mux("a", &countA, 200))
	defer upA.Close()
	upB := httptest.NewServer(mux("b", &countB, 200))
	defer upB.Close()

	cfg := groupConfig(upA.URL, upB.URL)
	srv := NewServer(cfg, NewPool(cfg.Proxy.File, "", "rotate", false), newLogger(false))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	post := func() string {
		body := bytes.NewBufferString(`{"model":"g","messages":[{"role":"user","content":"hi"}]}`)
		resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", body)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("status %d: %s", resp.StatusCode, raw)
		}
		return string(raw)
	}
	post()
	post()
	if a, b := countA.Load(), countB.Load(); a != 1 || b != 1 {
		t.Fatalf("request split a=%d b=%d, want 1/1", a, b)
	}

	// Kill B: the next request must fail over to A even when the rotation
	// would have started there.
	upB.Close()
	countA.Store(0)
	// One request lands on p1 directly, the next must start at the dead
	// p2 and fail over to p1.
	post()
	post()
	if countA.Load() != 2 {
		t.Fatalf("failover: a=%d, want 2 requests served by a", countA.Load())
	}
}

// TestGroupListedOnce verifies /v1/models advertises the group under one
// entry, with all of its endpoints named.
func TestGroupListedOnce(t *testing.T) {
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upA.Close()
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upB.Close()
	cfg := groupConfig(upA.URL, upB.URL)
	srv := NewServer(cfg, NewPool(cfg.Proxy.File, "", "rotate", false), newLogger(false))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list struct {
		Data []struct {
			ID       string   `json:"id"`
			Backends []string `json:"backends"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list.Data) != 1 || list.Data[0].ID != "g" {
		t.Fatalf("data = %+v", list.Data)
	}
	if fmt.Sprint(list.Data[0].Backends) != "[p1 p2]" {
		t.Fatalf("backends = %v", list.Data[0].Backends)
	}
}

// TestModelTableGroupRowsAreStable checks what ModelTable's doc comment
// promises: a stable order. The group rows were appended while ranging a map,
// so two builds of the same config could list them differently - and row order
// is what decides who wins an id clash further down.
func TestModelTableGroupRowsAreStable(t *testing.T) {
	build := func() []string {
		cfg := &Config{
			Server: ServerConfig{MaxBodyMB: 1},
			Proxy:  ProxyConfig{File: filepath.Join("/tmp", "none.txt"), Enabled: boolPtr(false)},
			Provider: map[string]*Provider{
				"p1": {Options: ProviderOptions{BaseURL: "http://x/v1"}, Models: map[string]*Model{
					"zeta-a":  {Group: "zeta"},
					"alpha-a": {Group: "alpha"},
					"mid-a":   {Group: "mid"},
					"beta-a":  {Group: "beta"},
					"bare":    {},
				}},
			},
		}
		cfg.ApplyDefaults()
		var calls []string
		for _, e := range cfg.ModelTable() {
			calls = append(calls, e.Call)
		}
		return calls
	}

	first := build()
	// Bare rows first (by key), then the group rows by name.
	if got, want := fmt.Sprint(first), "[bare alpha beta mid zeta]"; got != want {
		t.Fatalf("ModelTable order = %s, want %s", got, want)
	}
	for i := 0; i < 20; i++ {
		if got := fmt.Sprint(build()); got != fmt.Sprint(first) {
			t.Fatalf("build %d: ModelTable order = %s, want the same order as the first build %s", i, got, first)
		}
	}
}
