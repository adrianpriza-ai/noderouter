package router

// Translation between the request shapes the router translates between.
//
// The router forwards a path through untouched, so a client that speaks
// Responses and a provider that only speaks chat completions never meet: the
// path /v1/responses reaches an upstream that has never heard of it, and
// answers 404. This file is the fix. A provider declared with
// "surface": "responses" is reached through whichever of the two endpoints the
// client used, and the router rewrites the request, the path and the response -
// streaming included - so neither side has to know about the other.
//
// A provider declared with "api": "anthropic" is the same story in a third
// dialect: it speaks only Anthropic's /v1/messages, and a chat-completions
// client reaches it through the translator in translate_anthropic.go.
//
// The translation is deliberately conservative about content it does not
// understand. Text, images, audio, video and files arrive as typed content
// parts whose payloads can be tens of megabytes of base64, so a part the
// translator does not have a proven mapping for is passed through as raw JSON
// rather than rebuilt: an omni model's audio and video must reach the provider
// exactly as the client sent them.
//
// What cannot be translated is reported instead of guessed at. Asking for
// server-side conversation state, a schema response format or more than one
// choice has no counterpart on the other surface, and silently dropping it
// would hand the caller an answer to a question they did not ask. Those cases
// come back as a 400 that names the field.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"noderouter/config"
	anthropic "noderouter/translate/anthropic"
)

// wire names the request shapes the router translates between.
type wire int

const (
	wireChat wire = iota
	wireResponses
	// wireAnthropic is Anthropic's /v1/messages. It is only ever an upstream
	// wire: a client reaches it through /v1/chat/completions, which the
	// router rewrites into a Messages request, and the answer - streamed or
	// not - comes back as a chat completion.
	wireAnthropic
)

// wireForProvider reports which wire a provider speaks, mapping the config
// package's spelling onto the router's own type. The two packages are kept
// apart deliberately - config owns the schema, router owns the protocol - so
// this is the single place the mapping is written.
//
// The dialect check comes first: an "anthropic" provider speaks the Messages
// API whatever its surface says, and config.Surface() reports chat for it
// precisely because it has no surface of its own to choose.
func wireForProvider(p *config.Provider) wire {
	if p == nil {
		return wireChat
	}
	if p.Options.Dialect() == "anthropic" {
		return wireAnthropic
	}
	if p.Options.Surface() == config.SurfaceResponses {
		return wireResponses
	}
	return wireChat
}

func (w wire) String() string {
	switch w {
	case wireResponses:
		return "responses"
	case wireAnthropic:
		return "anthropic"
	default:
		return "chat"
	}
}

// endpointSuffix is the path each wire is served on. The router matches on the
// suffix so a client may call either the bare path or one under a provider's
// own prefix (/v1/chat/completions, /zen/v1/chat/completions, ...).
var endpointSuffix = map[wire]string{
	wireChat:      "/chat/completions",
	wireResponses: "/responses",
	wireAnthropic: "/messages",
}

// wireOfPath reports which wire a request path is served on, and the prefix
// before the endpoint's suffix, so the same endpoint on any other wire can be
// built from it. A path that names neither client wire is not translated at
// all - /v1/embeddings, /v1/images/generations, /v1/videos/generations,
// /v1/messages and every other endpoint keep working exactly as before.
//
// A /responses/{id} path is deliberately not matched: it is a lookup of a
// stored response, which only the surface that stored it can answer.
func wireOfPath(path string) (w wire, prefix string, ok bool) {
	p := strings.TrimSuffix(path, "/")
	for _, w := range []wire{wireChat, wireResponses} {
		suffix := endpointSuffix[w]
		if !strings.HasSuffix(p, suffix) {
			continue
		}
		prefix := strings.TrimSuffix(p, suffix)
		if strings.HasPrefix(prefix, "/responses/") {
			return 0, "", false
		}
		return w, prefix, true
	}
	return 0, "", false
}

// plan decides what a request needs. It is nil when nothing has to be
// translated: the two wires already agree, or the path is not one of the two
// endpoints, or the provider is not declared to speak the other surface.
func plan(r requestShape, clientWire, upstreamWire wire) *translation {
	if clientWire == upstreamWire {
		return nil
	}
	return &translation{from: clientWire, to: upstreamWire, streaming: r.Stream()}
}

// requestShape is the little bit of a request body the translator needs to
// know before it rewrites it: which wire it is written in, and whether the
// caller wants a stream back.
type requestShape struct {
	stream bool
}

// Stream reports whether the client asked for a streamed answer, read from the
// body rather than from a header, since only the body knows.
func (s *requestShape) Stream() bool { return s.stream }

// inspectShape reads the stream flag out of a body on either wire. Both use
// the same "stream" boolean, so one field covers both; a body that is not JSON
// object-shaped simply reports no stream.
func inspectShape(body []byte) requestShape {
	var probe struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &probe)
	return requestShape{stream: probe.Stream}
}

// translation is one planned rewrite: a client wire, the wire to send, and
// whether the answer comes back as a stream.
type translation struct {
	from      wire
	to        wire
	streaming bool

	// State the request step learned and the response step needs. Only the
	// chat -> anthropic direction uses it: whether response_format made this
	// a JSON-mode answer (whose tool call has to be folded back into the
	// message), the assistant prefix to restore, and whether the client asked
	// for the usage tail chunk on a stream.
	jsonMode     bool
	prefixPrompt string
	includeUsage bool
}

// Request rewrites a request body onto the upstream wire. It returns the body
// unchanged when there is nothing to do.
func (t *translation) Request(body []byte) ([]byte, error) {
	if t == nil {
		return body, nil
	}
	if t.to == wireAnthropic {
		return t.chatToAnthropic(body)
	}
	if t.from == wireChat {
		return chatToResponses(body)
	}
	return responsesToChat(body)
}

// Response rewrites a non-streamed response body back onto the client's wire.
// A body the translator cannot parse is passed through rather than replaced
// with an error: the client asked a question, the provider answered it, and a
// half-understood answer is better delivered intact than withheld.
func (t *translation) Response(body []byte) ([]byte, error) {
	if t == nil || len(bytes.TrimSpace(body)) == 0 {
		return body, nil
	}
	if t.to == wireAnthropic {
		out, err := t.anthropicToChat(body)
		if err != nil {
			return body, nil
		}
		return out, nil
	}
	if t.from == wireChat {
		out, err := responsesToChatCompletion(body)
		if err != nil {
			return body, nil
		}
		return out, nil
	}
	out, err := chatCompletionToResponse(body)
	if err != nil {
		return body, nil
	}
	return out, nil
}

// ------------------------------------------------------------- chat -> responses

// chatFields are the chat-completions fields with a direct counterpart on the
// Responses wire. Anything not listed here is checked against
// chatOnlyFields before the request is rewritten.
var chatPassThrough = []string{
	"stream", "stream_options", "temperature", "top_p", "tools", "tool_choice",
	"parallel_tool_calls", "user", "metadata", "store", "service_tier",
	"truncation", "max_tool_calls",
}

// chatOnlyFields are chat-completions knobs the Responses API has no equivalent
// for. Their presence is refused with a named error instead of being dropped,
// because each one changes what the caller asked for.
var chatOnlyFields = map[string]string{
	"n":                  "the Responses API always returns one choice",
	"stop":               "stop sequences",
	"presence_penalty":   "presence penalty",
	"frequency_penalty":  "frequency penalty",
	"logprobs":           "logprobs",
	"top_logprobs":       "top_logprobs",
	"logit_bias":         "logit bias",
	"audio":              "audio output options",
	"modalities":         "modalities",
	"web_search_options": "web search options",
	"prediction":         "predicted outputs",
	"seed":               "seed",
}

// chatTranslatedFields are the chat fields chatToResponses reads and rebuilds
// under another name, so they must not be copied across as they stand on top of
// what the conversions produce.
var chatTranslatedFields = []string{
	// model crosses untouched. messages becomes input, plus instructions when a
	// system message is pulled out of it; input is listed too because it is the
	// name the translation writes, so a body that carried both must not have
	// its own input copied over the converted one.
	"model", "messages", "input",
	"max_tokens", "max_completion_tokens", "response_format", "reasoning_effort",
}

