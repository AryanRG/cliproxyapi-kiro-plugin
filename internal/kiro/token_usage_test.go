package kiro

import (
	"encoding/json"
	"testing"
)

func TestToolOnlyUsageIsNotZeroAndMatchesStreaming(t *testing.T) {
	calls := []toolCall{{id: "tool-1", name: "write_file", input: json.RawMessage(`{"path":"example.txt","content":"hello world"}`)}}
	message := aggregateToClaudeMessage("", calls, "claude-opus-5.5", []byte(`{"messages":[]}`))
	if message.Usage.OutputTokens <= 0 {
		t.Fatal("tool output was omitted from token estimate")
	}
	for _, chunk := range buildClaudeStreamChunks("", calls, message.Model, message.Usage.InputTokens) {
		if sseEventType(chunk.Payload) != "message_delta" {
			continue
		}
		var delta struct {
			Usage claudeUsage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(sseData(chunk.Payload)), &delta); err != nil {
			t.Fatal(err)
		}
		if delta.Usage.OutputTokens != message.Usage.OutputTokens {
			t.Fatalf("stream and nonstream estimates differ: %d vs %d", delta.Usage.OutputTokens, message.Usage.OutputTokens)
		}
		return
	}
	t.Fatal("missing usage delta")
}
