package middleware

import (
	"log"
	"net/http"
	"runtime/debug"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/requestmeta"
)

// Recovery contains a panic raised by any handler.
//
// gin.New() does not install a recovery handler, and gin.Default()'s version
// only knows how to write a 500. This variant keeps the correlation ID in the
// log line, avoids writing a second body when the handler already flushed a
// streaming response, and never lets a panic reach the runtime — which would
// terminate the process and drop every connection the gateway is serving.
func Recovery(logger *log.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}
			requestID := c.GetString(requestmeta.RequestIDKey)
			logger.Printf("panic recovered: request_id=%s %s %s: %v\n%s",
				requestID, c.Request.Method, c.Request.URL.Path, recovered, debug.Stack())

			// A stream may already have delivered SSE bytes and a 200. Writing
			// another status line there would corrupt the response, so only
			// record the failure and let the client observe the broken stream.
			if c.Writer.Written() {
				c.Abort()
				return
			}

			c.Set(requestmeta.RequestErrorKey, "internal server error")
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
				"error": gin.H{
					"message":    "internal server error",
					"type":       "server_error",
					"request_id": requestID,
				},
			})
		}()
		c.Next()
	}
}
