package assistant

import (
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nightgopher/easygateway/internal/memory"
	"github.com/nightgopher/easygateway/internal/storage"
)

func newTestMemStore(t *testing.T) memory.Store {
	t.Helper()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "metrics.db"), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestMemoryProposeToolCreatesCandidateNotActive(t *testing.T) {
	store := newTestMemStore(t)
	tool := newMemoryProposeTool(store)

	result, err := tool.Execute(t.Context(), []byte(`{
		"statement": "部署走 tag 驱动流水线",
		"scope_type": "project",
		"scope_key": "github:nightgopher/easygateway"
	}`), nil)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if result.IsError {
		t.Fatalf("propose 不应出错: %s", result.Content)
	}
	if !strings.Contains(result.Content, "candidate") {
		t.Fatalf("结果应说明候选状态: %s", result.Content)
	}

	// candidate 不进检索——助理查不到自己未确认的提案
	lookup := newMemoryLookupTool(store)
	res, err := lookup.Execute(t.Context(), []byte(`{"project":"github:nightgopher/easygateway"}`), nil)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if !strings.Contains(res.Content, "暂无生效记忆") {
		t.Fatalf("未确认提案不应出现在检索: %s", res.Content)
	}
}

func TestMemoryProposeToolRejectsSecret(t *testing.T) {
	store := newTestMemStore(t)
	tool := newMemoryProposeTool(store)
	result, err := tool.Execute(t.Context(), []byte(`{
		"statement": "key 是 sk-abcdefgh12345678",
		"scope_type": "global"
	}`), nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !result.IsError {
		t.Fatal("疑似密钥提案应被拒绝")
	}
}
