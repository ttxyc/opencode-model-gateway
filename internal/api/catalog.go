package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ttxyc/opencode-model-gateway/internal/config"
	"github.com/ttxyc/opencode-model-gateway/internal/logx"
	"github.com/ttxyc/opencode-model-gateway/internal/workbuddy"
)

const catalogFileVersion = 1

// modelsDevURL is a var so tests can point it at a local fixture.
var modelsDevURL = "https://models.dev/api.json"

type catalogState struct {
	Version   int                              `json:"version"`
	UpdatedAt string                           `json:"updatedAt,omitempty"`
	Providers map[string]*catalogProviderState `json:"providers"`
}

type catalogProviderState struct {
	Models []string                     `json:"models"`
	Meta   map[string]*config.ModelMeta `json:"meta,omitempty"`
}

type providerSnapshot struct {
	ID     string
	Label  string
	Kind   string
	Models []string
	Meta   map[string]*config.ModelMeta
}

type catalogModel struct {
	ID         string            `json:"id"`
	Name       string            `json:"name,omitempty"`
	Configured bool              `json:"configured"`
	Upstream   bool              `json:"upstream"`
	Meta       *config.ModelMeta `json:"meta,omitempty"`
}

type catalogProvider struct {
	ID         string         `json:"id"`
	Label      string         `json:"label"`
	Kind       string         `json:"kind,omitempty"`
	Configured []string       `json:"configured"`
	Models     []catalogModel `json:"models"`
	Fetched    bool           `json:"fetched"`
	Error      string         `json:"error,omitempty"`
}

type detectedMeta struct {
	Meta   config.ModelMeta
	Fields map[string]bool
	Name   string
}

func (s *Server) catalogPath() string {
	return filepath.Join(filepath.Dir(s.cfg.Paths.State), "model-catalog.json")
}

// loadCatalog overlays the admin-managed model list and metadata on top of
// gateway.json. Keeping it in the state directory avoids rewriting the config
// file (and its ACLs) on every catalog update.
func (s *Server) loadCatalog() {
	raw, err := os.ReadFile(s.catalogPath())
	if err != nil {
		return
	}
	var st catalogState
	if err := json.Unmarshal(raw, &st); err != nil {
		s.log.Warn("model_catalog_load_failed", map[string]any{"error": logx.Redact(err.Error(), 300)})
		return
	}
	for pid, ps := range st.Providers {
		p := s.cfg.Providers[pid]
		if p == nil || ps == nil {
			continue
		}
		if ps.Models != nil {
			p.Models = append([]string(nil), ps.Models...)
		}
		if ps.Meta != nil {
			p.ModelMeta = cloneMetaMap(ps.Meta)
		}
	}
}

func (s *Server) snapshotProviders() []providerSnapshot {
	s.modelsMu.RLock()
	defer s.modelsMu.RUnlock()
	out := make([]providerSnapshot, 0, len(s.cfg.Providers))
	for id, p := range s.cfg.Providers {
		out = append(out, providerSnapshot{
			ID: id, Label: p.Label, Kind: p.Kind,
			Models: append([]string(nil), p.Models...),
			Meta:   cloneMetaMap(p.ModelMeta),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Server) snapshotProvider(id string) (providerSnapshot, bool) {
	s.modelsMu.RLock()
	defer s.modelsMu.RUnlock()
	p := s.cfg.Providers[id]
	if p == nil {
		return providerSnapshot{}, false
	}
	return providerSnapshot{
		ID: id, Label: p.Label, Kind: p.Kind,
		Models: append([]string(nil), p.Models...),
		Meta:   cloneMetaMap(p.ModelMeta),
	}, true
}

func cloneMetaMap(in map[string]*config.ModelMeta) map[string]*config.ModelMeta {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]*config.ModelMeta, len(in))
	for id, m := range in {
		out[id] = m.Clone()
	}
	return out
}

func (s *Server) catalogSnapshot() []catalogProvider {
	snaps := s.snapshotProviders()
	out := make([]catalogProvider, 0, len(snaps))
	for _, p := range snaps {
		cp := catalogProvider{ID: p.ID, Label: p.Label, Kind: p.Kind, Configured: append([]string(nil), p.Models...)}
		for _, m := range p.Models {
			cp.Models = append(cp.Models, catalogModel{ID: m, Configured: true, Meta: p.Meta[m]})
		}
		out = append(out, cp)
	}
	return out
}

func (s *Server) handleCatalogGet(w http.ResponseWriter, r *http.Request) int {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "只支持 GET", "invalid_request_error", "method_not_allowed", "")
		return http.StatusMethodNotAllowed
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "providers": s.catalogSnapshot()})
	return http.StatusOK
}

