package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ttxyc/opencode-model-gateway/internal/config"
)

func TestDetectOpenAIModel(t *testing.T) {
	raw := map[string]any{
		"id":                    "demo",
		"context_length":        float64(1000000),
		"max_completion_tokens": float64(384000),
		"architecture":          map[string]any{"input_modalities": []any{"text", "image"}},
		"supported_parameters":  []any{"reasoning", "reasoning_effort"},
		"reasoning_options":     []any{map[string]any{"type": "effort", "values": []any{"low", "high", "max"}}},
	}
	det := detectOpenAIModel(raw)
	if !det.Fields["context"] || det.Meta.Context != 1000000 {
		t.Fatalf("context %+v", det)
	}
	if !det.Fields["maxOutput"] || det.Meta.MaxOutput != 384000 {
		t.Fatalf("maxOutput %+v", det)
	}
	if !det.Fields["image"] || !det.Meta.Image {
		t.Fatalf("image %+v", det)
	}
	if !det.Fields["reasoning"] || !det.Meta.Reasoning {
		t.Fatalf("reasoning %+v", det)
	}
	if det.Meta.ReasoningParam != "reasoning_effort" || strings.Join(det.Meta.ReasoningLevels, ",") != "low,high,max" {
		t.Fatalf("levels %+v", det.Meta)
	}
	if det.Meta.Sources["context"] != "upstream" {
		t.Fatalf("source %+v", det.Meta.Sources)
	}
}

func TestDetectTopProviderAndModalities(t *testing.T) {
	raw := map[string]any{
		"id": "demo",
		"top_provider": map[string]any{
			"context_length":        float64(200000),
			"max_completion_tokens": float64(64000),
		},
		"modalities": map[string]any{"input": []any{"text"}},
		"reasoning":  false,
	}
	det := detectOpenAIModel(raw)
	if det.Meta.Context != 200000 || det.Meta.MaxOutput != 64000 {
		t.Fatalf("limits %+v", det.Meta)
	}
	if !det.Fields["image"] || det.Meta.Image {
		t.Fatalf("image should be known false: %+v", det)
	}
	if !det.Fields["reasoning"] || det.Meta.Reasoning {
		t.Fatalf("reasoning should be known false: %+v", det)
	}
}

func TestNormalizeAndMatchMarket(t *testing.T) {
	if got := normalizeModelID("cohere/north-mini-code:free"); got != "northminicode" {
		t.Fatalf("normalize target %q", got)
	}
	if got := normalizeModelID("north-mini-code-1-0"); got != "northminicode" {
		t.Fatalf("normalize candidate %q", got)
	}
	idx := map[string][]mdHit{
		"northminicode": {{
			Provider: "cohere",
			ID:       "north-mini-code-1-0",
			Model: mdModel{
				ID:    "north-mini-code-1-0",
				Name:  "North Mini Code",
				Limit: mdLimit{Context: 256000, Output: 64000},
			},
		}},
	}
	det, ok := matchMarket(idx, "cline", "cohere/north-mini-code:free")
	if !ok || det.Meta.Context != 256000 || det.Meta.MaxOutput != 64000 {
		t.Fatalf("match %v %+v", ok, det)
	}
	if det.Meta.Sources["context"] != "models.dev:cohere" {
		t.Fatalf("source %+v", det.Meta.Sources)
	}
}

func TestCleanErrorText(t *testing.T) {
	got := cleanErrorText("<html>\n<head><title>503 Service</title></head><body><h1>Unavailable</h1></body></html>")
	if strings.Contains(got, "<") || strings.Contains(got, "\n") || !strings.Contains(got, "503 Service") {
		t.Fatalf("clean %q", got)
	}
}

func TestMergeManualMetaSources(t *testing.T) {
	in := &config.ModelMeta{
		Context: 1000, MaxOutput: 200, Reasoning: false, Image: false,
		Sources: map[string]string{
			"context": "models.dev:demo", "maxOutput": "models.dev:demo",
			"reasoning": "models.dev:demo", "image": "models.dev:demo",
		},
	}
	got := mergeManualMeta(nil, in)
	for _, field := range []string{"context", "maxOutput", "reasoning", "image"} {
		if got.Sources[field] != "models.dev:demo" {
			t.Fatalf("%s source %+v", field, got.Sources)
		}
	}
	manual := &config.ModelMeta{Context: 1000, Sources: map[string]string{"context": "manual"}}
	got = mergeManualMeta(manual, in)
	if got.Sources["context"] != "manual" {
		t.Fatalf("manual source overwritten: %+v", got.Sources)
	}
	lock := &config.ModelMeta{Image: true, Sources: map[string]string{"image": "upstream"}}
	off := &config.ModelMeta{Image: false, Sources: map[string]string{"image": "upstream"}}
	got = mergeManualMeta(lock, off)
	if got.Image || got.Sources["image"] != "manual" {
		t.Fatalf("manual toggle: %+v", got)
	}
	bad := &config.ModelMeta{Context: 1, Sources: map[string]string{"context": "javascript:alert(1)"}}
	got = mergeManualMeta(nil, bad)
	if got.Sources["context"] != "manual" {
		t.Fatalf("unsanitized source: %+v", got.Sources)
	}
}

