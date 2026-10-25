package handlers

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/gwkeys"
)

func newGatewayKeysEngine(t *testing.T) (*gin.Engine, *gwkeys.Store) {
	gin.SetMode(gin.TestMode)
	store, err := gwkeys.Load(t.TempDir() + "/keys.json")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	handler := NewGatewayKeyHandler(store, log.New(os.Stderr, "", 0))
	e := gin.New()
	e.GET("/admin/gateway-keys", handler.HandleList)
	e.POST("/admin/gateway-keys", handler.HandleCreate)
	e.POST("/admin/gateway-keys/:id/rotate", handler.HandleRotate)
	e.PATCH("/admin/gateway-keys/:id", handler.HandleSetEnabled)
	e.DELETE("/admin/gateway-keys/:id", handler.HandleRemove)
	return e, store
}

func TestGatewayKeyAdminLifecycle(t *testing.T) {
	e, store := newGatewayKeysEngine(t)

	// 创建：完整 token 只出现一次。
	create := httptest.NewRecorder()
	e.ServeHTTP(create, body(t, http.MethodPost, "/admin/gateway-keys", map[string]any{
		"name": "phone", "allowed_models": []string{"glm-*"}, "limits": map[string]any{"day": 500000},
	}))
	if create.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", create.Code, create.Body.String())
	}
	var created gwkeys.Key
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.Token == "" {
		t.Fatal("创建响应应带完整 token")
	}

	// 列表：token 只给 hint，限额与用量可见。
	list := httptest.NewRecorder()
	e.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/admin/gateway-keys", nil))
	var listed struct {
		Keys []gwkeys.Summary `json:"keys"`
	}
	_ = json.Unmarshal(list.Body.Bytes(), &listed)
	if len(listed.Keys) != 1 || listed.Keys[0].TokenHint == created.Token || listed.Keys[0].TokenHint == "" {
		t.Fatalf("列表不应暴露完整 token: %+v", listed.Keys)
	}
	if listed.Keys[0].Limits.Day != 500000 {
		t.Fatalf("限额应回显: %+v", listed.Keys[0])
	}

	// 停用后 Lookup 拒绝。
	if _, ok, _ := store.Lookup(created.Token); !ok {
		t.Fatal("新建密钥应立即可用")
	}
	patch := httptest.NewRecorder()
	e.ServeHTTP(patch, body(t, http.MethodPatch, "/admin/gateway-keys/"+created.ID, map[string]any{"enabled": false}))
	if patch.Code != http.StatusOK {
		t.Fatalf("patch = %d: %s", patch.Code, patch.Body.String())
	}
	if _, ok, reason := store.Lookup(created.Token); ok || reason != "disabled" {
		t.Fatalf("停用应生效: ok=%v reason=%s", ok, reason)
	}

	// 轮换：旧 token 失效、新 token 生效。
	rotate := httptest.NewRecorder()
	e.ServeHTTP(rotate, httptest.NewRequest(http.MethodPost, "/admin/gateway-keys/"+created.ID+"/rotate", nil))
	var rotated gwkeys.Key
	_ = json.Unmarshal(rotate.Body.Bytes(), &rotated)
	if rotate.Code != http.StatusOK || rotated.Token == "" || rotated.Token == created.Token {
		t.Fatalf("rotate = %d, body=%s", rotate.Code, rotate.Body.String())
	}
	if _, ok, _ := store.Lookup(created.Token); ok {
		t.Fatal("旧 token 轮换后应失效")
	}

	// 删除。
	del := httptest.NewRecorder()
	e.ServeHTTP(del, httptest.NewRequest(http.MethodDelete, "/admin/gateway-keys/"+created.ID, nil))
	if del.Code != http.StatusOK {
		t.Fatalf("delete = %d", del.Code)
	}
	if keys := store.List(); len(keys) != 0 {
		t.Fatalf("删除后应为空: %+v", keys)
	}
}

func TestGatewayKeyAdminValidation(t *testing.T) {
	e, _ := newGatewayKeysEngine(t)
	w := httptest.NewRecorder()
	e.ServeHTTP(w, body(t, http.MethodPost, "/admin/gateway-keys", map[string]any{"limits": map[string]any{"day": 1}}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("缺 name 应 400, got %d", w.Code)
	}
	w = httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/admin/gateway-keys/missing", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("删除不存在的密钥应 404, got %d", w.Code)
	}
}

func TestGatewayKeyAdminDisabledStore(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewGatewayKeyHandler(nil, log.New(os.Stderr, "", 0))
	e := gin.New()
	e.GET("/admin/gateway-keys", handler.HandleList)
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/gateway-keys", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("密钥库禁用应 503, got %d", w.Code)
	}
}

func body(t *testing.T, method, target string, payload any) *http.Request {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(method, target, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	return req
}