// chatHandled is every chat field the translator accounts for in this direction:
// the ones it copies as they are, the ones it rebuilds under a new name, and the
// ones it refuses. Whatever is left belongs to neither wire - a provider extra
// such as top_k or enable_thinking, or a wrapper's own knob - and crosses
// untouched rather than being dropped on the floor.
var chatHandled = fieldSet(chatPassThrough, chatTranslatedFields, tableNames(chatOnlyFields))

// chatToResponses rewrites a chat-completions request as a Responses request.
func chatToResponses(body []byte) ([]byte, error) {
	var src map[string]json.RawMessage
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, fmt.Errorf("request body is not a JSON object: %w", err)
	}
	if err := refuseChatOnly(src); err != nil {
		return nil, err
	}

	out := map[string]json.RawMessage{}
	// model is required by /v1/responses and by every chat provider that
	// validates its input, so it always crosses with the request. It is
	// copied rather than re-derived: handleForward has already run
	// ReplaceModel on this body, so what sits here is the upstream id when
	// upstreamName renamed it.
	if v, ok := src["model"]; ok {
		out["model"] = v
	}
	// Fields that mean the same on both wires.
	for _, k := range chatPassThrough {
		if v, ok := src[k]; ok {
			out[k] = v
		}
	}
	// Everything the translator does not name goes across as it was sent. The
	// conversions below still overwrite whatever they produce, so a field they
	// rename cannot be clobbered by the client's own copy of it.
	passUnknown(src, out, chatHandled)
	// max_tokens and max_completion_tokens both become max_output_tokens.
	for _, k := range []string{"max_completion_tokens", "max_tokens"} {
		if v, ok := src[k]; ok {
			out["max_output_tokens"] = v
			break
		}
	}
	// max_tokens has no Responses counterpart under its own name; it was
	// never written to out (only max_output_tokens is), so there is nothing
	// to delete.
	// response_format becomes the Responses "text.format".
	if v, ok := src["response_format"]; ok {
		format, err := responseFormatToTextFormat(v)
		if err != nil {
			return nil, err
		}
		out["text"] = format
	}
	// reasoning_effort becomes reasoning.effort.
	if v, ok := src["reasoning_effort"]; ok {
		effort, err := json.Marshal(map[string]json.RawMessage{"effort": v})
		if err != nil {
			return nil, err
		}
		out["reasoning"] = effort
	}

	messages, hasMessages := src["messages"]
	if !hasMessages {
		// A chat request without messages is not a chat request, so there is
		// nothing to convert. Two shapes are still worth handling: a body
		// that already carries Responses input came from a client that used
		// the wrong path in the right shape and goes through untouched, and
		// anything else is refused here by name. Forwarding it instead would
		// hand the provider a chat body it cannot read and come back as a
		// schema error about a field this side never sent - the translator is
		// the one that knows what was actually missing.
		if _, hasInput := src["input"]; hasInput {
			return body, nil
		}
		return nil, fmt.Errorf("this chat completions request has no messages field, so there is no conversation to translate into a Responses request; put the conversation in messages, or call /v1/responses directly")
	}

	items, instructions, err := chatMessagesToInput(messages)
	if err != nil {
		return nil, err
	}
	if instructions != "" {
		enc, err := json.Marshal(instructions)
		if err != nil {
			return nil, err
		}
		out["instructions"] = enc
	}
	enc, err := json.Marshal(items)
	if err != nil {
		return nil, err
	}
	out["input"] = enc
	return marshalObject(out)
}

// refuseChatOnly reports the first chat-only field that is actually set. A
// field present but null or false is a client sending its defaults, which is
// not a request for anything the Responses wire lacks.
func refuseChatOnly(src map[string]json.RawMessage) error {
	var found []string
	for name := range chatOnlyFields {
		v, ok := src[name]
		if !ok || !isMeaningful(v) {
			continue
		}
		// modalities is only chat-only when it asks for something beyond
		// text. A wrapper that unconditionally sends ["text"] is asking for
		// exactly what the Responses API already returns, so the field is
		// dropped (chatToResponses never copies it) rather than refused.
		if name == "modalities" && onlyTextModalities(v) {
			continue
		}
		found = append(found, name)
	}
	if len(found) == 0 {
		return nil
	}
	sort.Strings(found)
	notes := make([]string, 0, len(found))
	for _, name := range found {
		notes = append(notes, fmt.Sprintf("%s (%s)", name, chatOnlyFields[name]))
	}
	return fmt.Errorf("this provider only speaks the OpenAI Responses API, and the request asks for %s, which it has no equivalent for; call the model without %s, or point a chat-completions provider at it instead",
		strings.Join(notes, ", "), strings.Join(found, " or "))
}

// onlyTextModalities reports whether a modalities value names only "text" (at
// least once). ["text"] and ["text","text"] ask for nothing the Responses API
// lacks; ["text","audio"] or a value that is not an array of strings does not.
func onlyTextModalities(raw json.RawMessage) bool {
	var mods []string
	if err := json.Unmarshal(raw, &mods); err != nil {
		return false
	}
	if len(mods) == 0 {
		return false
	}
	for _, m := range mods {
		if !strings.EqualFold(strings.TrimSpace(m), "text") {
			return false
		}
	}
	return true
}

// isMeaningful reports whether a JSON value asks for something, as opposed to
// being an unset default a client sends anyway: null, false, 0, "", [] and {}
// all mean "not asking".
func isMeaningful(raw json.RawMessage) bool {
	v := strings.TrimSpace(string(raw))
	switch v {
	case "", "null", "false", "0", `""`, "[]", "{}":
		return false
	}
	// An object or array that is only empty after trimming is still "unset",
	// but a non-empty one is a real request.
	return true
}

// responseFormatToTextFormat maps a chat response_format onto the Responses
// "text" object. json_schema and json_object have counterparts; text and the
// unset value mean plain text.
func responseFormatToTextFormat(raw json.RawMessage) (json.RawMessage, error) {
	var rf struct {
		Type       string `json:"type"`
		JSONSchema *struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Schema      json.RawMessage `json:"schema"`
			Strict      *bool           `json:"strict"`
		} `json:"json_schema"`
	}
	if err := json.Unmarshal(raw, &rf); err != nil {
		return nil, fmt.Errorf("could not read response_format: %w", err)
	}
	format := map[string]json.RawMessage{}
	switch rf.Type {
	case "", "text":
		format["type"] = mustMarshal("text")
	case "json_object":
		format["type"] = mustMarshal("json_object")
	case "json_schema":
		if rf.JSONSchema == nil {
			return nil, fmt.Errorf("response_format is json_schema but carries no json_schema block")
		}
		format["type"] = mustMarshal("json_schema")
		if rf.JSONSchema.Name != "" {
			format["name"] = mustMarshal(rf.JSONSchema.Name)
		}
		if rf.JSONSchema.Description != "" {
			format["description"] = mustMarshal(rf.JSONSchema.Description)
		}
		if len(rf.JSONSchema.Schema) > 0 {
			format["schema"] = rf.JSONSchema.Schema
		}
		if rf.JSONSchema.Strict != nil {
			format["strict"] = mustMarshal(*rf.JSONSchema.Strict)
		}
	default:
		return nil, fmt.Errorf("response_format %q has no equivalent on the Responses API", rf.Type)
	}
	return marshalObject(map[string]json.RawMessage{"format": mustMarshal(format)})
}

