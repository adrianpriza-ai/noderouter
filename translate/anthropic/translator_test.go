package anthropic

import (
	"encoding/json"
	"testing"
)

func TestBuildRequestBasic(t *testing.T) {
	req := OpenAIChatRequest{
		Model: "claude-3-sonnet",
		Messages: []OpenAIMessage{
			{Role: "system", Content: []ContentBlock{NewTextBlock("policy")}},
			{Role: "user", Content: []ContentBlock{NewTextBlock("hello")}},
		},
		MaxTokens: ptrInt(123),
		Tools: []Tool{
			{
				Type: "function",
				Raw: map[string]any{
					"type": "function",
					"function": map[string]any{
						"name": "get_weather",
						"parameters": map[string]any{
							"type":       "object",
							"properties": map[string]any{},
						},
					},
				},
			},
		},
		Temperature: ptrFloat(0.3),
		TopP:        ptrFloat(0.9),
	}

	tr := NewTranslator()
	out, warnings, err := tr.BuildRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings, got %v", warnings)
	}
	if len(out.System) != 1 || len(out.Messages) != 1 {
		t.Fatalf("system/user split incorrect: %#v", out)
	}
	if out.MaxTokens != 123 {
		t.Fatalf("expected max_tokens propagated, got %d", out.MaxTokens)
	}
	if len(out.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(out.Tools))
	}
}

func TestMarshalRequestDropsJsonModeFlag(t *testing.T) {
	// json_mode is a translator-local flag, not a Messages API field: the
	// rendered body must not carry it.
	req := OpenAIChatRequest{
		Model:    "claude-3",
		Messages: []OpenAIMessage{{Role: "user", Content: []ContentBlock{NewTextBlock("ping")}}},
		ResponseFormat: map[string]any{
			"type":        "json_schema",
			"json_schema": map[string]any{"schema": map[string]any{"type": "object"}},
		},
	}
	built, _, err := NewTranslator().BuildRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !built.JsonMode {
		t.Fatal("expected json mode to be enabled on the built request")
	}
	raw, err := MarshalRequest(built)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("marshalled request is not JSON: %v", err)
	}
	if _, ok := wire["json_mode"]; ok {
		t.Errorf("rendered body carries json_mode, which the Messages API has never heard of: %s", raw)
	}
	if _, ok := wire["max_tokens"]; !ok {
		t.Errorf("rendered body has no max_tokens: %s", raw)
	}
}

func TestBuildRequestAddsDummyTool(t *testing.T) {
	req := OpenAIChatRequest{
		Model: "claude-3",
		Messages: []OpenAIMessage{
			{
				Role:    "user",
				Content: []ContentBlock{NewTextBlock("use tool")},
			},
			{
				Role: "assistant",
				ToolCalls: []ToolCall{
					{
						ID:   "call_1",
						Type: "function",
						Function: ToolCallFunction{
							Name:      "dummy",
							Arguments: "{}",
						},
					},
				},
			},
		},
	}

	tr := NewTranslator()
	out, _, err := tr.BuildRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Tools) == 0 {
		t.Fatalf("expected dummy tool injection")
	}
}

