package workbuddy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CheckinStatus is the daily check-in activity state for one account.
type CheckinStatus struct {
	Active            bool     `json:"active"`
	TodayCheckedIn    bool     `json:"today_checked_in"`
	StreakDays        int      `json:"streak_days"`
	DailyCredit       float64  `json:"daily_credit"`
	TodayCredit       float64  `json:"today_credit"`
	IsStreakDay       bool     `json:"is_streak_day"`
	NextStreakDay     int      `json:"next_streak_day"`
	StreakBonusDays   int      `json:"streak_bonus_days"`
	StreakBonusCredit float64  `json:"streak_bonus_credit"`
	CheckinDates      []string `json:"checkin_dates"`
	WeekCheckinDays   int      `json:"week_checkin_days"`
	TotalCredits      float64  `json:"total_credits"`
	StartTime         string   `json:"start_time"`
	EndTime           string   `json:"end_time"`
	ActivityName      string   `json:"activity_name"`
	ThemeName         string   `json:"theme_name"`
}

type checkinClaimPayload struct {
	Credit      *float64 `json:"credit"`
	StreakDays  *int     `json:"streak_days"`
	IsStreakDay *bool    `json:"is_streak_day"`
}

type checkinClaim struct {
	Code        int
	Message     string
	Credit      float64
	StreakDays  int
	IsStreakDay bool
}

// CheckinAccountResult is the per-account result exposed by the admin API.
// Tokens are never included.
type CheckinAccountResult struct {
	ID             string   `json:"id"`
	Label          string   `json:"label,omitempty"`
	Region         string   `json:"region"`
	Domain         string   `json:"domain,omitempty"`
	State          string   `json:"state"`
	Active         bool     `json:"active"`
	TodayCheckedIn bool     `json:"todayCheckedIn"`
	Claimed        bool     `json:"claimed"`
	DryRun         bool     `json:"dryRun,omitempty"`
	Credit         float64  `json:"credit,omitempty"`
	StreakDays     int      `json:"streakDays,omitempty"`
	IsStreakDay    bool     `json:"isStreakDay,omitempty"`
	DailyCredit    float64  `json:"dailyCredit,omitempty"`
	CheckinDates   []string `json:"checkinDates,omitempty"`
	Message        string   `json:"message,omitempty"`
	Error          string   `json:"error,omitempty"`
}

// CheckinResult is the admin API response for one run.
type CheckinResult struct {
	OK          bool                   `json:"ok"`
	DryRun      bool                   `json:"dryRun"`
	Region      string                 `json:"region,omitempty"`
	Success     int                    `json:"success"`
	Already     int                    `json:"already"`
	Eligible    int                    `json:"eligible"`
	Skipped     int                    `json:"skipped"`
	Failed      int                    `json:"failed"`
	TotalCredit float64                `json:"totalCredit"`
	Results     []CheckinAccountResult `json:"results"`
}

func (m *Manager) checkinStatus(ctx context.Context, acc Account, token, deviceToken string) (CheckinStatus, error) {
	host := m.billingHost(acc)
	headers := billingHeaders(acc, token, host)
	if strings.TrimSpace(deviceToken) != "" {
		headers["x-device-token"] = strings.TrimSpace(deviceToken)
	}
	status, raw, err := m.doJSON(ctx, "POST", host+"/v2/billing/meter/checkin-activity-status", headers, []byte("{}"), 20*time.Second)
	if err != nil {
		return CheckinStatus{}, fmt.Errorf("WorkBuddy 签到状态接口连接失败: %w", err)
	}
	if status < 200 || status >= 300 {
		return CheckinStatus{}, errors.New(envelopeError(status, raw))
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || env.Code != 0 {
		return CheckinStatus{}, errors.New(envelopeError(status, raw))
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return CheckinStatus{}, errors.New("WorkBuddy 签到状态接口没有返回数据")
	}
	var data CheckinStatus
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return CheckinStatus{}, errors.New("WorkBuddy 签到状态返回格式无法解析")
	}
	return data, nil
}

