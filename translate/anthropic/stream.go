package anthropic

// Anthropic's /v1/messages SSE stream translated into OpenAI chat completion
// chunks. Nothing in the reference module does this - it defines the chunk
// types and stops there - so this file is written for the router.
//
// The event mapping, in the order Anthropic sends them:
//
//	message_start        -> the first chunk, carrying role "assistant"
//	content_block_start  -> tool_use opens a tool_calls entry, named and id'd
//	content_block_delta  -> text_delta as content, input_json_delta as the
//	                        tool call's arguments, thinking_delta as
//	                        reasoning_content (the field OpenAI-compatible
//	                        routers use for a model's private reasoning)
//	message_delta        -> the stop reason, mapped to a finish_reason
//	message_stop         -> the terminal chunk, then [DONE]
//	ping                 -> dropped
//	error                -> a data frame carrying the error object, then
//	                        [DONE]: a failure reported as a clean finish would
//	                        hand the client a truncated answer with nothing to
//	                        tell it so
//
// A stream that ends without message_stop still owes the client its terminal
// chunk and [DONE], the same way the chat<->responses translator treats a
// Responses stream that ends without response.completed.

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// StreamOptions tunes the translation of one stream.
type StreamOptions struct {
	// IncludeUsage adds the usage chunk the chat wire sends after
	// stream_options.include_usage was set on the request.
	IncludeUsage bool
}

// streamReadBuffer is how many bytes are pulled from the upstream at a time.
const streamReadBuffer = 32 << 10

// maxStreamPending is the largest an unfinished SSE block may grow. An
// upstream that never sends the blank line would otherwise make this grow to
// the size of the whole response.
const maxStreamPending = 8 << 20 // 8 MiB

// TranslateStream reads an Anthropic SSE stream from r and writes the chat
// chunk stream to w, flushing after every chunk (w is expected to be a
// flushing writer). The first failed write to w stops the translation and
// comes back as the returned error.
func TranslateStream(r io.Reader, w io.Writer, opts StreamOptions) error {
	t := &streamTranslator{w: w, opts: opts}
	buf := make([]byte, streamReadBuffer)
	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			if err := t.consume(buf[:n]); err != nil {
				return err
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				t.finish()
				return t.err
			}
			return readErr
		}
	}
}

// streamTranslator carries the state of one translated stream.
type streamTranslator struct {
	w    io.Writer
	opts StreamOptions

	// err is the first failure to write to the client. A stream cannot be
	// re-sent, so every later emit is skipped once it is set.
	err error

	// pending accumulates a partial event until its blank line arrives.
	pending strings.Builder

	id      string
	model   string
	created int64

	// started says the first chunk (the role chunk) has gone out.
	started bool
	// terminal says the chunk carrying finish_reason has gone out.
	terminal bool
	// done says [DONE] has gone out. Nothing may follow it.
	done bool

	// finishReason is the mapped finish reason, known from message_delta on.
	finishReason string
	usage        Usage

	// nextCall is the chat-side tool_calls index to hand the next tool_use
	// block. Chat indexes calls across the whole answer; Anthropic indexes
	// content blocks within the message, so the two numbering schemes are
	// not the same and have to be mapped.
	nextCall int
	blocks   map[int]*streamBlock
}

// streamBlock is what one Anthropic content block has turned into so far.
type streamBlock struct {
	callIndex int
	id        string
	name      string
}

func (t *streamTranslator) block(index int) *streamBlock {
	if t.blocks == nil {
		t.blocks = map[int]*streamBlock{}
	}
	b, ok := t.blocks[index]
	if !ok {
		b = &streamBlock{}
		t.blocks[index] = b
	}
	return b
}

// --------------------------------------------------------------- SSE framing

// consume feeds translated bytes in, writing finished chunks out. It is called
// by the copy loop with whatever the last read returned.
func (t *streamTranslator) consume(p []byte) error {
	t.pending.WriteString(string(p))
	if t.pending.Len() > maxStreamPending {
		return fmt.Errorf("anthropic sse translation: upstream sent %d bytes without a blank line, aborting", t.pending.Len())
	}
	for {
		block, rest, ok := splitStreamBlock(t.pending.String())
		if !ok {
			return t.err
		}
		t.pending.Reset()
		t.pending.WriteString(rest)
		if ev := parseStreamEvent(block); ev != nil {
			t.handle(ev)
		}
		if t.err != nil {
			return t.err
		}
	}
}

// streamSeparators are the blank-line forms an event can be terminated by.
var streamSeparators = []string{"\r\n\r\n", "\n\n", "\r\r"}

