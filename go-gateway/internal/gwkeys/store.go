// Package gwkeys 实现 gateway key 多密钥体系：给不同客户端发不同的网关
// 访问密钥，每个密钥可独立配置模型白名单与按天/周/月的 token 限额。
//
// 与 master key 的分工：master key 是管理员身份（全部模型、无限量、可进
// /admin）；gateway key 是发给具体客户端的受限身份。语义对齐 magpie 的
// gateway-key：限额按固定窗口计数，预算耗尽返回 429 与窗口重置时间；
// 模型白名单支持 glob（如 "glm-*"、具体的模型 ID）；窗口内最后一个请求
// 可能略微超出预算（先放行后计数），这是有意的取舍，不做中途截断。
//
// 存储为网关主目录下的 gateway-keys.json（0600）。计数器与密钥同文件
// 持久化，重启不丢当期用量。
package gwkeys

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ContextKey 是认证中间件写入 gin context 的当前 gateway key ID。
const ContextKey = "gateway_key_id"

// GuardContextKey 是认证中间件写入 gin context 的密钥视图，供数据面
// handler 做模型白名单校验；未使用 gateway key 的请求不设置该键。
const GuardContextKey = "gateway_key_guard"

// Guard 是随请求携带的密钥视图（认证时刻的快照）。
type Guard struct {
	AllowedModels []string
}

// ModelAllowed 报告 model 是否在白名单内。空白名单放行全部。
// pattern 语法用 path.Match 的 glob（* ? [class]）。
func (g *Guard) ModelAllowed(model string) bool {
	if len(g.AllowedModels) == 0 {
		return true
	}
	for _, pattern := range g.AllowedModels {
		if ok, err := filepath.Match(pattern, model); err == nil && ok {
			return true
		}
	}
	return false
}

// Limits 是一个密钥的 token 限额（0 表示不限）。
type Limits struct {
	Day   int64 `json:"day,omitempty"`
	Week  int64 `json:"week,omitempty"`
	Month int64 `json:"month,omitempty"`
}

// Configured 报告是否配置了任何限额。
func (l Limits) Configured() bool { return l.Day > 0 || l.Week > 0 || l.Month > 0 }

