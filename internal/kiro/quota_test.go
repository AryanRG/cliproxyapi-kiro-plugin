package kiro

import (
	"encoding/json"
	"github.com/xiaokui-dev/cliproxyapi-kiro-plugin/internal/hostapi"
	"testing"
)

func TestNormalizedQuotaPrecisionAndReset(t *testing.T) {
	result, err := normalizeQuota([]byte(`{"nextDateReset":1796083200,"usageBreakdownList":[{"resourceType":"CREDIT","currentUsage":10,"currentUsageWithPrecision":10.25,"usageLimit":100} ]}`))
	if err != nil {
		t.Fatal(err)
	}
	metrics := result["summary"].([]any)
	if metrics[1].(map[string]any)["value"] != 89.75 {
		t.Fatal(result)
	}
	bucket := result["groups"].([]any)[0].(map[string]any)["buckets"].([]any)[0].(map[string]any)
	if bucket["resetTime"] != "2026-12-01T00:00:00Z" {
		t.Fatal(bucket)
	}
}
func TestQuotaMissingIsNotZero(t *testing.T) {
	for _, raw := range []string{`{}`, `{"usageBreakdownList":[{"resourceType":"CREDIT","usageLimit":100}]}`, `{"usageBreakdownList":[{"resourceType":"CREDIT","currentUsage":-1,"usageLimit":100}]}`} {
		if _, err := normalizeQuota([]byte(raw)); err == nil {
			t.Fatal(raw)
		}
	}
}
func TestQuotaUsesOnlySuppliedAccount(t *testing.T) {
	old := kiroHTTPDo
	defer func() { kiroHTTPDo = old }()
	calls := 0
	kiroHTTPDo = func(req hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		calls++
		if req.HostCallbackID != "scoped" || req.Headers["Authorization"][0] != "Bearer selected-test-token" {
			t.Fatal("wrong auth scope")
		}
		return &hostapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"usageBreakdownList":[{"resourceType":"CREDIT","currentUsage":120,"usageLimit":100}]}`)}, nil
	}
	raw, _ := json.Marshal(map[string]any{"provider": "kiro", "host_callback_id": "scoped", "storage_json": []byte(`{"accessToken":"selected-test-token"}`)})
	response, err := fetchNormalizedQuota(raw)
	if err != nil || calls != 1 {
		t.Fatal(err, calls)
	}
	var env struct{ OK bool }
	json.Unmarshal(response, &env)
	if !env.OK {
		t.Fatal(string(response))
	}
}