// splitStreamBlock cuts the first complete event off s, returning the block,
// the remainder, and whether a separator was found.
func splitStreamBlock(s string) (block, rest string, ok bool) {
	best := -1
	sepLen := 0
	for _, sep := range streamSeparators {
		if i := strings.Index(s, sep); i >= 0 && (best < 0 || i < best) {
			best = i
			sepLen = len(sep)
		}
	}
	if best < 0 {
		return "", s, false
	}
	return s[:best], s[best+sepLen:], true
}

// streamEvent is one server-sent event, with its name read off the wire or
// out of the payload's type field - Anthropic sends both.
type streamEvent struct {
	name string
	data string
}

func parseStreamEvent(block string) *streamEvent {
	ev := &streamEvent{}
	var data strings.Builder
	// Normalise CRLF and bare CR so a block framed either way parses the same.
	block = strings.ReplaceAll(block, "\r\n", "\n")
	block = strings.ReplaceAll(block, "\r", "\n")
	for _, line := range strings.Split(block, "\n") {
		switch {
		case strings.HasPrefix(line, "event:"):
			ev.name = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimSpace(line[len("data:"):]))
		}
	}
	ev.data = data.String()
	if ev.data == "" {
		return nil
	}
	if ev.name == "" {
		var probe struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(ev.data), &probe) == nil {
			ev.name = probe.Type
		}
	}
	return ev
}

// finish flushes an unterminated final event and emits whatever the client is
// still owed, for a stream that ended without message_stop.
func (t *streamTranslator) finish() {
	if rest := t.pending.String(); strings.TrimSpace(rest) != "" {
		t.pending.Reset()
		if ev := parseStreamEvent(rest); ev != nil {
			t.handle(ev)
		}
	}
	if t.started && !t.terminal {
		t.emitTerminal()
	}
	t.emitDone()
}

// ------------------------------------------------------------------ events

