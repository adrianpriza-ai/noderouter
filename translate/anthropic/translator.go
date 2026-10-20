package anthropic

// OpenAI chat completions -> Anthropic /v1/messages, and back. Copied from
// openai-anthropic-endpoint-translation/pkg/translator; see the package
// comment in types.go for the three places this copy deliberately differs.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Translator converts OpenAI-compatible requests/responses into Anthropic-compatible payloads.
type Translator struct {
	DropInvalidStops bool
	AllowDummyTool   bool
	DummyToolName    string
	DefaultMaxTokens int
}

// Option customizes the translator.
type Option func(*Translator)

// WithStopSanitizer toggles whitespace-stop stripping (default true).
func WithStopSanitizer(enabled bool) Option {
	return func(t *Translator) {
		t.DropInvalidStops = enabled
	}
}

// WithDummyTool toggles automatic dummy tool injection when tool calls exist but the request omitted `tools`.
func WithDummyTool(name string) Option {
	return func(t *Translator) {
		t.AllowDummyTool = true
		t.DummyToolName = name
	}
}

// WithoutDummyTool disables the automatic fallback.
func WithoutDummyTool() Option {
	return func(t *Translator) {
		t.AllowDummyTool = false
	}
}

// WithDefaultMaxTokens overrides the fallback for `max_tokens`.
func WithDefaultMaxTokens(max int) Option {
	return func(t *Translator) {
		if max > 0 {
			t.DefaultMaxTokens = max
		}
	}
}

// NewTranslator returns a translator with sensible defaults inspired by the upstream router.
func NewTranslator(opts ...Option) Translator {
	t := Translator{
		DropInvalidStops: true,
		AllowDummyTool:   true,
		DummyToolName:    "auto_added_tool",
		DefaultMaxTokens: DefaultAnthropicChatMaxTokens,
	}
	for _, opt := range opts {
		opt(&t)
	}
	return t
}

