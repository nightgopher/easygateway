package channel

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// registryStub 是注册表测试用的最小渠道实现。
type registryStub struct{ name string }

func (s registryStub) Name() string        { return s.name }
func (s registryStub) Accounts() *Accounts { return nil }
func (s registryStub) BeginLogin(context.Context) (LoginSession, error) {
	return nil, errors.New("not implemented")
}

// TestRegistryOrderAndGet 验证登记顺序稳定、同名覆盖不重复、未注册返回 false。
func TestRegistryOrderAndGet(t *testing.T) {
	reg := NewRegistry()
	a := registryStub{name: "alpha"}
	b := registryStub{name: "beta"}

	reg.Register(a)
	reg.Register(b)
	// 同名再登记应覆盖实例但不改变顺序。
	reg.Register(registryStub{name: "alpha"})

	if reg.Len() != 2 {
		t.Fatalf("渠道数应为 2，得到 %d", reg.Len())
	}
	names := reg.Names()
	if len(names) != 2 || names[0] != "alpha" || names[1] != "beta" {
		t.Fatalf("登记顺序错误: %v", names)
	}
	if _, ok := reg.Get("alpha"); !ok {
		t.Fatalf("应能取到 alpha")
	}
	if _, ok := reg.Get("nope"); ok {
		t.Fatalf("未注册渠道不应命中")
	}
}

// TestStoreRoundTripPermissions 验证落盘权限、原子写、以及未登录时 Load 不报错。
func TestStoreRoundTripPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "channels")
	store := NewStore(dir)

	// 未登录是正常状态：Load 返回空切片而非错误。
	creds, err := store.Load("x")
	if err != nil || len(creds) != 0 {
		t.Fatalf("空存储应返回空切片与 nil 错误，得到 %v / %v", creds, err)
	}

	if err := store.Save("x", []Credential{{AccessToken: "t", RefreshToken: "r"}}); err != nil {
		t.Fatalf("save failed: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, "x.json"))
	if err != nil {
		t.Fatalf("stat creds: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("凭据文件权限应为 0600，得到 %o", perm)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0700 {
		t.Fatalf("凭据目录权限应为 0700，得到 %o", perm)
	}

	// 原子写不应留下临时文件。
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("残留临时文件: %s", e.Name())
		}
	}

	if !store.Has("x") {
		t.Fatalf("Has 应为 true")
	}
}

func TestStoreUpsertAndRemove(t *testing.T) {
	store := NewStore(t.TempDir())

	// 同一 AccountID 应替换而非追加。
	_ = store.UpsertAccount("c", Credential{AccountID: "a1", AccessToken: "t1"})
	_ = store.UpsertAccount("c", Credential{AccountID: "a1", AccessToken: "t1-new"})
	_ = store.UpsertAccount("c", Credential{AccountID: "a2", AccessToken: "t2"})

	creds, _ := store.Load("c")
	if len(creds) != 2 {
		t.Fatalf("应有 2 个账号，得到 %d", len(creds))
	}
	if creds[0].AccessToken != "t1-new" {
		t.Fatalf("同账号应就地替换: %+v", creds[0])
	}

	if err := store.RemoveAccount("c", 0); err != nil {
		t.Fatalf("remove failed: %v", err)
	}
	creds, _ = store.Load("c")
	if len(creds) != 1 || creds[0].AccountID != "a2" {
		t.Fatalf("删除后应剩 a2: %+v", creds)
	}

	// 删到空应清除文件并回到干净的未登录状态。
	if err := store.RemoveAccount("c", 0); err != nil {
		t.Fatalf("remove last failed: %v", err)
	}
	if store.Has("c") {
		t.Fatalf("账号清空后不应再有凭据文件")
	}
	if _, err := os.Stat(filepath.Join(store.dir, "c.json")); !os.IsNotExist(err) {
		t.Fatalf("凭据文件应已删除，得到 err=%v", err)
	}

	// 越界删除应报错。
	if err := store.RemoveAccount("c", 5); err == nil {
		t.Fatalf("越界删除应报错")
	}
}

func TestStoreClear(t *testing.T) {
	store := NewStore(t.TempDir())
	_ = store.Save("c", []Credential{{AccessToken: "t"}})
	if err := store.Clear("c"); err != nil {
		t.Fatalf("clear failed: %v", err)
	}
	if store.Has("c") {
		t.Fatalf("clear 后不应有凭据")
	}
	// 重复 clear 不存在的文件应视为成功（幂等）。
	if err := store.Clear("c"); err != nil {
		t.Fatalf("clear 不存在的渠道应成功，得到 %v", err)
	}
}

