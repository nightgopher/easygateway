package storage

import (
	"io"
	"log"
	"path/filepath"
	"testing"

	"github.com/weijian/go-llm-gateway/internal/memory"
)

func newTestMemoryStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "metrics.db"), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func activeProjectStatement(statement string) memory.Memory {
	return memory.Memory{
		ScopeType: memory.ScopeProject,
		ScopeKey:  "github:nightgopher/litellm-gateway",
		Statement: statement,
		Status:    memory.StatusActive,
		Source:    "manual",
	}
}

func TestMemoryInsertDeduplicatesByScopeAndStatement(t *testing.T) {
	store := newTestMemoryStore(t)
	first, inserted, err := store.Insert(activeProjectStatement("部署走 tag 驱动流水线"))
	if err != nil || !inserted {
		t.Fatalf("首次插入应成功: inserted=%v err=%v", inserted, err)
	}
	second, inserted, err := store.Insert(activeProjectStatement("部署走 tag 驱动流水线"))
	if err != nil {
		t.Fatalf("重复插入不应报错: %v", err)
	}
	if inserted {
		t.Fatal("同作用域同命题应判定为重复")
	}
	if second.ID != first.ID {
		t.Fatalf("重复命题应刷新既有行 %d，而不是新建 %d", first.ID, second.ID)
	}
}

func TestMemoryLookupMergesScopesProjectFirst(t *testing.T) {
	store := newTestMemoryStore(t)
	seed := []memory.Memory{
		{ScopeType: memory.ScopeGlobal, Statement: "回复使用中文", Status: memory.StatusActive, Source: "manual"},
		{ScopeType: memory.ScopeClient, ScopeKey: "claude-code", Statement: "claude-code 偏好简洁输出", Status: memory.StatusActive, Source: "manual"},
		activeProjectStatement("部署走 tag 驱动流水线"),
		// candidate 不参与检索
		{ScopeType: memory.ScopeProject, ScopeKey: "github:nightgopher/litellm-gateway", Statement: "候选条目", Status: memory.StatusCandidate, Source: "agent:claude-code"},
		// retired 不参与检索
		{ScopeType: memory.ScopeProject, ScopeKey: "github:nightgopher/litellm-gateway", Statement: "退役条目", Status: memory.StatusRetired, Source: "manual"},
		// 其他 project 的条目不属于当前作用域
		{ScopeType: memory.ScopeProject, ScopeKey: "github:hwj123hwj/other", Statement: "别的项目条目", Status: memory.StatusActive, Source: "manual"},
	}
	for _, m := range seed {
		if _, _, err := store.Insert(m); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	got, err := store.LookupFor("claude-code", "github:nightgopher/litellm-gateway", 20)
	if err != nil {
		t.Fatalf("LookupFor: %v", err)
	}
	var statements []string
	for _, m := range got {
		statements = append(statements, m.Statement)
	}
	want := []string{
		"部署走 tag 驱动流水线", // project 最相关，排最前
		"claude-code 偏好简洁输出",
		"回复使用中文",
	}
	if len(statements) != len(want) {
		t.Fatalf("应返回 3 条，得到 %d: %v", len(statements), statements)
	}
	for i, statement := range want {
		if statements[i] != statement {
			t.Fatalf("顺序错误: 得到 %v，期望 %v", statements, want)
		}
	}
}

func TestMemoryLookupMarksHits(t *testing.T) {
	store := newTestMemoryStore(t)
	entry, _, err := store.Insert(activeProjectStatement("部署走 tag 驱动流水线"))
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := store.LookupFor("", entry.ScopeKey, 20); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	after, err := store.Get(entry.ID)
	if err != nil || after == nil {
		t.Fatalf("get after lookup: %v", err)
	}
	if after.HitCount != 1 || after.LastHitAt == nil {
		t.Fatalf("命中计数应 +1 且记录时间: hit=%d lastHit=%v", after.HitCount, after.LastHitAt)
	}
}

func TestMemoryGovernanceTransitions(t *testing.T) {
	store := newTestMemoryStore(t)
	entry, _, err := store.Insert(memory.Memory{
		ScopeType: memory.ScopeClient,
		ScopeKey:  "hwjcode",
		Statement: "HwjCode 走内网网关",
		Source:    "agent:hwjcode",
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if entry.Status != memory.StatusCandidate {
		t.Fatalf("agent 起草默认应为 candidate，得到 %s", entry.Status)
	}

	// candidate 不进检索
	got, err := store.LookupFor("hwjcode", "", 20)
	if err != nil || len(got) != 0 {
		t.Fatalf("candidate 不应被检索到: %v %v", got, err)
	}

	if updated, err := store.SetStatus(entry.ID, memory.StatusActive); err != nil || !updated {
		t.Fatalf("confirm: %v %v", updated, err)
	}
	got, err = store.LookupFor("hwjcode", "", 20)
	if err != nil || len(got) != 1 {
		t.Fatalf("active 应被检索到: %v %v", got, err)
	}

	if updated, err := store.SetStatus(entry.ID, memory.StatusRetired); err != nil || !updated {
		t.Fatalf("retire: %v %v", updated, err)
	}
	got, _ = store.LookupFor("hwjcode", "", 20)
	if len(got) != 0 {
		t.Fatalf("retired 不应被检索到: %v", got)
	}

	if deleted, err := store.Delete(entry.ID); err != nil || !deleted {
		t.Fatalf("delete: %v %v", deleted, err)
	}
	if deleted, err := store.Delete(entry.ID); err != nil || deleted {
		t.Fatalf("重复删除应返回 false: %v %v", deleted, err)
	}
}

func TestMemoryListFiltering(t *testing.T) {
	store := newTestMemoryStore(t)
	seed := []memory.Memory{
		{ScopeType: memory.ScopeGlobal, Statement: "A", Status: memory.StatusActive, Source: "manual"},
		{ScopeType: memory.ScopeClient, ScopeKey: "hwjcode", Statement: "B", Status: memory.StatusCandidate, Source: "agent:hwjcode"},
	}
	for _, m := range seed {
		if _, _, err := store.Insert(m); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	memories, total, err := store.List(memory.ListFilter{Status: memory.StatusCandidate, Limit: 50})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || len(memories) != 1 || memories[0].Statement != "B" {
		t.Fatalf("按 status 过滤失败: total=%d memories=%v", total, memories)
	}
}
