package tests

// Translation between the chat-completions and Responses surfaces, and the
// media handling that goes with it.
//
// The tests here are grouped by direction: a chat client against a Responses
// provider, a Responses client against a chat provider, and the streaming case
// for each. Media gets its own section, because the rule it has to keep - a
// part the router does not understand reaches the provider byte for byte - is
// what makes an omni model usable at all.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// surfaceTestServer wires a router in front of an upstream that answers with
// whatever handler says, with the proxy pool off. options decides which
// surface the provider claims to speak.
func surfaceTestServer(t *testing.T, options ProviderOptions, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	return surfaceTestServerCfg(t, options, ServerConfig{MaxBodyMB: 1, RequestSecs: 5}, handler)
}

// surfaceTestServerCfg is surfaceTestServer with a server block of the test's
// choosing, for the cases that are about body limits.
func surfaceTestServerCfg(t *testing.T, options ProviderOptions, server ServerConfig, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)

	options.BaseURL = upstream.URL
	cfg := &Config{
		Server: server,
		Proxy:  ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt"), Enabled: boolPtr(false)},
		Retry:  noWait,
		Provider: map[string]*Provider{"P": {Options: options, Models: map[string]*Model{
			"m": {},
			// An omni model: it takes media, so the router has to let a big
			// body through for it.
			"omni": {Modalities: Modalities{
				Input:  []string{"text", "image", "audio", "video"},
				Output: []string{"text"},
			}},
		}}},
	}
	cfg.ApplyDefaults()
	srv := NewServer(cfg, NewPool(cfg.Proxy.File, "", "rotate", false), newLogger(false))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// postJSON sends a body to one endpoint and returns the status and the body.
func postJSON(t *testing.T, ts *httptest.Server, path, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(ts.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// decode parses a JSON body, failing the test with the raw text when it does
// not parse - which is itself the assertion for most of these tests.
func decode(t *testing.T, body string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, body)
	}
	return out
}

// ------------------------------------------- chat client, responses provider

// TestChatClientReachesResponsesProvider is the core case: a client that only
// speaks chat completions, a provider that only speaks Responses. The request
// must arrive upstream in the Responses shape and on the Responses path, and
// the answer must come back as a chat completion.
func TestChatClientReachesResponsesProvider(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	ts := surfaceTestServer(t, ProviderOptions{SurfaceName: "responses", APIKey: "sk-test"},
		func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &gotBody)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"resp_1","object":"response","status":"completed",
				"model":"m","created_at":1700000000,
				"output":[{"type":"message","role":"assistant","content":[
					{"type":"output_text","text":"pong"}]}],
				"usage":{"input_tokens":11,"output_tokens":2,"total_tokens":13}}`)
		})

	status, body := postJSON(t, ts, "/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"ping"}]}`)
	if status != http.StatusOK {
		t.Fatalf("status %d, body %s", status, body)
	}

	if want := "/v1/responses"; gotPath != want {
		t.Errorf("upstream path = %q, want %q", gotPath, want)
	}
	if _, ok := gotBody["input"]; !ok {
		t.Errorf("upstream body has no input: %v", gotBody)
	}
	if _, ok := gotBody["messages"]; ok {
		t.Errorf("upstream body still carries chat messages: %v", gotBody)
	}

	got := decode(t, body)
	if got["object"] != "chat.completion" {
		t.Errorf("object = %v, want chat.completion", got["object"])
	}
	choices, _ := got["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %v, want one", got["choices"])
	}
	msg, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "pong" {
		t.Errorf("content = %v, want pong", msg["content"])
	}
	usage, _ := got["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(11) || usage["completion_tokens"] != float64(2) {
		t.Errorf("usage = %v, want the Responses token counts mapped onto chat names", usage)
	}
}

