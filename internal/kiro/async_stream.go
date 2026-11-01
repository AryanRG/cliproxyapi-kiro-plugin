package kiro

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/xiaokui-dev/cliproxyapi-kiro-plugin/internal/hostapi"
	"github.com/xiaokui-dev/cliproxyapi-kiro-plugin/internal/wire"
)

type streamPolicy struct{ idle, total, heartbeat time.Duration }

var productionStreamPolicy = streamPolicy{6 * time.Minute, 30 * time.Minute, 10 * time.Second}
var activeStreams = struct {
	sync.Mutex
	jobs    map[*streamJob]struct{}
	stopped bool
}{jobs: map[*streamJob]struct{}{}}

type streamJob struct {
	cancel context.CancelFunc
	done   chan struct{}
}
type streamInput struct {
	payload []byte
	err     error
	done    bool
}

func ShutdownStreams() {
	activeStreams.Lock()
	activeStreams.stopped = true
	jobs := make([]*streamJob, 0, len(activeStreams.jobs))
	for job := range activeStreams.jobs {
		jobs = append(jobs, job)
		job.cancel()
	}
	activeStreams.Unlock()
	for _, job := range jobs {
		<-job.done
	}
}

func executeKiroStream(request []byte) ([]byte, error) {
	var req executorRequest
	if json.Unmarshal(request, &req) != nil {
		return wire.ErrorStatus("invalid_request", "Invalid Kiro streaming request", 400), nil
	}
	if req.StreamID == "" || req.HostCallbackID == "" {
		return wire.ErrorStatus("stream_callback_required", "Kiro requires the host asynchronous stream callbacks", 400), nil
	}
	var creq claudeRequest
	var cred kiroCredential
	if json.Unmarshal(req.Payload, &creq) != nil || json.Unmarshal(req.StorageJSON, &cred) != nil {
		return wire.ErrorStatus("invalid_request", "Invalid Kiro request or credential", 400), nil
	}
	if cred.AccessToken == "" {
		return wire.ErrorStatus("invalid_credential", "Kiro access token missing", 401), nil
	}
	model := firstNonEmptyStr(req.Model, creq.Model)
	controls, err := kiroRequestControls(creq, model)
	if err != nil {
		return wire.ErrorStatus("invalid_model_controls", err.Error(), 400), nil
	}
	region, err := resolveRegion(cred.Region, cred.IDCRegion)
	if err != nil {
		return wire.ErrorStatus("invalid_credential", "Invalid Kiro region", 400), nil
	}
	url, err := safeEndpoint(generateURLTemplate, region, "")
	if err != nil {
		return nil, err
	}
	cw, maps := buildCodeWhispererRequest(creq, model, cred)
	cw.AdditionalModelRequestFields = controls
	body, err := json.Marshal(cw)
	if err != nil {
		return nil, err
	}
	op, err := hostapi.OpenOperation(req.HostCallbackID)
	if err != nil || op == "" {
		return wire.ErrorStatus("stream_host_unavailable", "Kiro streaming requires CLIProxy host HTTP operation support", 502), nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	job := &streamJob{cancel: cancel, done: make(chan struct{})}
	activeStreams.Lock()
	if activeStreams.stopped {
		activeStreams.Unlock()
		cancel()
		_ = hostapi.CancelOperation(req.HostCallbackID, op)
		return wire.ErrorStatus("plugin_stopping", "Kiro plugin is stopping", 503), nil
	}
	activeStreams.jobs[job] = struct{}{}
	activeStreams.Unlock()
	httpReq := hostapi.HTTPRequest{HostCallbackID: req.HostCallbackID, Method: "POST", URL: url, Headers: kiroRequestHeaders(cred), Body: body}
	go func() {
		defer func() { activeStreams.Lock(); delete(activeStreams.jobs, job); activeStreams.Unlock(); close(job.done) }()
		runKiroStream(ctx, req.StreamID, op, httpReq, model, estimateTokens(len(req.Payload)), maps, productionStreamPolicy)
	}()
	return wire.OK(executorStreamResponse{Headers: estimatedUsageHeaders("text/event-stream")})
}

func runKiroStream(parent context.Context, id, op string, request hostapi.HTTPRequest, model string, inputTokens int, maps *toolNameMaps, policy streamPolicy) {
	ctx, cancel := context.WithTimeout(parent, policy.total)
	code := "stream_interrupted"
	defer func() {
		if recover() != nil {
			code = "stream_internal_error"
		}
		cancel()
		_ = hostapi.CancelOperation(request.HostCallbackID, op)
		if !regexp.MustCompile(`^[a-z0-9_]{0,80}$`).MatchString(code) {
			code = "stream_write_or_decode_failed"
		}
		_ = hostapi.StreamClose(id, code)
	}()
	emit := func(b []byte) error { return hostapi.StreamEmit(id, b) }
	message, err := newIncrementalMessage(model, inputTokens, maps, emit)
	if err != nil {
		code = "client_disconnected"
		return
	}
	parts := make(chan streamInput, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		send := func(p streamInput) bool {
			select {
			case parts <- p:
				return true
			case <-ctx.Done():
				return false
			}
		}
		defer func() {
			if recover() != nil {
				send(streamInput{err: fmt.Errorf("stream_reader_failed")})
			}
		}()
		up, err := hostapi.HTTPDoStream(request, op)
		if err != nil {
			send(streamInput{err: fmt.Errorf("upstream_connect_failed")})
			return
		}
		if up.StreamID != "" {
			defer hostapi.HTTPStreamClose(up.StreamID)
		}
		if up.StatusCode < 200 || up.StatusCode >= 300 {
			send(streamInput{err: fmt.Errorf("upstream_http_%d", up.StatusCode)})
			return
		}
		if up.StreamID == "" {
			send(streamInput{err: fmt.Errorf("missing_upstream_stream")})
			return
		}
		for {
			chunk, err := hostapi.HTTPStreamRead(up.StreamID)
			if err != nil || chunk.Error != "" {
				send(streamInput{err: fmt.Errorf("upstream_read_failed")})
				return
			}
			if !send(streamInput{payload: chunk.Payload, done: chunk.Done}) || chunk.Done {
				return
			}
		}
	}()
	defer func() { cancel(); _ = hostapi.CancelOperation(request.HostCallbackID, op); <-readerDone }()
	heartbeat := time.NewTicker(policy.heartbeat)
	defer heartbeat.Stop()
	idle := time.NewTimer(policy.idle)
	defer idle.Stop()
	decoder := frameDecoder{}
	received := 0
	for {
		select {
		case <-ctx.Done():
			code = "stream_cancelled_or_deadline"
			return
		case <-idle.C:
			code = "upstream_idle_timeout"
			return
		case <-heartbeat.C:
			if message.event("ping", map[string]any{}) != nil {
				code = "client_disconnected"
				return
			}
		case p := <-parts:
			if p.err != nil {
				code = p.err.Error()
				return
			}
			if len(p.payload) > 0 {
				if !idle.Stop() {
					select {
					case <-idle.C:
					default:
					}
				}
				idle.Reset(policy.idle)
				received += len(p.payload)
				if received > 64*1024*1024 {
					code = "stream_size_limit"
					return
				}
				if err := decoder.feed(p.payload, message.consume); err != nil {
					code = err.Error()
					return
				}
			}
			if p.done {
				if err := decoder.finish(); err != nil {
					code = err.Error()
					return
				}
				if err := message.finish(); err != nil {
					code = err.Error()
					return
				}
				code = ""
				return
			}
		}
	}
}