// Key 是一条 gateway key 及其运行状态。
type Key struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Token         string    `json:"token"` // 完整 token 只在创建/轮换时返回一次，列表只给提示
	Enabled       bool      `json:"enabled"`
	AllowedModels []string  `json:"allowed_models,omitempty"` // glob；空 = 全部模型
	Limits        Limits    `json:"limits,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	Usage         Usage     `json:"usage"`
}

// Usage 是三个固定窗口的 token 计数；窗口起点过期后整体滚动清零。
type Usage struct {
	Day   Window `json:"day"`
	Week  Window `json:"week"`
	Month Window `json:"month"`
}

// Window 是一个固定窗口的计数。
type Window struct {
	Start  time.Time `json:"start"`
	Tokens int64     `json:"tokens"`
}

// Store 管理 gateway key 集合与并发安全的用量记账。
type Store struct {
	mu    sync.RWMutex
	path  string
	keys  []Key
	index map[string]int // sha256(token) hex -> keys 下标
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Load 从 path 载入密钥集合；文件不存在时返回空集合。
func Load(path string) (*Store, error) {
	s := &Store{path: path, index: make(map[string]int)}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read gateway keys: %w", err)
	}
	if len(raw) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(raw, &s.keys); err != nil {
		return nil, fmt.Errorf("parse gateway keys: %w", err)
	}
	s.rebuildIndex()
	return s, nil
}

func (s *Store) rebuildIndex() {
	s.index = make(map[string]int, len(s.keys))
	for i := range s.keys {
		s.index[tokenHash(s.keys[i].Token)] = i
	}
}

func (s *Store) persistLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return fmt.Errorf("create gateway home: %w", err)
	}
	encoded, err := json.MarshalIndent(s.keys, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(s.path, append(encoded, '\n'), 0600); err != nil {
		return fmt.Errorf("write gateway keys: %w", err)
	}
	return nil
}

// Lookup 按 token 找到可用密钥。返回的 Key 是值拷贝，调用方可安全持有。
// 第二个返回值说明拒绝原因：空串 = 通过，"disabled" = 密钥被停用。
func (s *Store) Lookup(token string) (Key, bool, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	i, ok := s.index[tokenHash(token)]
	if !ok {
		return Key{}, false, ""
	}
	key := s.keys[i]
	if !key.Enabled {
		return Key{}, false, "disabled"
	}
	return key, true, ""
}

// BudgetState 描述密钥当前预算是否可用。
type BudgetState struct {
	OK         bool
	RetryAfter time.Duration // 预算耗尽时距最近一个窗口重置的时长
	Limit      int64         // 触发拒绝的限额值
	Window     string        // 触发拒绝的窗口名（day/week/month）
}

// Budget 检查密钥当前窗口用量是否超出限额。
func (s *Store) Budget(key Key, now time.Time) BudgetState {
	if !key.Limits.Configured() {
		return BudgetState{OK: true}
	}
	s.rollWindows(&key.Usage, now)
	state := BudgetState{OK: true}
	check := func(window string, limit, used int64, start time.Time) {
		if limit > 0 && used >= limit && state.OK {
			state.OK = false
			state.Limit = limit
			state.Window = window
			state.RetryAfter = windowReset(window, start).Sub(now)
		}
	}
	check("day", key.Limits.Day, key.Usage.Day.Tokens, key.Usage.Day.Start)
	check("week", key.Limits.Week, key.Usage.Week.Tokens, key.Usage.Week.Start)
	check("month", key.Limits.Month, key.Usage.Month.Tokens, key.Usage.Month.Start)
	return state
}

func windowReset(window string, start time.Time) time.Time {
	switch window {
	case "day":
		return start.AddDate(0, 0, 1)
	case "week":
		return start.AddDate(0, 0, 7)
	default:
		return start.AddDate(0, 1, 0)
	}
}

// rollWindows 把过期的窗口滚动清零。按当前时间推导各窗口的起点；
// 起点不同（跨窗口）即清零重建。
func (s *Store) rollWindows(u *Usage, now time.Time) {
	// 窗口起点按本地日历日对齐（自然日/自然周/自然月）；Truncate 是对绝对
	// 时长的截断，在非整小时时区会把起点挪到别的钟点。
	year, month, dayNumber := now.Date()
	day := time.Date(year, month, dayNumber, 0, 0, 0, 0, now.Location())
	week := day.AddDate(0, 0, -((int(now.Weekday()) + 6) % 7)) // 周一为一周之始
	monthStart := time.Date(year, month, 1, 0, 0, 0, 0, now.Location())
	if !u.Day.Start.Equal(day) {
		u.Day = Window{Start: day}
	}
	if !u.Week.Start.Equal(week) {
		u.Week = Window{Start: week}
	}
	if !u.Month.Start.Equal(monthStart) {
		u.Month = Window{Start: monthStart}
	}
}

// RecordUsage 给 keyID 记一笔 token 用量并落盘。窗口滚动后计数清零。
// 未知 keyID 静默忽略（密钥刚被删除时在途请求的记账）。
func (s *Store) RecordUsage(keyID string, tokens int64, now time.Time) {
	if keyID == "" || tokens <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.keys {
		if s.keys[i].ID != keyID {
			continue
		}
		s.rollWindows(&s.keys[i].Usage, now)
		s.keys[i].Usage.Day.Tokens += tokens
		s.keys[i].Usage.Week.Tokens += tokens
		s.keys[i].Usage.Month.Tokens += tokens
		_ = s.persistLocked()
		return
	}
}

// Summary 是列表视图：不带完整 token。
type Summary struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	TokenHint     string    `json:"token_hint"`
	Enabled       bool      `json:"enabled"`
	AllowedModels []string  `json:"allowed_models,omitempty"`
	Limits        Limits    `json:"limits"`
	Usage         Usage     `json:"usage"`
	CreatedAt     time.Time `json:"created_at"`
}

func tokenHint(token string) string {
	if len(token) <= 10 {
		return token
	}
	return token[:8] + "…" + token[len(token)-4:]
}

// List 返回全部密钥的摘要（不含完整 token），按创建时间排序。
func (s *Store) List() []Summary {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Summary, 0, len(s.keys))
	for i := range s.keys {
		k := &s.keys[i]
		out = append(out, Summary{
			ID:            k.ID,
			Name:          k.Name,
			TokenHint:     tokenHint(k.Token),
			Enabled:       k.Enabled,
			AllowedModels: append([]string(nil), k.AllowedModels...),
			Limits:        k.Limits,
			Usage:         k.Usage,
			CreatedAt:     k.CreatedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// CreateInput 是创建密钥的请求体。
type CreateInput struct {
	Name          string   `json:"name"`
	AllowedModels []string `json:"allowed_models"`
	Limits        Limits   `json:"limits"`
}

var tokenPrefix = "sk-gw-"

// randomToken 生成 sk-gw- 前缀的 32 字节随机 hex token。
func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return tokenPrefix + hex.EncodeToString(buf), nil
}

// Create 新建密钥并落盘，返回完整密钥（token 仅此一次可见）。
func (s *Store) Create(input CreateInput, now time.Time) (Key, error) {
	token, err := randomToken()
	if err != nil {
		return Key{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := Key{
		ID:            fmt.Sprintf("k%d", now.UnixNano()),
		Name:          input.Name,
		Token:         token,
		Enabled:       true,
		AllowedModels: input.AllowedModels,
		Limits:        input.Limits,
		CreatedAt:     now,
	}
	s.rollWindows(&key.Usage, now)
	s.keys = append(s.keys, key)
	if err := s.persistLocked(); err != nil {
		s.keys = s.keys[:len(s.keys)-1]
		return Key{}, err
	}
	s.index[tokenHash(token)] = len(s.keys) - 1
	return key, nil
}

// Rotate 为 keyID 生成新 token 并落盘，返回更新后的密钥。
func (s *Store) Rotate(keyID string, now time.Time) (Key, error) {
	token, err := randomToken()
	if err != nil {
		return Key{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.keys {
		if s.keys[i].ID != keyID {
			continue
		}
		delete(s.index, tokenHash(s.keys[i].Token))
		s.keys[i].Token = token
		s.index[tokenHash(token)] = i
		if err := s.persistLocked(); err != nil {
			return Key{}, err
		}
		return s.keys[i], nil
	}
	return Key{}, fmt.Errorf("gateway key %s not found", keyID)
}

// SetEnabled 启用/停用密钥。停用立即生效（Lookup 拒绝）。
func (s *Store) SetEnabled(keyID string, enabled bool) error {
	return s.mutate(keyID, func(k *Key) { k.Enabled = enabled })
}

// Remove 删除密钥。
func (s *Store) Remove(keyID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.keys {
		if s.keys[i].ID != keyID {
			continue
		}
		delete(s.index, tokenHash(s.keys[i].Token))
		s.keys = append(s.keys[:i], s.keys[i+1:]...)
		return s.persistLocked()
	}
	return fmt.Errorf("gateway key %s not found", keyID)
}

func (s *Store) mutate(keyID string, apply func(*Key)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.keys {
		if s.keys[i].ID == keyID {
			apply(&s.keys[i])
			return s.persistLocked()
		}
	}
	return fmt.Errorf("gateway key %s not found", keyID)
}
