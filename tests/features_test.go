package tests

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// TestModelKindInference checks that a model's kind is honoured when set and
// inferred from its output modalities otherwise, with anything unrecognised
// falling back to chat rather than failing the config.
func TestModelKindInference(t *testing.T) {
	cfg := &Config{Provider: map[string]*Provider{
		"P": {Options: ProviderOptions{BaseURL: "http://x/v1"}, Models: map[string]*Model{
			"chat-1":  {},
			"img-1":   {Modalities: Modalities{Output: []string{"image"}}},
			"audio-1": {Modalities: Modalities{Output: []string{"text", "audio"}}},
			"vid-1":   {Modalities: Modalities{Output: []string{"video"}}},
			"jev":     {Kind: "system-one"},
			"embed":   {Kind: "embeddings"},
			"doc":     {Kind: "document-parsing"}, // an accepted OCR spelling
			"mod":     {Kind: "safety"},           // an accepted moderation spelling
			"say":     {Kind: "text-to-speech"},   // an accepted TTS spelling
			"hear":    {Kind: "whisper"},          // an accepted STT spelling
			"weird":   {Kind: "not-a-kind"},
		}},
	}}
	cfg.ApplyDefaults()

	want := map[string]string{
		"chat-1":  kindChat,
		"img-1":   kindImage,
		"audio-1": kindAudio,
		"vid-1":   kindVideo,
		"jev":     kindSystemOne,
		"embed":   kindEmbedding,
		"doc":     kindOCR,
		"mod":     kindModeration,
		"say":     kindTTS,
		"hear":    kindSTT,
		"weird":   kindChat,
	}
	for id, w := range want {
		if got := cfg.Provider["P"].Models[id].Kind; got != w {
			t.Errorf("model %q kind = %q, want %q", id, got, w)
		}
	}
}

func TestModelKindsSupportMultipleCapabilities(t *testing.T) {
	cfg := &Config{Provider: map[string]*Provider{
		"Images": {Models: map[string]*Model{
			"explicit":    {Kind: "image", Kinds: []string{"image-edit"}},
			"duplicate":   {Kind: "image", Kinds: []string{"image", "image-edit"}},
			"plural-only": {Kinds: []string{"image-edit", "image"}},
		}},
	}}
	cfg.ApplyDefaults()

	want := map[string]struct {
		primary, additional, resolved string
	}{
		"explicit":    {kindImage, "image_edit", "image,image_edit"},
		"duplicate":   {kindImage, "image_edit", "image,image_edit"},
		"plural-only": {kindImageEdit, kindImage, "image_edit,image"},
	}
	for id, w := range want {
		model := cfg.Provider["Images"].Models[id]
		if model.Kind != w.primary {
			t.Errorf("%s primary kind = %q, want %q", id, model.Kind, w.primary)
		}
		if got := strings.Join(model.Kinds, ","); got != w.additional {
			t.Errorf("%s additional kinds = %q, want %q", id, got, w.additional)
		}
		if got := strings.Join(model.ResolveKinds(), ","); got != w.resolved {
			t.Errorf("%s resolved kinds = %q, want %q", id, got, w.resolved)
		}
		if !model.TakesMedia() {
			t.Errorf("%s image/edit model does not count as media-bearing", id)
		}
	}
}

