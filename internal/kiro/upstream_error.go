package kiro

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/xiaokui-dev/cliproxyapi-kiro-plugin/internal/wire"
)

// Kiro reason codes observed in error bodies and in-band exception frames.
const (
	reasonContentLengthExceeded = "CONTENT_LENGTH_EXCEEDS_THRESHOLD"
	reasonMonthlyRequestCount   = "MONTHLY_REQUEST_COUNT"
	reasonModelCapacity         = "INSUFFICIENT_MODEL_CAPACITY"
	reasonUserRateExceeded      = "USER_REQUEST_RATE_EXCEEDED"
	blockedAccountMessage       = "User is not authorized to make this call"
	promptTooLongPrefix         = "prompt is too long: "
	maxErrorDetail              = 300
)

// kiroFailure is a classified upstream failure, rendered either as an error
// envelope (before any client output) or as an Anthropic SSE error event.
type kiroFailure struct {
	Status     int
	Code       string // host error code; also the Anthropic error type where one applies
	Message    string
	RetryAfter int // seconds, 0 when unknown
	Retryable  bool
}

func (f *kiroFailure) Error() string { return f.Code }

func (f *kiroFailure) envelope() []byte {
	return wire.ErrorDetail(wire.EnvelopeError{Code: f.Code, Message: f.Message, HTTPStatus: f.Status, Retryable: f.Retryable, RetryAfterSeconds: f.RetryAfter})
}

// anthropicType maps the failure to an Anthropic error type for SSE error events.
func (f *kiroFailure) anthropicType() string {
	switch {
	case f.Status == 400:
		return "invalid_request_error"
	case f.Status == 401:
		return "authentication_error"
	case f.Status == 403:
		return "permission_error"
	case f.Status == 429:
		return "rate_limit_error"
	case f.Status == 503 || f.Status == 529:
		return "overloaded_error"
	default:
		return "api_error"
	}
}

func (f *kiroFailure) sseEvent() []byte {
	b, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"type": f.anthropicType(), "message": f.Message}})
	return append(append([]byte("event: error\ndata: "), b...), '\n', '\n')
}

var credentialLike = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]{16,}|(aoa|aor)[A-Za-z0-9._~+/=-]{16,}`)

// errorDetail extracts a short, credential-free description from a Kiro error
// body: the JSON message/reason when present, otherwise the truncated raw text.
func errorDetail(body []byte) (reason, detail string) {
	var parsed struct {
		Message  string `json:"message"`
		MessageU string `json:"Message"`
		Reason   string `json:"reason"`
		Type     string `json:"__type"`
	}
	text := strings.TrimSpace(string(body))
	if json.Unmarshal(body, &parsed) == nil {
		reason = parsed.Reason
		if m := firstNonEmptyStr(parsed.Message, parsed.MessageU); m != "" {
			text = m
		}
		if reason != "" && !strings.Contains(text, reason) {
			text = strings.TrimSpace(text + " (" + reason + ")")
		}
	}
	text = credentialLike.ReplaceAllString(strings.Join(strings.Fields(text), " "), "${1}[redacted]")
	return reason, strings.ToValidUTF8(truncate(text, maxErrorDetail), "")
}

func isPromptTooLong(reason, detail string, lenient bool) bool {
	if reason == reasonContentLengthExceeded || strings.Contains(detail, reasonContentLengthExceeded) {
		return true
	}
	lower := strings.ToLower(detail)
	if strings.Contains(lower, "input is too long") {
		return true
	}
	return lenient && strings.Contains(lower, "too long")
}

func promptTooLong(detail string) *kiroFailure {
	if detail == "" {
		detail = "Kiro rejected the request because the input exceeds the model context window"
	}
	return &kiroFailure{Status: http.StatusBadRequest, Code: "invalid_request_error", Message: promptTooLongPrefix + detail}
}

// classifyKiroFailure maps a non-2xx generateAssistantResponse answer.
func classifyKiroFailure(status int, body []byte) *kiroFailure {
	reason, detail := errorDetail(body)
	return classifyReason(status, reason, detail, status == http.StatusBadRequest)
}

// classifyStreamException maps an in-band exception/error frame.
func classifyStreamException(headers map[string]string, payload []byte) *kiroFailure {
	reason, detail := errorDetail(payload)
	code := firstNonEmptyStr(headers[":exception-type"], headers[":error-code"], headers[":event-type"])
	if reason == "" {
		reason = code
	}
	if detail == "" {
		detail = code
	}
	if f := classifyReason(0, reason, detail, true); f != nil && f.Status != 0 {
		return f
	}
	return &kiroFailure{Status: http.StatusBadGateway, Code: "upstream_event_error", Message: "Kiro stream exception: " + detail}
}

func classifyReason(status int, reason, detail string, lenient bool) *kiroFailure {
	has := func(code string) bool { return reason == code || strings.Contains(detail, code) }
	switch {
	case isPromptTooLong(reason, detail, lenient):
		return promptTooLong(detail)
	case has(reasonMonthlyRequestCount):
		now := kiroNow().UTC()
		reset := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		return &kiroFailure{Status: http.StatusTooManyRequests, Code: "rate_limit_error", RetryAfter: int(reset.Sub(now).Seconds()) + 1,
			Message: fmt.Sprintf("Kiro monthly request limit reached for this account (MONTHLY_REQUEST_COUNT); resets %s", reset.Format("2006-01-02"))}
	case has(reasonModelCapacity):
		return &kiroFailure{Status: http.StatusServiceUnavailable, Code: "overloaded_error", Retryable: true,
			Message: "Kiro model capacity temporarily unavailable (INSUFFICIENT_MODEL_CAPACITY); retry later"}
	case has(reasonUserRateExceeded):
		return &kiroFailure{Status: http.StatusTooManyRequests, Code: "rate_limit_error", Retryable: true,
			Message: "Kiro request rate exceeded (USER_REQUEST_RATE_EXCEEDED); retry later"}
	case status == http.StatusForbidden && strings.Contains(detail, blockedAccountMessage):
		return &kiroFailure{Status: http.StatusForbidden, Code: "permission_error",
			Message: "Kiro account blocked/unauthorized: " + detail}
	case status == 0:
		return nil
	default:
		// Status is propagated verbatim. Token-expired 403 is deliberately not
		// remapped to 401 (see fetchKiroEvents).
		return &kiroFailure{Status: status, Code: "upstream_status", Retryable: status == 429 || status >= 500,
			Message: fmt.Sprintf("kiro generateAssistantResponse HTTP %d: %s", status, detail)}
	}
}

// kiroNow is a test seam for the monthly reset calculation.
var kiroNow = time.Now

// contextWindowFull maps an empty turn with contextUsagePercentage >= 100.
func contextWindowFull(u kiroUsage) *kiroFailure {
	return promptTooLong(fmt.Sprintf("Kiro context window full (contextUsagePercentage=%.1f) and no output was produced", u.pct))
}
