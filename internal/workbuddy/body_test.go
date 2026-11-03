package workbuddy

import (
	"encoding/json"
	"strings"
	"testing"
)

func decode(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	return obj
}

func firstMessage(t *testing.T, obj map[string]any) map[string]any {
	t.Helper()
	msgs, _ := obj["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("no messages")
	}
	msg, _ := msgs[0].(map[string]any)
	return msg
}

func TestPrepareBodyAddsSystemHeadAndForcesStream(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	out, err := PrepareBody(raw)
	if err != nil {
		t.Fatal(err)
	}
	obj := decode(t, out)
	if obj["stream"] != true {
		t.Fatalf("stream not forced: %v", obj["stream"])
	}
	if role := firstMessage(t, obj)["role"]; role != "system" {
		t.Fatalf("first role = %v", role)
	}
}

func TestPrepareBodyRewritesIdentity(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[{"role":"system","content":"You are an AI agent powered by DeepSeek Harness. Do work."},{"role":"user","content":"hi"}]}`)
	out, err := PrepareBody(raw)
	if err != nil {
		t.Fatal(err)
	}
	obj := decode(t, out)
	content, _ := firstMessage(t, obj)["content"].(string)
	if strings.Contains(content, "DeepSeek Harness") {
		t.Fatalf("identity not rewritten: %q", content)
	}
	if !strings.HasPrefix(content, "You are CodeBuddy, an AI coding assistant.") {
		t.Fatalf("unexpected identity: %q", content)
	}
}

func TestPrepareBodyNormalizesToolChoiceAndTokens(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[{"role":"system","content":"s"}],"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":{"type":"function","function":{"name":"f"}},"max_completion_tokens":12}`)
	out, err := PrepareBody(raw)
	if err != nil {
		t.Fatal(err)
	}
	obj := decode(t, out)
	if obj["tool_choice"] != "f" {
		t.Fatalf("tool_choice = %v", obj["tool_choice"])
	}
	if _, ok := obj["max_completion_tokens"]; ok {
		t.Fatal("max_completion_tokens should be removed")
	}
	if obj["max_tokens"] != float64(12) {
		t.Fatalf("max_tokens = %v", obj["max_tokens"])
	}

	raw = []byte(`{"model":"m","messages":[{"role":"system","content":"s"}],"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":{"type":"none"}}`)
	out, _ = PrepareBody(raw)
	obj = decode(t, out)
	if _, ok := obj["tools"]; ok {
		t.Fatal("tools should be removed for tool_choice none")
	}
	if _, ok := obj["tool_choice"]; ok {
		t.Fatal("tool_choice should be removed for none")
	}
}

func TestNeutralizeBodyIsIdempotent(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[{"role":"system","content":"You are opencode. Do work now."},{"role":"assistant","content":"Hello! More text."},{"role":"user","content":"hi"}]}`)
	out1, err := NeutralizeBody(raw)
	if err != nil {
		t.Fatal(err)
	}
	out2, err := NeutralizeBody(out1)
	if err != nil {
		t.Fatal(err)
	}
	if string(out1) != string(out2) {
		t.Fatalf("not idempotent:\n%s\n%s", out1, out2)
	}
	obj := decode(t, out1)
	msgs, _ := obj["messages"].([]any)
	for i := 0; i < 2; i++ {
		msg, _ := msgs[i].(map[string]any)
		content, _ := msg["content"].(string)
		if !strings.HasPrefix(content, neutralHead) {
			t.Fatalf("message %d missing neutral head: %q", i, content)
		}
	}
	if !strings.Contains(string(out1), "\u200b") {
		t.Fatal("expected zero-width breaks")
	}
}

func TestStreamExplicitlyDisabled(t *testing.T) {
	if !StreamExplicitlyDisabled([]byte(`{"stream":false}`)) {
		t.Fatal("expected disabled")
	}
	if StreamExplicitlyDisabled([]byte(`{"stream":true}`)) {
		t.Fatal("expected enabled")
	}
	if StreamExplicitlyDisabled([]byte(`{}`)) {
		t.Fatal("absent should not count as disabled")
	}
}