// textFormatToResponseFormat maps the Responses text object back onto a chat
// response_format.
func textFormatToResponseFormat(raw json.RawMessage) (json.RawMessage, error) {
	var t struct {
		Format json.RawMessage `json:"format"`
	}
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("could not read text: %w", err)
	}
	if len(t.Format) == 0 || !isMeaningful(t.Format) {
		return nil, fmt.Errorf("the request sets text without a format; chat completions has no equivalent")
	}
	var f struct {
		Type        string          `json:"type"`
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Schema      json.RawMessage `json:"schema"`
		Strict      *bool           `json:"strict"`
	}
	if err := json.Unmarshal(t.Format, &f); err != nil {
		return nil, fmt.Errorf("could not read text.format: %w", err)
	}
	switch f.Type {
	case "text", "":
		return mustMarshal(map[string]string{"type": "text"}), nil
	case "json_object":
		return mustMarshal(map[string]string{"type": "json_object"}), nil
	case "json_schema":
		schema := map[string]json.RawMessage{}
		if f.Name != "" {
			schema["name"] = mustMarshal(f.Name)
		}
		if f.Description != "" {
			schema["description"] = mustMarshal(f.Description)
		}
		if len(f.Schema) > 0 {
			schema["schema"] = f.Schema
		}
		if f.Strict != nil {
			schema["strict"] = mustMarshal(*f.Strict)
		}
		block, err := marshalObject(schema)
		if err != nil {
			return nil, err
		}
		return marshalObject(map[string]json.RawMessage{
			"type":        mustMarshal("json_schema"),
			"json_schema": block,
		})
	default:
		return nil, fmt.Errorf("text.format %q has no equivalent on chat completions", f.Type)
	}
}

// responsesInputToMessages converts a Responses input (a string, or an array of
// typed items) into chat messages.
func responsesInputToMessages(input json.RawMessage) ([]json.RawMessage, error) {
	trimmed := strings.TrimSpace(string(input))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	if strings.HasPrefix(trimmed, `"`) {
		var s string
		if err := json.Unmarshal(input, &s); err != nil {
			return nil, err
		}
		msg, err := simpleMessage("user", s)
		if err != nil {
			return nil, err
		}
		return []json.RawMessage{msg}, nil
	}

	var items []map[string]json.RawMessage
	if err := json.Unmarshal(input, &items); err != nil {
		// A single object rather than an array.
		var one map[string]json.RawMessage
		if err2 := json.Unmarshal(input, &one); err2 != nil {
			return nil, fmt.Errorf("could not read input: %w", err)
		}
		items = []map[string]json.RawMessage{one}
	}

	var msgs []json.RawMessage
	for _, item := range items {
		converted, err := responsesItemToChatMessage(item)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, converted...)
	}
	return msgs, nil
}

