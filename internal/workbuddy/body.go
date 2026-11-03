package workbuddy

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

var (
	identitySentenceRe = regexp.MustCompile(`(?i)\bYou are an?(?:\s+AI)?(?:\s+coding)?\s+(?:agent\s+powered by|assistant\s+built on)[^\r\n]*`)
	deepSeekHarnessRe  = regexp.MustCompile(`(?i)\bDeepSeek Harness\b`)
	neutralHead        = "You are a helpful assistant.\n"
)

const zeroWidth = "\u200b"

// PrepareBody applies the request quirks WorkBuddy needs. It is called once
// per client request, before any account-specific retry.
func PrepareBody(raw []byte) ([]byte, error) {
	obj, err := decodeObject(raw)
	if err != nil {
		return nil, err
	}
	forceStream(obj)
	normalizeMaxTokens(obj)
	normalizeToolChoice(obj)
	rewriteIdentity(obj)
	ensureSystemHead(obj)
	return json.Marshal(obj)
}

// NeutralizeBody is the 11128 rescue request: it moves scanned openings behind
// a neutral head and inserts invisible zero-width breaks at sentence
// boundaries. It is only used after the upstream already rejected a request.
func NeutralizeBody(raw []byte) ([]byte, error) {
	obj, err := decodeObject(raw)
	if err != nil {
		return nil, err
	}
	ensureSystemHead(obj)
	forEachMessage(obj, func(msg map[string]any) {
		role, _ := msg["role"].(string)
		if role != "system" && role != "developer" && role != "assistant" {
			return
		}
		msg["content"] = mapContent(msg["content"], neutralizeText)
	})
	return json.Marshal(obj)
}

func decodeObject(raw []byte) (map[string]any, error) {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, errors.New("请求体不是合法 JSON 对象")
	}
	return obj, nil
}

func forceStream(obj map[string]any) {
	obj["stream"] = true
}

func normalizeMaxTokens(obj map[string]any) {
	v, ok := obj["max_completion_tokens"]
	if !ok || v == nil {
		return
	}
	if _, has := obj["max_tokens"]; !has {
		obj["max_tokens"] = v
	}
	delete(obj, "max_completion_tokens")
}

// normalizeToolChoice rewrites OpenAI object spellings into the string form the
// upstream accepts (ported from dsh-connect-workbuddy, MIT).
func normalizeToolChoice(obj map[string]any) {
	choice, present := obj["tool_choice"]
	if !present {
		return
	}
	suppress := func() {
		delete(obj, "tools")
		delete(obj, "functions")
	}
	if s, ok := choice.(string); ok {
		if strings.EqualFold(strings.TrimSpace(s), "none") {
			delete(obj, "tool_choice")
			suppress()
		}
		return
	}
	wrapped, ok := choice.(map[string]any)
	if !ok {
		delete(obj, "tool_choice")
		return
	}
	typ, _ := wrapped["type"].(string)
	switch strings.ToLower(strings.TrimSpace(typ)) {
	case "none":
		delete(obj, "tool_choice")
		suppress()
	case "auto", "required":
		obj["tool_choice"] = strings.ToLower(strings.TrimSpace(typ))
	case "function":
		name := ""
		if fn, ok := wrapped["function"].(map[string]any); ok {
			name, _ = fn["name"].(string)
		}
		if name == "" {
			name, _ = wrapped["name"].(string)
		}
		name = strings.TrimSpace(name)
		if name == "" {
			obj["tool_choice"] = "auto"
		} else {
			obj["tool_choice"] = name
		}
	default:
		delete(obj, "tool_choice")
	}
}

func ensureSystemHead(obj map[string]any) {
	messages, _ := obj["messages"].([]any)
	if len(messages) == 0 {
		obj["messages"] = []any{map[string]any{"role": "system", "content": "You are a helpful assistant."}}
		return
	}
	first, _ := messages[0].(map[string]any)
	role, _ := first["role"].(string)
	if role == "system" {
		return
	}
	head := map[string]any{"role": "system", "content": "You are a helpful assistant."}
	obj["messages"] = append([]any{head}, messages...)
}

func forEachMessage(obj map[string]any, fn func(map[string]any)) {
	messages, _ := obj["messages"].([]any)
	for _, item := range messages {
		if msg, ok := item.(map[string]any); ok {
			fn(msg)
		}
	}
}

func rewriteIdentity(obj map[string]any) {
	forEachMessage(obj, func(msg map[string]any) {
		role, _ := msg["role"].(string)
		if role != "system" && role != "developer" {
			return
		}
		msg["content"] = mapContent(msg["content"], func(s string) string {
			s = identitySentenceRe.ReplaceAllString(s, "You are CodeBuddy, an AI coding assistant.")
			return deepSeekHarnessRe.ReplaceAllString(s, "CodeBuddy")
		})
	})
}

func mapContent(content any, fn func(string) string) any {
	switch v := content.(type) {
	case string:
		return fn(v)
	case []any:
		out := make([]any, len(v))
		copy(out, v)
		for i, part := range out {
			pm, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if typ, _ := pm["type"].(string); typ != "text" {
				continue
			}
			if text, ok := pm["text"].(string); ok {
				nm := map[string]any{}
				for k, val := range pm {
					nm[k] = val
				}
				nm["text"] = fn(text)
				out[i] = nm
			}
		}
		return out
	default:
		return content
	}
}

func neutralizeText(s string) string {
	if !strings.HasPrefix(s, neutralHead) {
		s = neutralHead + s
	}
	return addZeroWidth(s)
}

// addZeroWidth inserts U+200B after sentence punctuation. Go's regexp engine
// has no lookahead, so this is a single pass and idempotent by construction.
func addZeroWidth(s string) string {
	runes := []rune(s)
	var b strings.Builder
	b.Grow(len(s) + 16)
	for i, r := range runes {
		b.WriteRune(r)
		nextIsZW := i+1 < len(runes) && runes[i+1] == '\u200b'
		if nextIsZW {
			continue
		}
		switch r {
		case '。', '！', '？':
			b.WriteString(zeroWidth)
		case '.', '!', '?':
			// Only break mid-line punctuation. A punctuation followed by a
			// newline is already a boundary, and adding U+200B there would
			// alter the neutral-head prefix this same function prepends.
			if i+1 < len(runes) && (runes[i+1] == ' ' || runes[i+1] == '\t') {
				b.WriteString(zeroWidth)
			}
		}
	}
	return b.String()
}

// StreamExplicitlyDisabled reports whether the caller explicitly asked for a
// non-streaming response (the upstream rejects those).
func StreamExplicitlyDisabled(raw []byte) bool {
	var probe struct {
		Stream *bool `json:"stream"`
	}
	if json.Unmarshal(raw, &probe) != nil {
		return false
	}
	return probe.Stream != nil && !*probe.Stream
}
