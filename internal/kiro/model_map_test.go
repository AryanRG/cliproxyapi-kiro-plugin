package kiro

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/xiaokui-dev/cliproxyapi-kiro-plugin/internal/hostapi"
	"github.com/xiaokui-dev/cliproxyapi-kiro-plugin/internal/wire"
)

func TestKiroModelsForAuthDiscoversAvailableModels(t *testing.T) {
	storage, errMarshal := json.Marshal(kiroCredential{
		AccessToken: "test-access-token",
		Region:      "eu-west-1",
		ProfileArn:  "arn:aws:codewhisperer:eu-west-1:123:profile/test",
	})
	if errMarshal != nil {
		t.Fatalf("marshal credential: %v", errMarshal)
	}
	rawRequest, errMarshal := json.Marshal(authModelRequest{
		AuthModelRequest: pluginapi.AuthModelRequest{StorageJSON: storage},
		HostCallbackID:   "callback-123",
	})
	if errMarshal != nil {
		t.Fatalf("marshal auth model request: %v", errMarshal)
	}

	var gotRequest hostapi.HTTPRequest
	oldHTTPDo := kiroHTTPDo
	t.Cleanup(func() { kiroHTTPDo = oldHTTPDo })
	kiroHTTPDo = func(req hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		gotRequest = req
		return &hostapi.HTTPResponse{
			StatusCode: 200,
			Body: []byte(`{
				"defaultModel":{"modelId":"auto"},
				"models":[
					{"modelId":"claude-sonnet-4.6","modelName":"Claude Sonnet 4.6","description":"1M context","supportedInputTypes":["TEXT","IMAGE","text"],"tokenLimits":{"maxInputTokens":1000000,"maxOutputTokens":64000}},
					{"modelId":"auto","modelName":"Auto","supportedInputTypes":["TEXT","IMAGE"],"tokenLimits":{"maxInputTokens":1000000,"maxOutputTokens":64000}},
					{"modelId":"qwen3-coder-next","modelName":"Qwen3 Coder Next","supportedInputTypes":["TEXT","IMAGE","IMAGE"]},
					{"modelId":""}
				]
			}`),
		}, nil
	}

	rawResponse, errHandle := kiroModelsForAuth(rawRequest)
	if errHandle != nil {
		t.Fatalf("model discovery failed: %v", errHandle)
	}
	if gotRequest.HostCallbackID != "callback-123" {
		t.Fatalf("expected callback ID callback-123, got %q", gotRequest.HostCallbackID)
	}
	if gotRequest.Method != "POST" {
		t.Fatalf("expected POST, got %q", gotRequest.Method)
	}
	if gotRequest.URL != "https://management.eu-west-1.kiro.dev/" {
		t.Fatalf("unexpected URL: %q", gotRequest.URL)
	}
	if got := gotRequest.Headers["Authorization"]; len(got) != 1 || got[0] != "Bearer test-access-token" {
		t.Fatalf("unexpected authorization header: %#v", got)
	}
	if got := gotRequest.Headers["Content-Type"]; len(got) != 1 || got[0] != "application/x-amz-json-1.0" {
		t.Fatalf("unexpected content type: %#v", got)
	}
	if got := gotRequest.Headers["x-amz-target"]; len(got) != 1 || got[0] != listAvailableModelsTarget {
		t.Fatalf("unexpected target header: %#v", got)
	}
	if got := gotRequest.Headers["TokenType"]; len(got) != 1 || got[0] != "SSO_OIDC" {
		t.Fatalf("unexpected token type header: %#v", got)
	}

	var body listAvailableModelsRequest
	if errUnmarshal := json.Unmarshal(gotRequest.Body, &body); errUnmarshal != nil {
		t.Fatalf("decode upstream request: %v", errUnmarshal)
	}
	if body.Origin != originAIEditor || body.ProfileArn != "arn:aws:codewhisperer:eu-west-1:123:profile/test" {
		t.Fatalf("unexpected upstream request body: %+v", body)
	}

	var envelope wire.Envelope
	if errUnmarshal := json.Unmarshal(rawResponse, &envelope); errUnmarshal != nil {
		t.Fatalf("decode plugin envelope: %v", errUnmarshal)
	}
	if !envelope.OK {
		t.Fatalf("unexpected error envelope: %s", rawResponse)
	}
	var result pluginapi.ModelResponse
	if errUnmarshal := json.Unmarshal(envelope.Result, &result); errUnmarshal != nil {
		t.Fatalf("decode model response: %v", errUnmarshal)
	}
	if result.Provider != providerKiro || len(result.Models) != 3 {
		t.Fatalf("unexpected model response: provider=%q models=%d", result.Provider, len(result.Models))
	}

	sonnet := result.Models[0]
	if sonnet.ID != "claude-sonnet-4-6" || sonnet.Name != "claude-sonnet-4.6" {
		t.Fatalf("unexpected Claude IDs: ID=%q Name=%q", sonnet.ID, sonnet.Name)
	}
	if sonnet.DisplayName != "Claude Sonnet 4.6" || sonnet.Description != "1M context" {
		t.Fatalf("unexpected Claude labels: %+v", sonnet)
	}
	if sonnet.InputTokenLimit != 1000000 || sonnet.OutputTokenLimit != 64000 ||
		sonnet.ContextLength != 1000000 || sonnet.MaxCompletionTokens != 64000 {
		t.Fatalf("unexpected Claude limits: %+v", sonnet)
	}
	if got, want := strings.Join(sonnet.SupportedInputModalities, ","), "text,image"; got != want {
		t.Fatalf("unexpected Claude modalities: %q", got)
	}

	if result.Models[1].ID != "auto" || result.Models[1].Name != "auto" {
		t.Fatalf("unexpected auto model: %+v", result.Models[1])
	}
	qwen := result.Models[2]
	if qwen.ID != "qwen3-coder-next" || qwen.InputTokenLimit != defaultModelInputTokenLimit ||
		qwen.OutputTokenLimit != defaultModelOutputTokenLimit {
		t.Fatalf("unexpected Qwen fallback metadata: %+v", qwen)
	}
}

