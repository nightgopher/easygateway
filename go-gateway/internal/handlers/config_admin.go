package handlers

import (
	"github.com/gin-gonic/gin"
	"github.com/weijian/go-llm-gateway/internal/provider"
	"net/http"
)

type ConfigAdminHandler struct{ reloader *provider.ConfigReloader }

func NewConfigAdminHandler(reloader *provider.ConfigReloader) *ConfigAdminHandler {
	return &ConfigAdminHandler{reloader: reloader}
}

func (h *ConfigAdminHandler) HandleStatus(c *gin.Context) { c.JSON(http.StatusOK, h.reloader.Status()) }

func (h *ConfigAdminHandler) HandleCheck(c *gin.Context) {
	report, err := h.reloader.Check()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "report": report})
		return
	}
	c.JSON(http.StatusOK, gin.H{"valid": true, "report": report})
}

func (h *ConfigAdminHandler) HandleReload(c *gin.Context) {
	status, err := h.reloader.Reload()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "status": status})
		return
	}
	c.JSON(http.StatusOK, status)
}
