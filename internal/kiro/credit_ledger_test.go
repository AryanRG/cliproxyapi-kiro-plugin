package kiro

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"testing"

	"github.com/xiaokui-dev/cliproxyapi-kiro-plugin/internal/config"
	"github.com/xiaokui-dev/cliproxyapi-kiro-plugin/internal/hostapi"
)

// An output-only frame used to clear every input counter and mark zero as exact.
func TestPartialTokenCounters(t *testing.T) {
	var u kiroUsage
	update := func(s string) { u.observe("metadataEvent", framePayload{TokenUsage: json.RawMessage(s)}) }
	update(`{"outputTokens":7}`)
	initial, source := u.anthropicUsage("model", 120, 9)
	if source != usageSourceEstimated || initial["input_tokens"] != 120 || initial["output_tokens"] != 7 || initial["cache_read_input_tokens"] != nil {
		t.Fatal(initial, source)
	}
	update(`{"cacheReadInputTokens":80}`)
	partial, _ := u.anthropicUsage("model", 120, 9)
	if partial["input_tokens"] != 40 || partial["cache_creation_input_tokens"] != nil {
		t.Fatal(partial)
	}
	update(`{"uncachedInputTokens":120,"cacheReadInputTokens":3000,"cacheWriteInputTokens":40,"outputTokens":7}`)
	update(`{"outputTokens":11,"cacheReadInputTokens":null}`)
	msg := claudeResponse{}
	if msg.applyUsage(&u) != usageSourceProvider || msg.Usage.InputTokens != 120 || msg.Usage.OutputTokens != 11 || *msg.Usage.CacheReadInputTokens != 3000 || *msg.Usage.CacheCreationInputTokens != 40 {
		t.Fatal(msg.Usage)
	}
	update(`{"uncachedInputTokens":0,"outputTokens":-1,"cacheWriteInputTokens":0}`)
	if n, src := u.inputTokens("model", 999); n != 0 || src != usageSourceProvider || *u.output != 11 || *u.cacheWrite != 0 {
		t.Fatal(u)
	}
}

func TestLedgerConcurrentIdempotentWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "usage #1.sqlite")
	a := creditAttempt{creditRequest: creditRequest{path: path, requestID: "req", model: "kiro-auto"}, attemptID: "duplicate"}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			entry := a
			u := kiroUsage{haveCredits: true}
			if i%2 != 0 {
				entry.attemptID = fmt.Sprint(i)
				u.haveCredits = false
			}
			if err := entry.write("completed", u); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var total, measured int
	var credits float64
	if err = db.QueryRow("SELECT count(*),count(credits),sum(credits) FROM kiro_requests").Scan(&total, &measured, &credits); err != nil || total != 9 || measured != 1 || credits != 0 {
		t.Fatal(total, measured, credits, err)
	}
}

func TestLedgerRecordsRetryAndAbsentMeter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credits.db")
	cfg, _ := json.Marshal(map[string][]byte{"config_yaml": []byte(fmt.Sprintf("credit_ledger_path: %q", path))})
	config.Apply(cfg)
	t.Cleanup(func() { config.Apply([]byte("{}")) })
	old := kiroHTTPDo
	t.Cleanup(func() { kiroHTTPDo = old })
	calls := 0
	kiroHTTPDo = func(hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		calls++
		body := encodeFrame("meteringEvent", `{"unit":"credit","usage":0.125}`)
		if calls == 2 {
			body = encodeFrame("assistantResponseEvent", `{"content":"OK"}`)
		}
		return &hostapi.HTTPResponse{StatusCode: 200, Headers: http.Header{"X-Amzn-Requestid": []string{fmt.Sprint(calls)}}, Body: body}, nil
	}
	if _, err := executeKiro(nonStreamRequest(t)); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var attempts, reqs, reported, empty, complete int
	var debit float64
	err = db.QueryRow(`SELECT count(*),count(distinct request_id),count(credits),sum(credits),sum(outcome='empty_response'),sum(outcome='completed' and credits is null) FROM kiro_requests`).Scan(&attempts, &reqs, &reported, &debit, &empty, &complete)
	if err != nil || attempts != 2 || reqs != 1 || reported != 1 || debit != 0.125 || empty != 1 || complete != 1 {
		t.Fatal(attempts, reqs, reported, debit, empty, complete, err)
	}
}
