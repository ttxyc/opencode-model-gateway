package workbuddy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// CreditSegment is one package/period inside a WorkBuddy credits balance.
type CreditSegment struct {
	Remaining float64 `json:"remaining"`
	Total     float64 `json:"total,omitempty"`
	ExpiresAt int64   `json:"expiresAt,omitempty"`
	Source    string  `json:"source,omitempty"`
}

// CreditAccount is one account's credits, without tokens.
type CreditAccount struct {
	ID        string          `json:"id"`
	Label     string          `json:"label,omitempty"`
	Region    string          `json:"region"`
	Credits   *float64        `json:"credits,omitempty"`
	Total     *float64        `json:"total,omitempty"`
	Unlimited bool            `json:"unlimited,omitempty"`
	Count     int             `json:"count,omitempty"`
	Segments  []CreditSegment `json:"segments,omitempty"`
	ResetAt   int64           `json:"resetAt,omitempty"`
	Error     string          `json:"error,omitempty"`
}

// CreditsResult is the sanitized response for the TUI balance plugin.
type CreditsResult struct {
	Region    string          `json:"region"`
	Accounts  []CreditAccount `json:"accounts"`
	Total     *float64        `json:"total,omitempty"`
	Unlimited bool            `json:"unlimited,omitempty"`
	Error     string          `json:"error,omitempty"`
}

// FetchCredits returns the sum of every eligible account's WorkBuddy credits.
// Results are cached for 60 seconds so a 5-minute TUI refresh cannot hammer the
// billing endpoint, and repeated requests within one render stay cheap.
func (m *Manager) FetchCredits(ctx context.Context, region string) (CreditsResult, error) {
	if region == "" {
		region = RegionCN
	}
	m.mu.Lock()
	if entry, ok := m.creditsCache[region]; ok && time.Since(entry.at) < 60*time.Second {
		result := entry.result
		m.mu.Unlock()
		return result, nil
	}
	m.mu.Unlock()

	accounts := m.Accounts(region)
	if len(accounts) == 0 {
		return CreditsResult{Region: region}, errors.New("没有可用的 WorkBuddy 账号")
	}
	result := CreditsResult{Region: region}
	var sum float64
	success := 0
	unlimited := false
	for _, acc := range accounts {
		ca := CreditAccount{ID: acc.ID, Label: acc.Label, Region: acc.Region}
		token, err := m.EnsureAccess(ctx, &acc)
		if err != nil {
			ca.Error = err.Error()
			result.Accounts = append(result.Accounts, ca)
			continue
		}
		got, err := m.fetchAccountCredits(ctx, acc, token)
		if err != nil {
			ca.Error = err.Error()
			result.Accounts = append(result.Accounts, ca)
			continue
		}
		ca = got
		ca.ID, ca.Label, ca.Region = acc.ID, acc.Label, acc.Region
		result.Accounts = append(result.Accounts, ca)
		success++
		if ca.Unlimited {
			unlimited = true
			continue
		}
		if ca.Credits != nil {
			sum += *ca.Credits
		}
	}
	if success == 0 {
		result.Error = "所有 WorkBuddy 账号积分查询失败"
	} else if unlimited {
		result.Unlimited = true
	} else {
		total := round2(sum)
		result.Total = &total
	}

	m.mu.Lock()
	m.creditsCache[region] = creditsCacheEntry{at: time.Now(), result: result}
	m.mu.Unlock()
	return result, nil
}

