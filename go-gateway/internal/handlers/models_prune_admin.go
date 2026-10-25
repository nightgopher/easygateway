package handlers

import (
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/modelsprune"
	"github.com/nightgopher/easygateway/internal/provider"
)

// ModelsPruneHandler 提供「清理失败模型」：把最近探活被上游确定性拒绝
// （401/402/403/404）的模型从 providers.yaml 移除并热重载。
//
// 安全边界：
//   - 只允许移除**当前候选集**内的模型（请求可指定子集，不能越界删健康模型）；
//   - 删除前先把清理后的配置写入临时文件、经真实加载器解析验证；
//   - 原配置备份到 <config>.prune-bak；热重载失败时回滚备份，保证磁盘与
//     运行时一致。
type ModelsPruneHandler struct {
	router     *provider.Router
	reloader   *provider.ConfigReloader
	configPath string
	logger     *log.Logger
}

func NewModelsPruneHandler(router *provider.Router, reloader *provider.ConfigReloader, configPath string, logger *log.Logger) *ModelsPruneHandler {
	return &ModelsPruneHandler{router: router, reloader: reloader, configPath: configPath, logger: logger}
}

type pruneRequest struct {
	DryRun bool     `json:"dry_run"`
	Models []string `json:"models"` // 空 = 全部候选
}

// HandlePrune POST /admin/models/prune
func (h *ModelsPruneHandler) HandlePrune(c *gin.Context) {
	var req pruneRequest
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	candidateList := modelsprune.Candidates(h.router.ModelCatalog())
	// 链别名与运行时动态模型不是 providers.yaml 里的可删条目，先过滤掉，
	// 避免它们混进预览清单（RemoveModels 对未声明模型只会 skip）。
	declared := map[string]bool{}
	if config, err := os.ReadFile(h.configPath); err == nil {
		declared = modelsprune.DeclaredModels(string(config))
	}
	filtered := candidateList[:0:0]
	for _, candidate := range candidateList {
		if declared[candidate.Model] {
			filtered = append(filtered, candidate)
		}
	}
	candidateList = filtered
	byModel := make(map[string]modelsprune.Candidate, len(candidateList))
	for _, candidate := range candidateList {
		byModel[candidate.Model] = candidate
	}

	// 计算实际生效的清理集：请求子集必须落在候选集内；请求了但不在候选集
	// 内的模型逐个说明，不静默忽略。
	var effective []string
	var notCandidates []modelsprune.Skipped
	if len(req.Models) == 0 {
		for _, candidate := range candidateList {
			effective = append(effective, candidate.Model)
		}
	} else {
		requested := make(map[string]bool, len(req.Models))
		for _, m := range req.Models {
			requested[m] = true
		}
		for _, candidate := range candidateList {
			if requested[candidate.Model] {
				effective = append(effective, candidate.Model)
			}
		}
		for _, m := range req.Models {
			if _, ok := byModel[m]; !ok {
				notCandidates = append(notCandidates, modelsprune.Skipped{Model: m, Reason: "not a prune candidate (healthy, degraded-only, or never probed)"})
			}
		}
		if len(notCandidates) > 0 && len(effective) == 0 {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "none of the requested models are prune candidates", "skipped": notCandidates})
			return
		}
	}

	if len(effective) == 0 {
		c.JSON(http.StatusOK, gin.H{
			"dry_run":    req.DryRun,
			"candidates": candidateList,
			"removed":    []modelsprune.Candidate{},
			"skipped":    []modelsprune.Skipped{},
			"message":    "没有可清理的失败模型",
		})
		return
	}

	if req.DryRun {
		c.JSON(http.StatusOK, gin.H{
			"dry_run":    true,
			"candidates": candidateList,
			"to_remove":  pickCandidates(candidateList, effective),
			"skipped":    notCandidates,
		})
		return
	}

	config, err := os.ReadFile(h.configPath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read providers.yaml: " + err.Error()})
		return
	}
	next, result, err := modelsprune.RemoveModels(string(config), effective)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	result.Skipped = append(result.Skipped, notCandidates...)
	// Removed 条目补全 provider 与失败原因（供桌面端确认结果展示）。
	for i := range result.Removed {
		if candidate, ok := byModel[result.Removed[i].Model]; ok {
			result.Removed[i].Provider, result.Removed[i].Reason = candidate.Provider, candidate.Reason
		}
	}

	// 解析验证：清理后的文本必须能被真实加载器加载，才允许落盘。
	temp := filepath.Join(os.TempDir(), "easygateway-prune-check.yaml")
	if err := os.WriteFile(temp, []byte(next), 0600); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "write temp config: " + err.Error()})
		return
	}
	defer os.Remove(temp)
	if _, err := provider.LoadProvidersConfig(temp); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "pruned config failed validation: " + err.Error()})
		return
	}

	backup := h.configPath + ".prune-bak"
	if err := writeFilePreservingMode(backup, config); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "write backup: " + err.Error()})
		return
	}
	mode := os.FileMode(0644)
	if info, err := os.Stat(h.configPath); err == nil {
		mode = info.Mode()
	}
	if err := os.WriteFile(h.configPath, []byte(next), mode); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "write providers.yaml: " + err.Error()})
		return
	}

	// 热重载；失败回滚备份并重载旧配置，磁盘与运行时保持一致。
	if _, err := h.reloader.Reload(); err != nil {
		h.logger.Printf("Prune reload failed, rolling back %s: %v", backup, err)
		if restoreErr := os.WriteFile(h.configPath, config, mode); restoreErr == nil {
			_, _ = h.reloader.Reload()
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "reload after prune failed, config restored from backup: " + err.Error()})
		return
	}

	h.logger.Printf("Pruned %d model(s) from %s (providers removed: %v, chains affected: %v)",
		len(result.Removed), h.configPath, result.ProvidersRemoved, chainNames(result.ChainsAffected))
	c.JSON(http.StatusOK, gin.H{
		"dry_run":           false,
		"removed":           result.Removed,
		"skipped":           result.Skipped,
		"providers_removed": result.ProvidersRemoved,
		"chains_affected":   result.ChainsAffected,
		"config_path":       h.configPath,
		"backup_path":       backup,
	})
}

func pickCandidates(all []modelsprune.Candidate, effective []string) []modelsprune.Candidate {
	wanted := make(map[string]bool, len(effective))
	for _, m := range effective {
		wanted[m] = true
	}
	out := make([]modelsprune.Candidate, 0, len(effective))
	for _, candidate := range all {
		if wanted[candidate.Model] {
			out = append(out, candidate)
		}
	}
	return out
}

func chainNames(impacts []modelsprune.ChainImpact) []string {
	names := make([]string, 0, len(impacts))
	for _, impact := range impacts {
		names = append(names, impact.Chain)
	}
	return names
}

func writeFilePreservingMode(path string, data []byte) error {
	mode := os.FileMode(0600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode()
	}
	return os.WriteFile(path, data, mode)
}
