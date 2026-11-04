package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ttxyc/opencode-model-gateway/internal/config"
	"github.com/ttxyc/opencode-model-gateway/internal/logx"
	"github.com/ttxyc/opencode-model-gateway/internal/workbuddy"
)

func regionLabel(region string) string {
	if region == workbuddy.RegionGlobal {
		return "国际区"
	}
	return "国内区"
}

// handleWorkBuddyChat is a separate path from the generic key-pool handler.
// It never touches router state, balance probes, cooldowns or other providers.
func (s *Server) handleWorkBuddyChat(w http.ResponseWriter, r *http.Request, requestID, providerID string, provider *config.Provider, model string, body []byte, rawQuery string) int {
	if s.wb == nil {
		writeError(w, http.StatusInternalServerError, "WorkBuddy 管理器未初始化", "gateway_error", "workbuddy_unavailable", requestID)
		return http.StatusInternalServerError
	}
	region := workbuddy.RegionForKind(provider.Kind)
	w.Header().Set("x-gateway-provider", providerID)
	if workbuddy.StreamExplicitlyDisabled(body) {
		writeError(w, http.StatusBadRequest, "WorkBuddy 通道仅支持 stream:true", "invalid_request_error", "stream_required", requestID)
		return http.StatusBadRequest
	}
	if s.wb.Count(region) == 0 {
		writeError(w, http.StatusServiceUnavailable, "尚未登录 WorkBuddy "+regionLabel(region)+"账号，请打开 /admin 导入或登录", "insufficient_quota", "workbuddy_no_account", requestID)
		return http.StatusServiceUnavailable
	}
	prepared, err := workbuddy.PrepareBody(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "invalid_json", requestID)
		return http.StatusBadRequest
	}

	start := time.Now()
	tried := map[string]bool{}
	maxAttempts := s.wb.Count(region) + s.cfg.Limits.MaxRetries
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	if maxAttempts > 16 {
		maxAttempts = 16
	}
	var last workbuddy.Failure
	lastBody := ""
	channelRetried := false
	attempts := 0

	for attempts < maxAttempts {
		acc := s.wb.Pick(region, tried)
		if acc == nil {
			break
		}
		tried[acc.ID] = true
		attempts++

		ctx := r.Context()
		var cancel context.CancelFunc
		if s.cfg.Limits.TotalTimeoutMS > 0 {
			ctx, cancel = context.WithTimeout(ctx, time.Duration(s.cfg.Limits.TotalTimeoutMS)*time.Millisecond)
		}
		token, err := s.wb.EnsureAccess(ctx, acc)
		if err != nil {
			s.wb.Report(acc.ID, "invalid", err.Error(), 0)
			last = workbuddy.Failure{Kind: workbuddy.FailureAuth, Message: "WorkBuddy 登录已失效：" + err.Error(), Switchable: true}
			if cancel != nil {
				cancel()
			}
			continue
		}

		send := func(payload []byte) (*http.Response, error) {
			base := s.wb.ChatBase(acc.Region, acc.Domain)
			u := base + "/v2/chat/completions"
			if rawQuery != "" {
				u += "?" + rawQuery
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
			if err != nil {
				return nil, err
			}
			for k, v := range workbuddy.ChatHeaders(*acc, token) {
				req.Header.Set(k, v)
			}
			return s.wbClient.Do(req)
		}

		resp, err := send(prepared)
		if err != nil {
			if cancel != nil {
				cancel()
			}
			if r.Context().Err() != nil {
				s.log.Warn("client_aborted", map[string]any{"requestId": requestID, "provider": providerID})
				return 499
			}
			last = workbuddy.Failure{Kind: workbuddy.FailureServer, Message: "WorkBuddy 上游连接失败：" + err.Error(), Retryable: true, Switchable: true}
			s.log.Warn("workbuddy_upstream_error", map[string]any{
				"requestId": requestID, "provider": providerID, "account": acc.Label,
				"message": logx.Redact(err.Error(), 300),
			})
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			status := s.relay(w, r, resp, providerID, requestID, attempts, true)
			if cancel != nil {
				cancel()
			}
			s.requests.Add(1)
			s.log.Info("workbuddy_request_done", map[string]any{
				"requestId": requestID, "ok": true, "provider": providerID, "model": model,
				"account": acc.Label, "attempts": attempts, "rewritten": false,
				"stream": strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream"),
				"ms":     time.Since(start).Milliseconds(),
			})
			return status
		}
		raw := readLimited(resp.Body, 64*1024)
		_ = resp.Body.Close()
		f := workbuddy.Classify(resp.StatusCode, raw)
		last, lastBody = f, raw

		if f.Kind == workbuddy.FailureChannel && !channelRetried {
			channelRetried = true
			neutral, nerr := workbuddy.NeutralizeBody(prepared)
			if nerr == nil {
				resp2, err2 := send(neutral)
				if err2 == nil {
					if resp2.StatusCode >= 200 && resp2.StatusCode < 300 {
						status := s.relay(w, r, resp2, providerID, requestID, attempts, true)
						if cancel != nil {
							cancel()
						}
						s.requests.Add(1)
						s.log.Info("workbuddy_request_done", map[string]any{
							"requestId": requestID, "ok": true, "provider": providerID, "model": model,
							"account": acc.Label, "attempts": attempts, "rewritten": true,
							"stream": strings.Contains(resp2.Header.Get("Content-Type"), "text/event-stream"),
							"ms":     time.Since(start).Milliseconds(),
						})
						return status
					}
					raw2 := readLimited(resp2.Body, 64*1024)
					_ = resp2.Body.Close()
					f = workbuddy.Classify(resp2.StatusCode, raw2)
					last, lastBody = f, raw2
				}
			}
			if cancel != nil {
				cancel()
			}
			s.log.Warn("workbuddy_channel_reject", map[string]any{
				"requestId": requestID, "provider": providerID, "account": acc.Label,
				"code": f.Code, "rewritten": true,
			})
			return writeWorkBuddyFailure(w, requestID, f, lastBody)
		}

		switch f.Kind {
		case workbuddy.FailureAuth:
			s.wb.Report(acc.ID, "invalid", f.Message, 0)
			s.log.Warn("workbuddy_account_invalid", map[string]any{
				"requestId": requestID, "provider": providerID, "account": acc.Label, "code": f.Code,
			})
		case workbuddy.FailureCredit:
			millis := workbuddy.ResetCooldown(f.ResetAt).Milliseconds()
			s.wb.Report(acc.ID, "exhausted", f.Message, millis)
			s.log.Warn("workbuddy_account_exhausted", map[string]any{
				"requestId": requestID, "provider": providerID, "account": acc.Label,
				"code": f.Code, "resetAt": f.ResetAt,
			})
		case workbuddy.FailureAccount:
			s.wb.Report(acc.ID, "invalid", f.Message, 0)
			s.log.Warn("workbuddy_account_restricted", map[string]any{
				"requestId": requestID, "provider": providerID, "account": acc.Label, "code": f.Code,
			})
		case workbuddy.FailureBusy, workbuddy.FailureRate:
			if cancel != nil {
				cancel()
			}
			s.log.Warn("workbuddy_rate_limited", map[string]any{
				"requestId": requestID, "provider": providerID, "account": acc.Label,
				"kind": f.Kind, "code": f.Code, "resetAt": f.ResetAt,
			})
			return writeWorkBuddyFailure(w, requestID, f, lastBody)
		case workbuddy.FailureServer:
			if cancel != nil {
				cancel()
			}
			s.log.Warn("workbuddy_upstream_5xx", map[string]any{
				"requestId": requestID, "provider": providerID, "account": acc.Label,
				"status": f.Status,
			})
		default:
			if cancel != nil {
				cancel()
			}
			s.log.Warn("workbuddy_request_rejected", map[string]any{
				"requestId": requestID, "provider": providerID, "account": acc.Label,
				"status": f.Status, "code": f.Code, "message": logx.Redact(lastBody, 300),
			})
			return writeWorkBuddyFailure(w, requestID, f, lastBody)
		}
		if cancel != nil {
			cancel()
		}
	}

	if last.Kind == workbuddy.FailureAuth || last.Kind == workbuddy.FailureCredit || last.Kind == workbuddy.FailureAccount {
		writeError(w, http.StatusServiceUnavailable,
			"WorkBuddy "+regionLabel(region)+"账号池暂不可用："+last.Message,
			"insufficient_quota", "workbuddy_accounts_unavailable", requestID)
		return http.StatusServiceUnavailable
	}
	if last.Kind == "" {
		writeError(w, http.StatusServiceUnavailable, "没有可用的 WorkBuddy 账号", "insufficient_quota", "workbuddy_no_account", requestID)
		return http.StatusServiceUnavailable
	}
	return writeWorkBuddyFailure(w, requestID, last, lastBody)
}

