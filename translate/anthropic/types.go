// Package anthropic translates between the OpenAI chat-completions wire and
// Anthropic's /v1/messages API.
//
// The request and response translation here is copied from the reference
// module at openai-anthropic-endpoint-translation/pkg/translator (itself a
// extraction of Upstream's translation pipeline), so the two can be compared
// line for line. Three things differ, because the router sends what this
// package produces straight to OpenAI-compatible clients:
//
//   - OpenAIChatResponse carries object and created, and ConvertResponse fills
//     them in, so the answer is a complete chat.completion rather than a
//     subset of one.
//   - A message whose content is a single text block marshals as a plain
//     string, which is what chat-completions clients expect content to be.
//   - MapFinishReason turns Anthropic's stop reasons (end_turn, tool_use,
//     max_tokens, ...) into the names the OpenAI wire uses.
//
// stream.go - the Anthropic-SSE to chat-chunk translation - has no
// counterpart in the reference module and was written for the router.

package anthropic

import (
	"encoding/json"
	"errors"
	"fmt"
)

const (
	// ResponseFormatToolName keeps parity with the upstream router so consumers can rely on a stable name.
	ResponseFormatToolName = "json_tool_call"

	// DefaultAnthropicChatMaxTokens mirrors upstream defaults for Anthropic chat.
	DefaultAnthropicChatMaxTokens = 4096

	// MaxStopSequences is the Messages API cap on stop_sequences entries.
	MaxStopSequences = 4

	reasoningBudgetMinimal = 128
	reasoningBudgetLow     = 1024
	reasoningBudgetMedium  = 2048
	reasoningBudgetHigh    = 4096
)

// Warning captures non-fatal translation issues (e.g., silently dropped params).
type Warning string

// StopSetting allows the OpenAI request to provide either a string or a list of stop sequences.
type StopSetting struct {
	Values []string
}

// MarshalJSON ensures the stop setting is always serialized as a JSON array.
func (s StopSetting) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.Values)
}

// UnmarshalJSON accepts strings or arrays for stop sequences.
func (s *StopSetting) UnmarshalJSON(data []byte) error {
	var asString string
	if err := json.Unmarshal(data, &asString); err == nil {
		if asString != "" {
			s.Values = []string{asString}
		}
		return nil
	}

	var asList []string
	if err := json.Unmarshal(data, &asList); err == nil {
		s.Values = asList
		return nil
	}

	return fmt.Errorf("invalid stop setting: %s", string(data))
}

// ContentBlock stores arbitrary message content while retaining type metadata.
// We intentionally keep a raw payload map so we can round-trip unrecognized keys.
type ContentBlock struct {
	Type string
	Data map[string]any
}

// NewContentBlock builds a content block with a shallow copy of the provided map.
func NewContentBlock(contentType string, payload map[string]any) ContentBlock {
	data := map[string]any{"type": contentType}
	for k, v := range payload {
		if k == "type" {
			continue
		}
		data[k] = v
	}
	return ContentBlock{
		Type: contentType,
		Data: data,
	}
}

// NewTextBlock is a helper for `"type": "text"` payloads.
func NewTextBlock(text string) ContentBlock {
	return NewContentBlock("text", map[string]any{"text": text})
}

// MarshalJSON serializes the preserved payload. The payload is copied before
// the type key is ensured, so marshalling never mutates shared block data
// (maps are references; a value receiver does not protect them).
func (c ContentBlock) MarshalJSON() ([]byte, error) {
	data := make(map[string]any, len(c.Data)+1)
	for k, v := range c.Data {
		data[k] = v
	}
	if _, ok := data["type"]; !ok {
		data["type"] = c.Type
	}
	return json.Marshal(data)
}

// UnmarshalJSON records the type and raw payload.
func (c *ContentBlock) UnmarshalJSON(data []byte) error {
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}
	value, _ := payload["type"].(string)
	c.Type = value
	c.Data = payload
	return nil
}

// Clone returns a shallow copy to avoid sharing maps between messages.
func (c ContentBlock) Clone() ContentBlock {
	copyData := make(map[string]any, len(c.Data))
	for k, v := range c.Data {
		copyData[k] = v
	}
	return ContentBlock{
		Type: c.Type,
		Data: copyData,
	}
}

// AsText fetches the text value if present.
func (c ContentBlock) AsText() (string, bool) {
	if c.Type != "text" {
		return "", false
	}
	value, _ := c.Data["text"].(string)
	return value, value != ""
}

// ToolCallFunction replicates OpenAI's `tool_calls[].function`.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolCall is the OpenAI tool call block emitted by the assistant role.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

// ToolChoice captures `tool_choice` semantics.
type ToolChoice struct {
	Kind         string
	FunctionName string
	RawObject    map[string]any
}

