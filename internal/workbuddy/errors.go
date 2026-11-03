package workbuddy

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	FailureChannel = "channel"
	FailureAuth    = "auth"
	FailureCredit  = "credit"
	FailureAccount = "account"
	FailureBusy    = "busy"
	FailureRate    = "rate"
	FailureServer  = "server"
	FailureClient  = "client"
)

// Failure is the WorkBuddy-specific classification of an upstream response.
type Failure struct {
	Kind       string
	Code       string
	Status     int
	Message    string
	ResetAt    string
	Switchable bool
	Retryable  bool
}

var (
	codeRe    = regexp.MustCompile(`(?i)"code"\s*:\s*"?(-?\d{1,9}(?:-\d{1,9})?)"?`)
	resetAtRe = regexp.MustCompile(`(20\d{2}-(?:0[1-9]|1[0-2])-(?:0[1-9]|[12]\d|3[01]) (?:[01]\d|2[0-3]):[0-5]\d:[0-5]\d)\s*UTC\+8`)
)

func businessCode(body string) string {
	var v any
	if json.Unmarshal([]byte(body), &v) == nil {
		if c := codeFromValue(v); c != "" {
			return c
		}
	}
	if m := codeRe.FindStringSubmatch(body); len(m) == 2 {
		return m[1]
	}
	return ""
}

func codeFromValue(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	for _, key := range []string{"code"} {
		if raw, ok := m[key]; ok {
			switch t := raw.(type) {
			case string:
				if t != "" {
					return t
				}
			case float64:
				return strconv.FormatInt(int64(t), 10)
			case json.Number:
				return t.String()
			}
		}
	}
	for _, key := range []string{"error", "data"} {
		if child, ok := m[key]; ok {
			if c := codeFromValue(child); c != "" {
				return c
			}
		}
	}
	return ""
}

func resetAtOf(body string) string {
	if m := resetAtRe.FindStringSubmatch(body); len(m) == 2 {
		return m[1]
	}
	return ""
}

// ResetCooldown converts a reset timestamp to a cooldown duration. If the
// timestamp is absent or already past, a conservative one-hour cooldown is
// used so the account is not hammered.
func ResetCooldown(resetAt string) time.Duration {
	if resetAt != "" {
		if t, err := time.Parse("2006-01-02 15:04:05", resetAt); err == nil {
			// The upstream prints UTC+8. time.Parse uses UTC, so subtract the
			// offset before comparing.
			t = t.Add(-8 * time.Hour)
			if d := time.Until(t); d > 0 {
				return d
			}
		}
	}
	return time.Hour
}

func Classify(status int, body string) Failure {
	code := businessCode(body)
	lower := strings.ToLower(body)
	f := Failure{Code: code, Status: status}
	has := func(parts ...string) bool {
		for _, p := range parts {
			if strings.Contains(lower, strings.ToLower(p)) {
				return true
			}
		}
		return false
	}
	switch {
	case code == "11128" || code == "11-128":
		f.Kind = FailureChannel
		f.Message = "WorkBuddy 渠道校验未通过（11128）"
	case status == 401 || status == 403:
		f.Kind = FailureAuth
		f.Message = "WorkBuddy 登录已失效，请重新登录"
		f.Switchable = true
	case code == "14018" || has("credits exhausted", "积分不足", "余额不足", "insufficient balance", "insufficient_quota"):
		f.Kind = FailureCredit
		f.Message = "WorkBuddy 账号积分不足或已耗尽"
		f.ResetAt = resetAtOf(body)
		f.Switchable = true
	case code == "11140" && has("账号存在异常行为", "account is restricted", "account restricted", "已被限制使用"):
		f.Kind = FailureAccount
		f.Message = "WorkBuddy 账号被上游限制使用"
		f.Switchable = true
	case code == "14003" || has("model is busy", "currently busy", "模型繁忙", "排队", "queue is full", "queue full"):
		f.Kind = FailureBusy
		f.Message = "WorkBuddy 当前模型繁忙/排队中，请稍后重试或换模型"
		f.ResetAt = resetAtOf(body)
	case status == 429 || code == "6004":
		f.Kind = FailureRate
		f.Message = "WorkBuddy 当前模型调用频率已达上限"
		f.ResetAt = resetAtOf(body)
	case status >= 500 && status <= 599:
		f.Kind = FailureServer
		f.Message = "WorkBuddy 上游服务错误"
		f.Retryable = true
		f.Switchable = true
	case status == 408 || status == 0:
		f.Kind = FailureServer
		f.Message = "WorkBuddy 上游连接失败"
		f.Retryable = true
		f.Switchable = true
	case status == 400 || status == 404 || status == 422:
		f.Kind = FailureClient
		f.Message = "WorkBuddy 拒绝了该请求"
	default:
		f.Kind = FailureClient
		f.Message = fmt.Sprintf("WorkBuddy 请求失败（HTTP %d）", status)
	}
	return f
}

func IsChannelReject(status int, body string) bool {
	return Classify(status, body).Kind == FailureChannel
}
