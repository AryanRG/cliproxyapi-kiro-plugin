package kiro

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiaokui-dev/cliproxyapi-kiro-plugin/internal/hostapi"
	"github.com/xiaokui-dev/cliproxyapi-kiro-plugin/internal/wire"
)

type fakeStreamHost struct {
	mu        sync.Mutex
	chunks    chan hostapi.HTTPChunk
	emitted   chan []byte
	closed    chan string
	cancelled chan struct{}
	once      sync.Once
	frames    [][]byte
	opens     int
	reads     int
	closes    int
	failEmit  bool
	status    int    // do_stream status; 0 means 200
	errBody   []byte // body served for a non-2xx status
	reading   chan struct{}
	readOnce  sync.Once
	closeMsg  chan map[string]any
}

func installStreamHost(t *testing.T) *fakeStreamHost {
	t.Helper()
	h := &fakeStreamHost{chunks: make(chan hostapi.HTTPChunk, 32), emitted: make(chan []byte, 128), closed: make(chan string, 1), cancelled: make(chan struct{}), reading: make(chan struct{}), closeMsg: make(chan map[string]any, 1)}
	old := hostapi.HostCall
	hostapi.HostCall = h.call
	t.Cleanup(func() { hostapi.HostCall = old })
	return h
}
func (h *fakeStreamHost) call(method string, body []byte) ([]byte, error) {
	var req struct {
		Payload []byte `json:"payload"`
		Detail  struct {
			Code string `json:"code"`
		} `json:"error_detail"`
	}
	_ = json.Unmarshal(body, &req)
	switch method {
	case "host.http.operation_open":
		return wire.OK(map[string]string{"operation_id": "op"})
	case "host.http.cancel":
		h.once.Do(func() { close(h.cancelled) })
		return wire.OK(map[string]any{})
	case "host.http.do_stream":
		h.mu.Lock()
		h.opens++
		status := h.status
		h.mu.Unlock()
		if status != 0 {
			return wire.OK(hostapi.HTTPStream{StatusCode: status, StreamID: "err"})
		}
		return wire.OK(hostapi.HTTPStream{StatusCode: 200, StreamID: "up"})
	case "host.http.stream_read":
		h.mu.Lock()
		h.reads++
		h.mu.Unlock()
		if strings.Contains(string(body), `"err"`) {
			return wire.OK(hostapi.HTTPChunk{Payload: h.errBody, Done: true})
		}
		h.readOnce.Do(func() { close(h.reading) })
		select {
		case c := <-h.chunks:
			return wire.OK(c)
		case <-h.cancelled:
			return nil, errors.New("secret-canary: cancelled")
		}
	case "host.http.stream_close":
		h.mu.Lock()
		h.closes++
		h.mu.Unlock()
		return wire.OK(map[string]any{})
	case "host.stream.emit":
		if strings.HasPrefix(string(req.Payload), "event:") {
			return wire.OK(map[string]any{})
		}
		var event map[string]any
		_ = json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(string(req.Payload), "data:"))), &event)
		req.Payload = append([]byte("event: "+fmt.Sprint(event["type"])+"\n"), req.Payload...)
		h.mu.Lock()
		bad := h.failEmit
		if !bad {
			h.frames = append(h.frames, req.Payload)
		}
		h.mu.Unlock()
		if bad {
			return nil, errors.New("consumer closed")
		}
		h.emitted <- req.Payload
		return wire.OK(map[string]any{})
	case "host.stream.close":
		var full struct {
			Detail map[string]any `json:"error_detail"`
		}
		_ = json.Unmarshal(body, &full)
		select {
		case h.closeMsg <- full.Detail:
		default:
		}
		h.closed <- req.Detail.Code
		return wire.OK(map[string]any{})
	default:
		return nil, fmt.Errorf("unexpected callback %s", method)
	}
}
func (h *fakeStreamHost) waitEvent(t *testing.T, kind string) []byte {
	t.Helper()
	for {
		select {
		case p := <-h.emitted:
			if sseEventType(p) == kind {
				return p
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("missing %s", kind)
		}
	}
}
func (h *fakeStreamHost) waitReading(t *testing.T) {
	t.Helper()
	select {
	case <-h.reading:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream not read")
	}
}
func (h *fakeStreamHost) waitClose(t *testing.T) string {
	t.Helper()
	select {
	case code := <-h.closed:
		return code
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not close")
		return ""
	}
}
func startFakeStream(h *fakeStreamHost, ctx context.Context, policy streamPolicy) chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		runKiroStream(ctx, "down", "op", hostapi.HTTPRequest{HostCallbackID: "cb"}, "claude-opus-5.5", 10, &toolNameMaps{}, policy)
	}()
	return done
}

