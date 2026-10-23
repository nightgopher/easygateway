package channel

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestAccountsConcurrentRefreshSerializes 验证并发 Refresh 不会撕裂读凭据，
// 且同一账号不会被二次续期（refreshFn 只被调用一次）。
func TestAccountsConcurrentRefreshSerializes(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := store.Save("race", []Credential{{
		AccountID:    "acct-1",
		AccessToken:  "old-token",
		RefreshToken: "r1",
		ExpiresAt:    time.Now().Add(-time.Minute),
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var refreshes atomic.Int32
	accounts := NewAccounts("race", store, newOAuthClient(), func(_ context.Context, cred Credential) (Credential, error) {
		refreshes.Add(1)
		next := cred
		next.AccessToken = "new-token"
		next.ExpiresAt = time.Now().Add(2 * time.Hour)
		return next, nil
	})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			accounts.Refresh(context.Background())
			accounts.BearerToken()
			accounts.CurrentExtra()
		}()
	}
	// 并发续期与读取同时进行，race detector 下验证无数据竞争。
	wg.Wait()

	if n := refreshes.Load(); n != 1 {
		t.Errorf("同一账号并发续期应只触发一次 refresh，实际 %d 次", n)
	}
	if token := accounts.BearerToken(); token != "new-token" {
		t.Errorf("续期后令牌应为 new-token，得到 %q", token)
	}
}