func writeWorkBuddyFailure(w http.ResponseWriter, requestID string, f workbuddy.Failure, body string) int {
	message := f.Message
	if f.ResetAt != "" {
		message += "，上游预计于 " + f.ResetAt + "（UTC+8）重置"
	}
	switch f.Kind {
	case workbuddy.FailureChannel:
		writeError(w, http.StatusBadRequest, message, "invalid_request_error", "workbuddy_channel_reject", requestID)
		return http.StatusBadRequest
	case workbuddy.FailureRate:
		writeError(w, http.StatusTooManyRequests, message, "rate_limit_error", "workbuddy_rate_limited", requestID)
		return http.StatusTooManyRequests
	case workbuddy.FailureBusy:
		writeError(w, http.StatusTooManyRequests, message, "rate_limit_error", "workbuddy_model_busy", requestID)
		return http.StatusTooManyRequests
	case workbuddy.FailureClient:
		detail := strings.TrimSpace(body)
		if detail != "" {
			message += "：" + detail
		}
		writeError(w, http.StatusBadRequest, message, "invalid_request_error", "workbuddy_request_error", requestID)
		return http.StatusBadRequest
	default:
		writeError(w, http.StatusBadGateway, message, "upstream_error", "workbuddy_upstream_error", requestID)
		return http.StatusBadGateway
	}
}

