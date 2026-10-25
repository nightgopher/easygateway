package handlers

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/provider"
)

// newPruneEnv 构造带真实探活状态的测试环境：dead 上游恒 404（确定性拒绝），
// alive 上游恒 200。providers.yaml 落在临时目录，reloader 指向同一文件。
func newPruneEnv(t *testing.T) (*gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"message":"model not found"}}`)
	}))
	t.Cleanup(dead.Close)
	alive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	t.Cleanup(alive.Close)

	dir := t.TempDir()
	configPath := filepath.Join(dir, "providers.yaml")
	yaml := "providers:\n" +
		"  - name: dead\n    type: openai\n    url: " + dead.URL + "\n    api_key_env: DEAD_KEY\n    allow_no_key: true\n    models:\n" +
		"      - id: gone-model\n        capabilities: [text]\n" +
		"  - name: alive\n    type: openai\n    url: " + alive.URL + "\n    api_key_env: ALIVE_KEY\n    allow_no_key: true\n    models:\n" +
		"      - id: healthy-model\n        capabilities: [text]\n" +
		"\nchains:\n  coding:\n    - healthy-model\n    - gone-model\n"
	if err := os.WriteFile(configPath, []byte(yaml), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	router := provider.NewRouter(log.New(io.Discard, "", 0))
	deadProvider := provider.NewOpenAIProvider(&provider.Config{Name: "gone-model", URL: dead.URL, APIKey: "k"})
	aliveProvider := provider.NewOpenAIProvider(&provider.Config{Name: "healthy-model", URL: alive.URL, APIKey: "k"})
	router.RegisterProvider("gone-model", deadProvider)
	router.RegisterProvider("healthy-model", aliveProvider)
	router.RegisterChain("gone-model", []string{"gone-model"})
	router.RegisterChain("healthy-model", []string{"healthy-model"})
	if result := router.CheckProviderHealth(context.Background(), "gone-model"); result.Status != provider.ProbeOffline {
		t.Fatalf("dead probe = %v, want offline", result.Status)
	}
	if result := router.CheckProviderHealth(context.Background(), "healthy-model"); result.Status != provider.ProbeOnline {
		t.Fatalf("alive probe = %v, want online", result.Status)
	}

	reloader := provider.NewConfigReloader(router, configPath, nil, nil)
	handler := NewModelsPruneHandler(router, reloader, configPath, log.New(io.Discard, "", 0))
	e := gin.New()
	e.POST("/admin/models/prune", handler.HandlePrune)
	return e, configPath
}

func pruneCall(e *gin.Engine, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/admin/models/prune", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	return w
}

func TestModelsPruneDryRunKeepsFile(t *testing.T) {
	e, configPath := newPruneEnv(t)
	before, _ := os.ReadFile(configPath)

	w := pruneCall(e, `{"dry_run":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("dry run = %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		ToRemove []struct {
			Model  string `json:"model"`
			Reason string `json:"reason"`
		} `json:"to_remove"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.ToRemove) != 1 || resp.ToRemove[0].Model != "gone-model" {
		t.Fatalf("to_remove = %+v, want only gone-model", resp.ToRemove)
	}
	if !strings.Contains(resp.ToRemove[0].Reason, "404") {
		t.Fatalf("原因应含 404: %+v", resp.ToRemove[0])
	}
	after, _ := os.ReadFile(configPath)
	if string(before) != string(after) {
		t.Fatal("dry run 不得改动 providers.yaml")
	}
}

func TestModelsPruneExecuteRemovesAndReloads(t *testing.T) {
	e, configPath := newPruneEnv(t)

	w := pruneCall(e, `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("execute = %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Removed []struct {
			Model string `json:"model"`
		} `json:"removed"`
		Chains []struct {
			Chain     string   `json:"chain"`
			Remaining []string `json:"remaining"`
		} `json:"chains_affected"`
		BackupPath string `json:"backup_path"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Removed) != 1 || resp.Removed[0].Model != "gone-model" {
		t.Fatalf("removed = %+v", resp.Removed)
	}
	if len(resp.Chains) != 1 || resp.Chains[0].Chain != "coding" || len(resp.Chains[0].Remaining) != 1 {
		t.Fatalf("chains = %+v", resp.Chains)
	}

	content, _ := os.ReadFile(configPath)
	if strings.Contains(string(content), "gone-model") || strings.Contains(string(content), "name: dead") {
		t.Fatalf("失败模型与空壳 provider 应从文件移除:\n%s", content)
	}
	if !strings.Contains(string(content), "healthy-model") {
		t.Fatalf("健康模型必须保留:\n%s", content)
	}
	if _, err := os.Stat(resp.BackupPath); err != nil {
		t.Fatalf("备份文件应存在: %v", err)
	}
}

// 请求清理健康模型必须被拒：候选集之外的模型一个都不能删。
func TestModelsPruneRejectsNonCandidates(t *testing.T) {
	e, configPath := newPruneEnv(t)
	before, _ := os.ReadFile(configPath)

	w := pruneCall(e, `{"models":["healthy-model"]}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("越界请求应 422, got %d: %s", w.Code, w.Body.String())
	}
	after, _ := os.ReadFile(configPath)
	if string(before) != string(after) {
		t.Fatal("被拒请求不得改动文件")
	}

	// 混合请求：只清候选内的，健康模型记入 skipped。
	w = pruneCall(e, `{"models":["healthy-model","gone-model"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("mixed = %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Removed []struct {
			Model string `json:"model"`
		} `json:"removed"`
		Skipped []struct {
			Model  string `json:"model"`
			Reason string `json:"reason"`
		} `json:"skipped"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Removed) != 1 || resp.Removed[0].Model != "gone-model" {
		t.Fatalf("removed = %+v", resp.Removed)
	}
	if len(resp.Skipped) != 1 || resp.Skipped[0].Model != "healthy-model" {
		t.Fatalf("skipped = %+v", resp.Skipped)
	}
}

