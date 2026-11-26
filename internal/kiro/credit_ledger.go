package kiro

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/xiaokui-dev/cliproxyapi-kiro-plugin/internal/config"
)

// One logical executor call can make multiple separately metered upstream attempts.
type creditRequest struct {
	path, requestID, sessionHash, accountHash, model string
	stream                                           bool
}

func creditRequestFor(req executorRequest, model string, stream bool) creditRequest {
	path := config.Get().CreditLedgerPath
	if path == "" {
		return creditRequest{}
	}
	hostID, _ := req.Metadata["request_id"].(string)
	if hostID == "" {
		hostID = req.Headers.Get("X-Request-Id")
	}
	if hostID == "" {
		hostID = uuidV4()
	}
	session, _ := req.Metadata["canonical_session_id"].(string)
	if session == "" {
		session, _ = req.Metadata["derived_session_id"].(string)
	}
	if session == "" {
		session = req.Headers.Get("X-Session-Id")
	}
	var sessionHash, accountHash string
	if session != "" {
		sessionHash = sha256Hex(session)
	}
	if req.AuthID != "" {
		accountHash = sha256Hex(req.AuthID)
	}
	return creditRequest{path: path, requestID: hostID, sessionHash: sessionHash, accountHash: accountHash, model: model, stream: stream}
}

type creditAttempt struct {
	creditRequest
	attemptID, upstreamID string
}

func (a creditAttempt) record(outcome string, u kiroUsage) {
	if a.path == "" {
		return
	}
	if err := a.write(outcome, u); err != nil {
		// A storage error must never replay a billed inference.
		log.Printf("kiro credit ledger: could not save attempt %s: %v", a.attemptID, err)
	}
}

func (a creditAttempt) write(outcome string, u kiroUsage) error {
	if !filepath.IsAbs(a.path) {
		return fmt.Errorf("credit_ledger_path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(a.path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(a.path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	path := filepath.ToSlash(a.path)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	uri := url.URL{Scheme: "file", Path: path, RawQuery: "_txlock=immediate&_busy_timeout=5000"}
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		return err
	}
	defer db.Close()
	// Schema and record commit together. SQLite serializes writers across plugin reloads.
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`CREATE TABLE IF NOT EXISTS kiro_requests (
 attempt_id TEXT PRIMARY KEY, request_id TEXT NOT NULL, upstream_request_id TEXT,
 finished_at TEXT NOT NULL, model TEXT NOT NULL, streamed INTEGER NOT NULL,
 account_hash TEXT, session_hash TEXT, outcome TEXT NOT NULL,
 credits REAL, context_percentage REAL)`)
	if err != nil {
		return err
	}
	var credits, pct *float64
	if u.haveCredits {
		credits = &u.credits
	}
	if u.havePct {
		pct = &u.pct
	}
	_, err = tx.Exec(`INSERT INTO kiro_requests(attempt_id,request_id,upstream_request_id,finished_at,model,streamed,account_hash,session_hash,outcome,credits,context_percentage)
 VALUES(?,?,NULLIF(?,''),?,?,?,NULLIF(?,''),NULLIF(?,''),?,?,?) ON CONFLICT(attempt_id) DO NOTHING`,
		a.attemptID, a.requestID, a.upstreamID, time.Now().UTC().Format(time.RFC3339Nano), a.model, a.stream, a.accountHash, a.sessionHash, outcome, credits, pct)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func upstreamRequestID(headers http.Header) string {
	return headers.Get("X-Amzn-Requestid")
}