func (s *Server) handleAdminWorkBuddy(w http.ResponseWriter, r *http.Request, requestID, path string) int {
	if s.wb == nil {
		writeError(w, http.StatusServiceUnavailable, "WorkBuddy 管理器未初始化", "gateway_error", "workbuddy_unavailable", requestID)
		return http.StatusServiceUnavailable
	}
	switch path {
	case "/admin/api/workbuddy/status":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "只支持 GET", "invalid_request_error", "method_not_allowed", requestID)
			return http.StatusMethodNotAllowed
		}
		accounts := s.wb.Snapshot()
		counts := map[string]int{workbuddy.RegionCN: 0, workbuddy.RegionGlobal: 0}
		for _, a := range accounts {
			counts[a.Region]++
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "accounts": accounts, "counts": counts, "pending": s.wb.Pending(),
		})
		return http.StatusOK

	case "/admin/api/workbuddy/import":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "只支持 POST", "invalid_request_error", "method_not_allowed", requestID)
			return http.StatusMethodNotAllowed
		}
		var body struct {
			Path string `json:"path"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "JSON 无效", "invalid_request_error", "invalid_json", requestID)
			return http.StatusBadRequest
		}
		res, err := s.wb.ImportDSH(strings.TrimSpace(body.Path))
		if err != nil && len(res.Files) == 0 {
			writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "workbuddy_import_failed", requestID)
			return http.StatusBadRequest
		}
		res.Accounts = s.wb.Snapshot()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": res})
		return http.StatusOK

	case "/admin/api/workbuddy/login/start":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "只支持 POST", "invalid_request_error", "method_not_allowed", requestID)
			return http.StatusMethodNotAllowed
		}
		var body struct {
			Region string `json:"region"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 16*1024)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "JSON 无效", "invalid_request_error", "invalid_json", requestID)
			return http.StatusBadRequest
		}
		res, err := s.wb.LoginStart(r.Context(), strings.TrimSpace(body.Region))
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error(), "upstream_error", "workbuddy_login_start_failed", requestID)
			return http.StatusBadGateway
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": res.State, "authUrl": res.AuthURL, "region": res.Region})
		return http.StatusOK

	case "/admin/api/workbuddy/login/poll":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "只支持 GET", "invalid_request_error", "method_not_allowed", requestID)
			return http.StatusMethodNotAllowed
		}
		state := strings.TrimSpace(r.URL.Query().Get("state"))
		if state == "" {
			writeError(w, http.StatusBadRequest, "缺少 state", "invalid_request_error", "invalid_input", requestID)
			return http.StatusBadRequest
		}
		res, err := s.wb.LoginPoll(r.Context(), state)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error(), "upstream_error", "workbuddy_login_failed", requestID)
			return http.StatusBadGateway
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": res.Status, "account": res.Account})
		return http.StatusOK

	case "/admin/api/workbuddy/remove":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "只支持 POST", "invalid_request_error", "method_not_allowed", requestID)
			return http.StatusMethodNotAllowed
		}
		var body struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 16*1024)).Decode(&body); err != nil || strings.TrimSpace(body.ID) == "" {
			writeError(w, http.StatusBadRequest, "缺少 id", "invalid_request_error", "invalid_input", requestID)
			return http.StatusBadRequest
		}
		if err := s.wb.Remove(body.ID); err != nil {
			writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "workbuddy_remove_failed", requestID)
			return http.StatusBadRequest
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": s.wb.Snapshot()})
		return http.StatusOK

	case "/admin/api/workbuddy/reset":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "只支持 POST", "invalid_request_error", "method_not_allowed", requestID)
			return http.StatusMethodNotAllowed
		}
		var body struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 16*1024)).Decode(&body); err != nil || strings.TrimSpace(body.ID) == "" {
			writeError(w, http.StatusBadRequest, "缺少 id", "invalid_request_error", "invalid_input", requestID)
			return http.StatusBadRequest
		}
		if err := s.wb.Reset(body.ID); err != nil {
			writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "workbuddy_reset_failed", requestID)
			return http.StatusBadRequest
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": s.wb.Snapshot()})
		return http.StatusOK

	case "/admin/api/workbuddy/models":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "只支持 GET", "invalid_request_error", "method_not_allowed", requestID)
			return http.StatusMethodNotAllowed
		}
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id == "" {
			writeError(w, http.StatusBadRequest, "缺少 id", "invalid_request_error", "invalid_input", requestID)
			return http.StatusBadRequest
		}
		models, err := s.wb.FetchModels(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error(), "upstream_error", "workbuddy_models_failed", requestID)
			return http.StatusBadGateway
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "models": models})
		return http.StatusOK
	}
	writeError(w, http.StatusNotFound, "未知 WorkBuddy 管理接口", "invalid_request_error", "not_found", requestID)
	return http.StatusNotFound
}