// BuildRequest converts an OpenAI chat completion request to Anthropic's /v1/messages payload.
func (t Translator) BuildRequest(req OpenAIChatRequest) (AnthropicRequest, []Warning, error) {
	if req.Model == "" {
		return AnthropicRequest{}, nil, fmt.Errorf("%w: missing model", ErrInvalidRequest)
	}
	if len(req.Messages) == 0 {
		return AnthropicRequest{}, nil, fmt.Errorf("%w: messages must not be empty", ErrInvalidRequest)
	}

	warnings := []Warning{}

	system, rest := extractSystemMessages(req.Messages)
	for _, msg := range req.Messages {
		if msg.Role == "developer" {
			warnings = append(warnings, Warning("developer role has no Messages API equivalent; hoisted into the system prompt"))
			break
		}
	}
	conversation, toolCallsPresent, err := convertConversation(rest)
	if err != nil {
		return AnthropicRequest{}, nil, err
	}
	// Anthropic requires the conversation to open with a user turn; a history
	// whose first remaining message is an assistant one would 400 upstream.
	if len(conversation) > 0 && conversation[0].Role == "assistant" {
		return AnthropicRequest{}, nil, fmt.Errorf("%w: first message must use the \"user\" role", ErrInvalidRequest)
	}

	anthropicTools, mcpServers, err := mapTools(req.Tools)
	if err != nil {
		return AnthropicRequest{}, nil, err
	}

	if len(anthropicTools) == 0 && toolCallsPresent {
		if !t.AllowDummyTool {
			return AnthropicRequest{}, nil, fmt.Errorf("%w: tool_calls present but `tools` not provided", ErrInvalidRequest)
		}
		anthropicTools = append(anthropicTools, defaultDummyTool(t.DummyToolName))
	}

	if req.WebSearchOptions != nil {
		anthropicTools = append(anthropicTools, mapWebSearchOptions(*req.WebSearchOptions))
	}

	if needsCodeExecutionTool(conversation) && !containsToolType(anthropicTools, "code_execution") {
		anthropicTools = append(anthropicTools, codeExecutionTool())
	}

	thinking := mapThinking(req)

	responseFormatTool, jsonMode := responseFormatToTool(req.ResponseFormat)

	if responseFormatTool != nil {
		anthropicTools = append(anthropicTools, *responseFormatTool)
	}

	toolChoice := mapToolChoice(req.ToolChoice, req.ParallelToolCalls)
	if responseFormatTool != nil && thinking == nil {
		toolChoice = &AnthropicToolChoice{
			Type: "tool",
			Name: ResponseFormatToolName,
		}
	}

	stopSequences := sanitizeStopSequences(req.Stop, t.DropInvalidStops)
	if len(stopSequences) > MaxStopSequences {
		warnings = append(warnings, Warning(fmt.Sprintf(
			"stop_sequences truncated from %d to %d, the Messages API maximum",
			len(stopSequences), MaxStopSequences)))
		stopSequences = stopSequences[:MaxStopSequences]
	}

	maxTokens := t.DefaultMaxTokens
	if req.MaxTokens != nil {
		maxTokens = *req.MaxTokens
	} else if req.MaxCompletion != nil {
		maxTokens = *req.MaxCompletion
	}

	// Anthropic rejects requests where thinking.budget_tokens is not strictly
	// below max_tokens ("max_tokens must be greater than thinking.budget_tokens"),
	// so the two are reconciled once both are known. Covers budgets derived
	// from reasoning_effort and explicit thinking config alike.
	if thinking != nil && thinking.Type == "enabled" && thinking.BudgetTokens >= maxTokens {
		if maxTokens <= 1 {
			return AnthropicRequest{}, nil, fmt.Errorf(
				"%w: max_tokens (%d) must be greater than thinking budget_tokens (%d)",
				ErrInvalidRequest, maxTokens, thinking.BudgetTokens)
		}
		warnings = append(warnings, Warning(fmt.Sprintf(
			"thinking budget_tokens capped from %d to %d to fit max_tokens",
			thinking.BudgetTokens, maxTokens-1)))
		thinking.BudgetTokens = maxTokens - 1
	}

	// With extended thinking enabled, Anthropic requires the default sampling
	// values, and the newest models reject temperature outright; dropping both
	// makes the API fall back to its defaults.
	var temperature, topP *float64
	if thinking != nil && thinking.Type == "enabled" {
		if req.Temperature != nil || req.TopP != nil {
			warnings = append(warnings, Warning("temperature/top_p ignored: thinking is enabled"))
		}
	} else {
		temperature = req.Temperature
		topP = req.TopP
	}

	var metadata map[string]any
	if len(req.Metadata) > 0 {
		metadata = map[string]any{}
		for k, v := range req.Metadata {
			metadata[k] = v
		}
	}

	if userID := extractUserID(req); userID != "" {
		if metadata == nil {
			metadata = map[string]any{}
		}
		metadata["user_id"] = userID
	}

	anthropicReq := AnthropicRequest{
		Model:         req.Model,
		Messages:      conversation,
		System:        system,
		Tools:         anthropicTools,
		MCPServers:    mcpServers,
		ToolChoice:    toolChoice,
		StopSequences: stopSequences,
		Temperature:   temperature,
		TopP:          topP,
		MaxTokens:     maxTokens,
		Metadata:      metadata,
		JsonMode:      jsonMode,
		Stream:        req.Stream,
		Thinking:      thinking,
	}

	return anthropicReq, warnings, nil
}

// MarshalRequest renders a built request as the JSON body to POST to
// /v1/messages. JsonMode is a translator-local flag recording that
// response_format forced a JSON tool onto the request; the Messages API has no
// such field, so it is cleared before the payload is rendered rather than
// being sent upstream as an unknown key.
func MarshalRequest(req AnthropicRequest) ([]byte, error) {
	req.JsonMode = false
	req.Headers = nil
	return json.Marshal(req)
}

