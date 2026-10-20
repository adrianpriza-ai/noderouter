package tests

// Request fields the translator does not name, and the stream edges it used to
// smooth over. Split out from translate_test.go because they are the same
// subject from the other end: that file covers what the translator understands,
// this one covers what it must leave alone and what it owes a client when the
// upstream is not well behaved.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// captureBody wires a provider that records the body it was sent and then
// answers with whatever the caller wants to return. Recording is the assertion
// here: every field test is about what crossed, not what came back.
func captureBody(t *testing.T, got *map[string]any, answer string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Errorf("upstream body is not a JSON object: %v\n%s", err, raw)
		}
		*got = parsed
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, answer)
	}
}

const chatAnswer = `{"id":"chatcmpl-1","object":"chat.completion","created":1700000000,
	"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},
	"finish_reason":"stop"}]}`

const responsesAnswer = `{"id":"resp_1","object":"response","status":"completed","model":"m",
	"created_at":1700000000,
	"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`

// TestUnlistedFieldsCrossUntouched is the other half of the refuse lists. Those
// cover the fields that belong to one wire and mean nothing on the other; a
// field in neither list is a provider extra (top_k, enable_thinking,
// repetition_penalty) or a wrapper's own knob, and the router has no idea what
// it means. Dropping it silently is the outcome the refuse lists exist to
// prevent - a caller gets an answer to a question they did not ask and cannot
// tell - so it crosses untouched and the provider, the only party that knows,
// gets to accept or reject it.
func TestUnlistedFieldsCrossUntouched(t *testing.T) {
	extras := `"top_k":40,"enable_thinking":true,"repetition_penalty":1.1,
		"vendor_extra":{"a":[1,2]},"safety":{"nested":{"deep":true}}`

	cases := []struct {
		name    string
		options ProviderOptions
		path    string
		prefix  string
		answer  string
	}{
		{
			name:    "chat to responses",
			options: ProviderOptions{SurfaceName: "responses"},
			path:    "/v1/chat/completions",
			prefix:  `{"model":"m","messages":[{"role":"user","content":"hi"}],`,
			answer:  responsesAnswer,
		},
		{
			name:    "responses to chat",
			options: ProviderOptions{},
			path:    "/v1/responses",
			prefix:  `{"model":"m","input":"hi",`,
			answer:  chatAnswer,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]any
			ts := surfaceTestServer(t, tc.options, captureBody(t, &got, tc.answer))

			status, body := postJSON(t, ts, tc.path, tc.prefix+extras+"}")
			if status != http.StatusOK {
				t.Fatalf("status %d, want 200. body %s", status, body)
			}
			if got == nil {
				t.Fatal("request never reached the provider")
			}
			for name, want := range decode(t, "{"+extras+"}") {
				if !reflect.DeepEqual(got[name], want) {
					t.Errorf("upstream %s = %#v, want %#v (an unlisted field has to cross)", name, got[name], want)
				}
			}
		})
	}
}

// TestUnlistedButUnsetFieldsAreNotForwarded is the other half of that rule: a
// field present but null or empty is how a client spells "leave this alone",
// and forwarding it would turn a body that asks for nothing into one that
// carries an argument the provider has to make sense of. A meaningful zero is
// still a value, though - repetition_penalty: 0 is a real request.
func TestUnlistedButUnsetFieldsAreNotForwarded(t *testing.T) {
	unset := `"think":null,"nothing":[],"blank":{},"repetition_penalty":0`

	for _, tc := range []struct {
		name    string
		options ProviderOptions
		path    string
		prefix  string
		answer  string
	}{
		{"chat to responses", ProviderOptions{SurfaceName: "responses"}, "/v1/chat/completions",
			`{"model":"m","messages":[{"role":"user","content":"hi"}],`, responsesAnswer},
		{"responses to chat", ProviderOptions{}, "/v1/responses",
			`{"model":"m","input":"hi",`, chatAnswer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]any
			ts := surfaceTestServer(t, tc.options, captureBody(t, &got, tc.answer))

			status, body := postJSON(t, ts, tc.path, tc.prefix+unset+"}")
			if status != http.StatusOK {
				t.Fatalf("status %d, want 200. body %s", status, body)
			}
			for _, name := range []string{"think", "nothing", "blank"} {
				if v, ok := got[name]; ok {
					t.Errorf("upstream carried %s = %#v; a client sending nothing must not make it a request for something", name, v)
				}
			}
			if got["repetition_penalty"] != float64(0) {
				t.Errorf("upstream repetition_penalty = %#v, want 0 - a zero is a value, not an unset default", got["repetition_penalty"])
			}
		})
	}
}

