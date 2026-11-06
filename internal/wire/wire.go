// Package wire defines the JSON envelope protocol exchanged with the CLIProxyAPI
// host. Every plugin method returns an {ok, result, error} envelope; this package
// centralizes its encoding so the CGO bridge and all domain handlers agree on the
// wire format.
package wire

import "encoding/json"

// Envelope is the JSON wire format exchanged with the host: {ok, result, error}.
type Envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *EnvelopeError  `json:"error,omitempty"`
}

// EnvelopeError carries a plugin error plus an optional HTTP status the host can
// use to drive retry/refresh scheduling.
type EnvelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
	// Retryable and RetryAfterSeconds are optional hints; older hosts ignore them.
	Retryable         bool `json:"retryable,omitempty"`
	RetryAfterSeconds int  `json:"retry_after_seconds,omitempty"`
}

// OK marshals v as the result payload of a successful envelope.
func OK(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(Envelope{OK: true, Result: raw})
}

// Error returns an error envelope carrying a code and message.
func Error(code, message string) []byte {
	raw, _ := json.Marshal(Envelope{OK: false, Error: &EnvelopeError{Code: code, Message: message}})
	return raw
}

// ErrorStatus returns an error envelope carrying an HTTP status the host can use
// to drive retry/refresh scheduling.
func ErrorStatus(code, message string, status int) []byte {
	raw, _ := json.Marshal(Envelope{OK: false, Error: &EnvelopeError{Code: code, Message: message, HTTPStatus: status}})
	return raw
}

// ErrorDetail returns an error envelope carrying the full structured error.
func ErrorDetail(e EnvelopeError) []byte {
	raw, _ := json.Marshal(Envelope{OK: false, Error: &e})
	return raw
}
