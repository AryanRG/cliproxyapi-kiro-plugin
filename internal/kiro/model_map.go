package kiro

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/xiaokui-dev/cliproxyapi-kiro-plugin/internal/hostapi"
	"github.com/xiaokui-dev/cliproxyapi-kiro-plugin/internal/wire"
)

const (
	listAvailableModelsURLTemplate = "https://management.%s.kiro.dev/"
	listAvailableModelsTarget      = "KiroControlPlaneBearerService.ListAvailableModels"
	listAvailableProfilesTarget    = "KiroControlPlaneBearerService.ListAvailableProfiles"
	defaultModelInputTokenLimit    = int64(200000)
	defaultModelOutputTokenLimit   = int64(8192)
)

// kiroModelIDs are the client-facing model names retained by the legacy fixed-list
// helper for compatibility with existing callers.
var kiroModelIDs = []string{
	"claude-opus-5",
	"claude-opus-4-8",
	"claude-opus-4-7",
	"claude-opus-4-6",
	"claude-opus-4-5",
	"claude-sonnet-5",
	"claude-sonnet-4-6",
	"claude-sonnet-4-5",
	"claude-sonnet-4-0",
	"claude-haiku-4-5",
}

// modelMapping translates a client-facing model name to the CodeWhisperer-native
// model name sent upstream. It ONLY normalizes the version separator (client
// "claude-opus-4-8" -> upstream "claude-opus-4.8"); it deliberately does NOT
// collapse several versions onto one model.
//
// Per-account availability is enforced UPSTREAM: Kiro accepts or rejects each
// model (INVALID_MODEL_ID) according to the account's plan/tier. The plugin must
// not pre-assume any account's capabilities.
var modelMapping = map[string]string{
	"claude-opus-5":     "claude-opus-5",
	"claude-opus-4-8":   "claude-opus-4.8",
	"claude-opus-4-7":   "claude-opus-4.7",
	"claude-opus-4-6":   "claude-opus-4.6",
	"claude-opus-4-5":   "claude-opus-4.5",
	"claude-sonnet-5":   "claude-sonnet-5",
	"claude-sonnet-4-6": "claude-sonnet-4.6",
	"claude-sonnet-4-5": "claude-sonnet-4.5",
	"claude-sonnet-4-0": "claude-sonnet-4.0",
	"claude-haiku-4-5":  "claude-haiku-4.5",
}

// nativeModelMapping keeps the public client ID stable while preserving the
// native dotted ID returned by Kiro in ModelInfo.Name.
var nativeModelMapping = map[string]string{
	"claude-opus-5":     "claude-opus-5",
	"claude-opus-4.8":   "claude-opus-4-8",
	"claude-opus-4.7":   "claude-opus-4-7",
	"claude-opus-4.6":   "claude-opus-4-6",
	"claude-opus-4.5":   "claude-opus-4-5",
	"claude-sonnet-5":   "claude-sonnet-5",
	"claude-sonnet-4.6": "claude-sonnet-4-6",
	"claude-sonnet-4.5": "claude-sonnet-4-5",
	"claude-sonnet-4.0": "claude-sonnet-4-0",
	"claude-haiku-4.5":  "claude-haiku-4-5",
}

// resolveKiroModel returns the CodeWhisperer-native model name for a client-facing
// model ID, falling back to the input when there is no mapping (used by the executor in M3).
func resolveKiroModel(id string) string {
	if mapped, ok := modelMapping[id]; ok {
		return mapped
	}
	return id
}

// clientKiroModelID converts a native Kiro model ID to the public client ID used
// by the existing plugin API. New or non-Claude IDs remain unchanged.
func clientKiroModelID(id string) string {
	if mapped, ok := nativeModelMapping[id]; ok {
		return mapped
	}
	return id
}

// fallbackModelInputTokenLimit applies documented Kiro limits when account
// discovery fails or omits token limits. Unknown models retain the conservative
// default; positive account-reported limits always take precedence.
func fallbackModelInputTokenLimit(id string) int64 {
	// https://kiro.dev/docs/models/ (verified 2026-10-04).
	// Only the dotted native ID is a Kiro route; "claude-opus-5-5" is a distinct
	// (non-Kiro) Claude account route in this fork and keeps the default.
	if strings.TrimSpace(id) == "claude-opus-5.5" {
		return 1000000
	}
	return defaultModelInputTokenLimit
}