// TestTranslatedNamesAreNotClobberedByTheirOwnSource guards the one way
// passing unlisted fields through could do harm: a body that carries both a
// field the translator reads and the name that field is written under. The
// conversion has to win, or the client's copy of it replaces what was just
// built - a body with messages and input would end up answered from the
// client's input, with the messages silently ignored.
func TestTranslatedNamesAreNotClobberedByTheirOwnSource(t *testing.T) {
	var got map[string]any
	ts := surfaceTestServer(t, ProviderOptions{SurfaceName: "responses"},
		captureBody(t, &got, responsesAnswer))

	// input is the Responses-shaped conversation, messages the chat one. Only
	// messages may become the conversation that goes upstream.
	status, body := postJSON(t, ts, "/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"from messages"}],
		  "input":[{"role":"user","content":"from input"}],
		  "max_output_tokens":5,"max_completion_tokens":7,
		  "text":{"format":{"type":"json_object"}},
		  "reasoning":{"effort":"low"},"reasoning_effort":"high"}`)
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200. body %s", status, body)
	}
	if !strings.Contains(string(mustMarshalAny(t, got["input"])), "from messages") {
		t.Errorf("upstream input = %#v, want the messages converted into it", got["input"])
	}
	if got["max_output_tokens"] != float64(7) {
		t.Errorf("upstream max_output_tokens = %#v, want 7 from max_completion_tokens", got["max_output_tokens"])
	}
	reasoning, _ := got["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" {
		t.Errorf("upstream reasoning = %#v, want the effort from reasoning_effort", got["reasoning"])
	}
	if _, ok := got["reasoning_effort"]; ok {
		t.Error("the chat-only name reasoning_effort went upstream; it has no meaning on the Responses wire")
	}
}

// mustMarshalAny renders a decoded JSON value for an error message.
func mustMarshalAny(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("could not render %#v: %v", v, err)
	}
	return b
}

// TestStoreCrossesBothWays covers the asymmetry that store used to have: chat
// completions has the parameter and the Responses API has the parameter, spelled
// the same way and meaning the same thing, so chat->responses passed it through
// while responses->chat refused the request over it. A caller asking a chat
// provider not to keep its answer got a 400 instead.
func TestStoreCrossesBothWays(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options ProviderOptions
		path    string
		body    string
		answer  string
	}{
		{"responses to chat", ProviderOptions{}, "/v1/responses",
			`{"model":"m","input":"hi","store":true}`, chatAnswer},
		{"responses to chat, false", ProviderOptions{}, "/v1/responses",
			`{"model":"m","input":"hi","store":false}`, chatAnswer},
		{"chat to responses", ProviderOptions{SurfaceName: "responses"}, "/v1/chat/completions",
			`{"model":"m","messages":[{"role":"user","content":"hi"}],"store":true}`, responsesAnswer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]any
			ts := surfaceTestServer(t, tc.options, captureBody(t, &got, tc.answer))

			status, body := postJSON(t, ts, tc.path, tc.body)
			if status != http.StatusOK {
				t.Fatalf("status %d, want 200 - store is a field both wires have. body %s", status, body)
			}
			if got == nil {
				t.Fatal("request never reached the provider")
			}
			// false is still the client's decision and has to cross with it.
			want := strings.Contains(tc.body, `"store":true`)
			if got["store"] != want {
				t.Errorf("upstream store = %#v, want %v", got["store"], want)
			}
		})
	}
}

// TestResponsesOnlyFieldsAreStillRefused is the other side of moving store: the
// fields that genuinely have no chat-completions counterpart must keep coming
// back as a 400 that names them. Passing everything through (3.4) must not have
// quietly turned the refusal into a shrug.
func TestResponsesOnlyFieldsAreStillRefused(t *testing.T) {
	chatProvider := surfaceTestServer(t, ProviderOptions{},
		func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","choices":[]}`)
		})

	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"previous response", `{"model":"m","input":"x","previous_response_id":"resp_0"}`, "previous_response_id"},
		{"conversation", `{"model":"m","input":"x","conversation":"conv_1"}`, "conversation"},
		{"background", `{"model":"m","input":"x","background":true}`, "background"},
		{"include", `{"model":"m","input":"x","include":["reasoning.encrypted_content"]}`, "include"},
		{"prompt cache key", `{"model":"m","input":"x","prompt_cache_key":"k"}`, "prompt_cache_key"},
		{"reasoning summary", `{"model":"m","input":"x","reasoning":{"effort":"low","summary":"auto"}}`, "reasoning.summary"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := postJSON(t, chatProvider, "/v1/responses", tc.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status %d, want 400. body %s", status, body)
			}
			if !strings.Contains(body, tc.want) {
				t.Errorf("error does not name %s: %s", tc.want, body)
			}
		})
	}
}

// --------------------------------------------------------------- stream edges

