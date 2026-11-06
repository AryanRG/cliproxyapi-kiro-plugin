package hostapi

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

func TestStreamCloseCarriesPlainErrorFallback(t *testing.T) {
	var got []map[string]any
	old := HostCall
	defer func() { HostCall = old }()
	HostCall = func(method string, payload []byte) ([]byte, error) {
		if method == pluginabi.MethodHostStreamClose {
			var m map[string]any
			_ = json.Unmarshal(payload, &m)
			got = append(got, m)
		}
		return []byte(`{"ok":true,"result":{}}`), nil
	}
	_ = StreamCloseError("s1", "invalid_request_error", "prompt is too long: x", 400, 0, false)
	_ = StreamClose("s2", "upstream_idle_timeout")
	_ = StreamClose("s3", "")
	if len(got) != 3 {
		t.Fatalf("closes = %d", len(got))
	}
	if got[0]["error"] != "prompt is too long: x" || got[0]["error_detail"] == nil {
		t.Fatalf("classified close = %v", got[0])
	}
	if got[1]["error"] == nil || got[1]["error"] == "" || got[1]["error_detail"] == nil {
		t.Fatalf("interrupted close = %v", got[1])
	}
	if _, ok := got[2]["error"]; ok {
		t.Fatalf("clean close must not carry an error: %v", got[2])
	}
}
