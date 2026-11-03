package workbuddy

import (
	"strings"
	"testing"
)

func TestClassifyWorkBuddyErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		kind   string
		reset  string
	}{
		{"channel", 400, `{"code":11128,"msg":"unapproved channel"}`, FailureChannel, ""},
		{"auth", 401, `{"code":401,"msg":"unauthorized"}`, FailureAuth, ""},
		{"credit", 400, `{"code":14018,"msg":"Credits exhausted"}`, FailureCredit, ""},
		{"account", 400, `{"code":11140,"displayMsg":"账号存在异常行为，已被限制使用"}`, FailureAccount, ""},
		{"busy", 429, `{"code":14003,"msg":"model is busy"}`, FailureBusy, ""},
		{"rate", 429, `{"code":6004,"msg":"请求过于频繁，将在 2026-12-03 02:30:30 UTC+8 重置"}`, FailureRate, "2026-12-03 02:30:30"},
		{"server", 500, `oops`, FailureServer, ""},
		{"client", 400, `{"code":400,"msg":"bad request"}`, FailureClient, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := Classify(c.status, c.body)
			if f.Kind != c.kind {
				t.Fatalf("kind = %s, want %s", f.Kind, c.kind)
			}
			if f.ResetAt != c.reset {
				t.Fatalf("reset = %q, want %q", f.ResetAt, c.reset)
			}
		})
	}
}

func TestClassifyNestedCode(t *testing.T) {
	f := Classify(400, `{"error":{"data":{"code":"11128"}},"displayTips":"x"}`)
	if f.Kind != FailureChannel {
		t.Fatalf("kind = %s", f.Kind)
	}
}

func TestResetCooldown(t *testing.T) {
	if d := ResetCooldown("2000-01-01 00:00:00"); d <= 0 {
		t.Fatalf("expected positive fallback, got %v", d)
	}
	if d := ResetCooldown(""); d <= 0 {
		t.Fatalf("expected fallback, got %v", d)
	}
}

func TestBusinessCodeSpaced(t *testing.T) {
	if got := businessCode(`{"code": 11128}`); !strings.Contains(got, "11128") {
		t.Fatalf("got %q", got)
	}
}
