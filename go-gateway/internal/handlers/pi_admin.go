package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/piconfig"
	"github.com/nightgopher/easygateway/internal/provider"
)

// PiConfigHandler 把 Pi 客户端的模型清单同步搬进控制面板：
// 看板里一键完成原本需要 `llm-gateway setup pi` 手动执行的动作。
type PiConfigHandler struct {
	gatewayHome string
	piHome      string
	logger      *log.Logger
	router      *provider.Router
}

// NewPiConfigHandler 创建 Pi 配置面板 handler
func NewPiConfigHandler(gatewayHome, piHome string, logger *log.Logger, routers ...*provider.Router) *PiConfigHandler {
	h := &PiConfigHandler{gatewayHome: gatewayHome, piHome: piHome, logger: logger}
	if len(routers) > 0 {
		h.router = routers[0]
	}
	return h
}

func (h *PiConfigHandler) modelsPath() string {
	return piconfig.ModelsFilePath(h.piHome)
}

// piStatus 汇总看板需要展示的状态。currentIDs 为空表示 Pi 配置还不存在。
func (h *PiConfigHandler) piStatus() gin.H {
	desired := h.desiredModels()
	desiredIDs := make([]string, 0, len(desired))
	for _, m := range desired {
		desiredIDs = append(desiredIDs, m["id"].(string))
	}

	path := h.modelsPath()
	currentIDs := []string{}
	fileExists := false
	inSync := false
	raw, err := os.ReadFile(path)
	if err == nil {
		fileExists = true
		currentIDs = currentModelIDs(raw)
		inSync = equalStringSets(desiredIDs, currentIDs)
	} else if !os.IsNotExist(err) {
		h.logger.Printf("read Pi models.json: %v", err)
	}

	return gin.H{
		"path":         path,
		"file_exists":  fileExists,
		"in_sync":      inSync,
		"desired":      desired,
		"current_ids":  currentIDs,
		"desired_ids":  desiredIDs,
		"missing_ids":  difference(desiredIDs, currentIDs),
		"stale_ids":    difference(currentIDs, desiredIDs),
		"gateway_home": h.gatewayHome,
	}
}

// HandleStatus GET /admin/pi
func (h *PiConfigHandler) HandleStatus(c *gin.Context) {
	c.JSON(http.StatusOK, h.piStatus())
}

// HandleSync POST /admin/pi/sync
// 与 `llm-gateway setup pi` 完全同一合并逻辑；写入前把现有文件轮转备份一份。
func (h *PiConfigHandler) HandleSync(c *gin.Context) {
	models, ok := selectSyncModels(c, h.desiredModels())
	if !ok {
		return
	}
	path := h.modelsPath()
	if raw, err := os.ReadFile(path); err == nil {
		backup := path + ".pre-sync.bak"
		if err := os.WriteFile(backup, raw, 0o600); err != nil {
			h.logger.Printf("backup Pi models.json failed: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("备份现有配置失败: %v", err)})
			return
		}
	}

	merged, _, err := piconfig.Setup(piconfig.SetupOptions{
		GatewayHome: h.gatewayHome,
		PiHome:      h.piHome,
		Models:      models,
	})
	if err != nil {
		h.logger.Printf("sync Pi models.json failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	status := h.piStatus()
	status["synced"] = true
	status["bytes"] = len(merged)
	h.logger.Printf("Pi models.json synced from dashboard: %s", path)
	c.JSON(http.StatusOK, status)
}

// currentModelIDs 从现有 models.json 里取 llm-gateway 组的模型 ID 列表。
func currentModelIDs(raw []byte) []string {
	ids := []string{}
	var document struct {
		Providers map[string]struct {
			Models []struct {
				ID string `json:"id"`
			} `json:"models"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		return ids
	}
	provider, ok := document.Providers[piconfig.ProviderID]
	if !ok {
		return ids
	}
	for _, m := range provider.Models {
		ids = append(ids, m.ID)
	}
	return ids
}

func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, v := range a {
		seen[v]++
	}
	for _, v := range b {
		if seen[v] == 0 {
			return false
		}
		seen[v]--
	}
	return true
}

func difference(a, b []string) []string {
	inB := make(map[string]bool, len(b))
	for _, v := range b {
		inB[v] = true
	}
	missing := []string{}
	for _, v := range a {
		if !inB[v] {
			missing = append(missing, v)
		}
	}
	return missing
}

func (h *PiConfigHandler) desiredModels() []map[string]any {
	if h.router == nil {
		return piconfig.DesiredModels()
	}
	models := []map[string]any{}
	for _, info := range h.router.ListModelInfos() {
		if !hasTextCapability(info.Capabilities) || hasCapabilityFlagValue(info.Capabilities) {
			continue
		}
		// 渠道未登录或 provider 已停用的模型写进客户端也无法使用，不推荐。
		if !h.router.ModelUsable(info.ID) {
			continue
		}
		model := map[string]any{"id": info.ID, "name": info.ID}
		if info.Protocol == "responses" {
			model["api"] = "openai-responses"
		}
		if info.MaxInputTokens > 0 {
			model["contextWindow"] = info.MaxInputTokens
		}
		if info.MaxOutputTokens > 0 {
			model["maxTokens"] = info.MaxOutputTokens
		}
		if len(info.InputModalities) > 0 {
			model["input"] = info.InputModalities
		}
		models = append(models, model)
	}
	return models
}

// Validate a selection before touching client files. Omitted bodies retain legacy sync behavior.
func selectSyncModels(c *gin.Context, available []map[string]any) ([]map[string]any, bool) {
	var request struct {
		ModelIDs *[]string `json:"model_ids"`
	}
	if c.Request.Body != nil {
		err := json.NewDecoder(c.Request.Body).Decode(&request)
		if err != nil && err != io.EOF {
			c.JSON(400, gin.H{"error": "模型选择格式无效"})
			return nil, false
		}
	}
	if request.ModelIDs == nil {
		return available, true
	}
	if len(*request.ModelIDs) == 0 {
		c.JSON(400, gin.H{"error": "请至少选择一个模型"})
		return nil, false
	}
	selected := []map[string]any{}
	byID := map[string]map[string]any{}
	seen := map[string]bool{}
	for _, m := range available {
		byID[m["id"].(string)] = m
	}
	for _, id := range *request.ModelIDs {
		m, ok := byID[id]
		if !ok {
			c.JSON(400, gin.H{"error": "不可同步的模型: " + id})
			return nil, false
		}
		if !seen[id] {
			selected = append(selected, m)
			seen[id] = true
		}
	}
	return selected, true
}
