package handlers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/dshconfig"
	"github.com/nightgopher/easygateway/internal/piconfig"
	"github.com/nightgopher/easygateway/internal/provider"
	"github.com/nightgopher/easygateway/internal/zcodeconfig"
)

// ClientConfigHandler 把「同步模型清单到客户端」扩展到 Pi 之外：
// ZCode（provider_config.json）与 deepseek-harness（cordis.patch.yml）。
// 模型清单从路由注册表实时推导（有 text 能力、非嵌入/转写的模型）。
type ClientConfigHandler struct {
	gatewayHome string
	zcodeHome   string
	dshHome     string
	router      *provider.Router
	logger      *log.Logger
}

// NewClientConfigHandler 创建客户端配置面板 handler
func NewClientConfigHandler(gatewayHome, zcodeHome, dshHome string, router *provider.Router, logger *log.Logger) *ClientConfigHandler {
	return &ClientConfigHandler{gatewayHome: gatewayHome, zcodeHome: zcodeHome, dshHome: dshHome, router: router, logger: logger}
}

// desiredModels 返回对外推荐的聊天模型清单（与 /v1/models 一致，剔除
// 嵌入/语音转写这类非聊天模型），且只保留当前可用的模型——渠道未登录
// 或 provider 已停用的模型写进客户端配置也无法使用。
func (h *ClientConfigHandler) desiredModels() []gin.H {
	ids := []gin.H{}
	seen := map[string]bool{}
	for _, info := range h.router.ListModelInfos() {
		if hasTextCapability(info.Capabilities) && !hasCapabilityFlagValue(info.Capabilities) && !seen[info.ID] {
			if h.router != nil && !h.router.ModelUsable(info.ID) {
				continue
			}
			seen[info.ID] = true
			ids = append(ids, gin.H{"id": info.ID, "name": info.ID})
		}
	}
	return ids
}

func hasTextCapability(caps []string) bool {
	for _, c := range caps {
		if c == "text" {
			return true
		}
	}
	return false
}

// hasCapabilityFlagValue 报告是否含非聊天能力（嵌入/语音转写）。
func hasCapabilityFlagValue(caps []string) bool {
	for _, c := range caps {
		if c == provider.CapabilityEmbedding || c == provider.CapabilityTranscription {
			return true
		}
	}
	return false
}

// endpoint 返回网关根地址（读 .env 的 PORT，默认 4001）。
func (h *ClientConfigHandler) endpoint() string {
	port := "4001"
	if values, err := readEnvFile(filepath.Join(h.gatewayHome, ".env")); err == nil {
		if v := strings.TrimSpace(values["PORT"]); v != "" {
			port = v
		}
	}
	return "http://127.0.0.1:" + port
}

func readEnvFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			values[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return values, nil
}

func desiredIDs(desired []gin.H) []string {
	ids := make([]string, 0, len(desired))
	for _, m := range desired {
		ids = append(ids, m["id"].(string))
	}
	return ids
}

// ─── ZCode ───────────────────────────────────────────────────────────────────

func (h *ClientConfigHandler) zcodeStatus() gin.H {
	desired := h.desiredModels()
	desiredIDs := desiredIDs(desired)
	path := zcodeconfig.ConfigPath(h.zcodeHome)

	currentIDs := []string{}
	fileExists := false
	inSync := false
	if raw, err := os.ReadFile(path); err == nil {
		fileExists = true
		var document map[string]any
		if err := jsonUnmarshal(raw, &document); err == nil {
			if rules := extractZCodeRules(document); rules != nil {
				currentIDs = extractZCodeModelIDs(rules, h.endpoint())
			}
		}
		inSync = equalStringSets(desiredIDs, currentIDs)
	}

	return gin.H{
		"path":        path,
		"file_exists": fileExists,
		"in_sync":     inSync,
		"desired":     desired,
		"current_ids": currentIDs,
		"desired_ids": desiredIDs,
		"missing_ids": difference(desiredIDs, currentIDs),
		"stale_ids":   difference(currentIDs, desiredIDs),
	}
}

// HandleZCodeStatus GET /admin/zcode
func (h *ClientConfigHandler) HandleZCodeStatus(c *gin.Context) {
	c.JSON(http.StatusOK, h.zcodeStatus())
}

