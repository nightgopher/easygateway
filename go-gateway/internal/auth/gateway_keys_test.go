package auth

import (
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/gwkeys"
)

func newKeysEngine(t *testing.T, store *gwkeys.Store) *gin.Engine {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	logger := log.New(os.Stderr, "", 0)
	e.Use(BearerAuthWithGatewayKeys("master-key", "admin-key", store, logger))
	e.GET("/v1/ping", func(c *gin.Context) { c.String(http.StatusOK, "pong") })
	return e
}

func TestGatewayKeyAuthenticates(t *testing.T) {
	store, _ := gwkeys.Load(t.TempDir() + "/keys.json")
	key, _ := store.Create(gwkeys.CreateInput{Name: "phone"}, time.Now())
	e := newKeysEngine(t, store)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/ping", nil)
	req.Header.Set("Authorization", "Bearer "+key.Token)
	e.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("gateway key 应通过认证, got %d: %s", w.Code, w.Body.String())
	}

	// 错误 key 仍然 401。
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/ping", nil)
	req.Header.Set("Authorization", "Bearer sk-gw-wrong")
	e.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("错误 token 应 401, got %d", w.Code)
	}
}

func TestGatewayKeyDisabledRejected(t *testing.T) {
	store, _ := gwkeys.Load(t.TempDir() + "/keys.json")
	key, _ := store.Create(gwkeys.CreateInput{Name: "phone"}, time.Now())
	store.SetEnabled(key.ID, false)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/ping", nil)
	req.Header.Set("Authorization", "Bearer "+key.Token)
	newKeysEngine(t, store).ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("停用密钥应 401, got %d", w.Code)
	}
}

func TestGatewayKeyBudgetExhaustedReturns429(t *testing.T) {
	store, _ := gwkeys.Load(t.TempDir() + "/keys.json")
	key, _ := store.Create(gwkeys.CreateInput{Name: "phone", Limits: gwkeys.Limits{Day: 100}}, time.Now())
	store.RecordUsage(key.ID, 100, time.Now())

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/ping", nil)
	req.Header.Set("Authorization", "Bearer "+key.Token)
	newKeysEngine(t, store).ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("预算耗尽应 429, got %d: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("429 应带 Retry-After")
	}
	var body struct {
		Window string `json:"window"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.Window != "day" {
		t.Fatalf("window = %s, want day", body.Window)
	}
}

// gateway key 不能进 /admin：AdminAuth 只认 master/admin token。
func TestGatewayKeyCannotReachAdmin(t *testing.T) {
	store, _ := gwkeys.Load(t.TempDir() + "/keys.json")
	key, _ := store.Create(gwkeys.CreateInput{Name: "phone"}, time.Now())
	gin.SetMode(gin.TestMode)
	logger := log.New(os.Stderr, "", 0)
	e := gin.New()
	e.Use(BearerAuthWithGatewayKeys("master-key", "", store, logger))
	e.Use(func(c *gin.Context) {
		if isAdminPath(c.Request.URL.Path) {
			AdminAuth("master-key", "", logger)(c)
			return
		}
		c.Next()
	})
	e.GET("/admin/anything", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/anything", nil)
	req.Header.Set("Authorization", "Bearer "+key.Token)
	e.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("gateway key 不应进入 /admin, got %d", w.Code)
	}
}
