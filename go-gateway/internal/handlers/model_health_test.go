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

func TestModelCatalogHealthAndAvailableOnlyFilter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := log.New(io.Discard, "", 0)
	r := provider.NewRouter(logger)
	r.RegisterProvider("restricted", provider.NewOpenCodeProvider(&provider.Config{Name: "jev-1.13-free", APIKey: "public"}))
	r.RegisterChain("restricted", []string{"restricted"})
	r.RegisterProvider("ready", provider.NewOpenAIProvider(&provider.Config{Name: "ready", APIKey: "test"}))
	r.RegisterChain("ready", []string{"ready"})
	e := gin.New()
	e.GET("/v1/models", NewModelHandler(r, logger).Handle)
	for _, tt := range []struct {
		query string
		want  int
	}{{"", 2}, {"?available_only=true", 1}} {
		w := httptest.NewRecorder()
		e.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/models"+tt.query, nil))
		var got struct {
			Data []struct {
				ID     string               `json:"id"`
				Health provider.ModelHealth `json:"gateway_status"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Data) != tt.want {
			t.Fatalf("catalog %s: %s", tt.query, w.Body.String())
		}
		for _, m := range got.Data {
			if m.Health.CheckedAt.IsZero() || m.Health.Status == "" {
				t.Fatal("health annotation missing")
			}
			if tt.query != "" && !m.Health.Available {
				t.Fatal("unavailable model returned")
			}
		}
	}
}