func TestBuildRequestMergesConsecutiveToolResults(t *testing.T) {
	req := OpenAIChatRequest{
		Model: "claude-3",
		Messages: []OpenAIMessage{
			{Role: "user", Content: []ContentBlock{NewTextBlock("run both")}},
			{
				Role: "assistant",
				ToolCalls: []ToolCall{
					{ID: "call_1", Type: "function", Function: ToolCallFunction{Name: "first", Arguments: "{}"}},
					{ID: "call_2", Type: "function", Function: ToolCallFunction{Name: "second", Arguments: "{}"}},
				},
			},
			{Role: "tool", ToolCallID: "call_1", Content: []ContentBlock{NewTextBlock("result 1")}},
			{Role: "tool", ToolCallID: "call_2", Content: []ContentBlock{NewTextBlock("result 2")}},
		},
	}

	out, _, err := NewTranslator().BuildRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Messages) != 3 {
		t.Fatalf("messages = %#v, want one merged user turn after the assistant", out.Messages)
	}
	if got := out.Messages[2].Role; got != "user" {
		t.Fatalf("merged message role = %q, want user", got)
	}
	if len(out.Messages[2].Content) != 2 {
		t.Fatalf("merged content = %#v, want two tool_result blocks", out.Messages[2].Content)
	}
	for i, wantID := range []string{"call_1", "call_2"} {
		block := out.Messages[2].Content[i]
		if block.Type != "tool_result" {
			t.Errorf("content[%d].type = %q, want tool_result", i, block.Type)
		}
		if got := block.Data["tool_use_id"]; got != wantID {
			t.Errorf("content[%d].tool_use_id = %v, want %q", i, got, wantID)
		}
	}
}

func TestBuildRequestMapsStructuredFunctionToolChoice(t *testing.T) {
	var req OpenAIChatRequest
	if err := json.Unmarshal([]byte(`{
		"model":"claude-3",
		"messages":[{"role":"user","content":"check the weather"}],
		"tools":[{"type":"function","function":{"name":"weather","parameters":{"type":"object"}}}],
		"tool_choice":{"type":"function","function":{"name":"weather"}},
		"parallel_tool_calls":false
	}`), &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}

	out, _, err := NewTranslator().BuildRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.ToolChoice == nil {
		t.Fatal("tool_choice was dropped")
	}
	if out.ToolChoice.Type != "tool" || out.ToolChoice.Name != "weather" {
		t.Fatalf("tool_choice = %#v, want type tool and name weather", out.ToolChoice)
	}
	if out.ToolChoice.DisableParallelToolUse == nil || !*out.ToolChoice.DisableParallelToolUse {
		t.Fatalf("tool_choice.disable_parallel_tool_use = %v, want true", out.ToolChoice.DisableParallelToolUse)
	}
}

