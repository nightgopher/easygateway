package channel

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// refreshLead 是「提前多久续期」的阈值：距过期不足这个时长才刷新，避免每次请求
// 都去轮换 refresh_token（上游会因此失效）。
const refreshLead = time.Hour

// refreshCooldown 是同一账号两次续期之间的最小间隔：并发 401 风暴下，
// 后到的 Refresh 若发现该账号刚被续期过就直接复用，避免旧 refresh_token
// 二次轮换把账号刷下线。
const refreshCooldown = 30 * time.Second

// defaultRateCooldown 是 429 未携带 Retry-After 时账号级配额冷却的默认时长。
// 订阅类额度窗口多为分钟级，取 5 分钟：过短会在窗口内反复撞限流，过长会让
// 已恢复的账号闲置。带合法 Retry-After 时以它为准（1s..24h 截断）。
const defaultRateCooldown = 5 * time.Minute

// refreshFn 对一个凭据执行一次续期并返回新凭据（不触碰存储）。
type refreshFn func(ctx context.Context, cred Credential) (Credential, error)

// Accounts 管理一个渠道的账号池：内存缓存 + 落盘存储 + 令牌续期。
//
// 它同时是 provider 层 AuthSource 与 CredentialReporter 的结构化实现：
// provider 只做接口断言，channel 包不 import provider，避免循环依赖。
type Accounts struct {
	channel string
	store   *Store
	client  *oauthClient
	refresh refreshFn

	mu        sync.RWMutex
	creds     []Credential
	index     int
	refreshMu sync.Mutex // 串行化令牌续期，避免并发双重刷新

	lastRefresh  map[string]time.Time // 账号级最近续期时间（冷却窗口用）
	rateCooldown map[string]time.Time // 账号级配额冷却截止时间（429 轮转用）
}

// NewAccounts 构造账号池并立即从存储载入已有凭据。
func NewAccounts(channel string, store *Store, client *oauthClient, refresh refreshFn) *Accounts {
	a := &Accounts{
		channel:      channel,
		store:        store,
		client:       client,
		refresh:      refresh,
		lastRefresh:  make(map[string]time.Time),
		rateCooldown: make(map[string]time.Time),
	}
	a.reload()
	return a
}

func (a *Accounts) reload() {
	creds, err := a.store.Load(a.channel)
	if err != nil {
		creds = nil
	}
	a.mu.Lock()
	a.creds = creds
	if a.index >= len(creds) {
		a.index = 0
	}
	a.mu.Unlock()
}

// Store 暴露凭据存储，供登录流程写入。
func (a *Accounts) Store() *Store { return a.store }

// Channel 实现 CredentialReporter。
func (a *Accounts) Channel() string { return a.channel }

// LoggedIn 实现 CredentialReporter：至少有一个可用账号即为已登录。
func (a *Accounts) LoggedIn() bool { return a.BearerToken() != "" }

// BearerToken 实现 AuthSource：返回当前账号的访问令牌。空串表示未登录。
func (a *Accounts) BearerToken() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	cred, ok := a.currentLocked()
	if !ok {
		return ""
	}
	return cred.AccessToken
}

// CurrentExtra 返回当前账号的附加字段（uid / machine_id 等）。返回的是副本，
// 调用方可以安全长期持有。未登录时返回 nil。
//
// 这是 trae 这类「请求头需要账号级身份字段」的渠道的扩展点：provider 只认
// AuthSource，拿不到 Credential，故由渠道侧在此暴露。
func (a *Accounts) CurrentExtra() map[string]string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	cred, ok := a.currentLocked()
	if !ok || len(cred.Extra) == 0 {
		return nil
	}
	out := make(map[string]string, len(cred.Extra))
	for key, value := range cred.Extra {
		out[key] = value
	}
	return out
}

// Credentials 实现 CredentialReporter：返回所有账号的非敏感摘要。
// 注意只回传账号标识与有效期，绝不回传 token 明文。
func (a *Accounts) Credentials() []credentialSummary {
	a.mu.RLock()
	defer a.mu.RUnlock()
	now := time.Now()
	out := make([]credentialSummary, 0, len(a.creds))
	for i := range a.creds {
		cred := &a.creds[i]
		summary := credentialSummary{
			AccountID:        cred.AccountID,
			Nickname:         cred.Nickname,
			Expired:          cred.Expired(now),
			RefreshAvailable: a.refresh != nil && cred.RefreshToken != "",
		}
		if !cred.ExpiresAt.IsZero() {
			summary.ExpiresAt = cred.ExpiresAt.UTC().Format(time.RFC3339)
		}
		if until, ok := a.rateCooldown[a.accountKey(*cred)]; ok && until.After(now) {
			summary.RateLimitedUntil = until.UTC().Format(time.RFC3339)
		}
		out = append(out, summary)
	}
	return out
}