// ConvertResponse maps Anthropic's JSON response back into an OpenAI-compatible payload.
func (t Translator) ConvertResponse(resp AnthropicResponse, prefixPrompt string, jsonMode bool) (OpenAIChatResponse, error) {
	if resp.Error != nil {
		return OpenAIChatResponse{}, fmt.Errorf("anthropic error: %v", resp.Error)
	}

	text, toolCalls, reasoning := extractResponseArtifacts(resp.Content)
	if prefixPrompt != "" && !strings.HasPrefix(text, prefixPrompt) {
		text = prefixPrompt + text
	}

	message := OpenAIMessage{
		Role:      "assistant",
		Content:   []ContentBlock{NewTextBlock(text)},
		ToolCalls: toolCalls,
	}

	if jsonMode && len(toolCalls) == 1 {
		if converted, ok := convertToolResponseToMessage(toolCalls[0]); ok {
			message = converted
		}
	}
	message.ReasoningContent = reasoning

	choice := OpenAIChoice{
		Index:        0,
		FinishReason: MapFinishReason(resp.StopReason),
		Message:      message,
	}

	usage := convertUsage(resp.Usage)
	return OpenAIChatResponse{
		ID:      chatCompletionID(resp.ID),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   resp.Model,
		Choices: []OpenAIChoice{choice},
		Usage:   usage,
	}, nil
}

// chatCompletionID re-spells an Anthropic message id as a chat-completion id.
// The two id spaces are otherwise distinguishable only by prefix, and some
// SDKs branch on the "chatcmpl-" one.
func chatCompletionID(id string) string {
	if id == "" {
		return "chatcmpl-anthropic"
	}
	if strings.HasPrefix(id, "msg_") {
		return "chatcmpl-" + strings.TrimPrefix(id, "msg_")
	}
	return id
}

// PrefixPrompt matches the helper from the upstream router to detect assistant prefix messages.
func PrefixPrompt(messages []OpenAIMessage) string {
	if len(messages) == 0 {
		return ""
	}
	last := messages[len(messages)-1]
	if last.Role != "assistant" || !last.Prefix {
		return ""
	}
	if len(last.Content) == 0 {
		return ""
	}
	if text, ok := last.Content[0].AsText(); ok {
		return text
	}
	return ""
}

// extractSystemMessages hoists system - and developer, which chat-wire
// clients occasionally send and the Messages API does not know - into the
// system prompt, returning the remaining conversation messages.
func extractSystemMessages(messages []OpenAIMessage) ([]ContentBlock, []OpenAIMessage) {
	systemBlocks := make([]ContentBlock, 0)
	rest := make([]OpenAIMessage, 0, len(messages))
	for _, msg := range messages {
		if msg.Role != "system" && msg.Role != "developer" {
			rest = append(rest, msg)
			continue
		}
		for _, block := range msg.Content {
			systemBlock := block.Clone()
			if msg.CacheControl != nil {
				systemBlock.Data["cache_control"] = msg.CacheControl
			}
			systemBlocks = append(systemBlocks, systemBlock)
		}
	}
	return systemBlocks, rest
}

func convertConversation(messages []OpenAIMessage) ([]AnthropicMessage, bool, error) {
	converted := make([]AnthropicMessage, 0, len(messages))
	toolCallsPresent := false

	for _, msg := range messages {
		switch msg.Role {
		case "user":
			content := cloneContent(msg.Content)
			if len(content) == 0 {
				return nil, false, fmt.Errorf("%w: user message missing content", ErrInvalidRequest)
			}
			converted = append(converted, AnthropicMessage{
				Role:    "user",
				Content: content,
			})
		case "assistant":
			content := cloneContent(msg.Content)
			for _, call := range msg.ToolCalls {
				toolCallsPresent = true
				block, err := toolCallToContent(call)
				if err != nil {
					return nil, false, err
				}
				content = append(content, block)
			}
			if len(content) == 0 && len(msg.ToolCalls) == 0 {
				return nil, false, fmt.Errorf("%w: assistant message missing content", ErrInvalidRequest)
			}
			converted = append(converted, AnthropicMessage{
				Role:    "assistant",
				Content: content,
			})
		case "tool":
			block, err := toolResultBlock(msg)
			if err != nil {
				return nil, false, err
			}
			converted = append(converted, AnthropicMessage{
				Role:    "user",
				Content: []ContentBlock{block},
			})
		default:
			return nil, false, fmt.Errorf("%w: unsupported message role %s", ErrInvalidRequest, msg.Role)
		}
	}

	return mergeAdjacentMessages(converted), toolCallsPresent, nil
}