func TestModelsPruneNoCandidates(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	path := filepath.Join(dir, "providers.yaml")
	if err := os.WriteFile(path, []byte("providers: []\n"), 0644); err != nil {
		t.Fatal(err)
	}
	router := provider.NewRouter(log.New(io.Discard, "", 0))
	reloader := provider.NewConfigReloader(router, path, nil, nil)
	handler := NewModelsPruneHandler(router, reloader, path, log.New(io.Discard, "", 0))
	e := gin.New()
	e.POST("/admin/models/prune", handler.HandlePrune)

	w := pruneCall(e, `{}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "没有可清理的失败模型") {
		t.Fatalf("no candidates = %d: %s", w.Code, w.Body.String())
	}
	_ = time.Now
}

// 链上只有失败模型时：模型、空壳 provider、空链键一并移除，热重载成功
// （配置校验拒绝空链，因此清空链必须连键删除）。
func TestModelsPruneRemovesEmptyChain(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"message":"access denied"}}`)
	}))
	defer dead.Close()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "providers.yaml")
	yaml := "providers:\n  - name: dead\n    type: openai\n    url: " + dead.URL + "\n    api_key_env: K\n    models:\n      - id: only-model\n        capabilities: [text]\n\nchains:\n  solo:\n    - only-model\n"
	if err := os.WriteFile(configPath, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	router := provider.NewRouter(log.New(io.Discard, "", 0))
	router.RegisterProvider("only-model", provider.NewOpenAIProvider(&provider.Config{Name: "only-model", URL: dead.URL, APIKey: "k"}))
	router.RegisterChain("only-model", []string{"only-model"})
	if result := router.CheckProviderHealth(context.Background(), "only-model"); result.Status != provider.ProbeOffline {
		t.Fatalf("probe = %v", result.Status)
	}
	reloader := provider.NewConfigReloader(router, configPath, nil, nil)
	handler := NewModelsPruneHandler(router, reloader, configPath, log.New(io.Discard, "", 0))
	e := gin.New()
	e.POST("/admin/models/prune", handler.HandlePrune)

	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admin/models/prune", strings.NewReader(`{}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("prune = %d: %s", w.Code, w.Body.String())
	}
	content, _ := os.ReadFile(configPath)
	if strings.Contains(string(content), "only-model") || strings.Contains(string(content), "solo") {
		t.Fatalf("模型与空链应全部移除:\n%s", content)
	}
}
