package anthropic

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// anthropicEvent renders one SSE event the way the Messages API sends it.
func anthropicEvent(name, data string) string {
	return "event: " + name + "\ndata: " + data + "\n\n"
}

// streamEvents is a whole Anthropic stream: a text block, then a tool call.
var streamEvents = []string{
	anthropicEvent("message_start", `{"type":"message_start","message":{"id":"msg_1","model":"claude-3","usage":{"input_tokens":21}}}`),
	anthropicEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
	anthropicEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`),
	anthropicEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`),
	anthropicEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
	anthropicEvent("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather"}}`),
	anthropicEvent("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`),
	anthropicEvent("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"Oslo\"}"}}`),
	anthropicEvent("content_block_stop", `{"type":"content_block_stop","index":1}`),
	anthropicEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":42}}`),
	anthropicEvent("message_stop", `{"type":"message_stop"}`),
}

// chatChunk is the shape the translated stream must come back in.
type chatChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int            `json:"index"`
		Delta        map[string]any `json:"delta"`
		FinishReason *string        `json:"finish_reason"`
	} `json:"choices"`
	Usage map[string]any `json:"usage"`
}

// runStream translates the given events and splits the answer into its data
// frames.
func runStream(t *testing.T, events []string, opts StreamOptions) []string {
	t.Helper()
	var out strings.Builder
	err := TranslateStream(strings.NewReader(strings.Join(events, "")), &flushBuffer{&out}, opts)
	if err != nil {
		t.Fatalf("TranslateStream: %v", err)
	}
	var frames []string
	for _, c := range readFrames(t, out.String()) {
		frames = append(frames, c)
	}
	return frames
}

// flushBuffer stands in for the router's flushing writer.
type flushBuffer struct{ s *strings.Builder }

func (f *flushBuffer) Write(p []byte) (int, error) { return f.s.Write(p) }