func (m *Manager) fetchAccountCredits(ctx context.Context, acc Account, token string) (CreditAccount, error) {
	host := m.billingHost(acc)
	headers := billingHeaders(acc, token, host)
	if strings.TrimSpace(acc.EnterpriseID) != "" {
		return m.fetchEnterpriseCredits(ctx, acc, host, headers)
	}
	body, err := json.Marshal(buildCreditBody(time.Now()))
	if err != nil {
		return CreditAccount{}, err
	}
	status, raw, err := m.doJSON(ctx, "POST", host+"/v2/billing/meter/get-user-resource", headers, body, 12*time.Second)
	if err != nil {
		return CreditAccount{}, fmt.Errorf("WorkBuddy 积分接口连接失败: %w", err)
	}
	if status < 200 || status >= 300 {
		return CreditAccount{}, errors.New(billingError(status, raw))
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return CreditAccount{}, errors.New("WorkBuddy 积分接口返回格式无法解析")
	}
	accounts := findAccounts(payload)
	if len(accounts) == 0 {
		return CreditAccount{}, errors.New("WorkBuddy 积分接口没有返回账号数据")
	}
	return parsePersonalCredits(accounts)
}

func (m *Manager) fetchEnterpriseCredits(ctx context.Context, acc Account, host string, headers map[string]string) (CreditAccount, error) {
	status, raw, err := m.doJSON(ctx, "POST", host+"/v2/billing/meter/get-enterprise-user-usage", headers, []byte("{}"), 12*time.Second)
	if err != nil {
		return CreditAccount{}, fmt.Errorf("WorkBuddy 企业积分接口连接失败: %w", err)
	}
	if status < 200 || status >= 300 {
		return CreditAccount{}, errors.New(billingError(status, raw))
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return CreditAccount{}, errors.New("WorkBuddy 企业积分接口返回格式无法解析")
	}
	return parseEnterpriseCredits(payload)
}

func (m *Manager) billingHost(acc Account) string {
	domain := strings.TrimSpace(acc.Domain)
	if domain != "" {
		candidate := domain
		if !strings.Contains(candidate, "://") {
			candidate = "https://" + candidate
		}
		if parsed, err := url.Parse(candidate); err == nil && parsed.Host != "" {
			hostname := strings.ToLower(parsed.Hostname())
			if strings.HasSuffix(hostname, "codebuddy.cn") ||
				strings.HasSuffix(hostname, "workbuddy.cn") ||
				strings.HasSuffix(hostname, "workbuddy.ai") ||
				strings.HasSuffix(hostname, "codebuddy.ai") {
				return parsed.Scheme + "://" + parsed.Host
			}
		}
	}
	if acc.Region == RegionGlobal {
		if isCodeBuddyDomain(domain) {
			return m.ep.GlobalCodeBuddyBillingBase
		}
		return m.ep.GlobalBillingBase
	}
	return m.ep.CNBillingBase
}

func billingHeaders(acc Account, token, host string) map[string]string {
	h := map[string]string{
		"accept":            "application/json, text/plain, */*",
		"content-type":      "application/json",
		"x-client-platform": "web",
		"origin":            host,
		"referer":           host + "/profile/plans-usage",
		"authorization":     "Bearer " + token,
		"user-agent":        "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36",
	}
	if acc.Domain != "" {
		h["x-domain"] = acc.Domain
	}
	if acc.UserID != "" {
		h["x-user-id"] = acc.UserID
	}
	if acc.EnterpriseID != "" {
		h["x-enterprise-id"] = acc.EnterpriseID
		h["x-tenant-id"] = acc.EnterpriseID
	}
	return h
}

func buildCreditBody(now time.Time) map[string]any {
	end := now.AddDate(101, 0, 0)
	format := func(t time.Time) string { return t.Format("2006-01-02 15:04:05") }
	return map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": format(now),
		"PackageEndTimeRangeEnd":   format(end),
	}
}

func billingError(status int, raw []byte) string {
	var payload map[string]any
	if json.Unmarshal(raw, &payload) == nil {
		for _, key := range []string{"msg", "message"} {
			if s, ok := payload[key].(string); ok && strings.TrimSpace(s) != "" {
				return fmt.Sprintf("WorkBuddy 积分接口 HTTP %d：%s", status, s)
			}
		}
	}
	text := strings.TrimSpace(string(raw))
	if len(text) > 200 {
		text = text[:200]
	}
	if text == "" {
		return fmt.Sprintf("WorkBuddy 积分接口 HTTP %d", status)
	}
	return fmt.Sprintf("WorkBuddy 积分接口 HTTP %d：%s", status, text)
}