// TestResponsesClientReachesChatProvider is the mirror image: a Responses
// client, a plain chat provider.
func TestResponsesClientReachesChatProvider(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	ts := surfaceTestServer(t, ProviderOptions{APIKey: "sk-test"},
		func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &gotBody)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1700000000,
				"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},
				"finish_reason":"stop"}],
				"usage":{"prompt_tokens":11,"completion_tokens":2,"total_tokens":13}}`)
		})

	status, body := postJSON(t, ts, "/v1/responses", `{"model":"m","input":"ping"}`)
	if status != http.StatusOK {
		t.Fatalf("status %d, body %s", status, body)
	}

	if want := "/v1/chat/completions"; gotPath != want {
		t.Errorf("upstream path = %q, want %q", gotPath, want)
	}
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("upstream messages = %v, want one user message", gotBody["messages"])
	}
	if _, ok := gotBody["input"]; ok {
		t.Errorf("upstream body still carries Responses input: %v", gotBody)
	}

	got := decode(t, body)
	if got["object"] != "response" {
		t.Errorf("object = %v, want response", got["object"])
	}
	output, _ := got["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output = %v, want one item", got["output"])
	}
	item, _ := output[0].(map[string]any)
	content, _ := item["content"].([]any)
	part, _ := content[0].(map[string]any)
	if part["type"] != "output_text" || part["text"] != "pong" {
		t.Errorf("output part = %v, want output_text pong", part)
	}
}

// TestSystemMessageBecomesInstructions checks the one field with a real name
// change: chat's system message is the Responses instructions field.
func TestSystemMessageBecomesInstructions(t *testing.T) {
	var got map[string]any
	ts := surfaceTestServer(t, ProviderOptions{SurfaceName: "responses"},
		func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &got)
			io.WriteString(w, `{"id":"resp_1","output":[]}`)
		})

	postJSON(t, ts, "/v1/chat/completions", `{"model":"m","messages":[
		{"role":"system","content":"be brief"},
		{"role":"user","content":"hi"}]}`)

	if got["instructions"] != "be brief" {
		t.Errorf("instructions = %v, want the system message", got["instructions"])
	}
	input, _ := got["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("input = %v, want only the user turn (the system one became instructions)", got["input"])
	}
}

// TestToolCallRoundTrips checks that a tool call survives the trip in both
// directions: chat's assistant tool_calls become Responses function_call items,
// and the function_call_output a client sends back becomes a tool message.
func TestToolCallRoundTrips(t *testing.T) {
	var got map[string]any
	ts := surfaceTestServer(t, ProviderOptions{SurfaceName: "responses"},
		func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &got)
			io.WriteString(w, `{"id":"resp_1","output":[
				{"type":"function_call","id":"fc_1","call_id":"call_abc","name":"get_weather","arguments":"{\"city\":\"Oslo\"}"}]}`)
		})

	status, body := postJSON(t, ts, "/v1/chat/completions", `{"model":"m","messages":[
		{"role":"user","content":"weather?"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_abc","type":"function",
		  "function":{"name":"get_weather","arguments":"{\"city\":\"Oslo\"}"}}]},
		{"role":"tool","tool_call_id":"call_abc","content":"7C"}]}`)
	if status != http.StatusOK {
		t.Fatalf("status %d body %s", status, body)
	}

	input, _ := got["input"].([]any)
	var sawCall, sawOutput bool
	for _, raw := range input {
		item, _ := raw.(map[string]any)
		switch item["type"] {
		case "function_call":
			sawCall = true
			if item["call_id"] != "call_abc" || item["name"] != "get_weather" {
				t.Errorf("function_call = %v, want call_id and name carried over", item)
			}
		case "function_call_output":
			sawOutput = true
			if item["call_id"] != "call_abc" || item["output"] != "7C" {
				t.Errorf("function_call_output = %v, want the tool result", item)
			}
		}
	}
	if !sawCall || !sawOutput {
		t.Errorf("input = %v, want both a function_call and a function_call_output", input)
	}

	// And back: the answer must be a chat tool_calls array.
	answer := decode(t, body)
	choices, _ := answer["choices"].([]any)
	msg, _ := choices[0].(map[string]any)["message"].(map[string]any)
	calls, _ := msg["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %v, want one", msg["tool_calls"])
	}
	call, _ := calls[0].(map[string]any)
	fn, _ := call["function"].(map[string]any)
	if fn["name"] != "get_weather" || !strings.Contains(fn["arguments"].(string), "Oslo") {
		t.Errorf("tool call = %v, want get_weather with its arguments", call)
	}
}

// TestSchemaFormatRoundTrips checks response_format and text.format, the two
// structured-output spellings.
func TestSchemaFormatRoundTrips(t *testing.T) {
	echo := func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		io.WriteString(w, `{"id":"resp_1","output":[]}`)
	}

	// chat client, responses provider
	var got map[string]any
	ts := surfaceTestServer(t, ProviderOptions{SurfaceName: "responses"},
		func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &got)
			echo(w, r)
		})

	postJSON(t, ts, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"x"}],
		"response_format":{"type":"json_schema","json_schema":{"name":"out","strict":true,
		"schema":{"type":"object"}}}}`)

	text, _ := got["text"].(map[string]any)
	format, _ := text["format"].(map[string]any)
	if format["type"] != "json_schema" || format["name"] != "out" || format["strict"] != true {
		t.Errorf("text.format = %v, want the schema carried over", text)
	}

	// Now the other way: the schema has to come back off a chat provider, so
	// this needs its own server.
	got = nil
	ts2 := surfaceTestServer(t, ProviderOptions{},
		func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &got)
			echo(w, r)
		})

	postJSON(t, ts2, "/v1/responses", `{"model":"m","input":"x",
		"text":{"format":{"type":"json_schema","name":"out","schema":{"type":"object"}}}}`)
	rf, _ := got["response_format"].(map[string]any)
	schema, _ := rf["json_schema"].(map[string]any)
	if rf["type"] != "json_schema" || schema["name"] != "out" {
		t.Errorf("response_format = %v, want the schema carried back", got["response_format"])
	}
}

// TestUnsupportedFieldsAreRefused checks the honest-failure rule: a field the
// target surface cannot express comes back as a 400 naming it, not as a
// quietly different request.
func TestUnsupportedFieldsAreRefused(t *testing.T) {
	// Each direction needs the provider on the other surface, or there is
	// nothing to translate and nothing to refuse.
	responsesProvider := surfaceTestServer(t, ProviderOptions{SurfaceName: "responses"},
		func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"id":"resp_1","output":[]}`)
		})
	chatProvider := surfaceTestServer(t, ProviderOptions{},
		func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","choices":[]}`)
		})

	cases := []struct {
		name string
		ts   *httptest.Server
		path string
		body string
		want string
	}{
		{"chat stop sequences", responsesProvider, "/v1/chat/completions",
			`{"model":"m","messages":[],"stop":["\n\n"]}`, "stop"},
		{"chat logprobs", responsesProvider, "/v1/chat/completions",
			`{"model":"m","messages":[],"logprobs":true}`, "logprobs"},
		{"chat multiple choices", responsesProvider, "/v1/chat/completions",
			`{"model":"m","messages":[],"n":3}`, "n"},
		{"responses previous id", chatProvider, "/v1/responses",
			`{"model":"m","input":"x","previous_response_id":"resp_0"}`, "previous_response_id"},
		{"responses conversation", chatProvider, "/v1/responses",
			`{"model":"m","input":"x","conversation":"conv_1"}`, "conversation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := postJSON(t, tc.ts, tc.path, tc.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status %d, want 400. body %s", status, body)
			}
			if !strings.Contains(body, tc.want) {
				t.Errorf("error does not name %s: %s", tc.want, body)
			}
		})
	}
}

