package kiro

import (
	"encoding/json"
	"strings"
)

// toolAccumulator collects the streamed name/input fragments for one tool call.
type toolAccumulator struct {
	name  string
	input strings.Builder
}

// toolCall is a fully-aggregated tool invocation with its original name.
type toolCall struct {
	id    string
	name  string
	input json.RawMessage
}

// claudeUsage is the token accounting block of a Claude message.
type claudeUsage struct {
	InputTokens              int  `json:"input_tokens"`
	OutputTokens             int  `json:"output_tokens"`
	CacheReadInputTokens     *int `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens,omitempty"`
}

// applyUsage replaces the byte estimates with provider or context-percentage
// counts when Kiro reported them, and returns the input-token source.
func (m *claudeResponse) applyUsage(u *kiroUsage) string {
	in, source := u.inputTokens(m.Model, m.Usage.InputTokens)
	m.Usage.InputTokens = in
	m.Usage.OutputTokens = u.outputTokens(m.Usage.OutputTokens)
	if u.haveTokens {
		read, write := u.cacheRead, u.cacheWrite
		m.Usage.CacheReadInputTokens, m.Usage.CacheCreationInputTokens = &read, &write
	}
	return source
}

// claudeResponse is a non-streaming Claude Messages response.
type claudeResponse struct {
	ID           string            `json:"id"`
	Type         string            `json:"type"`
	Role         string            `json:"role"`
	Model        string            `json:"model"`
	Content      []json.RawMessage `json:"content"`
	StopReason   string            `json:"stop_reason"`
	StopSequence *string           `json:"stop_sequence"`
	Usage        claudeUsage       `json:"usage"`
}

// aggregateEvents folds decoded stream events into final assistant text and
// tool calls. Text deltas are concatenated; tool-use fragments are accumulated
// per toolUseId, the placeholder tool is dropped, and names are restored.
func aggregateEvents(events []cwEvent, maps *toolNameMaps) (string, []toolCall) {
	var text strings.Builder
	accums := map[string]*toolAccumulator{}
	order := make([]string, 0)
	lastToolID := ""

	for _, ev := range events {
		switch ev.Kind {
		case cwEventContent:
			text.WriteString(ev.Text)
		case cwEventReasoning:
			// Reasoning/thinking output is not forwarded yet (planned for a later milestone).
		case cwEventToolUse:
			id := ev.ToolUseID
			acc, ok := accums[id]
			if !ok {
				acc = &toolAccumulator{name: ev.Name}
				accums[id] = acc
				order = append(order, id)
			} else if acc.name == "" {
				acc.name = ev.Name
			}
			acc.input.WriteString(ev.Text)
			lastToolID = id
		case cwEventToolUseInput:
			id := ev.ToolUseID
			if id == "" {
				id = lastToolID
			}
			acc, ok := accums[id]
			if !ok {
				acc = &toolAccumulator{}
				accums[id] = acc
				order = append(order, id)
			}
			acc.input.WriteString(ev.Text)
			lastToolID = id
		}
	}

	calls := make([]toolCall, 0, len(order))
	for _, id := range order {
		acc := accums[id]
		if acc == nil || acc.name == kiroPlaceholderToolName {
			continue
		}
		calls = append(calls, toolCall{
			id:    id,
			name:  maps.fromKiro(acc.name),
			input: parseToolInput(acc.input.String()),
		})
	}

	// Some responses emit tool calls as inline text ("[Called x with args: {...}]")
	// instead of structured toolUse events; extract those and strip them from text.
	finalText, bracketCalls := extractBracketToolCalls(text.String(), maps)
	calls = append(calls, bracketCalls...)
	return finalText, dedupToolCalls(calls)
}

// aggregateToClaudeMessage builds a non-streaming Claude message from the
// aggregated assistant text and tool calls.
func aggregateToClaudeMessage(text string, calls []toolCall, model string, requestPayload []byte) claudeResponse {
	content := make([]json.RawMessage, 0, len(calls)+1)
	if strings.TrimSpace(text) != "" {
		block, _ := json.Marshal(map[string]any{"type": "text", "text": text})
		content = append(content, block)
	}
	for _, tc := range calls {
		block, _ := json.Marshal(map[string]any{
			"type": "tool_use", "id": tc.id, "name": tc.name, "input": tc.input,
		})
		content = append(content, block)
	}

	stopReason := "end_turn"
	if len(calls) > 0 {
		stopReason = "tool_use"
	}

	return claudeResponse{
		ID:         randomMessageID(),
		Type:       "message",
		Role:       "assistant",
		Model:      model,
		Content:    content,
		StopReason: stopReason,
		Usage: claudeUsage{
			InputTokens:  estimateTokens(len(requestPayload)),
			OutputTokens: estimateOutputTokens(text, calls),
		},
	}
}

// Kiro supplies no exact token counters; include tool output in the compatibility estimate.
func estimateOutputTokens(text string, calls []toolCall) int {
	byteLen := len(text)
	for _, call := range calls {
		byteLen += len(call.name) + len(call.input)
	}
	return estimateTokens(byteLen)
}

// parseToolInput turns accumulated input text into a JSON value; invalid or
// empty input becomes an empty object so the tool_use block stays valid.
func parseToolInput(s string) json.RawMessage {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return json.RawMessage(`{}`)
	}
	if json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed)
	}
	return json.RawMessage(`{}`)
}

// estimateTokens is a rough byte-based token estimate (~4 bytes/token).
func estimateTokens(byteLen int) int {
	if byteLen <= 0 {
		return 0
	}
	return (byteLen + 3) / 4
}
