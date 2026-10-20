package tests

// The config loader speaks JSONC, because the file it is documented against is
// written that way: noderouter.jsonc carries // comments throughout, and the
// docs say to copy it into ~/.noderouter/models.json as it stands. A loader
// that only accepted strict JSON rejected both - "invalid character '/'
// looking for beginning of object key string" - so the documented setup step
// failed before it ever reached a provider. These tests hold the loader to
// the two halves of that promise: the shipped example parses, and the syntax
// around JSON's own (comments, trailing commas) is stripped without touching
// a string value - URLs are full of slashes that must survive.

import (
	"os"
	"path/filepath"
	"testing"
)

// TestShippedExampleConfigLoads is the one that matters most: the example
// config the docs tell you to copy has to load, comments and all, and still
// describe the models it advertises - including the OCR and moderation pair
// and the limits the models site renders.
func TestShippedExampleConfigLoads(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join("..", "noderouter.jsonc"))
	if err != nil {
		t.Fatalf("LoadConfig(noderouter.jsonc): %v\nthe shipped example is the config the docs say to copy, so it must parse as it stands", err)
	}

	// URLs inside string values must survive the comment stripper byte for
	// byte - a naive "remove // to end of line" would eat them.
	if got, want := cfg.Provider["Mistral"].Options.BaseURL, "https://api.mistral.ai/v1"; got != want {
		t.Errorf("Mistral baseURL = %q, want %q with its slashes intact", got, want)
	}

	byID := map[string]*Model{}
	for _, e := range cfg.ModelTable() {
		byID[e.Call] = e.Model
	}
	ocr, ok := byID["mistral-ocr-latest"]
	if !ok {
		t.Fatal("mistral-ocr-latest missing from the example config")
	}
	if ocr.Kind != kindOCR {
		t.Errorf("ocr kind = %q, want %q", ocr.Kind, kindOCR)
	}
	if ocr.Limit.Context != 16384 || ocr.Limit.Output != 4096 {
		t.Errorf("ocr limits = %+v, want context 16384 / output 4096", ocr.Limit)
	}
	mod, ok := byID["mistral-moderation-latest"]
	if !ok {
		t.Fatal("mistral-moderation-latest missing from the example config")
	}
	if mod.Kind != kindModeration {
		t.Errorf("moderation kind = %q, want %q", mod.Kind, kindModeration)
	}
	if mod.Limit.Context != 131072 {
		t.Errorf("moderation context = %d, want 131072", mod.Limit.Context)
	}

	// The video example spells the kind on one model and lets the other fall
	// out of its output modality, and both have to land on the video kind -
	// the endpoint the card advertises depends on it.
	for _, id := range []string{"veo-3.1-generate", "kling-v2.5-turbo"} {
		m, ok := byID[id]
		if !ok {
			t.Fatalf("%s missing from the example config", id)
		}
		if m.Kind != kindVideo {
			t.Errorf("%s kind = %q, want %q (video is classified by kind or by output modality)", id, m.Kind, kindVideo)
		}
	}
	emb, ok := byID["text-embedding-3-large"]
	if !ok {
		t.Fatal("text-embedding-3-large missing from the example config")
	}
	if emb.Kind != kindEmbedding {
		t.Errorf("embedding kind = %q, want %q", emb.Kind, kindEmbedding)
	}

	// The speech example spells both kinds explicitly: tts answers at
	// /v1/audio/speech, stt at /v1/audio/transcriptions. The stt card also
	// carries audio+file input, which is what opens the media body ceiling
	// for the whisper upload.
	tts, ok := byID["gpt-4o-mini-tts"]
	if !ok {
		t.Fatal("gpt-4o-mini-tts missing from the example config")
	}
	if tts.Kind != kindTTS {
		t.Errorf("tts kind = %q, want %q", tts.Kind, kindTTS)
	}
	stt, ok := byID["whisper-1"]
	if !ok {
		t.Fatal("whisper-1 missing from the example config")
	}
	if stt.Kind != kindSTT {
		t.Errorf("stt kind = %q, want %q", stt.Kind, kindSTT)
	}
	if !stt.HasModality("audio") || !stt.HasModality("file") {
		t.Errorf("stt modalities = %+v, want audio and file input so the upload clears maxBodyMb", stt.Modalities)
	}
}

// TestConfigAcceptsJSONC exercises the syntax around JSON's own directly:
// line and block comments (including ones containing quotes and braces) and
// trailing commas, with a URL whose double slash must come through untouched.
func TestConfigAcceptsJSONC(t *testing.T) {
	body := `{
  // a line comment with "quotes" and a { brace inside
  /* a block
     comment */
  "server": { "port": 32256, },
  "provider": {
    "P": {
      "options": { "baseURL": "https://example.com/v1//x", },
      "models": {
        "m": { "limit": { "context": 1000, } }
      }
    }
  }
}`
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig on JSONC body: %v", err)
	}
	if cfg.Server.Port != 32256 {
		t.Errorf("port = %d, want 32256", cfg.Server.Port)
	}
	if got, want := cfg.Provider["P"].Options.BaseURL, "https://example.com/v1//x"; got != want {
		t.Errorf("baseURL = %q, want %q with the double slash intact", got, want)
	}
	if got := cfg.Provider["P"].Models["m"].Limit.Context; got != 1000 {
		t.Errorf("limit.context = %d, want 1000", got)
	}
}
