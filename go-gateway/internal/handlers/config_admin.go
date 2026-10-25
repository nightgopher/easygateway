package handlers

import (
	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/provider"
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

func (h *ConfigAdminHandler) HandleCORS(c *gin.Context) {
	c.JSON(http.StatusOK, h.reloader.CORSConfig())
}

func (h *ConfigAdminHandler) HandleUpdateCORS(c *gin.Context) {
	var request struct {
		AllowedOrigins *[]string `json:"allowed_origins"`
	}
	if err := c.ShouldBindJSON(&request); err != nil || request.AllowedOrigins == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "allowed_origins must be an array; use [] to allow all origins"})
		return
	}
	config, err := h.reloader.UpdateCORS(*request.AllowedOrigins)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, config)
}