func findAccounts(v any) []map[string]any {
	switch t := v.(type) {
	case map[string]any:
		if arr, ok := t["Accounts"].([]any); ok {
			out := make([]map[string]any, 0, len(arr))
			for _, item := range arr {
				if m, ok := item.(map[string]any); ok {
					out = append(out, m)
				}
			}
			if len(out) > 0 {
				return out
			}
		}
		for _, key := range []string{"data", "Response", "Data", "accounts"} {
			if child, ok := t[key]; ok {
				if found := findAccounts(child); len(found) > 0 {
					return found
				}
			}
		}
	case []any:
		for _, item := range t {
			if found := findAccounts(item); len(found) > 0 {
				return found
			}
		}
	}
	return nil
}

var (
	remainingFields = []string{
		"SlicePeriodCapacityRemainPrecise", "SlicePeriodCapacityRemain",
		"CycleCapacityRemainPrecise", "CycleCapacityRemain",
		"CapacityRemainPrecise", "CapacityRemain",
		"RemainPrecise", "Remain", "Remaining", "Balance",
	}
	totalFields = []string{
		"SlicePeriodCapacitySizePrecise", "SlicePeriodCapacitySize",
		"CycleCapacitySizePrecise", "CycleCapacitySize",
		"CycleCapacityPrecise", "CycleCapacity",
		"CapacityPrecise", "Capacity",
		"TotalCapacityPrecise", "TotalCapacity",
		"PackageCapacity", "Quota", "Amount",
	}
	expiryFields = []string{
		"DeductionEndTime", "ExpiredTime", "SlicePeriodEndTime", "PackageEndTime",
		"EndTime", "CycleEndTime", "ExpireTime", "ExpirationTime",
		"ValidEndTime", "ValidPeriodEndTime", "EndAt", "ExpireAt",
	}
	labelFields = []string{"PackageName", "PackageTypeName", "AccountName", "ProductName", "Name", "RuleName", "Description"}
)

func parsePersonalCredits(accounts []map[string]any) (CreditAccount, error) {
	out := CreditAccount{Count: len(accounts)}
	var sum float64
	var segments []CreditSegment
	for _, account := range accounts {
		details := []map[string]any{account}
		if arr, ok := account["SlicePeriodUsageDetails"].([]any); ok && len(arr) > 0 {
			details = details[:0]
			for _, item := range arr {
				detail, ok := item.(map[string]any)
				if !ok {
					continue
				}
				merged := make(map[string]any, len(account)+len(detail))
				for k, v := range account {
					merged[k] = v
				}
				for k, v := range detail {
					merged[k] = v
				}
				details = append(details, merged)
			}
		}
		for _, item := range details {
			remaining := firstNumber(item, remainingFields)
			if remaining == nil {
				continue
			}
			sum += *remaining
			if *remaining <= 0 {
				continue
			}
			total := firstNumber(item, totalFields)
			segment := CreditSegment{Remaining: round2(*remaining)}
			if total != nil {
				segment.Total = round2(*total)
			}
			segment.ExpiresAt = firstTimestamp(item, expiryFields)
			segment.Source = firstString(item, labelFields)
			segments = append(segments, segment)
		}
	}
	out.Credits = ptrFloat(round2(sum))
	out.Segments = mergeCreditSegments(segments)
	return out, nil
}

