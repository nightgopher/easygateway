package handlers

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/piconfig"
)

func newPiTestServer(t *testing.T) (*gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	piHome := t.TempDir()
	handler := NewPiConfigHandler(t.TempDir(), piHome, discardLogger())
	engine := gin.New()
	engine.GET("/admin/pi", handler.HandleStatus)
	engine.POST("/admin/pi/sync", handler.HandleSync)
	return engine, piHome
}

func discardLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}

func TestPiStatusReportsMissingFile(t *testing.T) {
	engine, _ := newPiTestServer(t)

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/pi", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		FileExists bool     `json:"file_exists"`
		InSync     bool     `json:"in_sync"`
		MissingIDs []string `json:"missing_ids"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.FileExists || resp.InSync {
		t.Fatalf("expected missing file to be out of sync, got exists=%v in_sync=%v", resp.FileExists, resp.InSync)
	}
	if len(resp.MissingIDs) == 0 {
		t.Fatal("expected missing_ids to list the desired models")
	}
}

func TestPiSyncWritesConfigAndReportsInSync(t *testing.T) {
	engine, piHome := newPiTestServer(t)

	// 预置一份带其他 provider 的配置，验证同步只动 llm-gateway 这一块。
	modelsPath := filepath.Join(piHome, "agent", "models.json")
	if err := os.MkdirAll(filepath.Dir(modelsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	existing := `{"providers":{"other-gateway":{"baseUrl":"http://example:9"},"llm-gateway":{"baseUrl":"http://old:4001/v1","models":[{"id":"stale-model","name":"Stale"}]}}}`
	if err := os.WriteFile(modelsPath, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admin/pi/sync", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Synced  bool     `json:"synced"`
		InSync  bool     `json:"in_sync"`
		Current []string `json:"current_ids"`
		Stale   []string `json:"stale_ids"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Synced || !resp.InSync {
		t.Fatalf("expected synced in_sync=true, got %+v", resp)
	}
	if len(resp.Stale) != 0 {
		t.Fatalf("expected no stale ids after sync, got %v", resp.Stale)
	}
	for _, want := range []string{"coding", "gemini-3.8-flash-high"} {
		found := false
		for _, id := range resp.Current {
			if id == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected %s in current_ids, got %v", want, resp.Current)
		}
	}

	// 其他 provider 必须原样保留。
	raw, err := os.ReadFile(modelsPath)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Providers map[string]json.RawMessage `json:"providers"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	if _, ok := document.Providers["other-gateway"]; !ok {
		t.Fatal("sync must not touch unrelated providers")
	}

	// 轮转备份应存在且内容为同步前的版本。
	backup, err := os.ReadFile(modelsPath + ".pre-sync.bak")
	if err != nil {
		t.Fatalf("expected pre-sync backup: %v", err)
	}
	if string(backup) != existing {
		t.Fatal("backup should contain the pre-sync content")
	}

	// 再次 GET 状态应为已同步。
	w = httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/pi", nil))
	var status struct {
		InSync bool `json:"in_sync"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if !status.InSync {
		t.Fatal("expected in_sync=true after sync")
	}
}

func TestPiDesiredModelsMatchesProviderConfig(t *testing.T) {
	models := piconfig.DesiredModels()
	if len(models) == 0 {
		t.Fatal("DesiredModels must not be empty")
	}
	for _, m := range models {
		if m["id"] == nil || m["name"] == nil {
			t.Fatalf("model entries need id and name, got %v", m)
		}
	}
}
