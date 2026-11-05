package api

import (
	"bytes"
	"encoding/json"
)

// isEmptyDeltaMember reports whether a delta member carries no information and
// can be dropped without changing semantics. WorkBuddy always emits these keys
// on every chunk; stripping them keeps the stream compact and lets downstream
// clients treat consecutive reasoning chunks as one continuous block.
func isEmptyDeltaMember(key string, v any) bool {
	switch key {
	case "refusal":
		s, ok := v.(string)
		return ok && s == ""
	case "function_call":
		if v == nil {
			return true
		}
		m, ok := v.(map[string]any)
		if !ok {
			return false
		}
		name, _ := m["name"].(string)
		args, _ := m["arguments"].(string)
		return name == "" && args == ""
	}
	switch v.(type) {
	case nil:
		return true
	case []any:
		return len(v.([]any)) == 0
	case map[string]any:
		return len(v.(map[string]any)) == 0
	case string:
		return v.(string) == ""
	}
	return false
}

// scrubChunk removes empty members from choices[].delta. Payloads it cannot
// parse as a chat.completion.chunk are returned untouched.
func scrubChunk(payload []byte) []byte {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return payload
	}
	var obj map[string]any
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	if err := dec.Decode(&obj); err != nil {
		return payload
	}
	choices, ok := obj["choices"].([]any)
	if !ok {
		return payload
	}
	changed := false
	for _, raw := range choices {
		choice, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		delta, ok := choice["delta"].(map[string]any)
		if !ok {
			continue
		}
		for k, v := range delta {
			if isEmptyDeltaMember(k, v) {
				delete(delta, k)
				changed = true
			}
		}
	}
	if !changed {
		return payload
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return payload
	}
	return out
}

// scrubSSEEvent rewrites one raw SSE event (a block without its trailing
// blank line). Non-data lines, comments and keep-alives pass through.
func scrubSSEEvent(event []byte) []byte {
	if !bytes.Contains(event, []byte("data:")) {
		return event
	}
	lines := bytes.Split(event, []byte("\n"))
	var out bytes.Buffer
	out.Grow(len(event))
	for i, line := range lines {
		if i > 0 {
			out.WriteByte('\n')
		}
		body := bytes.TrimSuffix(line, []byte("\r"))
		if !bytes.HasPrefix(body, []byte("data:")) {
			out.Write(line)
			continue
		}
		payload := body[len("data:"):]
		if len(payload) > 0 && payload[0] == ' ' {
			payload = payload[1:]
		}
		if bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
			out.Write(line)
			continue
		}
		cleaned := scrubChunk(payload)
		if bytes.Equal(cleaned, payload) {
			out.Write(line)
			continue
		}
		out.WriteString("data: ")
		out.Write(cleaned)
		if bytes.HasSuffix(body, []byte("\r")) {
			out.WriteByte('\r')
		}
	}
	return out.Bytes()
}

// sseScrubber is a streaming rewriter: it buffers partial events across Read
// boundaries and only rewrites complete ones, so a frame is never split.
type sseScrubber struct {
	pending []byte
	max     int
}

func newSSEScrubber() *sseScrubber { return &sseScrubber{max: 256 * 1024} }

// Write consumes a chunk and returns the bytes ready to forward. Complete
// events are rewritten; an oversized unterminated event is flushed verbatim so
// memory stays bounded.
func (s *sseScrubber) Write(chunk []byte) []byte {
	s.pending = append(s.pending, chunk...)
	var out bytes.Buffer
	for {
		idx := bytes.Index(s.pending, []byte("\n\n"))
		if idx < 0 {
			break
		}
		event := s.pending[:idx]
		rest := s.pending[idx+2:]
		out.Write(scrubSSEEvent(event))
		out.WriteString("\n\n")
		s.pending = append(s.pending[:0], rest...)
	}
	if len(s.pending) > s.max {
		out.Write(s.pending)
		s.pending = s.pending[:0]
	}
	return out.Bytes()
}

// Flush returns whatever is still buffered (end of stream).
func (s *sseScrubber) Flush() []byte {
	if len(s.pending) == 0 {
		return nil
	}
	out := scrubSSEEvent(s.pending)
	s.pending = nil
	return out
}
