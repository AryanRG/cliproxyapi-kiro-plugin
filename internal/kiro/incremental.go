package kiro

import (
	"encoding/json"
	"fmt"
	"strings"
)

const maxToolBytes = 4 * 1024 * 1024
const maxStreamTools = 128

type streamingTool struct {
	index   int
	name    string
	input   strings.Builder
	closed  bool
	ignored bool
}
type incrementalMessage struct {
	emit         func([]byte) error
	maps         *toolNameMaps
	next         int
	block        int
	kind         string
	tools        map[string]*streamingTool
	order        []string
	lastTool     string
	outputBytes  int
	visibleBytes int
	pendingText  string
	bracketCalls []toolCall
	fingerprints map[string]bool
	toolCount    int
}

func newIncrementalMessage(model string, inputTokens int, maps *toolNameMaps, emit func([]byte) error) (*incrementalMessage, error) {
	s := &incrementalMessage{emit: emit, maps: maps, block: -1, tools: map[string]*streamingTool{}, fingerprints: map[string]bool{}}
	err := s.event("message_start", map[string]any{"message": map[string]any{"id": randomMessageID(), "type": "message", "role": "assistant", "model": model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": inputTokens, "output_tokens": 0}}})
	return s, err
}
func (s *incrementalMessage) event(kind string, data map[string]any) error {
	data["type"] = kind
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return s.emit(append(append([]byte("event: "+kind+"\ndata: "), b...), []byte("\n\n")...))
}
func (s *incrementalMessage) closeBlock() error {
	if s.block < 0 {
		return nil
	}
	i := s.block
	s.block = -1
	s.kind = ""
	return s.event("content_block_stop", map[string]any{"index": i})
}
func (s *incrementalMessage) delta(kind, text string) error {
	if text == "" {
		return nil
	}
	if s.kind != kind {
		if err := s.closeBlock(); err != nil {
			return err
		}
		s.block = s.next
		s.next++
		s.kind = kind
		block := map[string]any{"type": kind, kind: ""}
		if err := s.event("content_block_start", map[string]any{"index": s.block, "content_block": block}); err != nil {
			return err
		}
	}
	s.outputBytes += len(text)
	if kind == "text" {
		s.visibleBytes += len(text)
	}
	return s.event("content_block_delta", map[string]any{"index": s.block, "delta": map[string]any{"type": kind + "_delta", kind: text}})
}
func (s *incrementalMessage) content(text string, final bool) error {
	s.pendingText += text
	for s.pendingText != "" {
		pos := strings.Index(s.pendingText, "[Called")
		if pos < 0 {
			n := len(s.pendingText)
			if !final {
				for k := 1; k < len("[Called") && k <= n; k++ {
					if strings.HasSuffix(s.pendingText, "[Called"[:k]) {
						n = len(s.pendingText) - k
					}
				}
			}
			if err := s.delta("text", s.pendingText[:n]); err != nil {
				return err
			}
			s.pendingText = s.pendingText[n:]
			return nil
		}
		if pos > 0 {
			if err := s.delta("text", s.pendingText[:pos]); err != nil {
				return err
			}
			s.pendingText = s.pendingText[pos:]
		}
		end := findMatchingBracket(s.pendingText, 0)
		if end < 0 {
			if len(s.pendingText) > maxToolBytes {
				return fmt.Errorf("tool_input_too_large")
			}
			if final {
				text = s.pendingText
				s.pendingText = ""
				return s.delta("text", text)
			}
			return nil
		}
		span := s.pendingText[:end+1]
		s.pendingText = s.pendingText[end+1:]
		if call, ok := parseSingleBracketToolCall(span, s.maps); ok {
			if len(s.bracketCalls) >= maxStreamTools {
				return fmt.Errorf("too_many_tools")
			}
			if call.name != kiroPlaceholderToolName {
				s.bracketCalls = append(s.bracketCalls, call)
			}
		} else if err := s.delta("text", span); err != nil {
			return err
		}
	}
	return nil
}
func (s *incrementalMessage) consume(kind string, p framePayload) error {
	// Shape fallback is only appropriate when the provider omitted the event type.
	if kind != "" && kind != "assistantResponseEvent" && kind != "reasoningContentEvent" && kind != "toolUseEvent" {
		return nil
	}
	events, _ := classifyFrame(kind, p)
	for _, ev := range events {
		switch ev.Kind {
		case cwEventContent:
			if err := s.content(ev.Text, false); err != nil {
				return err
			}
		case cwEventReasoning:
			if err := s.delta("thinking", ev.Text); err != nil {
				return err
			}
		case cwEventToolUse, cwEventToolUseInput:
			id := ev.ToolUseID
			if id == "" {
				id = s.lastTool
			}
			if id == "" {
				return fmt.Errorf("tool_input_without_id")
			}
			t := s.tools[id]
			if t == nil {
				if ev.Name == "" {
					return fmt.Errorf("tool_input_before_start")
				}
				if len(s.tools) >= maxStreamTools {
					return fmt.Errorf("too_many_tools")
				}
				if err := s.content("", true); err != nil {
					return err
				}
				if err := s.closeBlock(); err != nil {
					return err
				}
				t = &streamingTool{index: s.next, name: s.maps.fromKiro(ev.Name), ignored: ev.Name == kiroPlaceholderToolName}
				s.tools[id] = t
				s.order = append(s.order, id)
				s.lastTool = id
				if !t.ignored {
					s.next++
					s.toolCount++
					s.outputBytes += len(t.name)
					if err := s.event("content_block_start", map[string]any{"index": t.index, "content_block": map[string]any{"type": "tool_use", "id": id, "name": t.name, "input": map[string]any{}}}); err != nil {
						return err
					}
				}
			} else if ev.Name != "" && s.maps.fromKiro(ev.Name) != t.name {
				return fmt.Errorf("tool_identity_changed")
			}
			if t.closed {
				return fmt.Errorf("tool_delta_after_stop")
			}
			if t.input.Len()+len(ev.Text) > maxToolBytes {
				return fmt.Errorf("tool_input_too_large")
			}
			t.input.WriteString(ev.Text)
			if !t.ignored && ev.Text != "" {
				s.outputBytes += len(ev.Text)
				if err := s.event("content_block_delta", map[string]any{"index": t.index, "delta": map[string]any{"type": "input_json_delta", "partial_json": ev.Text}}); err != nil {
					return err
				}
			}
			if ev.Stop {
				if err := s.closeTool(t); err != nil {
					return err
				}
			}
		}
	}
	// Some providers send the final stop flag without a new input fragment.
	if kind == "toolUseEvent" && len(events) == 0 && p.Stop {
		id := p.ToolUseID
		if id == "" {
			id = s.lastTool
		}
		if t := s.tools[id]; t != nil && !t.closed {
			return s.closeTool(t)
		}
	}
	return nil
}
func toolFingerprint(name, input string) (string, error) {
	var obj map[string]any
	decoder := json.NewDecoder(strings.NewReader(input))
	decoder.UseNumber()
	if !json.Valid([]byte(input)) || decoder.Decode(&obj) != nil || obj == nil {
		return "", fmt.Errorf("invalid_tool_json")
	}
	canonical, _ := json.Marshal(obj)
	return name + ":" + string(canonical), nil
}
func (s *incrementalMessage) closeTool(t *streamingTool) error {
	if t.closed {
		return nil
	}
	t.closed = true
	if t.ignored {
		return nil
	}
	input := t.input.String()
	if strings.TrimSpace(input) == "" {
		input = "{}"
		if err := s.event("content_block_delta", map[string]any{"index": t.index, "delta": map[string]any{"type": "input_json_delta", "partial_json": "{}"}}); err != nil {
			return err
		}
	}
	fp, err := toolFingerprint(t.name, input)
	if err != nil {
		return err
	}
	s.fingerprints[fp] = true
	return s.event("content_block_stop", map[string]any{"index": t.index})
}
func (s *incrementalMessage) finish() error {
	if err := s.content("", true); err != nil {
		return err
	}
	if err := s.closeBlock(); err != nil {
		return err
	}
	for _, id := range s.order {
		if err := s.closeTool(s.tools[id]); err != nil {
			return err
		}
	}
	for _, call := range s.bracketCalls {
		fp, err := toolFingerprint(call.name, string(call.input))
		if err != nil {
			return err
		}
		if s.fingerprints[fp] {
			continue
		}
		s.fingerprints[fp] = true
		if err := s.consume("toolUseEvent", framePayload{Name: call.name, ToolUseID: call.id, Input: call.input, Stop: true}); err != nil {
			return err
		}
	}
	if s.visibleBytes == 0 && s.toolCount == 0 {
		return fmt.Errorf("empty_upstream_response")
	}
	stop := "end_turn"
	if s.toolCount > 0 {
		stop = "tool_use"
	}
	if err := s.event("message_delta", map[string]any{"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": estimateTokens(s.outputBytes)}}); err != nil {
		return err
	}
	return s.event("message_stop", map[string]any{})
}
