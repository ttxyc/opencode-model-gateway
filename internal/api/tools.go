package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ttxyc/opencode-model-gateway/internal/modelscope"
)

// toolsActionMu serializes WorkBuddy inspection/claim runs. They share result
// files and a single user task, so overlapping requests would race.
var toolsActionMu sync.Mutex

func (s *Server) handleTools(w http.ResponseWriter, r *http.Request, requestID, path string) int {
	switch path {
	case "/admin/api/tools/workbuddy/status":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "只支持 GET", "invalid_request_error", "method_not_allowed", requestID)
			return http.StatusMethodNotAllowed
		}
		if !toolsActionMu.TryLock() {
			writeError(w, http.StatusConflict, "已有 WorkBuddy 巡检/领取任务在进行中", "invalid_request_error", "tools_busy", requestID)
			return http.StatusConflict
		}
		defer toolsActionMu.Unlock()
		if s.wb == nil {
			writeError(w, http.StatusServiceUnavailable, "WorkBuddy 管理器未初始化", "gateway_error", "workbuddy_unavailable", requestID)
			return http.StatusServiceUnavailable
		}
		raw, err := s.wb.ToolStatus(r.Context())
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error(), "upstream_error", "tools_workbuddy_status_failed", requestID)
			return http.StatusBadGateway
		}
		writeRawJSON(w, http.StatusOK, raw)
		return http.StatusOK

	case "/admin/api/tools/workbuddy/claim":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "只支持 POST", "invalid_request_error", "method_not_allowed", requestID)
			return http.StatusMethodNotAllowed
		}
		if !toolsActionMu.TryLock() {
			writeError(w, http.StatusConflict, "已有 WorkBuddy 巡检/领取任务在进行中", "invalid_request_error", "tools_busy", requestID)
			return http.StatusConflict
		}
		defer toolsActionMu.Unlock()
		if s.wb == nil {
			writeError(w, http.StatusServiceUnavailable, "WorkBuddy 管理器未初始化", "gateway_error", "workbuddy_unavailable", requestID)
			return http.StatusServiceUnavailable
		}
		raw, err := s.wb.ToolClaim(r.Context())
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error(), "upstream_error", "tools_workbuddy_claim_failed", requestID)
			return http.StatusBadGateway
		}
		writeRawJSON(w, http.StatusOK, raw)
		return http.StatusOK

	case "/admin/api/tools/workbuddy/refresh-token":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "只支持 POST", "invalid_request_error", "method_not_allowed", requestID)
			return http.StatusMethodNotAllowed
		}
		if !toolsActionMu.TryLock() {
			writeError(w, http.StatusConflict, "已有 WorkBuddy 巡检/领取任务在进行中", "invalid_request_error", "tools_busy", requestID)
			return http.StatusConflict
		}
		defer toolsActionMu.Unlock()
		if s.wb == nil {
			writeError(w, http.StatusServiceUnavailable, "WorkBuddy 管理器未初始化", "gateway_error", "workbuddy_unavailable", requestID)
			return http.StatusServiceUnavailable
		}
		view, err := s.wb.RefreshToolAccount(r.Context())
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error(), "upstream_error", "tools_workbuddy_refresh_token_failed", requestID)
			return http.StatusBadGateway
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "expiresAt": view.ExpiresAt, "account": view})
		return http.StatusOK

	case "/admin/api/tools/workbuddy/panel":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "只支持 POST", "invalid_request_error", "method_not_allowed", requestID)
			return http.StatusMethodNotAllowed
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "WorkBuddy 数据已整合到当前工具中心页面"})
		return http.StatusOK

	case "/admin/api/tools/modelscope/status":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "只支持 GET", "invalid_request_error", "method_not_allowed", requestID)
			return http.StatusMethodNotAllowed
		}
		if s.ms == nil {
			writeError(w, http.StatusServiceUnavailable, "ModelScope 管理器未初始化", "gateway_error", "modelscope_unavailable", requestID)
			return http.StatusServiceUnavailable
		}
		data, err := s.ms.Status()
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error(), "upstream_error", "tools_modelscope_status_failed", requestID)
			return http.StatusBadGateway
		}
		writeJSON(w, http.StatusOK, data)
		return http.StatusOK

	case "/admin/api/tools/modelscope/run", "/admin/api/tools/modelscope/refresh-cookies":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "只支持 POST", "invalid_request_error", "method_not_allowed", requestID)
			return http.StatusMethodNotAllowed
		}
		action := "run"
		if strings.HasSuffix(path, "/refresh-cookies") {
			action = "refresh"
		}
		if s.ms == nil {
			writeError(w, http.StatusServiceUnavailable, "ModelScope 管理器未初始化", "gateway_error", "modelscope_unavailable", requestID)
			return http.StatusServiceUnavailable
		}
		var data map[string]any
		var err error
		if action == "run" {
			data, err = s.ms.StartRun(true)
		} else {
			data, err = s.ms.StartRefresh()
		}
		if err != nil {
			writeError(w, http.StatusConflict, err.Error(), "invalid_request_error", "tools_modelscope_busy", requestID)
			return http.StatusConflict
		}
		writeJSON(w, http.StatusOK, data)
		return http.StatusOK

	case "/admin/api/tools/modelscope/login":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "只支持 POST", "invalid_request_error", "method_not_allowed", requestID)
			return http.StatusMethodNotAllowed
		}
		if s.ms == nil {
			writeError(w, http.StatusServiceUnavailable, "ModelScope 管理器未初始化", "gateway_error", "modelscope_unavailable", requestID)
			return http.StatusServiceUnavailable
		}
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&body); err != nil || strings.TrimSpace(body.Name) == "" {
			writeError(w, http.StatusBadRequest, "缺少账号名", "invalid_request_error", "invalid_input", requestID)
			return http.StatusBadRequest
		}
		loginName, err := s.ms.LoginAccount(r.Context(), strings.TrimSpace(body.Name))
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error(), "upstream_error", "tools_modelscope_login_failed", requestID)
			return http.StatusBadGateway
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "loginName": loginName})
		return http.StatusOK

	case "/admin/api/tools/modelscope/account":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "只支持 POST", "invalid_request_error", "method_not_allowed", requestID)
			return http.StatusMethodNotAllowed
		}
		if s.ms == nil {
			writeError(w, http.StatusServiceUnavailable, "ModelScope 管理器未初始化", "gateway_error", "modelscope_unavailable", requestID)
			return http.StatusServiceUnavailable
		}
		var body struct {
			Name     string `json:"name"`
			MsName   string `json:"msName"`
			Password string `json:"password"`
			Cookie   string `json:"cookie"`
			Article  string `json:"article"`
			Enabled  *bool  `json:"enabled"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "JSON 无效", "invalid_request_error", "invalid_json", requestID)
			return http.StatusBadRequest
		}
		enabled := true
		if body.Enabled != nil {
			enabled = *body.Enabled
		}
		acc := modelscope.Account{
			Name: strings.TrimSpace(body.Name), MsName: strings.TrimSpace(body.MsName),
			Password: body.Password, Cookie: strings.TrimSpace(body.Cookie),
			Article: strings.TrimSpace(body.Article), Enabled: enabled,
		}
		if err := s.ms.Upsert(acc); err != nil {
			writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "tools_modelscope_account_failed", requestID)
			return http.StatusBadRequest
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return http.StatusOK

	case "/admin/api/tools/modelscope/remove":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "只支持 POST", "invalid_request_error", "method_not_allowed", requestID)
			return http.StatusMethodNotAllowed
		}
		if s.ms == nil {
			writeError(w, http.StatusServiceUnavailable, "ModelScope 管理器未初始化", "gateway_error", "modelscope_unavailable", requestID)
			return http.StatusServiceUnavailable
		}
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&body); err != nil || strings.TrimSpace(body.Name) == "" {
			writeError(w, http.StatusBadRequest, "缺少账号名", "invalid_request_error", "invalid_input", requestID)
			return http.StatusBadRequest
		}
		if err := s.ms.Remove(strings.TrimSpace(body.Name)); err != nil {
			writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "tools_modelscope_remove_failed", requestID)
			return http.StatusBadRequest
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return http.StatusOK
	}
	writeError(w, http.StatusNotFound, "未知工具接口", "invalid_request_error", "not_found", requestID)
	return http.StatusNotFound
}

func writeRawJSON(w http.ResponseWriter, status int, raw []byte) {
	if !json.Valid(raw) {
		writeError(w, http.StatusBadGateway, "工具返回的不是有效 JSON", "upstream_error", "tools_invalid_json", "")
		return
	}
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// toolTimeout is deliberately independent from the browser request so a
// refresh/reconnect does not abort the user task halfway.
const toolTimeout = 60 * time.Second
