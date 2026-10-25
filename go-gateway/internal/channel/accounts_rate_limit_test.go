package channel

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// newRateTestPool 构造一个带 N 个账号的测试账号池（token-1..N）。
func newRateTestPool(t *testing.T, n int) *Accounts {
	t.Helper()
	store := NewStore(t.TempDir())
	creds := make([]Credential, 0, n)
	for i := 1; i <= n; i++ {
		creds = append(creds, Credential{AccountID: string(rune('a' + i - 1)), AccessToken: tokenName(i)})
	}
	writeCreds(t, store, "ratetest", creds...)
	return NewAccounts("ratetest", store, newOAuthClient(), nil)
}

func tokenName(i int) string { return "token-" + string(rune('0'+i)) }

func TestOnRateLimitedRotatesToNextAccount(t *testing.T) {
	a := newRateTestPool(t, 3)
	if got := a.BearerToken(); got != "token-1" {
		t.Fatalf("初始账号 = %s, want token-1", got)
	}
	if !a.OnRateLimited(context.Background(), "token-1", "60") {
		t.Fatal("多账号渠道 429 后应能换号")
	}
	if got := a.BearerToken(); got != "token-2" {
		t.Fatalf("换号后 = %s, want token-2", got)
	}
}

func TestOnRateLimitedReturnsFalseWhenAllAccountsCooling(t *testing.T) {
	a := newRateTestPool(t, 2)
	if !a.OnRateLimited(context.Background(), "token-1", "3600") {
		t.Fatal("第一次 429 应换到 token-2")
	}
	if a.OnRateLimited(context.Background(), "token-2", "3600") {
		t.Fatal("全部账号都在冷却时应返回 false，交给 provider 级熔断")
	}
	// 换号失败时索引不动：冷却过期后仍从 token-2 继续。
	if got := a.BearerToken(); got != "token-2" {
		t.Fatalf("轮转失败后当前账号 = %s, want token-2", got)
	}
}

func TestOnRateLimitedSingleAccount(t *testing.T) {
	a := newRateTestPool(t, 1)
	if a.OnRateLimited(context.Background(), "token-1", "") {
		t.Fatal("单账号渠道没有可换的账号，应返回 false")
	}
	// 冷却仍要记账：Admin API 能看到额度窗口，冷却过期前语义一致。
	creds := a.Credentials()
	if len(creds) != 1 || creds[0].RateLimitedUntil == "" {
		t.Fatalf("单账号 429 后摘要应带 rate_limited_until, got %+v", creds)
	}
}

func TestOnRateLimitedSkipsCoolingAccountsAfterExpiry(t *testing.T) {
	a := newRateTestPool(t, 2)
	// token-1 冷却 1 秒、换到 token-2；等 token-1 的冷却过期后再对 token-2
	// 触发 429，应轮转回 token-1 而不是报「全部耗尽」。
	if !a.OnRateLimited(context.Background(), "token-1", "1") {
		t.Fatal("第一次 429 应换号")
	}
	time.Sleep(1100 * time.Millisecond)
	if !a.OnRateLimited(context.Background(), "token-2", "1") {
		t.Fatal("token-1 冷却过期后应可再次选中")
	}
	if got := a.BearerToken(); got != "token-1" {
		t.Fatalf("换号后 = %s, want token-1", got)
	}
}

func TestOnRateLimitedIgnoresUnknownToken(t *testing.T) {
	a := newRateTestPool(t, 2)
	// 并发下「先发请求、后被轮转、最后才收到 429」的旧令牌：不应把冷却记到
	// 当前账号头上，只做轮转判定。
	if !a.OnRateLimited(context.Background(), "token-gone", "3600") {
		t.Fatal("未知令牌仍应能换号")
	}
	creds := a.Credentials()
	for _, c := range creds {
		if c.RateLimitedUntil != "" {
			t.Fatalf("未知令牌不应给任何账号记冷却: %+v", creds)
		}
	}
}

func TestCredentialsExposesRateLimitedUntil(t *testing.T) {
	a := newRateTestPool(t, 2)
	a.OnRateLimited(context.Background(), "token-1", "120")
	creds := a.Credentials()
	if len(creds) != 2 {
		t.Fatalf("账号数 = %d, want 2", len(creds))
	}
	if creds[0].RateLimitedUntil == "" {
		t.Fatal("被 429 的账号摘要应包含 rate_limited_until")
	}
	if _, err := time.Parse(time.RFC3339, creds[0].RateLimitedUntil); err != nil {
		t.Fatalf("rate_limited_until 应为 RFC3339: %v", err)
	}
	if creds[1].RateLimitedUntil != "" {
		t.Fatal("未被 429 的账号不应有 rate_limited_until")
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		raw  string
		want time.Duration
	}{
		{"空串用默认窗口", "", defaultRateCooldown},
		{"秒数", "90", 90 * time.Second},
		{"零秒下限", "0", time.Second},
		{"负值不可信退默认", "-5", defaultRateCooldown},
		{"垃圾值退默认", "soon", defaultRateCooldown},
		{"超上限截断", "999999", 24 * time.Hour},
		{"HTTP 日期", now.Add(time.Minute).Format(http.TimeFormat), time.Minute},
		{"过去的 HTTP 日期退默认", now.Add(-time.Minute).Format(http.TimeFormat), defaultRateCooldown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseRetryAfter(tc.raw, now)
			if got != tc.want {
				t.Fatalf("parseRetryAfter(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}
