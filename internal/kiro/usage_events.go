package kiro

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
)

// Token-usage provenance, reported in X-Kiro-Token-Usage.
const (
	usageSourceProvider   = "provider"           // metadataEvent/messageMetadataEvent tokenUsage
	usageSourceContextPct = "context-percentage" // contextUsageEvent x model context window
	usageSourceEstimated  = "estimated"          // UTF-8 bytes / 4
)

// kiroUsage accumulates usage-bearing events from one Kiro response.
type kiroUsage struct {
	haveTokens                              bool
	uncached, output, cacheRead, cacheWrite int
	haveOutput                              bool
	havePct                                 bool
	pct                                     float64
	credits                                 float64
	creditUnit                              string
	haveCredits                             bool
}

type kiroTokenUsage struct {
	UncachedInputTokens   *float64 `json:"uncachedInputTokens"`
	OutputTokens          *float64 `json:"outputTokens"`
	CacheReadInputTokens  *float64 `json:"cacheReadInputTokens"`
	CacheWriteInputTokens *float64 `json:"cacheWriteInputTokens"`
}

func isUsageEvent(kind string) bool {
	switch kind {
	case "metadataEvent", "messageMetadataEvent", "contextUsageEvent", "meteringEvent":
		return true
	}
	return false
}

func rawNumber(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return f, true
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if v, err := strconv.ParseFloat(s, 64); err == nil {
			return v, true
		}
	}
	return 0, false
}

func toInt(f *float64) int {
	if f == nil || *f < 0 || math.IsNaN(*f) || *f > 1e12 {
		return 0
	}
	return int(math.Round(*f))
}

// observe records a usage event. It returns true when the frame carried usage.
func (u *kiroUsage) observe(kind string, p framePayload) bool {
	seen := false
	for _, raw := range []json.RawMessage{p.TokenUsage, nestedTokenUsage(p.MetadataEvent), nestedTokenUsage(p.MessageMetadataEvent)} {
		var t kiroTokenUsage
		if len(raw) == 0 || json.Unmarshal(raw, &t) != nil {
			continue
		}
		if t.UncachedInputTokens == nil && t.OutputTokens == nil && t.CacheReadInputTokens == nil && t.CacheWriteInputTokens == nil {
			continue
		}
		// Later frames carry cumulative totals; the last one wins.
		u.haveTokens, seen = true, true
		u.uncached, u.cacheRead, u.cacheWrite = toInt(t.UncachedInputTokens), toInt(t.CacheReadInputTokens), toInt(t.CacheWriteInputTokens)
		if t.OutputTokens != nil {
			u.output, u.haveOutput = toInt(t.OutputTokens), true
		}
	}
	if pct, ok := rawNumber(p.ContextUsagePercentage); ok && pct >= 0 {
		u.pct, u.havePct, seen = pct, true, true
	}
	if kind == "meteringEvent" {
		if v, ok := rawNumber(p.Usage); ok && v >= 0 {
			u.credits += v
			u.haveCredits, seen = true, true
			u.creditUnit = firstNonEmptyStr(u.creditUnit, p.UnitPlural, p.Unit)
		}
	}
	return seen
}

func nestedTokenUsage(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var wrapper struct {
		TokenUsage json.RawMessage `json:"tokenUsage"`
	}
	if json.Unmarshal(raw, &wrapper) != nil {
		return nil
	}
	return wrapper.TokenUsage
}

// contextOverflow reports a full context window, which Kiro signals with
// contextUsagePercentage >= 100.
func (u *kiroUsage) contextOverflow() bool { return u.havePct && u.pct >= 100 }

// inputTokens returns (uncached input tokens, source) given the request-byte estimate.
func (u *kiroUsage) inputTokens(model string, estimate int) (int, string) {
	switch {
	case u.haveTokens:
		return u.uncached, usageSourceProvider
	case u.havePct && u.pct > 0:
		return int(math.Round(u.pct / 100 * float64(contextWindowFor(model)))), usageSourceContextPct
	default:
		return estimate, usageSourceEstimated
	}
}

func (u *kiroUsage) outputTokens(estimate int) int {
	if u.haveTokens && u.haveOutput {
		return u.output
	}
	return estimate
}

// anthropicUsage renders the Anthropic usage object for message_delta / non-stream responses.
func (u *kiroUsage) anthropicUsage(model string, inputEstimate, outputEstimate int) (map[string]any, string) {
	in, source := u.inputTokens(model, inputEstimate)
	out := map[string]any{"input_tokens": in, "output_tokens": u.outputTokens(outputEstimate)}
	if u.haveTokens {
		out["cache_read_input_tokens"] = u.cacheRead
		out["cache_creation_input_tokens"] = u.cacheWrite
	}
	return out, source
}

// usageHeaders describes token provenance and, when Kiro metered the call, the
// credit charge. Credits are account units, never tokens.
func usageHeaders(contentType, source string, u *kiroUsage) http.Header {
	h := http.Header{"Content-Type": {contentType}, "X-Kiro-Token-Usage": {source}}
	if source == usageSourceEstimated {
		h.Set("X-Kiro-Token-Estimator", "utf8-bytes-div4")
	}
	if u != nil && u.haveCredits {
		h.Set("X-Kiro-Credits", strconv.FormatFloat(u.credits, 'f', -1, 64))
		if u.creditUnit != "" {
			h.Set("X-Kiro-Credit-Unit", u.creditUnit)
		}
	}
	return h
}

// contextWindowFor resolves the context window used to convert
// contextUsagePercentage into tokens. It defers to contextWindowForModel
// (account-reported maxInputTokens, then documented fallbacks); when that only
// yields the generic default, known 1M models (kiro.rs
// get_context_window_size) use 1M for this conversion. This does not change
// the advertised catalogue limits.
func contextWindowFor(model string) int {
	limit := contextWindowForModel(model)
	if limit != defaultModelInputTokenLimit {
		return int(limit)
	}
	if _, discovered := discoveredContextWindow(model); discovered {
		return int(limit)
	}
	switch resolveKiroModel(model) {
	case "claude-fable-5.1", "claude-sonnet-5.5", "claude-sonnet-5", "claude-opus-5.5", "claude-opus-5",
		"claude-sonnet-4.6", "claude-opus-4.6", "claude-opus-4.7", "claude-opus-4.8":
		return 1_000_000
	}
	return int(limit)
}