var testStreamPolicy = streamPolicy{time.Second, 3 * time.Second, 100 * time.Millisecond}

func TestStreamEmitsBeforeUpstreamEOF(t *testing.T) {
	h := installStreamHost(t)
	done := startFakeStream(h, context.Background(), testStreamPolicy)
	frame := encodeFrame("assistantResponseEvent", `{"content":"Hello café 🌍"}`)
	for i := 0; i < len(frame); i += 7 {
		end := i + 7
		if end > len(frame) {
			end = len(frame)
		}
		h.chunks <- hostapi.HTTPChunk{Payload: frame[i:end]}
	}
	first := h.waitEvent(t, "content_block_delta")
	if !strings.Contains(string(first), "Hello café 🌍") {
		t.Fatal(string(first))
	}
	select {
	case <-done:
		t.Fatal("finished before EOF")
	default:
	}
	h.chunks <- hostapi.HTTPChunk{Payload: encodeFrame("assistantResponseEvent", `{"content":"!"}`), Done: true}
	if code := h.waitClose(t); code != "" {
		t.Fatal(code)
	}
	<-done
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.opens != 1 || h.closes != 1 {
		t.Fatalf("opens=%d closes=%d", h.opens, h.closes)
	}
}
func TestStreamCancellationClosesUpstreamWithoutReplay(t *testing.T) {
	h := installStreamHost(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := startFakeStream(h, ctx, testStreamPolicy)
	h.waitReading(t)
	cancel()
	if code := h.waitClose(t); code != "stream_cancelled_or_deadline" {
		t.Fatal(code)
	}
	<-done
	select {
	case <-h.cancelled:
	default:
		t.Fatal("HTTP operation not cancelled")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range h.frames {
		if sseEventType(p) == "message_stop" {
			t.Fatal("cancellation reported success")
		}
	}
	if h.opens > 1 {
		t.Fatal("request replayed")
	}
}
func TestStreamTruncationAndUpstreamErrorAreNotSuccess(t *testing.T) {
	for _, mode := range []string{"truncated", "crc", "upstream", "empty", "tool"} {
		t.Run(mode, func(t *testing.T) {
			h := installStreamHost(t)
			done := startFakeStream(h, context.Background(), testStreamPolicy)
			frame := encodeFrame("assistantResponseEvent", `{"content":"partial"}`)
			switch mode {
			case "truncated":
				h.chunks <- hostapi.HTTPChunk{Payload: frame[:len(frame)-1], Done: true}
			case "crc":
				frame[len(frame)-1] ^= 1
				h.chunks <- hostapi.HTTPChunk{Payload: frame, Done: true}
			case "upstream":
				h.chunks <- hostapi.HTTPChunk{Payload: frame}
				h.chunks <- hostapi.HTTPChunk{Error: "secret-canary: upstream error", Done: true}
			case "empty":
				h.chunks <- hostapi.HTTPChunk{Done: true}
			case "tool":
				h.chunks <- hostapi.HTTPChunk{Payload: encodeFrame("toolUseEvent", `{"name":"exec","toolUseId":"t","input":"{broken","stop":true}`), Done: true}
			}
			code := h.waitClose(t)
			if code == "" || strings.Contains(code, "secret-canary") {
				t.Fatal(code)
			}
			<-done
			h.mu.Lock()
			defer h.mu.Unlock()
			for _, p := range h.frames {
				if sseEventType(p) == "message_stop" {
					t.Fatal("failure reported success")
				}
			}
			if h.opens != 1 {
				t.Fatal("request replayed")
			}
		})
	}
}
func TestStreamHeartbeatAndIdleDeadline(t *testing.T) {
	h := installStreamHost(t)
	done := startFakeStream(h, context.Background(), streamPolicy{80 * time.Millisecond, time.Second, 10 * time.Millisecond})
	h.chunks <- hostapi.HTTPChunk{Payload: encodeFrame("assistantResponseEvent", `{"content":"x"}`)}
	h.waitEvent(t, "ping")
	if code := h.waitClose(t); code != "upstream_idle_timeout" {
		t.Fatal(code)
	}
	<-done
}
func TestStreamDownstreamFailureCancelsReader(t *testing.T) {
	h := installStreamHost(t)
	done := startFakeStream(h, context.Background(), testStreamPolicy)
	h.chunks <- hostapi.HTTPChunk{Payload: encodeFrame("assistantResponseEvent", `{"content":"x"}`)}
	h.waitEvent(t, "content_block_delta")
	h.mu.Lock()
	h.failEmit = true
	h.mu.Unlock()
	if code := h.waitClose(t); code != "client_disconnected" {
		t.Fatal(code)
	}
	<-done
}
func TestIncrementalInterleavedToolsAndReasoning(t *testing.T) {
	var frames [][]byte
	s, err := newIncrementalMessage("claude-opus-5.5", 10, &toolNameMaps{}, func(b []byte) error { frames = append(frames, b); return nil })
	if err != nil {
		t.Fatal(err)
	}
	inputs := []struct{ kind, body string }{
		{"reasoningContentEvent", `{"text":"Planning a test."}`},
		{"assistantResponseEvent", `{"content":"Working. "}`},
		{"toolUseEvent", `{"name":"one","toolUseId":"a","input":"{\"x\":"}`},
		{"toolUseEvent", `{"name":"two","toolUseId":"b","input":"{\"y\":2}","stop":true}`},
		{"toolUseEvent", `{"toolUseId":"a","input":"1}"}`},
		{"toolUseEvent", `{"toolUseId":"a","stop":true}`},
	}
	for _, i := range inputs {
		var p framePayload
		if err := json.Unmarshal([]byte(i.body), &p); err != nil {
			t.Fatal(err)
		}
		if err := s.consume(i.kind, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.finish(); err != nil {
		t.Fatal(err)
	}
	all := ""
	for _, f := range frames {
		all += string(f)
	}
	if !strings.Contains(all, "thinking_delta") || !strings.Contains(all, `"stop_reason":"tool_use"`) {
		t.Fatal(all)
	}
	if s.toolCount != 2 {
		t.Fatal(s.toolCount)
	}
}
func TestIncrementalBracketToolsDoNotDuplicateStructuredCalls(t *testing.T) {
	s, _ := newIncrementalMessage("m", 1, &toolNameMaps{}, func([]byte) error { return nil })
	for _, text := range []string{"hello [Cal", "led one with args: {\"x\":1}] tail"} {
		if err := s.content(text, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.consume("toolUseEvent", framePayload{Name: "one", ToolUseID: "real", Input: json.RawMessage(`{"x":1}`), Stop: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.finish(); err != nil {
		t.Fatal(err)
	}
	if s.toolCount != 1 {
		t.Fatal(s.toolCount)
	}
}
func TestFrameDecoderEverySplit(t *testing.T) {
	frame := encodeFrame("assistantResponseEvent", `{"content":"hello 🌍"}`)
	for split := 0; split <= len(frame); split++ {
		d := frameDecoder{}
		count := 0
		consume := func(kind string, p framePayload) error {
			count++
			if kind != "assistantResponseEvent" || p.Content != "hello 🌍" {
				t.Fatal(kind, p.Content)
			}
			return nil
		}
		if err := d.feed(frame[:split], consume); err != nil {
			t.Fatal(err)
		}
		if err := d.feed(frame[split:], consume); err != nil {
			t.Fatal(err)
		}
		if err := d.finish(); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatal(count)
		}
	}
}

func TestExecutorAsyncReturnsAndQuiesces(t *testing.T) {
	h := installStreamHost(t)
	raw := []byte(`{"Model":"claude-opus-5.5","Payload":"eyJtZXNzYWdlcyI6W3sicm9sZSI6InVzZXIiLCJjb250ZW50IjoiaGkifV19","StorageJSON":"eyJhY2Nlc3NUb2tlbiI6InRlc3QifQ==","stream_id":"down","host_callback_id":"cb"}`)
	result, err := executeKiroStream(raw)
	if err != nil {
		t.Fatal(err)
	}
	var env wire.Envelope
	if json.Unmarshal(result, &env) != nil || !env.OK {
		t.Fatal(string(result))
	}
	h.waitReading(t)
	ShutdownStreams()
	if code := h.waitClose(t); code != "stream_cancelled_or_deadline" {
		t.Fatal(code)
	}
	activeStreams.Lock()
	remaining := len(activeStreams.jobs)
	activeStreams.Unlock()
	if remaining != 0 {
		t.Fatal(remaining)
	}
	rejected, _ := executeKiroStream(raw)
	if json.Unmarshal(rejected, &env) != nil || env.OK {
		t.Fatal("quiesced plugin accepted request")
	}
	if _, err := HandleMethod("plugin.register", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	activeStreams.Lock()
	stopped := activeStreams.stopped
	activeStreams.Unlock()
	if stopped {
		t.Fatal("registration did not reactivate drained plugin")
	}
}
func TestToolFingerprintPreservesNumbersAndNormalizesNestedKeys(t *testing.T) {
	a, _ := toolFingerprint("x", `{"nested":{"a":1,"b":2},"id":9007199254740993}`)
	b, _ := toolFingerprint("x", `{"id":9007199254740993,"nested":{"b":2,"a":1}}`)
	c, _ := toolFingerprint("x", `{"id":9007199254740992,"nested":{"b":2,"a":1}}`)
	if a != b || a == c {
		t.Fatal("unsafe argument deduplication")
	}
}
