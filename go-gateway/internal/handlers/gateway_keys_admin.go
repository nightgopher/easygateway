package handlers

import (
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/gwkeys"
)

// GatewayKeyHandler 管理 gateway key 多密钥体系（Admin API）。
// gateway key 是发给具体客户端的受限身份：模型白名单 + 按天/周/月的
// token 限额；master key 仍是管理员身份。完整 token 只在创建与轮换的
// 响应里出现一次，列表只返回 token_hint。
type GatewayKeyHandler struct {
	keys   *gwkeys.Store
	logger *log.Logger
}

func NewGatewayKeyHandler(keys *gwkeys.Store, logger *log.Logger) *GatewayKeyHandler {
	return &GatewayKeyHandler{keys: keys, logger: logger}
}

// keysUnavailable 统一处理密钥库禁用（加载失败）时的响应。
func (h *GatewayKeyHandler) keysUnavailable(c *gin.Context) bool {
	if h.keys == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "gateway keys unavailable: key store disabled"})
		return true
	}
	return false
}

// HandleList GET /admin/gateway-keys
func (h *GatewayKeyHandler) HandleList(c *gin.Context) {
	if h.keysUnavailable(c) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"keys": h.keys.List()})
}

// HandleCreate POST /admin/gateway-keys
// body: {"name": "...", "allowed_models": ["glm-*"], "limits": {"day": 500000, "week": 2000000}}
func (h *GatewayKeyHandler) HandleCreate(c *gin.Context) {
	if h.keysUnavailable(c) {
		return
	}
	var input gwkeys.CreateInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if input.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	key, err := h.keys.Create(input, time.Now())
	if err != nil {
		h.logger.Printf("Create gateway key error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.logger.Printf("Gateway key created: %s (%s), models=%v limits=day:%d/week:%d/month:%d",
		key.ID, key.Name, key.AllowedModels, key.Limits.Day, key.Limits.Week, key.Limits.Month)
	c.JSON(http.StatusCreated, key)
}

// HandleRotate POST /admin/gateway-keys/:id/rotate —— 换新 token，旧 token 立即失效。
func (h *GatewayKeyHandler) HandleRotate(c *gin.Context) {
	if h.keysUnavailable(c) {
		return
	}
	key, err := h.keys.Rotate(c.Param("id"), time.Now())
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	h.logger.Printf("Gateway key rotated: %s (%s)", key.ID, key.Name)
	c.JSON(http.StatusOK, key)
}

// HandleSetEnabled PATCH /admin/gateway-keys/:id  body: {"enabled": bool}
func (h *GatewayKeyHandler) HandleSetEnabled(c *gin.Context) {
	if h.keysUnavailable(c) {
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Enabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "enabled (bool) is required"})
		return
	}
	if err := h.keys.SetEnabled(c.Param("id"), *body.Enabled); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": c.Param("id"), "enabled": *body.Enabled})
}

// HandleRemove DELETE /admin/gateway-keys/:id
func (h *GatewayKeyHandler) HandleRemove(c *gin.Context) {
	if h.keysUnavailable(c) {
		return
	}
	if err := h.keys.Remove(c.Param("id")); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	h.logger.Printf("Gateway key removed: %s", c.Param("id"))
	c.JSON(http.StatusOK, gin.H{"id": c.Param("id"), "removed": true})
}
