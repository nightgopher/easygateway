package handlers

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/weijian/go-llm-gateway/internal/provider"
)

func TestConfigAdminCheckDoesNotApplyAndInvalidReloadKeepsRouter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "providers.yaml")
	body := "providers:\n - name: test\n   type: openai\n   url: https://example.invalid\n   allow_no_key: true\n   models: [{id: first}]\n"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	router := provider.NewRouter(nil)
	reloader := provider.NewConfigReloader(router, path, nil, nil)
	if _, err := reloader.Reload(); err != nil {
		t.Fatal(err)
	}
	h := NewConfigAdminHandler(reloader)
	e := gin.New()
	e.POST("/check", h.HandleCheck)
	e.POST("/reload", h.HandleReload)
	e.GET("/status", h.HandleStatus)
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(body, "first", "second")), 0600); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/check", nil))
	if w.Code != http.StatusOK || reloader.Status().Generation != 1 {
		t.Fatalf("check mutated state: %d %s", w.Code, w.Body.String())
	}
	if _, err := router.Route("second"); err == nil {
		t.Fatal("check applied new route")
	}
	if err := os.WriteFile(path, []byte("providers: ["), 0600); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/reload", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid reload: %d %s", w.Code, w.Body.String())
	}
	if _, err := router.Route("first"); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/status", nil))
	if !strings.Contains(w.Body.String(), `"last_error"`) || !strings.Contains(w.Body.String(), `"generation":1`) {
		t.Fatal(w.Body.String())
	}
}
