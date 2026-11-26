package workbuddy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

// This file moves the old workbuddy-claim Node tooling into the gateway. The
// admin tools page calls these methods directly with the gateway's WorkBuddy
// account pool, so a new machine only needs the gateway itself.

const growthUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36"

type growthEndpoint struct {
	key    string
	method string
	path   string
}

var growthEndpoints = []growthEndpoint{
	{"tasks", "GET", "/v2/activity/growth/tasks"},
	{"profile", "GET", "/v2/activity/growth/profile"},
	{"streak", "GET", "/activity/growth/streak"},
	{"energy", "GET", "/activity/growth/energy"},
	{"lottery", "GET", "/activity/growth/lottery/summary"},
	{"buddy", "GET", "/activity/growth/buddy/info"},
	{"travel", "GET", "/activity/growth/buddy/travel/status"},
	{"gift", "POST", "/billing/meter/check-gift-claimed"},
}

func (m *Manager) pickToolAccount() (Account, bool) {
	for _, region := range []string{RegionCN, RegionGlobal} {
		accounts := m.Accounts(region)
		for _, a := range accounts {
			if a.Status != "invalid" && a.Status != "exhausted" {
				return a, true
			}
		}
		if len(accounts) > 0 {
			return accounts[0], true
		}
	}
	return Account{}, false
}

func (m *Manager) growthBase(acc Account) string {
	domain := strings.TrimSpace(acc.Domain)
	if domain == "" {
		if acc.Region == RegionGlobal {
			domain = "www.workbuddy.ai"
		} else {
			domain = "www.codebuddy.cn"
		}
	}
	return "https://" + domain
}

func (m *Manager) growthHeaders(acc Account, token string) map[string]string {
	return map[string]string{
		"accept":            "application/json",
		"content-type":      "application/json",
		"authorization":     "Bearer " + token,
		"user-agent":        growthUA,
		"x-domain":          acc.Domain,
		"x-client-platform": "web",
	}
}

func (m *Manager) growthJSON(ctx context.Context, acc Account, token, method, path string, body any) (json.RawMessage, error) {
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	status, raw, err := m.doJSON(ctx, method, m.growthBase(acc)+path, m.growthHeaders(acc, token), payload, 30*time.Second)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", status, truncateText(string(raw), 200))
	}
	var env struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, errors.New("上游返回格式无法解析")
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("%s (code %d)", env.Msg, env.Code)
	}
	if len(env.Data) == 0 {
		return json.RawMessage("null"), nil
	}
	return env.Data, nil
}

func truncateText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func decodeAny(raw json.RawMessage) any {
	var v any
	if len(raw) == 0 {
		return nil
	}
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	return v
}