// responsesContentToChatParts maps Responses content parts to chat content
// parts, passing through anything without a proven mapping.
func responsesContentToChatParts(content json.RawMessage) (json.RawMessage, error) {
	trimmed := strings.TrimSpace(string(content))
	if trimmed == "" || trimmed == "null" {
		return mustMarshal(nil), nil
	}
	if strings.HasPrefix(trimmed, `"`) {
		var s string
		if err := json.Unmarshal(content, &s); err != nil {
			return nil, err
		}
		return mustMarshal(s), nil
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(content, &parts); err != nil {
		return nil, fmt.Errorf("could not read item content: %w", err)
	}
	out := make([]json.RawMessage, 0, len(parts))
	for _, p := range parts {
		out = append(out, responsesPartToChatPart(p))
	}
	return marshalArray(out)
}

// responsesPartToChatPart maps one Responses content part. Unknown types pass
// through untouched, for the same reason as on the way out.
func responsesPartToChatPart(p map[string]json.RawMessage) json.RawMessage {
	switch rawString(p["type"]) {
	case "input_text", "output_text", "text":
		out := map[string]json.RawMessage{"type": mustMarshal("text")}
		if t, ok := p["text"]; ok {
			out["text"] = t
		}
		if enc, err := marshalObject(out); err == nil {
			return enc
		}
		return mustMarshal(map[string]any{})
	case "input_image":
		out := map[string]json.RawMessage{"type": mustMarshal("image_url")}
		if u, ok := p["image_url"]; ok {
			// The Responses wire carries a bare URL string where chat carries
			// a {url, detail} object.
			var asString string
			if json.Unmarshal(u, &asString) == nil {
				obj := map[string]json.RawMessage{"url": u}
				if d, ok := p["detail"]; ok {
					obj["detail"] = d
				}
				enc, err := marshalObject(obj)
				if err == nil {
					out["image_url"] = enc
				}
			} else {
				out["image_url"] = u
			}
		}
		if enc, err := marshalObject(out); err == nil {
			return enc
		}
		return mustMarshal(map[string]any{})
	default:
		// input_audio, input_file, video and anything else: passthrough.
		if enc, err := marshalObject(p); err == nil {
			return enc
		}
		return mustMarshal(map[string]any{})
	}
}

// responsesItemToChatMessage converts one Responses input item to the chat
// message(s) it corresponds to. An item that maps to one message returns one
// item; a function_call is the one case that expands, since a chat assistant
// message can carry several tool_calls in one turn.
func responsesItemToChatMessage(item map[string]json.RawMessage) ([]json.RawMessage, error) {
	switch typ := rawString(item["type"]); typ {
	case "function_call_output":
		out := map[string]json.RawMessage{"role": mustMarshal("tool")}
		if id := rawString(item["call_id"]); id != "" {
			out["tool_call_id"] = mustMarshal(id)
		}
		out["content"] = mustMarshal(rawString(item["output"]))
		enc, err := marshalObject(out)
		if err != nil {
			return nil, err
		}
		return []json.RawMessage{enc}, nil
	case "function_call":
		call := map[string]json.RawMessage{"type": mustMarshal("function")}
		if id := rawString(item["call_id"]); id != "" {
			call["id"] = mustMarshal(id)
		}
		fn := map[string]json.RawMessage{}
		if name := rawString(item["name"]); name != "" {
			fn["name"] = mustMarshal(name)
		}
		if args, ok := item["arguments"]; ok {
			fn["arguments"] = args
		}
		fnEnc, err := marshalObject(fn)
		if err != nil {
			return nil, err
		}
		call["function"] = fnEnc
		callEnc, err := marshalObject(call)
		if err != nil {
			return nil, err
		}
		enc, err := marshalObject(map[string]json.RawMessage{
			"role":       mustMarshal("assistant"),
			"content":    mustMarshal(nil),
			"tool_calls": mustMarshal([]json.RawMessage{callEnc}),
		})
		if err != nil {
			return nil, err
		}
		return []json.RawMessage{enc}, nil
	case "", "message":
		role := rawString(item["role"])
		if role == "" {
			role = "user"
		}
		out := map[string]json.RawMessage{"role": mustMarshal(role)}
		if content, ok := item["content"]; ok {
			parts, err := responsesContentToChatParts(content)
			if err != nil {
				return nil, err
			}
			out["content"] = parts
		}
		enc, err := marshalObject(out)
		if err != nil {
			return nil, err
		}
		return []json.RawMessage{enc}, nil
	default:
		// An item type with no chat counterpart. Passing it through keeps the
		// payload intact, so a provider that does know the type still gets it.
		enc, err := marshalObject(item)
		if err != nil {
			return nil, fmt.Errorf("could not convert input item of type %q", typ)
		}
		return []json.RawMessage{enc}, nil
	}
}

// chatMessagesToInput turns a chat message array into Responses input items.
// System and developer messages become the top-level instructions field,
// because that is what the Responses API calls them; everything else becomes
// an input item in order.
func chatMessagesToInput(messages json.RawMessage) (items []json.RawMessage, instructions string, err error) {
	var msgs []map[string]json.RawMessage
	if err := json.Unmarshal(messages, &msgs); err != nil {
		// A single message object is not valid chat, but let the provider say
		// so rather than inventing a request.
		return nil, "", fmt.Errorf("could not read messages: %w", err)
	}
	var sys []string
	for _, m := range msgs {
		role := rawString(m["role"])
		content, hasContent := m["content"]
		switch role {
		case "system", "developer":
			if !hasContent {
				continue
			}
			text, err := messageText(content)
			if err != nil {
				return nil, "", fmt.Errorf("%s message: %w", role, err)
			}
			if text != "" {
				sys = append(sys, text)
			}
		case "tool", "function":
			item, err := toolMessageToInput(m, content, hasContent)
			if err != nil {
				return nil, "", err
			}
			items = append(items, item)
		default:
			converted, err := chatMessageToInput(m)
			if err != nil {
				return nil, "", err
			}
			items = append(items, converted...)
		}
	}
	return items, strings.Join(sys, "\n\n"), nil
}

// chatMessageToInput converts one user or assistant message into the Responses
// items it corresponds to.
//
// On the chat wire an assistant turn that calls tools is one message carrying
// both content and tool_calls. On the Responses wire a message item and a
// function_call item are separate entries in the input array, so one message
// here can become several items.
func chatMessageToInput(m map[string]json.RawMessage) ([]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	if role, ok := m["role"]; ok {
		out["role"] = role
	}
	content, hasContent := m["content"]
	hasText := hasContent && !isNullOrEmpty(content)
	if hasText {
		parts, err := chatContentToInputParts(content)
		if err != nil {
			return nil, err
		}
		out["content"] = parts
	}

	var items []json.RawMessage
	// An assistant turn that only called tools carries no text, so it becomes
	// the call items alone; a turn with text keeps its message item first.
	if hasText || len(out) > 0 {
		enc, err := marshalObject(out)
		if err != nil {
			return nil, err
		}
		items = append(items, enc)
	}

	// tool_calls on an assistant message become separate function_call items.
	if calls, ok := m["tool_calls"]; ok && !isNullOrEmpty(calls) {
		var parsed []map[string]json.RawMessage
		if err := json.Unmarshal(calls, &parsed); err != nil {
			return nil, fmt.Errorf("could not read tool_calls: %w", err)
		}
		converted, err := toolCallsToInput(parsed)
		if err != nil {
			return nil, err
		}
		items = append(items, converted...)
	}
	return items, nil
}

// toolCallsToInput converts an assistant tool_calls array into function_call
// items.
func toolCallsToInput(calls []map[string]json.RawMessage) ([]json.RawMessage, error) {
	var out []json.RawMessage
	for _, c := range calls {
		item, err := toolCallToInput(c)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, nil
}

// toolCallToInput converts one tool call.
func toolCallToInput(c map[string]json.RawMessage) (json.RawMessage, error) {
	fn, ok := c["function"]
	if !ok {
		return nil, fmt.Errorf("tool call has no function block")
	}
	var f map[string]json.RawMessage
	if err := json.Unmarshal(fn, &f); err != nil {
		return nil, fmt.Errorf("could not read tool call function: %w", err)
	}
	item := map[string]json.RawMessage{"type": mustMarshal("function_call")}
	if id := rawString(c["id"]); id != "" {
		item["call_id"] = mustMarshal(id)
	}
	if name := rawString(f["name"]); name != "" {
		item["name"] = mustMarshal(name)
	}
	args := f["arguments"]
	if len(args) == 0 {
		args = mustMarshal("")
	}
	item["arguments"] = args
	return marshalObject(item)
}

// toolMessageToInput converts a tool result message into the Responses
// function_call_output item.
func toolMessageToInput(m map[string]json.RawMessage, content json.RawMessage, hasContent bool) (json.RawMessage, error) {
	item := map[string]json.RawMessage{"type": mustMarshal("function_call_output")}
	id := rawString(m["tool_call_id"])
	if id == "" {
		id = rawString(m["id"])
	}
	if id != "" {
		item["call_id"] = mustMarshal(id)
	}
	out := ""
	if hasContent {
		text, err := messageText(content)
		if err != nil {
			return nil, fmt.Errorf("tool message: %w", err)
		}
		out = text
	}
	item["output"] = mustMarshal(out)
	return marshalObject(item)
}

// messageText flattens a message content into plain text, accepting both the
// string form and the typed-parts array.
func messageText(content json.RawMessage) (string, error) {
	trimmed := strings.TrimSpace(string(content))
	if strings.HasPrefix(trimmed, `"`) {
		var s string
		if err := json.Unmarshal(content, &s); err != nil {
			return "", err
		}
		return s, nil
	}
	parts, err := chatContentToInputParts(content)
	if err != nil {
		return "", err
	}
	var list []map[string]json.RawMessage
	if err := json.Unmarshal(parts, &list); err != nil {
		return "", fmt.Errorf("could not read message content: %w", err)
	}
	var b strings.Builder
	for _, m := range list {
		switch rawString(m["type"]) {
		case "input_text", "text", "":
			b.WriteString(rawString(m["text"]))
		}
	}
	return b.String(), nil
}

// chatContentToInputParts maps chat content parts onto Responses input parts.
// It returns a JSON array so the caller can drop it straight into an item.
//
// The rule that matters here: a part whose type this file has no proven
// mapping for is copied through unchanged. Media parts carry base64 payloads
// that can be tens of megabytes, and rebuilding them from parsed fields risks
// quietly altering bytes the model is meant to see exactly as sent.
func chatContentToInputParts(content json.RawMessage) (json.RawMessage, error) {
	trimmed := strings.TrimSpace(string(content))
	if trimmed == "" || trimmed == "null" {
		return mustMarshal([]json.RawMessage{}), nil
	}
	if strings.HasPrefix(trimmed, `"`) {
		var s string
		if err := json.Unmarshal(content, &s); err != nil {
			return nil, err
		}
		part := map[string]json.RawMessage{"type": mustMarshal("input_text"), "text": mustMarshal(s)}
		enc, err := marshalObject(part)
		if err != nil {
			return nil, err
		}
		return mustMarshal([]json.RawMessage{enc}), nil
	}

	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(content, &parts); err != nil {
		return nil, fmt.Errorf("could not read message content: %w", err)
	}
	out := make([]json.RawMessage, 0, len(parts))
	for _, p := range parts {
		out = append(out, chatPartToInputPart(p))
	}
	return marshalArray(out)
}

// chatPartToInputPart maps one chat content part. Known types are rewritten;
// anything else is passed through byte for byte.
func chatPartToInputPart(p map[string]json.RawMessage) json.RawMessage {
	switch rawString(p["type"]) {
	case "text", "input_text", "":
		out := map[string]json.RawMessage{"type": mustMarshal("input_text")}
		if t, ok := p["text"]; ok {
			out["text"] = t
		}
		if enc, err := marshalObject(out); err == nil {
			return enc
		}
		return mustMarshal(map[string]any{})
	case "image_url":
		// Chat nests the URL in an object; Responses wants a bare string, with
		// detail as a sibling of image_url rather than a field of it.
		out := map[string]json.RawMessage{"type": mustMarshal("input_image")}
		if u, ok := p["image_url"]; ok {
			out["image_url"] = chatImageURLToString(u)
		}
		if d, ok := p["detail"]; ok {
			out["detail"] = d
		}
		if enc, err := marshalObject(out); err == nil {
			return enc
		}
		return mustMarshal(map[string]any{})
	case "input_audio", "input_image", "input_file", "file":
		// Already the Responses spelling, or close enough that rewriting it
		// could only lose information.
		if enc, err := marshalObject(p); err == nil {
			return enc
		}
		return mustMarshal(map[string]any{})
	default:
		// Unknown type: video, audio_url, anything a gateway added. Pass the
		// original part through untouched.
		if enc, err := marshalObject(p); err == nil {
			return enc
		}
		return mustMarshal(map[string]any{})
	}
}

// chatImageURLToString reduces chat's image_url to the bare string the
// Responses wire expects. A client that already sent a string is passed through
// as it is, so a gateway that spells it the Responses way keeps working.
func chatImageURLToString(raw json.RawMessage) json.RawMessage {
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, `"`) {
		return raw
	}
	var obj struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil || obj.URL == "" {
		return raw
	}
	return mustMarshal(obj.URL)
}

// -------------------------------------------------------- responses -> chat

// responsesPassThrough are Responses fields a chat-completions request has room
// for under the same name. store is one of them: both APIs spell it the same way
// and both mean "keep this answer for later use", so refusing it here while
// chatToResponses passes it the other way round was an asymmetry with no
// reason behind it.
var responsesPassThrough = []string{
	"stream", "stream_options", "temperature", "top_p", "tools", "tool_choice",
	"parallel_tool_calls", "user", "metadata", "service_tier", "truncation",
	"max_tool_calls", "store",
}

// responsesOnlyFields are Responses features with no chat-completions
// counterpart at all. Each is refused by name rather than dropped. Every key
// here is a top-level name, which is what the table can express; the one
// Responses-only field that is nested, reasoning.summary, is checked by
// refuseResponsesOnly instead.
var responsesOnlyFields = map[string]string{
	"previous_response_id": "server-side conversation state",
	"conversation":         "a server-side conversation",
	"background":           "background responses",
	"prompt":               "a stored prompt template",
	"include":              "extra output includes",
	"context_management":   "context management",
	"safety_identifier":    "a safety identifier",
	"prompt_cache_key":     "a prompt cache key",
}

// responsesTranslatedFields are the Responses fields responsesToChat reads and
// rebuilds under another name, so they must not be copied across as they stand
// on top of what the conversions produce.
var responsesTranslatedFields = []string{
	// model crosses untouched. input becomes messages, plus a system message
	// when instructions carries one; messages is listed for the same reason
	// input is on the way out. The reasoning object is read for effort (which
	// crosses as reasoning_effort) and refused for summary, and chat
	// completions has no reasoning object of its own to leave it in.
	"model", "input", "messages", "instructions", "text", "reasoning",
	"reasoning.summary", "max_output_tokens",
}

// responsesHandled is every Responses field the translator accounts for in this
// direction: copied as it is, rebuilt under a new name, or refused. Whatever is
// left belongs to neither wire and crosses untouched rather than being dropped.
var responsesHandled = fieldSet(responsesPassThrough, responsesTranslatedFields, tableNames(responsesOnlyFields))

// maxSSEPending is the largest an sseTranslator.pending block may grow. A
// proxy that never sends the blank line would otherwise make this grow to the
// whole response and blind the body budget.
const maxSSEPending = 8 << 20 // 8 MiB

// asksForReasoningSummary reports whether a Responses request asks for a
// summary of the model's reasoning. The wire nests it inside the reasoning
// object - "reasoning": {"effort": "low", "summary": "auto"} - where effort
// survives the crossing as the chat wire's reasoning_effort but summary has no
// counterpart at all, so it has to be refused by name rather than dropped on
// the floor. A literal top-level "reasoning.summary" key is a mis-spelling of
// the same request: no client sends it, and one that does has misunderstood
// the contract, so it is refused for the same reason.
func asksForReasoningSummary(src map[string]json.RawMessage) bool {
	if v, ok := src["reasoning.summary"]; ok && isMeaningful(v) {
		return true
	}
	v, ok := src["reasoning"]
	if !ok {
		return false
	}
	var r map[string]json.RawMessage
	if json.Unmarshal(v, &r) != nil {
		return false
	}
	sum, ok := r["summary"]
	return ok && isMeaningful(sum)
}

// refuseResponsesOnly reports the first Responses-only feature that is
// actually set. A field present but null or false is a client sending its
// defaults, which is not a request for anything the other surface lacks.
func refuseResponsesOnly(src map[string]json.RawMessage) error {
	var found []string
	for name, what := range responsesOnlyFields {
		if v, ok := src[name]; ok && isMeaningful(v) {
			found = append(found, fmt.Sprintf("%s (%s)", name, what))
		}
	}
	// The one Responses-only field that is not top level, so it cannot live in
	// the table above.
	if asksForReasoningSummary(src) {
		found = append(found, "reasoning.summary (reasoning summaries)")
	}
	if len(found) == 0 {
		return nil
	}
	sort.Strings(found)
	return fmt.Errorf("this provider only speaks chat completions, and the request asks for %s, which that API has no equivalent for; drop it, or point a Responses-API provider at it instead",
		strings.Join(found, ", "))
}

// responsesToChat rewrites a Responses request as a chat-completions request.
func responsesToChat(body []byte) ([]byte, error) {
	var src map[string]json.RawMessage
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, fmt.Errorf("request body is not a JSON object: %w", err)
	}
	if err := refuseResponsesOnly(src); err != nil {
		return nil, err
	}

	out := map[string]json.RawMessage{}
	// model crosses for the same reason chatToResponses keeps it: the
	// provider validates it, and ReplaceModel has already run on this body
	// so the value is the upstream id whenever upstreamName renamed it.
	if v, ok := src["model"]; ok {
		out["model"] = v
	}
	for _, k := range responsesPassThrough {
		if v, ok := src[k]; ok {
			out[k] = v
		}
	}
	// Everything the translator does not name goes across as it was sent; the
	// conversions below still overwrite whatever they produce.
	passUnknown(src, out, responsesHandled)
	if v, ok := src["max_output_tokens"]; ok {
		out["max_completion_tokens"] = v
	}
	// reasoning.effort becomes reasoning_effort.
	if v, ok := src["reasoning"]; ok {
		var r map[string]json.RawMessage
		if err := json.Unmarshal(v, &r); err == nil {
			if effort, ok := r["effort"]; ok {
				out["reasoning_effort"] = effort
			}
		}
	}
	// text.format becomes response_format.
	if v, ok := src["text"]; ok {
		rf, err := textFormatToResponseFormat(v)
		if err != nil {
			return nil, err
		}
		out["response_format"] = rf
	}

	var msgs []json.RawMessage
	if instructions, ok := src["instructions"]; ok {
		text, err := messageText(instructions)
		if err == nil && text != "" {
			msg, err := simpleMessage("system", text)
			if err != nil {
				return nil, err
			}
			msgs = append(msgs, msg)
		}
	}
	input, hasInput := src["input"]
	if hasInput {
		converted, err := responsesInputToMessages(input)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, converted...)
	} else if _, hasMessages := src["messages"]; hasMessages {
		// No input, but the body already reads as chat: a chat payload
		// posted to the Responses endpoint. Forwarding it as it stands is
		// the translation - rebuilding it from the instructions field alone
		// would replace messages with whatever instructions produced.
		return body, nil
	} else if len(msgs) == 0 {
		// Neither input nor messages, and no instructions to make a system
		// message from: nothing to translate. Say so here rather than
		// forwarding a body the provider can only reject with a schema error
		// about a field this side never sent.
		return nil, fmt.Errorf("this Responses request has neither input nor messages, so there is no conversation to translate into a chat completions request; put the conversation in input, or call /v1/chat/completions directly")
	}
	enc, err := marshalArray(msgs)
	if err != nil {
		return nil, err
	}
	out["messages"] = enc
	return marshalObject(out)
}

