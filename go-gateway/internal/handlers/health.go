package handlers

import (
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/provider"
)

// HealthHandler 处理 /health 端点
type HealthHandler struct {
	router *provider.Router
	logger *log.Logger
}

func NewHealthHandler(router *provider.Router, logger *log.Logger) *HealthHandler {
	return &HealthHandler{router: router, logger: logger}
}

func (h *HealthHandler) Handle(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// HandleReady is a lightweight readiness probe. Liveness remains independent
// of provider availability so orchestrators can distinguish a running process
// from a gateway that has not loaded any routeable provider yet.
func (h *HealthHandler) HandleReady(c *gin.Context) {
	if h.router == nil || len(h.router.ListProviders()) == 0 || len(h.router.ListChains()) == 0 {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"status": "not_ready",
			"reason": "no routeable providers configured",
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ready"})
}
