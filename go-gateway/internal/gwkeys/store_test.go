package gwkeys

import (
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Load(t.TempDir() + "/gateway-keys.json")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return s
}

func TestCreateLookupPersistRoundTrip(t *testing.T) {
	path := t.TempDir() + "/gateway-keys.json"
	s, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	now := time.Now()
	key, err := s.Create(CreateInput{Name: "phone", AllowedModels: []string{"glm-*"}, Limits: Limits{Day: 1000}}, now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(key.Token) < len("sk-gw-")+16 {
		t.Fatalf("token too short: %s", key.Token)
	}

	// 重新 Load：密钥与用量计数跨重启保留。
	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got, ok, _ := reloaded.Lookup(key.Token); !ok || got.ID != key.ID {
		t.Fatalf("reload 后应能按 token 找到密钥")
	}
}

func TestLookupDisabledAndUnknown(t *testing.T) {
	s := newTestStore(t)
	key, _ := s.Create(CreateInput{Name: "k"}, time.Now())
	if _, ok, _ := s.Lookup("sk-gw-unknown"); ok {
		t.Fatal("未知 token 不应命中")
	}
	if err := s.SetEnabled(key.ID, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, ok, reason := s.Lookup(key.Token); ok || reason != "disabled" {
		t.Fatalf("停用密钥应拒绝: ok=%v reason=%s", ok, reason)
	}
}

func TestBudgetWindows(t *testing.T) {
	s := newTestStore(t)
	// 固定基准时间（2026-10-21 是周三）：+1 天仍在同一周，断言不依赖运行日。
	base := time.Date(2026, 10, 21, 10, 0, 0, 0, time.Local)
	key, _ := s.Create(CreateInput{Name: "k", Limits: Limits{Day: 100, Week: 1000}}, base)

	if state := s.Budget(key, base); !state.OK {
		t.Fatalf("初始预算应可用: %+v", state)
	}
	s.RecordUsage(key.ID, 100, base)
	// Budget 用的是调用方传入的 Key 快照，重新 Lookup 拿最新用量。
	used, _, _ := s.Lookup(key.Token)
	if state := s.Budget(used, base); state.OK || state.Window != "day" || state.Limit != 100 {
		t.Fatalf("日限额耗尽应拒绝: %+v", state)
	}
	if state := s.Budget(used, base); state.RetryAfter <= 0 {
		t.Fatalf("应给出窗口重置时长: %+v", state)
	}
	// 日窗口按本地自然日对齐（不是 Truncate 的绝对 24h 边界）。
	if used.Usage.Day.Start.Hour() != 0 || used.Usage.Day.Start.Minute() != 0 {
		t.Fatalf("日窗口应从本地午夜开始: %v", used.Usage.Day.Start)
	}

	// 跨天滚动：第二天窗口清零、预算恢复（周计数仍累计）。
	s.RecordUsage(key.ID, 1, base.AddDate(0, 0, 1))
	list := s.List()
	if list[0].Usage.Day.Tokens != 1 {
		t.Fatalf("跨天后日计数应清零重计: %+v", list[0].Usage)
	}
	if list[0].Usage.Week.Tokens != 101 {
		t.Fatalf("周计数应累计: %+v", list[0].Usage)
	}
}

func TestBudgetUnlimitedWhenNoLimits(t *testing.T) {
	s := newTestStore(t)
	key, _ := s.Create(CreateInput{Name: "k"}, time.Now())
	s.RecordUsage(key.ID, 1_000_000, time.Now())
	used, _, _ := s.Lookup(key.Token)
	if state := s.Budget(used, time.Now()); !state.OK {
		t.Fatalf("未配置限额应恒可用: %+v", state)
	}
}

func TestGuardModelAllowed(t *testing.T) {
	guard := &Guard{AllowedModels: []string{"glm-*", "coding"}}
	cases := map[string]bool{
		"glm-5.3":     true,
		"coding":      true,
		"deepseek-v4": false,
	}
	for model, want := range cases {
		if got := guard.ModelAllowed(model); got != want {
			t.Fatalf("ModelAllowed(%s) = %v, want %v", model, got, want)
		}
	}
	if (&Guard{}).ModelAllowed("anything") != true {
		t.Fatal("空白名单应放行全部模型")
	}
}

func TestRotateInvalidatesOldToken(t *testing.T) {
	s := newTestStore(t)
	key, _ := s.Create(CreateInput{Name: "k"}, time.Now())
	rotated, err := s.Rotate(key.ID, time.Now())
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rotated.Token == key.Token {
		t.Fatal("轮换必须换新 token")
	}
	if _, ok, _ := s.Lookup(key.Token); ok {
		t.Fatal("旧 token 轮换后必须立即失效")
	}
	if _, ok, _ := s.Lookup(rotated.Token); !ok {
		t.Fatal("新 token 应立即可用")
	}
}

func TestRemoveAndListMasking(t *testing.T) {
	s := newTestStore(t)
	key, _ := s.Create(CreateInput{Name: "k"}, time.Now())
	list := s.List()
	if list[0].TokenHint == key.Token || list[0].TokenHint == "" {
		t.Fatalf("列表不应暴露完整 token: %q", list[0].TokenHint)
	}
	if err := s.Remove(key.ID); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, ok, _ := s.Lookup(key.Token); ok {
		t.Fatal("删除后 token 应失效")
	}
	if err := s.Remove(key.ID); err == nil {
		t.Fatal("重复删除应报错")
	}
}
