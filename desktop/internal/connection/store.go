// Package connection owns desktop profiles and authenticated Admin API traffic.
package connection

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Profile struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	HasToken bool   `json:"hasToken"`
}
type Input struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	URL   string `json:"url"`
	Token string `json:"token"`
}
type State struct {
	ActiveID string    `json:"activeId"`
	Profiles []Profile `json:"profiles"`
}
type storedProfile struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	URL   string `json:"url"`
	Token string `json:"token"`
}
type diskState struct {
	ActiveID string          `json:"activeId"`
	Profiles []storedProfile `json:"profiles"`
}
type Store struct {
	mu     sync.RWMutex
	state  diskState
	path   string
	client *http.Client
}

func New(path string) (*Store, error) {
	s := &Store{path: path, client: &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyFromEnvironment,
			DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second, IdleConnTimeout: 90 * time.Second},
		// Never follow an upstream redirect carrying a management credential.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取连接配置失败: %w", err)
	}
	if err := json.Unmarshal(b, &s.state); err != nil {
		return nil, errors.New("连接配置格式无效，请先备份 connections.json")
	}
	for _, p := range s.state.Profiles {
		if _, err := NormalizeURL(p.URL); err != nil {
			return nil, errors.New("连接配置包含无效地址")
		}
	}
	return s, nil
}

func NormalizeURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("请输入完整的 http:// 或 https:// 网关地址，不要包含密码、查询参数或片段")
	}
	u.Path = strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/admin")
	u.RawPath = ""
	return strings.TrimRight(u.String(), "/"), nil
}

func (s *Store) State() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return publicState(s.state)
}
func publicState(d diskState) State {
	out := State{ActiveID: d.ActiveID, Profiles: make([]Profile, 0, len(d.Profiles))}
	for _, p := range d.Profiles {
		out.Profiles = append(out.Profiles, Profile{p.ID, p.Name, p.URL, p.Token != ""})
	}
	return out
}
func (s *Store) prepare(in Input) (storedProfile, error) {
	address, err := NormalizeURL(in.URL)
	if err != nil {
		return storedProfile{}, err
	}
	p := storedProfile{in.ID, strings.TrimSpace(in.Name), address, strings.TrimSpace(in.Token)}
	if p.Name == "" {
		return p, errors.New("请填写连接名称")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if p.ID != "" {
		found := false
		for _, old := range s.state.Profiles {
			if old.ID == p.ID {
				found = true
				if p.Token == "" {
					p.Token = old.Token
				}
			}
		}
		if !found {
			return p, errors.New("此连接已被移除，请重新添加")
		}
	}
	if p.Token == "" {
		return p, errors.New("请填写 ADMIN_TOKEN 或 EASYGATEWAY_MASTER_KEY")
	}
	return p, nil
}
func (s *Store) Test(ctx context.Context, in Input) error {
	p, err := s.prepare(in)
	if err != nil {
		return err
	}
	return s.probe(ctx, p)
}
func (s *Store) probe(ctx context.Context, p storedProfile) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL+"/admin/health", nil)
	if err != nil {
		return errors.New("网关地址无效")
	}
	req.Header.Set("Authorization", "Bearer "+p.Token)
	resp, err := s.client.Do(req)
	if err != nil {
		return errors.New("无法连接网关，请检查地址、网络和证书")
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return errors.New("管理 Token 无效或没有访问权限")
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("网关检查失败（HTTP %d）", resp.StatusCode)
	}
	return nil
}

// Save only persists a verified connection. Blank tokens retain an existing secret.
// VerifySave checks the connection off the UI thread. The returned commit has
// no network work and is invoked only if the UI still accepts this operation.
func (s *Store) VerifySave(ctx context.Context, in Input) (func() (State, error), error) {
	p, err := s.prepare(in)
	if err != nil {
		return nil, err
	}
	if err = s.probe(ctx, p); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	var once sync.Once
	var state State
	var commitErr error
	return func() (State, error) {
		once.Do(func() { state, commitErr = s.saveProfile(p) })
		return state, commitErr
	}, nil
}
func (s *Store) Save(ctx context.Context, in Input) (State, error) {
	commit, err := s.VerifySave(ctx, in)
	if err != nil {
		return State{}, err
	}
	return commit()
}
func (s *Store) saveProfile(p storedProfile) (State, error) {
	var err error
	s.mu.Lock()
	defer s.mu.Unlock()
	next := diskState{ActiveID: p.ID, Profiles: append([]storedProfile(nil), s.state.Profiles...)}
	if p.ID == "" {
		var id [12]byte
		if _, err = rand.Read(id[:]); err != nil {
			return State{}, err
		}
		p.ID = hex.EncodeToString(id[:])
		next.Profiles = append(next.Profiles, p)
	} else {
		found := false
		for i := range next.Profiles {
			if next.Profiles[i].ID == p.ID {
				next.Profiles[i] = p
				found = true
			}
		}
		if !found {
			return State{}, errors.New("此连接已被移除")
		}
	}
	next.ActiveID = p.ID
	if err = s.persist(next); err != nil {
		return State{}, err
	}
	return publicState(next), nil
}
func (s *Store) Select(id string) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.state
	found := false
	for _, p := range next.Profiles {
		if p.ID == id {
			found = true
		}
	}
	if !found {
		return State{}, errors.New("连接不存在")
	}
	next.ActiveID = id
	if err := s.persist(next); err != nil {
		return State{}, err
	}
	return publicState(next), nil
}
func (s *Store) Delete(id string) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := diskState{ActiveID: s.state.ActiveID, Profiles: make([]storedProfile, 0)}
	for _, p := range s.state.Profiles {
		if p.ID != id {
			next.Profiles = append(next.Profiles, p)
		}
	}
	if next.ActiveID == id {
		next.ActiveID = ""
		if len(next.Profiles) > 0 {
			next.ActiveID = next.Profiles[0].ID
		}
	}
	if err := s.persist(next); err != nil {
		return State{}, err
	}
	return publicState(next), nil
}
func (s *Store) persist(next diskState) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return errors.New("无法创建连接配置目录")
	}
	b, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".connections-*")
	if err != nil {
		return errors.New("无法保存连接配置")
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, s.path)
	}
	if err != nil {
		return errors.New("无法保存连接配置")
	}
	s.state = next
	return nil
}

// Prefs 是桌面端的 UI 偏好（低频、可丢失，丢失不影响功能）。
type Prefs struct {
	ChatModel     string `json:"chat_model,omitempty"`
	SkillSource   string `json:"skill_source,omitempty"`
	ModelCategory string `json:"model_category,omitempty"`
	MarketSection string `json:"market_section,omitempty"`
}

func (s *Store) prefsPath() string {
	return filepath.Join(filepath.Dir(s.path), "prefs.json")
}

// LoadPrefs 读取 UI 偏好；缺失或损坏时返回零值。
func (s *Store) LoadPrefs() Prefs {
	var p Prefs
	raw, err := os.ReadFile(s.prefsPath())
	if err != nil {
		return p
	}
	_ = json.Unmarshal(raw, &p)
	return p
}

// SavePrefs 写 UI 偏好，尽力而为：失败静默忽略。
func (s *Store) SavePrefs(p Prefs) {
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(s.prefsPath(), append(raw, '\n'), 0o600)
}