// kiroModels builds the ModelInfo list advertised to the host registry. It is
// retained for static compatibility, while model.for_auth uses the account's
// management response below.
func kiroModels() []pluginapi.ModelInfo {
	models := make([]pluginapi.ModelInfo, 0, len(kiroModelIDs))
	for _, id := range kiroModelIDs {
		inputLimit := fallbackModelInputTokenLimit(id)
		models = append(models, pluginapi.ModelInfo{
			ID:                         id,
			Object:                     "model",
			OwnedBy:                    providerKiro,
			Type:                       "claude",
			DisplayName:                id,
			Name:                       id,
			SupportedGenerationMethods: []string{"generateContent"},
			InputTokenLimit:            inputLimit,
			OutputTokenLimit:           defaultModelOutputTokenLimit,
			ContextLength:              inputLimit,
			MaxCompletionTokens:        defaultModelOutputTokenLimit,
		})
	}
	return models
}

// authModelRequest mirrors the host's rpcAuthModelRequest: the embedded
// AuthModelRequest plus the callback ID needed for host.http.do.
type authModelRequest struct {
	pluginapi.AuthModelRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type listAvailableModelsRequest struct {
	Origin     string `json:"origin"`
	ProfileArn string `json:"profileArn,omitempty"`
}

type listAvailableProfilesResponse struct {
	Profiles []availableProfile `json:"profiles"`
}

// availableProfile tolerates both field spellings the control plane has used for
// the ARN (`arn` in current responses, `profileArn` defensively).
type availableProfile struct {
	Arn         string `json:"arn"`
	ProfileArn  string `json:"profileArn"`
	ProfileName string `json:"profileName"`
}

type listAvailableModelsResponse struct {
	Models []availableModel `json:"models"`
}

type availableModel struct {
	ModelID             string      `json:"modelId"`
	ModelName           string      `json:"modelName"`
	Description         string      `json:"description"`
	SupportedInputTypes []string    `json:"supportedInputTypes"`
	TokenLimits         tokenLimits `json:"tokenLimits"`
}

type tokenLimits struct {
	MaxInputTokens  int64 `json:"maxInputTokens"`
	MaxOutputTokens int64 `json:"maxOutputTokens"`
}

// kiroModelsForAuth asks Kiro which models the credential can actually use.
// The management endpoint is account-scoped by accessToken/profileArn, so when
// it answers we advertise exactly what it returns.
//
// Graceful degradation: ListAvailableModels hard-requires a valid profileArn,
// which AWS Builder ID (free tier) accounts do not have and cannot discover
// (ListAvailableProfiles returns AccessDenied for them). Chat still works for
// those accounts without a profileArn, so instead of returning an error — which
// makes the host UNREGISTER every model for this auth and surfaces as "no models
// bound" — we fall back to the static catalog. Per-account availability is still
// enforced upstream at chat time (INVALID_MODEL_ID), consistent with the
// modelMapping design note above.
func kiroModelsForAuth(request []byte) ([]byte, error) {
	var req authModelRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}

	var cred kiroCredential
	if errUnmarshal := json.Unmarshal(req.StorageJSON, &cred); errUnmarshal != nil {
		return wire.ErrorStatus("invalid_credential", "decode kiro credential: "+errUnmarshal.Error(), http.StatusBadRequest), nil
	}
	if strings.TrimSpace(cred.AccessToken) == "" {
		return wire.ErrorStatus("invalid_credential", "kiro credential has no accessToken", http.StatusBadRequest), nil
	}

	// region 非法时不发任何请求,退回静态目录(与其他发现失败情形一致)。
	region, errRegion := resolveRegion(cred.Region, cred.IDCRegion)
	if errRegion != nil {
		return staticModelsEnvelope()
	}

	// ListAvailableModels hard-requires a valid profileArn. IdC / organization
	// accounts have one but the device-code login never captured it, so discover
	// it now via ListAvailableProfiles. AWS Builder ID accounts are not authorized
	// for that call (AccessDenied) and yield "", so we advertise the static catalog
	// instead — their chat works fine without a profileArn, and per-account
	// availability is enforced upstream at chat time (INVALID_MODEL_ID).
	profileArn := strings.TrimSpace(cred.ProfileArn)
	if profileArn == "" {
		profileArn = discoverProfileArn(cred, req.HostCallbackID, region)
	}
	if profileArn == "" {
		return staticModelsEnvelope()
	}

	body, errMarshal := json.Marshal(listAvailableModelsRequest{
		Origin:     originAIEditor,
		ProfileArn: profileArn,
	})
	if errMarshal != nil {
		return nil, fmt.Errorf("encode list available models request: %w", errMarshal)
	}

	// 请求头携带 accessToken:region 不合法时退回静态目录,绝不发往未校验主机。
	modelsURL, errURL := safeEndpoint(listAvailableModelsURLTemplate, region, "")
	if errURL != nil {
		return staticModelsEnvelope()
	}

	resp, errDo := kiroHTTPDo(hostapi.HTTPRequest{
		HostCallbackID: req.HostCallbackID,
		Method:         http.MethodPost,
		URL:            modelsURL,
		Headers:        listAvailableModelsHeaders(cred),
		Body:           body,
	})
	// On any discovery failure, degrade to the static catalog rather than error.
	if errDo != nil || resp == nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return staticModelsEnvelope()
	}

	var decoded listAvailableModelsResponse
	if errUnmarshal := json.Unmarshal(resp.Body, &decoded); errUnmarshal != nil {
		return staticModelsEnvelope()
	}
	models := availableModelsToPluginModels(decoded.Models)
	if len(models) == 0 {
		return staticModelsEnvelope()
	}

	return wire.OK(pluginapi.ModelResponse{
		Provider: providerKiro,
		Models:   models,
	})
}