// MarshalJSON writes either a string literal or the original object.
func (t ToolChoice) MarshalJSON() ([]byte, error) {
	switch t.Kind {
	case "auto", "required", "none":
		return json.Marshal(t.Kind)
	case "tool":
		if t.RawObject != nil {
			return json.Marshal(t.RawObject)
		}
		if t.FunctionName != "" {
			return json.Marshal(map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": t.FunctionName,
				},
			})
		}
	}
	return json.Marshal(t.RawObject)
}

// UnmarshalJSON accepts OpenAI's literal or structured forms.
func (t *ToolChoice) UnmarshalJSON(data []byte) error {
	var literal string
	if err := json.Unmarshal(data, &literal); err == nil {
		t.Kind = literal
		return nil
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	t.RawObject = raw
	rawType, _ := raw["type"].(string)
	if rawType != "" {
		t.Kind = rawType
	}
	if rawType == "function" {
		if fn, _ := raw["function"].(map[string]any); fn != nil {
			if name, _ := fn["name"].(string); name != "" {
				t.FunctionName = name
			}
		}
	}
	return nil
}

// Tool stores the OpenAI tool definition but keeps raw metadata for passthrough.
type Tool struct {
	Type string
	Raw  map[string]any
}

// MarshalJSON preserves the original object.
func (t Tool) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.Raw)
}

// UnmarshalJSON records the object and exposes the `type`.
func (t *Tool) UnmarshalJSON(data []byte) error {
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}
	value, _ := payload["type"].(string)
	t.Type = value
	t.Raw = payload
	return nil
}

// Clone returns a shallow copy of the raw payload.
func (t Tool) Clone() Tool {
	raw := make(map[string]any, len(t.Raw))
	for k, v := range t.Raw {
		raw[k] = v
	}
	return Tool{
		Type: t.Type,
		Raw:  raw,
	}
}

// OpenAIMessage mirrors OpenAI chat message JSON structure.
type OpenAIMessage struct {
	Role             string         `json:"role"`
	Content          []ContentBlock `json:"content,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall     `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
	Name             string         `json:"name,omitempty"`
	Prefix           bool           `json:"prefix,omitempty"`
	Metadata         map[string]any `json:"metadata,omitempty"`
	CacheControl     map[string]any `json:"cache_control,omitempty"`
	// rawContent stores the original representation (string vs list) for debugging.
	rawContent json.RawMessage
}

// MarshalJSON keeps the converted slice representation, except for the common
// case: a message whose whole content is one text block goes out as the plain
// string chat-completions clients expect, rather than as a one-element parts
// array they mostly do not.
func (m OpenAIMessage) MarshalJSON() ([]byte, error) {
	type alias OpenAIMessage
	aux := struct {
		Content any `json:"content,omitempty"`
		alias
	}{
		alias: (alias)(m),
	}
	if len(m.Content) == 1 {
		if text, ok := m.Content[0].AsText(); ok {
			aux.Content = text
		}
	}
	if aux.Content == nil {
		aux.Content = m.Content
	}
	return json.Marshal(aux)
}