// TestReasoningSummaryIsRefusedNotDropped covers the one Responses-only field
// that is not top level: the summary lives inside the reasoning object, so
// nothing that only looks at top-level names can see it. A chat provider has no
// way to summarise its reasoning at all, so a request that asks for one is
// refused by name here rather than translated with the summary quietly
// discarded. reasoning.effort, the other member of that object, does have a
// counterpart and must still cross as reasoning_effort.
func TestReasoningSummaryIsRefusedNotDropped(t *testing.T) {
	var reached atomic.Bool
	var gotBody map[string]any
	chatProvider := surfaceTestServer(t, ProviderOptions{},
		func(w http.ResponseWriter, r *http.Request) {
			reached.Store(true)
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &gotBody)
			io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1700000000,
				"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},
				"finish_reason":"stop"}]}`)
		})

	refused := []struct {
		name string
		body string
	}{
		{"summary alone", `{"model":"m","input":"x","reasoning":{"summary":"auto"}}`},
		{"summary with effort", `{"model":"m","input":"x","reasoning":{"effort":"high","summary":"concise"}}`},
		{"literal dotted key", `{"model":"m","input":"x","reasoning.summary":"auto"}`},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			reached.Store(false)
			status, body := postJSON(t, chatProvider, "/v1/responses", tc.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status %d, want 400. body %s", status, body)
			}
			if !strings.Contains(body, "reasoning.summary") {
				t.Errorf("error does not name reasoning.summary: %s", body)
			}
			if reached.Load() {
				t.Error("a request the chat wire cannot satisfy reached the provider anyway")
			}
		})
	}

	// Effort alone crosses as reasoning_effort, and a null summary is the unset
	// default a client sends anyway - neither is a reason to refuse.
	for _, tc := range []struct {
		name string
		body string
	}{
		{"effort only", `{"model":"m","input":"x","reasoning":{"effort":"high"}}`},
		{"null summary", `{"model":"m","input":"x","reasoning":{"effort":"high","summary":null}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reached.Store(false)
			gotBody = nil
			status, body := postJSON(t, chatProvider, "/v1/responses", tc.body)
			if status != http.StatusOK {
				t.Fatalf("status %d, want 200. body %s", status, body)
			}
			if !reached.Load() {
				t.Fatal("request never reached the provider")
			}
			if got := gotBody["reasoning_effort"]; got != "high" {
				t.Errorf("upstream reasoning_effort = %v, want \"high\"", got)
			}
		})
	}
}

// TestBodiesWithoutAConversationAreRefused covers the other half of that
// rule: a body with nothing to translate is refused here by name, rather than
// forwarded to a provider that can only answer with a schema error about a
// field this side never sent.
func TestBodiesWithoutAConversationAreRefused(t *testing.T) {
	var reached atomic.Bool
	responsesProvider := surfaceTestServer(t, ProviderOptions{SurfaceName: "responses"},
		func(w http.ResponseWriter, r *http.Request) {
			reached.Store(true)
			io.WriteString(w, `{"id":"resp_1","output":[]}`)
		})
	chatProvider := surfaceTestServer(t, ProviderOptions{},
		func(w http.ResponseWriter, r *http.Request) {
			reached.Store(true)
			io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","choices":[]}`)
		})

	// A chat body with no messages has no conversation to put in input.
	status, body := postJSON(t, responsesProvider, "/v1/chat/completions", `{"model":"m"}`)
	if status != http.StatusBadRequest {
		t.Errorf("status %d, want 400. body %s", status, body)
	}
	if !strings.Contains(body, "messages") {
		t.Errorf("error does not name the missing field: %s", body)
	}
	if reached.Load() {
		t.Error("a body with nothing to translate reached the provider anyway")
	}

	// A Responses body with no input has no conversation to put in messages.
	reached.Store(false)
	status, body = postJSON(t, chatProvider, "/v1/responses", `{"model":"m"}`)
	if status != http.StatusBadRequest {
		t.Errorf("status %d, want 400. body %s", status, body)
	}
	if !strings.Contains(body, "input") {
		t.Errorf("error does not name the missing field: %s", body)
	}
	if reached.Load() {
		t.Error("a body with nothing to translate reached the provider anyway")
	}
}

// TestAlreadyShapedBodiesAreForwarded is the counterpart: a body already
// written in the shape the provider reads needs no translation, so it goes
// through untouched instead of being rebuilt - which for a chat payload on
// the Responses endpoint would have replaced messages with whatever the
// instructions field alone produced.
func TestAlreadyShapedBodiesAreForwarded(t *testing.T) {
	var got map[string]any
	var gotPath string
	chatProvider := surfaceTestServer(t, ProviderOptions{},
		func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &got)
			io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","model":"m",
				"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
		})

	// Chat payload posted to /v1/responses: already chat-shaped.
	status, body := postJSON(t, chatProvider, "/v1/responses",
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200 for a body already in the provider's shape. body %s", status, body)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("upstream path = %q, want the chat endpoint", gotPath)
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 1 {
		t.Errorf("upstream body = %v, want the messages passed through untouched", got)
	}

	// Instructions with no input still translate: they become the system
	// message the chat wire expects, instead of being dropped with the rest
	// of a body that had nothing else to convert.
	got = nil
	status, body = postJSON(t, chatProvider, "/v1/responses", `{"model":"m","instructions":"be brief"}`)
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200 for an instructions-only request. body %s", status, body)
	}
	msgs, _ = got["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("upstream messages = %v, want the instructions as a system message", got["messages"])
	}
	sys, _ := msgs[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "be brief" {
		t.Errorf("message = %v, want a system message carrying the instructions", sys)
	}
}

// TestUnsetDefaultsAreNotRefused is the other half of that rule: clients send
// null and false for fields they do not use, and that is not a request for
// anything.
func TestUnsetDefaultsAreNotRefused(t *testing.T) {
	ts := surfaceTestServer(t, ProviderOptions{SurfaceName: "responses"},
		func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"id":"resp_1","output":[{"type":"message","role":"assistant",
				"content":[{"type":"output_text","text":"ok"}]}]}`)
		})

	status, body := postJSON(t, ts, "/v1/chat/completions", `{"model":"m","messages":[
		{"role":"user","content":"hi"}],"n":null,"stop":null,"logprobs":false,"seed":0,
		"frequency_penalty":0,"presence_penalty":0,"logit_bias":{}}`)
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200 for a body full of unset defaults. body %s", status, body)
	}
}

// TestTextOnlyModalitiesAreDroppedNotRefused checks that a client sending
// modalities: ["text"] - a default many wrappers set unconditionally - is not
// refused by a Responses provider, since it asks for nothing that wire lacks.
// A modality beyond text is still refused.
func TestTextOnlyModalitiesAreDroppedNotRefused(t *testing.T) {
	var got map[string]any
	ts := surfaceTestServer(t, ProviderOptions{SurfaceName: "responses"},
		func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &got)
			io.WriteString(w, `{"id":"resp_1","output":[]}`)
		})

	status, body := postJSON(t, ts, "/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"modalities":["text"]}`)
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200 for modalities:[\"text\"]. body %s", status, body)
	}
	if _, ok := got["modalities"]; ok {
		t.Errorf("modalities reached the Responses provider: %v", got)
	}

	status, body = postJSON(t, ts, "/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"modalities":["text","audio"]}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 for modalities:[\"text\",\"audio\"]. body %s", status, body)
	}
	if !strings.Contains(body, "modalities") {
		t.Errorf("error does not name modalities: %s", body)
	}
}