// TestModelsListCarriesKindAndDialect checks that /v1/models advertises the
// kind of every model and flags a non-default upstream dialect, so a client
// can tell a chat model from an image model and a Messages-API provider from
// an OpenAI one.
func TestModelsListCarriesKindAndDialect(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{MaxBodyMB: 1, RequestSecs: 5},
		Proxy:  ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt"), Enabled: boolPtr(false)},
		Provider: map[string]*Provider{
			"Anthropic": {Options: ProviderOptions{BaseURL: "http://x/v1", API: "anthropic"},
				Models: map[string]*Model{"claude": {}}},
			"Images": {Options: ProviderOptions{BaseURL: "http://y/v1"},
				Models: map[string]*Model{"gpt-image-1": {Kind: "image"}}},
		},
	}
	cfg.ApplyDefaults()
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
			ID   string `json:"id"`
			Kind string `json:"kind"`
			API  string `json:"api"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}

	byID := map[string]struct{ kind, api string }{}
	for _, c := range list.Data {
		byID[c.ID] = struct{ kind, api string }{c.Kind, c.API}
	}
	if got := byID["claude"]; got.kind != kindChat || got.api != "anthropic" {
		t.Errorf("claude = %+v, want kind chat and api anthropic", got)
	}
	if got := byID["gpt-image-1"]; got.kind != kindImage || got.api != "" {
		t.Errorf("gpt-image-1 = %+v, want kind image and no api", got)
	}
}

// TestAnthropicDialectAuth checks that an "anthropic" provider is called with
// x-api-key and anthropic-version instead of a bearer Authorization header.
func TestAnthropicDialectAuth(t *testing.T) {
	var gotKey, gotVersion, gotAuth string
	ts := retryTestServer(t, noWait, ProviderOptions{API: "anthropic", APIKey: "sk-ant-test"},
		func(w http.ResponseWriter, r *http.Request) {
			gotKey = r.Header.Get("x-api-key")
			gotVersion = r.Header.Get("anthropic-version")
			gotAuth = r.Header.Get("Authorization")
			io.WriteString(w, `{"ok":true}`)
		})

	resp, err := http.Post(ts.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("status %d body %s, want the upstream 200", resp.StatusCode, body)
	}
	if gotKey != "sk-ant-test" {
		t.Errorf("x-api-key = %q, want the provider key", gotKey)
	}
	if gotVersion != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want the default", gotVersion)
	}
	if gotAuth != "" {
		t.Errorf("anthropic provider sent Authorization %q; the Messages API uses x-api-key", gotAuth)
	}
}

// postWithUA sends one forwarded request carrying the given User-Agent, the
// way an SDK client that names itself does.
func postWithUA(t *testing.T, ts *httptest.Server, ua string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", ua)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
}

// TestProviderUserAgent checks the options.userAgent entry: some upstreams
// filter requests carrying no browser-shaped User-Agent, so a configured one
// replaces the client's upstream, leaving it out forwards the client's, and
// an explicit User-Agent in options.headers still wins over it.
func TestProviderUserAgent(t *testing.T) {
	t.Run("configured replaces the client's", func(t *testing.T) {
		var got string
		ts := retryTestServer(t, noWait, ProviderOptions{UserAgent: "MyBot/2.0"},
			func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get("User-Agent")
				io.WriteString(w, `{"ok":true}`)
			})
		postWithUA(t, ts, "TestClient/1.0")
		if got != "MyBot/2.0" {
			t.Errorf("upstream User-Agent = %q, want the configured one", got)
		}
	})

	t.Run("unset forwards the client's", func(t *testing.T) {
		var got string
		ts := retryTestServer(t, noWait, ProviderOptions{},
			func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get("User-Agent")
				io.WriteString(w, `{"ok":true}`)
			})
		postWithUA(t, ts, "TestClient/1.0")
		if got != "TestClient/1.0" {
			t.Errorf("upstream User-Agent = %q, want the client's passed through", got)
		}
	})

	t.Run("headers entry wins", func(t *testing.T) {
		var got string
		ts := retryTestServer(t, noWait, ProviderOptions{
			UserAgent: "MyBot/2.0",
			Headers:   map[string]string{"User-Agent": "Explicit/9.9"},
		}, func(w http.ResponseWriter, r *http.Request) {
			got = r.Header.Get("User-Agent")
			io.WriteString(w, `{"ok":true}`)
		})
		postWithUA(t, ts, "TestClient/1.0")
		if got != "Explicit/9.9" {
			t.Errorf("upstream User-Agent = %q, want the headers entry to win", got)
		}
	})
}
