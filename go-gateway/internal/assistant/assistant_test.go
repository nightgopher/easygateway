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

func TestMemoryListPendingTool(t *testing.T) {
	store := newTestMemStore(t)
	propose := newMemoryProposeTool(store)
	if _, err := propose.Execute(t.Context(), []byte(`{
		"statement": "回复使用中文",
		"scope_type": "global"
	}`), nil); err != nil {
		t.Fatalf("propose: %v", err)
	}

	list := newMemoryListPendingTool(store)
	result, err := list.Execute(t.Context(), []byte(`{}`), nil)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if !strings.Contains(result.Content, "回复使用中文") {
		t.Fatalf("候选清单应包含提案: %s", result.Content)
	}
}

func TestUpdateSystemPromptHotSwap(t *testing.T) {
	a, err := New(Config{
		Enabled: true, Model: "mock",
		BaseURL: "http://127.0.0.1:1", APIKey: "test",
	}, newTestMemStore(t), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.SystemPrompt() != DefaultSystemPrompt {
		t.Fatal("初始应为内置默认 prompt")
	}
	a.UpdateSystemPrompt("只讨论 Go 后端。")
	if a.SystemPrompt() != "只讨论 Go 后端。" {
		t.Fatalf("热更新失败: %q", a.SystemPrompt())
	}
	// 空串回默认。
	a.UpdateSystemPrompt("")
	if a.SystemPrompt() != DefaultSystemPrompt {
		t.Fatal("空串应恢复默认 prompt")
	}
}

func TestDefaultSystemPromptEncodesStateNotKnowledge(t *testing.T) {
	// 用户设定的核心纪律必须写进默认人设：记状态不记知识。
	for _, want := range []string{"用户本人状态", "通用知识", "不提案"} {
		if !strings.Contains(DefaultSystemPrompt, want) {
			t.Fatalf("默认 prompt 缺少关键纪律 %q", want)
		}
	}
}
