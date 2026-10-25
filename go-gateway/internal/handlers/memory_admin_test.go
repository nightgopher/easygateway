package handlers

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/nightgopher/easygateway/internal/memory"
)

// fakeMemoryStore implements memory.Store in-process for handler tests.
type fakeMemoryStore struct {
	entries []memory.Memory
	nextID  int64
}

func (s *fakeMemoryStore) Insert(m memory.Memory) (memory.Memory, bool, error) {
	for i := range s.entries {
		if s.entries[i].ScopeType == m.ScopeType &&
			s.entries[i].ScopeKey == m.ScopeKey &&
			s.entries[i].Statement == m.Statement {
			return s.entries[i], false, nil
		}
	}
	s.nextID++
	m.ID = s.nextID
	s.entries = append(s.entries, m)
	return m, true, nil
}

func (s *fakeMemoryStore) Get(id int64) (*memory.Memory, error) {
	for i := range s.entries {
		if s.entries[i].ID == id {
			return &s.entries[i], nil
		}
	}
	return nil, nil
}

func (s *fakeMemoryStore) List(filter memory.ListFilter) ([]memory.Memory, int, error) {
	var result []memory.Memory
	for _, m := range s.entries {
		if filter.Status != "" && m.Status != filter.Status {
			continue
		}
		result = append(result, m)
	}
	return result, len(result), nil
}

func (s *fakeMemoryStore) SetStatus(id int64, status memory.Status) (bool, error) {
	for i := range s.entries {
		if s.entries[i].ID == id {
			s.entries[i].Status = status
			return true, nil
		}
	}
	return false, nil
}

func (s *fakeMemoryStore) Delete(id int64) (bool, error) {
	for i := range s.entries {
		if s.entries[i].ID == id {
			s.entries = append(s.entries[:i], s.entries[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

func (s *fakeMemoryStore) LookupFor(client, project string, limit int) ([]memory.Memory, error) {
	var result []memory.Memory
	for _, m := range s.entries {
		if m.Status != memory.StatusActive {
			continue
		}
		switch m.ScopeType {
		case memory.ScopeGlobal:
			result = append(result, m)
		case memory.ScopeClient:
			if m.ScopeKey == client {
				result = append(result, m)
			}
		case memory.ScopeProject:
			if m.ScopeKey == project {
				result = append(result, m)
			}
		}
		if len(result) >= limit {
			break
		}
	}
	return result, nil
}

func (s *fakeMemoryStore) MarkHits([]int64) error { return nil }

func memoryTestRouter(store memory.Store) *gin.Engine {
	gin.SetMode(gin.TestMode)
	adminHandler := NewMemoryAdminHandler(store, log.New(io.Discard, "", 0))
	clientHandler := NewMemoryHandler(store, log.New(io.Discard, "", 0))
	engine := gin.New()
	admin := engine.Group("/admin")
	{
		admin.GET("/memories", adminHandler.HandleList)
		admin.POST("/memories", adminHandler.HandleCreate)
		admin.POST("/memories/:id/confirm", adminHandler.HandleConfirm)
		admin.POST("/memories/:id/retire", adminHandler.HandleRetire)
		admin.DELETE("/memories/:id", adminHandler.HandleDelete)
	}
	engine.GET("/v1/memory", clientHandler.HandleLookup)
	engine.POST("/v1/memory", clientHandler.HandleDraft)
	return engine
}

func doJSON(t *testing.T, engine *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	engine.ServeHTTP(w, req)
	return w
}

func TestMemoryDraftGoesToCandidateAndConfirmActivates(t *testing.T) {
	store := &fakeMemoryStore{}
	engine := memoryTestRouter(store)

	// agent 起草 → candidate
	w := doJSON(t, engine, http.MethodPost, "/v1/memory", map[string]any{
		"statement":  "部署走 tag 驱动流水线",
		"scope_type": "project",
		"scope_key":  "github:nightgopher/easygateway",
		"client":     "hwjcode",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("起草失败: %d %s", w.Code, w.Body.String())
	}
	if len(store.entries) != 1 || store.entries[0].Status != memory.StatusCandidate {
		t.Fatalf("起草应落 candidate: %+v", store.entries)
	}

	// candidate 不出现在检索
	w = doJSON(t, engine, http.MethodGet, "/v1/memory?client=hwjcode&project=github:nightgopher/easygateway", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("lookup 失败: %d", w.Code)
	}
	var lookup struct {
		Memories []memory.Memory `json:"memories"`
		Block    string          `json:"block"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &lookup)
	if len(lookup.Memories) != 0 || lookup.Block != "" {
		t.Fatalf("candidate 不应被检索/渲染: %+v", lookup)
	}

	// 人拍板确认 → active，出现在检索且带注入块
	w = doJSON(t, engine, http.MethodPost, "/admin/memories/1/confirm", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("确认失败: %d %s", w.Code, w.Body.String())
	}
	w = doJSON(t, engine, http.MethodGet, "/v1/memory?client=hwjcode&project=github:nightgopher/easygateway", nil)
	_ = json.Unmarshal(w.Body.Bytes(), &lookup)
	if len(lookup.Memories) != 1 || lookup.Block == "" {
		t.Fatalf("确认后应可检索且带注入块: %+v", lookup)
	}
}

func TestMemoryDraftRejectsSecretLike(t *testing.T) {
	store := &fakeMemoryStore{}
	engine := memoryTestRouter(store)
	w := doJSON(t, engine, http.MethodPost, "/v1/memory", map[string]any{
		"statement":  "key 是 sk-abcdefgh12345678",
		"scope_type": "global",
	})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("疑似密钥应 422: %d %s", w.Code, w.Body.String())
	}
	if len(store.entries) != 0 {
		t.Fatal("疑似密钥不应入库")
	}
}

func TestMemoryAdminCreateDirectlyActiveAndRetire(t *testing.T) {
	store := &fakeMemoryStore{}
	engine := memoryTestRouter(store)

	w := doJSON(t, engine, http.MethodPost, "/admin/memories", map[string]any{
		"statement":  "回复使用中文",
		"scope_type": "global",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("admin 创建失败: %d %s", w.Code, w.Body.String())
	}
	if len(store.entries) != 1 || store.entries[0].Status != memory.StatusActive {
		t.Fatalf("admin 创建应直接 active: %+v", store.entries)
	}

	w = doJSON(t, engine, http.MethodPost, "/admin/memories/1/retire", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("退役失败: %d", w.Code)
	}

	w = doJSON(t, engine, http.MethodDelete, "/admin/memories/1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("删除失败: %d", w.Code)
	}
	if len(store.entries) != 0 {
		t.Fatalf("删除后应为空: %+v", store.entries)
	}
}

func TestMemoryNoopStoreKeepsEndpointsAlive(t *testing.T) {
	engine := memoryTestRouter(memory.NoopStore{})
	w := doJSON(t, engine, http.MethodGet, "/v1/memory?client=x", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("停用状态下 lookup 仍应 200: %d", w.Code)
	}
}