func mergeAdjacentMessages(messages []AnthropicMessage) []AnthropicMessage {
	if len(messages) < 2 {
		return messages
	}

	merged := make([]AnthropicMessage, 0, len(messages))
	for _, message := range messages {
		if len(merged) > 0 && merged[len(merged)-1].Role == message.Role {
			merged[len(merged)-1].Content = append(merged[len(merged)-1].Content, message.Content...)
			continue
		}
		merged = append(merged, message)
	}
	return merged
}

func cloneContent(blocks []ContentBlock) []ContentBlock {
	cloned := make([]ContentBlock, 0, len(blocks))
	for _, block := range blocks {
		cloned = append(cloned, block.Clone())
	}
	return cloned
}

func toolCallToContent(call ToolCall) (ContentBlock, error) {
	var parsed any
	if call.Function.Arguments != "" {
		if err := json.Unmarshal([]byte(call.Function.Arguments), &parsed); err != nil {
			return ContentBlock{}, fmt.Errorf("%w: tool call %s has invalid JSON: %v", ErrInvalidRequest, call.ID, err)
		}
	} else {
		parsed = map[string]any{}
	}
	payload := map[string]any{
		"type":  "tool_use",
		"id":    call.ID,
		"name":  call.Function.Name,
		"input": parsed,
	}
	return NewContentBlock("tool_use", payload), nil
}

func toolResultBlock(msg OpenAIMessage) (ContentBlock, error) {
	if msg.ToolCallID == "" {
		return ContentBlock{}, fmt.Errorf("%w: tool message missing tool_call_id", ErrInvalidRequest)
	}

	payload := map[string]any{
		"type":        "tool_result",
		"tool_use_id": msg.ToolCallID,
	}

	switch len(msg.Content) {
	case 0:
		payload["content"] = ""
	case 1:
		if text, ok := msg.Content[0].AsText(); ok {
			payload["content"] = text
		} else {
			payload["content"] = []map[string]any{msg.Content[0].Data}
		}
	default:
		blockList := make([]map[string]any, 0, len(msg.Content))
		for _, block := range msg.Content {
			blockList = append(blockList, block.Data)
		}
		payload["content"] = blockList
	}

	return NewContentBlock("tool_result", payload), nil
}

func mapTools(tools []Tool) ([]AnthropicTool, []AnthropicMCPServer, error) {
	converted := []AnthropicTool{}
	mcpServers := []AnthropicMCPServer{}
	for _, tool := range tools {
		anth, mcp, err := convertTool(tool)
		if err != nil {
			return nil, nil, err
		}
		if anth != nil {
			converted = append(converted, *anth)
		}
		if mcp != nil {
			mcpServers = append(mcpServers, *mcp)
		}
	}
	return converted, mcpServers, nil
}