// responsesToChatCompletion rewrites a Responses answer as a chat completion.
func responsesToChatCompletion(body []byte) ([]byte, error) {
	var resp struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Created int64  `json:"created_at"`
		Status  string `json:"status"`
		Output  []struct {
			Type    string `json:"type"`
			ID      string `json:"id"`
			Role    string `json:"role"`
			Status  string `json:"status"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"output"`
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}

	var text strings.Builder
	var calls []map[string]any
	for _, item := range resp.Output {
		switch item.Type {
		case "message", "":
			for _, c := range item.Content {
				if c.Type == "output_text" || c.Type == "input_text" || c.Type == "" {
					text.WriteString(c.Text)
				}
			}
		case "function_call":
			id := item.CallID
			if id == "" {
				id = item.ID
			}
			calls = append(calls, map[string]any{
				"id":   id,
				"type": "function",
				"function": map[string]any{
					"name":      item.Name,
					"arguments": item.Arguments,
				},
			})
		}
	}

	message := map[string]any{
		"role":    "assistant",
		"content": text.String(),
	}
	if len(calls) > 0 {
		message["tool_calls"] = calls
	}

	finish := "stop"
	if resp.Status == "incomplete" || (resp.IncompleteDetails != nil && resp.IncompleteDetails.Reason != "") {
		finish = "length"
	}

	completion := map[string]any{
		"id":      chatID(resp.ID),
		"object":  "chat.completion",
		"created": resp.Created,
		"model":   resp.Model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
	}
	if resp.Usage != nil {
		completion["usage"] = map[string]any{
			"prompt_tokens":     resp.Usage.InputTokens,
			"completion_tokens": resp.Usage.OutputTokens,
			"total_tokens":      resp.Usage.TotalTokens,
		}
	}
	return json.Marshal(completion)
}

