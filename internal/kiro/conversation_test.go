package kiro

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestConversationScopeAndWorkerRoot(t *testing.T) {
	req := executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{AuthID: "account-A", Headers: http.Header{"X-Session-Id": {"thread-1"}}, Metadata: map[string]any{"caller_scope": "caller-A"}}}
	parent := conversationFor(req, claudeRequest{})
	if parent != conversationFor(req, claudeRequest{}) || parent.id != parent.root {
		t.Fatal("root identity not stable")
	}
	client, _ := json.Marshal(map[string]string{"session_id": "thread-1", "agent_id": "worker"})
	meta, _ := json.Marshal(map[string]string{"user_id": string(client)})
	worker := conversationFor(req, claudeRequest{Metadata: meta})
	if worker.id == parent.id || worker.root != parent.root {
		t.Fatal("shared worker and parent conversation or wrong root")
	}
	req.Headers.Set("X-Session-Id", "thread-2")
	if conversationFor(req, claudeRequest{}).id == parent.id {
		t.Fatal("chats share an ID")
	}
	req.Headers.Set("X-Session-Id", "thread-1")
	req.Metadata["caller_scope"] = "caller-B"
	if conversationFor(req, claudeRequest{}).id == parent.id {
		t.Fatal("callers share an ID")
	}
	req.Metadata["caller_scope"] = "caller-A"
	req.AuthID = "account-B"
	if conversationFor(req, claudeRequest{}).id == parent.id {
		t.Fatal("accounts share an ID")
	}
	req.Headers = nil
	req.Metadata = map[string]any{"derived_session_id": "same-prompt", "canonical_session_id": "derived-canonical"}
	if conversationFor(req, claudeRequest{}) == conversationFor(req, claudeRequest{}) {
		t.Fatal("unidentified chats were grouped by prompt")
	}
}
