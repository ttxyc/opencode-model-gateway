package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ttxyc/opencode-model-gateway/internal/config"
	"github.com/ttxyc/opencode-model-gateway/internal/workbuddy"
)

// workbuddyStream mimics the real upstream frames: every chunk repeats the
// empty members that make clients split one thought into many blocks.
func workbuddyStream() string {
	chunks := []string{
		`{"id":"cmb-1","model":"deepseek-v4.1-flash","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{"role":"assistant","content":"","reasoning_content":"","function_call":null,"refusal":"","tool_calls":[],"extra_fields":null},"logprobs":null,"finish_reason":""}],"usage":null}`,
		`{"id":"cmb-1","model":"deepseek-v4.1-flash","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{"content":"","reasoning_content":"Let","function_call":null,"refusal":"","tool_calls":[],"extra_fields":null},"logprobs":null,"finish_reason":""}],"usage":null}`,
		`{"id":"cmb-1","model":"deepseek-v4.1-flash","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{"content":"","reasoning_content":" me","function_call":null,"refusal":"","tool_calls":[],"extra_fields":null},"logprobs":null,"finish_reason":""}],"usage":null}`,
		`{"id":"cmb-1","model":"deepseek-v4.1-flash","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{"content":"","reasoning_content":" think","function_call":null,"refusal":"","tool_calls":[],"extra_fields":null},"logprobs":null,"finish_reason":""}],"usage":null}`,
		`{"id":"cmb-1","model":"deepseek-v4.1-flash","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{"content":"Answer","reasoning_content":"","function_call":null,"refusal":"","tool_calls":[],"extra_fields":null},"logprobs":null,"finish_reason":""}],"usage":null}`,
		`{"id":"cmb-1","model":"deepseek-v4.1-flash","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{"role":"assistant","content":"","reasoning_content":"","function_call":{"name":"","arguments":""},"refusal":"","tool_calls":[],"extra_fields":null},"logprobs":null,"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"completion_tokens_details":{"reasoning_tokens":3}}}`,
	}
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString("data: ")
		b.WriteString(c)
		b.WriteString("\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func newScrubServer(t *testing.T, kind string) (*Server, string) {
	t.Helper()
	cfg := &config.Config{Providers: map[string]*config.Provider{
		"wb": {ID: "wb", Kind: kind},
		"ms": {ID: "ms", Kind: ""},
	}}
	s := &Server{cfg: cfg}
	return s, workbuddyStream()
}

func relayThrough(t *testing.T, s *Server, provider, body string) string {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(body))
	}))
	defer upstream.Close()

	resp, err := http.Get(upstream.URL)
	if err != nil {
		t.Fatalf("upstream: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	s.relay(rec, req, resp, provider, "req-1", 1, true)
	return rec.Body.String()
}

func TestRelayScrubsWorkBuddyStream(t *testing.T) {
	s, body := newScrubServer(t, workbuddy.KindCN)
	out := relayThrough(t, s, "wb", body)

	if strings.Count(out, "\n\n") != strings.Count(body, "\n\n") {
		t.Fatalf("event count changed: %d vs %d", strings.Count(out, "\n\n"), strings.Count(body, "\n\n"))
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("DONE marker lost")
	}
	for _, banned := range []string{`"tool_calls":[]`, `"function_call":null`, `"extra_fields":null`, `"refusal":""`} {
		if strings.Contains(out, banned) {
			t.Errorf("%s survived scrubbing", banned)
		}
	}
	if !strings.Contains(out, `"reasoning_content":"Let"`) {
		t.Errorf("reasoning content lost: %s", out)
	}

	var reasoning, content strings.Builder
	usageSeen := false
	finishSeen := ""
	for _, ev := range strings.Split(out, "\n\n") {
		ev = strings.TrimPrefix(strings.TrimSpace(ev), "data: ")
		if ev == "" || ev == "[DONE]" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(ev), &obj); err != nil {
			t.Fatalf("bad frame %q: %v", ev, err)
		}
		if obj["usage"] != nil {
			usageSeen = true
		}
		choices, _ := obj["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice := choices[0].(map[string]any)
		if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
			finishSeen = fr
		}
		delta, _ := choice["delta"].(map[string]any)
		if v, ok := delta["reasoning_content"].(string); ok {
			reasoning.WriteString(v)
		}
		if v, ok := delta["content"].(string); ok {
			content.WriteString(v)
		}
	}
	if reasoning.String() != "Let me think" {
		t.Errorf("reasoning corrupted: %q", reasoning.String())
	}
	if content.String() != "Answer" {
		t.Errorf("content corrupted: %q", content.String())
	}
	if !usageSeen {
		t.Errorf("usage frame lost")
	}
	if finishSeen != "stop" {
		t.Errorf("finish_reason lost: %q", finishSeen)
	}
}

func TestRelayScrubsWorkBuddyGlobal(t *testing.T) {
	s, body := newScrubServer(t, workbuddy.KindGlobal)
	out := relayThrough(t, s, "wb", body)
	if strings.Contains(out, `"tool_calls":[]`) {
		t.Errorf("global channel not scrubbed")
	}
}

func TestRelayLeavesOtherProvidersAlone(t *testing.T) {
	s, body := newScrubServer(t, "")
	out := relayThrough(t, s, "ms", body)
	if out != body {
		t.Errorf("non-workbuddy stream was modified")
	}
}
