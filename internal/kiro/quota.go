package kiro

import (
	"encoding/json"
	"fmt"
	"github.com/xiaokui-dev/cliproxyapi-kiro-plugin/internal/wire"
	"math"
	"net/http"
	"time"
)

// The optional v8 quota RPC is JSON-compatible with the existing C ABI.
// Keep the v7 SDK dependency for older hosts; they ignore this capability.
func fetchNormalizedQuota(raw []byte) ([]byte, error) {
	var req struct {
		Provider    string `json:"provider"`
		StorageJSON []byte `json:"storage_json"`
		Callback    string `json:"host_callback_id"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return wire.ErrorStatus("invalid_request", "Invalid quota request", 400), nil
	}
	if req.Provider != "kiro" {
		return wire.ErrorStatus("invalid_provider", "Kiro credentials required", 400), nil
	}
	var cred kiroCredential
	if json.Unmarshal(req.StorageJSON, &cred) != nil || cred.AccessToken == "" {
		return wire.ErrorStatus("invalid_credential", "Kiro credential unavailable", 401), nil
	}
	data, err := fetchUsageLimits(req.Callback, cred)
	if err != nil {
		return wire.ErrorStatus("quota_unavailable", "Kiro quota request failed; no balance inferred", http.StatusBadGateway), nil
	}
	result, err := normalizeQuota(data)
	if err != nil {
		return wire.ErrorStatus("quota_unavailable", err.Error(), http.StatusBadGateway), nil
	}
	return wire.OK(result)
}
func normalizeQuota(raw []byte) (map[string]any, error) {
	var report struct {
		Next *float64 `json:"nextDateReset"`
		Rows []struct {
			Resource     string   `json:"resourceType"`
			Used         *float64 `json:"currentUsage"`
			PreciseUsed  *float64 `json:"currentUsageWithPrecision"`
			Limit        *float64 `json:"usageLimit"`
			PreciseLimit *float64 `json:"usageLimitWithPrecision"`
		} `json:"usageBreakdownList"`
	}
	if json.Unmarshal(raw, &report) != nil {
		return nil, fmt.Errorf("Invalid Kiro quota response")
	}
	found := false
	var used, limit float64
	for _, row := range report.Rows {
		if row.Resource != "CREDIT" {
			continue
		}
		if found {
			return nil, fmt.Errorf("Ambiguous Kiro credit buckets")
		}
		found = true
		u, l := row.Used, row.Limit
		if row.PreciseUsed != nil {
			u = row.PreciseUsed
		}
		if row.PreciseLimit != nil {
			l = row.PreciseLimit
		}
		if u == nil || l == nil || *u < 0 || *l <= 0 || math.IsNaN(*u) || math.IsNaN(*l) || math.IsInf(*u, 0) || math.IsInf(*l, 0) {
			return nil, fmt.Errorf("Kiro plan credit balance unavailable")
		}
		used, limit = *u, *l
	}
	if !found {
		return nil, fmt.Errorf("No Kiro plan credit bucket")
	}
	remaining := math.Max(0, limit-used)
	metric := func(key, label string, value float64) map[string]any {
		return map[string]any{"key": key, "label": label, "value": value, "unit": "credits", "format": "number"}
	}
	bucket := map[string]any{"window": "monthly", "remainingFraction": remaining / limit, "description": "Plan credits only; bonuses, trials and overages are separate"}
	if report.Next != nil && *report.Next > 0 && *report.Next < 253402300799 {
		bucket["resetTime"] = time.Unix(int64(*report.Next), 0).UTC().Format(time.RFC3339)
	}
	return map[string]any{"summary": []any{metric("credits_used", "Plan credits used", used), metric("credits_remaining", "Plan credits remaining", remaining), metric("credits_limit", "Plan credit allowance", limit)}, "groups": []any{map[string]any{"displayName": "Monthly plan credits", "buckets": []any{bucket}}}}, nil
}