func TestBuildRequestMapsToolChoiceNone(t *testing.T) {
	var req OpenAIChatRequest
	if err := json.Unmarshal([]byte(`{
		"model":"claude-3",
		"messages":[{"role":"user","content":"do not use tools"}],
		"tools":[{"type":"function","function":{"name":"weather","parameters":{"type":"object"}}}],
		"tool_choice":"none",
		"parallel_tool_calls":false
	}`), &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}

	out, _, err := NewTranslator().BuildRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Tools) != 1 {
		t.Fatalf("tools = %#v, want the provided tool retained with tool_choice none", out.Tools)
	}
	if out.ToolChoice == nil || out.ToolChoice.Type != "none" {
		t.Fatalf("tool_choice = %#v, want type none", out.ToolChoice)
	}
	if out.ToolChoice.DisableParallelToolUse != nil {
		t.Fatalf("tool_choice.disable_parallel_tool_use = %v, want omitted for none", out.ToolChoice.DisableParallelToolUse)
	}

	raw, err := MarshalRequest(out)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	var wire struct {
		Tools      []map[string]any `json:"tools"`
		ToolChoice *struct {
			Type string `json:"type"`
		} `json:"tool_choice"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal wire request: %v", err)
	}
	if len(wire.Tools) != 1 {
		t.Fatalf("wire tools = %#v, want one tool definition", wire.Tools)
	}
	if wire.ToolChoice == nil || wire.ToolChoice.Type != "none" {
		t.Fatalf("wire tool_choice = %#v, want type none", wire.ToolChoice)
	}
}

func TestBuildRequestMergesAdjacentSameRoleMessages(t *testing.T) {
	tests := []struct {
		name         string
		role         string
		messages     []OpenAIMessage
		messageIndex int
		messageCount int
	}{
		{
			name: "user",
			role: "user",
			messages: []OpenAIMessage{
				{Role: "user", Content: []ContentBlock{NewTextBlock("one")}},
				{Role: "user", Content: []ContentBlock{NewTextBlock("two")}},
			},
			messageIndex: 0,
			messageCount: 1,
		},
		{
			name: "assistant",
			role: "assistant",
			messages: []OpenAIMessage{
				{Role: "user", Content: []ContentBlock{NewTextBlock("start")}},
				{Role: "assistant", Content: []ContentBlock{NewTextBlock("one")}},
				{Role: "assistant", Content: []ContentBlock{NewTextBlock("two")}},
			},
			messageIndex: 1,
			messageCount: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := OpenAIChatRequest{
				Model:    "claude-3",
				Messages: tt.messages,
			}

			out, _, err := NewTranslator().BuildRequest(req)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(out.Messages) != tt.messageCount {
				t.Fatalf("messages = %#v, want %d messages", out.Messages, tt.messageCount)
			}
			merged := out.Messages[tt.messageIndex]
			if got := merged.Role; got != tt.role {
				t.Fatalf("merged role = %q, want %q", got, tt.role)
			}
			if len(merged.Content) != 2 {
				t.Fatalf("merged content = %#v, want both content blocks", merged.Content)
			}
			for i, want := range []string{"one", "two"} {
				if got, ok := merged.Content[i].AsText(); !ok || got != want {
					t.Errorf("content[%d] = %q, want %q", i, got, want)
				}
			}
		})
	}
}

func TestBuildRequestMergesConvertedToolAndUserRoles(t *testing.T) {
	req := OpenAIChatRequest{
		Model: "claude-3",
		Messages: []OpenAIMessage{
			{Role: "user", Content: []ContentBlock{NewTextBlock("start")}},
			{
				Role: "assistant",
				ToolCalls: []ToolCall{{
					ID: "call_1", Type: "function",
					Function: ToolCallFunction{Name: "lookup", Arguments: "{}"},
				}},
			},
			{Role: "tool", ToolCallID: "call_1", Content: []ContentBlock{NewTextBlock("result")}},
			{Role: "user", Content: []ContentBlock{NewTextBlock("continue")}},
		},
	}

	out, _, err := NewTranslator().BuildRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Messages) != 3 {
		t.Fatalf("messages = %#v, want initial user, assistant, and merged user turn", out.Messages)
	}
	if got := out.Messages[2].Role; got != "user" {
		t.Fatalf("merged message role = %q, want user", got)
	}
	if len(out.Messages[2].Content) != 2 || out.Messages[2].Content[0].Type != "tool_result" {
		t.Fatalf("merged user content = %#v, want tool result followed by text", out.Messages[2].Content)
	}
	if got, ok := out.Messages[2].Content[1].AsText(); !ok || got != "continue" {
		t.Errorf("merged user text = %q, want continue", got)
	}
}

func TestBuildRequestDoesNotMergeAcrossRoleBoundary(t *testing.T) {
	req := OpenAIChatRequest{
		Model: "claude-3",
		Messages: []OpenAIMessage{
			{Role: "user", Content: []ContentBlock{NewTextBlock("first")}},
			{Role: "assistant", Content: []ContentBlock{NewTextBlock("middle")}},
			{Role: "user", Content: []ContentBlock{NewTextBlock("second")}},
		},
	}

	out, _, err := NewTranslator().BuildRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Messages) != 3 {
		t.Fatalf("messages = %#v, want separate turns around the assistant", out.Messages)
	}
	if first, _ := out.Messages[0].Content[0].AsText(); first != "first" {
		t.Errorf("first user content = %q, want first", first)
	}
	if second, _ := out.Messages[2].Content[0].AsText(); second != "second" {
		t.Errorf("second user content = %q, want second", second)
	}
}

func TestBuildRequestRejectsMissingToolsWhenDisabled(t *testing.T) {
	req := OpenAIChatRequest{
		Model: "claude-3",
		Messages: []OpenAIMessage{
			{Role: "user", Content: []ContentBlock{NewTextBlock("ping")}},
			{
				Role: "assistant",
				ToolCalls: []ToolCall{
					{
						ID:   "call_1",
						Type: "function",
						Function: ToolCallFunction{
							Name:      "dummy",
							Arguments: "{}",
						},
					},
				},
			},
		},
	}

	tr := NewTranslator(WithoutDummyTool())
	if _, _, err := tr.BuildRequest(req); err == nil {
		t.Fatalf("expected error due to missing tools")
	}
}

func TestResponseFormatForcesJsonMode(t *testing.T) {
	req := OpenAIChatRequest{
		Model: "claude-3",
		Messages: []OpenAIMessage{
			{Role: "user", Content: []ContentBlock{NewTextBlock("ping")}},
		},
		ResponseFormat: map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"schema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"result": map[string]any{"type": "string"},
					},
				},
			},
		},
	}
	out, _, err := NewTranslator().BuildRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !out.JsonMode {
		t.Fatalf("expected json mode to be enabled")
	}
	if out.ToolChoice == nil || out.ToolChoice.Name != ResponseFormatToolName {
		t.Fatalf("expected tool choice forcing json tool: %#v", out.ToolChoice)
	}
}

func TestStopSettingSanitize(t *testing.T) {
	data := `"  "`
	var stop StopSetting
	if err := json.Unmarshal([]byte(data), &stop); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	req := OpenAIChatRequest{
		Model: "claude",
		Messages: []OpenAIMessage{
			{Role: "user", Content: []ContentBlock{NewTextBlock("ping")}},
		},
		Stop: &stop,
	}
	out, _, err := NewTranslator().BuildRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.StopSequences) != 0 {
		t.Fatalf("expected stop seq dropped: %#v", out.StopSequences)
	}
}

func TestUserIDFiltering(t *testing.T) {
	req := OpenAIChatRequest{
		Model: "claude",
		Messages: []OpenAIMessage{
			{Role: "user", Content: []ContentBlock{NewTextBlock("ping")}},
		},
		User: "user@example.com",
	}
	out, _, err := NewTranslator().BuildRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Metadata != nil {
		t.Fatalf("email should not propagate to metadata: %#v", out.Metadata)
	}
	req.User = "valid-user"
	out, _, err = NewTranslator().BuildRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Metadata == nil || out.Metadata["user_id"] != "valid-user" {
		t.Fatalf("expected user id to propagate: %#v", out.Metadata)
	}
}

func TestConvertResponseJsonTool(t *testing.T) {
	resp := AnthropicResponse{
		ID:    "resp_1",
		Model: "claude",
		Content: []ContentBlock{
			NewContentBlock("tool_use", map[string]any{
				"id":   "call_1",
				"name": ResponseFormatToolName,
				"input": map[string]any{
					"values": map[string]any{"result": "ok"},
				},
			}),
		},
		StopReason: "tool_use",
		Usage: AnthropicUsage{
			InputTokens:  10,
			OutputTokens: 5,
		},
	}
	out, err := NewTranslator().ConvertResponse(resp, "", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Choices) != 1 {
		t.Fatalf("expected one choice: %#v", out)
	}
	text, ok := out.Choices[0].Message.Content[0].AsText()
	if !ok || text == "" {
		t.Fatalf("expected json text content: %#v", out.Choices[0].Message.Content)
	}
	if out.Usage.TotalTokens != 15 {
		t.Fatalf("usage aggregation failed: %#v", out.Usage)
	}
}

func TestConvertResponsePreservesReasoningContent(t *testing.T) {
	resp := AnthropicResponse{
		Model: "claude",
		Content: []ContentBlock{
			NewContentBlock("thinking", map[string]any{"thinking": "plan first"}),
			NewTextBlock("answer"),
		},
		StopReason: "end_turn",
	}

	out, err := NewTranslator().ConvertResponse(resp, "", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	message := out.Choices[0].Message
	if message.ReasoningContent != "plan first" {
		t.Errorf("reasoning_content = %q, want plan first", message.ReasoningContent)
	}
	if text, ok := message.Content[0].AsText(); !ok || text != "answer" {
		t.Errorf("content = %#v, want answer", message.Content)
	}

	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	var wire struct {
		Choices []struct {
			Message struct {
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if got := wire.Choices[0].Message.ReasoningContent; got != "plan first" {
		t.Errorf("wire reasoning_content = %q, want plan first", got)
	}
}

func TestConvertResponseKeepsReasoningWhenJsonToolIsFolded(t *testing.T) {
	resp := AnthropicResponse{
		Model: "claude",
		Content: []ContentBlock{
			NewContentBlock("thinking", map[string]any{"thinking": "plan first"}),
			NewContentBlock("tool_use", map[string]any{
				"id":    "call_json",
				"name":  ResponseFormatToolName,
				"input": map[string]any{"values": map[string]any{"result": "ok"}},
			}),
		},
		StopReason: "tool_use",
	}

	out, err := NewTranslator().ConvertResponse(resp, "", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	message := out.Choices[0].Message
	if message.ReasoningContent != "plan first" {
		t.Errorf("reasoning_content = %q, want plan first", message.ReasoningContent)
	}
	if text, ok := message.Content[0].AsText(); !ok || text != `{"result":"ok"}` {
		t.Errorf("folded content = %#v, want the JSON tool result", message.Content)
	}
}

// TestConvertResponseIsACompleteChatCompletion covers the router-specific
// adaptation: the answer has to carry every field a chat client reads, not a
// subset of one.
func TestConvertResponseIsACompleteChatCompletion(t *testing.T) {
	resp := AnthropicResponse{
		ID:         "msg_abc",
		Model:      "claude-3-sonnet",
		Content:    []ContentBlock{NewTextBlock("pong")},
		StopReason: "end_turn",
		Usage:      AnthropicUsage{InputTokens: 3, OutputTokens: 1},
	}
	out, err := NewTranslator().ConvertResponse(resp, "", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Object != "chat.completion" {
		t.Errorf("object = %q, want chat.completion", out.Object)
	}
	if out.Created == 0 {
		t.Error("created is unset; chat clients read it as the answer's timestamp")
	}
	if out.ID != "chatcmpl-abc" {
		t.Errorf("id = %q, want the Anthropic msg_ id re-spelled as chatcmpl-abc", out.ID)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("answer does not parse as a chat completion: %v\n%s", err, raw)
	}
	if len(wire.Choices) != 1 {
		t.Fatalf("choices = %v, want one", raw)
	}
	if got := wire.Choices[0].Message.Content; got != "pong" {
		t.Errorf("message.content = %q, want the plain string clients expect", got)
	}
	if finish := out.Choices[0].FinishReason; finish != "stop" {
		t.Errorf("finish_reason = %q, want end_turn translated to stop", finish)
	}
}

func TestMapFinishReasonUsesTheOpenAIWords(t *testing.T) {
	for anthropic, openai := range map[string]string{
		"end_turn":      "stop",
		"stop_sequence": "stop",
		"pause_turn":    "stop",
		"":              "stop",
		"max_tokens":    "length",
		"tool_use":      "tool_calls",
		"refusal":       "content_filter",
	} {
		if got := MapFinishReason(anthropic); got != openai {
			t.Errorf("MapFinishReason(%q) = %q, want %q", anthropic, got, openai)
		}
	}
}

func TestWebSearchMapping(t *testing.T) {
	req := OpenAIChatRequest{
		Model: "claude",
		Messages: []OpenAIMessage{
			{Role: "user", Content: []ContentBlock{NewTextBlock("ping")}},
		},
		WebSearchOptions: &WebSearchOptions{
			UserLocation: map[string]any{"approximate": map[string]any{"country": "US"}},
		},
	}
	out, _, err := NewTranslator().BuildRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Tools) == 0 {
		t.Fatalf("expected web search tool injection")
	}
}

// F4: Anthropic 400s when thinking.budget_tokens >= max_tokens, so a
// reasoning_effort-derived budget larger than the client's max_tokens must be
// capped to leave at least one output token.
func TestBuildRequestCapsReasoningBudgetToFitMaxTokens(t *testing.T) {
	req := OpenAIChatRequest{
		Model:    "claude-3-sonnet",
		Messages: []OpenAIMessage{{Role: "user", Content: []ContentBlock{NewTextBlock("ping")}}},
		// "high" maps to reasoningBudgetHigh (4096) by default.
		ReasoningEffort: "high",
		MaxTokens:       ptrInt(512),
	}

	out, warnings, err := NewTranslator().BuildRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Thinking == nil {
		t.Fatal("expected thinking to be enabled")
	}
	if out.Thinking.BudgetTokens != 511 {
		t.Fatalf("budget_tokens = %d, want capped to 511 (max_tokens 512 - 1)", out.Thinking.BudgetTokens)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want one warning about the cap", warnings)
	}
}

// F4: an explicit thinking.budget_tokens above max_tokens is capped the same
// way, not just effort-derived budgets.
func TestBuildRequestCapsExplicitThinkingBudget(t *testing.T) {
	req := OpenAIChatRequest{
		Model:     "claude-3-sonnet",
		Messages:  []OpenAIMessage{{Role: "user", Content: []ContentBlock{NewTextBlock("ping")}}},
		Thinking:  &ThinkingConfig{Type: "enabled", BudgetTokens: 8192},
		MaxTokens: ptrInt(4096),
	}

	out, warnings, err := NewTranslator().BuildRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Thinking.BudgetTokens != 4095 {
		t.Fatalf("budget_tokens = %d, want capped to 4095", out.Thinking.BudgetTokens)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want one warning about the cap", warnings)
	}
}

// F4: a budget that already fits under max_tokens passes through untouched,
// with no warning.
func TestBuildRequestLeavesFittingThinkingBudgetAlone(t *testing.T) {
	req := OpenAIChatRequest{
		Model:     "claude-3-sonnet",
		Messages:  []OpenAIMessage{{Role: "user", Content: []ContentBlock{NewTextBlock("ping")}}},
		Thinking:  &ThinkingConfig{Type: "enabled", BudgetTokens: 1024},
		MaxTokens: ptrInt(4096),
	}

	out, warnings, err := NewTranslator().BuildRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Thinking.BudgetTokens != 1024 {
		t.Fatalf("budget_tokens = %d, want 1024 unchanged", out.Thinking.BudgetTokens)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
}

// F4: max_tokens <= 1 leaves no room for any thinking budget; refuse with a
// clear error rather than sending a request the API is guaranteed to reject.
func TestBuildRequestRejectsMaxTokensTooSmallForThinking(t *testing.T) {
	req := OpenAIChatRequest{
		Model:           "claude-3-sonnet",
		Messages:        []OpenAIMessage{{Role: "user", Content: []ContentBlock{NewTextBlock("ping")}}},
		ReasoningEffort: "high",
		MaxTokens:       ptrInt(1),
	}

	_, _, err := NewTranslator().BuildRequest(req)
	if err == nil {
		t.Fatal("expected error for max_tokens 1 with thinking enabled")
	}
}

// F5: extended thinking requires default sampling; temperature/top_p must
// not be forwarded when thinking is enabled, and the client is warned.
func TestBuildRequestDropsSamplingWhenThinkingEnabled(t *testing.T) {
	req := OpenAIChatRequest{
		Model:           "claude-3-sonnet",
		Messages:        []OpenAIMessage{{Role: "user", Content: []ContentBlock{NewTextBlock("ping")}}},
		ReasoningEffort: "high",
		// Large enough that the effort-derived budget (4096) already fits, so
		// the only adjustment under test is the sampling drop below.
		MaxTokens:   ptrInt(8192),
		Temperature: ptrFloat(0.7),
		TopP:        ptrFloat(0.95),
	}

	out, warnings, err := NewTranslator().BuildRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Temperature != nil {
		t.Fatalf("temperature = %v, want dropped while thinking is enabled", *out.Temperature)
	}
	if out.TopP != nil {
		t.Fatalf("top_p = %v, want dropped while thinking is enabled", *out.TopP)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, one wanted for the dropped sampling params", warnings)
	}

	// Same request without thinking keeps its sampling params.
	req.ReasoningEffort = ""
	out, warnings, err = NewTranslator().BuildRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Temperature == nil || *out.Temperature != 0.7 {
		t.Fatalf("temperature = %v, want 0.7 passed through", out.Temperature)
	}
	if out.TopP == nil || *out.TopP != 0.95 {
		t.Fatalf("top_p = %v, want 0.95 passed through", out.TopP)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
}

// F5: an explicitly disabled thinking config keeps sampling untouched, since
// the thinking-related sampling constraints do not apply.
func TestBuildRequestKeepsSamplingWhenThinkingDisabled(t *testing.T) {
	req := OpenAIChatRequest{
		Model:       "claude-3-sonnet",
		Messages:    []OpenAIMessage{{Role: "user", Content: []ContentBlock{NewTextBlock("ping")}}},
		Thinking:    &ThinkingConfig{Type: "disabled", BudgetTokens: 0},
		Temperature: ptrFloat(0.7),
	}

	out, warnings, err := NewTranslator().BuildRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Temperature == nil || *out.Temperature != 0.7 {
		t.Fatalf("temperature = %v, want 0.7 passed through", out.Temperature)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
}

// F8: chat-wire clients occasionally send a developer role, which the
// Messages API does not know; it is hoisted into the system prompt.
func TestBuildRequestHoistsDeveloperRoleIntoSystem(t *testing.T) {
	req := OpenAIChatRequest{
		Model: "claude-3",
		Messages: []OpenAIMessage{
			{Role: "developer", Content: []ContentBlock{NewTextBlock("you are terse")}},
			{Role: "user", Content: []ContentBlock{NewTextBlock("hi")}},
		},
	}

	out, warnings, err := NewTranslator().BuildRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.System) != 1 {
		t.Fatalf("system = %#v, want the developer message hoisted", out.System)
	}
	if text, ok := out.System[0].AsText(); !ok || text != "you are terse" {
		t.Errorf("system[0] = %v, want the developer text", out.System[0])
	}
	if len(out.Messages) != 1 || out.Messages[0].Role != "user" {
		t.Fatalf("messages = %#v, want only the user turn left", out.Messages)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, one wanted for the hoisted role", warnings)
	}
}

// F8: Anthropic requires the first conversation message to use the user role;
// refuse with a clear error instead of an opaque upstream 400.
func TestBuildRequestRejectsAssistantFirstConversation(t *testing.T) {
	tests := []struct {
		name     string
		messages []OpenAIMessage
	}{
		{
			name: "assistant only",
			messages: []OpenAIMessage{
				{Role: "assistant", Content: []ContentBlock{NewTextBlock("hello")}},
			},
		},
		{
			name: "system then assistant",
			messages: []OpenAIMessage{
				{Role: "system", Content: []ContentBlock{NewTextBlock("policy")}},
				{Role: "assistant", Content: []ContentBlock{NewTextBlock("hello")}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := OpenAIChatRequest{Model: "claude-3", Messages: tt.messages}
			_, _, err := NewTranslator().BuildRequest(req)
			if err == nil {
				t.Fatal("expected error for assistant-first conversation")
			}
		})
	}
}

// F8: the Messages API caps stop_sequences at 4; a longer list is truncated
// with a warning rather than refused.
func TestBuildRequestTruncatesStopSequencesToAPIMaximum(t *testing.T) {
	build := func(values ...string) ([]string, []Warning) {
		req := OpenAIChatRequest{
			Model:    "claude-3",
			Messages: []OpenAIMessage{{Role: "user", Content: []ContentBlock{NewTextBlock("ping")}}},
			Stop:     &StopSetting{Values: values},
		}
		out, warnings, err := NewTranslator().BuildRequest(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return out.StopSequences, warnings
	}

	stops, warnings := build("a", "b", "c", "d", "e")
	if len(stops) != MaxStopSequences {
		t.Fatalf("stop_sequences = %v, want truncated to %d", stops, MaxStopSequences)
	}
	for i, want := range []string{"a", "b", "c", "d"} {
		if stops[i] != want {
			t.Errorf("stop_sequences[%d] = %q, want %q", i, stops[i], want)
		}
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, one wanted for the truncation", warnings)
	}

	stops, warnings = build("a", "b", "c", "d")
	if len(stops) != 4 {
		t.Fatalf("stop_sequences = %v, want all 4 kept", stops)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
}

// F8: MarshalJSON used to inject the type key into the shared Data map
// through a value receiver; concurrent marshalling would race on it.
func TestMarshalJSONDoesNotMutateSharedData(t *testing.T) {
	block := ContentBlock{Type: "text", Data: map[string]any{"text": "hi"}}
	encoded, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal block: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatalf("unmarshal block: %v", err)
	}
	if wire["type"] != "text" {
		t.Errorf("encoded block = %s, want the type key ensured", encoded)
	}
	if _, ok := block.Data["type"]; ok {
		t.Error("block.Data was mutated by MarshalJSON")
	}

	tool := AnthropicTool{Type: "custom", Data: map[string]any{"name": "lookup"}}
	encoded, err = json.Marshal(tool)
	if err != nil {
		t.Fatalf("marshal tool: %v", err)
	}
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatalf("unmarshal tool: %v", err)
	}
	if wire["type"] != "custom" {
		t.Errorf("encoded tool = %s, want the type key ensured", encoded)
	}
	if _, ok := tool.Data["type"]; ok {
		t.Error("tool.Data was mutated by MarshalJSON")
	}
}

// F8: with thinking enabled the JSON tool is offered but not forced; if the
// model calls it voluntarily the fold-back still reconstructs the plain JSON
// content the client's response_format asked for.
func TestJsonModeWithThinkingFoldsVoluntaryToolCall(t *testing.T) {
	req := OpenAIChatRequest{
		Model:    "claude-3-sonnet",
		Messages: []OpenAIMessage{{Role: "user", Content: []ContentBlock{NewTextBlock("ping")}}},
		ResponseFormat: map[string]any{
			"type": "json_object",
		},
		Thinking: &ThinkingConfig{Type: "enabled", BudgetTokens: 1024},
	}

	built, _, err := NewTranslator().BuildRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !built.JsonMode {
		t.Fatal("expected json mode to be enabled")
	}
	if built.ToolChoice != nil {
		t.Fatalf("tool_choice = %#v, want no forced tool while thinking is enabled", built.ToolChoice)
	}
	offered := false
	for _, tool := range built.Tools {
		if tool.Type == ResponseFormatToolName {
			offered = true
		}
	}
	if !offered {
		t.Fatal("expected the json tool to be offered")
	}

	resp := AnthropicResponse{
		ID:    "msg_1",
		Model: "claude-3-sonnet",
		Content: []ContentBlock{NewContentBlock("tool_use", map[string]any{
			"id":   "call_1",
			"name": ResponseFormatToolName,
			"input": map[string]any{
				"values": map[string]any{"answer": "42"},
			},
		})},
		StopReason: "tool_use",
	}
	out, err := NewTranslator().ConvertResponse(resp, "", built.JsonMode)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text, ok := out.Choices[0].Message.Content[0].AsText()
	if !ok || text != `{"answer":"42"}` {
		t.Fatalf("folded content = %#v, want the JSON values as text", out.Choices[0].Message.Content)
	}
}

func ptrInt(v int) *int           { return &v }
func ptrFloat(v float64) *float64 { return &v }
