package kiro

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
)

// Token-usage provenance, reported in X-Kiro-Token-Usage.
const (
	usageSourceProvider   = "provider"           // metadataEvent/messageMetadataEvent tokenUsage
	usageSourceContextPct = "context-percentage" // contextUsageEvent x model context window
	usageSourceEstimated  = "estimated"          // local text-and-image estimate
)

// kiroUsage accumulates usage-bearing events from one Kiro response.
type kiroUsage struct {
	uncached, output, cacheRead, cacheWrite *int
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
	if string(raw) != "null" && json.Unmarshal(raw, &f) == nil {
		return f, true
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if v, err := strconv.ParseFloat(s, 64); err == nil && !math.IsNaN(v) && !math.IsInf(v, 0) {
			return v, true
		}
	}
	return 0, false
}

// Usage frames may carry only some cumulative counters. A missing or invalid
// field must leave its previous observation intact, including a reported zero.
func observeCounter(previous **int, value *float64) {
	if value != nil && *value >= 0 && *value <= 1e12 && !math.IsNaN(*value) {
		n := int(math.Round(*value))
		*previous = &n
	}
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
		observeCounter(&u.uncached, t.UncachedInputTokens)
		observeCounter(&u.output, t.OutputTokens)
		observeCounter(&u.cacheRead, t.CacheReadInputTokens)
		observeCounter(&u.cacheWrite, t.CacheWriteInputTokens)
		seen = true
	}
	if pct, ok := rawNumber(p.ContextUsagePercentage); ok && pct >= 0 {
		u.pct, u.havePct, seen = pct, true, true
	}
	unit := firstNonEmptyStr(p.UnitPlural, p.Unit)
	if kind == "meteringEvent" && (unit == "" || strings.EqualFold(unit, "credit") || strings.EqualFold(unit, "credits")) {
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
	if u.uncached != nil {
		return *u.uncached, usageSourceProvider
	}
	source := usageSourceEstimated
	if u.havePct && u.pct > 0 {
		estimate = int(math.Round(u.pct / 100 * float64(contextWindowFor(model))))
		source = usageSourceContextPct
	}
	// Both estimates cover the full input. Anthropic reports cache buckets separately.
	if u.cacheRead != nil {
		estimate -= *u.cacheRead
	}
	if u.cacheWrite != nil {
		estimate -= *u.cacheWrite
	}
	return max(0, estimate), source
}

func (u *kiroUsage) outputTokens(estimate int) int {
	if u.output != nil {
		return *u.output
	}
	return estimate
}

// kiroMetering contains only measurements observed in provider events.
type kiroMetering struct {
	ContextUsagePercentage *float64 `json:"context_usage_percentage,omitempty"`
	Credits                *float64 `json:"credits,omitempty"`
}

func (u *kiroUsage) metering() *kiroMetering {
	if !u.havePct && !u.haveCredits {
		return nil
	}
	m := &kiroMetering{}
	if u.havePct {
		m.ContextUsagePercentage = &u.pct
	}
	if u.haveCredits {
		m.Credits = &u.credits
	}
	return m
}

// anthropicUsage renders the Anthropic usage object for message_delta / non-stream responses.
func (u *kiroUsage) anthropicUsage(model string, inputEstimate, outputEstimate int) (map[string]any, string) {
	in, source := u.inputTokens(model, inputEstimate)
	out := map[string]any{"input_tokens": in, "output_tokens": u.outputTokens(outputEstimate)}
	if u.cacheRead != nil {
		out["cache_read_input_tokens"] = *u.cacheRead
	}
	if u.cacheWrite != nil {
		out["cache_creation_input_tokens"] = *u.cacheWrite
	}
	return out, source
}

// usageHeaders describes token provenance and, when Kiro metered the call, the
// credit charge. Credits are account units, never tokens.
func usageHeaders(contentType, source string, u *kiroUsage) http.Header {
	h := http.Header{"Content-Type": {contentType}, "X-Kiro-Token-Usage": {source}}
	if source == usageSourceEstimated {
		h.Set("X-Kiro-Token-Estimator", countTokensEstimator)
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
	case "claude-fable-5.1", "claude-fable-5.2", "claude-sonnet-5.5", "claude-sonnet-5", "claude-opus-5.5", "claude-opus-5",
		"claude-sonnet-4.6", "claude-opus-4.6", "claude-opus-4.7", "claude-opus-4.8":
		return 1_000_000
	}
	return int(limit)
}
