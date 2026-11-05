package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func decodeDelta(t *testing.T, payload string) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal([]byte(payload), &obj); err != nil {
		t.Fatalf("decode %q: %v", payload, err)
	}
	choices, _ := obj["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("no choices in %q", payload)
	}
	delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
	return delta
}

func TestScrubChunkDropsEmptyMembers(t *testing.T) {
	in := `{"id":"cmb","model":"deepseek-v4.1-flash","object":"chat.completion.chunk",` +
		`"choices":[{"index":0,"delta":{"content":"Hi","reasoning_content":"","function_call":null,` +
		`"refusal":"","tool_calls":[],"extra_fields":null,"role":"assistant"},"finish_reason":""}],"usage":null}`
	out := scrubChunk([]byte(in))
	delta := decodeDelta(t, string(out))
	if _, ok := delta["tool_calls"]; ok {
		t.Errorf("tool_calls should be dropped: %s", out)
	}
	if _, ok := delta["function_call"]; ok {
		t.Errorf("function_call should be dropped: %s", out)
	}
	if _, ok := delta["extra_fields"]; ok {
		t.Errorf("extra_fields should be dropped: %s", out)
	}
	if _, ok := delta["reasoning_content"]; ok {
		t.Errorf("empty reasoning_content should be dropped: %s", out)
	}
	if delta["content"] != "Hi" {
		t.Errorf("content lost: %s", out)
	}
	if delta["role"] != "assistant" {
		t.Errorf("role lost: %s", out)
	}
}

func TestScrubChunkKeepsRealToolCalls(t *testing.T) {
	in := `{"choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_1","index":0,` +
		`"function":{"name":"bash","arguments":""}}],"refusal":"","extra_fields":null},"finish_reason":""}]}`
	out := scrubChunk([]byte(in))
	delta := decodeDelta(t, string(out))
	tcs, ok := delta["tool_calls"].([]any)
	if !ok || len(tcs) != 1 {
		t.Fatalf("real tool_calls dropped: %s", out)
	}
	fn, _ := tcs[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "bash" {
		t.Errorf("tool name lost: %s", out)
	}
}

func TestScrubChunkPassThrough(t *testing.T) {
	cases := []string{
		`[DONE]`,
		``,
		`{"choices":[],"usage":{"prompt_tokens":12}}`,
		`not json at all`,
	}
	for _, in := range cases {
		if out := scrubChunk([]byte(in)); string(out) != in {
			t.Errorf("expected passthrough for %q, got %q", in, out)
		}
	}
}

func TestScrubSSEEventPreservesStructure(t *testing.T) {
	event := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\",\"tool_calls\":[],\"refusal\":\"\"},\"finish_reason\":\"\"}]}"
	out := string(scrubSSEEvent([]byte(event)))
	if !strings.HasPrefix(out, "data: ") {
		t.Fatalf("data prefix lost: %q", out)
	}
	if strings.Contains(out, "tool_calls") || strings.Contains(out, "refusal") {
		t.Errorf("empty members survived: %q", out)
	}
	if !strings.Contains(out, `"content":"x"`) {
		t.Errorf("content lost: %q", out)
	}
}

func TestScrubSSEEventKeepsDoneAndComments(t *testing.T) {
	for _, in := range []string{"data: [DONE]", ": keep-alive", "event: ping"} {
		if out := string(scrubSSEEvent([]byte(in))); out != in {
			t.Errorf("expected %q unchanged, got %q", in, out)
		}
	}
}

func TestSSEScrubberHandlesSplitFrames(t *testing.T) {
	full := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\",\"tool_calls\":[]},\"finish_reason\":\"\"}]}\n\n" +
		"data: [DONE]\n\n"
	s := newSSEScrubber()
	var got strings.Builder
	for i := 0; i < len(full); i++ {
		got.Write(s.Write([]byte{full[i]}))
	}
	got.Write(s.Flush())
	out := got.String()
	if strings.Count(out, "\n\n") != 2 {
		t.Fatalf("expected 2 events, got %q", out)
	}
	if strings.Contains(out, "tool_calls") {
		t.Errorf("frame split lost scrubbing: %q", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Errorf("DONE lost: %q", out)
	}
}

func TestSSEScrubberFlushesTrailingPartial(t *testing.T) {
	s := newSSEScrubber()
	if out := s.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"z\",\"refusal\":\"\"}}]}")); len(out) != 0 {
		t.Fatalf("expected buffering, got %q", out)
	}
	out := string(s.Flush())
	if !strings.Contains(out, `"content":"z"`) || strings.Contains(out, "refusal") {
		t.Fatalf("flush did not scrub: %q", out)
	}
}