func convertTool(tool Tool) (*AnthropicTool, *AnthropicMCPServer, error) {
	switch {
	case tool.Type == "function" || tool.Type == "custom":
		function, _ := tool.Raw["function"].(map[string]any)
		if function == nil {
			return nil, nil, fmt.Errorf("%w: function tool missing `function` block", ErrInvalidRequest)
		}
		name, _ := function["name"].(string)
		if name == "" {
			return nil, nil, fmt.Errorf("%w: function tool missing name", ErrInvalidRequest)
		}
		inputSchema, _ := function["parameters"].(map[string]any)
		if inputSchema == nil {
			inputSchema = map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			}
		}
		payload := map[string]any{
			"name":         name,
			"input_schema": inputSchema,
		}
		if description, _ := function["description"].(string); description != "" {
			payload["description"] = description
		}
		if cc, ok := tool.Raw["cache_control"]; ok {
			payload["cache_control"] = cc
		} else if fnCC, ok := function["cache_control"]; ok {
			payload["cache_control"] = fnCC
		}
		return &AnthropicTool{Type: "custom", Data: payload}, nil, nil
	case strings.HasPrefix(tool.Type, "computer_"):
		fn, _ := tool.Raw["function"].(map[string]any)
		if fn == nil {
			return nil, nil, fmt.Errorf("%w: computer tool missing function block", ErrInvalidRequest)
		}
		params, _ := fn["parameters"].(map[string]any)
		if params == nil {
			return nil, nil, fmt.Errorf("%w: computer tool missing parameters", ErrInvalidRequest)
		}
		width := toInt(params["display_width_px"])
		height := toInt(params["display_height_px"])
		if width == 0 || height == 0 {
			return nil, nil, fmt.Errorf("%w: computer tool missing display dimensions", ErrInvalidRequest)
		}
		payload := map[string]any{
			"type":              tool.Type,
			"name":              safeString(fn["name"], "computer"),
			"display_width_px":  width,
			"display_height_px": height,
		}
		if number := toInt(params["display_number"]); number > 0 {
			payload["display_number"] = number
		}
		return &AnthropicTool{Type: tool.Type, Data: payload}, nil, nil
	case tool.Type == "url":
		mcp, err := convertURLToolToMCP(tool.Raw)
		return nil, mcp, err
	case tool.Type == "mcp":
		mcp, err := convertMCPTool(tool.Raw)
		return nil, mcp, err
	default:
		// Hosted Anthropic tools are already in Anthropic format; pass through.
		payload := make(map[string]any, len(tool.Raw))
		for k, v := range tool.Raw {
			payload[k] = v
		}
		if _, ok := payload["type"]; !ok {
			payload["type"] = tool.Type
		}
		return &AnthropicTool{Type: tool.Type, Data: payload}, nil, nil
	}
}

func convertURLToolToMCP(raw map[string]any) (*AnthropicMCPServer, error) {
	name, _ := raw["name"].(string)
	url, _ := raw["url"].(string)
	if url == "" || name == "" {
		return nil, fmt.Errorf("%w: url tool missing name/url", ErrInvalidRequest)
	}
	return &AnthropicMCPServer{
		Type: "url",
		URL:  url,
		Name: name,
	}, nil
}

func convertMCPTool(raw map[string]any) (*AnthropicMCPServer, error) {
	config, _ := raw["tool_configuration"].(map[string]any)
	headers, _ := raw["headers"].(map[string]any)
	serverURL, _ := raw["server_url"].(string)
	if serverURL == "" {
		return nil, fmt.Errorf("%w: mcp tool missing server_url", ErrInvalidRequest)
	}
	label, _ := raw["server_label"].(string)
	server := &AnthropicMCPServer{
		Type: "url",
		URL:  serverURL,
		Name: label,
	}
	if config != nil {
		server.ToolConfiguration = config
	}
	if headers != nil {
		if token, _ := headers["Authorization"].(string); strings.HasPrefix(strings.ToLower(token), "bearer ") {
			server.AuthorizationToken = strings.TrimPrefix(token, "Bearer ")
		}
	}
	return server, nil
}

