package kiro

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/xiaokui-dev/cliproxyapi-kiro-plugin/internal/hostapi"

	"github.com/xiaokui-dev/cliproxyapi-kiro-plugin/internal/wire"
)

// stubAuthSeams installs mock host.auth.list/get + host.http.do implementations
// and restores them on cleanup.
func stubAuthSeams(t *testing.T, entries []hostapi.AuthEntry, creds map[string]string, httpDo func(hostapi.HTTPRequest) (*hostapi.HTTPResponse, error)) {
	t.Helper()
	oldList, oldGet, oldHTTP := hostAuthListFn, hostAuthGetFn, kiroHTTPDo
	t.Cleanup(func() { hostAuthListFn, hostAuthGetFn, kiroHTTPDo = oldList, oldGet, oldHTTP })

	hostAuthListFn = func() ([]hostapi.AuthEntry, error) { return entries, nil }
	hostAuthGetFn = func(authIndex string) (*hostapi.AuthGetResult, error) {
		j, ok := creds[authIndex]
		if !ok {
			return nil, errNotFound(authIndex)
		}
		return &hostapi.AuthGetResult{AuthIndex: authIndex, JSON: json.RawMessage(j)}, nil
	}
	kiroHTTPDo = httpDo
}

type errNotFound string

func (e errNotFound) Error() string { return "auth not found: " + string(e) }

func decodeUsageReport(t *testing.T, body []byte) usageReport {
	t.Helper()
	var r usageReport
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("decode usage report: %v (body=%s)", err, body)
	}
	return r
}

func TestHandleUsageLimitsAggregatesKiroOnly(t *testing.T) {
	entries := []hostapi.AuthEntry{
		{AuthIndex: "1", Name: "kiro-a.json", Provider: providerKiro},
		{AuthIndex: "2", Name: "gemini-x.json", Provider: "gemini"}, // must be skipped
		{AuthIndex: "3", Name: "kiro-b.json", Provider: providerKiro},
	}
	creds := map[string]string{
		"1": `{"accessToken":"AT1","authMethod":"builder-id","region":"us-east-1"}`,
		"3": `{"accessToken":"AT3","authMethod":"social","profileArn":"arn:p","region":"us-east-1"}`,
	}
	var sawProfileArn bool
	httpDo := func(req hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		if !strings.Contains(req.URL, "/getUsageLimits") {
			t.Fatalf("unexpected URL: %s", req.URL)
		}
		if strings.Contains(req.URL, "profileArn=arn") {
			sawProfileArn = true
		}
		return &hostapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"usedCount":3,"limitCount":100}`)}, nil
	}
	stubAuthSeams(t, entries, creds, httpDo)

	resp := handleUsageLimits("cb-1", url.Values{})
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d (%s)", resp.StatusCode, resp.Body)
	}
	report := decodeUsageReport(t, resp.Body)
	if len(report.Credentials) != 2 {
		t.Fatalf("expected 2 kiro credentials (gemini skipped), got %d", len(report.Credentials))
	}
	for _, c := range report.Credentials {
		if c.Error != "" {
			t.Fatalf("unexpected per-credential error: %+v", c)
		}
		if !strings.Contains(string(c.Usage), "limitCount") {
			t.Fatalf("usage passthrough missing: %s", c.Usage)
		}
	}
	if !sawProfileArn {
		t.Fatalf("social credential must send profileArn in query")
	}
}

func TestOrganizationUsageDiscoversAndAttachesOwnProfile(t *testing.T) {
	old := kiroHTTPDo
	t.Cleanup(func() { kiroHTTPDo = old })
	discovery, usage := 0, 0
	kiroHTTPDo = func(req hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		if req.Headers["Authorization"][0] != "Bearer own-token" {
			t.Fatal("wrong usage account")
		}
		if strings.Contains(req.URL, "management.") {
			discovery++
			return &hostapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"profiles":[{"arn":"arn:own-profile"}]}`)}, nil
		}
		usage++
		u, _ := url.Parse(req.URL)
		if u.Query().Get("profileArn") != "arn:own-profile" {
			return &hostapi.HTTPResponse{StatusCode: 400, Body: []byte(`{"message":"Invalid profileArn."}`)}, nil
		}
		return &hostapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"usageBreakdownList":[{"resourceType":"CREDIT","currentUsage":1,"usageLimit":500}]}`)}, nil
	}
	_, err := fetchUsageLimits("cb", kiroCredential{AuthMethod: "IdC", AccessToken: "own-token", Region: "us-east-1"})
	if err != nil || discovery != 1 || usage != 1 {
		t.Fatalf("quota profile failed: discovery=%d usage=%d err=%v", discovery, usage, err)
	}
}

func TestHandleUsageLimitsPerCredentialError(t *testing.T) {
	entries := []hostapi.AuthEntry{
		{AuthIndex: "1", Name: "kiro-ok.json", Provider: providerKiro},
		{AuthIndex: "2", Name: "kiro-bad.json", Provider: providerKiro},
	}
	creds := map[string]string{
		"1": `{"accessToken":"AT1","authMethod":"builder-id"}`,
		"2": `{"accessToken":"ATbad","authMethod":"builder-id"}`,
	}
	httpDo := func(req hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		if strings.Contains(req.Headers["Authorization"][0], "ATbad") {
			return &hostapi.HTTPResponse{StatusCode: 403, Body: []byte(`{"message":"forbidden"}`)}, nil
		}
		return &hostapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"usedCount":1,"limitCount":50}`)}, nil
	}
	stubAuthSeams(t, entries, creds, httpDo)

	resp := handleUsageLimits("cb-1", url.Values{})
	report := decodeUsageReport(t, resp.Body)
	if len(report.Credentials) != 2 {
		t.Fatalf("expected 2 credentials, got %d", len(report.Credentials))
	}
	var okCount, errCount int
	for _, c := range report.Credentials {
		if c.Error != "" {
			errCount++
			if !strings.Contains(c.Error, "403") {
				t.Fatalf("expected 403 in error, got %q", c.Error)
			}
		} else {
			okCount++
		}
	}
	if okCount != 1 || errCount != 1 {
		t.Fatalf("expected one ok + one error, got ok=%d err=%d", okCount, errCount)
	}
}