func (s *Server) handleCatalogCheck(w http.ResponseWriter, r *http.Request, requestID string) int {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "只支持 POST", "invalid_request_error", "method_not_allowed", requestID)
		return http.StatusMethodNotAllowed
	}
	var body struct {
		Provider  string `json:"provider"`
		Overwrite bool   `json:"overwrite"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "JSON 无效", "invalid_request_error", "invalid_json", requestID)
		return http.StatusBadRequest
	}
	body.Provider = strings.TrimSpace(body.Provider)
	snaps := s.snapshotProviders()
	if body.Provider != "" {
		filtered := snaps[:0]
		for _, p := range snaps {
			if p.ID == body.Provider {
				filtered = append(filtered, p)
			}
		}
		snaps = filtered
		if len(snaps) == 0 {
			writeError(w, http.StatusNotFound, "未知服务商: "+body.Provider, "invalid_request_error", "provider_not_found", requestID)
			return http.StatusNotFound
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	index, mdStatus := s.marketIndex(ctx)

	type result struct {
		cp catalogProvider
	}
	ch := make(chan result, len(snaps))
	sem := make(chan struct{}, 4)
	for _, p := range snaps {
		p := p
		go func() {
			defer func() {
				if v := recover(); v != nil {
					ch <- result{cp: catalogProvider{
						ID: p.ID, Label: p.Label, Kind: p.Kind,
						Configured: append([]string(nil), p.Models...),
						Error:      fmt.Sprintf("内部错误: %v", v),
					}}
				}
			}()
			sem <- struct{}{}
			defer func() { <-sem }()
			ch <- result{cp: s.buildCatalogProvider(ctx, p, index, body.Overwrite)}
		}()
	}
	out := make([]catalogProvider, 0, len(snaps))
	for range snaps {
		out = append(out, (<-ch).cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "checkedAt": time.Now().UTC().Format(time.RFC3339),
		"modelsDev": mdStatus, "providers": out,
	})
	return http.StatusOK
}

func (s *Server) buildCatalogProvider(ctx context.Context, p providerSnapshot, index map[string][]mdHit, overwrite bool) catalogProvider {
	cp := catalogProvider{
		ID: p.ID, Label: p.Label, Kind: p.Kind,
		Configured: append([]string(nil), p.Models...),
	}
	upstream, err := s.fetchProviderModels(ctx, p)
	if err != nil {
		cp.Error = err.Error()
	} else {
		cp.Fetched = true
	}

	enrich := func(id string, base *config.ModelMeta) (*config.ModelMeta, string) {
		det := upstream[id]
		name := det.Name
		if md, ok := matchMarket(index, p.ID, id); ok {
			if det.Fields == nil {
				det = md
			} else {
				mergeDetected(&det, md)
			}
			if name == "" {
				name = md.Name
			}
		}
		if det.Fields == nil {
			return base.Clone(), name
		}
		return mergeDetectedMeta(base, det, overwrite), name
	}

	seen := map[string]bool{}
	for _, id := range p.Models {
		seen[id] = true
		meta, name := enrich(id, p.Meta[id])
		_, isUpstream := upstream[id]
		cp.Models = append(cp.Models, catalogModel{ID: id, Name: name, Configured: true, Upstream: isUpstream, Meta: meta})
	}
	extra := make([]string, 0)
	for id := range upstream {
		if !seen[id] {
			extra = append(extra, id)
		}
	}
	sort.Strings(extra)
	for _, id := range extra {
		meta, name := enrich(id, nil)
		cp.Models = append(cp.Models, catalogModel{ID: id, Name: name, Upstream: true, Meta: meta})
	}
	return cp
}

func (s *Server) fetchProviderModels(ctx context.Context, p providerSnapshot) (map[string]detectedMeta, error) {
	if workbuddy.IsKind(p.Kind) {
		if s.wb == nil {
			return nil, errors.New("WorkBuddy 管理器未初始化")
		}
		region := workbuddy.RegionForKind(p.Kind)
		accounts := s.wb.Accounts(region)
		if len(accounts) == 0 {
			return nil, fmt.Errorf("尚未登录 WorkBuddy %s账号", region)
		}
		infos, err := s.wb.FetchModels(ctx, accounts[0].ID)
		if err != nil {
			return nil, err
		}
		out := map[string]detectedMeta{}
		for _, m := range infos {
			out[m.ID] = detectWorkBuddyModel(m)
		}
		return out, nil
	}

	keys := s.router.Keys(p.ID)
	if len(keys) == 0 {
		return nil, errors.New("该平台没有可用 key，无法读取上游模型列表")
	}
	var lastErr error
	tried := 0
	for _, key := range keys {
		if tried >= 3 {
			break
		}
		tried++
		models, err := s.fetchOpenAIModelList(ctx, p, key)
		if err == nil {
			return models, nil
		}
		lastErr = err
		if !strings.Contains(err.Error(), "HTTP 401") && !strings.Contains(err.Error(), "HTTP 403") {
			break
		}
	}
	if lastErr == nil {
		lastErr = errors.New("读取上游模型列表失败")
	}
	return nil, lastErr
}

func (s *Server) fetchOpenAIModelList(ctx context.Context, p providerSnapshot, key string) (map[string]detectedMeta, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(s.cfg.Providers[p.ID].BaseURL, "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, cleanErrorText(string(raw)))
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("上游模型列表不是 JSON: %w", err)
	}
	list := findModelArray(doc, 0)
	out := map[string]detectedMeta{}
	for _, item := range list {
		id, _ := item["id"].(string)
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		out[id] = detectOpenAIModel(item)
	}
	return out, nil
}

func (s *Server) handleCatalogApply(w http.ResponseWriter, r *http.Request, requestID string) int {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "只支持 POST", "invalid_request_error", "method_not_allowed", requestID)
		return http.StatusMethodNotAllowed
	}
	var body struct {
		Providers map[string]struct {
			Models []string                     `json:"models"`
			Meta   map[string]*config.ModelMeta `json:"meta"`
		} `json:"providers"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 2*1024*1024)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "JSON 无效", "invalid_request_error", "invalid_json", requestID)
		return http.StatusBadRequest
	}
	if len(body.Providers) == 0 {
		writeError(w, http.StatusBadRequest, "没有要保存的模型目录", "invalid_request_error", "invalid_input", requestID)
		return http.StatusBadRequest
	}
	snaps := s.snapshotProviders()
	current := make(map[string]providerSnapshot, len(snaps))
	for _, p := range snaps {
		current[p.ID] = p
	}
	st := catalogState{Version: catalogFileVersion, UpdatedAt: time.Now().UTC().Format(time.RFC3339), Providers: map[string]*catalogProviderState{}}
	for _, p := range snaps {
		st.Providers[p.ID] = &catalogProviderState{Models: append([]string(nil), p.Models...), Meta: cloneMetaMap(p.Meta)}
	}
	for pid, upd := range body.Providers {
		p, ok := current[pid]
		if !ok {
			writeError(w, http.StatusBadRequest, "未知服务商: "+pid, "invalid_request_error", "provider_not_found", requestID)
			return http.StatusBadRequest
		}
		models := normalizeModelList(upd.Models)
		meta := map[string]*config.ModelMeta{}
		for _, id := range models {
			if mm := mergeManualMeta(p.Meta[id], upd.Meta[id]); mm != nil {
				meta[id] = mm
			}
		}
		st.Providers[pid] = &catalogProviderState{Models: models, Meta: meta}
	}
	if err := s.saveCatalog(st); err != nil {
		writeError(w, http.StatusInternalServerError, "保存模型目录失败: "+err.Error(), "gateway_error", "catalog_save_failed", requestID)
		return http.StatusInternalServerError
	}
	s.modelsMu.Lock()
	for pid, ps := range st.Providers {
		p := s.cfg.Providers[pid]
		if p == nil {
			continue
		}
		p.Models = append([]string(nil), ps.Models...)
		p.ModelMeta = cloneMetaMap(ps.Meta)
	}
	s.modelsMu.Unlock()
	s.log.Info("model_catalog_saved", map[string]any{"providers": len(body.Providers)})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "providers": s.catalogSnapshot()})
	return http.StatusOK
}