func (m *Manager) claimDailyCheckin(ctx context.Context, acc Account, token, deviceToken string) (checkinClaim, error) {
	host := m.billingHost(acc)
	headers := billingHeaders(acc, token, host)
	if strings.TrimSpace(deviceToken) != "" {
		headers["x-device-token"] = strings.TrimSpace(deviceToken)
	}
	status, raw, err := m.doJSON(ctx, "POST", host+"/v2/billing/meter/daily-checkin", headers, []byte("{}"), 20*time.Second)
	if err != nil {
		return checkinClaim{}, fmt.Errorf("WorkBuddy 签到接口连接失败: %w", err)
	}

	var env envelope
	parseErr := json.Unmarshal(raw, &env)
	if parseErr != nil {
		if status < 200 || status >= 300 {
			return checkinClaim{}, errors.New(envelopeError(status, raw))
		}
		return checkinClaim{}, errors.New("WorkBuddy 签到返回格式无法解析")
	}

	out := checkinClaim{Code: env.Code, Message: env.Msg}
	if out.Message == "" {
		out.Message = env.Message
	}
	if env.Code != 0 {
		// Business codes (1001 already, 1002 not eligible, 1003 ended) are
		// handled by the caller; transport failures are handled above.
		return out, nil
	}
	if status < 200 || status >= 300 {
		return out, errors.New(envelopeError(status, raw))
	}

	// The desktop endpoint has been observed returning credit fields at the
	// top level, while the web/cloud shape wraps them in data. Accept both.
	var payload checkinClaimPayload
	if len(env.Data) > 0 && string(env.Data) != "null" {
		_ = json.Unmarshal(env.Data, &payload)
	}
	if payload.Credit == nil && payload.StreakDays == nil && payload.IsStreakDay == nil {
		_ = json.Unmarshal(raw, &payload)
	}
	if payload.Credit != nil {
		out.Credit = *payload.Credit
	}
	if payload.StreakDays != nil {
		out.StreakDays = *payload.StreakDays
	}
	if payload.IsStreakDay != nil {
		out.IsStreakDay = *payload.IsStreakDay
	}
	return out, nil
}

