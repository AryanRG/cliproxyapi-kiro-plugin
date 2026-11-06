package kiro

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"  // register decoder for DecodeConfig
	_ "image/jpeg" // register decoder for DecodeConfig
	_ "image/png"  // register decoder for DecodeConfig
	"math"
	"net/http"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tiktoken-go/tokenizer/codec"

	"github.com/xiaokui-dev/cliproxyapi-kiro-plugin/internal/wire"
)

// Token counting for executor.count_tokens.
//
// Kiro exposes no count endpoint and no provider token counters, so this is a
// LOCAL ESTIMATE, never a provider-reported count. Text is tokenized with the
// OpenAI o200k_base BPE (tiktoken-go/tokenizer, pure Go, vocabulary compiled
// into the binary so it works offline). Anthropic's tokenizer differs, so the
// result is approximate (typically within ~10-20% for English and code).
// Images use Anthropic's published (width*height)/750 rule after its resize
// cap; undecodable images use a flat per-image estimate.
const (
	countTokensEstimator      = "o200k-bpe"
	imageTokenFallback        = int64(1600)
	imageMaxLongEdge          = 1568.0
	imageMaxPixels            = 1150000.0
	countTokensPerMessage     = int64(3)
	countTokensPerToolDefault = int64(8)
)

var (
	countCodecOnce sync.Once
	countCodec     *codec.Codec
)

func o200kCodec() *codec.Codec {
	countCodecOnce.Do(func() { countCodec = codec.NewO200kBase() })
	return countCodec
}

// countKiroTokens handles executor.count_tokens. The host translates the
// returned Claude-format payload ({"input_tokens":N}) for the caller.
func countKiroTokens(request []byte) ([]byte, error) {
	var req executorRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return wire.ErrorStatus("invalid_request", "decode count_tokens request: "+err.Error(), http.StatusBadRequest), nil
	}
	count, err := countClaudeInputTokens(req.Payload)
	if err != nil {
		return wire.ErrorStatus("invalid_request", err.Error(), http.StatusBadRequest), nil
	}
	body, err := json.Marshal(map[string]int64{"input_tokens": count})
	if err != nil {
		return nil, err
	}
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	headers.Set("X-Kiro-Token-Usage", "estimated")
	headers.Set("X-Kiro-Token-Estimator", countTokensEstimator)
	return wire.OK(pluginapi.ExecutorResponse{Payload: body, Headers: headers})
}

// countClaudeInputTokens estimates input tokens for an Anthropic Messages /
// count_tokens body (model, system, messages, tools).
func countClaudeInputTokens(payload []byte) (int64, error) {
	if len(bytes.TrimSpace(payload)) == 0 {
		return 0, nil
	}
	var req claudeRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return 0, fmt.Errorf("decode claude count_tokens request: %w", err)
	}
	var c tokenCounter
	c.content(req.System)
	for _, msg := range req.Messages {
		c.extra += countTokensPerMessage
		c.text(msg.Role)
		c.content(msg.Content)
	}
	for _, tool := range req.Tools {
		c.extra += countTokensPerToolDefault
		c.text(tool.Name)
		c.text(tool.Description)
		c.rawJSON(tool.InputSchema)
	}
	return c.total(), nil
}

type tokenCounter struct {
	parts []string
	extra int64
}

func (c *tokenCounter) total() int64 {
	if len(c.parts) == 0 {
		return c.extra
	}
	n, _ := o200kCodec().Count(strings.Join(c.parts, "\n"))
	return int64(n) + c.extra
}

func (c *tokenCounter) text(s string) {
	if s = strings.TrimSpace(s); s != "" {
		c.parts = append(c.parts, s)
	}
}

func (c *tokenCounter) rawJSON(raw json.RawMessage) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return
	}
	var buf bytes.Buffer
	if json.Compact(&buf, raw) == nil {
		c.text(buf.String())
		return
	}
	c.text(string(raw))
}

// content walks a string or array-of-blocks content field (system, message
// content, or tool_result content).
func (c *tokenCounter) content(raw json.RawMessage) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return
	}
	switch raw[0] {
	case '"':
		var s string
		if json.Unmarshal(raw, &s) == nil {
			c.text(s)
		}
	case '[':
		var blocks []json.RawMessage
		if json.Unmarshal(raw, &blocks) != nil {
			return
		}
		for _, b := range blocks {
			c.block(b)
		}
	case '{':
		c.block(raw)
	}
}

func (c *tokenCounter) block(raw json.RawMessage) {
	var b claudeBlock
	if json.Unmarshal(raw, &b) != nil {
		return
	}
	switch b.Type {
	case "text":
		c.text(b.Text)
	case "thinking":
		c.text(b.Thinking)
	case "redacted_thinking":
	case "tool_use":
		c.text(b.Name)
		c.rawJSON(b.Input)
	case "tool_result":
		c.content(b.Content)
	case "image":
		c.extra += imageTokens(b.Source)
	case "document":
		if b.Source != nil && b.Source.Type == "text" {
			c.text(b.Source.Data)
		} else {
			c.rawJSON(json.RawMessage(raw))
		}
	default:
		c.rawJSON(json.RawMessage(raw))
	}
}

// imageTokens applies Anthropic's documented estimate: images are scaled to fit
// a 1568px long edge and ~1.15MP, then cost about (width*height)/750 tokens.
func imageTokens(src *claudeImageSource) int64 {
	if src == nil || src.Type != "base64" || src.Data == "" {
		return imageTokenFallback
	}
	data, err := base64.StdEncoding.DecodeString(src.Data)
	if err != nil {
		return imageTokenFallback
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return imageTokenFallback
	}
	w, h := float64(cfg.Width), float64(cfg.Height)
	scale := math.Min(1, imageMaxLongEdge/math.Max(w, h))
	scale = math.Min(scale, math.Sqrt(imageMaxPixels/(w*h)))
	w, h = math.Floor(w*scale), math.Floor(h*scale)
	return max(int64(math.Ceil(w*h/750)), 1)
}