// readFrames pulls the data payloads out of a chat stream.
func readFrames(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, block := range strings.Split(body, "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "data:") {
				out = append(out, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
	}
	return out
}

// TestStreamTranslatesTextAndToolCalls is the main case: the text arrives as
// content deltas, the tool call as a named tool_calls entry whose arguments
// accumulate, and the stream ends with a finish reason and one [DONE].
func TestStreamTranslatesTextAndToolCalls(t *testing.T) {
	frames := runStream(t, streamEvents, StreamOptions{})

	if len(frames) == 0 {
		t.Fatal("no frames translated")
	}
	if frames[len(frames)-1] != "[DONE]" {
		t.Errorf("last frame = %q, want [DONE]", frames[len(frames)-1])
	}
	if n := strings.Count(strings.Join(frames, "\n"), "[DONE]"); n != 1 {
		t.Errorf("stream carried %d [DONE] terminators, want exactly 1", n)
	}

	var text strings.Builder
	var calls []map[string]any
	var finish string
	for _, f := range frames {
		if f == "[DONE]" {
			continue
		}
		var chunk chatChunk
		if err := json.Unmarshal([]byte(f), &chunk); err != nil {
			t.Fatalf("chunk is not JSON: %v\n%s", err, f)
		}
		if chunk.Object != "chat.completion.chunk" {
			t.Errorf("object = %q, want chat.completion.chunk", chunk.Object)
		}
		if chunk.ID != "chatcmpl-1" {
			t.Errorf("id = %q, want the msg_ id re-spelled with the chatcmpl- prefix", chunk.ID)
		}
		if chunk.Model != "claude-3" {
			t.Errorf("model = %q, want claude-3", chunk.Model)
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta
		if s, ok := delta["content"].(string); ok {
			text.WriteString(s)
		}
		if raw, ok := delta["tool_calls"].([]any); ok {
			for _, one := range raw {
				calls = append(calls, one.(map[string]any))
			}
		}
		if chunk.Choices[0].FinishReason != nil {
			finish = *chunk.Choices[0].FinishReason
		}
	}

	if text.String() != "Hello" {
		t.Errorf("streamed text = %q, want Hello", text.String())
	}
	if finish != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls from the Anthropic stop reason", finish)
	}
	if len(calls) != 3 {
		t.Fatalf("tool_call frames = %v, want the opening frame and one per argument delta", calls)
	}
	if calls[0]["id"] != "toolu_1" || calls[0]["type"] != "function" {
		t.Errorf("opening tool_call = %v, want the id and type set", calls[0])
	}
	fn, _ := calls[0]["function"].(map[string]any)
	if fn == nil || fn["name"] != "get_weather" {
		t.Errorf("function = %v, want get_weather", calls[0]["function"])
	}
	var args strings.Builder
	for _, call := range calls {
		fn, _ := call["function"].(map[string]any)
		if fn == nil {
			continue
		}
		if s, ok := fn["arguments"].(string); ok {
			args.WriteString(s)
		}
	}
	if args.String() != `{"city":"Oslo"}` {
		t.Errorf("accumulated arguments = %q, want the streamed JSON", args.String())
	}
}

// TestStreamUsageArrivesWhenAskedFor covers stream_options.include_usage: the
// usage tail chunk carries the token counts off message_start and
// message_delta, and is absent when the client did not ask.
func TestStreamUsageArrivesWhenAskedFor(t *testing.T) {
	with := runStream(t, streamEvents, StreamOptions{IncludeUsage: true})
	var last chatChunk
	for _, f := range with {
		if f == "[DONE]" {
			continue
		}
		var chunk chatChunk
		_ = json.Unmarshal([]byte(f), &chunk)
		if chunk.Usage != nil {
			last = chunk
		}
	}
	if last.Usage == nil {
		t.Fatalf("no usage chunk in the stream: %v", with)
	}
	if last.Usage["prompt_tokens"] != float64(21) || last.Usage["completion_tokens"] != float64(42) {
		t.Errorf("usage = %v, want the Anthropic counts on the chat names", last.Usage)
	}
	if last.Usage["total_tokens"] != float64(63) {
		t.Errorf("total_tokens = %v, want 63", last.Usage["total_tokens"])
	}

	without := runStream(t, streamEvents, StreamOptions{})
	for _, f := range without {
		if f == "[DONE]" {
			continue
		}
		var chunk chatChunk
		_ = json.Unmarshal([]byte(f), &chunk)
		if chunk.Usage != nil {
			t.Errorf("usage chunk sent without include_usage: %s", f)
		}
	}
}

func TestStreamUsageIncludesCachedInput(t *testing.T) {
	frames := runStream(t, []string{
		anthropicEvent("message_start", `{"type":"message_start","message":{"id":"msg_cached","model":"claude-3","usage":{"input_tokens":21,"cache_creation_input_tokens":3,"cache_read_input_tokens":5}}}`),
		anthropicEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}`),
		anthropicEvent("message_stop", `{"type":"message_stop"}`),
	}, StreamOptions{IncludeUsage: true})

	var usage map[string]any
	for _, frame := range frames {
		if frame == "[DONE]" {
			continue
		}
		var chunk chatChunk
		if err := json.Unmarshal([]byte(frame), &chunk); err != nil {
			t.Fatalf("chunk is not JSON: %v\n%s", err, frame)
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
	}
	if usage == nil {
		t.Fatalf("no usage chunk in the stream: %v", frames)
	}
	if usage["prompt_tokens"] != float64(29) || usage["completion_tokens"] != float64(42) {
		t.Errorf("usage = %v, want prompt 29 and completion 42", usage)
	}
	if usage["total_tokens"] != float64(71) {
		t.Errorf("total_tokens = %v, want 71", usage["total_tokens"])
	}
}

// TestStreamWithoutMessageStopStillTerminates covers an upstream that closes
// the connection after the deltas: the client must still get its finish
// reason and its single [DONE].
func TestStreamWithoutMessageStopStillTerminates(t *testing.T) {
	frames := runStream(t, streamEvents[:4], StreamOptions{})

	if frames[len(frames)-1] != "[DONE]" {
		t.Errorf("last frame = %q, want [DONE] even though the stream was cut", frames[len(frames)-1])
	}
	var finish string
	for _, f := range frames {
		if f == "[DONE]" {
			continue
		}
		var chunk chatChunk
		_ = json.Unmarshal([]byte(f), &chunk)
		if len(chunk.Choices) > 0 && chunk.Choices[0].FinishReason != nil {
			finish = *chunk.Choices[0].FinishReason
		}
	}
	if finish != "stop" {
		t.Errorf("finish_reason = %q, want stop for a stream cut before message_delta", finish)
	}
}

// TestStreamErrorBecomesAnErrorFrame covers a failure mid-stream: reporting a
// clean finish would hand the client a truncated answer with nothing to tell
// it so, so the error object reaches the client in a data frame instead.
func TestStreamErrorBecomesAnErrorFrame(t *testing.T) {
	frames := runStream(t, []string{
		streamEvents[0],
		streamEvents[2],
		anthropicEvent("error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`),
	}, StreamOptions{})

	var errFrame map[string]any
	for _, f := range frames {
		if f == "[DONE]" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(f), &payload); err != nil {
			t.Fatalf("frame is not JSON: %v\n%s", err, f)
		}
		if e, ok := payload["error"].(map[string]any); ok {
			errFrame = e
		}
	}
	if errFrame == nil {
		t.Fatalf("no error frame in the stream, so the client cannot tell it failed: %v", frames)
	}
	if errFrame["message"] != "Overloaded" || errFrame["type"] != "overloaded_error" {
		t.Errorf("error frame = %v, want the upstream's own message and type", errFrame)
	}
	if frames[len(frames)-1] != "[DONE]" {
		t.Errorf("last frame = %q, want [DONE]", frames[len(frames)-1])
	}
}

// TestStreamStopsWritingAfterAClientFailure is the guard on the write path: a
// client that hangs up mid-stream must not leave the translator reading the
// upstream to the end.
func TestStreamStopsWritingAfterAClientFailure(t *testing.T) {
	r := io.Reader(strings.NewReader(strings.Join(streamEvents, "")))
	w := &failingWriter{failAfter: 1}
	err := TranslateStream(r, w, StreamOptions{})
	if err == nil {
		t.Fatal("TranslateStream ignored a failed client write")
	}
	if w.writes > 4 {
		t.Errorf("wrote %d frames after the client had gone; the translation should stop", w.writes)
	}
}

type failingWriter struct {
	failAfter int
	writes    int
}

func (f *failingWriter) Write(p []byte) (int, error) {
	f.writes++
	if f.writes > f.failAfter {
		return 0, io.ErrClosedPipe
	}
	return len(p), nil
}