func mapWebSearchOptions(options WebSearchOptions) AnthropicTool {
	payload := map[string]any{
		"type": "web_search_20250305",
		"name": "web_search",
	}
	if options.UserLocation != nil {
		payload["user_location"] = options.UserLocation
	}
	if options.SearchContextSize != "" {
		payload["max_uses"] = searchContextSizeToMaxUses(options.SearchContextSize)
	}
	return AnthropicTool{Type: "web_search_20250305", Data: payload}
}

func searchContextSizeToMaxUses(value string) int {
	switch strings.ToLower(value) {
	case "low":
		return 1
	case "high":
		return 10
	default:
		return 5
	}
}

func needsCodeExecutionTool(messages []AnthropicMessage) bool {
	for _, message := range messages {
		for _, block := range message.Content {
			if block.Type == "container_upload" {
				return true
			}
		}
	}
	return false
}

func codeExecutionTool() AnthropicTool {
	return AnthropicTool{
		Type: "code_execution_20250522",
		Data: map[string]any{
			"type": "code_execution_20250522",
			"name": "code_execution",
		},
	}
}

func containsToolType(tools []AnthropicTool, prefix string) bool {
	for _, tool := range tools {
		if strings.HasPrefix(tool.Type, prefix) {
			return true
		}
	}
	return false
}

func responseFormatToTool(responseFormat map[string]any) (*AnthropicTool, bool) {
	if len(responseFormat) == 0 {
		return nil, false
	}
	if formatType, _ := responseFormat["type"].(string); formatType == "text" {
		return nil, false
	}

	var schema map[string]any
	if respSchema, ok := responseFormat["response_schema"].(map[string]any); ok {
		schema = respSchema
	} else if jsonSchema, ok := responseFormat["json_schema"].(map[string]any); ok {
		if inner, _ := jsonSchema["schema"].(map[string]any); inner != nil {
			schema = inner
		}
	}
	payload := map[string]any{
		"name": ResponseFormatToolName,
		"input_schema": map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}
	if schema != nil {
		payload["input_schema"] = schema
	}
	return &AnthropicTool{
		Type: ResponseFormatToolName,
		Data: payload,
	}, true
}

func mapToolChoice(choice *ToolChoice, parallel *bool) *AnthropicToolChoice {
	var mapped *AnthropicToolChoice
	if choice != nil {
		switch choice.Kind {
		case "auto":
			mapped = &AnthropicToolChoice{Type: "auto"}
		case "required":
			mapped = &AnthropicToolChoice{Type: "any"}
		case "none":
			mapped = &AnthropicToolChoice{Type: "none"}
		case "tool", "function":
			mapped = &AnthropicToolChoice{
				Type: "tool",
				Name: choice.FunctionName,
			}
		}
	}

	if parallel != nil {
		disable := !*parallel
		if mapped == nil {
			mapped = &AnthropicToolChoice{Type: "auto"}
		}
		if mapped.Type != "none" {
			mapped.DisableParallelToolUse = &disable
		}
	}
	return mapped
}

func sanitizeStopSequences(stop *StopSetting, dropInvalid bool) []string {
	if stop == nil {
		return nil
	}
	values := make([]string, 0, len(stop.Values))
	for _, v := range stop.Values {
		if strings.TrimSpace(v) == "" && dropInvalid {
			continue
		}
		values = append(values, v)
	}
	if len(values) == 0 {
		return nil
	}
	return values
}

func extractUserID(req OpenAIChatRequest) string {
	if req.Metadata != nil {
		if userID, _ := req.Metadata["user_id"].(string); validUserID(userID) {
			return userID
		}
	}
	if validUserID(req.User) {
		return req.User
	}
	return ""
}

var (
	emailRegex = regexp.MustCompile(`^[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}$`)
	phoneRegex = regexp.MustCompile(`^\+?[\d\s\(\)\-]{7,}$`)
)

func validUserID(userid string) bool {
	if userid == "" {
		return false
	}
	if emailRegex.MatchString(userid) {
		return false
	}
	if phoneRegex.MatchString(userid) {
		return false
	}
	return true
}

