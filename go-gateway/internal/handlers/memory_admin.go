package handlers

import (
	"log"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/nightgopher/easygateway/internal/memory"
)

// MemoryAdminHandler exposes governance endpoints for the memory data plane.
// It is mounted under /admin/memories and protected by the same admin auth
// middleware as the rest of the admin API. 人拍板入口：confirm/retire/delete。
type MemoryAdminHandler struct {
	store memory.Store
	log   *log.Logger
}

// NewMemoryAdminHandler constructs the admin handler. store may be a
// NoopStore when the feature is disabled — endpoints still respond so the
// dashboard needs no feature flag.
func NewMemoryAdminHandler(store memory.Store, logger *log.Logger) *MemoryAdminHandler {
	if store == nil {
		store = memory.NoopStore{}
	}
	return &MemoryAdminHandler{store: store, log: logger}
}

// HandleList GET /admin/memories?status=&scope_type=&scope_key=&limit=&offset=
func (h *MemoryAdminHandler) HandleList(c *gin.Context) {
	limit := parsePositiveInt(c.Query("limit"), 50, 500)
	offset := parsePositiveInt(c.Query("offset"), 0, 100000)
	filter := memory.ListFilter{
		Status:    memory.Status(c.Query("status")),
		ScopeType: memory.ScopeType(c.Query("scope_type")),
		ScopeKey:  c.Query("scope_key"),
		Limit:     limit,
		Offset:    offset,
	}
	memories, total, err := h.store.List(filter)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"memories": memories,
		"total":    total,
		"limit":    limit,
		"offset":   offset,
	})
}

// HandleCreate POST /admin/memories — human-authored entry, directly active.
type createMemoryRequest struct {
	ScopeType  string  `json:"scope_type"`
	ScopeKey   string  `json:"scope_key"`
	Statement  string  `json:"statement"`
	Source     string  `json:"source"`
	Confidence float64 `json:"confidence"`
}

func (h *MemoryAdminHandler) HandleCreate(c *gin.Context) {
	var req createMemoryRequest
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
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	source := req.Source
	if source == "" {
		source = "manual"
	}
	entry, inserted, err := h.store.Insert(memory.Memory{
		ScopeType:  scopeType,
		ScopeKey:   scopeKey,
		Statement:  statement,
		Status:     memory.StatusActive,
		Source:     source,
		Confidence: req.Confidence,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"memory": entry, "inserted": inserted})
}

// HandleConfirm POST /admin/memories/:id/confirm — 人拍板：candidate → active。
func (h *MemoryAdminHandler) HandleConfirm(c *gin.Context) {
	h.transition(c, memory.StatusActive)
}

// HandleRetire POST /admin/memories/:id/retire — 过时/被反驳 → retired。
func (h *MemoryAdminHandler) HandleRetire(c *gin.Context) {
	h.transition(c, memory.StatusRetired)
}

func (h *MemoryAdminHandler) transition(c *gin.Context, status memory.Status) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id 必须是整数"})
		return
	}
	updated, err := h.store.SetStatus(id, status)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !updated {
		c.JSON(http.StatusNotFound, gin.H{"error": "记忆不存在"})
		return
	}
	entry, err := h.store.Get(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"memory": entry})
}

// HandleDelete DELETE /admin/memories/:id
func (h *MemoryAdminHandler) HandleDelete(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id 必须是整数"})
		return
	}
	deleted, err := h.store.Delete(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !deleted {
		c.JSON(http.StatusNotFound, gin.H{"error": "记忆不存在"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": true})
}
