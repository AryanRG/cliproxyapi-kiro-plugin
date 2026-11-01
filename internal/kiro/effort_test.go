package kiro

import (
	"encoding/json"
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/xiaokui-dev/cliproxyapi-kiro-plugin/internal/hostapi"
	"testing"
)

func TestEffortReachesKiro(t *testing.T) {
	old := kiroHTTPDo
	defer func() { kiroHTTPDo = old }()
	for _, model := range []string{"claude-opus-5.5", "claude-sonnet-5.5", "claude-fable-5.1", "claude-haiku-5.5"} {
		for _, effort := range []string{"low", "medium", "high", "xhigh", "max"} {
			t.Run(model+"/"+effort, func(t *testing.T) {
				calls := 0
				kiroHTTPDo = func(req hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
					calls++
					var body map[string]json.RawMessage
					json.Unmarshal(req.Body, &body)
					var fields struct {
						Thinking struct {
							Type string `json:"type"`
						}
						Output struct {
							Effort string `json:"effort"`
						} `json:"output_config"`
						Max int `json:"max_tokens"`
					}
					json.Unmarshal(body["additionalModelRequestFields"], &fields)
					if fields.Output.Effort != effort || fields.Thinking.Type != "adaptive" || fields.Max != 2048 {
						t.Errorf("controls lost: %s", body["additionalModelRequestFields"])
					}
					return &hostapi.HTTPResponse{StatusCode: 200, Body: encodeFrame("assistantResponseEvent", `{"content":"ok"}`)}, nil
				}
				payload := json.RawMessage(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello"}],"output_config":{"effort":%q},"max_tokens":2048}`, model, effort))
				raw, _ := json.Marshal(executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{Model: model, Payload: payload, StorageJSON: json.RawMessage(`{"accessToken":"test"}`)}})
				_, env, err := fetchKiroEvents(raw)
				if err != nil || env != nil || calls != 1 {
					t.Fatalf("err=%v env=%s calls=%d", err, env, calls)
				}
			})
		}
	}
}

func TestInvalidEffortBlockedBeforeHTTP(t *testing.T) {
	old := kiroHTTPDo
	defer func() { kiroHTTPDo = old }()
	kiroHTTPDo = func(req hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		t.Fatal("invalid control contacted upstream")
		return nil, nil
	}
	for _, payload := range []string{
		`{"model":"claude-opus-5.5","output_config":{"effort":"ultracode"}}`,
		`{"model":"unknown","output_config":{"effort":"high"}}`,
		`{"model":"claude-opus-5.5","thinking":{"type":"disabled"}}`,
		`{"model":"claude-opus-5.5","max_tokens":100000}`,
		`{"model":"claude-opus-5.5","max_tokens":100}`,
		`{"model":"claude-opus-5.5","thinking":{"type":"adaptive","display":"raw"}}`,
	} {
		var p struct{ Model string }
		json.Unmarshal([]byte(payload), &p)
		raw, _ := json.Marshal(executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{Model: p.Model, Payload: []byte(payload), StorageJSON: []byte(`{"accessToken":"test"}`)}})
		_, env, err := fetchKiroEvents(raw)
		if err != nil || env == nil {
			t.Fatalf("err=%v env=%s", err, env)
		}
		var e struct {
			Error struct {
				Code       string
				HTTPStatus int `json:"http_status"`
			}
		}
		json.Unmarshal(env, &e)
		if e.Error.Code != "invalid_model_controls" || e.Error.HTTPStatus != 400 {
			t.Fatalf("%s", env)
		}
	}
}
func TestLegacyAndOptionalControls(t *testing.T) {
	var req claudeRequest
	json.Unmarshal([]byte(`{"thinking":{"type":"enabled","budget_tokens":8192},"output_config":{"effort":"xhigh"}}`), &req)
	fields, err := kiroRequestControls(req, "claude-opus-5.5")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(fields)
	if string(b) != `{"output_config":{"effort":"xhigh"},"thinking":{"type":"adaptive"}}` {
		t.Fatalf("%s", b)
	}
	fields, err = kiroRequestControls(claudeRequest{}, "claude-opus-5.5")
	if err != nil || fields != nil {
		t.Fatalf("default changed: %v %v", fields, err)
	}
	if modelThinkingSupport("claude-opus-5-5") != nil {
		t.Fatal("cross-provider hyphen alias advertised")
	}
	for _, model := range []string{"claude-opus-5.5", "claude-sonnet-5.5", "claude-fable-5.1", "claude-haiku-5.5"} {
		m := availableModelsToPluginModels([]availableModel{{ModelID: model}})
		if m[0].ID != model || m[0].Thinking == nil || len(m[0].Thinking.Levels) != 5 {
			t.Fatalf("catalog: %+v", m)
		}
	}
}
