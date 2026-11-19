package handlers

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

// DefaultMaxRequestBodyBytes caps the three protocol entry points
// (/v1/chat/completions, /v1/messages, /v1/responses). It matches the
// long-standing passthrough limit so a multipart upload and a base64 image sent
// as JSON hit the same ceiling. The value is generous on purpose: a single
// vision request carrying several inline images is already tens of megabytes,
// while anything past this point is a runaway client rather than a real prompt.
const DefaultMaxRequestBodyBytes = 64 << 20

// readRequestBody reads the entire request body under a hard size limit and
// rewinds it for the protocol decoder. It writes the error response itself and
// reports false, so handlers only need `if body, ok := ...; !ok { return }`.
//
// The limits matter because the opposite — an unbounded io.ReadAll on a request
// body — lets one client stream gigabytes into the process, and http.Server has
// no ReadTimeout here (streaming responses need long-lived connections), so
// nothing else stops it.
func readRequestBody(c *gin.Context, limit int64) ([]byte, bool) {
	if limit <= 0 {
		limit = DefaultMaxRequestBodyBytes
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
	rawBody, err := io.ReadAll(c.Request.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{
				"error": gin.H{
					"message": "request body exceeds " + byteSizeLabel(limit),
					"type":    "invalid_request_error",
					"code":    "request_too_large",
				},
			})
			return nil, false
		}
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error": gin.H{
				"message": "could not read request body",
				"type":    "invalid_request_error",
			},
		})
		return nil, false
	}
	// Restore the body: the protocol decoders below consume it a second time,
	// and bytes.Reader keeps the original slice instead of copying it.
	c.Request.Body = io.NopCloser(bytes.NewReader(rawBody))
	return rawBody, true
}

// byteSizeLabel renders a limit for the client-facing 413 message.
func byteSizeLabel(limit int64) string {
	const mib = 1 << 20
	if limit%mib == 0 {
		return strconv.FormatInt(limit/mib, 10) + " MiB"
	}
	return strconv.FormatInt(limit, 10) + " bytes"
}