// TestProviderErrorsPassThroughUntranslated checks that a refusal stays in the
// provider's own dialect: the client asked in one shape, the provider refused,
// and the error is the provider's words rather than a rewritten version.
func TestProviderErrorsPassThroughUntranslated(t *testing.T) {
	ts := surfaceTestServer(t, ProviderOptions{SurfaceName: "responses"},
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"message":"rate limited","type":"rate_limit_error"}}`)
		})

	status, body := postJSON(t, ts, "/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusTooManyRequests {
		t.Fatalf("status %d, want the provider's 429", status)
	}
	if !strings.Contains(body, "rate limited") {
		t.Errorf("body = %s, want the provider's own error", body)
	}
}

// TestMatchingSurfacesAreNotTranslated checks the router stays out of the way
// when the client and the provider already agree.
func TestMatchingSurfacesAreNotTranslated(t *testing.T) {
	const original = `{"model":"m","messages":[{"role":"user","content":"hi"}],"seed":7}`
	var got string
	ts := surfaceTestServer(t, ProviderOptions{},
		func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/chat/completions" {
				t.Errorf("path = %q, want the request forwarded untouched", r.URL.Path)
			}
			raw, _ := io.ReadAll(r.Body)
			got = string(raw)
			io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","choices":[]}`)
		})

	status, _ := postJSON(t, ts, "/v1/chat/completions", original)
	if status != http.StatusOK {
		t.Fatalf("status %d, want the request to pass straight through", status)
	}
	if got != original {
		t.Errorf("upstream body was rewritten:\n got %s\nwant %s", got, original)
	}
}