func (s *Server) saveCatalog(st catalogState) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	path := s.catalogPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func normalizeModelList(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, id := range in {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func mergeManualMeta(existing, incoming *config.ModelMeta) *config.ModelMeta {
	out := existing.Clone()
	if out == nil {
		out = &config.ModelMeta{}
	}
	if incoming == nil {
		return out
	}
	if out.Sources == nil {
		out.Sources = map[string]string{}
	}
	changed := false
	apply := func(field string, changedValue bool, incomingSource string, set func()) {
		src := sanitizeSource(incomingSource)
		if !metaFieldKnown(out, field) {
			if src != "" {
				set()
				out.Sources[field] = src
				changed = true
				return
			}
			if !changedValue {
				return
			}
		} else if !changedValue {
			if out.Sources[field] == "" && src != "" {
				out.Sources[field] = src
				changed = true
			}
			return
		}
		set()
		out.Sources[field] = "manual"
		changed = true
	}
	if incoming.Context != out.Context {
		ctx := maxInt64(incoming.Context, 0)
		apply("context", true, incoming.Sources["context"], func() { out.Context = ctx })
	} else {
		apply("context", false, incoming.Sources["context"], func() { out.Context = maxInt64(incoming.Context, 0) })
	}
	if incoming.MaxOutput != out.MaxOutput {
		n := maxInt64(incoming.MaxOutput, 0)
		apply("maxOutput", true, incoming.Sources["maxOutput"], func() { out.MaxOutput = n })
	} else {
		apply("maxOutput", false, incoming.Sources["maxOutput"], func() { out.MaxOutput = maxInt64(incoming.MaxOutput, 0) })
	}
	if incoming.Reasoning != out.Reasoning {
		apply("reasoning", true, incoming.Sources["reasoning"], func() { out.Reasoning = incoming.Reasoning })
	} else {
		apply("reasoning", false, incoming.Sources["reasoning"], func() { out.Reasoning = incoming.Reasoning })
	}
	if incoming.Image != out.Image {
		apply("image", true, incoming.Sources["image"], func() { out.Image = incoming.Image })
	} else {
		apply("image", false, incoming.Sources["image"], func() { out.Image = incoming.Image })
	}
	levels := normalizeStringList(incoming.ReasoningLevels)
	param := strings.TrimSpace(incoming.ReasoningParam)
	reasonChanged := strings.Join(levels, ",") != strings.Join(out.ReasoningLevels, ",") || param != out.ReasoningParam
	if reasonChanged {
		out.ReasoningLevels = levels
		out.ReasoningParam = param
		out.Sources["reasoning"] = "manual"
		changed = true
	}
	if changed {
		out.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	return out
}

func sanitizeSource(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 80 {
		return ""
	}
	switch s {
	case "upstream", "workbuddy", "manual", "detected":
		return s
	}
	if strings.HasPrefix(s, "models.dev:") {
		return s
	}
	return ""
}

func normalizeStringList(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func mergeDetectedMeta(base *config.ModelMeta, det detectedMeta, overwrite bool) *config.ModelMeta {
	out := base.Clone()
	if out == nil {
		out = &config.ModelMeta{}
	}
	if out.Sources == nil {
		out.Sources = map[string]string{}
	}
	changed := false
	apply := func(field string, known bool, set func()) {
		if !known {
			return
		}
		if !overwrite && metaFieldKnown(out, field) {
			return
		}
		set()
		src := det.Meta.Sources[field]
		if src == "" {
			src = "detected"
		}
		out.Sources[field] = src
		changed = true
	}
	apply("context", det.Fields["context"] && det.Meta.Context > 0, func() { out.Context = det.Meta.Context })
	apply("maxOutput", det.Fields["maxOutput"] && det.Meta.MaxOutput > 0, func() { out.MaxOutput = det.Meta.MaxOutput })
	apply("image", det.Fields["image"], func() { out.Image = det.Meta.Image })
	apply("reasoning", det.Fields["reasoning"], func() {
		out.Reasoning = det.Meta.Reasoning
		if len(det.Meta.ReasoningLevels) > 0 {
			out.ReasoningLevels = append([]string(nil), det.Meta.ReasoningLevels...)
		}
		if det.Meta.ReasoningParam != "" {
			out.ReasoningParam = det.Meta.ReasoningParam
		}
	})
	if changed {
		out.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	return out
}

func metaFieldKnown(m *config.ModelMeta, field string) bool {
	if m == nil {
		return false
	}
	if m.Sources[field] != "" {
		return true
	}
	switch field {
	case "context":
		return m.Context > 0
	case "maxOutput":
		return m.MaxOutput > 0
	case "reasoning":
		return m.Reasoning
	case "image":
		return m.Image
	}
	return false
}

func mergeDetected(dst *detectedMeta, src detectedMeta) {
	if dst.Fields == nil {
		dst.Fields = map[string]bool{}
	}
	if dst.Meta.Sources == nil {
		dst.Meta.Sources = map[string]string{}
	}
	if dst.Name == "" {
		dst.Name = src.Name
	}
	for field, known := range src.Fields {
		if !known || dst.Fields[field] {
			continue
		}
		dst.Fields[field] = true
		dst.Meta.Sources[field] = src.Meta.Sources[field]
		switch field {
		case "context":
			dst.Meta.Context = src.Meta.Context
		case "maxOutput":
			dst.Meta.MaxOutput = src.Meta.MaxOutput
		case "reasoning":
			dst.Meta.Reasoning = src.Meta.Reasoning
			dst.Meta.ReasoningLevels = append([]string(nil), src.Meta.ReasoningLevels...)
			dst.Meta.ReasoningParam = src.Meta.ReasoningParam
		case "image":
			dst.Meta.Image = src.Meta.Image
		}
	}
}

func detectWorkBuddyModel(m workbuddy.ModelInfo) detectedMeta {
	det := detectedMeta{
		Name:   m.Name,
		Fields: map[string]bool{},
		Meta:   config.ModelMeta{Sources: map[string]string{}},
	}
	if m.ContextWindow > 0 {
		det.Meta.Context = m.ContextWindow
		det.Meta.Sources["context"] = "workbuddy"
		det.Fields["context"] = true
	}
	if m.MaxTokens > 0 {
		det.Meta.MaxOutput = m.MaxTokens
		det.Meta.Sources["maxOutput"] = "workbuddy"
		det.Fields["maxOutput"] = true
	}
	det.Meta.Image = m.SupportsImages
	det.Meta.Sources["image"] = "workbuddy"
	det.Fields["image"] = true
	if m.Reasoning != nil {
		det.Meta.Reasoning = true
		if b, ok := m.Reasoning.(bool); ok {
			det.Meta.Reasoning = b
		}
		det.Meta.Sources["reasoning"] = "workbuddy"
		det.Fields["reasoning"] = true
	}
	return det
}

func detectOpenAIModel(raw map[string]any) detectedMeta {
	det := detectedMeta{Fields: map[string]bool{}, Meta: config.ModelMeta{Sources: map[string]string{}}}
	if name, ok := raw["name"].(string); ok {
		det.Name = strings.TrimSpace(name)
	}
	if n, ok := firstInt(raw, [][]string{
		{"context_length"}, {"context_window"}, {"contextWindow"}, {"max_context_length"},
		{"max_input_tokens"}, {"maxInputTokens"}, {"input_token_limit"}, {"maxAllowedSize"},
		{"limit", "context"}, {"limits", "context"}, {"top_provider", "context_length"},
	}); ok && n > 0 {
		det.Meta.Context = n
		det.Meta.Sources["context"] = "upstream"
		det.Fields["context"] = true
	}
	if n, ok := firstInt(raw, [][]string{
		{"max_completion_tokens"}, {"max_output_tokens"}, {"maxOutputTokens"}, {"max_tokens"},
		{"maxTokens"}, {"output_token_limit"}, {"limit", "output"}, {"limits", "output"},
		{"top_provider", "max_completion_tokens"},
	}); ok && n > 0 {
		det.Meta.MaxOutput = n
		det.Meta.Sources["maxOutput"] = "upstream"
		det.Fields["maxOutput"] = true
	}

	imageKnown, image := false, false
	if b, ok := firstBool(raw, [][]string{
		{"supports_vision"}, {"supports_images"}, {"supportsImages"}, {"vision"}, {"image_input"},
		{"capabilities", "vision"}, {"capabilities", "image_input"}, {"capabilities", "imageInput"},
	}); ok {
		imageKnown, image = true, b
	}
	if !imageKnown {
		if list, ok := firstStringList(raw, [][]string{
			{"architecture", "input_modalities"}, {"modalities", "input"}, {"input_modalities"},
			{"capabilities", "input_modalities"},
		}); ok {
			imageKnown, image = true, containsFold(list, "image")
		}
	}
	if imageKnown {
		det.Meta.Image = image
		det.Meta.Sources["image"] = "upstream"
		det.Fields["image"] = true
	}

	reasonKnown, reasoning := false, false
	if b, ok := firstBool(raw, [][]string{
		{"supports_reasoning"}, {"supportsReasoning"}, {"supports_thinking"}, {"thinking"},
		{"capabilities", "reasoning"}, {"capabilities", "thinking"},
	}); ok {
		reasonKnown, reasoning = true, b
	}
	if !reasonKnown {
		if v, ok := pathValue(raw, "reasoning"); ok {
			if b, isBool := v.(bool); isBool {
				reasonKnown, reasoning = true, b
			} else if v != nil {
				reasonKnown, reasoning = true, true
			}
		}
	}
	if !reasonKnown {
		if list, ok := firstStringList(raw, [][]string{{"supported_parameters"}}); ok {
			for _, v := range list {
				if lv := strings.ToLower(v); lv == "reasoning" || lv == "reasoning_effort" || lv == "thinking" {
					reasonKnown, reasoning = true, true
					break
				}
			}
		}
	}
	if reasonKnown {
		det.Meta.Reasoning = reasoning
		det.Meta.Sources["reasoning"] = "upstream"
		det.Fields["reasoning"] = true
		levels := reasoningLevels(raw)
		if len(levels) > 0 {
			det.Meta.ReasoningLevels = levels
		}
		det.Meta.ReasoningParam = reasoningParam(raw, levels)
	}
	return det
}

func reasoningLevels(raw map[string]any) []string {
	for _, key := range []string{"reasoning_levels", "reasoningLevels", "reasoning_efforts", "supported_reasoning_efforts"} {
		if v, ok := pathValue(raw, key); ok {
			if list, ok := asStringList(v); ok {
				return normalizeStringList(list)
			}
		}
	}
	for _, path := range [][]string{{"reasoning_options"}, {"capabilities", "reasoning_options"}} {
		if v, ok := pathValue(raw, path...); ok {
			if list, ok := asStringListFromOptions(v); ok && len(list) > 0 {
				return normalizeStringList(list)
			}
		}
	}
	return nil
}

func reasoningParam(raw map[string]any, levels []string) string {
	for _, key := range []string{"reasoning_param", "reasoningParam", "reasoning_effort_param"} {
		if s, ok := stringAt(raw, key); ok && s != "" {
			return s
		}
	}
	if list, ok := firstStringList(raw, [][]string{{"supported_parameters"}}); ok {
		for _, v := range list {
			lv := strings.ToLower(v)
			if lv == "reasoning_effort" {
				return "reasoning_effort"
			}
			if lv == "thinking" {
				return "thinking"
			}
		}
	}
	if len(levels) > 0 {
		return "reasoning_effort"
	}
	return ""
}

func findModelArray(v any, depth int) []map[string]any {
	if depth > 4 || v == nil {
		return nil
	}
	switch t := v.(type) {
	case []any:
		out := make([]map[string]any, 0, len(t))
		for _, item := range t {
			if m, ok := item.(map[string]any); ok {
				if _, has := m["id"]; has {
					out = append(out, m)
				}
			}
		}
		if len(out) > 0 {
			return out
		}
		for _, item := range t {
			if a := findModelArray(item, depth+1); len(a) > 0 {
				return a
			}
		}
	case map[string]any:
		for _, key := range []string{"data", "models", "result", "items", "list"} {
			if child, ok := t[key]; ok {
				if a := findModelArray(child, depth+1); len(a) > 0 {
					return a
				}
			}
		}
		for _, child := range t {
			if a := findModelArray(child, depth+1); len(a) > 0 {
				return a
			}
		}
	}
	return nil
}

func pathValue(m map[string]any, path ...string) (any, bool) {
	var cur any = m
	for _, key := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = mm[key]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func firstInt(m map[string]any, paths [][]string) (int64, bool) {
	for _, p := range paths {
		if v, ok := pathValue(m, p...); ok {
			if n, ok := asInt64(v); ok {
				return n, true
			}
		}
	}
	return 0, false
}

func firstBool(m map[string]any, paths [][]string) (bool, bool) {
	for _, p := range paths {
		if v, ok := pathValue(m, p...); ok {
			if b, ok := v.(bool); ok {
				return b, true
			}
		}
	}
	return false, false
}

func firstStringList(m map[string]any, paths [][]string) ([]string, bool) {
	for _, p := range paths {
		if v, ok := pathValue(m, p...); ok {
			if list, ok := asStringList(v); ok {
				return list, true
			}
		}
	}
	return nil, false
}

func stringAt(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return strings.TrimSpace(s), ok
}

func asInt64(v any) (int64, bool) {
	switch t := v.(type) {
	case float64:
		return int64(t), true
	case float32:
		return int64(t), true
	case int:
		return int64(t), true
	case int64:
		return t, true
	case json.Number:
		n, err := t.Int64()
		return n, err == nil
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		return n, err == nil
	}
	return 0, false
}

func asStringList(v any) ([]string, bool) {
	list, ok := v.([]any)
	if !ok {
		if ss, ok := v.([]string); ok {
			return append([]string(nil), ss...), true
		}
		return nil, false
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out, true
}

func asStringListFromOptions(v any) ([]string, bool) {
	list, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0)
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := m["type"].(string)
		if typ != "effort" {
			continue
		}
		if values, ok := asStringList(m["values"]); ok {
			out = append(out, values...)
		}
	}
	return out, true
}

func containsFold(list []string, target string) bool {
	for _, s := range list {
		if strings.EqualFold(strings.TrimSpace(s), target) {
			return true
		}
	}
	return false
}

var htmlTagRe = regexp.MustCompile(`<[^>]*>`)

func cleanErrorText(s string) string {
	s = htmlTagRe.ReplaceAllString(s, " ")
	s = strings.Join(strings.Fields(s), " ")
	return truncateText(s, 200)
}

func truncateText(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---- models.dev fallback ----

type mdModel struct {
	ID               string        `json:"id"`
	Name             string        `json:"name"`
	Attachment       *bool         `json:"attachment"`
	Reasoning        *bool         `json:"reasoning"`
	ReasoningOptions []mdReasoning `json:"reasoning_options"`
	Modalities       struct {
		Input []string `json:"input"`
	} `json:"modalities"`
	Limit mdLimit `json:"limit"`
}

type mdLimit struct {
	Context int64 `json:"context"`
	Output  int64 `json:"output"`
}

type mdReasoning struct {
	Type   string   `json:"type"`
	Values []string `json:"values"`
}

type mdHit struct {
	Provider string
	ID       string
	Model    mdModel
}

func (s *Server) marketIndex(ctx context.Context) (map[string][]mdHit, string) {
	s.mdMu.Lock()
	defer s.mdMu.Unlock()
	if s.mdIndex != nil && time.Since(s.mdAt) < 24*time.Hour {
		return s.mdIndex, "cached"
	}
	if modelsDevURL == "" {
		return nil, "disabled"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsDevURL, nil)
	if err != nil {
		return nil, "error: " + err.Error()
	}
	client := &http.Client{Timeout: 40 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		s.mdErr = logx.Redact(err.Error(), 300)
		return nil, "error: " + s.mdErr
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		s.mdErr = fmt.Sprintf("HTTP %d", resp.StatusCode)
		return nil, "error: " + s.mdErr
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32*1024*1024))
	if err != nil {
		s.mdErr = logx.Redact(err.Error(), 300)
		return nil, "error: " + s.mdErr
	}
	var doc map[string]struct {
		Models map[string]mdModel `json:"models"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		s.mdErr = logx.Redact(err.Error(), 300)
		return nil, "error: " + s.mdErr
	}
	index := make(map[string][]mdHit, len(doc)*8)
	for pid, provider := range doc {
		for mid, model := range provider.Models {
			if model.ID == "" {
				model.ID = mid
			}
			hit := mdHit{Provider: pid, ID: mid, Model: model}
			keys := []string{mid, strings.ToLower(mid), normalizeModelID(mid)}
			if model.Name != "" {
				keys = append(keys, normalizeModelID(model.Name))
			}
			seen := map[string]bool{}
			for _, key := range keys {
				if key == "" || seen[key] {
					continue
				}
				seen[key] = true
				index[key] = append(index[key], hit)
			}
		}
	}
	if len(index) == 0 {
		s.mdErr = "模型目录为空"
		return nil, "error: " + s.mdErr
	}
	s.mdIndex = index
	s.mdAt = time.Now()
	s.mdErr = ""
	return index, "ok"
}

func matchMarket(index map[string][]mdHit, provider, model string) (detectedMeta, bool) {
	if len(index) == 0 {
		return detectedMeta{}, false
	}
	keys := []string{model, strings.ToLower(model), normalizeModelID(model)}
	var hits []mdHit
	seen := map[string]bool{}
	for _, key := range keys {
		for _, h := range index[key] {
			id := h.Provider + "/" + h.ID
			if seen[id] {
				continue
			}
			seen[id] = true
			hits = append(hits, h)
		}
	}
	if len(hits) == 0 {
		return detectedMeta{}, false
	}
	best := hits[0]
	bestScore := -1
	for _, h := range hits {
		score := 0
		if strings.EqualFold(h.Provider, provider) {
			score += 100
		}
		if strings.EqualFold(h.ID, model) {
			score += 30
		}
		if strings.EqualFold(h.Model.Name, model) {
			score += 10
		}
		if h.Model.Limit.Context > 0 || h.Model.Limit.Output > 0 {
			score += 5
		}
		if score > bestScore || (score == bestScore && h.Provider < best.Provider) {
			best, bestScore = h, score
		}
	}
	return detectMarketModel(best), true
}

func detectMarketModel(h mdHit) detectedMeta {
	det := detectedMeta{
		Name:   h.Model.Name,
		Fields: map[string]bool{},
		Meta:   config.ModelMeta{Sources: map[string]string{}},
	}
	src := "models.dev:" + h.Provider
	if h.Model.Limit.Context > 0 {
		det.Meta.Context = h.Model.Limit.Context
		det.Meta.Sources["context"] = src
		det.Fields["context"] = true
	}
	if h.Model.Limit.Output > 0 {
		det.Meta.MaxOutput = h.Model.Limit.Output
		det.Meta.Sources["maxOutput"] = src
		det.Fields["maxOutput"] = true
	}
	image := containsFold(h.Model.Modalities.Input, "image")
	if h.Model.Attachment != nil && *h.Model.Attachment {
		image = true
	}
	if len(h.Model.Modalities.Input) > 0 || h.Model.Attachment != nil {
		det.Meta.Image = image
		det.Meta.Sources["image"] = src
		det.Fields["image"] = true
	}
	if h.Model.Reasoning != nil {
		det.Meta.Reasoning = *h.Model.Reasoning
		det.Meta.Sources["reasoning"] = src
		det.Fields["reasoning"] = true
	}
	var levels []string
	for _, opt := range h.Model.ReasoningOptions {
		if opt.Type == "effort" {
			levels = append(levels, opt.Values...)
		}
	}
	if len(levels) > 0 {
		det.Meta.ReasoningLevels = normalizeStringList(levels)
		det.Meta.ReasoningParam = "reasoning_effort"
		if h.Model.Reasoning != nil && !*h.Model.Reasoning {
			det.Meta.Reasoning = true
			det.Meta.Sources["reasoning"] = src
			det.Fields["reasoning"] = true
		}
	}
	return det
}

func normalizeModelID(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.LastIndex(s, "/"); i >= 0 && i < len(s)-1 {
		s = s[i+1:]
	}
	if i := strings.Index(s, ":"); i >= 0 {
		s = s[:i]
	}
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	})
	for len(parts) > 1 && isDigits(parts[len(parts)-1]) {
		parts = parts[:len(parts)-1]
	}
	return strings.Join(parts, "")
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ---- /v1/models metadata rendering ----

func modelJSON(id, provider string, meta *config.ModelMeta, aliasOf string) map[string]any {
	item := map[string]any{"id": id, "object": "model", "created": 0, "owned_by": provider}
	if aliasOf != "" {
		item["alias_of"] = aliasOf
	}
	if meta == nil {
		return item
	}
	if meta.Context > 0 {
		item["context_length"] = meta.Context
		item["max_context_length"] = meta.Context
	}
	if meta.MaxOutput > 0 {
		item["max_completion_tokens"] = meta.MaxOutput
		item["max_output_tokens"] = meta.MaxOutput
	}
	imageKnown := meta.Sources["image"] != "" || meta.Image
	reasonKnown := meta.Sources["reasoning"] != "" || meta.Reasoning
	if meta.Context > 0 || meta.MaxOutput > 0 {
		top := map[string]any{}
		limit := map[string]any{}
		if meta.Context > 0 {
			top["context_length"] = meta.Context
			limit["context"] = meta.Context
		}
		if meta.MaxOutput > 0 {
			top["max_completion_tokens"] = meta.MaxOutput
			limit["output"] = meta.MaxOutput
		}
		item["top_provider"] = top
		item["limit"] = limit
	}
	if imageKnown {
		modalities := []string{"text"}
		if meta.Image {
			modalities = append(modalities, "image")
		}
		item["architecture"] = map[string]any{"input_modalities": modalities, "output_modalities": []string{"text"}}
		item["modalities"] = map[string]any{"input": modalities, "output": []string{"text"}}
		item["input_modalities"] = modalities
		item["attachment"] = meta.Image
	}
	capabilities := map[string]any{}
	if imageKnown {
		capabilities["vision"] = meta.Image
	}
	if reasonKnown {
		capabilities["reasoning"] = meta.Reasoning
	}
	if len(capabilities) > 0 {
		item["capabilities"] = capabilities
	}
	if reasonKnown {
		item["reasoning"] = meta.Reasoning
	}
	if meta.Reasoning {
		params := []string{"reasoning"}
		param := strings.TrimSpace(meta.ReasoningParam)
		if param == "" {
			param = "reasoning_effort"
		}
		params = append(params, param)
		item["supported_parameters"] = normalizeStringList(params)
		item["reasoning_param"] = param
		if len(meta.ReasoningLevels) > 0 {
			item["reasoning_levels"] = append([]string(nil), meta.ReasoningLevels...)
		}
	}
	if len(meta.Sources) > 0 || meta.UpdatedAt != "" {
		item["gateway_meta"] = map[string]any{"sources": meta.Sources, "updatedAt": meta.UpdatedAt}
	}
	return item
}
