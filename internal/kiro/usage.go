package kiro

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/xiaokui-dev/cliproxyapi-kiro-plugin/internal/hostapi"
)

const usageURLTemplate = "https://q.%s.amazonaws.com/getUsageLimits"

// usageResourceType is the internal resource kind CodeWhisperer reports usage for.
const usageResourceType = "AGENTIC_REQUEST"

// usageEntry is one credential's usage query outcome.
type usageEntry struct {
	AuthIndex  string          `json:"auth_index"`
	Name       string          `json:"name"`
	AuthMethod string          `json:"authMethod,omitempty"`
	Email      string          `json:"email,omitempty"`
	Usage      json.RawMessage `json:"usage,omitempty"`
	Error      string          `json:"error,omitempty"`
}

// usageReport aggregates usage across the selected Kiro credentials.
type usageReport struct {
	Credentials []usageEntry `json:"credentials"`
}

// handleUsageLimits queries getUsageLimits for every Kiro credential (or the one
// named by ?auth=<auth_index|name>) and returns an aggregated JSON report. It
// never fails wholesale: a per-credential error is recorded inline so one bad
// credential does not hide the others.
func handleUsageLimits(callbackID string, query url.Values) pluginapi.ManagementResponse {
	entries, errList := hostAuthListFn()
	if errList != nil {
		return usageErrorResponse(http.StatusBadGateway, "list credentials: "+errList.Error())
	}

	want := strings.TrimSpace(query.Get("auth"))
	report := usageReport{Credentials: make([]usageEntry, 0)}
	matched := 0

	for _, entry := range entries {
		if !strings.EqualFold(strings.TrimSpace(entry.Provider), providerKiro) &&
			!strings.EqualFold(strings.TrimSpace(entry.Type), providerKiro) {
			continue
		}
		if want != "" && entry.AuthIndex != want && !strings.EqualFold(entry.Name, want) {
			continue
		}
		matched++
		report.Credentials = append(report.Credentials, queryOneUsage(callbackID, entry))
	}

	if want != "" && matched == 0 {
		return usageErrorResponse(http.StatusNotFound, "no kiro credential matches auth="+want)
	}

	payload, errMarshal := json.Marshal(report)
	if errMarshal != nil {
		return usageErrorResponse(http.StatusInternalServerError, "encode report: "+errMarshal.Error())
	}
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       payload,
	}
}

// queryOneUsage resolves one credential's stored JSON and queries its usage.
func queryOneUsage(callbackID string, entry hostapi.AuthEntry) usageEntry {
	out := usageEntry{AuthIndex: entry.AuthIndex, Name: entry.Name, Email: entry.Email}

	got, errGet := hostAuthGetFn(entry.AuthIndex)
	if errGet != nil {
		out.Error = "get credential: " + errGet.Error()
		return out
	}
	var cred kiroCredential
	if errUnmarshal := json.Unmarshal(got.JSON, &cred); errUnmarshal != nil {
		out.Error = "decode credential: " + errUnmarshal.Error()
		return out
	}
	out.AuthMethod = cred.AuthMethod
	if strings.TrimSpace(cred.AccessToken) == "" {
		out.Error = "credential has no accessToken"
		return out
	}

	usage, errUsage := fetchUsageLimits(callbackID, cred)
	if errUsage != nil {
		out.Error = errUsage.Error()
		return out
	}
	out.Usage = usage
	return out
}

// fetchUsageLimits calls the upstream getUsageLimits endpoint for one credential.
func fetchUsageLimits(callbackID string, cred kiroCredential) (json.RawMessage, error) {
	region, errRegion := resolveRegion(cred.Region, cred.IDCRegion)
	if errRegion != nil {
		return nil, fmt.Errorf("invalid kiro credential region: %w", errRegion)
	}
	cred, errProfile := credentialWithProfile(cred, callbackID, region)
	if errProfile != nil {
		return nil, errProfile
	}

	params := url.Values{}
	params.Set("isEmailRequired", "true")
	params.Set("origin", originAIEditor)
	params.Set("resourceType", usageResourceType)
	if cred.ProfileArn != "" {
		params.Set("profileArn", cred.ProfileArn)
	}
	// 请求头携带 accessToken:先校验 region 再拼查询串。
	base, errURL := safeEndpoint(usageURLTemplate, region, "")
	if errURL != nil {
		return nil, fmt.Errorf("invalid kiro usage endpoint: %w", errURL)
	}
	fullURL := base + "?" + params.Encode()

	resp, errDo := kiroHTTPDo(hostapi.HTTPRequest{
		HostCallbackID: callbackID,
		Method:         http.MethodGet,
		URL:            fullURL,
		Headers:        usageRequestHeaders(cred),
	})
	if errDo != nil {
		return nil, fmt.Errorf("getUsageLimits request: %w", errDo)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("getUsageLimits HTTP %d: %s", resp.StatusCode, truncate(string(resp.Body), 200))
	}
	if !json.Valid(resp.Body) {
		return nil, fmt.Errorf("getUsageLimits returned non-JSON body")
	}
	return json.RawMessage(resp.Body), nil
}

// usageRequestHeaders builds the KiroIDE headers for a getUsageLimits GET.
func usageRequestHeaders(cred kiroCredential) map[string][]string {
	mid := machineID(cred)
	return map[string][]string{
		"Authorization":         {"Bearer " + cred.AccessToken},
		"Accept":                {"application/json"},
		"amz-sdk-invocation-id": {uuidV4()},
		"amz-sdk-request":       {"attempt=1; max=1"},
		"x-amz-user-agent":      {fmt.Sprintf("aws-sdk-js/1.0.34 KiroIDE-%s-%s", kiroVersion, mid)},
		"user-agent":            {fmt.Sprintf("aws-sdk-js/1.0.34 ua/2.1 os/other lang/js md/nodejs#20.11.0 api/codewhispererstreaming#1.0.34 m/E KiroIDE-%s-%s", kiroVersion, mid)},
	}
}

// usageErrorResponse builds a Management API error response body.
func usageErrorResponse(status int, message string) pluginapi.ManagementResponse {
	body, _ := json.Marshal(map[string]string{"error": message})
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       body,
	}
}
