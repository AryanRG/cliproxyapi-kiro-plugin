package kiro

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/xiaokui-dev/cliproxyapi-kiro-plugin/internal/hostapi"
	"github.com/xiaokui-dev/cliproxyapi-kiro-plugin/internal/wire"
)

// encodeFrameWith builds a CRC-valid frame with arbitrary string headers (name, value pairs).
func encodeFrameWith(payload string, headers ...string) []byte {
	var h []byte
	for i := 0; i+1 < len(headers); i += 2 {
		h = append(h, byte(len(headers[i])))
		h = append(h, headers[i]...)
		h = append(h, 7)
		h = binary.BigEndian.AppendUint16(h, uint16(len(headers[i+1])))
		h = append(h, headers[i+1]...)
	}
	total := 12 + len(h) + len(payload) + 4
	f := binary.BigEndian.AppendUint32(nil, uint32(total))
	f = binary.BigEndian.AppendUint32(f, uint32(len(h)))
	f = binary.BigEndian.AppendUint32(f, crc32.ChecksumIEEE(f[:8]))
	f = append(append(f, h...), payload...)
	return binary.BigEndian.AppendUint32(f, crc32.ChecksumIEEE(f))
}

func exceptionFrame(kind, payload string) []byte {
	return encodeFrameWith(payload, ":message-type", "exception", ":exception-type", kind)
}

const providerUsageFrame = `{"tokenUsage":{"uncachedInputTokens":120,"outputTokens":7,"cacheReadInputTokens":3000,"cacheWriteInputTokens":40}}`

func TestParseUsageEvents(t *testing.T) {
	var body []byte
	body = append(body, encodeFrame("assistantResponseEvent", `{"content":"hi"}`)...)
	body = append(body, encodeFrame("messageMetadataEvent", `{"messageMetadataEvent":{"tokenUsage":{"uncachedInputTokens":1,"outputTokens":1}}}`)...)
	body = append(body, encodeFrame("metadataEvent", providerUsageFrame)...)
	body = append(body, encodeFrame("contextUsageEvent", `{"contextUsagePercentage":12.5}`)...)
	body = append(body, encodeFrame("meteringEvent", `{"unit":"credit","unitPlural":"credits","usage":0.25}`)...)
	body = append(body, encodeFrame("meteringEvent", `{"unit":"credit","usage":"0.5"}`)...)
	events, u, fail := parseEventStreamResponse(body)
	if fail != nil || len(events) != 1 {
		t.Fatal(fail, events)
	}
	if !u.haveTokens || u.uncached != 120 || u.output != 7 || u.cacheRead != 3000 || u.cacheWrite != 40 {
		t.Fatalf("%+v", u)
	}
	if !u.havePct || u.pct != 12.5 || u.credits != 0.75 || u.creditUnit != "credits" {
		t.Fatalf("%+v", u)
	}
	// Unexpected shapes never fail decoding.
	var odd kiroUsage
	if odd.observe("metadataEvent", framePayload{TokenUsage: json.RawMessage(`"x"`), Usage: json.RawMessage(`{}`)}) {
		t.Fatal("garbage counted as usage")
	}
}

func TestUsageInputTokenSources(t *testing.T) {
	var u kiroUsage
	if n, src := u.inputTokens("claude-sonnet-4-5", 55); n != 55 || src != usageSourceEstimated {
		t.Fatal(n, src)
	}
	u.observe("contextUsageEvent", framePayload{ContextUsagePercentage: json.RawMessage(`10`)})
	if n, src := u.inputTokens("claude-sonnet-4-5", 55); n != 20000 || src != usageSourceContextPct {
		t.Fatal(n, src)
	}
	if n, _ := u.inputTokens("claude-opus-4.8", 55); n != 100000 {
		t.Fatal("1M window not applied", n)
	}
	rememberContextWindow("claude-test-9.9", "claude-test-9-9", 300000)
	if n, _ := u.inputTokens("claude-test-9-9", 55); n != 30000 {
		t.Fatal("catalogue window not applied", n)
	}
	u.observe("metadataEvent", framePayload{TokenUsage: json.RawMessage(`{"uncachedInputTokens":9,"outputTokens":4}`)})
	if n, src := u.inputTokens("claude-sonnet-4-5", 55); n != 9 || src != usageSourceProvider {
		t.Fatal(n, src)
	}
}

