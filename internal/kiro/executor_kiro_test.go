package kiro

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/xiaokui-dev/cliproxyapi-kiro-plugin/internal/hostapi"
)

func TestGenerationUsesSupportedKiroClientVersion(t *testing.T) {
	old := kiroHTTPDo
	t.Cleanup(func() { kiroHTTPDo = old })
	calls := 0
	kiroHTTPDo = func(req hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		calls++
		for _, name := range []string{"user-agent", "x-amz-user-agent"} {
			if len(req.Headers[name]) != 1 || !strings.Contains(req.Headers[name][0], "KiroIDE-1.4.121-") {
				return &hostapi.HTTPResponse{StatusCode: 403, Body: []byte(`{"reason":"FEATURE_NOT_SUPPORTED","message":"This version of Kiro is no longer supported. Please update to the latest version of Kiro to continue."}`)}, nil
			}
		}
		return &hostapi.HTTPResponse{StatusCode: 200, Body: encodeFrame("assistantResponseEvent", `{"content":"SUPPORTED_CLIENT_OK"}`)}, nil
	}
	raw, _ := json.Marshal(executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		Model: "claude-opus-5.5", Payload: []byte(`{"messages":[{"role":"user","content":"synthetic test"}]}`), StorageJSON: []byte(`{"accessToken":"test"}`),
	}})
	result, envelope, err := fetchKiroEvents(raw)
	if err != nil || envelope != nil || result == nil || result.text != "SUPPORTED_CLIENT_OK" || calls != 1 {
		t.Fatalf("client admission failed: calls=%d err=%v envelope=%s", calls, err, envelope)
	}
}

func TestGenerationDiscoversOwnIdCProfile(t *testing.T) {
	old := kiroHTTPDo
	t.Cleanup(func() { kiroHTTPDo = old })
	discovery, generation := 0, 0
	kiroHTTPDo = func(req hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		if req.Headers["Authorization"][0] != "Bearer own-token" {
			t.Fatal("profile discovery changed account")
		}
		if strings.Contains(req.URL, "management.") {
			discovery++
			return &hostapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"profiles":[{"arn":"arn:own-profile"}]}`)}, nil
		}
		generation++
		var body cwRequest
		_ = json.Unmarshal(req.Body, &body)
		if body.ProfileArn != "arn:own-profile" {
			return &hostapi.HTTPResponse{StatusCode: 400, Body: []byte(`{"message":"profileArn is required for this request."}`)}, nil
		}
		return &hostapi.HTTPResponse{StatusCode: 200, Body: encodeFrame("assistantResponseEvent", `{"content":"OWN_PROFILE_OK"}`)}, nil
	}
	raw, _ := json.Marshal(executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		Model: "claude-opus-5.5", Payload: []byte(`{"messages":[{"role":"user","content":"synthetic test"}]}`), StorageJSON: []byte(`{"accessToken":"own-token","authMethod":"IdC","region":"us-east-1"}`),
	}})
	result, envelope, err := fetchKiroEvents(raw)
	if err != nil || envelope != nil || result == nil || result.text != "OWN_PROFILE_OK" || discovery != 1 || generation != 1 {
		t.Fatalf("profile routing failed: discovery=%d generation=%d err=%v envelope=%s", discovery, generation, err, envelope)
	}
}

func TestIdCProfileLookupFailureStopsBeforeInference(t *testing.T) {
	old := kiroHTTPDo
	t.Cleanup(func() { kiroHTTPDo = old })
	kiroHTTPDo = func(req hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		if !strings.Contains(req.URL, "management.") {
			t.Fatal("inference started without a profile")
		}
		return &hostapi.HTTPResponse{StatusCode: 403}, nil
	}
	cred := kiroCredential{AuthMethod: "IdC", AccessToken: "own-token"}
	if _, err := credentialWithProfile(cred, "cb", "us-east-1"); err == nil {
		t.Fatal("missing profile silently accepted")
	}
	cred.ProfileArn = "arn:already-known"
	if got, err := credentialWithProfile(cred, "cb", "us-east-1"); err != nil || got.ProfileArn != cred.ProfileArn {
		t.Fatal("stored profile was not retained")
	}
}

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
	for _, execute := range []func([]byte) ([]byte, error){executeKiro} {
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
