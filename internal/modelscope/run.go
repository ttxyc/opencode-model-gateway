package modelscope

import (
	"context"
	"errors"
	"math/rand"
	"net/http"
	"strings"
	"time"
)

const defaultArticle = "435888"

func (m *Manager) enabledAccounts() []Account {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Account, 0, len(m.store.Accounts))
	for _, a := range m.store.Accounts {
		if a.Enabled {
			out = append(out, a)
		}
	}
	return out
}

func (m *Manager) finishRun(exit int, phase string) {
	m.mu.Lock()
	m.run.Running = false
	m.run.LastExit = exit
	m.run.FinishedAt = time.Now().Format(timeFmt)
	if m.run.Progress == nil {
		m.run.Progress = &Progress{}
	}
	m.run.Progress.Phase = phase
	m.run.Progress.UpdatedAt = time.Now().Format(time.RFC3339)
	m.mu.Unlock()
}

func (m *Manager) runAll(force bool) {
	ctx := context.Background()
	start := time.Now()
	accounts := m.enabledAccounts()
	if !force {
		m.mu.Lock()
		done := m.store.Daily.Date == time.Now().Format(todayFmt)
		m.mu.Unlock()
		if done {
			m.logf("", "今日已全部完成，跳过本次运行（--force 可强制重跑）")
			m.setProgress(0, 0, "", "done")
			m.finishRun(0, "done")
			return
		}
	}
	if len(accounts) == 0 {
		m.logf("", "没有启用的账号")
		m.setProgress(0, 0, "", "done")
		m.finishRun(1, "done")
		return
	}

	m.logf("", "=== 每日魔粒任务开始 ===")
	names := make([]string, 0, len(accounts))
	for _, a := range accounts {
		names = append(names, a.Name+"("+a.Article+")")
	}
	m.logf("", "待处理账号 %d 个: %s", len(accounts), strings.Join(names, ", "))
	m.setProgress(0, len(accounts), "", "running")

	allGranted := true
	allLikes := true
	done := 0
	for _, acct := range accounts {
		m.setProgress(done, len(accounts), acct.Name, "running")
		granted, likes := m.runAccount(ctx, acct)
		if !granted {
			allGranted = false
		}
		if !likes {
			allLikes = false
		}
		done++
		m.setProgress(done, len(accounts), "", "running")
	}

	if allGranted && allLikes {
		m.saveDaily(accounts)
		m.logf("", "今日（%s）任务全部完成，已记录状态", time.Now().Format(todayFmt))
	} else {
		m.logf("", "本次未全部完成，不记录“今日已完成”，后续触发仍会重试")
	}
	exit := 0
	if !allGranted {
		exit = 2
	}
	m.logf("", "=== 任务结束，用时 %s ===", time.Since(start).Round(time.Second))
	m.finishRun(exit, "done")
}

func (m *Manager) runAccount(ctx context.Context, acct Account) (bool, bool) {
	if acct.Article == "" {
		acct.Article = defaultArticle
	}
	m.logf(acct.Name, "账号 %s 开始", acct.Name)

	m.mu.Lock()
	loginFn := m.login
	m.mu.Unlock()
	if strings.TrimSpace(acct.Cookie) == "" {
		if loginFn == nil {
			m.logf(acct.Name, "缺少 Cookie，需要登录后重试")
			return false, false
		}
		if _, err := m.LoginAccount(ctx, acct.Name); err != nil {
			return false, false
		}
		if updated, ok := m.Find(acct.Name); ok {
			acct = updated
		}
	}

	m.pingActivity(ctx, acct, acct.Cookie)
	m.loginReport(ctx, acct, acct.Cookie)

	st, err := m.getStatus(ctx, acct, acct.Cookie)
	if err != nil {
		m.logf(acct.Name, "%v", err)
		if loginFn != nil {
			m.logf(acct.Name, "尝试自动重新登录…")
			if _, lerr := m.LoginAccount(ctx, acct.Name); lerr == nil {
				if updated, ok := m.Find(acct.Name); ok {
					acct = updated
				}
				st, err = m.getStatus(ctx, acct, acct.Cookie)
			}
		}
		if err != nil {
			return false, false
		}
	}
	m.logf(acct.Name, "今日状态: 余额=%v 登录200=%s 阿里云50=%s",
		st.Balance, grantedText(st.DailyGranted), grantedText(st.AliyunOK))

	if !st.DailyGranted {
		time.Sleep(5 * time.Second)
		m.loginReport(ctx, acct, acct.Cookie)
		st2, err2 := m.getStatus(ctx, acct, acct.Cookie)
		if err2 == nil {
			st = st2
		}
		m.logf(acct.Name, "登录上报后: 余额=%v 登录200=%s", st.Balance, grantedText(st.DailyGranted))
	}
	if st.DailyGranted {
		m.logf(acct.Name, "完成：今日每日魔粒已到账（200+50）")
	} else {
		m.logf(acct.Name, "结果：今日未确认发放，可稍后重试或手动打开一次网页")
	}

	likesOK := true
	if err := m.likeTask(ctx, acct); err != nil {
		likesOK = false
		m.logf(acct.Name, "喜欢任务失败: %v", err)
	}
	return st.DailyGranted, likesOK
}