// TestResponsesAgainstAnthropicIsRefused checks that a Responses client
// pointed at an Anthropic Messages provider gets a clear 400 rather than a
// translated chat body sent to an endpoint the provider does not serve.
func TestResponsesAgainstAnthropicIsRefused(t *testing.T) {
	var called atomic.Bool
	ts := surfaceTestServer(t, ProviderOptions{API: "anthropic"},
		func(w http.ResponseWriter, r *http.Request) {
			called.Store(true)
			io.WriteString(w, `{"ok":true}`)
		})

	status, body := postJSON(t, ts, "/v1/responses", `{"model":"m","input":"hi"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status %d, want 400. body %s", status, body)
	}
	if called.Load() {
		t.Errorf("the request reached the anthropic provider; it should have been refused first")
	}
	if !strings.Contains(body, "Messages API") {
		t.Errorf("error does not explain the dialect problem: %s", body)
	}
}

// TestOtherEndpointsAreNotTranslated checks the translation is scoped to the
// two endpoints that have a counterpart: embeddings, image generation, image
// edits, video and audio stay exactly as they were.
func TestOtherEndpointsAreNotTranslated(t *testing.T) {
	for _, path := range []string{"/v1/embeddings", "/v1/images/generations", "/v1/images/edits", "/v1/videos/generations", "/v1/audio/speech"} {
		t.Run(path, func(t *testing.T) {
			var got string
			ts := surfaceTestServer(t, ProviderOptions{SurfaceName: "responses"},
				func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != path {
						t.Errorf("path = %q, want %q", r.URL.Path, path)
					}
					raw, _ := io.ReadAll(r.Body)
					got = string(raw)
					io.WriteString(w, `{"ok":true}`)
				})
			postJSON(t, ts, path, `{"model":"m","seed":7}`)
			if !strings.Contains(got, `"seed":7`) {
				t.Errorf("body = %s, want it forwarded untouched", got)
			}
		})
	}
}

// TestStoredResponseLookupIsNotTranslated checks that /v1/responses/{id} is
// left alone: it is a lookup of state only the Responses side holds.
func TestStoredResponseLookupIsNotTranslated(t *testing.T) {
	var got string
	ts := surfaceTestServer(t, ProviderOptions{SurfaceName: "responses"},
		func(w http.ResponseWriter, r *http.Request) {
			got = r.URL.Path
			io.WriteString(w, `{"id":"resp_1","object":"response"}`)
		})

	postJSON(t, ts, "/v1/responses/resp_1", `{}`)
	if got != "/v1/responses/resp_1" {
		t.Errorf("path = %q, want the lookup forwarded as it came", got)
	}
}

// ------------------------------------------------------------------ streams

// sseChunk is one event read back out of a translated stream.
type sseChunk struct {
	event string
	data  string
}

// readSSE splits a translated stream into its events. Chat streams carry no
// event name, so those come back with an empty one.
func readSSE(t *testing.T, body string) []sseChunk {
	t.Helper()
	var out []sseChunk
	var cur sseChunk
	flush := func() {
		if cur.data != "" {
			out = append(out, cur)
		}
		cur = sseChunk{}
	}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSuffix(line, "\r")
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "event:"):
			cur.event = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			cur.data += strings.TrimSpace(line[len("data:"):])
		}
	}
	flush()
	return out
}

// TestChatStreamFromResponsesProvider checks a streamed answer comes back as a
// chat stream: the text deltas in order, a terminal chunk with finish_reason,
// and [DONE] last.
func TestChatStreamFromResponsesProvider(t *testing.T) {
	ts := surfaceTestServer(t, ProviderOptions{SurfaceName: "responses"},
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			// Each event's data is one line: SSE data is a single "data:" field,
			// and a wrapped payload would be a different protocol.
			for _, ev := range []struct{ name, data string }{
				{"response.created", `{"type":"response.created","response":{"id":"resp_1","model":"m","created_at":1700000000,"status":"in_progress"}}`},
				{"response.output_text.delta", `{"type":"response.output_text.delta","delta":"Hel"}`},
				{"response.output_text.delta", `{"type":"response.output_text.delta","delta":"lo"}`},
				{"response.completed", `{"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`},
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

	chunks := readSSE(t, string(raw))
	var text strings.Builder
	var sawFinish, sawDone bool
	for _, c := range chunks {
		if c.data == "[DONE]" {
			sawDone = true
			continue
		}
		var payload struct {
			Object  string `json:"object"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(c.data), &payload); err != nil {
			t.Fatalf("chunk is not JSON: %v\n%s", err, c.data)
		}
		if payload.Object != "chat.completion.chunk" {
			t.Errorf("object = %q, want chat.completion.chunk", payload.Object)
		}
		if len(payload.Choices) == 0 {
			t.Fatalf("chunk with no choices: %s", c.data)
		}
		text.WriteString(payload.Choices[0].Delta.Content)
		if payload.Choices[0].FinishReason != nil {
			sawFinish = true
		}
	}
	if text.String() != "Hello" {
		t.Errorf("streamed text = %q, want Hello", text.String())
	}
	if !sawFinish {
		t.Error("no chunk carried a finish_reason")
	}
	if !sawDone {
		t.Error("stream did not end with [DONE]")
	}
}

// TestStreamAcceptsCRLFFraming covers a gateway or provider that frames SSE
// with CRLF: the old "\n\n" split left the whole stream in one unparsed block
// and emitted no events at all. The final event is also left without a trailing
// blank line, which the translator must flush at EOF rather than drop.
func TestStreamAcceptsCRLFFraming(t *testing.T) {
	ts := surfaceTestServer(t, ProviderOptions{SurfaceName: "responses"},
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			frames := []struct{ name, data string }{
				{"response.created", `{"type":"response.created","response":{"id":"resp_1","model":"m","status":"in_progress"}}`},
				{"response.output_text.delta", `{"type":"response.output_text.delta","delta":"Hel"}`},
				{"response.output_text.delta", `{"type":"response.output_text.delta","delta":"lo"}`},
				{"response.completed", `{"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed"}}`},
			}
			for i, ev := range frames {
				sep := "\r\n\r\n"
				if i == len(frames)-1 {
					// The stream ends right after the last data line.
					sep = ""
				}
				io.WriteString(w, "event: "+ev.name+"\r\ndata: "+ev.data+sep)
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

	chunks := readSSE(t, string(raw))
	var text strings.Builder
	sawDone := false
	for _, c := range chunks {
		if c.data == "[DONE]" {
			sawDone = true
			continue
		}
		var payload struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(c.data), &payload); err != nil {
			t.Fatalf("chunk is not JSON: %v\n%s", err, c.data)
		}
		if len(payload.Choices) > 0 {
			text.WriteString(payload.Choices[0].Delta.Content)
		}
	}
	if text.String() != "Hello" {
		t.Errorf("CRLF streamed text = %q, want Hello", text.String())
	}
	if !sawDone {
		t.Error("CRLF stream did not end with [DONE] (final event dropped?)")
	}
}

// TestResponsesStreamFromChatProvider is the mirror image, and the more
// demanding one: a chat stream has to be dressed up as a Responses event
// sequence, including the events a Responses client waits for.
func TestResponsesStreamFromChatProvider(t *testing.T) {
	ts := surfaceTestServer(t, ProviderOptions{},
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			for _, data := range []string{
				`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
				`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":"m","choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}`,
				`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":"m","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}`,
				`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
				`[DONE]`,
			} {
				io.WriteString(w, "data: "+data+"\n\n")
				flusher.Flush()
			}
		})

	resp, err := http.Post(ts.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"m","input":"hi","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	events := readSSE(t, string(raw))
	seen := map[string]bool{}
	var text strings.Builder
	for _, c := range events {
		seen[c.event] = true
		if c.event == "response.output_text.delta" {
			var payload struct {
				Delta string `json:"delta"`
			}
			_ = json.Unmarshal([]byte(c.data), &payload)
			text.WriteString(payload.Delta)
		}
	}
	if text.String() != "Hello" {
		t.Errorf("streamed text = %q, want Hello", text.String())
	}
	// A Responses client reads response.created to learn the id and waits for
	// response.completed to learn the answer is finished.
	for _, want := range []string{"response.created", "response.output_text.delta", "response.completed"} {
		if !seen[want] {
			t.Errorf("no %s event in the translated stream: got %v", want, keys(seen))
		}
	}
}

// TestResponsesStreamCarriesToolCalls checks the function_call events a tool
// using Responses client needs are present and that the arguments accumulate
// into a finished call.
func TestResponsesStreamCarriesToolCalls(t *testing.T) {
	ts := surfaceTestServer(t, ProviderOptions{},
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			for _, data := range []string{
				`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
				`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":""}}]},"finish_reason":null}]}`,
				`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":1}"}}]},"finish_reason":null}]}`,
				`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				`[DONE]`,
			} {
				io.WriteString(w, "data: "+data+"\n\n")
				flusher.Flush()
			}
		})

	resp, err := http.Post(ts.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"m","input":"hi","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var sawAdded, sawArgs string
	for _, c := range readSSE(t, string(raw)) {
		switch c.event {
		case "response.output_item.added":
			var p struct {
				Item struct {
					Type string `json:"type"`
					Name string `json:"name"`
				} `json:"item"`
			}
			_ = json.Unmarshal([]byte(c.data), &p)
			if p.Item.Type != "function_call" || p.Item.Name != "f" {
				t.Errorf("output_item.added item = %+v, want a function_call named f", p.Item)
			}
			sawAdded = c.data
		case "response.function_call_arguments.delta":
			var p struct {
				Delta string `json:"delta"`
			}
			_ = json.Unmarshal([]byte(c.data), &p)
			sawArgs += p.Delta
		}
	}
	if sawAdded == "" {
		t.Error("no response.output_item.added for the tool call")
	}
	if !strings.Contains(sawArgs, `"a":1`) {
		t.Errorf("argument deltas = %q, want the streamed arguments", sawArgs)
	}
}

// TestToolCallTerminalEventsAreOrdered checks the tail of a tool-calling
// stream: the terminal events come out in the order the calls were added in
// (ranging the call map reshuffled them run to run), each carries its own
// output_index, and every output_item.added is answered by an
// output_item.done - which is what tells a client tracking items by
// output_index that the call finished.
func TestToolCallTerminalEventsAreOrdered(t *testing.T) {
	ts := surfaceTestServer(t, ProviderOptions{},
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			// Two calls, announced in order and fed arguments out of order
			// afterwards, so only the translator's own bookkeeping can put
			// the tail right.
			for _, data := range []string{
				`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
				`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":""}}]},"finish_reason":null}]}`,
				`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_2","type":"function","function":{"name":"g","arguments":""}}]},"finish_reason":null}]}`,
				`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":1}"}}]},"finish_reason":null}]}`,
				`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"b\":2}"}}]},"finish_reason":null}]}`,
				`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				`[DONE]`,
			} {
				io.WriteString(w, "data: "+data+"\n\n")
				flusher.Flush()
			}
		})

	// Map iteration order is randomised per range, so one run can pass by
	// luck: repeat until a reshuffled sequence would have shown itself.
	for run := 0; run < 10; run++ {
		resp, err := http.Post(ts.URL+"/v1/responses", "application/json",
			strings.NewReader(`{"model":"m","input":"hi","stream":true}`))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var addedIndex, doneIndex, itemDoneIndex []float64
		var addedNames, itemDoneNames, doneArgs []string
		var terminal []string
		for _, c := range readSSE(t, string(raw)) {
			switch c.event {
			case "response.output_item.added":
				var p struct {
					OutputIndex float64 `json:"output_index"`
					Item        struct {
						Type string `json:"type"`
						Name string `json:"name"`
					} `json:"item"`
				}
				_ = json.Unmarshal([]byte(c.data), &p)
				addedIndex = append(addedIndex, p.OutputIndex)
				addedNames = append(addedNames, p.Item.Name)
			case "response.function_call_arguments.done":
				var p struct {
					OutputIndex float64 `json:"output_index"`
					Arguments   string  `json:"arguments"`
				}
				_ = json.Unmarshal([]byte(c.data), &p)
				doneIndex = append(doneIndex, p.OutputIndex)
				doneArgs = append(doneArgs, p.Arguments)
				terminal = append(terminal, fmt.Sprintf("arguments.done:%g", p.OutputIndex))
			case "response.output_item.done":
				var p struct {
					OutputIndex float64 `json:"output_index"`
					Item        struct {
						Name string `json:"name"`
					} `json:"item"`
				}
				_ = json.Unmarshal([]byte(c.data), &p)
				itemDoneIndex = append(itemDoneIndex, p.OutputIndex)
				itemDoneNames = append(itemDoneNames, p.Item.Name)
				terminal = append(terminal, fmt.Sprintf("output_item.done:%g", p.OutputIndex))
			}
		}

		if got := fmt.Sprint(addedIndex); got != "[0 1]" {
			t.Fatalf("run %d: added output_index = %s, want the calls in the order they appeared", run, got)
		}
		if got := fmt.Sprint(addedNames); got != "[f g]" {
			t.Errorf("run %d: added items = %s, want f then g", run, got)
		}
		if got := fmt.Sprint(terminal); got != "[arguments.done:0 output_item.done:0 arguments.done:1 output_item.done:1]" {
			t.Errorf("run %d: terminal sequence = %s", run, got)
		}
		if got := fmt.Sprint(doneIndex); got != "[0 1]" {
			t.Errorf("run %d: arguments.done output_index = %s, want one per call in order", run, got)
		}
		if got := fmt.Sprint(doneArgs); got != "[{\"a\":1} {\"b\":2}]" {
			t.Errorf("run %d: arguments = %s, want each call's own arguments", run, got)
		}
		if got := fmt.Sprint(itemDoneIndex); got != "[0 1]" {
			t.Errorf("run %d: output_item.done output_index = %s, want one per added item", run, got)
		}
		if got := fmt.Sprint(itemDoneNames); got != "[f g]" {
			t.Errorf("run %d: output_item.done items = %s, want f then g", run, got)
		}
	}
}

// TestStreamWithoutTerminalEventStillFinishes checks a provider that cuts the
// stream without a terminal event does not leave the client hanging: the
// router emits the terminal event the client's dialect requires.
func TestStreamWithoutTerminalEventStillFinishes(t *testing.T) {
	ts := surfaceTestServer(t, ProviderOptions{},
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: "+`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`+"\n\n")
			w.(http.Flusher).Flush()
			// No [DONE], no finish_reason: the connection just ends.
		})

	resp, err := http.Post(ts.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"m","input":"hi","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var sawCompleted bool
	for _, c := range readSSE(t, string(raw)) {
		if c.event == "response.completed" {
			sawCompleted = true
		}
	}
	if !sawCompleted {
		t.Errorf("no terminal event after an unterminated upstream stream: %s", raw)
	}
}

// TestTranslatedRequestsKeepModel is the regression guard for the model id
// crossing the translation. Both translators rebuild the request from a
// pass-through list, and "model" used to be in neither list nor copied, so a
// translated body went upstream with no id at all - a 400 from any provider
// that validates its input, or worse, a silent fallback to the provider's
// default. The upstream must see the id in both directions, streamed and not.
func TestTranslatedRequestsKeepModel(t *testing.T) {
	cases := []struct {
		name    string
		options ProviderOptions
		path    string
		body    string
		handler http.HandlerFunc
	}{
		{
			name:    "chat to responses",
			options: ProviderOptions{SurfaceName: "responses", APIKey: "sk-test"},
			path:    "/v1/chat/completions",
			body:    `{"model":"m","messages":[{"role":"user","content":"ping"}]}`,
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"id":"resp_1","object":"response","status":"completed",
					"model":"m","created_at":1700000000,
					"output":[{"type":"message","role":"assistant","content":[
						{"type":"output_text","text":"pong"}]}]}`)
			},
		},
		{
			name:    "responses to chat",
			options: ProviderOptions{APIKey: "sk-test"},
			path:    "/v1/responses",
			body:    `{"model":"m","input":"ping"}`,
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1700000000,
					"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},
					"finish_reason":"stop"}]}`)
			},
		},
		{
			name:    "chat to responses, streamed",
			options: ProviderOptions{SurfaceName: "responses"},
			path:    "/v1/chat/completions",
			body:    `{"model":"m","messages":[{"role":"user","content":"ping"}],"stream":true}`,
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				flusher := w.(http.Flusher)
				for _, ev := range []struct{ name, data string }{
					{"response.created", `{"type":"response.created","response":{"id":"resp_1","model":"m","created_at":1700000000,"status":"in_progress"}}`},
					{"response.output_text.delta", `{"type":"response.output_text.delta","delta":"pong"}`},
					{"response.completed", `{"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed"}}`},
				} {
					io.WriteString(w, "event: "+ev.name+"\ndata: "+ev.data+"\n\n")
					flusher.Flush()
				}
			},
		},
		{
			name:    "responses to chat, streamed",
			options: ProviderOptions{},
			path:    "/v1/responses",
			body:    `{"model":"m","input":"ping","stream":true}`,
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				flusher := w.(http.Flusher)
				for _, data := range []string{
					`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"pong"},"finish_reason":null}]}`,
					`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
					`[DONE]`,
				} {
					io.WriteString(w, "data: "+data+"\n\n")
					flusher.Flush()
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var sawModel any
			capture := func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				var parsed map[string]any
				_ = json.Unmarshal(raw, &parsed)
				sawModel = parsed["model"]
				tc.handler(w, r)
			}
			ts := surfaceTestServer(t, tc.options, capture)

			status, body := postJSON(t, ts, tc.path, tc.body)
			if status != http.StatusOK {
				t.Fatalf("status %d, body %s", status, body)
			}
			if sawModel != "m" {
				t.Errorf("translated request went upstream with model = %v, want \"m\" (the id must cross the translation)", sawModel)
			}
		})
	}
}