// credentialSummary 是 channel 层的非敏感凭据摘要（provider.CredentialSummary 的
// 结构化对应物，字段名一致以便 Admin API 直接序列化）。
type credentialSummary struct {
	AccountID        string `json:"account_id,omitempty"`
	Nickname         string `json:"nickname,omitempty"`
	ExpiresAt        string `json:"expires_at,omitempty"`
	Expired          bool   `json:"expired"`
	RefreshAvailable bool   `json:"refresh_available"`
	RateLimitedUntil string `json:"rate_limited_until,omitempty"`
}

// Refresh 实现 AuthSource：当前账号临近过期或已过期时续期一次。
// 返回 true 表示令牌已更新。续期失败但仍有其他账号时会自动换号。
//
// refreshMu 串行化续期：并发调用者等待后重读当前凭据，若同账号令牌已被
// 前一个调用者换新则直接返回，避免用旧 refresh_token 二次轮换把账号刷下线。
func (a *Accounts) Refresh(ctx context.Context) bool {
	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()

	a.mu.RLock()
	cred, ok := a.currentLocked()
	cooled := a.cooledDownLocked(cred)
	a.mu.RUnlock()
	if !ok {
		return false
	}
	if cooled {
		// 等待期间同账号已被续期，后到者直接复用新令牌。
		return true
	}
	if !a.refreshable(cred) {
		a.advance()
		return a.BearerToken() != ""
	}

	next, err := a.refresh(ctx, cred)
	if err != nil {
		// 续期被上游拒绝（refresh_token 失效）时换下一个账号，而不是死磕一个。
		a.advance()
		return a.BearerToken() != ""
	}
	a.replace(cred, next)
	a.noteRefreshed(next)
	return true
}

// ensureFresh 在发送请求前按 lead-time 静默续期，避免第一个请求先撞一次 401。
func (a *Accounts) ensureFresh(ctx context.Context) {
	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()

	a.mu.RLock()
	cred, ok := a.currentLocked()
	a.mu.RUnlock()
	if !ok || !a.needsRefresh(cred) {
		return
	}
	if next, err := a.refresh(ctx, cred); err == nil {
		a.replace(cred, next)
		a.noteRefreshed(next)
	}
}

// accountKey 返回账号的唯一标识；无 AccountID 的渠道退化为访问令牌。
func (a *Accounts) accountKey(cred Credential) string {
	if cred.AccountID != "" {
		return cred.AccountID
	}
	return cred.AccessToken
}

// cooledDownLocked 报告该账号是否处于续期冷却窗口内；调用方须持锁。
func (a *Accounts) cooledDownLocked(cred Credential) bool {
	ts, ok := a.lastRefresh[a.accountKey(cred)]
	return ok && time.Since(ts) < refreshCooldown
}

// noteRefreshed 记录账号最近一次成功续期的时间。
func (a *Accounts) noteRefreshed(cred Credential) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastRefresh[a.accountKey(cred)] = time.Now()
}

func (a *Accounts) refreshable(cred Credential) bool {
	return cred.RefreshToken != ""
}

func (a *Accounts) needsRefresh(cred Credential) bool {
	return cred.Expired(time.Now().Add(refreshLead))
}

// currentLocked 返回当前选中凭据的值拷贝；调用方须持有读锁或写锁。
// 拷贝是为了让解锁后使用凭据不再与其他写方（replace/reload）竞争。
func (a *Accounts) currentLocked() (Credential, bool) {
	if len(a.creds) == 0 {
		return Credential{}, false
	}
	index := a.index
	if index < 0 || index >= len(a.creds) {
		index = 0
	}
	return a.creds[index], true
}

// advance 切到下一个账号（多账号轮换）。
func (a *Accounts) advance() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.creds) > 1 {
		a.index = (a.index + 1) % len(a.creds)
	}
}

