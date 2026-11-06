package hostapi

import (
	"bytes"
	"encoding/json"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

type HTTPStream struct {
	StatusCode int    `json:"status_code"`
	StreamID   string `json:"stream_id"`
}
type HTTPChunk struct {
	Payload []byte `json:"payload"`
	Error   string `json:"error"`
	Done    bool   `json:"done"`
}

func invoke(method string, request any, result any) error {
	payload, err := json.Marshal(request)
	if err != nil {
		return err
	}
	return callJSON(method, payload, result)
}
func OpenOperation(callback string) (string, error) {
	var out struct {
		ID string `json:"operation_id"`
	}
	err := invoke("host.http.operation_open", map[string]string{"host_callback_id": callback}, &out)
	return out.ID, err
}
func CancelOperation(callback, operation string) error {
	return invoke("host.http.cancel", map[string]string{"host_callback_id": callback, "operation_id": operation}, nil)
}
func HTTPDoStream(req HTTPRequest, operation string) (*HTTPStream, error) {
	var out HTTPStream
	err := invoke(pluginabi.MethodHostHTTPDoStream, struct {
		HTTPRequest
		Operation string `json:"operation_id"`
	}{req, operation}, &out)
	return &out, err
}
func HTTPStreamRead(id string) (HTTPChunk, error) {
	var out HTTPChunk
	err := invoke(pluginabi.MethodHostHTTPStreamRead, map[string]string{"stream_id": id}, &out)
	return out, err
}
func HTTPStreamClose(id string) error {
	return invoke(pluginabi.MethodHostHTTPStreamClose, map[string]string{"stream_id": id}, nil)
}
func StreamEmit(id string, payload []byte) error {
	// CPA translators consume data lines; Claude passthrough retains the event line.
	if i := bytes.Index(payload, []byte("\ndata:")); i >= 0 {
		if err := invoke(pluginabi.MethodHostStreamEmit, map[string]any{"stream_id": id, "payload": payload[:i+1]}, nil); err != nil {
			return err
		}
		payload = payload[i+1:]
	}
	return invoke(pluginabi.MethodHostStreamEmit, map[string]any{"stream_id": id, "payload": payload}, nil)
}
func StreamClose(id, code string) error {
	req := map[string]any{"stream_id": id}
	if code != "" {
		req["error_detail"] = map[string]any{"code": code, "http_status": 502, "message": "Kiro stream interrupted (" + code + "). No request replayed."}
	}
	return invoke(pluginabi.MethodHostStreamClose, req, nil)
}

// StreamCloseError closes a stream with a classified error. Hosts that have not
// yet written response headers surface the status to the client.
func StreamCloseError(id, code, message string, status, retryAfterSeconds int, retryable bool) error {
	detail := map[string]any{"code": code, "http_status": status, "message": message}
	if retryAfterSeconds > 0 {
		detail["retry_after_seconds"] = retryAfterSeconds
	}
	if retryable {
		detail["retryable"] = true
	}
	return invoke(pluginabi.MethodHostStreamClose, map[string]any{"stream_id": id, "error_detail": detail}, nil)
}
