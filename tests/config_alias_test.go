package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A model-id prefix picks the provider it points at. "opencode" is the whole
// of one provider's alias and only the start of another one's name, and
// provider names are sorted, so without an exact match first the sort decides
// and the request goes to the wrong upstream.
func TestNamespacePrefixPrefersTheExactAlias(t *testing.T) {
	cfg := &Config{Provider: map[string]*Provider{
		// Sorts before Zen, and its name starts with the prefix being asked for.
		"OpenCode-v2": {Options: ProviderOptions{BaseURL: "http://v2"}, Models: map[string]*Model{
			"m3": {},
		}},
		"Zen": {ID: "zen", Aliases: []string{"opencode"}, Options: ProviderOptions{BaseURL: "http://zen"}, Models: map[string]*Model{
			"m3": {},
		}},
	}}
	cfg.ApplyDefaults()

	backends, err := cfg.ResolveBackends("opencode/m3")
	if err != nil {
		t.Fatalf("ResolveBackends(opencode/m3) = %v", err)
	}
	if len(backends) != 1 {
		t.Fatalf("backends = %+v, want one", backends)
	}
	if backends[0].Name != "Zen" || backends[0].Upstream != "m3" {
		t.Errorf("opencode/m3 went to %q / %q, want Zen / m3", backends[0].Name, backends[0].Upstream)
	}

	// The card feeds /v1/models/{id} and the body-size gate, so it has to
	// resolve the prefix the same way the forward path does.
	ref, _, err := cfg.ModelCard("opencode/m3")
	if err != nil {
		t.Fatalf("ModelCard(opencode/m3) = %v", err)
	}
	if ref.Name != "Zen" {
		t.Errorf("card provider = %q, want Zen", ref.Name)
	}
}

// The prefix still matches a name that only starts the same way when there is
// nothing to be exact about: one candidate is not an ambiguity.
func TestNamespacePrefixFallsBackToOnePartialMatch(t *testing.T) {
	cfg := &Config{Provider: map[string]*Provider{
		"Groq": {Options: ProviderOptions{BaseURL: "http://groq"}, Models: map[string]*Model{
			"llama": {},
		}},
		"Other": {Options: ProviderOptions{BaseURL: "http://other"}, Models: map[string]*Model{
			"llama": {},
		}},
	}}
	cfg.ApplyDefaults()

	backends, err := cfg.ResolveBackends("groq/llama")
	if err != nil {
		t.Fatalf("ResolveBackends(groq/llama) = %v", err)
	}
	if len(backends) != 1 || backends[0].Name != "Groq" {
		t.Errorf("groq/llama went to %+v, want Groq", backends)
	}
}

// Two providers and a prefix that only starts both of their names is not a
// resolution, it is a guess. Refusing beats sending the request to whichever
// one the sort happened to pick, and the error has to name both so the client
// can spell one out.
func TestAmbiguousNamespacePrefixIsRefusedNotGuessed(t *testing.T) {
	cfg := &Config{Provider: map[string]*Provider{
		"groq-fast": {Options: ProviderOptions{BaseURL: "http://fast"}, Models: map[string]*Model{
			"llama": {},
		}},
		"groq-slow": {Options: ProviderOptions{BaseURL: "http://slow"}, Models: map[string]*Model{
			"llama": {},
		}},
	}}
	cfg.ApplyDefaults()

	_, err := cfg.ResolveBackends("groq/llama")
	if err == nil {
		t.Fatal("ResolveBackends(groq/llama) picked a provider, want an ambiguity error")
	}
	for _, want := range []string{"groq-fast", "groq-slow"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}

	if _, _, err := cfg.ModelCard("groq/llama"); err == nil {
		t.Error("ModelCard(groq/llama) picked a provider, want an ambiguity error")
	}

	// Spelling the provider out is the way out, so it has to work.
	backends, err := cfg.ResolveBackends("groq-slow/llama")
	if err != nil {
		t.Fatalf("ResolveBackends(groq-slow/llama) = %v", err)
	}
	if len(backends) != 1 || backends[0].Name != "groq-slow" {
		t.Errorf("groq-slow/llama went to %+v, want groq-slow", backends)
	}
}

// An unknown prefix is still unknown: the ambiguity check must not swallow the
// "no such provider" fallbacks (default provider, single provider) that come
// after it.
func TestUnknownNamespacePrefixStillFallsBack(t *testing.T) {
	cfg := &Config{Provider: map[string]*Provider{
		"Zen": {Options: ProviderOptions{BaseURL: "http://zen"}, Models: map[string]*Model{
			"m3": {},
		}},
	}}
	cfg.Routing.DefaultProvider = "Zen"
	cfg.ApplyDefaults()

	backends, err := cfg.ResolveBackends("nosuchprefix/m3")
	if err != nil {
		t.Fatalf("ResolveBackends(nosuchprefix/m3) = %v", err)
	}
	if len(backends) != 1 || backends[0].Name != "Zen" {
		t.Errorf("unknown prefix went to %+v, want the default provider Zen", backends)
	}
}

// Save must not invent a spelling for this one key: it writes the
// lower-camel key docs/configuration.md documents, and it still reads the
// capitalised one an older build may have written into the user's file.
func TestSaveWritesTheDocumentedRevalidateKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")

	cfg := &Config{}
	cfg.ApplyDefaults()
	cfg.Proxy.Test.ReValidateAfter = "90m"
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"reValidateAfter": "90m"`) {
		t.Errorf("saved config does not carry the documented key:\n%s", raw)
	}
	if strings.Contains(string(raw), `"ReValidateAfter"`) {
		t.Errorf("saved config still writes the capitalised key:\n%s", raw)
	}

	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.ReValidateAfter(); got != 90*time.Minute {
		t.Errorf("saved config read back as %v, want 90m", got)
	}

	for _, spelling := range []string{"reValidateAfter", "ReValidateAfter"} {
		body := `{"proxy":{"test":{"` + spelling + `":"45m"}}}`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		loaded, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("%s: %v", spelling, err)
		}
		if got := loaded.ReValidateAfter(); got != 45*time.Minute {
			t.Errorf("%s read back as %v, want 45m", spelling, got)
		}
	}
}