// payload of the events the translator reads.
type messageStartPayload struct {
	Message struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage struct {
			InputTokens        int `json:"input_tokens"`
			CacheCreationInput int `json:"cache_creation_input_tokens"`
			CacheReadInput     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

type contentBlockStartPayload struct {
	Index        int `json:"index"`
	ContentBlock struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
}

type contentBlockDeltaPayload struct {
	Index int `json:"index"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		Thinking    string `json:"thinking"`
	} `json:"delta"`
}

type messageDeltaPayload struct {
	Delta struct {
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Usage struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// handle translates one event.
func (t *streamTranslator) handle(ev *streamEvent) {
	switch ev.name {
	case "message_start":
		var p messageStartPayload
		_ = json.Unmarshal([]byte(ev.data), &p)
		t.id = chatStreamID(p.Message.ID)
		if p.Message.Model != "" {
			t.model = p.Message.Model
		}
		t.created = time.Now().Unix()
		t.started = true
		t.usage.PromptTokens = p.Message.Usage.InputTokens +
			p.Message.Usage.CacheCreationInput +
			p.Message.Usage.CacheReadInput
		t.emitChunk(t.chunkDelta(map[string]any{"role": "assistant", "content": ""}), nil)
	case "content_block_start":
		var p contentBlockStartPayload
		_ = json.Unmarshal([]byte(ev.data), &p)
		block := t.block(p.Index)
		if p.ContentBlock.Type != "tool_use" {
			return
		}
		block.callIndex = t.nextCall
		t.nextCall++
		block.id = p.ContentBlock.ID
		block.name = p.ContentBlock.Name
		t.emitChunk(t.chunkDelta(map[string]any{
			"tool_calls": []map[string]any{{
				"index":    block.callIndex,
				"id":       block.id,
				"type":     "function",
				"function": map[string]any{"name": block.name, "arguments": ""},
			}},
		}), nil)
	case "content_block_delta":
		var p contentBlockDeltaPayload
		_ = json.Unmarshal([]byte(ev.data), &p)
		block := t.block(p.Index)
		switch p.Delta.Type {
		case "text_delta":
			if p.Delta.Text == "" {
				return
			}
			t.emitChunk(t.chunkDelta(map[string]any{"content": p.Delta.Text}), nil)
		case "input_json_delta":
			if p.Delta.PartialJSON == "" {
				return
			}
			t.emitChunk(t.chunkDelta(map[string]any{
				"tool_calls": []map[string]any{{
					"index":    block.callIndex,
					"function": map[string]any{"arguments": p.Delta.PartialJSON},
				}},
			}), nil)
		case "thinking_delta":
			if p.Delta.Thinking == "" {
				return
			}
			t.emitChunk(t.chunkDelta(map[string]any{"reasoning_content": p.Delta.Thinking}), nil)
		}
	case "message_delta":
		var p messageDeltaPayload
		_ = json.Unmarshal([]byte(ev.data), &p)
		if p.Delta.StopReason != "" {
			t.finishReason = MapFinishReason(p.Delta.StopReason)
		}
		if p.Usage.OutputTokens > 0 {
			t.usage.CompletionTokens = p.Usage.OutputTokens
		}
	case "message_stop":
		t.emitTerminal()
		t.emitDone()
	case "error":
		// The stream failed part-way. The chat wire's terminal event has no
		// room for a failure, so the in-band form reaches the client: a data
		// frame carrying the error object, which the OpenAI SDKs raise on.
		// The error frame replaces the terminal chunk, and finish() owes the
		// client nothing after it.
		t.emitError(ev.data)
		t.terminal = true
		t.emitDone()
	case "ping":
		// A keep-alive with no chat counterpart.
	}
}

// chatStreamID re-spells an Anthropic message id as a chat-completion id.
func chatStreamID(id string) string {
	if id == "" {
		return "chatcmpl-anthropic"
	}
	if strings.HasPrefix(id, "msg_") {
		return "chatcmpl-" + strings.TrimPrefix(id, "msg_")
	}
	return id
}

// ------------------------------------------------------------------ chunks

// chatStreamChunk is the OpenAI streaming chunk shape.
type chatStreamChunk struct {
	ID      string               `json:"id"`
	Object  string               `json:"object"`
	Created int64                `json:"created"`
	Model   string               `json:"model"`
	Choices []chatStreamChoice   `json:"choices"`
	Usage   *chatStreamUsageTail `json:"usage,omitempty"`
}

type chatStreamChoice struct {
	Index        int            `json:"index"`
	Delta        map[string]any `json:"delta"`
	FinishReason *string        `json:"finish_reason"`
}

type chatStreamUsageTail struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// chunkDelta builds one chunk carrying a delta.
func (t *streamTranslator) chunkDelta(delta map[string]any) chatStreamChunk {
	return chatStreamChunk{
		ID:      t.id,
		Object:  "chat.completion.chunk",
		Created: t.created,
		Model:   t.model,
		Choices: []chatStreamChoice{{Index: 0, Delta: delta, FinishReason: nil}},
	}
}

// emitChunk writes one chunk.
func (t *streamTranslator) emitChunk(chunk chatStreamChunk, finish *string) {
	if t.err != nil {
		return
	}
	if finish != nil && len(chunk.Choices) > 0 {
		chunk.Choices[0].FinishReason = finish
	}
	body, err := json.Marshal(chunk)
	if err != nil {
		return
	}
	if _, err := fmt.Fprintf(t.w, "data: %s\n\n", body); err != nil {
		t.writeFailed(err)
	}
}

// emitTerminal writes the chunk carrying the finish reason, and - when the
// client asked for it - the usage chunk that follows it.
func (t *streamTranslator) emitTerminal() {
	if t.err != nil || t.terminal {
		return
	}
	t.terminal = true
	reason := t.finishReason
	if reason == "" {
		reason = "stop"
	}
	t.emitChunk(t.chunkDelta(map[string]any{}), &reason)
	if t.opts.IncludeUsage {
		usage := chatStreamUsageTail{
			PromptTokens:     t.usage.PromptTokens,
			CompletionTokens: t.usage.CompletionTokens,
			TotalTokens:      t.usage.PromptTokens + t.usage.CompletionTokens,
		}
		t.emitChunk(chatStreamChunk{
			ID:      t.id,
			Object:  "chat.completion.chunk",
			Created: t.created,
			Model:   t.model,
			Choices: []chatStreamChoice{},
			Usage:   &usage,
		}, nil)
	}
}

// emitError writes a data frame carrying an upstream failure. evData is the
// raw JSON of the error event, whose shape is {type, error:{type, message}}.
func (t *streamTranslator) emitError(evData string) {
	if t.err != nil {
		return
	}
	var p struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal([]byte(evData), &p)
	errType := p.Error.Type
	if errType == "" {
		errType = "api_error"
	}
	message := p.Error.Message
	if message == "" {
		message = p.Message
	}
	if message == "" {
		message = "the upstream reported that this response failed"
	}
	body, err := json.Marshal(map[string]any{
		"error": map[string]any{"message": message, "type": errType, "code": nil},
	})
	if err != nil {
		return
	}
	if _, err := fmt.Fprintf(t.w, "data: %s\n\n", body); err != nil {
		t.writeFailed(err)
	}
}

// emitDone writes the chat stream terminator, at most once per stream: a
// second [DONE] is a protocol error.
func (t *streamTranslator) emitDone() {
	if t.err != nil || t.done {
		return
	}
	if _, err := fmt.Fprint(t.w, "data: [DONE]\n\n"); err != nil {
		t.writeFailed(err)
		return
	}
	t.done = true
}

// writeFailed records the first failed write to the client; the copy loop
// reads it and stops.
func (t *streamTranslator) writeFailed(err error) {
	if t.err == nil {
		t.err = fmt.Errorf("writing the translated stream to the client: %w", err)
	}
}