func grantedText(ok bool) string {
	if ok {
		return "已发放"
	}
	return "未发放"
}

func (m *Manager) articleStarred(ctx context.Context, cookie, article string) bool {
	res, err := m.client.get(ctx, cookie, "/api/v1/articles/"+article)
	if err != nil || res.JSON == nil {
		return false
	}
	data := obj(res.JSON["Data"])
	if data == nil {
		return false
	}
	list := arr(data["Articles"])
	if len(list) == 0 {
		return false
	}
	starred, _ := obj(list[0])["AlreadyStar"].(bool)
	return starred
}

func (m *Manager) likeTask(ctx context.Context, acct Account) error {
	article := acct.Article
	if article == "" {
		article = defaultArticle
	}
	rules, err := m.client.get(ctx, acct.Cookie, "/openapi/v1/magicubes/earn/rules")
	if err != nil || rules.JSON == nil {
		m.logf(acct.Name, "喜欢任务：未取到规则，跳过")
		return nil
	}
	likeRule := findRule(rules.JSON["data"], "interaction_like")
	if likeRule == nil {
		m.logf(acct.Name, "喜欢任务：未取到 interaction_like 规则，跳过")
		return nil
	}
	remain, _ := getFloat(likeRule, "today_remain")
	used, _ := getFloat(likeRule, "today_used")
	earnedBefore, _ := getFloat(likeRule, "today_earned_amount")
	wanted := 20
	cycles := wanted
	if int(remain) < cycles {
		cycles = int(remain)
	}
	balBefore := 0.0
	if res, err := m.client.get(ctx, acct.Cookie, "/openapi/v1/magicubes/balance"); err == nil && res.JSON != nil {
		if b, ok := getFloat(obj(res.JSON["data"]), "available_balance"); ok {
			balBefore = b
		}
	}
	m.logf(acct.Name, "喜欢任务：文章 %s（账号固定），计划 %d 次，今日剩余 %d 次，本次执行 %d 次，当前余额=%v",
		article, wanted, int(remain), cycles, balBefore)
	if cycles <= 0 {
		return nil
	}
	referer := baseURL + "/posts/" + article
	if m.articleStarred(ctx, acct.Cookie, article) {
		m.logf(acct.Name, "检测到文章处于已收藏状态，先取消清理")
		_, _ = m.client.post(ctx, acct.Cookie, "/api/v1/articles/"+article+"/unstar", referer)
		time.Sleep(1500 * time.Millisecond)
	}

	ok := 0
	consecFail := 0
	aborted := false
	for i := 1; i <= cycles; i++ {
		res, err := m.client.post(ctx, acct.Cookie, "/api/v1/articles/"+article+"/star", referer)
		starred := err == nil && res.JSON != nil && res.JSON["Success"] == true
		if !starred && err == nil && (res.Status == http.StatusBadRequest || isResidualStar(res)) {
			m.logf(acct.Name, "喜欢被拒（残留已收藏），清理后重试")
			_, _ = m.client.post(ctx, acct.Cookie, "/api/v1/articles/"+article+"/unstar", referer)
			time.Sleep(1500 * time.Millisecond)
			res, err = m.client.post(ctx, acct.Cookie, "/api/v1/articles/"+article+"/star", referer)
			starred = err == nil && res.JSON != nil && res.JSON["Success"] == true
		}
		if !starred {
			consecFail++
			m.logf(acct.Name, "喜欢任务：第 %d/%d 次 喜欢失败，跳过该轮", i, cycles)
			if consecFail >= 3 {
				m.logf(acct.Name, "喜欢任务：连续 %d 轮失败，中止本账号任务", consecFail)
				aborted = true
				break
			}
			time.Sleep(3 * time.Second)
			continue
		}
		time.Sleep(time.Duration(1200+rand.Intn(800)) * time.Millisecond)
		unstar, err := m.client.post(ctx, acct.Cookie, "/api/v1/articles/"+article+"/unstar", referer)
		unstarred := err == nil && unstar.JSON != nil && unstar.JSON["Success"] == true
		if !unstarred {
			time.Sleep(1500 * time.Millisecond)
			unstar, err = m.client.post(ctx, acct.Cookie, "/api/v1/articles/"+article+"/unstar", referer)
			unstarred = err == nil && unstar.JSON != nil && unstar.JSON["Success"] == true
		}
		if unstarred {
			ok++
			consecFail = 0
		} else {
			consecFail++
			m.logf(acct.Name, "喜欢任务：第 %d/%d 次 取消喜欢失败，下轮会自动清理", i, cycles)
			if consecFail >= 3 {
				m.logf(acct.Name, "喜欢任务：连续 %d 轮失败，中止本账号任务", consecFail)
				aborted = true
				break
			}
		}
		m.logf(acct.Name, "喜欢任务：第 %d/%d 次 喜欢OK 取消%s", i, cycles, map[bool]string{true: "OK", false: "失败"}[unstarred])
		time.Sleep(time.Duration(1000+rand.Intn(800)) * time.Millisecond)
	}

	rulesAfter, _ := m.client.get(ctx, acct.Cookie, "/openapi/v1/magicubes/earn/rules")
	likeAfter := map[string]any(nil)
	if rulesAfter != nil && rulesAfter.JSON != nil {
		likeAfter = findRule(rulesAfter.JSON["data"], "interaction_like")
	}
	balAfter := 0.0
	if res, err := m.client.get(ctx, acct.Cookie, "/openapi/v1/magicubes/balance"); err == nil && res.JSON != nil {
		balAfter, _ = getFloat(obj(res.JSON["data"]), "available_balance")
	}
	usedAfter := used
	earnedAfter := earnedBefore
	if likeAfter != nil {
		usedAfter, _ = getFloat(likeAfter, "today_used")
		earnedAfter, _ = getFloat(likeAfter, "today_earned_amount")
	}
	m.logf(acct.Name, "喜欢任务完成：成功 %d/%d 次，今日已用 %d 次，余额=%v（+%v 魔粒）",
		ok, cycles, int(usedAfter), balAfter, earnedAfter-earnedBefore)
	if aborted {
		return errors.New("连续失败已中止")
	}
	return nil
}

