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

const dshFileReferencePrompt = "Tokens prefixed with @ are paths the user explicitly referenced. Relative paths resolve from the workspace root; absolute paths identify files or directories on the host."

func TestPrepareBodyRewritesKnownFingerprint(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[{"role":"system","content":"` + dshFileReferencePrompt + `"},{"role":"user","content":"hi"}]}`)
	out, err := PrepareBody(raw)
	if err != nil {
		t.Fatal(err)
	}
	content, _ := firstMessage(t, decode(t, out))["content"].(string)
	if strings.Contains(content, "explicitly referenced") {
		t.Fatalf("fingerprint not rewritten: %q", content)
	}
	if !strings.Contains(content, "paths the user explicitly mentioned") {
		t.Fatalf("expected reworded sentence: %q", content)
	}
	// The rest of the instruction must survive intact.
	if !strings.Contains(content, "Relative paths resolve from the workspace root") {
		t.Fatalf("instruction lost: %q", content)
	}
}

func TestPrepareBodyRewritesFingerprintInAssistantHistory(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[{"role":"system","content":"You are a helpful assistant."},` +
		`{"role":"assistant","content":"` + dshFileReferencePrompt + `"},` +
		`{"role":"user","content":"ok"}]}`)
	out, err := PrepareBody(raw)
	if err != nil {
		t.Fatal(err)
	}
	msgs, _ := decode(t, out)["messages"].([]any)
	assistant, _ := msgs[1].(map[string]any)
	if strings.Contains(assistant["content"].(string), "explicitly referenced") {
		t.Fatalf("assistant fingerprint not rewritten: %v", assistant["content"])
	}
}

func TestPrepareBodyLeavesUnscannedRolesAlone(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[{"role":"system","content":"s"},` +
		`{"role":"user","content":"` + dshFileReferencePrompt + `"},` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"read","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"c1","content":"` + dshFileReferencePrompt + `"}]}`)
	out, err := PrepareBody(raw)
	if err != nil {
		t.Fatal(err)
	}
	msgs, _ := decode(t, out)["messages"].([]any)
	for _, idx := range []int{1, 3} {
		msg, _ := msgs[idx].(map[string]any)
		content, _ := msg["content"].(string)
		if content != dshFileReferencePrompt {
			t.Fatalf("role %v must stay byte-identical, got %q", msg["role"], content)
		}
	}
}

func TestPrepareBodyFingerprintRewriteIsIdempotent(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[{"role":"system","content":"` + dshFileReferencePrompt + `"},{"role":"user","content":"hi"}]}`)
	out1, err := PrepareBody(raw)
	if err != nil {
		t.Fatal(err)
	}
	out2, err := PrepareBody(out1)
	if err != nil {
		t.Fatal(err)
	}
	if string(out1) != string(out2) {
		t.Fatalf("not idempotent:\n%s\n%s", out1, out2)
	}
}

func TestPrepareBodyLeavesCleanPromptByteIdentical(t *testing.T) {
	clean := "You are a helpful assistant. Answer concisely."
	raw := []byte(`{"model":"m","messages":[{"role":"system","content":"` + clean + `"},{"role":"user","content":"hi"}]}`)
	out, err := PrepareBody(raw)
	if err != nil {
		t.Fatal(err)
	}
	content, _ := firstMessage(t, decode(t, out))["content"].(string)
	if content != clean {
		t.Fatalf("clean prompt changed: %q", content)
	}
}

func TestPrepareBodyMapsDeveloperRoleToSystem(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[{"role":"developer","content":"You are a helpful assistant."},{"role":"user","content":"hi"}]}`)
	out, err := PrepareBody(raw)
	if err != nil {
		t.Fatal(err)
	}
	obj := decode(t, out)
	msgs, _ := obj["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("expected no extra system head, got %d messages: %s", len(msgs), out)
	}
	if role := firstMessage(t, obj)["role"]; role != "system" {
		t.Fatalf("developer role not mapped: %v", role)
	}
	if !strings.Contains(string(out), `"system"`) || strings.Contains(string(out), `"developer"`) {
		t.Fatalf("unexpected roles in %s", out)
	}
}

func TestPrepareBodyDeveloperRoleWithFingerprint(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[{"role":"developer","content":"` + dshFileReferencePrompt + `"},{"role":"user","content":"hi"}]}`)
	out, err := PrepareBody(raw)
	if err != nil {
		t.Fatal(err)
	}
	msg, _ := decode(t, out)["messages"].([]any)[0].(map[string]any)
	if msg["role"] != "system" {
		t.Fatalf("role = %v", msg["role"])
	}
	if content, _ := msg["content"].(string); strings.Contains(content, "explicitly referenced") {
		t.Fatalf("fingerprint not rewritten: %q", content)
	}
}

func TestPrepareBodyLeavesToolAndUserRolesAlone(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[{"role":"system","content":"s"},` +
		`{"role":"user","content":"u"},` +
		`{"role":"tool","tool_call_id":"c1","content":"t"},` +
		`{"role":"assistant","content":"a"}]}`)
	out, err := PrepareBody(raw)
	if err != nil {
		t.Fatal(err)
	}
	msgs, _ := decode(t, out)["messages"].([]any)
	want := []string{"system", "user", "tool", "assistant"}
	for i, w := range want {
		msg, _ := msgs[i].(map[string]any)
		if msg["role"] != w {
			t.Fatalf("message %d role = %v, want %s", i, msg["role"], w)
		}
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
