package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/requestmeta"
)

const requestIDHeader = "X-Request-ID"

// RequestID assigns one bounded correlation ID to every HTTP request. A
// caller-provided ID is preserved when it is safe to echo; malformed or
// oversized values are replaced with a cryptographically random ID.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(requestIDHeader)
		if !validRequestID(id) {
			id = newRequestID()
		}
		c.Set(requestmeta.RequestIDKey, id)
		c.Header(requestIDHeader, id)
		c.Next()
	}
}

func validRequestID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' || r == ':' {
			continue
		}
		return false
	}
	return true
}

func newRequestID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err == nil {
		return hex.EncodeToString(raw[:])
	}
	// crypto/rand failure is exceptionally rare. Keep correlation available
	// even in a constrained runtime instead of failing the user request.
	return hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
}