// keys lists a set's members, for error messages.
func keys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}

// -------------------------------------------------------------------- media

// TestAudioInputIsPassedThroughByteForByte is the media test that matters
// most: base64 audio is data the model has to hear exactly, so the router must
// not touch it while changing the surrounding shape.
func TestAudioInputIsPassedThroughByteForByte(t *testing.T) {
	// A recognisable, deliberately non-round payload: if anything decoded and
	// re-encoded it, these bytes would not come back the same.
	const audio = "QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVphYmNkZWZnaGlqa2xtbm9wcXJzdA==//79"
	var got map[string]any
	ts := surfaceTestServer(t, ProviderOptions{SurfaceName: "responses"},
		func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &got)
			io.WriteString(w, `{"id":"resp_1","output":[{"type":"message","role":"assistant",
				"content":[{"type":"output_text","text":"heard it"}]}]}`)
		})

	status, body := postJSON(t, ts, "/v1/chat/completions", `{"model":"omni","messages":[
		{"role":"user","content":[
			{"type":"input_audio","input_audio":{"data":"`+audio+`","format":"wav"}}]}]}`)
	if status != http.StatusOK {
		t.Fatalf("status %d body %s", status, body)
	}

	input, _ := got["input"].([]any)
	item, _ := input[0].(map[string]any)
	parts, _ := item["content"].([]any)
	part, _ := parts[0].(map[string]any)
	ia, _ := part["input_audio"].(map[string]any)
	if ia["data"] != audio {
		t.Errorf("audio data = %q, want it byte for byte", ia["data"])
	}
	if ia["format"] != "wav" {
		t.Errorf("audio format = %v, want wav", ia["format"])
	}
}