// TestAccountsRefreshLeadTime 验证只在临近过期时才续期：避免每次请求都轮换
// refresh_token（上游会因此失效）。
func TestAccountsRefreshLeadTime(t *testing.T) {
	calls := 0
	refresh := func(context.Context, Credential) (Credential, error) {
		calls++
		return Credential{AccessToken: "refreshed", RefreshToken: "r", ExpiresAt: time.Now().Add(10 * time.Hour)}, nil
	}
	store := NewStore(t.TempDir())

	// 距过期还有 10 小时（> 1h lead），不应触发续期。
	fresh := Credential{AccessToken: "fresh", RefreshToken: "r", ExpiresAt: time.Now().Add(10 * time.Hour)}
	writeCreds(t, store, "c", fresh)
	accounts := NewAccounts("c", store, newOAuthClient(), refresh)
	accounts.ensureFresh(context.Background())
	if calls != 0 {
		t.Fatalf("未临近过期不应续期，实际调用 %d 次", calls)
	}

	// 距过期只剩 10 分钟（< 1h lead），应触发续期。
	soon := Credential{AccessToken: "soon", RefreshToken: "r", ExpiresAt: time.Now().Add(10 * time.Minute)}
	writeCreds(t, store, "c2", soon)
	accounts2 := NewAccounts("c2", store, newOAuthClient(), refresh)
	accounts2.ensureFresh(context.Background())
	if calls != 1 {
		t.Fatalf("临近过期应续期一次，实际调用 %d 次", calls)
	}
	if got := accounts2.BearerToken(); got != "refreshed" {
		t.Fatalf("续期后令牌应更新，得到 %s", got)
	}
}

// TestAccountsRefreshFailureRotates 验证当前账号续期失败时自动换下一个账号。
func TestAccountsRefreshFailureRotates(t *testing.T) {
	refresh := func(context.Context, Credential) (Credential, error) {
		return Credential{}, errors.New("refresh_token 失效")
	}
	store := NewStore(t.TempDir())
	writeCreds(t, store, "c",
		Credential{AccountID: "a1", AccessToken: "t1", RefreshToken: "r1"},
		Credential{AccountID: "a2", AccessToken: "t2", RefreshToken: "r2"},
	)
	accounts := NewAccounts("c", store, newOAuthClient(), refresh)

	// 强制把当前账号设为临近过期，使 Refresh 走续期分支。
	accounts.mu.Lock()
	accounts.creds[0].ExpiresAt = time.Now().Add(-time.Hour)
	accounts.mu.Unlock()

	if !accounts.Refresh(context.Background()) {
		t.Fatalf("续期失败但有备用账号时应换号并返回 true")
	}
	if got := accounts.BearerToken(); got != "t2" {
		t.Fatalf("应已切到第二个账号，得到 %s", got)
	}
}

// TestAccountsCredentialsSummaryHidesTokens 验证摘要不含 token 明文。
func TestAccountsCredentialsSummaryHidesTokens(t *testing.T) {
	store := NewStore(t.TempDir())
	writeCreds(t, store, "c", Credential{
		AccessToken:  "super-secret-token",
		RefreshToken: "super-secret-refresh",
		AccountID:    "acct",
		Nickname:     "nick",
		ExpiresAt:    time.Now().Add(time.Hour),
	})
	accounts := NewAccounts("c", store, newOAuthClient(), func(context.Context, Credential) (Credential, error) { return Credential{}, nil })

	summaries := accounts.Credentials()
	if len(summaries) != 1 {
		t.Fatalf("应有 1 条摘要，得到 %d", len(summaries))
	}
	summary := summaries[0]
	if summary.AccountID != "acct" || summary.Nickname != "nick" || summary.Expired {
		t.Fatalf("摘要内容错误: %+v", summary)
	}
	if !summary.RefreshAvailable {
		t.Fatal("refresh capability missing from safe summary")
	}
	// 摘要结构体里根本没有 token 字段——这里再确认序列化后不含明文。
	encoded := summary.ExpiresAt
	if encoded == "" {
		t.Fatalf("应带过期时间")
	}
	raw, err := json.Marshal(summaries)
	if err != nil || strings.Contains(string(raw), "super-secret") {
		t.Fatal("account summary exposed credentials")
	}
	accounts.refresh = nil
	if accounts.Credentials()[0].RefreshAvailable {
		t.Fatal("renewal advertised without a refresh implementation")
	}
}

func TestAccountsRemoveAllAndLoggedIn(t *testing.T) {
	store := NewStore(t.TempDir())
	accounts := NewAccounts("c", store, newOAuthClient(), nil)

	if accounts.LoggedIn() {
		t.Fatalf("初始应为未登录")
	}
	writeCreds(t, store, "c", Credential{AccessToken: "t"})
	accounts.reload()
	if !accounts.LoggedIn() {
		t.Fatalf("写入凭据后应为已登录")
	}
	if err := accounts.RemoveAll(); err != nil {
		t.Fatalf("remove all failed: %v", err)
	}
	if accounts.LoggedIn() || accounts.Len() != 0 {
		t.Fatalf("登出后应回到未登录状态")
	}
}

func TestCredentialExpired(t *testing.T) {
	now := time.Now()
	if !(&Credential{ExpiresAt: now.Add(-time.Minute)}).Expired(now) {
		t.Fatalf("已过期凭据应判为过期")
	}
	if (&Credential{ExpiresAt: now.Add(time.Hour)}).Expired(now) {
		t.Fatalf("未过期凭据不应判为过期")
	}
	// 零值 ExpiresAt 表示上游未给出过期时间：不判过期，交由 401 触发刷新。
	if (&Credential{}).Expired(now) {
		t.Fatalf("零值过期时间不应判为过期")
	}
}