func isResidualStar(res *apiResponse) bool {
	if res == nil || res.JSON == nil {
		return false
	}
	code, ok := getFloat(res.JSON, "Code")
	return ok && code == 10018401019
}

func (m *Manager) refreshAll() {
	ctx := context.Background()
	m.mu.Lock()
	accounts := make([]Account, 0, len(m.store.Accounts))
	for _, a := range m.store.Accounts {
		if a.Enabled {
			accounts = append(accounts, a)
		}
	}
	m.refresh.Progress = &Progress{Total: len(accounts), Phase: "running"}
	m.mu.Unlock()

	if len(accounts) == 0 {
		m.logf("", "没有启用的账号")
		m.finishRefresh(1, "done")
		return
	}
	failed := 0
	for i, acct := range accounts {
		m.mu.Lock()
		m.refresh.Progress.Done = i
		m.refresh.Progress.Current = acct.Name
		m.refresh.Progress.UpdatedAt = time.Now().Format(time.RFC3339)
		m.mu.Unlock()
		if strings.TrimSpace(acct.Cookie) == "" {
			m.logf(acct.Name, "没有 Cookie，需要登录")
			failed++
			continue
		}
		name, err := m.fetchLoginInfo(ctx, acct.Cookie)
		if err != nil {
			m.logf(acct.Name, "Cookie 已失效（%v），启动登录助手…", err)
			if loginName, lerr := m.LoginAccount(ctx, acct.Name); lerr == nil {
				m.logf(acct.Name, "Cookie 已更新（登录账号 %s）", loginName)
				continue
			}
			failed++
			continue
		}
		m.updateAccountCookie(acct.Name, acct.Cookie)
		m.logf(acct.Name, "Cookie 有效（登录账号 %s）", name)
	}
	m.mu.Lock()
	m.refresh.Progress.Done = len(accounts)
	m.refresh.Progress.Current = ""
	m.refresh.Progress.Phase = "done"
	m.refresh.Progress.UpdatedAt = time.Now().Format(time.RFC3339)
	m.mu.Unlock()
	exit := 0
	if failed > 0 {
		exit = 1
	}
	m.logf("", "Cookie 检查完成：%d/%d 有效", len(accounts)-failed, len(accounts))
	m.finishRefresh(exit, "done")
}

func (m *Manager) finishRefresh(exit int, phase string) {
	m.mu.Lock()
	m.refresh.Running = false
	m.refresh.LastExit = exit
	m.refresh.FinishedAt = time.Now().Format(timeFmt)
	if m.refresh.Progress != nil {
		m.refresh.Progress.Phase = phase
		m.refresh.Progress.UpdatedAt = time.Now().Format(time.RFC3339)
	}
	m.mu.Unlock()
}

func (m *Manager) saveDaily(accounts []Account) {
	names := make([]string, 0, len(accounts))
	for _, a := range accounts {
		names = append(names, a.Name)
	}
	m.saveDailyNames(names)
}

func (m *Manager) saveDailyNames(names []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.store.Daily = dailyState{
		Date:     time.Now().Format(todayFmt),
		DoneAt:   time.Now().Format(time.RFC3339),
		Accounts: names,
	}
	m.saveLocked()
}