// TestVideoPartIsPassedThroughUnchanged covers the video case: no standard
// video part exists on either surface, so the router has no mapping and must
// not invent one.
func TestVideoPartIsPassedThroughUnchanged(t *testing.T) {
	var got map[string]any
	ts := surfaceTestServer(t, ProviderOptions{SurfaceName: "responses"},
		func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &got)
			io.WriteString(w, `{"id":"resp_1","output":[]}`)
		})

	postJSON(t, ts, "/v1/chat/completions", `{"model":"omni","messages":[
		{"role":"user","content":[
			{"type":"video_url","video_url":{"url":"https://example.com/clip.mp4","detail":"high"}},
			{"type":"video","mime_type":"video/mp4","data":"AAAA"}]}]}`)

	input, _ := got["input"].([]any)
	item, _ := input[0].(map[string]any)
	parts, _ := item["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("content = %v, want both video parts", item["content"])
	}
	first, _ := parts[0].(map[string]any)
	if first["type"] != "video_url" {
		t.Errorf("first part type = %v, want video_url untouched", first["type"])
	}
	vu, _ := first["video_url"].(map[string]any)
	if vu["url"] != "https://example.com/clip.mp4" || vu["detail"] != "high" {
		t.Errorf("video_url = %v, want its fields intact", vu)
	}
	second, _ := parts[1].(map[string]any)
	if second["mime_type"] != "video/mp4" || second["data"] != "AAAA" {
		t.Errorf("second part = %v, want its fields intact", second)
	}
}

// TestImagePartIsTranslated checks the one media part that does have a mapping
// on both sides: chat's image_url object becomes the Responses bare string.
func TestImagePartIsTranslated(t *testing.T) {
	var got map[string]any
	ts := surfaceTestServer(t, ProviderOptions{SurfaceName: "responses"},
		func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &got)
			io.WriteString(w, `{"id":"resp_1","output":[]}`)
		})

	postJSON(t, ts, "/v1/chat/completions", `{"model":"omni","messages":[
		{"role":"user","content":[
			{"type":"image_url","image_url":{"url":"https://example.com/a.png"},"detail":"low"}]}]}`)

	input, _ := got["input"].([]any)
	item, _ := input[0].(map[string]any)
	parts, _ := item["content"].([]any)
	part, _ := parts[0].(map[string]any)
	if part["type"] != "input_image" {
		t.Fatalf("part type = %v, want input_image", part["type"])
	}
	if part["image_url"] != "https://example.com/a.png" {
		t.Errorf("image_url = %v, want the bare URL string", part["image_url"])
	}
	if part["detail"] != "low" {
		t.Errorf("detail = %v, want low carried over", part["detail"])
	}
}

// TestMediaBodyGetsTheLargerCeiling checks the size rule: a body carrying audio
// is allowed past maxBodyMb, because that is what an omni model is asked for.
func TestMediaBodyGetsTheLargerCeiling(t *testing.T) {
	var reached bool
	ts := surfaceTestServer(t, ProviderOptions{SurfaceName: "responses"},
		func(w http.ResponseWriter, r *http.Request) {
			reached = true
			_, _ = io.Copy(io.Discard, r.Body)
			io.WriteString(w, `{"id":"resp_1","output":[]}`)
		})

	// A 2MB payload against a 1MB ceiling: over the ordinary limit, under the
	// media one. The audio field is what makes it media.
	pad := strings.Repeat("A", 2<<20)
	status, body := postJSON(t, ts, "/v1/chat/completions", `{"model":"omni","messages":[
		{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"`+pad+`","format":"wav"}}]}]}`)
	if status != http.StatusOK {
		t.Errorf("status %d, want the media ceiling to allow a 2MB audio body. body %s", status, body)
	}
	if !reached {
		t.Error("the upstream was never reached, so the ceiling was not lifted")
	}
}