func mapThinking(req OpenAIChatRequest) *AnthropicThinking {
	if req.Thinking != nil {
		return &AnthropicThinking{
			Type:         req.Thinking.Type,
			BudgetTokens: req.Thinking.BudgetTokens,
		}
	}
	if req.ReasoningEffort == "" {
		return nil
	}
	budget := reasoningBudgetLow
	switch strings.ToLower(req.ReasoningEffort) {
	case "minimal":
		budget = reasoningBudgetMinimal
	case "low":
		budget = reasoningBudgetLow
	case "medium":
		budget = reasoningBudgetMedium
	case "high":
		budget = reasoningBudgetHigh
	default:
		return nil
	}
	return &AnthropicThinking{
		Type:         "enabled",
		BudgetTokens: budget,
	}
}

func extractResponseArtifacts(content []ContentBlock) (string, []ToolCall, string) {
	var builder strings.Builder
	toolCalls := []ToolCall{}
	var reasoning strings.Builder

	for _, block := range content {
		switch block.Type {
		case "text":
			if text, ok := block.AsText(); ok {
				builder.WriteString(text)
			}
		case "thinking", "redacted_thinking":
			if text, ok := block.Data["thinking"].(string); ok {
				reasoning.WriteString(text)
			}
		case "tool_use":
			toolCalls = append(toolCalls, convertToolUseBlock(block))
		}
	}

	return builder.String(), toolCalls, reasoning.String()
}

func convertToolUseBlock(block ContentBlock) ToolCall {
	inputJSON, _ := json.Marshal(block.Data["input"])
	return ToolCall{
		ID:   safeString(block.Data["id"], ""),
		Type: "function",
		Function: ToolCallFunction{
			Name:      safeString(block.Data["name"], ""),
			Arguments: string(inputJSON),
		},
	}
}

func convertToolResponseToMessage(call ToolCall) (OpenAIMessage, bool) {
	if call.Function.Name != ResponseFormatToolName {
		return OpenAIMessage{}, false
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return OpenAIMessage{
			Role:    "assistant",
			Content: []ContentBlock{NewTextBlock(call.Function.Arguments)},
		}, true
	}

	if values, ok := args["values"]; ok {
		encoded, _ := json.Marshal(values)
		return OpenAIMessage{
			Role:    "assistant",
			Content: []ContentBlock{NewTextBlock(string(encoded))},
		}, true
	}

	encoded, _ := json.Marshal(args)
	return OpenAIMessage{
		Role:    "assistant",
		Content: []ContentBlock{NewTextBlock(string(encoded))},
	}, true
}

// MapFinishReason translates an Anthropic stop reason into the name the
// OpenAI wire uses. The reference module passes Anthropic's own word through;
// the router's clients check for "stop", "length" and "tool_calls", so the
// dialect has to be translated here like every other field.
func MapFinishReason(reason string) string {
	switch reason {
	case "end_turn", "stop_sequence", "pause_turn", "":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	default:
		return reason
	}
}

func convertUsage(usage AnthropicUsage) Usage {
	prompt := usage.InputTokens + usage.CacheCreationInput + usage.CacheReadInput
	completion := usage.OutputTokens
	total := prompt + completion
	return Usage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      total,
	}
}

func safeString(value any, fallback string) string {
	if str, ok := value.(string); ok && str != "" {
		return str
	}
	return fallback
}

func toInt(value any) int {
	switch v := value.(type) {
	case int:
		return v
	case int32:
		return int(v)
	case int64:
		return int(v)
	case float64:
		return int(v)
	case float32:
		return int(v)
	case json.Number:
		i, _ := v.Int64()
		return int(i)
	default:
		return 0
	}
}

func defaultDummyTool(name string) AnthropicTool {
	if name == "" {
		name = "auto_added_tool"
	}
	return AnthropicTool{
		Type: "tool",
		Data: map[string]any{
			"type": "tool",
			"name": name,
			"input_schema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
}