// OnRateLimited 实现 provider 层的账号轮转协议（结构化鸭子类型，provider 侧用
// accountRotator 断言，channel 包不 import provider）：请求撞上 429 后，先在
// 渠道内换一个配额未耗尽的账号，而不是立刻降级到链上其他 provider。
//
// accessToken 是本次请求实际使用的令牌，只给它对应的账号记冷却——并发请求
// 各自收到 429 时，不会把冷却错记到已被别的请求轮转到的账号头上。retryAfter
// 是上游 Retry-After 原文（秒数或 HTTP 日期），可为空。
//
// 返回 true 表示已切到未冷却账号（调用方重取令牌重试一次）；false 表示全部
// 账号都在冷却或没有多账号，由调用方按原 429 走 provider 级配额熔断。冷却状态
// 只在内存中，进程重启即清零，由 provider 级熔断兜底。
func (a *Accounts) OnRateLimited(_ context.Context, accessToken, retryAfter string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	if len(a.creds) == 0 {
		return false
	}
	if key := a.accountKeyForTokenLocked(accessToken); key != "" {
		until := now.Add(parseRetryAfter(retryAfter, now))
		if prev, ok := a.rateCooldown[key]; !ok || prev.Before(until) {
			a.rateCooldown[key] = until
		}
	}
	if len(a.creds) == 1 {
		return false
	}
	// 从当前位置起找下一个未冷却账号；绕一圈回到起点说明全部在冷却。
	for step := 1; step < len(a.creds); step++ {
		next := (a.index + step) % len(a.creds)
		key := a.accountKey(a.creds[next])
		if until, ok := a.rateCooldown[key]; ok && until.After(now) {
			continue
		}
		a.index = next
		return true
	}
	return false
}

// accountKeyForTokenLocked 按访问令牌反查账号键；令牌已不在池中（比如续期
// 换新后被并发覆盖）时返回空串，调用方跳过记账只做轮转判定。调用方须持锁。
func (a *Accounts) accountKeyForTokenLocked(token string) string {
	if token == "" {
		return ""
	}
	for i := range a.creds {
		if a.creds[i].AccessToken == token {
			return a.accountKey(a.creds[i])
		}
	}
	return ""
}

// parseRetryAfter 解析 Retry-After 头（秒数或 HTTP 日期），返回冷却时长。
// 无法解析时退回默认窗口；合法值截断到 [1s, 24h]，避免异常值把账号冻结一天
// 以上。与 provider.CircuitBreaker.RecordRateLimitAt 的解析规则保持一致。
func parseRetryAfter(raw string, now time.Time) time.Duration {
	delay := defaultRateCooldown
	trimmed := strings.TrimSpace(raw)
	if seconds, err := strconv.ParseInt(trimmed, 10, 64); err == nil && seconds >= 0 {
		delay = time.Duration(seconds) * time.Second
	} else if date, err := http.ParseTime(trimmed); err == nil && date.After(now) {
		delay = date.Sub(now)
	}
	if delay < time.Second {
		delay = time.Second
	}
	if delay > 24*time.Hour {
		delay = 24 * time.Hour
	}
	return delay
}

// replace 用续期后的凭据覆盖原凭据并落盘。
func (a *Accounts) replace(old, next Credential) {
	a.mu.Lock()
	index := -1
	for i := range a.creds {
		if a.sameAccount(&a.creds[i], &old) {
			index = i
			break
		}
	}
	if index >= 0 {
		a.creds[index] = next
	}
	a.mu.Unlock()
	if index >= 0 {
		_ = a.store.ReplaceAccount(a.channel, index, next)
	}
}

// add 新增一条账号凭据并落盘，随后重新载入内存。
func (a *Accounts) add(cred Credential) error {
	if err := a.store.UpsertAccount(a.channel, cred); err != nil {
		return fmt.Errorf("save %s credential: %w", a.channel, err)
	}
	a.reload()
	return nil
}

func (a *Accounts) sameAccount(x, y *Credential) bool {
	if x.AccountID != "" && y.AccountID != "" {
		return x.AccountID == y.AccountID
	}
	return x.AccessToken == y.AccessToken
}

// Len 返回账号数量（含已过期）。
func (a *Accounts) Len() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.creds)
}

// RemoveAccountAt 删除第 index 个账号并落盘，随后重载内存。
func (a *Accounts) RemoveAccountAt(index int) error {
	if err := a.store.RemoveAccount(a.channel, index); err != nil {
		return err
	}
	a.reload()
	return nil
}

// RemoveAll 清空本渠道的全部凭据（登出）。
func (a *Accounts) RemoveAll() error {
	if err := a.store.Clear(a.channel); err != nil {
		return err
	}
	a.mu.Lock()
	a.creds = nil
	a.index = 0
	a.lastRefresh = make(map[string]time.Time)
	a.rateCooldown = make(map[string]time.Time)
	a.mu.Unlock()
	return nil
}
