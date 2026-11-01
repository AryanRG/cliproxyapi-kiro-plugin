package kiro

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/xiaokui-dev/cliproxyapi-kiro-plugin/internal/hostapi"
)

func TestFetchKiroEventsRetriesEmptyResponse(t *testing.T) {
	payload, err := json.Marshal(claudeRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []claudeMessage{{Role: "user", Content: json.RawMessage(`"hello"`)}},
	})
	if err != nil {
		t.Fatalf("marshal Claude request: %v", err)
	}
	storage, err := json.Marshal(kiroCredential{AccessToken: "test-access-token"})
	if err != nil {
		t.Fatalf("marshal credential: %v", err)
	}
	rawRequest, err := json.Marshal(executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		Model:       "claude-sonnet-4-5",
		Payload:     payload,
		StorageJSON: storage,
	}})
	if err != nil {
		t.Fatalf("marshal executor request: %v", err)
	}

	oldHTTPDo := kiroHTTPDo
	t.Cleanup(func() { kiroHTTPDo = oldHTTPDo })
	calls := 0
	kiroHTTPDo = func(req hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		calls++
		switch calls {
		case 1, 2:
			return &hostapi.HTTPResponse{StatusCode: 200}, nil
		case 3:
			body := encodeFrame("assistantResponseEvent", `{"content":"recovered"}`)
			return &hostapi.HTTPResponse{StatusCode: 200, Body: body}, nil
		default:
			t.Fatalf("unexpected extra HTTP call: %d", calls)
			return nil, nil
		}
	}

	result, errEnvelope, errFetch := fetchKiroEvents(rawRequest)
	if errFetch != nil {
		t.Fatalf("fetch failed: %v", errFetch)
	}
	if errEnvelope != nil {
		t.Fatalf("unexpected error envelope: %s", errEnvelope)
	}
	if calls != 3 {
		t.Fatalf("expected two retries and one successful request, got %d calls", calls)
	}
	if result == nil || result.text != "recovered" || len(result.calls) != 0 {
		t.Fatalf("unexpected recovered result: %+v", result)
	}
}

func TestFetchKiroEventsStopsAfterEmptyResponseLimit(t *testing.T) {
	payload, err := json.Marshal(claudeRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []claudeMessage{{Role: "user", Content: json.RawMessage(`"hello"`)}},
	})
	if err != nil {
		t.Fatalf("marshal Claude request: %v", err)
	}
	storage, err := json.Marshal(kiroCredential{AccessToken: "test-access-token"})
	if err != nil {
		t.Fatalf("marshal credential: %v", err)
	}
	rawRequest, err := json.Marshal(executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		Model:       "claude-sonnet-4-5",
		Payload:     payload,
		StorageJSON: storage,
	}})
	if err != nil {
		t.Fatalf("marshal executor request: %v", err)
	}

	oldHTTPDo := kiroHTTPDo
	t.Cleanup(func() { kiroHTTPDo = oldHTTPDo })
	calls := 0
	kiroHTTPDo = func(req hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		calls++
		return &hostapi.HTTPResponse{StatusCode: 200}, nil
	}

	result, errEnvelope, errFetch := fetchKiroEvents(rawRequest)
	if errFetch != nil {
		t.Fatalf("fetch failed: %v", errFetch)
	}
	if errEnvelope != nil {
		t.Fatalf("unexpected error envelope: %s", errEnvelope)
	}
	if calls != maxEmptyResponseAttempts {
		t.Fatalf("expected %d attempts, got %d", maxEmptyResponseAttempts, calls)
	}
	if result == nil || result.text != "" || len(result.calls) != 0 {
		t.Fatalf("expected final empty result, got %+v", result)
	}
}

func TestExecutorLabelsTokenEstimates(t *testing.T) {
	oldHTTPDo := kiroHTTPDo
	t.Cleanup(func() { kiroHTTPDo = oldHTTPDo })
	kiroHTTPDo = func(req hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		return &hostapi.HTTPResponse{StatusCode: 200, Body: encodeFrame("assistantResponseEvent", `{"content":"hello"}`)}, nil
	}
	raw, _ := json.Marshal(executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		Model: "claude-opus-5.5", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`), StorageJSON: []byte(`{"accessToken":"test"}`),
	}})
	for _, execute := range []func([]byte) ([]byte, error){executeKiro, executeKiroStream} {
		output, err := execute(raw)
		if err != nil {
			t.Fatal(err)
		}
		var envelope struct {
			OK     bool `json:"ok"`
			Result struct {
				Headers map[string][]string `json:"headers"`
			} `json:"result"`
		}
		if err := json.Unmarshal(output, &envelope); err != nil {
			t.Fatal(err)
		}
		values := envelope.Result.Headers["X-Kiro-Token-Usage"]
		if !envelope.OK || len(values) != 1 || values[0] != "estimated" {
			t.Fatalf("missing estimate provenance: %s", output)
		}
	}
}