// chatCompletionToResponse rewrites a chat completion as a Responses answer.
func chatCompletionToResponse(body []byte) ([]byte, error) {
	var completion struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Created int64  `json:"created"`
		Choices []struct {
			Index   int `json:"index"`
			Message struct {
				Role      string `json:"role"`
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &completion); err != nil {
		return nil, err
	}

	var output []map[string]any
	finish := "stop"
	if len(completion.Choices) > 0 {
		ch := completion.Choices[0]
		if ch.FinishReason != "" {
			finish = ch.FinishReason
		}
		if ch.Message.Content != "" || len(ch.Message.ToolCalls) == 0 {
			output = append(output, map[string]any{
				"type":   "message",
				"id":     "msg_" + chatID(completion.ID),
				"status": "completed",
				"role":   "assistant",
				"content": []map[string]any{{
					"type":        "output_text",
					"text":        ch.Message.Content,
					"annotations": []any{},
				}},
			})
		}
		for _, c := range ch.Message.ToolCalls {
			output = append(output, map[string]any{
				"type":      "function_call",
				"id":        c.ID,
				"call_id":   c.ID,
				"status":    "completed",
				"name":      c.Function.Name,
				"arguments": c.Function.Arguments,
			})
		}
	}

	resp := map[string]any{
		"id":     responseID(completion.ID),
		"object": "response",
		"model":  completion.Model,
		"output": output,
	}
	if completion.Created > 0 {
		resp["created_at"] = completion.Created
	}
	status := "completed"
	if finish == "length" {
		status = "incomplete"
		resp["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	resp["status"] = status
	if completion.Usage != nil {
		resp["usage"] = map[string]any{
			"input_tokens":  completion.Usage.PromptTokens,
			"output_tokens": completion.Usage.CompletionTokens,
			"total_tokens":  completion.Usage.TotalTokens,
		}
	}
	return json.Marshal(resp)
}

// ------------------------------------------------------------------ streams

// streamEvent is one server-sent event.
type streamEvent struct {
	name string
	data string
	done bool
}

// parseSSEBlock reads one event's fields out of its block.
func parseSSEBlock(block string) *streamEvent {
	ev := &streamEvent{}
	var data strings.Builder
	// Normalise CRLF and bare CR line endings before splitting, so a block
	// framed either way parses the same.
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
	if strings.TrimSpace(ev.data) == "[DONE]" {
		ev.done = true
		return ev
	}
	if ev.name == "" {
		// Chat completions carry no event name; the JSON's type field is the
		// event name on the Responses wire, and absent there too on chat.
		var probe struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(ev.data), &probe) == nil {
			ev.name = probe.Type
		}
	}
	return ev
}

// sseTranslator translates a single stream between the two wires.
type sseTranslator struct {
	r io.Reader
	w io.Writer

	// err is the first failure to write to the client. A stream cannot be
	// re-sent, so once a write has failed the translation is over: every
	// later emit is skipped and the copy loop unwinds, which closes the
	// upstream body instead of parsing a response nobody is reading.
	err error

	// fromChat says the upstream speaks chat and the client wants responses.
	fromChat bool

	// pending accumulates a partial event until its blank line arrives.
	pending strings.Builder

	// State carried across events for a stream.
	created bool
	// closed says the terminal event for the client's wire has been emitted, so
	// finish knows there is nothing left to owe.
	closed bool
	// doneSent says a chat [DONE] has gone out. Closed cannot cover that: it is
	// set by response.completed, which is the other direction's terminal.
	doneSent  bool
	respID    string
	model     string
	createdAt int64
	// text accumulated so far, for the done events.
	text strings.Builder
	// function call items, keyed by the chat stream's tool_call index.
	calls         map[int]*callState
	nextCallIndex int
}

type callState struct {
	id   string
	name string
	// index is the output_index this call was announced with. Every later
	// event about the call - its argument deltas, its terminal events - must
	// report that same index, and the terminal events must come out in the
	// order the calls were added.
	index     int
	arguments strings.Builder
}

// orderedCalls lists the accumulated calls in the order they were announced.
// Ranging the map directly would emit the terminal events in a different order
// on every run, which is a sequence a client correlating calls by position
// cannot rely on.
func (t *sseTranslator) orderedCalls() []*callState {
	out := make([]*callState, 0, len(t.calls))
	for _, state := range t.calls {
		out = append(out, state)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].index < out[b].index })
	return out
}

// sseReadBuffer is how many bytes are pulled from the reader at a time while
// translating a stream.
const sseReadBuffer = 32 << 10

// translateStream copies a streamed answer from r to w in the client's wire
// format, flushing each event as it is translated.
func (t *translation) translateStream(r io.Reader, w io.Writer) error {
	if t.to == wireAnthropic {
		return anthropic.TranslateStream(r, w, anthropic.StreamOptions{IncludeUsage: t.includeUsage})
	}
	tr := &sseTranslator{r: r, w: w, fromChat: t.from == wireChat}
	buf := make([]byte, sseReadBuffer)
	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			// The pending cap is an error to honor, not a suggestion: a proxy
			// that never terminates its event block must abort here rather
			// than grow pending to the size of the whole response.
			if _, err := tr.consume(buf[:n]); err != nil {
				return err
			}
			// Flush after every read, so a chunk reaches the client as soon as
			// it has been translated. Without this the stream would arrive in
			// 32KB lumps and stop being a stream. (flushWriter implements
			// flushing through http.ResponseController.Flush in its Write.)
		}
		if readErr != nil {
			// io.EOF ends the stream normally. Anything else is a failure
			// part-way through: whatever was translated is already on the wire
			// and a stream cannot be re-sent, so the error is reported rather
			// than turned into an error body mid-stream.
			if readErr == io.EOF {
				tr.finish()
				// finish emits the terminal event; a client that hung up
				// during it is a failed write like any other.
				return tr.err
			}
			return readErr
		}
	}
}

// finish emits whatever terminal event the client's wire expects, for a stream
// that ended without one.
func (t *sseTranslator) finish() {
	// An event that was not terminated by a blank line is still an event: some
	// upstreams end the stream right after the last data line, with no trailing
	// blank line. Flush it before the terminal event rather than dropping the
	// whole final block on the floor.
	if rest := t.pending.String(); strings.TrimSpace(rest) != "" {
		t.pending.Reset()
		t.handleBlock(rest)
	}
	if t.fromChat {
		// Client wants chat; upstream was responses. A chat stream's terminal
		// is [DONE], and an upstream that ends without a terminal event still
		// owes the client one: without it the client waits for a frame that
		// will never come, or reports the answer as truncated when it was only
		// the stream that was. A terminal event upstream did arrive has
		// already emitted one (response.completed, or the [DONE] a failure
		// emits), and a second terminator would be a protocol error, so this
		// is only for a stream that genuinely ended without one.
		if !t.doneSent {
			t.emitDone()
		}
		return
	}
	if !t.created {
		return
	}
	if !t.closed {
		t.emit("response.completed", map[string]any{
			"type":     "response.completed",
			"response": t.responseObject("completed"),
		})
	}
}