func nonStreamRequest(t *testing.T) []byte {
	t.Helper()
	payload, _ := json.Marshal(claudeRequest{Model: "claude-sonnet-4-5", Messages: []claudeMessage{{Role: "user", Content: json.RawMessage(`"hello"`)}}})
	storage, _ := json.Marshal(kiroCredential{AccessToken: "test-access-token"})
	raw, err := json.Marshal(executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{Model: "claude-sonnet-4-5", Payload: payload, StorageJSON: storage}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func stubHTTP(t *testing.T, status int, body []byte) *int {
	t.Helper()
	old := kiroHTTPDo
	t.Cleanup(func() { kiroHTTPDo = old })
	calls := 0
	kiroHTTPDo = func(hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		calls++
		return &hostapi.HTTPResponse{StatusCode: status, Body: body}, nil
	}
	return &calls
}

func TestNonStreamReportsProviderUsageAndCredits(t *testing.T) {
	body := append(encodeFrame("assistantResponseEvent", `{"content":"hi"}`), encodeFrame("metadataEvent", providerUsageFrame)...)
	body = append(body, encodeFrame("meteringEvent", `{"unit":"credit","unitPlural":"credits","usage":0.1}`)...)
	stubHTTP(t, 200, body)
	out, err := executeKiro(nonStreamRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	var env wire.Envelope
	var resp pluginapi.ExecutorResponse
	if json.Unmarshal(out, &env) != nil || !env.OK || json.Unmarshal(env.Result, &resp) != nil {
		t.Fatal(string(out))
	}
	var msg claudeResponse
	_ = json.Unmarshal(resp.Payload, &msg)
	if msg.Usage.InputTokens != 120 || msg.Usage.OutputTokens != 7 || *msg.Usage.CacheReadInputTokens != 3000 || *msg.Usage.CacheCreationInputTokens != 40 {
		t.Fatalf("%+v", msg.Usage)
	}
	if resp.Headers.Get("X-Kiro-Token-Usage") != "provider" || resp.Headers.Get("X-Kiro-Token-Estimator") != "" || resp.Headers.Get("X-Kiro-Credits") != "0.1" {
		t.Fatal(resp.Headers)
	}
}

func envelopeError(t *testing.T, raw []byte) wire.EnvelopeError {
	t.Helper()
	var env wire.Envelope
	if json.Unmarshal(raw, &env) != nil || env.OK || env.Error == nil {
		t.Fatalf("expected error envelope: %s", raw)
	}
	return *env.Error
}

func assertPromptTooLong(t *testing.T, e wire.EnvelopeError) {
	t.Helper()
	if e.HTTPStatus != 400 || e.Code != "invalid_request_error" || !strings.HasPrefix(e.Message, promptTooLongPrefix) {
		t.Fatalf("%+v", e)
	}
}

func TestNonStreamContextOverflow(t *testing.T) {
	cases := map[string]struct {
		status int
		body   []byte
	}{
		"http400":   {400, []byte(`{"message":"Input is too long for requested model.","reason":"CONTENT_LENGTH_EXCEEDS_THRESHOLD"}`)},
		"exception": {200, append(encodeFrame("assistantResponseEvent", `{"content":""}`), exceptionFrame("ValidationException", `{"message":"Input is too long."}`)...)},
		"pct100":    {200, encodeFrame("contextUsageEvent", `{"contextUsagePercentage":100}`)},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			calls := stubHTTP(t, c.status, c.body)
			_, env, err := fetchKiroEvents(nonStreamRequest(t))
			if err != nil {
				t.Fatal(err)
			}
			assertPromptTooLong(t, envelopeError(t, env))
			if *calls != 1 {
				t.Fatal("overflow retried", *calls)
			}
		})
	}
}

func TestErrorClassification(t *testing.T) {
	old := kiroNow
	kiroNow = func() time.Time { return time.Date(2026, 11, 30, 23, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { kiroNow = old })
	cases := []struct {
		status     int
		body       string
		wantStatus int
		wantMsg    string
	}{
		{400, `{"message":"quota","reason":"MONTHLY_REQUEST_COUNT"}`, 429, "monthly request limit"},
		{403, `{"message":"User is not authorized to make this call."}`, 403, "Kiro account blocked/unauthorized"},
		{403, `{"message":"The bearer token included in the request is invalid. Bearer abcdefghijklmnopqrstuvwxyz0123"}`, 403, "HTTP 403"},
		{429, `{"message":"busy","reason":"INSUFFICIENT_MODEL_CAPACITY"}`, 503, "retry later"},
		{429, `{"message":"slow down","reason":"USER_REQUEST_RATE_EXCEEDED"}`, 429, "retry later"},
		{500, `oops`, 500, "HTTP 500: oops"},
	}
	for _, c := range cases {
		f := classifyKiroFailure(c.status, []byte(c.body))
		if f.Status != c.wantStatus || !strings.Contains(f.Message, c.wantMsg) || strings.Contains(f.Message, "abcdefghijklmnop") {
			t.Fatalf("%d %s -> %+v", c.status, c.body, f)
		}
	}
	monthly := classifyKiroFailure(400, []byte(`{"reason":"MONTHLY_REQUEST_COUNT"}`))
	if monthly.RetryAfter != 3601 || !strings.Contains(monthly.Message, "2026-12-01") {
		t.Fatalf("%+v", monthly)
	}
	e := envelopeError(t, monthly.envelope())
	if e.RetryAfterSeconds != 3601 || e.HTTPStatus != 429 {
		t.Fatalf("%+v", e)
	}
}

func streamEvents(h *fakeStreamHost) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	all := ""
	for _, f := range h.frames {
		all += string(f)
	}
	return all
}

func TestStreamProviderUsageInMessageDelta(t *testing.T) {
	h := installStreamHost(t)
	done := startFakeStream(h, context.Background(), testStreamPolicy)
	h.chunks <- hostapi.HTTPChunk{Payload: encodeFrame("metadataEvent", providerUsageFrame)}
	start := h.waitEvent(t, "message_start")
	if !strings.Contains(string(start), `"input_tokens":120`) || !strings.Contains(string(start), `"cache_read_input_tokens":3000`) {
		t.Fatal(string(start))
	}
	h.chunks <- hostapi.HTTPChunk{Payload: encodeFrame("assistantResponseEvent", `{"content":"ok"}`), Done: true}
	delta := h.waitEvent(t, "message_delta")
	for _, want := range []string{`"input_tokens":120`, `"output_tokens":7`, `"cache_creation_input_tokens":40`} {
		if !strings.Contains(string(delta), want) {
			t.Fatal(string(delta))
		}
	}
	if code := h.waitClose(t); code != "" {
		t.Fatal(code)
	}
	<-done
}

func TestStreamContextOverflow(t *testing.T) {
	cases := map[string]func(h *fakeStreamHost){
		"http400": func(h *fakeStreamHost) {
			h.status, h.errBody = 400, []byte(`{"message":"Input is too long for requested model.","reason":"CONTENT_LENGTH_EXCEEDS_THRESHOLD"}`)
		},
		"exception": func(h *fakeStreamHost) {
			h.chunks <- hostapi.HTTPChunk{Payload: exceptionFrame("ValidationException", `{"message":"Input is too long."}`)}
		},
		"pct100": func(h *fakeStreamHost) {
			h.chunks <- hostapi.HTTPChunk{Payload: encodeFrame("contextUsageEvent", `{"contextUsagePercentage":100}`), Done: true}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			h := installStreamHost(t)
			setup(h)
			done := startFakeStream(h, context.Background(), testStreamPolicy)
			if code := h.waitClose(t); code != "invalid_request_error" {
				t.Fatal(code)
			}
			<-done
			detail := <-h.closeMsg
			if detail["http_status"] != float64(400) || !strings.HasPrefix(detail["message"].(string), promptTooLongPrefix) {
				t.Fatal(detail)
			}
			if all := streamEvents(h); all != "" {
				t.Fatal("message started before error:", all)
			}
		})
	}
}

func TestStreamExceptionAfterContentEmitsErrorEvent(t *testing.T) {
	h := installStreamHost(t)
	done := startFakeStream(h, context.Background(), testStreamPolicy)
	h.chunks <- hostapi.HTTPChunk{Payload: encodeFrame("assistantResponseEvent", `{"content":"partial"}`)}
	h.waitEvent(t, "content_block_delta")
	h.chunks <- hostapi.HTTPChunk{Payload: exceptionFrame("ValidationException", `{"message":"CONTENT_LENGTH_EXCEEDS_THRESHOLD"}`)}
	ev := h.waitEvent(t, "error")
	if !strings.Contains(string(ev), `"type":"invalid_request_error"`) || !strings.Contains(string(ev), `"message":"prompt is too long: `) {
		t.Fatal(string(ev))
	}
	if code := h.waitClose(t); code != "invalid_request_error" {
		t.Fatal(code)
	}
	<-done
	if strings.Contains(streamEvents(h), "message_stop") {
		t.Fatal("error reported success")
	}
}