// UnmarshalJSON supports string or array `content` fields.
func (m *OpenAIMessage) UnmarshalJSON(data []byte) error {
	type alias OpenAIMessage
	aux := struct {
		Content json.RawMessage `json:"content"`
		*alias
	}{
		alias: (*alias)(m),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	m.rawContent = aux.Content
	if len(aux.Content) == 0 {
		return nil
	}

	var asString string
	if err := json.Unmarshal(aux.Content, &asString); err == nil {
		if asString != "" {
			m.Content = []ContentBlock{NewTextBlock(asString)}
		}
		return nil
	}

	var single map[string]any
	if err := json.Unmarshal(aux.Content, &single); err == nil {
		block := ContentBlock{}
		if err := block.UnmarshalJSON(aux.Content); err != nil {
			return err
		}
		m.Content = []ContentBlock{block}
		return nil
	}

	var blocks []ContentBlock
	if err := json.Unmarshal(aux.Content, &blocks); err != nil {
		return err
	}
	m.Content = blocks
	return nil
}

// WebSearchOptions mirrors the upstream OpenAI-compatible flag.
type WebSearchOptions struct {
	UserLocation      map[string]any `json:"user_location,omitempty"`
	SearchContextSize string         `json:"search_context_size,omitempty"`
}

// ThinkingConfig mirrors OpenAI `thinking` payload.
type ThinkingConfig struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

// OpenAIChatRequest aggregates OpenAI chat parameters needed for translation.
type OpenAIChatRequest struct {
	Model             string            `json:"model"`
	Messages          []OpenAIMessage   `json:"messages"`
	Tools             []Tool            `json:"tools,omitempty"`
	ToolChoice        *ToolChoice       `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool             `json:"parallel_tool_calls,omitempty"`
	Stream            bool              `json:"stream,omitempty"`
	MaxTokens         *int              `json:"max_tokens,omitempty"`
	MaxCompletion     *int              `json:"max_completion_tokens,omitempty"`
	Stop              *StopSetting      `json:"stop,omitempty"`
	Temperature       *float64          `json:"temperature,omitempty"`
	TopP              *float64          `json:"top_p,omitempty"`
	ResponseFormat    map[string]any    `json:"response_format,omitempty"`
	WebSearchOptions  *WebSearchOptions `json:"web_search_options,omitempty"`
	Metadata          map[string]any    `json:"metadata,omitempty"`
	ExtraHeaders      map[string]string `json:"extra_headers,omitempty"`
	User              string            `json:"user,omitempty"`
	ReasoningEffort   string            `json:"reasoning_effort,omitempty"`
	Thinking          *ThinkingConfig   `json:"thinking,omitempty"`
}

// AnthropicTool encapsulates Anthropic's tool definition while allowing custom fields.
type AnthropicTool struct {
	Type string
	Data map[string]any
}

// MarshalJSON ensures the payload always contains the `type` attribute,
// without mutating the shared Data map.
func (t AnthropicTool) MarshalJSON() ([]byte, error) {
	data := make(map[string]any, len(t.Data)+1)
	for k, v := range t.Data {
		data[k] = v
	}
	if _, ok := data["type"]; !ok {
		data["type"] = t.Type
	}
	return json.Marshal(data)
}

// AnthropicMCPServer describes MCP tool servers Anthropic understands.
type AnthropicMCPServer struct {
	Type               string         `json:"type"`
	URL                string         `json:"url"`
	Name               string         `json:"name"`
	AuthorizationToken string         `json:"authorization_token,omitempty"`
	ToolConfiguration  map[string]any `json:"tool_configuration,omitempty"`
}

// AnthropicToolChoice mirrors Anthropic's `tool_choice` request field.
type AnthropicToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse *bool  `json:"disable_parallel_tool_use,omitempty"`
}

// AnthropicThinking configures reasoning budget.
type AnthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

// AnthropicMessage is the wire-format for `/v1/messages`.
type AnthropicMessage struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
}

// AnthropicRequest is the payload we send to Anthropic's `/v1/messages`.
type AnthropicRequest struct {
	Model         string               `json:"model"`
	Messages      []AnthropicMessage   `json:"messages"`
	System        []ContentBlock       `json:"system,omitempty"`
	Tools         []AnthropicTool      `json:"tools,omitempty"`
	MCPServers    []AnthropicMCPServer `json:"mcp_servers,omitempty"`
	ToolChoice    *AnthropicToolChoice `json:"tool_choice,omitempty"`
	StopSequences []string             `json:"stop_sequences,omitempty"`
	Temperature   *float64             `json:"temperature,omitempty"`
	TopP          *float64             `json:"top_p,omitempty"`
	MaxTokens     int                  `json:"max_tokens,omitempty"`
	Metadata      map[string]any       `json:"metadata,omitempty"`
	JsonMode      bool                 `json:"json_mode,omitempty"`
	Stream        bool                 `json:"stream,omitempty"`
	Thinking      *AnthropicThinking   `json:"thinking,omitempty"`
	Headers       map[string]string    `json:"-"`
}

// AnthropicUsage mirrors the API response.
type AnthropicUsage struct {
	InputTokens            int            `json:"input_tokens"`
	OutputTokens           int            `json:"output_tokens"`
	CacheCreationInput     int            `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInput         int            `json:"cache_read_input_tokens,omitempty"`
	CacheCreationBreakdown map[string]int `json:"cache_creation,omitempty"`
	ServerToolUse          map[string]int `json:"server_tool_use,omitempty"`
}

// AnthropicResponse is a simplified snapshot of `/v1/messages` responses.
type AnthropicResponse struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Role       string         `json:"role"`
	Model      string         `json:"model"`
	Content    []ContentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      AnthropicUsage `json:"usage"`
	Error      map[string]any `json:"error,omitempty"`
}

// OpenAIChatResponse is the OpenAI-compatible structure consumers expect.
type OpenAIChatResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []OpenAIChoice `json:"choices"`
	Usage   Usage          `json:"usage"`
}

// OpenAIChoice mirrors the ChatCompletion choice shape.
type OpenAIChoice struct {
	Index        int           `json:"index"`
	FinishReason string        `json:"finish_reason"`
	Message      OpenAIMessage `json:"message"`
}

// Usage conforms to OpenAI's response usage block.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// StreamChunk tracks streaming translation output.
type StreamChunk struct {
	ID           string        `json:"id"`
	Choices      []DeltaChoice `json:"choices"`
	Usage        *Usage        `json:"usage,omitempty"`
	FinishReason string        `json:"finish_reason,omitempty"`
}

// DeltaChoice mirrors OpenAI's streaming `choices[].delta`.
type DeltaChoice struct {
	Index int          `json:"index"`
	Delta DeltaMessage `json:"delta"`
}

// DeltaMessage is the OpenAI streaming delta payload.
type DeltaMessage struct {
	Content  string    `json:"content,omitempty"`
	ToolCall *ToolCall `json:"tool_calls,omitempty"`
}

// ErrInvalidRequest is raised when the request cannot be translated cleanly.
var ErrInvalidRequest = errors.New("invalid OpenAI request")