func TestHandleUsageLimitsAuthFilter(t *testing.T) {
	entries := []hostapi.AuthEntry{
		{AuthIndex: "1", Name: "kiro-a.json", Provider: providerKiro},
		{AuthIndex: "2", Name: "kiro-b.json", Provider: providerKiro},
	}
	creds := map[string]string{
		"1": `{"accessToken":"AT1","authMethod":"builder-id"}`,
		"2": `{"accessToken":"AT2","authMethod":"builder-id"}`,
	}
	calls := 0
	httpDo := func(req hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		calls++
		return &hostapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"usedCount":0,"limitCount":10}`)}, nil
	}
	stubAuthSeams(t, entries, creds, httpDo)

	q := url.Values{}
	q.Set("auth", "kiro-b.json")
	resp := handleUsageLimits("cb-1", q)
	report := decodeUsageReport(t, resp.Body)
	if len(report.Credentials) != 1 || report.Credentials[0].Name != "kiro-b.json" {
		t.Fatalf("auth filter should select only kiro-b.json: %+v", report.Credentials)
	}
	if calls != 1 {
		t.Fatalf("expected exactly one upstream call, got %d", calls)
	}
}

func TestHandleUsageLimitsUnknownAuth(t *testing.T) {
	entries := []hostapi.AuthEntry{{AuthIndex: "1", Name: "kiro-a.json", Provider: providerKiro}}
	creds := map[string]string{"1": `{"accessToken":"AT1"}`}
	stubAuthSeams(t, entries, creds, func(hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		return &hostapi.HTTPResponse{StatusCode: 200, Body: []byte(`{}`)}, nil
	})

	q := url.Values{}
	q.Set("auth", "does-not-exist")
	resp := handleUsageLimits("cb-1", q)
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404 for unknown auth, got %d", resp.StatusCode)
	}
}

func TestHandleManagementRoutesUsage(t *testing.T) {
	entries := []hostapi.AuthEntry{{AuthIndex: "1", Name: "kiro-a.json", Provider: providerKiro}}
	creds := map[string]string{"1": `{"accessToken":"AT1","authMethod":"builder-id"}`}
	stubAuthSeams(t, entries, creds, func(hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		return &hostapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"usedCount":5,"limitCount":100}`)}, nil
	})

	req, err := json.Marshal(managementHandleRequest{
		ManagementRequest: pluginapi.ManagementRequest{Method: "GET", Path: "/v0/management/kiro-usage"},
		HostCallbackID:    "cb-1",
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	raw, errHandle := handleManagement(req)
	if errHandle != nil {
		t.Fatalf("handleManagement: %v", errHandle)
	}
	var env wire.Envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("bad envelope: %v %+v", err, env.Error)
	}
}