// ToolStatus is the JSON consumed by /admin/tools (WorkBuddy card).
func (m *Manager) ToolStatus(ctx context.Context) ([]byte, error) {
	acc, ok := m.pickToolAccount()
	if !ok {
		return nil, errors.New("尚未登录 WorkBuddy 账号，请先在管理页登录")
	}
	token, err := m.EnsureAccess(ctx, &acc)
	if err != nil {
		return nil, err
	}

	data := map[string]any{"errors": []string{}}
	var errs []string

	type result struct {
		key  string
		data json.RawMessage
		err  error
	}
	results := make([]result, len(growthEndpoints))
	var wg sync.WaitGroup
	for i, ep := range growthEndpoints {
		wg.Add(1)
		go func(i int, ep growthEndpoint) {
			defer wg.Done()
			var body any
			if ep.method == "POST" {
				body = map[string]any{}
			}
			raw, err := m.growthJSON(ctx, acc, token, ep.method, ep.path, body)
			results[i] = result{key: ep.key, data: raw, err: err}
		}(i, ep)
	}
	wg.Wait()
	for _, r := range results {
		if r.err != nil {
			errs = append(errs, r.key+": "+r.err.Error())
			data[r.key] = nil
			continue
		}
		v := decodeAny(r.data)
		if r.key == "tasks" {
			if obj, ok := v.(map[string]any); ok {
				if tasks, ok := obj["tasks"]; ok {
					v = tasks
				}
			}
		}
		data[r.key] = v
	}
	if len(errs) == 0 {
		data["errors"] = []string{}
	} else {
		data["errors"] = errs
	}

	credits, cerr := m.FetchCredits(ctx, acc.Region)
	if cerr != nil {
		data["gateway"] = map[string]any{"ok": false, "error": cerr.Error()}
		data["billing"] = map[string]any{"ok": false, "error": cerr.Error()}
	} else {
		data["gateway"] = map[string]any{"ok": true, "data": credits}
		billing := map[string]any{"ok": true, "total": credits.Total}
		if len(credits.Accounts) > 0 {
			first := credits.Accounts[0]
			if first.Credits != nil {
				billing["total"] = *first.Credits
			}
			billing["segments"] = first.Segments
		}
		data["billing"] = billing
	}

	out := map[string]any{
		"session": map[string]any{
			"displayName": acc.Label,
			"userId":      acc.UserID,
			"expiresAt":   acc.ExpiresAt,
			"source":      "gateway",
		},
		"data": data,
	}
	return json.Marshal(out)
}

type claimAction struct {
	Key   string `json:"key"`
	Kind  string `json:"kind"`
	Label string `json:"label"`
	run   func() error
}

type claimResult struct {
	Key     string `json:"key"`
	Kind    string `json:"kind"`
	Label   string `json:"label"`
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

func num(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	case string:
		var f float64
		fmt.Sscanf(strings.TrimSpace(n), "%f", &f)
		return f
	default:
		return 0
	}
}

