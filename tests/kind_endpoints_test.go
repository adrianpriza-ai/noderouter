package tests

// The specialist kinds - OCR and moderation above all, since they are the two
// with a dedicated endpoint and no fallback - have to agree with themselves
// end to end: /v1/models must say where a model answers, the router must carry
// a request to that path untouched, and an OCR document upload must clear the
// media body ceiling even when the config classified the model without
// spelling out its modalities.

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// TestModelCardsCarryTheirEndpoint pins the endpoint field on every kind. The
// models site renders it straight onto the card, and a client reading
// /v1/models should not need its own kind-to-endpoint table to know where to
// POST - chat included, where the dialect and the surface, not the kind,
// decide the answer.
func TestModelCardsCarryTheirEndpoint(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{MaxBodyMB: 1, RequestSecs: 5},
		Proxy:  ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt"), Enabled: boolPtr(false)},
		Provider: map[string]*Provider{
			"OpenAI": {Options: ProviderOptions{BaseURL: "http://x/v1"}, Models: map[string]*Model{
				"chat-1":  {},
				"img-1":   {Kind: "image", Kinds: []string{"image_edit"}},
				"edit-1":  {Kind: "image_edit"},
				"vid-1":   {Kind: "video"},
				"sys-1":   {Kind: "systemone"},
				"embed-1": {Kind: "embedding"},
				"tts-1":   {Kind: "audio"},
				"tts-2":   {Kind: "tts"},
				"stt-1":   {Kind: "stt"},
				"ocr-1":   {Kind: "ocr"},
				"mod-1":   {Kind: "moderation"},
			}},
			"Anthropic": {Options: ProviderOptions{BaseURL: "http://y/v1", API: "anthropic"},
				Models: map[string]*Model{"claude": {}}},
			"Native": {Options: ProviderOptions{BaseURL: "http://z/v1", SurfaceName: "responses"},
				Models: map[string]*Model{"r1": {}}},
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
			ID       string   `json:"id"`
			Kind     string   `json:"kind"`
			Kinds    []string `json:"kinds"`
			Endpoint string   `json:"endpoint"`
			Accepts  []string `json:"accepts"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}

	byID := map[string]struct {
		kind, endpoint string
		kinds, accepts []string
	}{}
	for _, c := range list.Data {
		byID[c.ID] = struct {
			kind, endpoint string
			kinds, accepts []string
		}{c.Kind, c.Endpoint, c.Kinds, c.Accepts}
	}
	want := map[string]string{
		"chat-1":  "/v1/chat/completions",
		"img-1":   "/v1/images/generations",
		"edit-1":  "/v1/images/edits",
		"vid-1":   "/v1/videos/generations",
		"sys-1":   "/v1/systemone",
		"embed-1": "/v1/embeddings",
		"tts-1":   "/v1/audio/speech",
		"tts-2":   "/v1/audio/speech",
		"stt-1":   "/v1/audio/transcriptions",
		"ocr-1":   "/v1/ocr",
		"mod-1":   "/v1/moderations",
		"claude":  "/v1/messages",
		"r1":      "/v1/responses",
	}
	for id, ep := range want {
		got, ok := byID[id]
		if !ok {
			t.Errorf("%q missing from /v1/models", id)
			continue
		}
		if got.endpoint != ep {
			t.Errorf("%s endpoint = %q, want %q", id, got.endpoint, ep)
		}
	}
	if got := byID["ocr-1"].kind; got != kindOCR {
		t.Errorf("ocr-1 kind = %q, want %q", got, kindOCR)
	}
	if got := byID["mod-1"].kind; got != kindModeration {
		t.Errorf("mod-1 kind = %q, want %q", got, kindModeration)
	}
	if got := byID["tts-2"].kind; got != kindTTS {
		t.Errorf("tts-2 kind = %q, want %q", got, kindTTS)
	}
	if got := byID["stt-1"].kind; got != kindSTT {
		t.Errorf("stt-1 kind = %q, want %q", got, kindSTT)
	}
	img := byID["img-1"]
	if got := strings.Join(img.kinds, ","); got != "image,image_edit" {
		t.Errorf("img-1 kinds = %q, want image,image_edit", got)
	}
	if got := strings.Join(img.accepts, ","); got != "/v1/images/generations,/v1/images/edits" {
		t.Errorf("img-1 accepts = %q, want both image endpoints", got)
	}
	if got := strings.Join(byID["edit-1"].accepts, ","); got != "/v1/images/edits" {
		t.Errorf("edit-1 accepts = %q, want its specialist endpoint", got)
	}
	if got := strings.Join(byID["chat-1"].accepts, ","); got != "/v1/chat/completions,/v1/responses" {
		t.Errorf("chat-1 accepts = %q, want both OpenAI chat endpoints", got)
	}
	// An Anthropic chat model answers natively on /v1/messages and, through
	// the router's translation, on the OpenAI chat endpoint too.
	if got := strings.Join(byID["claude"].accepts, ","); got != "/v1/messages,/v1/chat/completions" {
		t.Errorf("claude accepts = %q, want the Anthropic Messages endpoint and the translated chat one", got)
	}
}

// TestModerationAndOCRReachTheirEndpoints walks the two dedicated endpoints
// through the router: the path is forwarded as the client sent it, the body
// arrives byte for byte (with only the model id rewritten to the upstream
// name), and the provider's answer comes straight back.
func TestModerationAndOCRReachTheirEndpoints(t *testing.T) {
	var gotPath, gotRaw string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		gotRaw = string(raw)
		io.WriteString(w, `{"results":[{"flagged":false}]}`)
	}))
	defer upstream.Close()

	cfg := &Config{
		Server: ServerConfig{MaxBodyMB: 4, RequestSecs: 5},
		Proxy:  ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt"), Enabled: boolPtr(false)},
		Retry:  noWait,
		Provider: map[string]*Provider{"Mistral": {
			Options: ProviderOptions{BaseURL: upstream.URL + "/v1", APIKey: "sk-test"},
			Models: map[string]*Model{
				"moderation": {Kind: "moderation"},
				"ocr-doc":    {Kind: "ocr", Upstream: "mistral-ocr-latest"},
			},
		}},
	}
	cfg.ApplyDefaults()
	srv := NewServer(cfg, NewPool(cfg.Proxy.File, "", "rotate", false), newLogger(false))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Moderation: untouched path, untouched body, answer passed back.
	status, body := postJSON(t, ts, "/v1/moderations",
		`{"model":"moderation","input":"hello world"}`)
	if status != http.StatusOK {
		t.Fatalf("POST /v1/moderations = %d, want 200. body %s", status, body)
	}
	if gotPath != "/v1/moderations" {
		t.Errorf("upstream path = %q, want /v1/moderations", gotPath)
	}
	sent := decode(t, gotRaw)
	if sent["model"] != "moderation" || sent["input"] != "hello world" {
		t.Errorf("upstream body = %s, want it forwarded as sent", gotRaw)
	}
	if body != `{"results":[{"flagged":false}]}` {
		t.Errorf("answer = %s, want the provider's body back unchanged", body)
	}

	// OCR: same, plus the id rewritten to the upstream name on the way out.
	status, body = postJSON(t, ts, "/v1/ocr",
		`{"model":"ocr-doc","document":{"type":"document_url","document_url":"https://example.com/doc.pdf"}}`)
	if status != http.StatusOK {
		t.Fatalf("POST /v1/ocr = %d, want 200. body %s", status, body)
	}
	if gotPath != "/v1/ocr" {
		t.Errorf("upstream path = %q, want /v1/ocr", gotPath)
	}
	sent = decode(t, gotRaw)
	if sent["model"] != "mistral-ocr-latest" {
		t.Errorf("upstream model = %v, want the upstream name", sent["model"])
	}
	doc, _ := sent["document"].(map[string]any)
	if doc == nil || doc["document_url"] != "https://example.com/doc.pdf" {
		t.Errorf("document = %v, want it forwarded as sent", sent["document"])
	}
}

func TestImageEditReachesItsEndpoint(t *testing.T) {
	var gotPath string
	gotFields := map[string]string{}
	gotFiles := map[string][]byte{}
	gotNames := map[string]string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		mr, err := r.MultipartReader()
		if err != nil {
			t.Errorf("upstream: not multipart: %v", err)
			io.WriteString(w, `{}`)
			return
		}
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("upstream: next part: %v", err)
				break
			}
			data, _ := io.ReadAll(part)
			if part.FileName() != "" {
				gotFiles[part.FormName()] = data
				gotNames[part.FormName()] = part.FileName()
			} else {
				gotFields[part.FormName()] = string(data)
			}
		}
		io.WriteString(w, `{"id":"img_edit_1","object":"image.edit"}`)
	}))
	defer upstream.Close()

	cfg := &Config{
		Server: ServerConfig{MaxBodyMB: 4, RequestSecs: 5},
		Proxy:  ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt"), Enabled: boolPtr(false)},
		Retry:  noWait,
		Provider: map[string]*Provider{"Images": {
			Options: ProviderOptions{BaseURL: upstream.URL + "/v1", APIKey: "sk-test"},
			Models: map[string]*Model{
				"image-model": {Kind: "image", Kinds: []string{"image_edit"}, Upstream: "provider-image-edit"},
			},
		}},
	}
	cfg.ApplyDefaults()
	srv := NewServer(cfg, NewPool(cfg.Proxy.File, "", "rotate", false), newLogger(false))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if err := w.WriteField("model", "image-model"); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteField("prompt", "remove the background"); err != nil {
		t.Fatal(err)
	}
	for _, file := range []struct {
		field, name, contents string
	}{
		{field: "image", name: "input.png", contents: "image-bytes"},
		{field: "mask", name: "mask.png", contents: "mask-bytes"},
	} {
		part, err := w.CreateFormFile(file.field, file.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write([]byte(file.contents)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/images/edits", &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/images/edits = %d, want 200. body %s", resp.StatusCode, answer)
	}
	if gotPath != "/v1/images/edits" {
		t.Errorf("upstream path = %q, want /v1/images/edits", gotPath)
	}
	if gotFields["model"] != "provider-image-edit" {
		t.Errorf("upstream model = %q, want rewritten upstream name", gotFields["model"])
	}
	if gotFields["prompt"] != "remove the background" {
		t.Errorf("upstream prompt = %q, want it forwarded", gotFields["prompt"])
	}
	if string(gotFiles["image"]) != "image-bytes" || gotNames["image"] != "input.png" {
		t.Errorf("upstream image = %q (%q), want original file", gotFiles["image"], gotNames["image"])
	}
	if string(gotFiles["mask"]) != "mask-bytes" || gotNames["mask"] != "mask.png" {
		t.Errorf("upstream mask = %q (%q), want original file", gotFiles["mask"], gotNames["mask"])
	}
}

func TestImageEditKindGetsTheLargerCeiling(t *testing.T) {
	var reached bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		_, _ = io.Copy(io.Discard, r.Body)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	cfg := &Config{
		Server: ServerConfig{MaxBodyMB: 1, MaxMediaBodyMB: 16, RequestSecs: 5},
		Proxy:  ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt"), Enabled: boolPtr(false)},
		Retry:  noWait,
		Provider: map[string]*Provider{"Images": {
			Options: ProviderOptions{BaseURL: upstream.URL + "/v1"},
			Models:  map[string]*Model{"image-model": {Kind: "image_edit"}},
		}},
	}
	cfg.ApplyDefaults()
	srv := NewServer(cfg, NewPool(cfg.Proxy.File, "", "rotate", false), newLogger(false))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	pad := strings.Repeat("A", 2<<20)
	status, body := postMultipart(t, ts, "/v1/images/edits",
		map[string]string{"model": "image-model", "prompt": "edit"},
		"image", "input.png", pad)
	if status != http.StatusOK {
		t.Fatalf("status %d, want image_edit to clear the 1MB ceiling. body %s", status, body)
	}
	if !reached {
		t.Error("the upstream was never reached, so the image edit upload was refused at the size gate")
	}
}

// TestOCRKindGetsTheLargerCeiling covers the gap kind-only configs hit: an OCR
// model whose payload is the document itself - often a base64 PDF well past
// maxBodyMb - has to clear the media ceiling from its kind alone, because a
// config that says "kind": "ocr" and nothing about modalities declares media
// all the same. The text-only model in the same config keeps the ordinary
// ceiling, so lifting it for OCR does not lift it for everything.
func TestOCRKindGetsTheLargerCeiling(t *testing.T) {
	var reached bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		_, _ = io.Copy(io.Discard, r.Body)
		io.WriteString(w, `{"pages":[]}`)
	}))
	defer upstream.Close()

	cfg := &Config{
		Server: ServerConfig{MaxBodyMB: 1, MaxMediaBodyMB: 16, RequestSecs: 5},
		Proxy:  ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt"), Enabled: boolPtr(false)},
		Retry:  noWait,
		Provider: map[string]*Provider{"Mistral": {
			Options: ProviderOptions{BaseURL: upstream.URL + "/v1", APIKey: "sk-test"},
			Models: map[string]*Model{
				// No modalities declared: the kind alone must open the ceiling.
				"ocr-doc": {Kind: "ocr"},
				"text-1":  {},
			},
		}},
	}
	cfg.ApplyDefaults()
	srv := NewServer(cfg, NewPool(cfg.Proxy.File, "", "rotate", false), newLogger(false))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 2MB against a 1MB ordinary ceiling, under the 16MB media one.
	pad := strings.Repeat("A", 2<<20)
	status, body := postJSON(t, ts, "/v1/ocr",
		`{"model":"ocr-doc","document":{"type":"document_url","document_url":"data:application/pdf;base64,`+pad+`"}}`)
	if status != http.StatusOK {
		t.Errorf("status %d, want the OCR kind to clear the 1MB ceiling. body %s", status, body)
	}
	if !reached {
		t.Error("the upstream was never reached, so the document upload was refused at the size gate")
	}

	// The same size naming a text-only model is still over the ordinary limit.
	status, body = postJSON(t, ts, "/v1/chat/completions",
		`{"model":"text-1","messages":[{"role":"user","content":"`+pad+`"}]}`)
	if status != http.StatusRequestEntityTooLarge {
		t.Errorf("status %d, want 413 for a text-only body over the ordinary ceiling", status)
	}
	if !strings.Contains(body, "over 1MB") {
		t.Errorf("error = %s, want it to name the ordinary ceiling", body)
	}
}

// TestVideoKindReachesItsEndpoint walks the video kind through the router: the
// card names /v1/videos/generations, the path is forwarded as the client sent
// it, and the body arrives with only the model id rewritten - the same
// path-through treatment image and OCR models get.
func TestVideoKindReachesItsEndpoint(t *testing.T) {
	var gotPath, gotRaw string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		gotRaw = string(raw)
		io.WriteString(w, `{"id":"vid_1","status":"queued"}`)
	}))
	defer upstream.Close()

	cfg := &Config{
		Server: ServerConfig{MaxBodyMB: 4, RequestSecs: 5},
		Proxy:  ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt"), Enabled: boolPtr(false)},
		Retry:  noWait,
		Provider: map[string]*Provider{"VideoGen": {
			Options: ProviderOptions{BaseURL: upstream.URL + "/v1", APIKey: "sk-test"},
			Models: map[string]*Model{
				"sora-2":  {Kind: "video", Upstream: "sora-2-pro"},
				"veo-3.1": {Kind: "video"},
			},
		}},
	}
	cfg.ApplyDefaults()
	srv := NewServer(cfg, NewPool(cfg.Proxy.File, "", "rotate", false), newLogger(false))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	status, body := postJSON(t, ts, "/v1/videos/generations",
		`{"model":"sora-2","prompt":"A drone shot over a pine forest at dawn","seconds":5}`)
	if status != http.StatusOK {
		t.Fatalf("POST /v1/videos/generations = %d, want 200. body %s", status, body)
	}
	if gotPath != "/v1/videos/generations" {
		t.Errorf("upstream path = %q, want /v1/videos/generations", gotPath)
	}
	sent := decode(t, gotRaw)
	if sent["model"] != "sora-2-pro" {
		t.Errorf("upstream model = %v, want the upstream name", sent["model"])
	}
	if sent["prompt"] != "A drone shot over a pine forest at dawn" || sent["seconds"] != float64(5) {
		t.Errorf("upstream body = %s, want it forwarded as sent", gotRaw)
	}
	if body != `{"id":"vid_1","status":"queued"}` {
		t.Errorf("answer = %s, want the provider's body back unchanged", body)
	}
}

// TestSTTKindGetsTheLargerCeiling is the whisper counterpart of the OCR
// case: an stt model's payload is the recording itself - minutes of audio in
// a multipart upload - so the kind alone has to open the media ceiling, even
// when the config never spelled out modalities. The text-only model in the
// same config keeps the ordinary ceiling.
func TestSTTKindGetsTheLargerCeiling(t *testing.T) {
	var reached bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		_, _ = io.Copy(io.Discard, r.Body)
		io.WriteString(w, `{"text":"hello"}`)
	}))
	defer upstream.Close()

	cfg := &Config{
		Server: ServerConfig{MaxBodyMB: 1, MaxMediaBodyMB: 16, RequestSecs: 5},
		Proxy:  ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt"), Enabled: boolPtr(false)},
		Retry:  noWait,
		Provider: map[string]*Provider{"Speech": {
			Options: ProviderOptions{BaseURL: upstream.URL + "/v1", APIKey: "sk-test"},
			Models: map[string]*Model{
				// No modalities declared: the kind alone must open the ceiling.
				"whisper-1": {Kind: "stt"},
				"text-1":    {},
			},
		}},
	}
	cfg.ApplyDefaults()
	srv := NewServer(cfg, NewPool(cfg.Proxy.File, "", "rotate", false), newLogger(false))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// A 2MB recording against the 1MB ordinary ceiling, under the 16MB media
	// one. The size gate has to read the model out of the form, not JSON.
	pad := strings.Repeat("A", 2<<20)
	status, body := postMultipart(t, ts, "/v1/audio/transcriptions",
		map[string]string{"model": "whisper-1", "response_format": "json"},
		"file", "audio.mp3", pad)
	if status != http.StatusOK {
		t.Errorf("status %d, want the stt kind to clear the 1MB ceiling. body %s", status, body)
	}
	if !reached {
		t.Error("the upstream was never reached, so the upload was refused at the size gate")
	}

	// The same size as JSON naming a text-only model is still over the
	// ordinary limit.
	reached = false
	padJSON := strings.Repeat("A", 2<<20)
	status, body = postJSON(t, ts, "/v1/chat/completions",
		`{"model":"text-1","messages":[{"role":"user","content":"`+padJSON+`"}]}`)
	if status != http.StatusRequestEntityTooLarge {
		t.Errorf("status %d, want 413 for a text-only body over the ordinary ceiling", status)
	}
	if reached {
		t.Error("the text-only oversized body reached the upstream; the ordinary ceiling did not hold")
	}
}

// postMultipart builds a multipart/form-data body the way a whisper client
// does - form fields plus a file part - and posts it. This is the shape
// /v1/audio/transcriptions actually sees: the model id rides in a form
// field, not in JSON.
func postMultipart(t *testing.T, ts *httptest.Server, path string, fields map[string]string, fileField, fileName, fileContent string) (int, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	fw, err := w.CreateFormFile(fileField, fileName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte(fileContent)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, ts.URL+path, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// TestSTTKindReachesItsEndpoint walks a whisper-style multipart upload
// through the router: the model id is read out of the form field, the path
// is forwarded as the client sent it, the id is rewritten to the upstream
// name on the way out, and the file part arrives byte for byte.
func TestSTTKindReachesItsEndpoint(t *testing.T) {
	var gotPath, gotCT string
	var gotForm map[string]string
	var gotFile []byte
	var gotFileName string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		mr, err := r.MultipartReader()
		if err != nil {
			t.Errorf("upstream: not multipart: %v", err)
			io.WriteString(w, `{}`)
			return
		}
		gotForm = map[string]string{}
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("upstream: next part: %v", err)
				break
			}
			if name := part.FileName(); name != "" {
				gotFileName = name
				gotFile, _ = io.ReadAll(part)
				continue
			}
			value, _ := io.ReadAll(part)
			gotForm[part.FormName()] = string(value)
		}
		io.WriteString(w, `{"text":"hello world","task":"transcribe"}`)
	}))
	defer upstream.Close()

	cfg := &Config{
		Server: ServerConfig{MaxBodyMB: 8, RequestSecs: 5},
		Proxy:  ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt"), Enabled: boolPtr(false)},
		Retry:  noWait,
		Provider: map[string]*Provider{"Speech": {
			Options: ProviderOptions{BaseURL: upstream.URL + "/v1", APIKey: "sk-test"},
			Models: map[string]*Model{
				// The client names "whisper"; the provider expects "whisper-1".
				"whisper":   {Kind: "stt", Upstream: "whisper-1"},
				"tts-voice": {Kind: "tts", Upstream: "tts-1"},
			},
		}},
	}
	cfg.ApplyDefaults()
	srv := NewServer(cfg, NewPool(cfg.Proxy.File, "", "rotate", false), newLogger(false))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	audio := "QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVphYmNkZWZnaGlqa2xtbm9wcXJzdA==//79"
	status, body := postMultipart(t, ts, "/v1/audio/transcriptions",
		map[string]string{"model": "whisper", "response_format": "json", "language": "en"},
		"file", "meeting.mp3", audio)
	if status != http.StatusOK {
		t.Fatalf("POST /v1/audio/transcriptions = %d, want 200. body %s", status, body)
	}
	if gotPath != "/v1/audio/transcriptions" {
		t.Errorf("upstream path = %q, want /v1/audio/transcriptions", gotPath)
	}
	if gotForm["model"] != "whisper-1" {
		t.Errorf("upstream model form field = %q, want the upstream name", gotForm["model"])
	}
	if gotForm["response_format"] != "json" || gotForm["language"] != "en" {
		t.Errorf("upstream form = %v, want the other fields forwarded as sent", gotForm)
	}
	if gotFileName != "meeting.mp3" {
		t.Errorf("upstream file name = %q, want meeting.mp3", gotFileName)
	}
	if string(gotFile) != audio {
		t.Errorf("upstream file bytes differ from what the client sent (%d vs %d bytes)", len(gotFile), len(audio))
	}
	if !strings.HasPrefix(gotCT, "multipart/form-data") {
		t.Errorf("upstream content type = %q, want multipart/form-data", gotCT)
	}
	if body != `{"text":"hello world","task":"transcribe"}` {
		t.Errorf("answer = %s, want the provider's body back unchanged", body)
	}
}

// TestSTTModelWithoutRenameStillRoutes covers the plain case: no upstream
// rewrite is needed, so the form - file bytes included - must cross exactly
// as the client sent it.
func TestSTTModelWithoutRenameStillRoutes(t *testing.T) {
	var gotModel string
	var gotFile []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mr, _ := r.MultipartReader()
		for {
			part, err := mr.NextPart()
			if err != nil {
				break
			}
			if part.FileName() != "" {
				gotFile, _ = io.ReadAll(part)
				continue
			}
			value, _ := io.ReadAll(part)
			if part.FormName() == "model" {
				gotModel = string(value)
			}
		}
		io.WriteString(w, `{"text":"bonjour"}`)
	}))
	defer upstream.Close()

	cfg := &Config{
		Server: ServerConfig{MaxBodyMB: 8, RequestSecs: 5},
		Proxy:  ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt"), Enabled: boolPtr(false)},
		Retry:  noWait,
		Provider: map[string]*Provider{"Speech": {
			Options: ProviderOptions{BaseURL: upstream.URL + "/v1", APIKey: "sk-test"},
			Models:  map[string]*Model{"whisper-1": {Kind: "stt"}},
		}},
	}
	cfg.ApplyDefaults()
	srv := NewServer(cfg, NewPool(cfg.Proxy.File, "", "rotate", false), newLogger(false))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	audio := "a small recording, byte for byte"
	status, body := postMultipart(t, ts, "/v1/audio/transcriptions",
		map[string]string{"model": "whisper-1"}, "file", "note.wav", audio)
	if status != http.StatusOK {
		t.Fatalf("POST /v1/audio/transcriptions = %d, want 200. body %s", status, body)
	}
	if gotModel != "whisper-1" {
		t.Errorf("upstream model = %q, want whisper-1", gotModel)
	}
	if string(gotFile) != audio {
		t.Errorf("upstream file = %q, want it byte for byte", gotFile)
	}
}

// TestTTSSpeechReachesItsEndpoint is the tts counterpart: a JSON body at
// /v1/audio/speech, forwarded untouched except for the upstream id rewrite.
func TestTTSSpeechReachesItsEndpoint(t *testing.T) {
	var gotPath, gotRaw string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		gotRaw = string(raw)
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Write([]byte("ID3audio-bytes"))
	}))
	defer upstream.Close()

	cfg := &Config{
		Server: ServerConfig{MaxBodyMB: 4, RequestSecs: 5},
		Proxy:  ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt"), Enabled: boolPtr(false)},
		Retry:  noWait,
		Provider: map[string]*Provider{"Speech": {
			Options: ProviderOptions{BaseURL: upstream.URL + "/v1", APIKey: "sk-test"},
			Models:  map[string]*Model{"speak": {Kind: "tts", Upstream: "tts-1"}},
		}},
	}
	cfg.ApplyDefaults()
	srv := NewServer(cfg, NewPool(cfg.Proxy.File, "", "rotate", false), newLogger(false))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/audio/speech",
		strings.NewReader(`{"model":"speak","input":"Hello from noderouter","voice":"alloy","response_format":"mp3"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/audio/speech = %d, want 200. body %s", resp.StatusCode, answer)
	}
	if gotPath != "/v1/audio/speech" {
		t.Errorf("upstream path = %q, want /v1/audio/speech", gotPath)
	}
	sent := decode(t, gotRaw)
	if sent["model"] != "tts-1" {
		t.Errorf("upstream model = %v, want the upstream name", sent["model"])
	}
	if sent["input"] != "Hello from noderouter" || sent["voice"] != "alloy" {
		t.Errorf("upstream body = %s, want it forwarded as sent", gotRaw)
	}
	if resp.Header.Get("Content-Type") != "audio/mpeg" || string(answer) != "ID3audio-bytes" {
		t.Errorf("answer = %s (%s), want the provider's audio back unchanged", answer, resp.Header.Get("Content-Type"))
	}
}

// TestVideoKindGetsTheLargerCeiling is the video counterpart of the OCR case:
// a model classified "kind": "video" and nothing else still carries media, so
// the kind alone opens the media ceiling. A clip arrives base64 encoded, which
// inflates it by a third, and an ordinary prompt-sized ceiling would refuse the
// request before the provider ever saw it.
func TestVideoKindGetsTheLargerCeiling(t *testing.T) {
	var reached bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		_, _ = io.Copy(io.Discard, r.Body)
		io.WriteString(w, `{"id":"vid_1","status":"queued"}`)
	}))
	defer upstream.Close()

	cfg := &Config{
		Server: ServerConfig{MaxBodyMB: 1, MaxMediaBodyMB: 16, RequestSecs: 5},
		Proxy:  ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt"), Enabled: boolPtr(false)},
		Retry:  noWait,
		Provider: map[string]*Provider{"VideoGen": {
			Options: ProviderOptions{BaseURL: upstream.URL + "/v1", APIKey: "sk-test"},
			Models: map[string]*Model{
				// No modalities declared: the kind alone must open the ceiling.
				"veo-3.1": {Kind: "video"},
				"text-1":  {},
			},
		}},
	}
	cfg.ApplyDefaults()
	srv := NewServer(cfg, NewPool(cfg.Proxy.File, "", "rotate", false), newLogger(false))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 2MB against a 1MB ordinary ceiling, under the 16MB media one.
	pad := strings.Repeat("A", 2<<20)
	status, body := postJSON(t, ts, "/v1/videos/generations",
		`{"model":"veo-3.1","prompt":"clip","input_reference":"data:video/mp4;base64,`+pad+`"}`)
	if status != http.StatusOK {
		t.Errorf("status %d, want the video kind to clear the 1MB ceiling. body %s", status, body)
	}
	if !reached {
		t.Error("the upstream was never reached, so the upload was refused at the size gate")
	}

	// The same size naming a text-only model is still over the ordinary limit.
	status, body = postJSON(t, ts, "/v1/chat/completions",
		`{"model":"text-1","messages":[{"role":"user","content":"`+pad+`"}]}`)
	if status != http.StatusRequestEntityTooLarge {
		t.Errorf("status %d, want 413 for a text-only body over the ordinary ceiling", status)
	}
}
