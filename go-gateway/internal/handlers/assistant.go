package handlers

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/nightgopher/easygateway/internal/assistant"
)

// AssistantHandler streams the resident assistant over SSE. Mounted under
// /admin/assistant (admin token): the assistant can draft memory candidates
// and read active memories, so it inherits the admin trust boundary.
type AssistantHandler struct {
	assistant *assistant.Assistant
	log       *log.Logger
}

// NewAssistantHandler constructs the handler; assistant may be nil when the
// feature is disabled — the endpoint then answers 503 instead of vanishing.
func NewAssistantHandler(assistant *assistant.Assistant, logger *log.Logger) *AssistantHandler {
	return &AssistantHandler{assistant: assistant, log: logger}
}

// HandleModels GET /admin/assistant/models — 聊天可选的模型清单与默认值。
func (h *AssistantHandler) HandleModels(c *gin.Context) {
	if h.assistant == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "助理未启用（ASSISTANT_ENABLED=true + ASSISTANT_MODEL）"})
		return
	}
	options := h.assistant.ModelOptions()
	models := make([]string, 0, len(options))
	for _, option := range options {
		models = append(models, option.ID)
	}
	c.JSON(http.StatusOK, gin.H{"default": h.assistant.DefaultModel(), "models": models, "model_options": options})
}

// HandleChat POST /admin/assistant/chat {message, model?} → SSE 流。
// model 缺省用 ASSISTANT_MODEL；事件形如 data: {"type":"text_delta","content":"..."} /
// tool_start / tool_end，结束帧 data: {"type":"done","content":"最终回复"} 或 {"type":"error"}。
func (h *AssistantHandler) HandleChat(c *gin.Context) {
	if h.assistant == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "助理未启用（ASSISTANT_ENABLED=true + ASSISTANT_MODEL）"})
		return
	}
	var req struct {
		Message string `json:"message"`
		Model   string `json:"model"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Message == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体需要 {\"message\": \"...\"}"})
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
		c.Writer.WriteString("data: " + string(raw) + "\n\n")
		flusher.Flush()
	}

	final, err := h.assistant.Chat(c.Request.Context(), req.Message, req.Model, func(update assistant.StreamUpdate) {
		if update.Type == "text_delta" {
			writeEvent(map[string]any{"type": "text_delta", "content": update.Content})
			return
		}
		writeEvent(map[string]any{"type": update.Type, "tool": update.Tool})
	})
	if err != nil {
		if c.Request.Context().Err() != nil {
			return // 客户端断开，无需收尾帧
		}
		writeEvent(map[string]any{"type": "error", "content": err.Error()})
		return
	}
	writeEvent(map[string]any{"type": "done", "content": final})
}