// handleWorkBuddyCredits is a client-authenticated (gateway token) endpoint so
// the local TUI balance plugin never needs to see WorkBuddy tokens.
func (s *Server) handleWorkBuddyCredits(w http.ResponseWriter, r *http.Request, requestID, providerID string) int {
	if s.wb == nil {
		writeError(w, http.StatusServiceUnavailable, "WorkBuddy 管理器未初始化", "gateway_error", "workbuddy_unavailable", requestID)
		return http.StatusServiceUnavailable
	}
	provider := s.cfg.Providers[providerID]
	if provider == nil || !workbuddy.IsKind(provider.Kind) {
		writeError(w, http.StatusNotFound, "该 provider 不支持 credits", "invalid_request_error", "not_found", requestID)
		return http.StatusNotFound
	}
	region := workbuddy.RegionForKind(provider.Kind)
	result, err := s.wb.FetchCredits(r.Context(), region)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error(), "insufficient_quota", "workbuddy_no_account", requestID)
		return http.StatusServiceUnavailable
	}
	if result.Total == nil && !result.Unlimited {
		message := result.Error
		if message == "" {
			message = "WorkBuddy 积分查询失败"
		}
		writeError(w, http.StatusBadGateway, message, "upstream_error", "workbuddy_credits_failed", requestID)
		return http.StatusBadGateway
	}
	w.Header().Set("x-gateway-provider", providerID)
	writeJSON(w, http.StatusOK, result)
	return http.StatusOK
}
