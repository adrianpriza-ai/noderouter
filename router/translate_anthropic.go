package router

// The chat <-> Anthropic Messages half of the translation. The rules of the
// game are the ones translate.go lays out for the two OpenAI surfaces: rewrite
// the request, the path and the answer so a client and a provider that speak
// different dialects can still meet, and refuse rather than silently drop what
// cannot be carried across.
//
// The heavy lifting lives in translate/anthropic, a copy of the reference
// module at openai-anthropic-endpoint-translation/pkg/translator with the
// router's own stream converter added. This file is the seam: it feeds a chat
// body to that package and remembers what the response side needs to know.

import (
	"encoding/json"
	"fmt"

	anthropic "noderouter/translate/anthropic"
)

// chatToAnthropic rewrites a chat-completions request as an Anthropic
// /v1/messages request. What the response side has to know - json mode, the
// assistant prefix, the usage tail - is recorded on t as it goes.
func (t *translation) chatToAnthropic(body []byte) ([]byte, error) {
	var src map[string]json.RawMessage
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, fmt.Errorf("request body is not a JSON object: %w", err)
	}

	var req anthropic.OpenAIChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("could not read the chat completions request: %w", err)
	}
	// model crosses for the same reason the OpenAI translators keep it: the
	// provider validates it, and ReplaceModel has already run on this body so
	// the value is the upstream id whenever upstreamName renamed it. It is
	// read off the raw body rather than the parsed one for that reason.
	var model string
	if v, ok := src["model"]; ok {
		_ = json.Unmarshal(v, &model)
	}
	req.Model = model

	built, _, err := anthropic.NewTranslator().BuildRequest(req)
	if err != nil {
		return nil, err
	}

	t.jsonMode = built.JsonMode
	t.prefixPrompt = anthropic.PrefixPrompt(req.Messages)
	t.includeUsage = streamIncludeUsage(src)

	encoded, err := anthropic.MarshalRequest(built)
	if err != nil {
		return nil, fmt.Errorf("could not render the Anthropic request: %w", err)
	}
	return encoded, nil
}

// anthropicToChat rewrites an Anthropic answer as a chat completion. A body
// the translator cannot read is returned as an error so the caller can hand
// the provider's own words to the client instead of a half-translated one.
func (t *translation) anthropicToChat(body []byte) ([]byte, error) {
	var resp anthropic.AnthropicResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("could not read the Anthropic response: %w", err)
	}
	out, err := anthropic.NewTranslator().ConvertResponse(resp, t.prefixPrompt, t.jsonMode)
	if err != nil {
		return nil, err
	}
	return json.Marshal(out)
}

// streamIncludeUsage reads stream_options.include_usage off the request, which
// is what makes the chat wire's usage tail chunk owed on the answer.
func streamIncludeUsage(src map[string]json.RawMessage) bool {
	raw, ok := src["stream_options"]
	if !ok {
		return false
	}
	var opts struct {
		IncludeUsage bool `json:"include_usage"`
	}
	_ = json.Unmarshal(raw, &opts)
	return opts.IncludeUsage
}