func TestKiroModelsForAuthReturnsCredentialErrors(t *testing.T) {
	// Hard credential errors surface before any network call and must stay errors.
	tests := []struct {
		name       string
		storage    string
		wantCode   string
		wantStatus int
	}{
		{
			name:       "invalid storage JSON",
			storage:    "{",
			wantCode:   "invalid_credential",
			wantStatus: 400,
		},
		{
			name:       "missing access token",
			storage:    `{"profileArn":"arn:test"}`,
			wantCode:   "invalid_credential",
			wantStatus: 400,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldHTTPDo := kiroHTTPDo
			t.Cleanup(func() { kiroHTTPDo = oldHTTPDo })
			kiroHTTPDo = func(req hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
				t.Fatalf("HTTP must not be called for credential errors")
				return nil, nil
			}

			rawRequest, errMarshal := json.Marshal(authModelRequest{
				AuthModelRequest: pluginapi.AuthModelRequest{StorageJSON: []byte(tt.storage)},
			})
			if errMarshal != nil {
				t.Fatalf("marshal request: %v", errMarshal)
			}
			rawResponse, errHandle := kiroModelsForAuth(rawRequest)
			if errHandle != nil {
				t.Fatalf("unexpected handler error: %v", errHandle)
			}

			var envelope wire.Envelope
			if errUnmarshal := json.Unmarshal(rawResponse, &envelope); errUnmarshal != nil {
				t.Fatalf("decode envelope: %v", errUnmarshal)
			}
			if envelope.OK || envelope.Error == nil {
				t.Fatalf("expected error envelope: %s", rawResponse)
			}
			if envelope.Error.Code != tt.wantCode || envelope.Error.HTTPStatus != tt.wantStatus {
				t.Fatalf("unexpected error: %+v", envelope.Error)
			}
		})
	}
}

// targetOf reads the x-amz-target operation from a management request.
func targetOf(req hostapi.HTTPRequest) string {
	if v := req.Headers["x-amz-target"]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// TestKiroModelsForAuthDiscoversProfileForIdC covers the IdC path: a credential
// without a stored profileArn discovers one via ListAvailableProfiles, then uses
// it to fetch the account-scoped model list.
func TestKiroModelsForAuthDiscoversProfileForIdC(t *testing.T) {
	const discovered = "arn:aws:codewhisperer:us-east-1:399359779555:profile/UPGA9WG4MPRN"

	oldHTTPDo := kiroHTTPDo
	t.Cleanup(func() { kiroHTTPDo = oldHTTPDo })
	var modelsBody listAvailableModelsRequest
	kiroHTTPDo = func(req hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		switch targetOf(req) {
		case listAvailableProfilesTarget:
			return &hostapi.HTTPResponse{StatusCode: 200, Body: []byte(
				`{"profiles":[{"arn":"` + discovered + `","profileName":"QDevProfile-us-east-1"}]}`)}, nil
		case listAvailableModelsTarget:
			_ = json.Unmarshal(req.Body, &modelsBody)
			return &hostapi.HTTPResponse{StatusCode: 200, Body: []byte(
				`{"models":[{"modelId":"auto","modelName":"Auto"},{"modelId":"claude-sonnet-4.6","modelName":"Claude Sonnet 4.6"}]}`)}, nil
		default:
			t.Fatalf("unexpected target: %q", targetOf(req))
			return nil, nil
		}
	}

	rawRequest, _ := json.Marshal(authModelRequest{
		AuthModelRequest: pluginapi.AuthModelRequest{StorageJSON: []byte(`{"accessToken":"idc-token","authMethod":"IdC"}`)},
	})
	rawResponse, errHandle := kiroModelsForAuth(rawRequest)
	if errHandle != nil {
		t.Fatalf("unexpected handler error: %v", errHandle)
	}

	// The discovered ARN must be forwarded to ListAvailableModels.
	if modelsBody.ProfileArn != discovered {
		t.Fatalf("expected discovered profileArn %q in models request, got %q", discovered, modelsBody.ProfileArn)
	}

	var envelope wire.Envelope
	if errUnmarshal := json.Unmarshal(rawResponse, &envelope); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v", errUnmarshal)
	}
	if !envelope.OK {
		t.Fatalf("expected OK envelope, got: %s", rawResponse)
	}
	var result pluginapi.ModelResponse
	if errUnmarshal := json.Unmarshal(envelope.Result, &result); errUnmarshal != nil {
		t.Fatalf("decode model response: %v", errUnmarshal)
	}
	// The account-scoped list (2 models), NOT the static catalog.
	if len(result.Models) != 2 || result.Models[0].ID != "auto" || result.Models[1].ID != "claude-sonnet-4-6" {
		t.Fatalf("expected account-scoped models [auto, claude-sonnet-4-6], got %+v", result.Models)
	}
}