func parseEnterpriseCredits(payload map[string]any) (CreditAccount, error) {
	data := findEnterpriseUsage(payload)
	if data == nil {
		return CreditAccount{}, errors.New("WorkBuddy 企业积分接口没有返回配额数据")
	}
	limit := firstNumber(data, []string{"limitNum", "LimitNum"})
	if limit == nil {
		return CreditAccount{}, errors.New("WorkBuddy 企业积分接口缺少 limitNum")
	}
	reset := firstTimestamp(data, []string{"cycleResetTime", "CycleResetTime", "CycleResetTimeMs"})
	if *limit == -1 {
		return CreditAccount{Unlimited: true, Count: 1, ResetAt: reset}, nil
	}
	used := firstNumber(data, []string{"credit", "Credit"})
	if used == nil {
		used = ptrFloat(0)
	}
	remaining := round2(*limit - *used)
	if remaining < 0 {
		remaining = 0
	}
	return CreditAccount{
		Credits: ptrFloat(remaining), Total: ptrFloat(round2(*limit)), Count: 1,
		Segments: []CreditSegment{{Remaining: remaining, Total: round2(*limit), ExpiresAt: reset, Source: "企业配额"}},
		ResetAt:  reset,
	}, nil
}

func findEnterpriseUsage(v any) map[string]any {
	switch t := v.(type) {
	case map[string]any:
		if _, ok := t["limitNum"]; ok {
			return t
		}
		if _, ok := t["LimitNum"]; ok {
			return t
		}
		for _, key := range []string{"data", "Response", "Data"} {
			if child, ok := t[key]; ok {
				if found := findEnterpriseUsage(child); found != nil {
					return found
				}
			}
		}
	case []any:
		for _, item := range t {
			if found := findEnterpriseUsage(item); found != nil {
				return found
			}
		}
	}
	return nil
}

func mergeCreditSegments(segments []CreditSegment) []CreditSegment {
	type key struct {
		source    string
		expiresAt int64
	}
	merged := map[key]CreditSegment{}
	for _, segment := range segments {
		if segment.Remaining <= 0 {
			continue
		}
		k := key{source: segment.Source, expiresAt: segment.ExpiresAt}
		if previous, ok := merged[k]; ok {
			previous.Remaining = round2(previous.Remaining + segment.Remaining)
			previous.Total = round2(previous.Total + segment.Total)
			merged[k] = previous
		} else {
			merged[k] = segment
		}
	}
	out := make([]CreditSegment, 0, len(merged))
	for _, segment := range merged {
		out = append(out, segment)
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			left, right := out[i], out[j]
			if left.ExpiresAt == 0 && right.ExpiresAt != 0 {
				out[i], out[j] = right, left
			} else if left.ExpiresAt != 0 && right.ExpiresAt != 0 && right.ExpiresAt < left.ExpiresAt {
				out[i], out[j] = right, left
			}
		}
	}
	return out
}

func firstNumber(v map[string]any, fields []string) *float64 {
	for _, field := range fields {
		raw, ok := v[field]
		if !ok || raw == nil {
			continue
		}
		switch t := raw.(type) {
		case float64:
			return &t
		case int:
			f := float64(t)
			return &f
		case json.Number:
			if f, err := t.Float64(); err == nil {
				return &f
			}
		case string:
			if strings.TrimSpace(t) == "" {
				continue
			}
			var f float64
			if _, err := fmt.Sscanf(strings.TrimSpace(t), "%f", &f); err == nil {
				return &f
			}
		}
	}
	return nil
}

func firstString(v map[string]any, fields []string) string {
	for _, field := range fields {
		if s, ok := v[field].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func firstTimestamp(v map[string]any, fields []string) int64 {
	for _, field := range fields {
		raw, ok := v[field]
		if !ok || raw == nil {
			continue
		}
		switch t := raw.(type) {
		case float64:
			return normalizeTimestamp(int64(t))
		case int64:
			return normalizeTimestamp(t)
		case json.Number:
			if n, err := t.Int64(); err == nil {
				return normalizeTimestamp(n)
			}
		case string:
			s := strings.TrimSpace(t)
			if s == "" {
				continue
			}
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				return normalizeTimestamp(n)
			}
			for _, layout := range []string{
				time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02",
			} {
				if parsed, err := time.Parse(layout, s); err == nil {
					return parsed.UnixMilli()
				}
			}
		}
	}
	return 0
}

func normalizeTimestamp(n int64) int64 {
	if n > 0 && n < 1_000_000_000_000 {
		return n * 1000
	}
	return n
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

func ptrFloat(v float64) *float64 { return &v }
