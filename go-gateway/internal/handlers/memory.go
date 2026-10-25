package handlers

import (
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/nightgopher/easygateway/internal/memory"
)

// MemoryHandler is the client-facing memory API. 读写分离遵循"agent 起草、
// 人拍板"的治理：读（按作用域路由）只返回 active；写（agent/客户端起草）
// 一律进 candidate，经 /admin/memories/:id/confirm 后才生效。
type MemoryHandler struct {
	store memory.Store
	log   *log.Logger
}

// NewMemoryHandler constructs the client-facing handler. store may be a
// NoopStore when the feature is disabled — lookups return an empty block and
// drafts are accepted but inert, so callers need no feature flag.
func NewMemoryHandler(store memory.Store, logger *log.Logger) *MemoryHandler {
	if store == nil {
		store = memory.NoopStore{}
	}
	return &MemoryHandler{store: store, log: logger}
}

// HandleLookup GET /v1/memory?client=&project=&limit=
// 作用域路由：project ⊕ client ⊕ global 三环合并，越窄越靠前。
// 响应同时给出渲染好的注入块（RenderBlock），wrapper 可直接使用。
func (h *MemoryHandler) HandleLookup(c *gin.Context) {
	limit := parsePositiveInt(c.Query("limit"), 20, 100)
	memories, err := h.store.LookupFor(
		strings.TrimSpace(c.Query("client")),
		strings.TrimSpace(c.Query("project")),
		limit,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"memories": memories,
		"block":    memory.RenderBlock(memories),
	})
}

// HandleDraft POST /v1/memory — agent/客户端起草一条候选记忆。
// draft 一律落 candidate；重复命题刷新既有行（吸收重试）。
type draftMemoryRequest struct {
	Statement string `json:"statement"`
	ScopeType string `json:"scope_type"`
	ScopeKey  string `json:"scope_key"`
	Client    string `json:"client"`
}

func (h *MemoryHandler) HandleDraft(c *gin.Context) {
	var req draftMemoryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体非法: " + err.Error()})
		return
	}
	scopeType, scopeKey, err := memory.NormalizeScope(memory.ScopeType(req.ScopeType), req.ScopeKey)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	statement, err := memory.SanitizeStatement(req.Statement)
	if err != nil {
		status := http.StatusBadRequest
		if err == memory.ErrSecretLike {
			status = http.StatusUnprocessableEntity
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	source := "api"
	if client := strings.TrimSpace(req.Client); client != "" {
		source = "agent:" + client
	}
	entry, inserted, err := h.store.Insert(memory.Memory{
		ScopeType:  scopeType,
		ScopeKey:   scopeKey,
		Statement:  statement,
		Status:     memory.StatusCandidate,
		Source:     source,
		Confidence: 0.3, // 起草置信度固定低值，确认后由 admin 侧调整
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"memory":   entry,
		"inserted": inserted,
		"note":     "候选记忆待管理员确认后生效（POST /admin/memories/:id/confirm）",
	})
}
