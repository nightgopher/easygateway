package handlers

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/provider"
	"github.com/nightgopher/easygateway/internal/requestmeta"
)

func routingErrorStatus(err error) int {
	if err == nil {
		return http.StatusServiceUnavailable
	}
	var unsupported *provider.UnsupportedCapabilityError
	if errors.As(err, &unsupported) {
		return http.StatusBadRequest
	}
	var unknown *provider.UnknownModelError
	if errors.As(err, &unknown) {
		return http.StatusNotFound
	}
	var providerErr *provider.ProviderError
	if errors.As(err, &providerErr) {
		return providerErr.HTTPStatus()
	}
	return http.StatusServiceUnavailable
}

// setProviderErrorHeaders preserves retry and correlation metadata without
// exposing the complete upstream response body to the client or logs.
func setProviderErrorHeaders(c *gin.Context, err error) {
	if err != nil {
		c.Set(requestmeta.RequestErrorKey, summarizeRequestError(err))
	}
	var providerErr *provider.ProviderError
	if !errors.As(err, &providerErr) {
		return
	}
	if providerErr.RetryAfter != "" {
		c.Header("Retry-After", providerErr.RetryAfter)
	}
	if providerErr.RequestID != "" {
		c.Header("X-Upstream-Request-ID", providerErr.RequestID)
	}
}
