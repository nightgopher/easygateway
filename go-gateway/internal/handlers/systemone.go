package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/weijian/go-llm-gateway/internal/provider"
	"github.com/weijian/go-llm-gateway/internal/requestmeta"
)

type SystemOneHandler struct{ router *provider.Router }

func NewSystemOneHandler(router *provider.Router) *SystemOneHandler {
	return &SystemOneHandler{router: router}
}

// Only inspect protocol boundaries. State, instructions and criteria can also
// contain structured JSON; retain those and extension fields unchanged.
func validateSystemOneRequest(body []byte) (string, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return "", fmt.Errorf("request must be a JSON object")
	}
	var model string
	if json.Unmarshal(fields["model"], &model) != nil || strings.TrimSpace(model) == "" {
		return "", fmt.Errorf("model is required")
	}
	if v := strings.TrimSpace(string(fields["state"])); v == "" || v == "null" || v == `""` {
		return "", fmt.Errorf("state is required")
	}
	for _, name := range []string{"messages", "input", "max_tokens"} {
		if _, ok := fields[name]; ok {
			return "", fmt.Errorf("%s is a text-generation field; use state and questions for System One", name)
		}
	}
	if raw, ok := fields["stream"]; ok && string(raw) != "false" {
		return "", fmt.Errorf("System One returns a JSON answer; stream must be omitted or false")
	}
	var questions map[string]json.RawMessage
	if json.Unmarshal(fields["questions"], &questions) != nil || len(questions) == 0 {
		return "", fmt.Errorf("questions must be a nonempty object")
	}
	for id, raw := range questions {
		var question struct {
			Type         string          `json:"type"`
			Instructions json.RawMessage `json:"instructions"`
			Criteria     json.RawMessage `json:"criteria"`
		}
		if strings.TrimSpace(id) == "" || json.Unmarshal(raw, &question) != nil || question.Type == "" {
			return "", fmt.Errorf("each question needs an ID and a type")
		}
		if v := strings.TrimSpace(string(question.Instructions)); v == "" || v == "null" || v == `""` {
			return "", fmt.Errorf("question %q needs instructions", id)
		}
		switch question.Type {
		case "choice":
			var criteria map[string]json.RawMessage
			if json.Unmarshal(question.Criteria, &criteria) != nil || len(criteria) == 0 {
				return "", fmt.Errorf("choice question %q needs a nonempty criteria object", id)
			}
		case "score":
			var criteria []json.RawMessage
			if json.Unmarshal(question.Criteria, &criteria) != nil || len(criteria) == 0 {
				return "", fmt.Errorf("score question %q needs a nonempty criteria array", id)
			}
		}
	}
	return model, nil
}

func (h *SystemOneHandler) Handle(c *gin.Context) {
	invalid := func(status int, message string) {
		c.JSON(status, gin.H{"error": gin.H{"type": "invalid_request_error", "message": message}})
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4<<20)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			invalid(http.StatusRequestEntityTooLarge, "System One request exceeds 4 MiB")
		} else {
			invalid(http.StatusBadRequest, "could not read request")
		}
		return
	}
	model, err := validateSystemOneRequest(body)
	if err != nil {
		invalid(http.StatusBadRequest, err.Error())
		return
	}
	setRequestMetadata(c, model, false)
	providers, err := h.router.Route(model)
	if err != nil {
		setProviderErrorHeaders(c, err)
		c.JSON(routingErrorStatus(err), gin.H{"error": gin.H{"type": "routing_error", "message": err.Error()}})
		return
	}
	p := providers[0]
	cp, ok := p.(provider.CapabilityProvider)
	native, supportsNative := p.(provider.SystemOneForwarder)
	supported := false
	if ok {
		for _, capability := range cp.Capabilities() {
			supported = supported || capability == provider.CapabilitySystemOne
		}
	}
	if !supportsNative || !supported {
		invalid(http.StatusBadRequest, "selected model does not support the System One protocol")
		return
	}
	started := time.Now()
	var result *provider.SystemOneResponse
	err = h.router.RunNonStreamAttempt(c.Request.Context(), p.Name(), func(ctx context.Context) error {
		var callErr error
		result, callErr = native.ForwardSystemOne(ctx, body)
		return callErr
	})
	if err == nil && result.StatusCode >= 400 {
		err = &provider.ProviderError{Provider: p.Name(), StatusCode: result.StatusCode, Message: "System One upstream rejected the request", RetryAfter: result.Header.Get("Retry-After"), RequestID: result.Header.Get("X-Request-ID")}
	}
	attempt := requestmeta.ProviderAttempt{Provider: p.Name(), Status: "success", LatencyMs: float64(time.Since(started).Milliseconds())}
	if err != nil {
		attempt.Status = "error"
		attempt.Error = summarizeRequestError(err)
		h.router.RecordProviderFailureFor(model, p, err)
		setProviderErrorHeaders(c, err)
	} else {
		h.router.RecordProviderSuccessFor(model, p)
	}
	if result != nil {
		attempt.StatusCode = result.StatusCode
	}
	setForwardMetadata(c, p.Name(), []requestmeta.ProviderAttempt{attempt}, err)
	if c.Request.Context().Err() != nil {
		c.Status(499)
		return
	}
	if result == nil || (err != nil && result.StatusCode < 400) {
		status := routingErrorStatus(err)
		c.JSON(status, gin.H{"error": gin.H{"type": "upstream_error", "message": "System One request failed or timed out"}})
		return
	}
	if value := result.Header.Get("Retry-After"); value != "" {
		c.Header("Retry-After", value)
	}
	if value := result.Header.Get("X-Request-ID"); value != "" {
		c.Header("X-Upstream-Request-ID", value)
	}
	if result.StatusCode >= 200 && result.StatusCode < 300 {
		var data struct {
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(result.Body, &data) == nil {
			setUsageMetadata(c, data.Usage.InputTokens, data.Usage.OutputTokens)
		}
	}
	c.Data(result.StatusCode, "application/json", result.Body)
}
