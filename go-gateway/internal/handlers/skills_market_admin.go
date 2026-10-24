package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easyagent/sdk/skillmarket"
)

// SkillsMarketHandler 把远端技能商城接到管理面板：浏览/搜索远端目录、
// 安装技能包到个人技能目录、卸载市场安装的技能。商城是可选能力：
// client 为 nil（未启用）时端点保持可用但返回 enabled=false，不影响
// 技能面板的其他功能。安装目录与 EasyAgent 同源（默认 ~/.agents/skills），
// 安装清单标记一致，两边互相识别对方安装的技能；技能同步只清理指向
// 技能仓库的符号链接，与市场安装互不影响。
type SkillsMarketHandler struct {
	client *skillmarket.Client
	logger *log.Logger
}

// NewSkillsMarketHandler 创建技能市场 handler；enabled=false 或缺少安装
// 目录时返回未启用状态。市场 HTTP 客户端的超时放宽到 10 分钟：安装包
// 下载（最大 200MB）在慢速链路上需要远超默认 15 秒的时间。
func NewSkillsMarketHandler(skillsDir, baseURL string, enabled bool, logger *log.Logger) *SkillsMarketHandler {
	if !enabled || skillsDir == "" {
		return &SkillsMarketHandler{logger: logger}
	}
	opts := []skillmarket.Option{skillmarket.WithHTTPClient(&http.Client{Timeout: 10 * time.Minute})}
	if baseURL != "" {
		opts = append(opts, skillmarket.WithBaseURL(baseURL))
	}
	return &SkillsMarketHandler{client: skillmarket.NewClient(skillsDir, opts...), logger: logger}
}

// Client 暴露市场客户端给安装任务 handler，共用同一实例以保证
// 安装串行化与进度回调一致。
func (h *SkillsMarketHandler) Client() *skillmarket.Client { return h.client }

// HandleBrowse GET /admin/skills/market/browse?q=&category=&sort=&page=&section=
func (h *SkillsMarketHandler) HandleBrowse(c *gin.Context) {
	if !h.enabled(c) {
		return
	}
	page, _ := strconv.Atoi(c.Query("page"))
	section, _ := strconv.Atoi(c.Query("section"))
	result, err := h.client.Search(c.Request.Context(), skillmarket.BrowseQuery{
		Query:    c.Query("q"),
		Category: c.Query("category"),
		Sort:     c.Query("sort"),
		Page:     page,
		Section:  section,
	})
	if err != nil {
		h.logger.Printf("skill market browse: %v", err)
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"enabled": true, "skills": result.Skills, "total": result.Total, "page": result.Page})
}

// HandleSections GET /admin/skills/market/sections — 商城主题分组。
func (h *SkillsMarketHandler) HandleSections(c *gin.Context) {
	if !h.enabled(c) {
		return
	}
	sections, err := h.client.Sections(c.Request.Context())
	if err != nil {
		h.logger.Printf("skill market sections: %v", err)
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"enabled": true, "sections": sections})
}

// HandleInstalled GET /admin/skills/market/installed — 扫描安装目录里带
// 市场清单的技能（含 EasyAgent 安装的）。
func (h *SkillsMarketHandler) HandleInstalled(c *gin.Context) {
	if !h.enabled(c) {
		return
	}
	installed, err := h.client.ListInstalled()
	if err != nil {
		h.logger.Printf("skill market installed: %v", err)
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"enabled": true, "skills_dir": h.client.SkillsDir(), "installed": installed})
}