// TestHiddenMediaModelGetsTheLargerCeiling covers a hidden provider/model that
// still routes. The media ceiling has to open for it too, or a correctly routed
// request is refused at the size gate. Hidden entries are excluded from the
// advertised model table, which is what the old lookup consulted.
func TestHiddenMediaModelGetsTheLargerCeiling(t *testing.T) {
	var reached bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		_, _ = io.Copy(io.Discard, r.Body)
		io.WriteString(w, `{"id":"resp_1","output":[]}`)
	}))
	defer upstream.Close()

	cfg := &Config{
		Server: ServerConfig{MaxBodyMB: 1, MaxMediaBodyMB: 16, RequestSecs: 5},
		Proxy:  ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt"), Enabled: boolPtr(false)},
		Retry:  noWait,
		Provider: map[string]*Provider{"ghost": {
			Hidden:  true,
			Options: ProviderOptions{BaseURL: upstream.URL, SurfaceName: "responses"},
			Models: map[string]*Model{
				"omni": {Hidden: true, Modalities: Modalities{
					Input:  []string{"text", "audio"},
					Output: []string{"text"},
				}},
			},
		}},
	}
	cfg.ApplyDefaults()
	srv := NewServer(cfg, NewPool(cfg.Proxy.File, "", "rotate", false), newLogger(false))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	pad := strings.Repeat("A", 2<<20)
	status, body := postJSON(t, ts, "/v1/chat/completions",
		`{"model":"omni","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"`+pad+`","format":"wav"}}]}]}`)
	if status != http.StatusOK {
		t.Errorf("status %d, want the hidden media model to get the larger ceiling. body %s", status, body)
	}
	if !reached {
		t.Error("the upstream was never reached, so the hidden media model was refused at the size gate")
	}

	// Hidden entries are unadvertised, not unreachable: an exact key still
	// resolves through the per-model endpoint.
	resp, err := http.Get(ts.URL + "/v1/models/omni")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /v1/models/omni = %d, want 200 for a hidden model resolved by key", resp.StatusCode)
	}
}

// TestNonMediaBodyKeepsTheOrdinaryCeiling is the other side: lifting the
// ceiling for media must not lift it for everything.
func TestNonMediaBodyKeepsTheOrdinaryCeiling(t *testing.T) {
	ts := surfaceTestServer(t, ProviderOptions{SurfaceName: "responses"},
		func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"id":"resp_1","output":[]}`)
		})

	// Same size, but naming a text-only model and carrying no media part.
	pad := strings.Repeat("A", 2<<20)
	status, body := postJSON(t, ts, "/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"`+pad+`"}]}`)
	if status != http.StatusRequestEntityTooLarge {
		t.Errorf("status %d, want a text-only request refused at the ordinary ceiling. body %s", status, body)
	}
	if !strings.Contains(body, "over 1MB") {
		t.Errorf("error = %s, want it to name the ordinary ceiling", body)
	}
}

// TestMediaModalitiesAreAdvertised checks /v1/models tells a client an omni
// model takes audio and video, which is how a client decides to send any.
func TestMediaModalitiesAreAdvertised(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{MaxBodyMB: 1, RequestSecs: 5},
		Proxy:  ProxyConfig{File: filepath.Join(t.TempDir(), "none.txt"), Enabled: boolPtr(false)},
		Provider: map[string]*Provider{"P": {
			Options: ProviderOptions{BaseURL: "http://x/v1", SurfaceName: "responses"},
			Models: map[string]*Model{
				"omni": {Modalities: Modalities{
					Input:  []string{"Text", "Image", "audio", " Video "},
					Output: []string{"text"},
				}},
			},
		}},
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
			ID         string   `json:"id"`
			Kind       string   `json:"kind"`
			Surface    string   `json:"surface"`
			Accepts    []string `json:"accepts"`
			Modalities struct {
				Input []string `json:"input"`
			} `json:"modalities"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list.Data) != 1 {
		t.Fatalf("got %d models, want 1", len(list.Data))
	}
	card := list.Data[0]
	want := map[string]bool{"text": true, "image": true, "audio": true, "video": true}
	for _, got := range card.Modalities.Input {
		if !want[got] {
			t.Errorf("input modality %q not canonicalised (got %v)", got, card.Modalities.Input)
		}
		delete(want, got)
	}
	if len(want) != 0 {
		t.Errorf("input modalities missing %v, got %v", want, card.Modalities.Input)
	}
	// An omni model is still a chat model: it answers on both surfaces.
	if card.Kind != kindChat {
		t.Errorf("kind = %q, want chat for a model that takes media", card.Kind)
	}
	if card.Surface != "responses" {
		t.Errorf("surface = %q, want responses", card.Surface)
	}
	if len(card.Accepts) != 2 {
		t.Errorf("accepts = %v, want both endpoints listed", card.Accepts)
	}
}

// TestMediaModelsFallback covers the provider that does not declare per-model
// modalities: naming the model in options.mediaModels opens the media ceiling
// for it anyway.
func TestMediaModelsFallback(t *testing.T) {
	options := ProviderOptions{SurfaceName: "responses", MediaModels: []string{"omni"}}
	ts := surfaceTestServerCfg(t, options, ServerConfig{MaxBodyMB: 1, RequestSecs: 5},
		func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			io.WriteString(w, `{"id":"resp_1","output":[]}`)
		})
	// Without mediaModal: a text-only model still gets refused at 1MB.
	pad := strings.Repeat("A", 2<<20)
	status, body := postJSON(t, ts, "/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"`+pad+`"}]}`)
	if status != http.StatusRequestEntityTooLarge {
		t.Errorf("status %d, want a text-only request still refused at 1MB", status)
	}
	_ = body

	// Naming the mediaModels entry: allowed through.
	status, body = postJSON(t, ts, "/v1/chat/completions",
		`{"model":"omni","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"`+pad+`","format":"wav"}}]}]}`)
	if status != http.StatusOK {
		t.Errorf("status %d, want mediaModels to open the ceiling. body %s", status, body)
	}
}