func TestCatalogApplyAndModelsMetadata(t *testing.T) {
	ts, _, cfg, _ := newTestServer(t)
	cfg.Aliases = map[string]string{"ms-flash": "modelscope/deepseek-ai/DeepSeek-V4.1-Flash-1017"}

	body := `{"providers":{"modelscope":{"models":["deepseek-ai/DeepSeek-V4.1-Flash-1017","deepseek-ai/DeepSeek-V4-Pro-0813"],"meta":{"deepseek-ai/DeepSeek-V4.1-Flash-1017":{"context":1048576,"maxOutput":384000,"reasoning":true,"reasoningLevels":["low","high","max"],"reasoningParam":"reasoning_effort","image":true}}}}}`
	code, out, _ := do(t, "POST", ts.URL+"/admin/api/model-catalog/apply", cfg.AdminToken, body)
	if code != 200 {
		t.Fatalf("apply %d %s", code, out)
	}
	statePath := filepath.Join(filepath.Dir(cfg.Paths.State), "model-catalog.json")
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("state file: %v", err)
	}
	if !strings.Contains(string(raw), `"deepseek-ai/DeepSeek-V4.1-Flash-1017"`) || !strings.Contains(string(raw), `"context": 1048576`) {
		t.Fatalf("state content %s", raw)
	}

	code, out, _ = do(t, "GET", ts.URL+"/v1/models", cfg.ClientToken, "")
	if code != 200 {
		t.Fatalf("models %d %s", code, out)
	}
	var doc struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	byID := map[string]map[string]any{}
	for _, m := range doc.Data {
		byID[m["id"].(string)] = m
	}
	m := byID["modelscope/deepseek-ai/DeepSeek-V4.1-Flash-1017"]
	if m == nil {
		t.Fatalf("model missing: %s", out)
	}
	if m["context_length"].(float64) != 1048576 || m["max_completion_tokens"].(float64) != 384000 {
		t.Fatalf("limits %v", m)
	}
	arch := m["architecture"].(map[string]any)
	if !containsFold(toStrings(arch["input_modalities"]), "image") {
		t.Fatalf("architecture %v", arch)
	}
	cap := m["capabilities"].(map[string]any)
	if cap["vision"] != true || cap["reasoning"] != true {
		t.Fatalf("capabilities %v", cap)
	}
	if !containsFold(toStrings(m["supported_parameters"]), "reasoning_effort") {
		t.Fatalf("supported_parameters %v", m["supported_parameters"])
	}
	if !containsFold(toStrings(m["reasoning_levels"]), "max") {
		t.Fatalf("reasoning_levels %v", m["reasoning_levels"])
	}

	alias := byID["ms-flash"]
	if alias == nil || alias["context_length"].(float64) != 1048576 {
		t.Fatalf("alias metadata %v", alias)
	}

	code, out, _ = do(t, "GET", ts.URL+"/p/modelscope/v1/models", cfg.ClientToken, "")
	if code != 200 || !strings.Contains(out, `"context_length":1048576`) || !strings.Contains(out, `"id":"deepseek-ai/DeepSeek-V4.1-Flash-1017"`) {
		t.Fatalf("provider models %d %s", code, out)
	}

	code, out, _ = do(t, "POST", ts.URL+"/admin/api/model-catalog/apply", cfg.AdminToken, `{"providers":{"nope":{"models":["x"]}}}`)
	if code != 400 || !strings.Contains(out, "未知服务商") {
		t.Fatalf("unknown provider %d %s", code, out)
	}
}

func TestCatalogCheckFallsBackToModelsDev(t *testing.T) {
	md := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"deepseek":{"models":{"deepseek-flash":{"id":"deepseek-flash","name":"DeepSeek Flash","attachment":false,"reasoning":true,"reasoning_options":[{"type":"effort","values":["low","high","max"]}],"modalities":{"input":["text"]},"limit":{"context":1000000,"output":384000}}}}}`))
	}))
	defer md.Close()
	old := modelsDevURL
	modelsDevURL = md.URL
	t.Cleanup(func() { modelsDevURL = old })

	ts, _, cfg, _ := newTestServer(t)
	code, out, _ := do(t, "POST", ts.URL+"/admin/api/model-catalog/check", cfg.AdminToken, `{"provider":"deepseek"}`)
	if code != 200 {
		t.Fatalf("check %d %s", code, out)
	}
	var resp struct {
		Providers []catalogProvider `json:"providers"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Providers) != 1 {
		t.Fatalf("providers %+v", resp.Providers)
	}
	p := resp.Providers[0]
	if p.Fetched {
		t.Fatalf("mock upstream has no /models, expected fetch error: %+v", p)
	}
	var found *catalogModel
	for i := range p.Models {
		if p.Models[i].ID == "deepseek-flash" {
			found = &p.Models[i]
		}
	}
	if found == nil || found.Meta == nil {
		t.Fatalf("model %+v", p.Models)
	}
	if found.Meta.Context != 1000000 || found.Meta.MaxOutput != 384000 || !found.Meta.Reasoning {
		t.Fatalf("meta %+v", found.Meta)
	}
	if found.Meta.Sources["context"] != "models.dev:deepseek" {
		t.Fatalf("sources %+v", found.Meta.Sources)
	}
}

func toStrings(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