// RunCheckin performs the daily check-in for one account, one region, or all
// configured accounts. It never refreshes on a business error; EnsureAccess
// handles token refresh based on expiry.
func (m *Manager) RunCheckin(ctx context.Context, region, id, deviceToken string, dryRun bool) (CheckinResult, error) {
	region = strings.ToLower(strings.TrimSpace(region))
	if region == "" {
		region = "all"
	}
	if region != "all" && region != RegionCN && region != RegionGlobal {
		return CheckinResult{}, fmt.Errorf("region 只能是 all、cn 或 global")
	}

	var accounts []Account
	if id = strings.TrimSpace(id); id != "" {
		acc, ok := m.Find(id)
		if !ok {
			return CheckinResult{}, fmt.Errorf("账号不存在: %s", id)
		}
		accounts = []Account{acc}
	} else if region == "all" {
		accounts = append(accounts, m.Accounts(RegionCN)...)
		accounts = append(accounts, m.Accounts(RegionGlobal)...)
	} else {
		accounts = m.Accounts(region)
	}
	if len(accounts) == 0 {
		return CheckinResult{}, errors.New("没有可用的 WorkBuddy 账号")
	}

	res := CheckinResult{OK: true, DryRun: dryRun, Region: region, Results: make([]CheckinAccountResult, 0, len(accounts))}
	for i := range accounts {
		acc := accounts[i]
		item := CheckinAccountResult{
			ID: acc.ID, Label: acc.Label, Region: acc.Region, Domain: acc.Domain,
			DryRun: dryRun,
		}

		token, err := m.EnsureAccess(ctx, &acc)
		if err != nil {
			item.State = "failed"
			item.Error = err.Error()
			res.Failed++
			res.OK = false
			res.Results = append(res.Results, item)
			m.logWarn("workbuddy_checkin_account", map[string]any{
				"account": acc.ID, "label": acc.Label, "region": acc.Region,
				"state": item.State, "error": err.Error(),
			})
			continue
		}

		status, err := m.checkinStatus(ctx, acc, token, deviceToken)
		if err != nil {
			item.State = "failed"
			item.Error = err.Error()
			res.Failed++
			res.OK = false
			res.Results = append(res.Results, item)
			m.logWarn("workbuddy_checkin_account", map[string]any{
				"account": acc.ID, "label": acc.Label, "region": acc.Region,
				"state": item.State, "error": err.Error(),
			})
			continue
		}
		item.Active = status.Active
		item.TodayCheckedIn = status.TodayCheckedIn
		item.DailyCredit = status.DailyCredit
		item.StreakDays = status.StreakDays
		item.CheckinDates = status.CheckinDates
		item.Message = status.ActivityName
		if item.Label == "" {
			item.Label = acc.Label
		}

		if !status.Active {
			item.State = "skipped"
			item.Message = "签到活动未开启"
			res.Skipped++
			res.Results = append(res.Results, item)
			continue
		}
		if status.TodayCheckedIn {
			item.State = "already"
			item.Message = "今日已签到"
			res.Already++
			res.Results = append(res.Results, item)
			continue
		}
		if dryRun {
			item.State = "eligible"
			item.Message = fmt.Sprintf("可签到，预计 %.2f 积分", status.DailyCredit)
			res.Eligible++
			res.Results = append(res.Results, item)
			continue
		}

		claim, err := m.claimDailyCheckin(ctx, acc, token, deviceToken)
		if err != nil {
			item.State = "failed"
			item.Error = err.Error()
			res.Failed++
			res.OK = false
			res.Results = append(res.Results, item)
			m.logWarn("workbuddy_checkin_account", map[string]any{
				"account": acc.ID, "label": acc.Label, "region": acc.Region,
				"state": item.State, "error": err.Error(),
			})
			continue
		}

		switch claim.Code {
		case 0:
			item.State = "claimed"
			item.Claimed = true
			item.TodayCheckedIn = true
			item.Credit = claim.Credit
			item.StreakDays = claim.StreakDays
			item.IsStreakDay = claim.IsStreakDay
			item.Message = "签到成功"
			res.Success++
			res.TotalCredit += claim.Credit
		case 1001:
			item.State = "already"
			item.TodayCheckedIn = true
			item.Message = "今日已签到"
			res.Already++
		case 1002:
			item.State = "skipped"
			item.Message = "当前不可签到"
			if claim.Message != "" {
				item.Message += "：" + claim.Message
			}
			res.Skipped++
		case 1003:
			item.State = "skipped"
			item.Message = "签到活动已结束"
			res.Skipped++
		default:
			item.State = "failed"
			item.Error = fmt.Sprintf("签到失败(code=%d): %s", claim.Code, claim.Message)
			res.Failed++
			res.OK = false
		}
		res.Results = append(res.Results, item)
		m.logInfo("workbuddy_checkin_account", map[string]any{
			"account": acc.ID, "label": acc.Label, "region": acc.Region,
			"state": item.State, "active": item.Active, "todayCheckedIn": item.TodayCheckedIn,
			"credit": item.Credit, "streakDays": item.StreakDays,
			"dailyCredit": item.DailyCredit, "message": item.Message, "error": item.Error,
		})
	}

	m.logInfo("workbuddy_checkin", map[string]any{
		"region": region, "dryRun": dryRun, "success": res.Success, "already": res.Already,
		"eligible": res.Eligible, "skipped": res.Skipped, "failed": res.Failed,
		"totalCredit": res.TotalCredit,
	})
	return res, nil
}
