package handlers

import (
	"context"
	"github.com/gin-gonic/gin"
	"net/http"
	"time"
)

func (h *AdminHandler) HandleDoctor(c *gin.Context) {
	var request struct {
		ProbeAnonymous bool `json:"probe_anonymous"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "expected JSON with optional probe_anonymous boolean"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()
	report, err := h.router.Doctor(ctx, request.ProbeAnonymous)
	if err != nil {
		status := http.StatusConflict
		if ctx.Err() != nil {
			status = http.StatusGatewayTimeout
		}
		c.JSON(status, gin.H{"error": err.Error(), "report": report})
		return
	}
	c.JSON(http.StatusOK, report)
}
