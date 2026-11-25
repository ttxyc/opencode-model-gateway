package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
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
		raw, err := workBuddyToolStatus()
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
		raw, err := workBuddyToolClaim()
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
		raw, err := workBuddyToolRefreshToken()
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error(), "upstream_error", "tools_workbuddy_refresh_token_failed", requestID)
			return http.StatusBadGateway
		}
		writeRawJSON(w, http.StatusOK, raw)
		return http.StatusOK

	case "/admin/api/tools/workbuddy/panel":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "只支持 POST", "invalid_request_error", "method_not_allowed", requestID)
			return http.StatusMethodNotAllowed
		}
		if err := workBuddyToolPanel(); err != nil {
			writeError(w, http.StatusBadGateway, err.Error(), "upstream_error", "tools_workbuddy_panel_failed", requestID)
			return http.StatusBadGateway
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "已在独立窗口打开 WorkBuddy 积分助手"})
		return http.StatusOK

	case "/admin/api/tools/modelscope/status":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "只支持 GET", "invalid_request_error", "method_not_allowed", requestID)
			return http.StatusMethodNotAllowed
		}
		data, err := modelScopeToolStatus()
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
		data, err := modelScopeToolAction(action)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error(), "upstream_error", "tools_modelscope_action_failed", requestID)
			return http.StatusBadGateway
		}
		writeJSON(w, http.StatusOK, data)
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
