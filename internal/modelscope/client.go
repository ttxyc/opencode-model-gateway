package modelscope

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	baseURL   = "https://www.modelscope.cn"
	userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/153.0.0.0 Safari/537.36 Edg/153.0.0.0"
)

type apiClient struct {
	http *http.Client
}

type apiResponse struct {
	Status int
	JSON   map[string]any
	Text   string
}

func newAPIClient() *apiClient {
	return &apiClient{http: &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

func (c *apiClient) do(ctx context.Context, method, path, cookie, referer string) (*apiResponse, error) {
	if cookie == "" {
		return nil, errors.New("Cookie 为空")
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("cookie", cookie)
	req.Header.Set("user-agent", userAgent)
	req.Header.Set("accept", "application/json, text/plain, */*")
	if referer != "" {
		req.Header.Set("referer", referer)
	} else {
		req.Header.Set("referer", baseURL+"/")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return nil, err
	}
	out := &apiResponse{Status: resp.StatusCode, Text: string(raw)}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out.JSON)
	}
	return out, nil
}

func (c *apiClient) get(ctx context.Context, cookie, path string) (*apiResponse, error) {
	return c.do(ctx, http.MethodGet, path, cookie, baseURL+"/magicube/usage")
}

func (c *apiClient) post(ctx context.Context, cookie, path, referer string) (*apiResponse, error) {
	return c.do(ctx, http.MethodPost, path, cookie, referer)
}

func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func arr(v any) []any {
	a, _ := v.([]any)
	return a
}

func getStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

func getFloat(m map[string]any, keys ...string) (float64, bool) {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch n := v.(type) {
		case float64:
			return n, true
		case int:
			return float64(n), true
		case string:
			var f float64
			if _, err := fmt.Sscanf(strings.TrimSpace(n), "%f", &f); err == nil {
				return f, true
			}
		}
	}
	return 0, false
}

func findRule(data any, key string) map[string]any {
	for _, item := range arr(data) {
		m := obj(item)
		if m != nil && getStr(m, "rule_key") == key {
			return m
		}
	}
	return nil
}

func (m *Manager) fetchLoginInfo(ctx context.Context, cookie string) (string, error) {
	res, err := m.client.get(ctx, cookie, "/api/v1/users/login/info")
	if err != nil {
		return "", err
	}
	if res.Status != http.StatusOK || res.JSON == nil {
		return "", fmt.Errorf("HTTP %d", res.Status)
	}
	data := obj(res.JSON["Data"])
	if data == nil {
		return "", errors.New("登录态无效")
	}
	name := getStr(data, "Name", "NickName")
	if name == "" {
		return "", errors.New("登录态无效")
	}
	return name, nil
}

func (m *Manager) pingActivity(ctx context.Context, acc Account, cookie string) {
	paths := []string{
		"/openapi/v1/magicubes/balance",
		"/openapi/v1/magicubes/earn/rules",
		"/api/v1/users/messages/summary",
		"/api/v1/users/binding/check",
		"/api/v1/users/authorized/check",
	}
	for _, p := range paths {
		res, err := m.client.get(ctx, cookie, p)
		if err != nil {
			m.logf(acc.Name, "ping %s 失败: %v", p, err)
			continue
		}
		m.logf(acc.Name, "ping %s -> HTTP %d", p, res.Status)
	}
}

func (m *Manager) loginReport(ctx context.Context, acc Account, cookie string) {
	paths := []string{
		"/api/v1/users/login/info",
		"/api/v1/medal/user/check",
		"/api/v1/dolphin/agg/query",
		"/api/v1/console/config/list/business/global-config",
		"/api/v1/medal/pendant?Status=worn&Category=avatar",
	}
	for _, p := range paths {
		res, err := m.client.get(ctx, cookie, p)
		if err != nil {
			m.logf(acc.Name, "登录上报 %s 失败: %v", p, err)
			continue
		}
		m.logf(acc.Name, "登录上报 %s -> HTTP %d", p, res.Status)
	}
}

type accountStatus struct {
	Balance      float64
	DailyGranted bool
	AliyunOK     bool
}

func (m *Manager) getStatus(ctx context.Context, acc Account, cookie string) (accountStatus, error) {
	res, err := m.client.get(ctx, cookie, "/openapi/v1/magicubes/balance")
	if err != nil {
		return accountStatus{}, err
	}
	if res.JSON == nil || res.JSON["success"] != true {
		return accountStatus{}, fmt.Errorf("登录态异常: HTTP %d %s", res.Status, truncate(res.Text, 150))
	}
	balance, _ := getFloat(obj(res.JSON["data"]), "available_balance")

	rules, err := m.client.get(ctx, cookie, "/openapi/v1/magicubes/earn/rules")
	if err != nil {
		return accountStatus{Balance: balance}, err
	}
	var daily, aliyun map[string]any
	if rules.JSON != nil {
		daily = findRule(rules.JSON["data"], "daily_active")
		aliyun = findRule(rules.JSON["data"], "aliyun_bindlogin")
	}
	dailyGranted := false
	if daily != nil {
		n, _ := getFloat(daily, "today_used")
		dailyGranted = n >= 1
	}
	aliyunOK := true
	if aliyun != nil {
		n, _ := getFloat(aliyun, "today_used")
		aliyunOK = n >= 1
	}
	return accountStatus{Balance: balance, DailyGranted: dailyGranted, AliyunOK: aliyunOK}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