// consume feeds translated bytes into the translator, writing finished events
// through to the client. It is called by the copy loop.
//
// Everything is accumulated in pending and split from there, so a separator
// straddling two reads is still seen. Real-world SSE framing is CRLF
// ("\r\n\r\n"): splitting on "\n\n" alone left a CRLF stream as one enormous
// unparsed block that never produced an event. pending is capped so a proxy
// that never sends the blank line cannot grow this into an OOM.
func (t *sseTranslator) consume(p []byte) (int, error) {
	t.pending.WriteString(string(p))
	if t.pending.Len() > maxSSEPending {
		return 0, fmt.Errorf("sse translation: upstream sent %d bytes without a blank line, aborting", t.pending.Len())
	}
	for {
		block, rest, ok := splitSSEBlock(t.pending.String())
		if !ok {
			return len(p), nil
		}
		// Keep the remainder and translate the block. Reset+Write avoids
		// reallocating the builder every event.
		t.pending.Reset()
		t.pending.WriteString(rest)
		t.handleBlock(block)
		if t.err != nil {
			// The client is gone. Stop translating and let the copy loop
			// return the write failure rather than keep reading upstream.
			return 0, t.err
		}
	}
}

// sseSeparators are the blank-line forms an event can be terminated by: the
// LF form OpenAI sends, the CRLF form most gateways and proxies emit, and the
// bare CR form an old server may use.
var sseSeparators = []string{"\r\n\r\n", "\n\n", "\r\r"}

