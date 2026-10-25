package handlers

import (
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/nightgopher/easygateway/internal/assistant"
	"github.com/nightgopher/easygateway/internal/storage"
)

// AssistantAdminHandler exposes prompt customization and feedback endpoints.
// 人设存 agent_settings 表（热更新，无需重启）；反馈存 assistant_feedback，
// 供用户在设置页复盘规律后自己迭代 prompt——agent 无权改写自己的人设。
type AssistantAdminHandler struct {
	assistant *assistant.Assistant
	store     *storage.SQLiteStore
	log       *log.Logger
}

// NewAssistantAdminHandler constructs the handler. assistant or store may be
// nil (assistant disabled / in-memory metrics store) — prompt endpoints then
// answer 503, feedback still works when store exists.
func NewAssistantAdminHandler(a *assistant.Assistant, store *storage.SQLiteStore, logger *log.Logger) *AssistantAdminHandler {
	if logger == nil {
		logger = log.New(nil, "", 0)
	}
	return &AssistantAdminHandler{assistant: a, store: store, log: logger}
}

const settingKeyAssistantPrompt = "assistant_system_prompt"

// HandleGetPrompt GET /admin/assistant/prompt — 返回当前生效 / 自定义 / 内置默认三层。
func (h *AssistantAdminHandler) HandleGetPrompt(c *gin.Context) {
	if h.assistant == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "助理未启用（ASSISTANT_ENABLED=true + ASSISTANT_MODEL）"})
		return
	}
	custom, err := h.store.GetSetting(settingKeyAssistantPrompt)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"prompt":  h.assistant.SystemPrompt(),
		"custom":  custom,
		"default": assistant.DefaultSystemPrompt,
	})
}

// HandlePutPrompt PUT /admin/assistant/prompt — 保存自定义人设并热更新。
// prompt 为空串 = 清除自定义、恢复内置默认。
func (h *AssistantAdminHandler) HandlePutPrompt(c *gin.Context) {
	if h.assistant == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "助理未启用（ASSISTANT_ENABLED=true + ASSISTANT_MODEL）"})
		return
	}
	var req struct {
		Prompt string `json:"prompt"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体需要 {\"prompt\": \"...\"}"})
		return
	}
	prompt := strings.TrimSpace(req.Prompt)
	if len(prompt) > 20000 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "prompt 过长（上限 20000 字符）"})
		return
	}
	if err := h.store.SetSetting(settingKeyAssistantPrompt, prompt); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.assistant.UpdateSystemPrompt(prompt)
	h.log.Printf("Assistant system prompt updated (custom=%v, %d chars)", prompt != "", len(prompt))
	c.JSON(http.StatusOK, gin.H{
		"prompt":  h.assistant.SystemPrompt(),
		"custom":  prompt,
		"default": assistant.DefaultSystemPrompt,
	})
}

// HandleAddFeedback POST /admin/assistant/feedback — 对一轮回复打分（可带备注）。
func (h *AssistantAdminHandler) HandleAddFeedback(c *gin.Context) {
	var req struct {
		Rating string `json:"rating"` // up | down
		Reply  string `json:"reply"`
		Note   string `json:"note"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体需要 {\"rating\": \"up|down\", ...}"})
		return
	}
	if req.Rating != "up" && req.Rating != "down" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "rating 只接受 up 或 down"})
		return
	}
	if h.store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "存储不可用（SQLite 初始化失败）"})
		return
	}
	if err := h.store.AddAssistantFeedback(req.Rating, strings.TrimSpace(req.Reply), strings.TrimSpace(req.Note)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"saved": true})
}

// HandleListFeedback GET /admin/assistant/feedback?limit=50 — 近期反馈，供设置页复盘。
func (h *AssistantAdminHandler) HandleListFeedback(c *gin.Context) {
	if h.store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "存储不可用（SQLite 初始化失败）"})
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	feedback, err := h.store.ListAssistantFeedback(limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if feedback == nil {
		feedback = []storage.AssistantFeedback{}
	}
	c.JSON(http.StatusOK, gin.H{"feedback": feedback, "total": len(feedback)})
}