// staticModelsEnvelope wraps the static model catalog in a model.for_auth OK
// envelope. Used as the fallback when account-scoped discovery is unavailable.
func staticModelsEnvelope() ([]byte, error) {
	return wire.OK(pluginapi.ModelResponse{
		Provider: providerKiro,
		Models:   kiroModels(),
	})
}

func listAvailableModelsHeaders(cred kiroCredential) map[string][]string {
	return kiroManagementHeaders(cred, listAvailableModelsTarget)
}

// kiroManagementHeaders builds the AWS-flavored headers for a management.kiro.dev
// bearer call, differing only by the x-amz-target operation.
func kiroManagementHeaders(cred kiroCredential, target string) map[string][]string {
	mid := machineID(cred)
	return map[string][]string{
		"Authorization":         {"Bearer " + cred.AccessToken},
		"Content-Type":          {"application/x-amz-json-1.0"},
		"Accept":                {"application/json"},
		"x-amz-target":          {target},
		"TokenType":             {"SSO_OIDC"},
		"amz-sdk-invocation-id": {uuidV4()},
		"amz-sdk-request":       {"attempt=1; max=3"},
		"x-amz-user-agent":      {fmt.Sprintf("aws-sdk-js/1.0.34 KiroIDE-%s-%s", kiroVersion, mid)},
		"user-agent":            {fmt.Sprintf("aws-sdk-js/1.0.34 ua/2.1 os/other lang/js md/nodejs#20.11.0 api/codewhispererstreaming#1.0.34 m/E KiroIDE-%s-%s", kiroVersion, mid)},
	}
}

// discoverProfileArn resolves the credential's own profile ARN via
// ListAvailableProfiles. IdC / organization accounts return their QDevProfile
// ARN here, which unlocks the account-scoped ListAvailableModels call. AWS
// Builder ID accounts are not authorized for this operation (AccessDenied) and
// yield "", signalling the caller to fall back to the static catalog. Any error
// or empty result is treated as "not discoverable" rather than fatal.
func discoverProfileArn(cred kiroCredential, callbackID, region string) string {
	// 请求头携带 accessToken:region 不合法时视为不可发现,直接返回空。
	profilesURL, errURL := safeEndpoint(listAvailableModelsURLTemplate, region, "")
	if errURL != nil {
		return ""
	}
	resp, errDo := kiroHTTPDo(hostapi.HTTPRequest{
		HostCallbackID: callbackID,
		Method:         http.MethodPost,
		URL:            profilesURL,
		Headers:        kiroManagementHeaders(cred, listAvailableProfilesTarget),
		Body:           []byte("{}"),
	})
	if errDo != nil || resp == nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ""
	}
	var decoded listAvailableProfilesResponse
	if json.Unmarshal(resp.Body, &decoded) != nil {
		return ""
	}
	for _, p := range decoded.Profiles {
		if arn := strings.TrimSpace(p.ProfileArn); arn != "" {
			return arn
		}
		if arn := strings.TrimSpace(p.Arn); arn != "" {
			return arn
		}
	}
	return ""
}

