package handlers

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/provider"
)

func TestModelHandlerExposesCapabilityMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := log.New(io.Discard, "", 0)
	router := provider.NewRouter(logger)
	router.RegisterModel(provider.ModelInfo{
		ID:              "glm-5.3-flash",
		Provider:        "glm-5.3-flash",
		Protocol:        "openai",
		Capabilities:    []string{provider.CapabilityText, provider.CapabilityVision, provider.CapabilityStreaming},
		InputModalities: []string{"text", "image"},
		MaxInputTokens:  65536,
		MaxOutputTokens: 8192,
	})

	engine := gin.New()
	engine.GET("/v1/models", NewModelHandler(router, logger).Handle)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()

	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var response struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Object != "list" || len(response.Data) != 1 || len(response.Models) != 1 {
		t.Fatalf("unexpected model list: %#v", response)
	}
	model := response.Data[0]
	if model["id"] != "glm-5.3-flash" || model["provider"] != "glm-5.3-flash" || model["protocol"] != "openai" {
		t.Fatalf("unexpected model identity: %#v", model)
	}
	capabilities, ok := model["capabilities"].([]any)
	if !ok || !containsString(capabilities, provider.CapabilityVision) {
		t.Fatalf("expected vision capability, got %#v", model["capabilities"])
	}
	modalities, ok := model["input_modalities"].([]any)
	if !ok || !containsString(modalities, "image") {
		t.Fatalf("expected image modality, got %#v", model["input_modalities"])
	}
	if model["max_input_tokens"] != float64(65536) || model["max_output_tokens"] != float64(8192) {
		t.Fatalf("unexpected token limits: %#v", model)
	}
}

func containsString(values []any, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
