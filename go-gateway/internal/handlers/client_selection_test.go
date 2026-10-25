package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/dshconfig"
	"github.com/nightgopher/easygateway/internal/provider"
)

func TestClientModelSelections(t *testing.T) {
	for _, target := range []string{"pi", "zcode", "harness"} {
		t.Run(target, func(t *testing.T) {
			home := t.TempDir()
			gateway := t.TempDir()
			router := provider.NewRouter(discardLogger())
			// model-a 必须可用（有已启用的 provider 链）才会进入同步清单。
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(upstream.Close)
			router.RegisterProvider("test-provider", provider.NewOpenAIProvider(&provider.Config{Name: "test-provider", URL: upstream.URL, APIKey: "k"}))
			router.RegisterChain("model-a", []string{"test-provider"})
			for _, id := range []string{"model-a", "model-b"} {
				router.RegisterModel(provider.ModelInfo{ID: id, Capabilities: []string{"text"}, Protocol: "responses"})
			}
			h := NewClientConfigHandler(gateway, home, home, router, discardLogger())
			pi := NewPiConfigHandler(gateway, home, discardLogger(), router)
			if target == "harness" {
				path := dshconfig.PatchPath(home)
				os.MkdirAll(filepath.Dir(path), 0700)
				os.WriteFile(path, []byte("- id: llm-deepseek\n  config:\n    models: []\n- id: llm-pi-ai\n  config:\n    providers:\n      anthropic:\n        models: []\n"), 0600)
			}
			engine := gin.New()
			switch target {
			case "pi":
				engine.POST("/sync", pi.HandleSync)
				engine.GET("/status", pi.HandleStatus)
			case "zcode":
				engine.POST("/sync", h.HandleZCodeSync)
				engine.GET("/status", h.HandleZCodeStatus)
			case "harness":
				engine.POST("/sync", h.HandleHarnessSync)
				engine.GET("/status", h.HandleHarnessStatus)
			}
			for _, body := range []string{`{"model_ids":["model-a","model-a"]}`, `{"model_ids":[]}`, `{"model_ids":["unknown"]}`, `{invalid`} {
				w := httptest.NewRecorder()
				engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/sync", strings.NewReader(body)))
				expected := 400
				if strings.Contains(body, "model-a") {
					expected = 200
				}
				if w.Code != expected {
					t.Fatalf("%s: status %d: %s", body, w.Code, w.Body)
				}
			}
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/status", nil))
			var status struct {
				Current []string `json:"current_ids"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
				t.Fatal(err)
			}
			if len(status.Current) != 1 || status.Current[0] != "model-a" {
				t.Fatalf("selection changed or unselected model written: %v", status.Current)
			}
		})
	}
}