// splitSSEBlock cuts the first complete event off s, returning the block, the
// untranslated remainder, and whether a separator was found. The earliest
// separator wins, so CRLF is preferred when both forms appear.
func splitSSEBlock(s string) (block, rest string, ok bool) {
	best := -1
	sepLen := 0
	for _, sep := range sseSeparators {
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

// handleBlock translates one raw SSE block.
func (t *sseTranslator) handleBlock(block string) {
	ev := parseSSEBlock(block)
	if ev == nil {
		return
	}
	if ev.done {
		// [DONE] from a chat upstream: the stream is over. If the client wants
		// responses, it still needs the terminal response.completed event.
		// The other way round a provider is entitled to end its Responses stream
		// with [DONE] too, and the client is owed the terminator of its own
		// dialect - which emitDone makes sure happens exactly once, so the
		// upstream's [DONE] and finish() cannot both send one.
		if t.fromChat {
			t.emitDone()
			return
		}
		if t.created && !t.closed {
			t.emit("response.completed", map[string]any{
				"type":     "response.completed",
				"response": t.responseObject("completed"),
			})
		}
		return
	}
	if t.fromChat {
		t.handleResponsesEvent(ev)
		return
	}
	t.handleChatEvent(ev)
}

// handleResponsesEvent translates one Responses event into chat chunks.
func (t *sseTranslator) handleResponsesEvent(ev *streamEvent) {
	var payload map[string]any
	if err := json.Unmarshal([]byte(ev.data), &payload); err != nil {
		return
	}
	switch ev.name {
	case "response.created", "response.in_progress":
		if resp, ok := payload["response"].(map[string]any); ok {
			t.learnResponse(resp)
		}
		t.emitChat(t.chatChunk(map[string]any{"role": "assistant", "content": ""}))
	case "response.output_text.delta":
		delta, _ := payload["delta"].(string)
		if delta == "" {
			return
		}
		t.text.WriteString(delta)
		t.emitChat(t.chatChunk(map[string]any{"content": delta}))
	case "response.completed", "response.incomplete":
		if resp, ok := payload["response"].(map[string]any); ok {
			t.learnResponse(resp)
		}
		t.emitChat(t.chatChunk(map[string]any{}, "stop"))
		t.emitDone()
	case "response.failed", "error":
		// The upstream said the answer failed. Reporting a clean finish_reason:
		// stop here would hand the client a truncated or empty answer with
		// nothing to tell it so - the chat stream's terminal event carries no
		// room for a failure, and a client that saw only "stop" has no way to
		// tell a completed answer from a model that gave up halfway.
		//
		// The in-band form is the one clients already handle: a data frame
		// carrying an error object, then [DONE]. The OpenAI SDKs raise on the
		// error key, and the frame is a legal (if not chunk-shaped) SSE event,
		// so this is how the failure reaches the caller instead of being
		// discarded here.
		t.emitChat(map[string]any{
			"error": map[string]any{
				"message": failureMessage(payload),
				"type":    "server_error",
				"code":    nil,
			},
		})
		t.emitDone()
	default:
		// Every other Responses event (content_part, output_item, reasoning,
		// annotations) has no chat counterpart. Dropping it is correct: the
		// client asked for a chat stream and these carry no chat fields.
	}
}

// handleChatEvent translates one chat chunk into Responses events.
func (t *sseTranslator) handleChatEvent(ev *streamEvent) {
	var chunk struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Created int64  `json:"created"`
		Choices []struct {
			Delta struct {
				Role      string `json:"role"`
				Content   string `json:"content"`
				ToolCalls []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(ev.data), &chunk); err != nil {
		return
	}
	if chunk.ID != "" && !t.created {
		t.startResponse(chunk.ID, chunk.Model, chunk.Created)
	}
	if len(chunk.Choices) == 0 {
		return
	}
	ch := chunk.Choices[0]
	if !t.created {
		t.startResponse(chunk.ID, chunk.Model, chunk.Created)
	}
	if ch.Delta.Content != "" {
		t.text.WriteString(ch.Delta.Content)
		t.emit("response.output_text.delta", map[string]any{
			"type":          "response.output_text.delta",
			"item_id":       "msg_" + t.respID,
			"output_index":  0,
			"content_index": 0,
			"delta":         ch.Delta.Content,
		})
	}
	for _, c := range ch.Delta.ToolCalls {
		state, ok := t.calls[c.Index]
		if !ok {
			state = &callState{index: t.nextCallIndex}
			if t.calls == nil {
				t.calls = map[int]*callState{}
			}
			t.calls[c.Index] = state
			if c.ID != "" {
				state.id = c.ID
			}
			state.name = c.Function.Name
			t.emit("response.output_item.added", map[string]any{
				"type":         "response.output_item.added",
				"output_index": t.nextCallIndex,
				"item": map[string]any{
					"type":      "function_call",
					"id":        state.id,
					"call_id":   state.id,
					"name":      state.name,
					"arguments": "",
				},
			})
			t.nextCallIndex++
		}
		// A provider is allowed to send the id, or the function name, in a
		// later chunk than the one that opens the call - the first chunk may
		// carry only the index. Taking the id from that first chunk and never
		// looking again left output_item.added, every arguments event and both
		// terminal events carrying "", which is an id a client cannot correlate
		// a tool result with. A later chunk's id wins, since the one already
		// announced was empty rather than wrong.
		if c.ID != "" && state.id == "" {
			state.id = c.ID
		}
		if state.name == "" && c.Function.Name != "" {
			state.name = c.Function.Name
		}
		if c.Function.Arguments != "" {
			state.arguments.WriteString(c.Function.Arguments)
			t.emit("response.function_call_arguments.delta", map[string]any{
				"type":         "response.function_call_arguments.delta",
				"item_id":      state.id,
				"output_index": state.index,
				"delta":        c.Function.Arguments,
			})
		}
	}
	if ch.FinishReason != nil && *ch.FinishReason != "" {
		t.finishStream(*ch.FinishReason)
	}
}

// startResponse emits response.created for a chat stream.
func (t *sseTranslator) startResponse(chatID, model string, created int64) {
	t.created = true
	t.respID = responseID(chatID)
	t.model = model
	t.createdAt = created
	if t.calls == nil {
		t.calls = map[int]*callState{}
	}
	t.emit("response.created", map[string]any{
		"type":     "response.created",
		"response": t.responseObject("in_progress"),
	})
	t.emit("response.in_progress", map[string]any{
		"type":     "response.in_progress",
		"response": t.responseObject("in_progress"),
	})
}

// learnResponse takes the identity fields off a Responses object.
func (t *sseTranslator) learnResponse(resp map[string]any) {
	if id, ok := resp["id"].(string); ok && id != "" {
		t.respID = id
		t.created = true
	}
	if m, ok := resp["model"].(string); ok && m != "" {
		t.model = m
	}
	if c, ok := resp["created_at"].(float64); ok && c > 0 {
		t.createdAt = int64(c)
	}
}

// finishStream emits the terminal events for a chat stream that has ended.
func (t *sseTranslator) finishStream(reason string) {
	status := "completed"
	if reason == "length" {
		status = "incomplete"
	}
	if t.text.Len() > 0 {
		t.emit("response.output_text.done", map[string]any{
			"type":          "response.output_text.done",
			"item_id":       "msg_" + t.respID,
			"output_index":  0,
			"content_index": 0,
		})
	}
	for _, state := range t.orderedCalls() {
		t.emit("response.function_call_arguments.done", map[string]any{
			"type":         "response.function_call_arguments.done",
			"item_id":      state.id,
			"output_index": state.index,
			"arguments":    state.arguments.String(),
		})
		// Every output_item.added owes an output_item.done: a client that
		// tracks items by output_index never learns one finished otherwise.
		t.emit("response.output_item.done", map[string]any{
			"type":         "response.output_item.done",
			"output_index": state.index,
			"item": map[string]any{
				"type":      "function_call",
				"id":        state.id,
				"call_id":   state.id,
				"name":      state.name,
				"arguments": state.arguments.String(),
			},
		})
	}
	resp := t.responseObject(status)
	if status == "incomplete" {
		resp["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	t.emit("response.completed", map[string]any{
		"type":     "response.completed",
		"response": resp,
	})
	t.closed = true
}

// responseObject builds the Responses object the stream events carry.
func (t *sseTranslator) responseObject(status string) map[string]any {
	out := []map[string]any{}
	if t.text.Len() > 0 {
		out = append(out, map[string]any{
			"type":   "message",
			"id":     "msg_" + t.respID,
			"status": "completed",
			"role":   "assistant",
			"content": []map[string]any{{
				"type":        "output_text",
				"text":        t.text.String(),
				"annotations": []any{},
			}},
		})
	}
	obj := map[string]any{
		"id":         t.respID,
		"object":     "response",
		"status":     status,
		"model":      t.model,
		"output":     out,
		"created_at": t.createdAt,
	}
	return obj
}

// chatChunk builds one chat completion chunk.
func (t *sseTranslator) chatChunk(delta map[string]any, finish ...string) map[string]any {
	choice := map[string]any{
		"index": 0,
		"delta": delta,
	}
	if len(finish) > 0 {
		choice["finish_reason"] = finish[0]
	} else {
		choice["finish_reason"] = nil
	}
	return map[string]any{
		"id":      chatID(t.respID),
		"object":  "chat.completion.chunk",
		"created": t.createdAt,
		"model":   t.model,
		"choices": []map[string]any{choice},
	}
}

// writeFailed records the first failed write to the client. The copy loop
// reads it and stops: the bytes already written cannot be un-sent, and
// translating the rest of the stream would only burn CPU against a closed
// connection while the upstream runs to completion.
func (t *sseTranslator) writeFailed(err error) {
	if t.err == nil {
		t.err = fmt.Errorf("writing the translated stream to the client: %w", err)
	}
}

// emit writes one Responses event.
func (t *sseTranslator) emit(name string, payload map[string]any) {
	if t.err != nil {
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	if _, err := fmt.Fprintf(t.w, "event: %s\ndata: %s\n\n", name, body); err != nil {
		t.writeFailed(err)
		return
	}
	// response.completed is the terminal event of a Responses stream; seeing it
	// means finish() owes the client nothing more.
	if name == "response.completed" {
		t.closed = true
	}
}

// emitChat writes one chat chunk.
func (t *sseTranslator) emitChat(payload map[string]any) {
	if t.err != nil {
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	if _, err := fmt.Fprintf(t.w, "data: %s\n\n", body); err != nil {
		t.writeFailed(err)
	}
}

// emitDone writes the chat stream terminator, at most once per stream: a second
// [DONE] is a protocol error, and finish() would otherwise send one after a
// terminal event already did.
func (t *sseTranslator) emitDone() {
	if t.err != nil || t.doneSent {
		return
	}
	if _, err := fmt.Fprint(t.w, "data: [DONE]\n\n"); err != nil {
		t.writeFailed(err)
		return
	}
	t.doneSent = true
}

// errorMessage pulls a human-readable message out of a failure event.
func errorMessage(payload map[string]any) string {
	errObj, ok := payload["error"].(map[string]any)
	if !ok {
		return ""
	}
	msg, _ := errObj["message"].(string)
	return msg
}

// failureMessage is the message for a failed response event, covering both
// shapes a provider can send it in: the bare error event carries a top-level
// message, while response.failed nests the error object inside the response it
// belongs to. An empty message would leave the client with a frame that says
// something failed without saying what, so a fallback covers the case where
// neither shape spelled it out.
func failureMessage(payload map[string]any) string {
	if msg := errorMessage(payload); msg != "" {
		return msg
	}
	if resp, ok := payload["response"].(map[string]any); ok {
		if msg := errorMessage(resp); msg != "" {
			return msg
		}
	}
	if msg, _ := payload["message"].(string); msg != "" {
		return msg
	}
	return "the upstream reported that this response failed"
}

// chatID renders a chat completion id from whatever the other wire called it.
func chatID(id string) string {
	if id == "" {
		return "chatcmpl-router"
	}
	if strings.HasPrefix(id, "resp_") {
		return "chatcmpl-" + strings.TrimPrefix(id, "resp_")
	}
	return id
}

// responseID renders a Responses id from whatever the other wire called it.
func responseID(id string) string {
	if id == "" {
		return "resp_router"
	}
	if strings.HasPrefix(id, "resp_") {
		return id
	}
	return "resp_" + strings.TrimPrefix(id, "chatcmpl-")
}

// rawString reads a JSON value as a string, returning "" for anything else.
func rawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// fieldSet collects every name in the given lists into one set.
func fieldSet(lists ...[]string) map[string]bool {
	set := make(map[string]bool)
	for _, list := range lists {
		for _, name := range list {
			set[name] = true
		}
	}
	return set
}

// tableNames lists the keys of a refuse table, so the fields it covers can join
// the set of names the translator has accounted for.
func tableNames(table map[string]string) []string {
	out := make([]string, 0, len(table))
	for name := range table {
		out = append(out, name)
	}
	return out
}

// passUnknown copies the fields src carries and the translator does not name
// from src to out.
//
// The policy elsewhere in this file is that an untranslatable field is refused
// by name rather than silently dropped, because a caller who asks for something
// and does not get it cannot tell the difference from having asked for nothing.
// A field in neither list is the case that rule does not reach: it belongs to
// neither wire, so this side has no idea what it means, and refusing it by name
// would be guessing too - top_k, enable_thinking, repetition_penalty and every
// provider extra would come back as 400s against providers that take them
// happily. So they cross untouched and the provider upstream gets to accept or
// reject them, which is the only party that actually knows.
//
// Unset values are not copied. null, [] and {} are how a client spells "leave
// this alone", and a field that is present but empty is not a request for
// anything: forwarding it would turn a body that asks for nothing into one that
// carries an argument the other side has to make sense of.
func passUnknown(src, out map[string]json.RawMessage, handled map[string]bool) {
	for name, v := range src {
		if handled[name] || isNullOrEmpty(v) {
			continue
		}
		out[name] = v
	}
}

// isNullOrEmpty reports whether a value is null or an empty container.
func isNullOrEmpty(raw json.RawMessage) bool {
	v := strings.TrimSpace(string(raw))
	return v == "" || v == "null" || v == "[]" || v == "{}"
}

// simpleMessage builds a plain text message.
func simpleMessage(role, text string) (json.RawMessage, error) {
	return marshalObject(map[string]json.RawMessage{
		"role":    mustMarshal(role),
		"content": mustMarshal(text),
	})
}

// marshalObject encodes a map as a JSON object.
func marshalObject(m map[string]json.RawMessage) (json.RawMessage, error) {
	return json.Marshal(m)
}

// marshalArray encodes a slice as a JSON array.
func marshalArray(v []json.RawMessage) (json.RawMessage, error) {
	return json.Marshal(v)
}

// mustMarshal encodes a value that cannot fail to encode.
func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`null`)
	}
	return b
}
