package kiro

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestCountClaudeInputTokensText(t *testing.T) {
	// "hello world" is 2 o200k tokens; role "user" is 1; +3 per-message overhead.
	got, err := countClaudeInputTokens([]byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hello world"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got < 5 || got > 8 {
		t.Fatalf("count = %d", got)
	}
	empty, _ := countClaudeInputTokens(nil)
	if empty != 0 {
		t.Fatalf("empty = %d", empty)
	}
	if _, err := countClaudeInputTokens([]byte(`{`)); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestCountClaudeInputTokensGrowsWithSystemToolsAndBlocks(t *testing.T) {
	base := `{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`
	full := `{"system":[{"type":"text","text":"You are a careful assistant who writes Go."}],
	 "tools":[{"name":"read_file","description":"Read a file from disk","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}],
	 "messages":[{"role":"user","content":[{"type":"text","text":"hi"}]},
	  {"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"read_file","input":{"path":"main.go"}}]},
	  {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"package main"}]}]}]}`
	a, err := countClaudeInputTokens([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	b, err := countClaudeInputTokens([]byte(full))
	if err != nil {
		t.Fatal(err)
	}
	if b <= a+30 {
		t.Fatalf("expected system/tools/tool blocks to add tokens: base=%d full=%d", a, b)
	}
}

func TestImageTokens(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 750, 100))); err != nil {
		t.Fatal(err)
	}
	src := &claudeImageSource{Type: "base64", MediaType: "image/png", Data: base64.StdEncoding.EncodeToString(buf.Bytes())}
	if got := imageTokens(src); got != 100 {
		t.Fatalf("750x100 image = %d, want 100", got)
	}
	buf.Reset()
	png.Encode(&buf, image.NewGray(image.Rect(0, 0, 4000, 4000)))
	src.Data = base64.StdEncoding.EncodeToString(buf.Bytes())
	if got := imageTokens(src); got < 1400 || got > 1600 {
		t.Fatalf("large image not capped: %d", got)
	}
	if got := imageTokens(&claudeImageSource{Type: "url"}); got != imageTokenFallback {
		t.Fatalf("fallback = %d", got)
	}
}

func TestHandleMethodCountTokens(t *testing.T) {
	payload := []byte(`{"model":"claude-opus-5.5","messages":[{"role":"user","content":"count me please"}]}`)
	request, _ := json.Marshal(executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{Model: "claude-opus-5.5", Payload: payload}})
	out, err := HandleMethod(pluginabi.MethodExecutorCountTokens, request)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		OK     bool                       `json:"ok"`
		Result pluginapi.ExecutorResponse `json:"result"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("decode envelope %s: %v", out, err)
	}
	var body struct {
		InputTokens int64 `json:"input_tokens"`
	}
	if err := json.Unmarshal(env.Result.Payload, &body); err != nil || body.InputTokens <= 0 {
		t.Fatalf("bad count payload %s (%v) from %s", env.Result.Payload, err, out)
	}
	if env.Result.Headers.Get("X-Kiro-Token-Usage") != "estimated" {
		t.Fatalf("missing estimate header: %+v", env.Result.Headers)
	}
	out, _ = HandleMethod(pluginabi.MethodExecutorHTTPRequest, []byte(`{}`))
	if !bytes.Contains(out, []byte("not_implemented")) {
		t.Fatalf("http-request should stay not_implemented: %s", out)
	}
}
