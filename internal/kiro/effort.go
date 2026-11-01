package kiro

import (
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"slices"
)

// Exact model IDs verified against both account catalogues on 2026-11-01.
// Do not normalize dotted Kiro IDs to the distinct Claude provider IDs.
func effortModel(model string) bool {
	return slices.Contains([]string{"claude-opus-5.5", "claude-sonnet-5.5", "claude-fable-5.1", "claude-haiku-5.5"}, resolveKiroModel(model))
}
func modelThinkingSupport(model string) *pluginapi.ThinkingSupport {
	if !effortModel(model) {
		return nil
	}
	return &pluginapi.ThinkingSupport{DynamicAllowed: true, ZeroAllowed: model == "claude-haiku-5.5", Levels: []string{"low", "medium", "high", "xhigh", "max"}}
}
func kiroRequestControls(req claudeRequest, model string) (map[string]any, error) {
	effort := ""
	if req.OutputConfig != nil {
		effort = req.OutputConfig.Effort
	}
	if !effortModel(model) {
		if effort != "" {
			return nil, fmt.Errorf("Kiro effort controls are not verified for model %s", model)
		}
		return nil, nil
	}
	fields := map[string]any{}
	if effort != "" {
		if !slices.Contains([]string{"low", "medium", "high", "xhigh", "max"}, effort) {
			return nil, fmt.Errorf("unsupported Kiro effort %q", effort)
		}
		fields["output_config"] = map[string]string{"effort": effort}
	}
	thinkingType := ""
	display := ""
	if req.Thinking != nil {
		thinkingType = req.Thinking.Type
		display = req.Thinking.Display
	}
	// Claude clients may use the legacy enabled/budget form for a custom ID.
	// These Kiro models expose adaptive effort, not a token-budget control.
	if thinkingType == "enabled" {
		thinkingType = "adaptive"
	}
	if thinkingType == "" && effort != "" {
		thinkingType = "adaptive"
	}
	if thinkingType != "" {
		valid := thinkingType == "adaptive" || (thinkingType == "between_tools" && model == "claude-sonnet-5.5") || (thinkingType == "disabled" && model == "claude-haiku-5.5")
		if !valid {
			return nil, fmt.Errorf("thinking type %q is unsupported for %s", thinkingType, model)
		}
		thinking := map[string]string{"type": thinkingType}
		if display != "" {
			if display != "summarized" && display != "omitted" {
				return nil, fmt.Errorf("unsupported thinking display")
			}
			thinking["display"] = display
		}
		fields["thinking"] = thinking
	}
	if req.MaxTokens != nil {
		if *req.MaxTokens < 1024 || *req.MaxTokens > 64000 {
			return nil, fmt.Errorf("Kiro max_tokens must be between 1024 and 64000")
		}
		fields["max_tokens"] = *req.MaxTokens
	}
	if len(fields) == 0 {
		return nil, nil
	}
	return fields, nil
}