func str(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// ToolClaim mirrors `node claim.js --apply` (non-optional actions only).
func (m *Manager) RefreshToolAccount(ctx context.Context) (AccountView, error) {
	acc, ok := m.pickToolAccount()
	if !ok {
		return AccountView{}, errors.New("尚未登录 WorkBuddy 账号，请先在管理页登录")
	}
	return m.RefreshNow(ctx, acc.ID)
}

func (m *Manager) ToolClaim(ctx context.Context) ([]byte, error) {
	acc, ok := m.pickToolAccount()
	if !ok {
		return nil, errors.New("尚未登录 WorkBuddy 账号，请先在管理页登录")
	}
	token, err := m.EnsureAccess(ctx, &acc)
	if err != nil {
		return nil, err
	}

	tasksRaw, err := m.growthJSON(ctx, acc, token, "GET", "/v2/activity/growth/tasks", nil)
	if err != nil {
		return nil, err
	}
	streakRaw, _ := m.growthJSON(ctx, acc, token, "GET", "/activity/growth/streak", nil)
	energyRaw, _ := m.growthJSON(ctx, acc, token, "GET", "/activity/growth/energy", nil)
	lotteryRaw, _ := m.growthJSON(ctx, acc, token, "GET", "/activity/growth/lottery/summary", nil)
	travelRaw, _ := m.growthJSON(ctx, acc, token, "GET", "/activity/growth/buddy/travel/status", nil)

	var tasksDoc struct {
		Tasks []struct {
			Code         string  `json:"task_code"`
			Title        string  `json:"title"`
			RewardCredit float64 `json:"reward_credit"`
			AcceptStatus string  `json:"accept_status"`
			Progress     *struct {
				Current float64 `json:"current"`
				Target  float64 `json:"target"`
			} `json:"progress"`
		} `json:"tasks"`
	}
	_ = json.Unmarshal(tasksRaw, &tasksDoc)

	actions := make([]claimAction, 0, 8)
	for _, t := range tasksDoc.Tasks {
		done := t.AcceptStatus == "completed" ||
			(t.AcceptStatus == "accepted" && t.Progress != nil && t.Progress.Target > 0 && t.Progress.Current >= t.Progress.Target)
		if t.AcceptStatus != "claimed" && done {
			code := t.Code
			title := t.Title
			if title == "" {
				title = code
			}
			actions = append(actions, claimAction{
				Key: "task:" + code, Kind: "task", Label: "任务 " + title,
				run: func() error {
					_, err := m.growthJSON(ctx, acc, token, "POST", "/activity/growth/tasks/"+code+"/claim", map[string]any{})
					return err
				},
			})
		}
	}

	var streakDoc struct {
		RedemptionStatus map[string]any   `json:"redemption_status"`
		Tiers            []map[string]any `json:"tiers"`
	}
	_ = json.Unmarshal(streakRaw, &streakDoc)
	for _, tier := range []string{"7d", "14d", "28d"} {
		status := str(streakDoc.RedemptionStatus["tier_"+tier+"_status"])
		count := num(streakDoc.RedemptionStatus["tier_"+tier+"_count"])
		if status == "" || status == "locked" || count > 0 {
			continue
		}
		tierCopy := tier
		label := "连登兑换 " + tier
		for _, info := range streakDoc.Tiers {
			if str(info["tier"]) == tier {
				label = fmt.Sprintf("连登兑换 %s（%d 积分）", tier, int(num(info["credit"])))
				break
			}
		}
		actions = append(actions, claimAction{
			Key: "redeem:" + tierCopy, Kind: "redeem", Label: label,
			run: func() error {
				_, err := m.growthJSON(ctx, acc, token, "POST", "/activity/growth/redeem", map[string]any{
					"tier": tierCopy, "client_time": time.Now().Unix(),
				})
				return err
			},
		})
	}

	var lotteryDoc struct {
		Chances float64 `json:"chances"`
	}
	_ = json.Unmarshal(lotteryRaw, &lotteryDoc)
	if lotteryDoc.Chances > 0 {
		chances := int(math.Floor(lotteryDoc.Chances))
		actions = append(actions, claimAction{
			Key: "lottery", Kind: "lottery", Label: fmt.Sprintf("福利抽奖 ×%d", chances),
			run: func() error {
				for i := 0; i < chances; i++ {
					if _, err := m.growthJSON(ctx, acc, token, "POST", "/activity/growth/lottery/draw", map[string]any{}); err != nil {
						return err
					}
				}
				return nil
			},
		})
	}

	var energyDoc struct {
		Balance float64 `json:"balance"`
	}
	_ = json.Unmarshal(energyRaw, &energyDoc)
	if energyDoc.Balance >= 10 {
		times := int(math.Floor(energyDoc.Balance / 10))
		actions = append(actions, claimAction{
			Key: "buddy-open", Kind: "buddy", Label: fmt.Sprintf("开启 Buddy 盲盒 ×%d", times),
			run: func() error {
				for i := 0; i < times; i++ {
					if _, err := m.growthJSON(ctx, acc, token, "POST", "/activity/growth/buddy/open", map[string]any{}); err != nil {
						return err
					}
				}
				return nil
			},
		})
	}

	var travelDoc struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal(travelRaw, &travelDoc)
	if travelDoc.State == "arrived" || travelDoc.State == "returned" {
		actions = append(actions, claimAction{
			Key: "travel-claim", Kind: "travel", Label: "领取旅行奖励",
			run: func() error {
				_, err := m.growthJSON(ctx, acc, token, "POST", "/activity/growth/buddy/travel/claim", map[string]any{})
				return err
			},
		})
	}

	results := make([]claimResult, 0, len(actions))
	success, failed := 0, 0
	for _, a := range actions {
		res := claimResult{Key: a.Key, Kind: a.Kind, Label: a.Label}
		if err := a.run(); err != nil {
			res.Message = err.Error()
			failed++
		} else {
			res.OK = true
			success++
		}
		results = append(results, res)
	}
	out := map[string]any{"success": success, "failed": failed, "results": results}
	return json.Marshal(out)
}