// HandleZCodeSync POST /admin/zcode/sync（写入前轮转备份）
func (h *ClientConfigHandler) HandleZCodeSync(c *gin.Context) {
	models, ok := h.selectedModels(c)
	if !ok {
		return
	}
	path := zcodeconfig.ConfigPath(h.zcodeHome)
	if err := backupFile(path); err != nil {
		h.logger.Printf("backup ZCode provider config failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("备份现有配置失败: %v", err)})
		return
	}

	apiKey := ""
	if key, err := piconfig.MasterKey(h.gatewayHome); err == nil {
		apiKey = key // 仅新建规则时使用；已有规则不动 access
	}
	_, _, err := zcodeconfig.Setup(zcodeconfig.SetupOptions{
		ZCodeHome: h.zcodeHome,
		Endpoint:  h.endpoint(),
		APIKey:    apiKey,
		ModelIDs:  modelSelectionIDs(models),
	})
	if err != nil {
		h.logger.Printf("sync ZCode provider config failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	status := h.zcodeStatus()
	status["synced"] = true
	h.logger.Printf("ZCode provider config synced from dashboard: %s", path)
	c.JSON(http.StatusOK, status)
}

// ─── deepseek-harness ────────────────────────────────────────────────────────

func (h *ClientConfigHandler) harnessStatus() gin.H {
	desired := h.desiredModels()
	desiredIDs := desiredIDs(desired)
	path := dshconfig.PatchPath(h.dshHome)

	currentIDs, exists, missingEntries, err := dshconfig.CurrentModels(h.dshHome)
	if err != nil {
		h.logger.Printf("read dsh patch config: %v", err)
	}

	inSync := err == nil && len(missingEntries) == 0 && len(exists) > 0 && equalStringSets(desiredIDs, currentIDs)
	return gin.H{
		"path":            path,
		"file_exists":     err == nil,
		"in_sync":         inSync,
		"desired":         desired,
		"current_ids":     currentIDs,
		"desired_ids":     desiredIDs,
		"missing_ids":     difference(desiredIDs, currentIDs),
		"stale_ids":       difference(currentIDs, desiredIDs),
		"missing_entries": missingEntries,
	}
}

// HandleHarnessStatus GET /admin/harness
func (h *ClientConfigHandler) HandleHarnessStatus(c *gin.Context) {
	c.JSON(http.StatusOK, h.harnessStatus())
}

// HandleHarnessSync POST /admin/harness/sync（写入前轮转备份）
func (h *ClientConfigHandler) HandleHarnessSync(c *gin.Context) {
	models, ok := h.selectedModels(c)
	if !ok {
		return
	}
	path := dshconfig.PatchPath(h.dshHome)
	if err := backupFile(path); err != nil {
		h.logger.Printf("backup dsh patch config failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("备份现有配置失败: %v", err)})
		return
	}

	_, _, skipped, err := dshconfig.Setup(dshconfig.SetupOptions{
		DshHome:  h.dshHome,
		Endpoint: h.endpoint(),
		ModelIDs: modelSelectionIDs(models),
	})
	if err != nil {
		h.logger.Printf("sync dsh patch config failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	status := h.harnessStatus()
	status["synced"] = true
	if len(skipped) > 0 {
		status["skipped_entries"] = skipped
	}
	h.logger.Printf("dsh patch config synced from dashboard: %s", path)
	c.JSON(http.StatusOK, status)
}

// ─── 辅助 ────────────────────────────────────────────────────────────────────

func jsonUnmarshal(data []byte, target any) error {
	return json.Unmarshal(data, target)
}

// backupFile 把现有文件轮转备份为 path.pre-sync.bak；文件不存在时跳过。
func backupFile(path string) error {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return os.WriteFile(path+".pre-sync.bak", raw, 0o600)
}

// extractZCodeRules 取出 providerRules 数组；结构缺失返回 nil。
func extractZCodeRules(document map[string]any) []any {
	config, _ := document["config"].(map[string]any)
	if config == nil {
		return nil
	}
	wrapper, _ := config["providerConfigRules"].(map[string]any)
	if wrapper == nil {
		return nil
	}
	rules, _ := wrapper["providerRules"].([]any)
	return rules
}

// extractZCodeModelIDs 找到网关规则（按 baseUrl host:port 匹配）并返回其
// personalModelIds；找不到返回空。
func extractZCodeModelIDs(rules []any, endpoint string) []string {
	want := ""
	if parsed, err := url.Parse(endpoint); err == nil {
		want = parsed.Host
	}
	for _, r := range rules {
		rule, ok := r.(map[string]any)
		if !ok {
			continue
		}
		ruleConfig, _ := rule["config"].(map[string]any)
		api, _ := ruleConfig["api"].(map[string]any)
		if api == nil {
			continue
		}
		base, _ := api["baseUrl"].(string)
		parsed, err := url.Parse(base)
		if err != nil || parsed.Host != want || want == "" {
			continue
		}
		ids, _ := ruleConfig["personalModelIds"].([]any)
		result := make([]string, 0, len(ids))
		for _, id := range ids {
			if s, ok := id.(string); ok {
				result = append(result, s)
			}
		}
		return result
	}
	return nil
}

func (h *ClientConfigHandler) selectedModels(c *gin.Context) ([]map[string]any, bool) {
	available := []map[string]any{}
	for _, model := range h.desiredModels() {
		available = append(available, map[string]any(model))
	}
	return selectSyncModels(c, available)
}
func modelSelectionIDs(models []map[string]any) []string {
	ids := []string{}
	for _, m := range models {
		ids = append(ids, m["id"].(string))
	}
	return ids
}
