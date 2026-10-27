package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ttxyc/opencode-model-gateway/internal/logx"
	"github.com/ttxyc/opencode-model-gateway/internal/router"
)

type importSkip struct {
	Provider string `json:"provider"`
	Key      string `json:"key"`
	Reason   string `json:"reason"`
}

func (s *Server) handleImportKeys(w http.ResponseWriter, r *http.Request, requestID string) int {
	var body struct {
		Content   string `json:"content"`
		Overwrite bool   `json:"overwrite"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 2*1024*1024)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "JSON 无效", "invalid_request_error", "invalid_json", requestID)
		return http.StatusBadRequest
	}
	content := strings.TrimPrefix(strings.TrimSpace(body.Content), "\ufeff")
	if content == "" {
		writeError(w, http.StatusBadRequest, "文件内容为空", "invalid_request_error", "empty_content", requestID)
		return http.StatusBadRequest
	}
	if len(content) > 1024*1024 {
		writeError(w, http.StatusRequestEntityTooLarge, "key 配置文件超过 1MB", "invalid_request_error", "file_too_large", requestID)
		return http.StatusRequestEntityTooLarge
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &top); err != nil {
		writeError(w, http.StatusBadRequest, "不是合法 JSON 文件", "invalid_request_error", "invalid_json", requestID)
		return http.StatusBadRequest
	}
	if wrapped, ok := top["providers"]; ok {
		var m map[string]json.RawMessage
		if json.Unmarshal(wrapped, &m) == nil {
			top = m
		}
	}
	type section struct {
		Keys []string `json:"keys"`
		Key  string   `json:"key"`
	}
	valid := map[string][]string{}
	var skipped []importSkip
	for provider, raw := range top {
		var sec section
		if json.Unmarshal(raw, &sec) != nil {
			skipped = append(skipped, importSkip{Provider: provider, Reason: "配置段不是对象"})
			continue
		}
		keys := sec.Keys
		if len(keys) == 0 && sec.Key != "" {
			keys = []string{sec.Key}
		}
		if _, ok := s.cfg.Providers[provider]; !ok {
			skipped = append(skipped, importSkip{Provider: provider, Reason: "未知服务商"})
			continue
		}
		existing := map[string]bool{}
		for _, k := range s.router.Keys(provider) {
			existing[k] = true
		}
		seen := map[string]bool{}
		for _, key := range keys {
			key = strings.TrimSpace(key)
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			if existing[key] {
				skipped = append(skipped, importSkip{Provider: provider, Key: router.Mask(key), Reason: "已存在"})
				continue
			}
			ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
			validated, err := s.router.ValidateKey(ctx, provider, key)
			cancel()
			if err != nil {
				skipped = append(skipped, importSkip{Provider: provider, Key: router.Mask(key), Reason: logx.Redact(err.Error(), 300)})
				continue
			}
			valid[provider] = append(valid[provider], key)
			_ = validated
		}
	}
	reports, err := s.router.ImportKeys(valid, body.Overwrite)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "写入 key 配置失败: "+err.Error(), "gateway_error", "import_write_failed", requestID)
		return http.StatusInternalServerError
	}
	for provider := range valid {
		s.router.RefreshProvider(context.Background(), provider, true)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "reports": reports, "skipped": skipped, "providers": s.router.Status(),
	})
	return http.StatusOK
}
