package handlers

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/skills"
)

// SkillsHandler 把 custom-skills 技能仓库接到管理面板：浏览技能目录、
// 维护启用清单（skills.enabled.json）、把启用的技能同步（符号链接）到
// 本地技能目录。仓库路径来自 SKILLS_REPO_PATH，未配置时端点保持可用但
// 返回 configured=false，不影响网关其他功能。
type SkillsHandler struct {
	mu     sync.RWMutex
	repo   string
	logger *log.Logger
}

// NewSkillsHandler 创建技能面板 handler；repo 为空表示未启用。
func NewSkillsHandler(repo string, logger *log.Logger) *SkillsHandler {
	return &SkillsHandler{repo: repo, logger: logger}
}

// HandleStatus GET /admin/skills
func (h *SkillsHandler) HandleStatus(c *gin.Context) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if !h.configured(c) {
		return
	}
	if _, err := os.Stat(skills.RegistryPath(h.repo)); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"configured": false,
			"repo":       h.repo,
			"hint":       "仓库里没有 registry/skills.json，请先在技能仓库运行 generate:registry",
		})
		return
	}
	scope, ok := h.scope(c)
	if !ok {
		return
	}
	manifest, _, err := scope.Load()
	if err != nil {
		h.logger.Printf("load skills manifest: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.writeStatus(c, scope, manifest)
}

// HandleDetail GET /admin/skills/:id — 目录条目 + SKILL.md 原文（前端渲染）。
func (h *SkillsHandler) HandleDetail(c *gin.Context) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if !h.configured(c) {
		return
	}
	id := c.Param("id")
	if id == "" || strings.ContainsAny(id, "/\\") || id == "." || id == ".." {
		c.JSON(http.StatusBadRequest, gin.H{"error": "非法技能 ID"})
		return
	}
	registry, err := skills.LoadRegistry(h.repo)
	if err != nil {
		h.logger.Printf("load skills registry: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	var entry *skills.Skill
	for i := range registry {
		if registry[i].ID == id {
			entry = &registry[i]
			break
		}
	}
	if entry == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("技能 %s 不在目录中", id)})
		return
	}
	detail := gin.H{"skill": entry}
	if raw, err := os.ReadFile(filepath.Join(skills.SkillsDir(h.repo), id, "SKILL.md")); err == nil {
		detail["skill_md"] = string(raw)
	}
	c.JSON(http.StatusOK, detail)
}

// HandleUpdateConfig PUT /admin/skills/config — 写入启用清单（写入前轮转备份）。
func (h *SkillsHandler) HandleUpdateConfig(c *gin.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.configured(c) {
		return
	}
	var body struct {
		Targets []string `json:"targets"`
		Enabled []string `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体应为 {targets, enabled}"})
		return
	}
	if body.Targets == nil || body.Enabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "targets 和 enabled 必须是数组；全部停用请显式传入空数组"})
		return
	}
	manifest := skills.Manifest{Version: 1, Targets: body.Targets, Enabled: body.Enabled}
	scope, ok := h.scope(c)
	if !ok {
		return
	}
	if err := backupFile(scope.Path()); err != nil {
		h.logger.Printf("backup skills manifest failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "备份清单失败，未保存修改"})
		return
	}
	if err := scope.Save(manifest); err != nil {
		h.logger.Printf("save skills manifest: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	manifest, _, err := scope.Load()
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	h.logger.Printf("skills manifest updated: %d enabled, %d targets", len(manifest.Enabled), len(manifest.Targets))
	h.writeStatus(c, scope, manifest)
}

// HandleSync POST /admin/skills/sync — 按当前清单落盘（建立/清理符号链接）。
func (h *SkillsHandler) HandleSync(c *gin.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.configured(c) {
		return
	}
	scope, ok := h.scope(c)
	if !ok {
		return
	}
	manifest, _, err := scope.Load()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	report, err := skills.Apply(h.repo, manifest)
	if err != nil {
		h.logger.Printf("apply skills manifest: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.logger.Printf("skills synced to %d target(s)", len(report.Targets))
	st, err := scope.Status(manifest)
	if err != nil {
		h.logger.Printf("build skills status: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"configured":      true,
		"project":         scope.Project,
		"local_skills":    scope.LocalSkills(manifest),
		"repo":            st.Repo,
		"registry_path":   st.RegistryPath,
		"manifest_path":   st.ManifestPath,
		"manifest_exists": st.ManifestExists,
		"targets":         st.Targets,
		"enabled":         st.Enabled,
		"in_sync":         st.InSync,
		"stale_links":     st.StaleLinks,
		"skills":          st.Skills,
		"sync":            report,
	})
}

// configured 报告技能面板是否可用；未配置时写入 200 + configured=false。
func (h *SkillsHandler) configured(c *gin.Context) bool {
	if h.repo != "" {
		return true
	}
	c.JSON(http.StatusOK, gin.H{
		"configured": false,
		"hint":       "设置 SKILLS_REPO_PATH 指向 custom-skills 仓库后启用技能面板",
	})
	return false
}

func (h *SkillsHandler) writeStatus(c *gin.Context, scope skills.Scope, manifest skills.Manifest) {
	st, err := scope.Status(manifest)
	if err != nil {
		h.logger.Printf("build skills status: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"configured":      true,
		"project":         scope.Project,
		"local_skills":    scope.LocalSkills(manifest),
		"repo":            st.Repo,
		"registry_path":   st.RegistryPath,
		"manifest_path":   st.ManifestPath,
		"manifest_exists": st.ManifestExists,
		"targets":         st.Targets,
		"enabled":         st.Enabled,
		"in_sync":         st.InSync,
		"stale_links":     st.StaleLinks,
		"skills":          st.Skills,
	})
}

func (h *SkillsHandler) scope(c *gin.Context) (skills.Scope, bool) {
	scope, err := skills.ResolveScope(h.repo, c.Query("project"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return scope, false
	}
	return scope, true
}
