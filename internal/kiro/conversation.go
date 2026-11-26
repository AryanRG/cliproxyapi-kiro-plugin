package kiro

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Claude Code sends a JSON-encoded identity in metadata.user_id; older versions
// use a user_..._session_<uuid> string. Other callers may supply session headers.
var legacySession = regexp.MustCompile("(?i)_session_([a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12})$")

type conversationIdentity struct{ id, root string }

func conversationFor(req executorRequest, payload claudeRequest) conversationIdentity {
	var meta struct {
		UserID  string `json:"user_id"`
		AgentID string `json:"agent_id"`
		Parent  string `json:"parent_session_id"`
	}
	_ = json.Unmarshal(payload.Metadata, &meta)
	var client struct {
		Session  string `json:"session_id"`
		Agent    string `json:"agent_id"`
		Subagent string `json:"subagent_id"`
		Parent   string `json:"parent_session_id"`
		Root     string `json:"root_session_id"`
	}
	if strings.HasPrefix(strings.TrimSpace(meta.UserID), "{") {
		_ = json.Unmarshal([]byte(meta.UserID), &client)
	} else if match := legacySession.FindStringSubmatch(meta.UserID); match != nil {
		client.Session = match[1]
	}
	host := func(key string) string { s, _ := req.Metadata[key].(string); return strings.TrimSpace(s) }
	session := firstNonEmptyStr(client.Session, req.Headers.Get("X-Claude-Code-Session-Id"), req.Headers.Get("X-Session-Id"), req.Headers.Get("Session_id"), req.Headers.Get("Session-Id"), host("execution_session_id"))
	// A prompt-derived affinity hash can be identical for unrelated chats.
	if session == "" && host("derived_session_id") == "" {
		session = host("canonical_session_id")
	}
	if session == "" || req.AuthID == "" {
		id := uuidV4()
		return conversationIdentity{id, id}
	}
	scope := host("caller_scope")
	if scope == "" {
		scope = firstNonEmptyStr(req.Headers.Get("X-Api-Key"), req.Headers.Get("Authorization"))
	}
	idFor := func(s, agent string) string {
		data, _ := json.Marshal([]string{"kiro-conversation-v1", req.AuthID, scope, s, agent})
		hash := sha256.Sum256(data)
		hash[6] = hash[6]&0x0f | 0x80
		hash[8] = hash[8]&0x3f | 0x80
		return fmt.Sprintf("%x-%x-%x-%x-%x", hash[:4], hash[4:6], hash[6:8], hash[8:10], hash[10:16])
	}
	agent := firstNonEmptyStr(client.Agent, client.Subagent, meta.AgentID, host("agent_id"))
	root := firstNonEmptyStr(client.Root, client.Parent, meta.Parent, host("parent_session_id"), session)
	return conversationIdentity{idFor(session, agent), idFor(root, "")}
}
