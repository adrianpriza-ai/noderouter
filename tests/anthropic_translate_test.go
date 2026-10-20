package tests

// The chat -> Anthropic Messages translation, end to end through a router in
// front of a fake Messages API. The rule these tests hold is the one the
// OpenAI-surface translator already holds: a chat client must be able to speak
// to an Anthropic provider without knowing it is there - the request leaves on
// /v1/messages in the Messages shape, and the answer comes back as a chat
// completion, streamed or not.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const anthropicAnswer = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-test",
	"stop_reason":"end_turn","content":[{"type":"text","text":"pong"}],
	"usage":{"input_tokens":11,"output_tokens":2}}`

// anthropicRouter wires a router in front of a fake Anthropic Messages
// provider: one provider, on the anthropic dialect, with the proxy pool off.
func anthropicRouter(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)

	cfg := &Config{
		Server: ServerConfig{MaxBodyMB: 1, RequestSecs: 5},
		Proxy:  ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt"), Enabled: boolPtr(false)},
		Retry:  noWait,
		Provider: map[string]*Provider{"Anthropic": {
			Options: ProviderOptions{BaseURL: upstream.URL, API: "anthropic", APIKey: "sk-ant-test"},
			Models:  map[string]*Model{"m": {}},
		}},
	}
	cfg.ApplyDefaults()
	srv := NewServer(cfg, NewPool(cfg.Proxy.File, "", "rotate", false), newLogger(false))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// postChat sends a chat completions body through the router.
func postChat(t *testing.T, ts *httptest.Server, body string) (int, string) {
	t.Helper()
	return postJSON(t, ts, "/v1/chat/completions", body)
}

// TestChatClientReachesAnthropicProvider is the core case: a chat client, an
// Anthropic Messages provider. The request must arrive on /v1/messages with
// the Messages shape and the Anthropic credential headers, and the answer must
// come back as a chat completion.
func TestChatClientReachesAnthropicProvider(t *testing.T) {
	var gotPath, gotAPIKey, gotVersion, gotAuth string
	var gotBody map[string]any
	ts := anthropicRouter(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, anthropicAnswer)
	})

	status, body := postChat(t, ts, `{"model":"m","messages":[
		{"role":"system","content":"be brief"},
		{"role":"user","content":"ping"}]}`)
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200. body %s", status, body)
	}

	if want := "/v1/messages"; gotPath != want {
		t.Errorf("upstream path = %q, want %q", gotPath, want)
	}
	if gotAPIKey != "sk-ant-test" || gotAuth != "" {
		t.Errorf("credential headers = x-api-key %q, Authorization %q; the Messages API authenticates with x-api-key alone", gotAPIKey, gotAuth)
	}
	if gotVersion == "" {
		t.Error("no anthropic-version header on the translated request")
	}

	system, _ := gotBody["system"].([]any)
	if len(system) != 1 {
		t.Fatalf("system = %v, want the system message pulled out of messages as a text block", gotBody["system"])
	}
	if block, _ := system[0].(map[string]any); block["text"] != "be brief" {
		t.Errorf("system = %v, want the system message's text", gotBody["system"])
	}
	if _, ok := gotBody["messages"]; !ok {
		t.Errorf("body has no messages: %v", gotBody)
	}
	if gotBody["max_tokens"] != float64(4096) {
		t.Errorf("max_tokens = %v, want the Anthropic-required default (the Messages API rejects a request without it)", gotBody["max_tokens"])
	}

	got := decode(t, body)
	if got["object"] != "chat.completion" {
		t.Errorf("object = %v, want chat.completion", got["object"])
	}
	if got["id"] != "chatcmpl-1" {
		t.Errorf("id = %v, want the msg_ id re-spelled for the chat wire", got["id"])
	}
	choices, _ := got["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %v, want one", got["choices"])
	}
	choice, _ := choices[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Errorf("finish_reason = %v, want end_turn translated to stop", choice["finish_reason"])
	}
	msg, _ := choice["message"].(map[string]any)
	if msg["content"] != "pong" {
		t.Errorf("content = %v, want pong", msg["content"])
	}
	usage, _ := got["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(11) || usage["completion_tokens"] != float64(2) {
		t.Errorf("usage = %v, want the Anthropic counts on the chat names", usage)
	}
}

// TestAnthropicToolCallRoundTrips covers the tool shapes both ways: chat's
// function tool becomes an Anthropic tool, and Anthropic's tool_use answer
// comes back as a chat tool_calls array with the OpenAI finish reason.
func TestAnthropicToolCallRoundTrips(t *testing.T) {
	var got map[string]any
	ts := anthropicRouter(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		io.WriteString(w, `{"id":"msg_1","type":"message","model":"claude-test",
			"stop_reason":"tool_use","content":[
				{"type":"text","text":"let me check"},
				{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Oslo"}}],
			"usage":{"input_tokens":5,"output_tokens":7}}`)
	})

	status, body := postChat(t, ts, `{"model":"m","messages":[{"role":"user","content":"weather?"}],
		"tools":[{"type":"function","function":{"name":"get_weather",
			"parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]}`)
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200. body %s", status, body)
	}

	tools, _ := got["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("upstream tools = %v, want the chat function tool translated", got["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if tool["name"] != "get_weather" || tool["type"] != "custom" {
		t.Errorf("upstream tool = %v, want get_weather in Anthropic's shape", tool)
	}
	if _, ok := tool["input_schema"]; !ok {
		t.Errorf("upstream tool has no input_schema: %v", tool)
	}

	answer := decode(t, body)
	choices, _ := answer["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v, want tool_calls from Anthropic's stop reason", choice["finish_reason"])
	}
	msg, _ := choice["message"].(map[string]any)
	if msg["content"] != "let me check" {
		t.Errorf("content = %v, want the text block that came with the tool call", msg["content"])
	}
	calls, _ := msg["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %v, want one", msg["tool_calls"])
	}
	call, _ := calls[0].(map[string]any)
	if call["id"] != "toolu_1" {
		t.Errorf("tool call id = %v, want toolu_1", call["id"])
	}
	fn, _ := call["function"].(map[string]any)
	if fn["name"] != "get_weather" || !strings.Contains(fn["arguments"].(string), "Oslo") {
		t.Errorf("tool call = %v, want get_weather with its arguments back as JSON", call)
	}
}

// TestAnthropicStreamBecomesChatStream is the streaming half: Anthropic's
// event sequence must arrive as chat chunks - text in order, a finish reason,
// one [DONE] - and the upstream must see stream: true.
func TestAnthropicStreamBecomesChatStream(t *testing.T) {
	var gotStream atomic.Bool
	ts := anthropicRouter(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		if s, _ := body["stream"].(bool); s {
			gotStream.Store(true)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, ev := range []struct{ name, data string }{
			{"message_start", `{"type":"message_start","message":{"id":"msg_1","model":"claude-test","usage":{"input_tokens":3}}}`},
			{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
			{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`},
			{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`},
			{"content_block_stop", `{"type":"content_block_stop","index":0}`},
			{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`},
			{"message_stop", `{"type":"message_stop"}`},
		} {
			io.WriteString(w, "event: "+ev.name+"\ndata: "+ev.data+"\n\n")
			flusher.Flush()
		}
	})

	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true,
			"stream_options":{"include_usage":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if !gotStream.Load() {
		t.Error("upstream never saw stream: true; the translated request must ask for a stream")
	}

	var text strings.Builder
	var finish string
	var usage map[string]any
	done := 0
	for _, c := range readSSE(t, string(raw)) {
		if c.data == "[DONE]" {
			done++
			continue
		}
		var chunk struct {
			Object  string `json:"object"`
			Choices []struct {
				Delta        map[string]any `json:"delta"`
				FinishReason *string        `json:"finish_reason"`
			} `json:"choices"`
			Usage map[string]any `json:"usage"`
		}
		if err := json.Unmarshal([]byte(c.data), &chunk); err != nil {
			t.Fatalf("chunk is not JSON: %v\n%s", err, c.data)
		}
		if chunk.Object != "chat.completion.chunk" {
			t.Errorf("object = %q, want chat.completion.chunk", chunk.Object)
		}
		if len(chunk.Choices) == 0 {
			if chunk.Usage != nil {
				usage = chunk.Usage
			}
			continue
		}
		if s, ok := chunk.Choices[0].Delta["content"].(string); ok {
			text.WriteString(s)
		}
		if chunk.Choices[0].FinishReason != nil {
			finish = *chunk.Choices[0].FinishReason
		}
	}

	if text.String() != "Hello" {
		t.Errorf("streamed text = %q, want Hello", text.String())
	}
	if finish != "stop" {
		t.Errorf("finish_reason = %q, want stop from Anthropic's end_turn", finish)
	}
	if done != 1 {
		t.Errorf("stream carried %d [DONE] terminators, want exactly 1", done)
	}
	if usage == nil || usage["prompt_tokens"] != float64(3) || usage["completion_tokens"] != float64(2) {
		t.Errorf("usage chunk = %v, want the counts when stream_options.include_usage was set", usage)
	}
}

// TestAnthropicStreamTerminatesWhenCutShort covers the upstream that stops
// talking: whatever the client has earned so far must still end in a finish
// reason and a single [DONE], or an SDK waits for a frame that never comes.
func TestAnthropicStreamTerminatesWhenCutShort(t *testing.T) {
	ts := anthropicRouter(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, ev := range []struct{ name, data string }{
			{"message_start", `{"type":"message_start","message":{"id":"msg_1","model":"claude-test"}}`},
			{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"par"}}`},
			// No message_delta, no message_stop: the connection just ends.
		} {
			io.WriteString(w, "event: "+ev.name+"\ndata: "+ev.data+"\n\n")
			flusher.Flush()
		}
	})

	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var text strings.Builder
	var finish string
	done := 0
	for _, c := range readSSE(t, string(raw)) {
		if c.data == "[DONE]" {
			done++
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta        map[string]any `json:"delta"`
				FinishReason *string        `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(c.data), &chunk); err != nil || len(chunk.Choices) == 0 {
			continue
		}
		if s, ok := chunk.Choices[0].Delta["content"].(string); ok {
			text.WriteString(s)
		}
		if chunk.Choices[0].FinishReason != nil {
			finish = *chunk.Choices[0].FinishReason
		}
	}
	if text.String() != "par" {
		t.Errorf("streamed text = %q, want what arrived before the cut", text.String())
	}
	if finish != "stop" {
		t.Errorf("finish_reason = %q, want the router to finish a cut-short stream", finish)
	}
	if done != 1 {
		t.Errorf("stream carried %d [DONE] terminators, want exactly 1", done)
	}
}

// TestAnthropicMessagesPassthroughStillWorks guards the native route: a client
// that already speaks Anthropic posts to /v1/messages and must not be touched
// by the translator at all.
func TestAnthropicMessagesPassthroughStillWorks(t *testing.T) {
	var got string
	ts := anthropicRouter(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got = string(raw)
		io.WriteString(w, anthropicAnswer)
	})

	const original = `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"ping"}]}`
	resp, err := http.Post(ts.URL+"/v1/messages", "application/json", strings.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200. body %s", resp.StatusCode, raw)
	}
	if got != original {
		t.Errorf("upstream body was rewritten:\n got %s\nwant %s", got, original)
	}
	if !strings.Contains(string(raw), "msg_1") {
		t.Errorf("answer = %s, want the provider's own body back", raw)
	}
}

// TestAnthropicProviderErrorsPassThroughUntranslated is the same rule the
// OpenAI translators hold: a refusal stays in the provider's own words.
func TestAnthropicProviderErrorsPassThroughUntranslated(t *testing.T) {
	ts := anthropicRouter(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"model not found"}}`)
	})

	status, body := postChat(t, ts, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status %d, want the provider's 400", status)
	}
	if !strings.Contains(body, "model not found") {
		t.Errorf("body = %s, want the provider's own error", body)
	}
}

// TestAnthropicBodiesThatCannotBeTranslatedAreRefused is the honest-failure
// rule: a chat body with nothing to translate is refused here by name, rather
// than forwarded as a Messages request the provider can only reject.
func TestAnthropicBodiesThatCannotBeTranslatedAreRefused(t *testing.T) {
	var reached atomic.Bool
	ts := anthropicRouter(t, func(w http.ResponseWriter, r *http.Request) {
		reached.Store(true)
		io.WriteString(w, anthropicAnswer)
	})

	status, body := postChat(t, ts, `{"model":"m"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status %d, want 400. body %s", status, body)
	}
	if !strings.Contains(body, "messages") {
		t.Errorf("error does not name the missing field: %s", body)
	}
	if reached.Load() {
		t.Error("a body with nothing to translate reached the provider anyway")
	}
}