// discoveredContextWindows remembers positive maxInputTokens values reported by
// ListAvailableModels, keyed by both client and native IDs. Limits are model
// properties, so the last reported value wins across accounts.
var discoveredContextWindows = struct {
	sync.RWMutex
	limits map[string]int64
}{limits: map[string]int64{}}

func rememberContextWindow(clientID, nativeID string, limit int64) {
	if limit <= 0 {
		return
	}
	discoveredContextWindows.Lock()
	discoveredContextWindows.limits[clientID] = limit
	discoveredContextWindows.limits[nativeID] = limit
	discoveredContextWindows.Unlock()
}

func resetDiscoveredContextWindows() {
	discoveredContextWindows.Lock()
	discoveredContextWindows.limits = map[string]int64{}
	discoveredContextWindows.Unlock()
}

// contextWindowForModel returns the input context window for a client or
// native Kiro model ID: the account-reported maxInputTokens when discovery has
// seen the model, else the documented fallback (1M for Opus 5.5), else 200K.
func contextWindowForModel(model string) int64 {
	id := strings.TrimSpace(model)
	discoveredContextWindows.RLock()
	limit, ok := discoveredContextWindows.limits[id]
	if !ok {
		limit, ok = discoveredContextWindows.limits[resolveKiroModel(id)]
	}
	discoveredContextWindows.RUnlock()
	if ok && limit > 0 {
		return limit
	}
	return fallbackModelInputTokenLimit(id)
}

func availableModelsToPluginModels(available []availableModel) []pluginapi.ModelInfo {
	models := make([]pluginapi.ModelInfo, 0, len(available))
	seen := make(map[string]struct{}, len(available))
	for _, item := range available {
		nativeID := strings.TrimSpace(item.ModelID)
		if nativeID == "" {
			continue
		}
		clientID := clientKiroModelID(nativeID)
		if _, ok := seen[clientID]; ok {
			continue
		}
		seen[clientID] = struct{}{}

		inputLimit := item.TokenLimits.MaxInputTokens
		rememberContextWindow(clientID, nativeID, inputLimit)
		if inputLimit <= 0 {
			inputLimit = fallbackModelInputTokenLimit(clientID)
		}
		outputLimit := item.TokenLimits.MaxOutputTokens
		if outputLimit <= 0 {
			outputLimit = defaultModelOutputTokenLimit
		}
		displayName := strings.TrimSpace(item.ModelName)
		if displayName == "" {
			displayName = clientID
		}

		models = append(models, pluginapi.ModelInfo{
			ID:                         clientID,
			Thinking:                   modelThinkingSupport(nativeID),
			Object:                     "model",
			OwnedBy:                    providerKiro,
			Type:                       "claude",
			DisplayName:                displayName,
			Name:                       nativeID,
			Description:                item.Description,
			SupportedGenerationMethods: []string{"generateContent"},
			InputTokenLimit:            inputLimit,
			OutputTokenLimit:           outputLimit,
			ContextLength:              inputLimit,
			MaxCompletionTokens:        outputLimit,
			SupportedInputModalities:   normalizeInputModalities(item.SupportedInputTypes),
		})
	}
	return models
}

func normalizeInputModalities(types []string) []string {
	if len(types) == 0 {
		return nil
	}
	out := make([]string, 0, len(types))
	seen := make(map[string]struct{}, len(types))
	for _, raw := range types {
		modality := strings.ToLower(strings.TrimSpace(raw))
		if modality == "" {
			continue
		}
		if _, ok := seen[modality]; ok {
			continue
		}
		seen[modality] = struct{}{}
		out = append(out, modality)
	}
	return out
}