// responsesStreamServer answers a streaming request with the given events and
// then closes, which is how a provider that cuts a stream short looks.
func responsesStreamServer(t *testing.T, events [][2]string) *httptest.Server {
	t.Helper()
	return surfaceTestServer(t, ProviderOptions{SurfaceName: "responses"},
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			for _, ev := range events {
				io.WriteString(w, "event: "+ev[0]+"\ndata: "+ev[1]+"\n\n")
				flusher.Flush()
			}
		})
}

// streamChat asks a server for a chat stream and returns its chunks.
func streamChat(t *testing.T, ts *httptest.Server, body string) []sseChunk {
	t.Helper()
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d, want 200. body %s", resp.StatusCode, raw)
	}
	raw, _ := io.ReadAll(resp.Body)
	return readSSE(t, string(raw))
}

// doneCount counts the [DONE] terminators in a translated chat stream. More
// than one is a protocol error: a client that stops reading at the first one
// leaves the rest unread, and one that reads them all sees a stream that ended
// twice.
func doneCount(chunks []sseChunk) int {
	n := 0
	for _, c := range chunks {
		if c.data == "[DONE]" {
			n++
		}
	}
	return n
}

// TestChatStreamTerminatesWhenTheUpstreamDoesNot is the missing terminator. A
// Responses stream that ends without a terminal event left the chat client with
// no [DONE]: an SDK waiting for one blocks until its own timeout, and a client
// that reads to EOF records the answer as truncated even though the router had
// translated all of it.
func TestChatStreamTerminatesWhenTheUpstreamDoesNot(t *testing.T) {
	ts := responsesStreamServer(t, [][2]string{
		{"response.created", `{"type":"response.created","response":{"id":"resp_1","model":"m","created_at":1700000000,"status":"in_progress"}}`},
		{"response.output_text.delta", `{"type":"response.output_text.delta","delta":"Hel"}`},
		{"response.output_text.delta", `{"type":"response.output_text.delta","delta":"lo"}`},
		// No response.completed: the connection just ends.
	})

	chunks := streamChat(t, ts, `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`)

	if got := doneCount(chunks); got != 1 {
		t.Errorf("stream carried %d [DONE] terminators, want exactly 1 - a chat client waits for one and errors on a second", got)
	}
	var text strings.Builder
	for _, c := range chunks {
		if c.data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(c.data), &chunk); err != nil {
			t.Fatalf("chunk is not JSON: %v\n%s", err, c.data)
		}
		if len(chunk.Choices) > 0 {
			text.WriteString(chunk.Choices[0].Delta.Content)
		}
	}
	if text.String() != "Hello" {
		t.Errorf("streamed text = %q, want Hello", text.String())
	}
}

// TestChatStreamTerminatesExactlyOnce is the guard on the fix above: a stream
// that does arrive with a terminal event already emitted its [DONE], and finish
// must not send a second.
func TestChatStreamTerminatesExactlyOnce(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events [][2]string
	}{
		{"terminal event", [][2]string{
			{"response.created", `{"type":"response.created","response":{"id":"resp_1","model":"m","created_at":1700000000,"status":"in_progress"}}`},
			{"response.output_text.delta", `{"type":"response.output_text.delta","delta":"hi"}`},
			{"response.completed", `{"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed"}}`},
		}},
		{"terminated by the upstream", [][2]string{
			{"response.created", `{"type":"response.created","response":{"id":"resp_1","model":"m","created_at":1700000000,"status":"in_progress"}}`},
			{"response.output_text.delta", `{"type":"response.output_text.delta","delta":"hi"}`},
			{"response.completed", `{"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed"}}`},
			// Some providers send [DONE] after the last event. It has to reach
			// the client as the one terminator, not add to it.
			{"", `[DONE]`},
		}},
		{"failed", [][2]string{
			{"response.created", `{"type":"response.created","response":{"id":"resp_1","model":"m","created_at":1700000000,"status":"in_progress"}}`},
			{"response.failed", `{"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"code":"server_error","message":"the model gave up"}}}`},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := responsesStreamServer(t, tc.events)
			chunks := streamChat(t, ts, `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`)
			if got := doneCount(chunks); got != 1 {
				t.Errorf("stream carried %d [DONE] terminators, want exactly 1", got)
			}
		})
	}
}