// HandleInstall POST /admin/skills/market/install {"id":123} — 下载、校验
// 并解包商城技能；元数据以商城详情响应为准。
func (h *SkillsMarketHandler) HandleInstall(c *gin.Context) {
	if !h.enabled(c) {
		return
	}
	var body struct {
		ID int `json:"id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.ID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体应为 {\"id\": <技能 ID>}"})
		return
	}
	item, err := h.client.Install(c.Request.Context(), body.ID)
	if err != nil {
		h.logger.Printf("skill market install %d: %v", body.ID, err)
		h.writeError(c, err)
		return
	}
	h.logger.Printf("skill market installed: %s (id=%d) -> %s", item.Name, item.ID, h.client.SkillsDir())
	c.JSON(http.StatusOK, gin.H{"enabled": true, "skill": item})
}

// HandleUninstall POST /admin/skills/market/uninstall {"name":"skill-slug"} —
// 只删除带市场清单的目录，手写技能永远不可经此路径删除。
func (h *SkillsMarketHandler) HandleUninstall(c *gin.Context) {
	if !h.enabled(c) {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体应为 {\"name\": \"<技能目录名>\"}"})
		return
	}
	if err := h.client.Uninstall(body.Name); err != nil {
		h.logger.Printf("skill market uninstall %s: %v", body.Name, err)
		h.writeError(c, err)
		return
	}
	h.logger.Printf("skill market uninstalled: %s", body.Name)
	c.JSON(http.StatusOK, gin.H{"enabled": true})
}

// enabled 报告市场面板是否可用；未启用时写入 200 + enabled=false。
func (h *SkillsMarketHandler) enabled(c *gin.Context) bool {
	if h.client != nil {
		return true
	}
	c.JSON(http.StatusOK, gin.H{
		"enabled": false,
		"hint":    "技能市场未启用：去掉 SKILLS_MARKET_ENABLED=false 或确认 SKILLS_MARKET_SKILLS_DIR 可用后重启网关",
	})
	return false
}

// marketErrorMessage 把运营性失败翻译成可直接展示的文案。
func marketErrorMessage(err error) string {
	switch {
	case errors.Is(err, skillmarket.ErrNotFound):
		return "技能市场里没有这个技能"
	case errors.Is(err, skillmarket.ErrAlreadyInstalled):
		return "本机已有同名技能目录（可能由 EasyAgent 安装或手写），安装已取消"
	case errors.Is(err, skillmarket.ErrDigestMismatch):
		return "下载包校验不匹配，已放弃安装"
	case errors.Is(err, skillmarket.ErrNotMarketplace):
		return "目标目录不是市场安装的技能，拒绝卸载"
	default:
		return "技能市场请求失败: " + err.Error()
	}
}

// writeError 把运营性失败输出为 200 + {"error"}，桌面端可直接展示原文；
// 认证与连接层错误仍由传输层处理。
func (h *SkillsMarketHandler) writeError(c *gin.Context, err error) {
	c.JSON(http.StatusOK, gin.H{"enabled": true, "error": marketErrorMessage(err)})
}

// HandleInstallStream POST /admin/skills/market/install-stream {"id":N} —
// SSE 流式安装：逐事件推送阶段与下载字节进度；失败以
// {"phase":"failed","error":...} 带内返回。
func (h *SkillsMarketHandler) HandleInstallStream(c *gin.Context) {
	if !h.enabled(c) {
		return
	}
	var body struct {
		ID int `json:"id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.ID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体应为 {\"id\": <技能 ID>}"})
		return
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "streaming unsupported"})
		return
	}
	writeEvent := func(payload map[string]any) {
		raw, err := json.Marshal(payload)
		if err != nil {
			return
		}
		fmt.Fprintf(c.Writer, "data: %s\n\n", raw)
		flusher.Flush()
	}

	writeEvent(map[string]any{"phase": "resolving"})
	item, err := h.client.InstallWithProgress(c.Request.Context(), body.ID, func(p skillmarket.InstallProgress) {
		switch p.Phase {
		case skillmarket.PhaseDownloading:
			writeEvent(map[string]any{"phase": "downloading", "bytes_done": p.BytesDone, "bytes_total": p.BytesTotal})
		case skillmarket.PhaseVerifying:
			writeEvent(map[string]any{"phase": "verifying"})
		case skillmarket.PhaseExtracting:
			writeEvent(map[string]any{"phase": "extracting"})
		}
	})
	if err != nil {
		h.logger.Printf("skill market install %d: %v", body.ID, err)
		writeEvent(map[string]any{"phase": "failed", "error": marketErrorMessage(err)})
		return
	}
	h.logger.Printf("skill market installed (stream): %s (id=%d) -> %s", item.Name, item.ID, h.client.SkillsDir())
	writeEvent(map[string]any{"phase": "done", "name": item.Name, "display_name": item.DisplayName})
}
