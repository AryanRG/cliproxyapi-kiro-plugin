package kiro

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"strings"
)

const maxFrameBytes = 16 * 1024 * 1024

// Each callback may split a prelude, UTF-8 sequence, payload or checksum.
type frameDecoder struct{ pending []byte }

func (d *frameDecoder) feed(chunk []byte, consume func(string, framePayload) error) error {
	for len(chunk) > 0 {
		need := 12
		if len(d.pending) >= 12 {
			need = int(binary.BigEndian.Uint32(d.pending))
		}
		n := need - len(d.pending)
		if n > len(chunk) {
			n = len(chunk)
		}
		d.pending = append(d.pending, chunk[:n]...)
		chunk = chunk[n:]
		if len(d.pending) < 12 {
			continue
		}
		total := int(binary.BigEndian.Uint32(d.pending))
		headers := int(binary.BigEndian.Uint32(d.pending[4:]))
		if total < 16 || total > maxFrameBytes || headers > total-16 {
			return fmt.Errorf("invalid_event_frame_length")
		}
		if crc32.ChecksumIEEE(d.pending[:8]) != binary.BigEndian.Uint32(d.pending[8:12]) {
			return fmt.Errorf("invalid_event_prelude_crc")
		}
		if len(d.pending) < total {
			continue
		}
		frame := d.pending
		if crc32.ChecksumIEEE(frame[:total-4]) != binary.BigEndian.Uint32(frame[total-4:]) {
			return fmt.Errorf("invalid_event_message_crc")
		}
		h, err := decodeFrameHeaders(frame[12 : 12+headers])
		if err != nil {
			return err
		}
		if h[":message-type"] == "exception" || h[":message-type"] == "error" || h[":exception-type"] != "" || strings.HasSuffix(h[":event-type"], "Exception") {
			return fmt.Errorf("upstream_event_error")
		}
		var payload framePayload
		if json.Unmarshal(frame[12+headers:total-4], &payload) != nil {
			return fmt.Errorf("invalid_event_json")
		}
		if err := consume(h[":event-type"], payload); err != nil {
			return err
		}
		d.pending = nil
	}
	return nil
}
func (d *frameDecoder) finish() error {
	if len(d.pending) != 0 {
		return fmt.Errorf("truncated_event_frame")
	}
	return nil
}
func decodeFrameHeaders(b []byte) (map[string]string, error) {
	out := map[string]string{}
	for len(b) > 0 {
		n := int(b[0])
		b = b[1:]
		if n == 0 || len(b) < n+1 {
			return nil, fmt.Errorf("invalid_event_headers")
		}
		name := string(b[:n])
		kind := b[n]
		b = b[n+1:]
		length := 0
		switch kind {
		case 0, 1:
		case 2:
			length = 1
		case 3:
			length = 2
		case 4:
			length = 4
		case 5, 8:
			length = 8
		case 9:
			length = 16
		case 6, 7:
			if len(b) < 2 {
				return nil, fmt.Errorf("invalid_event_headers")
			}
			length = int(binary.BigEndian.Uint16(b))
			b = b[2:]
		default:
			return nil, fmt.Errorf("invalid_event_headers")
		}
		if len(b) < length {
			return nil, fmt.Errorf("invalid_event_headers")
		}
		if kind == 7 {
			out[name] = string(b[:length])
		}
		b = b[length:]
	}
	return out, nil
}