// TestFailedResponsesStreamReachesTheChatClient covers the discarded message.
// A response.failed event used to be translated into a clean finish_reason:
// stop, so a model that failed halfway - and the many gateways that answer with
// an empty stream and a failure - reached the caller as a short, successful
// answer. Chat's terminal chunk has no room for a failure, but the stream does:
// a data frame carrying an error object is what the OpenAI SDKs raise on, and
// it is the shape the in-band failure convention uses.
func TestFailedResponsesStreamReachesTheChatClient(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event [2]string
		want  string
	}{
		{
			// The documented shape: the error object sits inside the response
			// that failed.
			name:  "nested in the response",
			event: [2]string{"response.failed", `{"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"code":"server_error","message":"the model gave up"}}}`},
			want:  "the model gave up",
		},
		{
			name:  "top-level error object",
			event: [2]string{"error", `{"type":"error","code":"server_error","message":"upstream refused","param":null}`},
			want:  "upstream refused",
		},
		{
			// A provider that names the failure but not its reason still owes
			// the client a message; an empty one would say something went wrong
			// without saying what.
			name:  "no message at all",
			event: [2]string{"response.failed", `{"type":"response.failed","response":{"id":"resp_1","status":"failed"}}`},
			want:  "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := responsesStreamServer(t, [][2]string{
				{"response.created", `{"type":"response.created","response":{"id":"resp_1","model":"m","created_at":1700000000,"status":"in_progress"}}`},
				{"response.output_text.delta", `{"type":"response.output_text.delta","delta":"par"}`},
				tc.event,
			})

			chunks := streamChat(t, ts, `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`)

			var errFrame map[string]any
			for _, c := range chunks {
				if c.data == "[DONE]" {
					continue
				}
				var frame struct {
					Error  map[string]any `json:"error"`
					Object string         `json:"object"`
				}
				if err := json.Unmarshal([]byte(c.data), &frame); err != nil {
					t.Fatalf("frame is not JSON: %v\n%s", err, c.data)
				}
				if frame.Error != nil {
					errFrame = frame.Error
					continue
				}
				// A clean terminal chunk would tell the client the answer is
				// finished, which is the lie this replaces.
				if frame.Object == "chat.completion.chunk" {
					var chunk struct {
						Choices []struct {
							FinishReason *string `json:"finish_reason"`
						} `json:"choices"`
					}
					if err := json.Unmarshal([]byte(c.data), &chunk); err == nil {
						for _, choice := range chunk.Choices {
							if choice.FinishReason != nil && *choice.FinishReason != "" {
								t.Errorf("failed stream reported finish_reason %q; a failure is not a completed answer", *choice.FinishReason)
							}
						}
					}
				}
			}
			if errFrame == nil {
				t.Fatalf("stream carried no error frame, so the client cannot tell the answer failed: %+v", chunks)
			}
			msg, _ := errFrame["message"].(string)
			if msg == "" {
				t.Errorf("error frame has no message: %+v", errFrame)
			}
			if tc.want != "" && !strings.Contains(msg, tc.want) {
				t.Errorf("error message = %q, want it to carry %q", msg, tc.want)
			}
			if got := doneCount(chunks); got != 1 {
				t.Errorf("stream carried %d [DONE] terminators, want exactly 1", got)
			}
		})
	}
}

// TestToolCallIDFromALaterChunkIsUsed covers the id that never arrived. A
// provider may open a tool call with only the index and send the id in the next
// chunk; taking the id from the first chunk and never looking again left
// output_item.added, every arguments event and both terminal events with an
// empty id, which is an id no client can match a tool result to.
func TestToolCallIDFromALaterChunkIsUsed(t *testing.T) {
	ts := surfaceTestServer(t, ProviderOptions{},
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			for _, data := range []string{
				// Opens the call with nothing but an index.
				`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":""}}]},"finish_reason":null}]}`,
				// The id and the name follow.
				`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_late","type":"function","function":{"name":"f","arguments":""}}]},"finish_reason":null}]}`,
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

	// Every event after the id arrived has to carry it: the arguments deltas,
	// the arguments terminal event and output_item.done, which is what a client
	// correlates a tool result by.
	for _, c := range readSSE(t, string(raw)) {
		if c.data == "[DONE]" {
			continue
		}
		var ev struct {
			Type        string         `json:"type"`
			ItemID      string         `json:"item_id"`
			OutputIndex *int           `json:"output_index"`
			Item        map[string]any `json:"item"`
		}
		if err := json.Unmarshal([]byte(c.data), &ev); err != nil {
			t.Fatalf("event is not JSON: %v\n%s", err, c.data)
		}
		switch ev.Type {
		case "response.function_call_arguments.delta",
			"response.function_call_arguments.done":
			if ev.ItemID != "call_late" {
				t.Errorf("%s item_id = %q, want call_late", ev.Type, ev.ItemID)
			}
		case "response.output_item.done":
			if ev.Item["call_id"] != "call_late" || ev.Item["id"] != "call_late" {
				t.Errorf("output_item.done item = %+v, want the id call_late", ev.Item)
			}
			if ev.Item["name"] != "f" {
				t.Errorf("output_item.done name = %v, want f", ev.Item["name"])
			}
		}
	}
}
