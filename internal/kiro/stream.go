package kiro

import (
	"encoding/json"
	"strings"
)

// executorStreamResponse mirrors the host's rpcExecutorStreamResponse. When
// Chunks is non-empty the host forwards them in order (see rpc_client_stream.go),
// so a plugin can emit a full SSE sequence synchronously without the async
// host.stream.emit path.
type executorStreamResponse struct {
	Headers map[string][]string   `json:"headers,omitempty"`
	Chunks  []executorStreamChunk `json:"chunks,omitempty"`
}

// executorStreamChunk mirrors pluginapi.ExecutorStreamChunk (PascalCase JSON).
type executorStreamChunk struct {
	Payload []byte `json:"Payload,omitempty"`
}

// buildClaudeStreamChunks renders aggregated text + tool calls as a standard
// Claude Messages SSE sequence, one SSE event per chunk:
// message_start → (content_block_start/delta/stop)* → message_delta → message_stop.
func buildClaudeStreamChunks(text string, calls []toolCall, model string, inputTokens int) []executorStreamChunk {
	chunks := make([]executorStreamChunk, 0, 8)
	add := func(event string, data any) {
		body, _ := json.Marshal(data)
		var sb strings.Builder
		sb.WriteString("event: ")
		sb.WriteString(event)
		sb.WriteString("\ndata: ")
		sb.Write(body)
		sb.WriteString("\n\n")
		chunks = append(chunks, executorStreamChunk{Payload: []byte(sb.String())})
	}

	msgID := randomMessageID()
	add("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": msgID, "type": "message", "role": "assistant", "model": model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": inputTokens, "output_tokens": 0},
		},
	})

	index := 0
	if strings.TrimSpace(text) != "" {
		add("content_block_start", map[string]any{
			"type": "content_block_start", "index": index,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
		add("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": index,
			"delta": map[string]any{"type": "text_delta", "text": text},
		})
		add("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
		index++
	}

	for _, tc := range calls {
		add("content_block_start", map[string]any{
			"type": "content_block_start", "index": index,
			"content_block": map[string]any{"type": "tool_use", "id": tc.id, "name": tc.name, "input": map[string]any{}},
		})
		add("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": index,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": string(tc.input)},
		})
		add("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
		index++
	}

	stopReason := "end_turn"
	if len(calls) > 0 {
		stopReason = "tool_use"
	}
	add("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": estimateOutputTokens(text, calls)},
	})
	add("message_stop", map[string]any{"type": "message_stop"})
	return chunks
}