// TestKiroModelsForAuthFallsBackToStaticCatalog covers the graceful degradation:
// when the account-scoped ListAvailableModels endpoint is unavailable — Builder ID
// (ListAvailableProfiles denied) or any discovery failure — the plugin must
// advertise the static catalog rather than return an error, which would make the
// host unregister every model for the auth.
func TestKiroModelsForAuthFallsBackToStaticCatalog(t *testing.T) {
	tests := []struct {
		name         string
		storage      string
		profilesResp *hostapi.HTTPResponse // ListAvailableProfiles reply (nil → not expected)
		modelsResp   *hostapi.HTTPResponse // ListAvailableModels reply (nil → not expected)
		wantProfiles bool
		wantModels   bool
	}{
		{
			name:         "builder id: ListAvailableProfiles denied",
			storage:      `{"accessToken":"test-token","authMethod":"builder-id"}`,
			profilesResp: &hostapi.HTTPResponse{StatusCode: 400, Body: []byte(`{"__type":"AccessDeniedException","message":"User is not authorized to access this feature."}`)},
			wantProfiles: true,
			wantModels:   false,
		},
		{
			name:         "discovered profile but models status error",
			storage:      `{"accessToken":"test-token"}`,
			profilesResp: &hostapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"profiles":[{"arn":"arn:test"}]}`)},
			modelsResp:   &hostapi.HTTPResponse{StatusCode: 403, Body: []byte("bearer token invalid")},
			wantProfiles: true,
			wantModels:   true,
		},
		{
			name:         "discovered profile but models empty",
			storage:      `{"accessToken":"test-token"}`,
			profilesResp: &hostapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"profiles":[{"arn":"arn:test"}]}`)},
			modelsResp:   &hostapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"models":[]}`)},
			wantProfiles: true,
			wantModels:   true,
		},
		{
			name:         "stored profileArn but models status error skips discovery",
			storage:      `{"accessToken":"test-token","profileArn":"arn:stored"}`,
			modelsResp:   &hostapi.HTTPResponse{StatusCode: 403, Body: []byte("bearer token invalid")},
			wantProfiles: false,
			wantModels:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldHTTPDo := kiroHTTPDo
			t.Cleanup(func() { kiroHTTPDo = oldHTTPDo })
			var gotProfiles, gotModels bool
			kiroHTTPDo = func(req hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
				switch targetOf(req) {
				case listAvailableProfilesTarget:
					gotProfiles = true
					return tt.profilesResp, nil
				case listAvailableModelsTarget:
					gotModels = true
					return tt.modelsResp, nil
				default:
					t.Fatalf("unexpected target: %q", targetOf(req))
					return nil, nil
				}
			}

			rawRequest, _ := json.Marshal(authModelRequest{
				AuthModelRequest: pluginapi.AuthModelRequest{StorageJSON: []byte(tt.storage)},
			})
			rawResponse, errHandle := kiroModelsForAuth(rawRequest)
			if errHandle != nil {
				t.Fatalf("unexpected handler error: %v", errHandle)
			}
			if gotProfiles != tt.wantProfiles {
				t.Fatalf("ListAvailableProfiles called=%v, want %v", gotProfiles, tt.wantProfiles)
			}
			if gotModels != tt.wantModels {
				t.Fatalf("ListAvailableModels called=%v, want %v", gotModels, tt.wantModels)
			}

			var envelope wire.Envelope
			if errUnmarshal := json.Unmarshal(rawResponse, &envelope); errUnmarshal != nil {
				t.Fatalf("decode envelope: %v", errUnmarshal)
			}
			if !envelope.OK {
				t.Fatalf("expected OK envelope, got: %s", rawResponse)
			}
			var result pluginapi.ModelResponse
			if errUnmarshal := json.Unmarshal(envelope.Result, &result); errUnmarshal != nil {
				t.Fatalf("decode model response: %v", errUnmarshal)
			}
			if result.Provider != providerKiro || len(result.Models) != len(kiroModelIDs) {
				t.Fatalf("expected static catalog (%d models), got provider=%q models=%d", len(kiroModelIDs), result.Provider, len(result.Models))
			}
		})
	}
}

func TestClientKiroModelID(t *testing.T) {
	tests := map[string]string{
		"claude-opus-4.8":   "claude-opus-4-8",
		"claude-sonnet-4.6": "claude-sonnet-4-6",
		"claude-sonnet-4":   "claude-sonnet-4",
		"auto":              "auto",
		"deepseek-3.2":      "deepseek-3.2",
	}
	for native, want := range tests {
		if got := clientKiroModelID(native); got != want {
			t.Errorf("clientKiroModelID(%q) = %q, want %q", native, got, want)
		}
	}
}

// TestOpus55FallbackContextLimit covers RVZO6 8c6a107 adapted to this fork:
// Opus 5.5 keeps its dotted ID (FORK.md); the hyphenated spelling is a distinct
// non-Kiro route and keeps the generic default.
func TestOpus55FallbackContextLimit(t *testing.T) {
	resetDiscoveredContextWindows()
	t.Cleanup(resetDiscoveredContextWindows)
	if got := fallbackModelInputTokenLimit("claude-opus-5.5"); got != 1000000 {
		t.Fatalf("fallback limit = %d, want 1000000", got)
	}
	if got := contextWindowForModel("claude-opus-5.5"); got != 1000000 {
		t.Fatalf("contextWindowForModel = %d, want 1000000", got)
	}
	if got := fallbackModelInputTokenLimit("claude-opus-5-5"); got != defaultModelInputTokenLimit {
		t.Fatalf("hyphen route limit = %d", got)
	}
	if got := clientKiroModelID("claude-opus-5.5"); got != "claude-opus-5.5" {
		t.Fatalf("dotted Opus 5.5 ID rewritten to %q", got)
	}
	for _, model := range kiroModels() {
		want := fallbackModelInputTokenLimit(model.ID)
		if model.ContextLength != want || model.InputTokenLimit != want {
			t.Fatalf("unexpected static context for %q: %+v", model.ID, model)
		}
	}
	for _, limit := range []int64{0, -1, 200000, 750000, 1000000} {
		models := availableModelsToPluginModels([]availableModel{{ModelID: "claude-opus-5.5", TokenLimits: tokenLimits{MaxInputTokens: limit, MaxOutputTokens: 12345}}})
		want := limit
		if want <= 0 {
			want = 1000000
		}
		if len(models) != 1 || models[0].ID != "claude-opus-5.5" || models[0].InputTokenLimit != want || models[0].ContextLength != want || models[0].OutputTokenLimit != 12345 {
			t.Fatalf("account limit %d not respected: %+v", limit, models)
		}
	}
	if got := fallbackModelInputTokenLimit("unknown-model"); got != defaultModelInputTokenLimit {
		t.Fatalf("unknown model limit changed: %d", got)
	}
}

func TestContextWindowForModelUsesDiscoveredLimits(t *testing.T) {
	resetDiscoveredContextWindows()
	t.Cleanup(resetDiscoveredContextWindows)
	if got := contextWindowForModel("claude-sonnet-4-6"); got != defaultModelInputTokenLimit {
		t.Fatalf("default = %d", got)
	}
	availableModelsToPluginModels([]availableModel{
		{ModelID: "claude-sonnet-4.6", TokenLimits: tokenLimits{MaxInputTokens: 1000000}},
		{ModelID: "deepseek-3.2", TokenLimits: tokenLimits{MaxInputTokens: 128000}},
	})
	for id, want := range map[string]int64{
		"claude-sonnet-4-6": 1000000, // client ID
		"claude-sonnet-4.6": 1000000, // native ID
		"deepseek-3.2":      128000,
		"":                  defaultModelInputTokenLimit,
	} {
		if got := contextWindowForModel(id); got != want {
			t.Errorf("contextWindowForModel(%q) = %d, want %d", id, got, want)
		}
	}
}
