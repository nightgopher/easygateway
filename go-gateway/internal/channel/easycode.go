package channel

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const easyCodeBase = "https://api-code.deepvlab.ai"
const easyCodeChannel = "easycode"

// ClientOAuth describes a one-use authorization. The desktop owns the
// loopback listener; only the gateway exchanges and stores credentials.
type ClientOAuth struct {
	State    string        `json:"state"`
	Callback string        `json:"callback"`
	Tenants  []OAuthTenant `json:"tenants"`
}

type OAuthTenant struct {
	AppID string `json:"app_id"`
	Label string `json:"label"`
}

type EasyCodeModel struct {
	ID             string
	Name           string
	ContextWindow  int
	SupportsImages bool
}

type EasyCodeChannel struct {
	accounts *Accounts
	base     string
	client   *http.Client
}

func NewEasyCodeChannel(dir string) *EasyCodeChannel {
	c := &EasyCodeChannel{base: easyCodeBase, client: &http.Client{
		Timeout:       20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	c.accounts = NewAccounts(easyCodeChannel, NewStore(dir), nil, c.refresh)
	return c
}

func (*EasyCodeChannel) Name() string          { return easyCodeChannel }
func (c *EasyCodeChannel) Accounts() *Accounts { return c.accounts }
func (*EasyCodeChannel) BeginLogin(context.Context) (LoginSession, error) {
	return nil, errors.New("请使用最新版 EasyGateway 桌面端发起飞书登录")
}

// Only the pre-registered loopback callback ports are accepted. In particular,
// a remote admin cannot make the gateway exchange codes for an arbitrary URI.
func validEasyCodeCallback(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Hostname() != "localhost" || u.User != nil || u.Path != "/callback" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	port, err := strconv.Atoi(u.Port())
	return err == nil && port >= 7863 && port <= 7873 && u.Host == "localhost:"+strconv.Itoa(port)
}

func (c *EasyCodeChannel) BeginClientLogin(ctx context.Context, callback string) (LoginSession, error) {
	if !validEasyCodeCallback(callback) {
		return nil, errors.New("飞书登录回调地址无效")
	}
	var response struct {
		Success bool `json:"success"`
		Data    struct {
			Feishu struct {
				AppID string `json:"appId"`
				Label string `json:"label"`
			} `json:"feishu"`
			Tenants []struct {
				AppID string `json:"appId"`
				Label string `json:"label"`
			} `json:"feishuTenants"`
		} `json:"data"`
	}
	if _, err := c.request(ctx, http.MethodGet, "/api/config/client", "", nil, &response); err != nil || !response.Success {
		return nil, errors.New("无法获取 EasyCode 飞书登录配置")
	}
	if len(response.Data.Tenants) == 0 {
		response.Data.Tenants = append(response.Data.Tenants, response.Data.Feishu)
	}
	tenants := make([]OAuthTenant, 0, len(response.Data.Tenants))
	seen := make(map[string]bool)
	for _, t := range response.Data.Tenants {
		if t.AppID == "" || len(t.AppID) > 200 || strings.Trim(t.AppID, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-") != "" || seen[t.AppID] {
			continue
		}
		seen[t.AppID] = true
		tenants = append(tenants, OAuthTenant{AppID: t.AppID, Label: t.Label})
	}
	if len(tenants) == 0 {
		return nil, errors.New("EasyCode 未提供可用的飞书组织")
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, errors.New("无法启动安全登录")
	}
	return &easyCodeLogin{channel: c, oauth: ClientOAuth{State: hex.EncodeToString(nonce[:]), Callback: callback, Tenants: tenants}, callback: make(chan easyCodeCallback, 1)}, nil
}

type easyCodeCallback struct {
	code  string
	appID string
	err   bool
}

type easyCodeLogin struct {
	channel   *EasyCodeChannel
	oauth     ClientOAuth
	callback  chan easyCodeCallback
	mu        sync.Mutex
	claimed   bool
	cancelled bool
	committed bool
	finished  bool
}

func (*easyCodeLogin) Channel() string { return easyCodeChannel }
func (*easyCodeLogin) DeviceCode() DeviceCode {
	return DeviceCode{ExpiresIn: 5 * time.Minute, Interval: 2 * time.Second}
}
func (s *easyCodeLogin) ClientOAuth() ClientOAuth { return s.oauth }

// Cancel prevents an in-flight exchange from saving a credential. A false
// result means the credential was already committed before cancellation.
func (s *easyCodeLogin) Cancel() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.committed {
		return false
	}
	s.cancelled = true
	return true
}

// Complete validates the state and hands the one-use code to Wait. It never
// returns the EasyCode access or refresh token to the desktop.
func (s *easyCodeLogin) Complete(ctx context.Context, state, code, oauthErr string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	idx := strings.LastIndexByte(state, '.')
	if idx < 0 || len(state[:idx]) != len(s.oauth.State) || subtle.ConstantTimeCompare([]byte(state[:idx]), []byte(s.oauth.State)) != 1 {
		return errors.New("飞书授权状态无效")
	}
	i, err := strconv.Atoi(state[idx+1:])
	if err != nil || i < 0 || i >= len(s.oauth.Tenants) || state[idx+1:] != strconv.Itoa(i) {
		return errors.New("飞书授权状态无效")
	}
	if len(code) > 4096 || strings.ContainsAny(code, "\r\n\x00") {
		return errors.New("飞书授权码无效")
	}
	s.mu.Lock()
	if s.claimed || s.cancelled || s.finished {
		s.mu.Unlock()
		return errors.New("授权已处理")
	}
	s.claimed = true
	s.callback <- easyCodeCallback{code: code, appID: s.oauth.Tenants[i].AppID, err: oauthErr != "" || code == ""}
	s.mu.Unlock()
	return nil
}

func (s *easyCodeLogin) Wait(ctx context.Context) (*Credential, error) {
	defer func() { s.mu.Lock(); s.finished = true; s.mu.Unlock() }()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case cb := <-s.callback:
		if cb.err {
			return nil, ErrLoginCancelled
		}
		cred, err := s.exchange(ctx, cb.code, cb.appID)
		s.mu.Lock()
		if err == nil {
			err = ctx.Err()
		}
		if err == nil && s.cancelled {
			err = context.Canceled
		}
		if err == nil {
			err = s.channel.accounts.add(*cred)
			if err == nil {
				s.committed = true
			}
		}
		s.mu.Unlock()
		return cred, err
	}
}

func (s *easyCodeLogin) exchange(ctx context.Context, code, appID string) (*Credential, error) {
	var feishu struct {
		Success bool `json:"success"`
		Data    struct {
			AccessToken string `json:"accessToken"`
		} `json:"data"`
	}
	_, err := s.channel.request(ctx, http.MethodPost, "/api/auth/feishu/exchange", "", map[string]string{"code": code, "redirect_uri": s.oauth.Callback, "app_id": appID}, &feishu)
	if err != nil || !feishu.Success || feishu.Data.AccessToken == "" {
		return nil, errors.New("飞书授权交换失败，请重新登录")
	}
	var jwt struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		User         struct {
			ID    string `json:"userId"`
			Name  string `json:"name"`
			Email string `json:"email"`
		} `json:"user"`
	}
	_, err = s.channel.request(ctx, http.MethodPost, "/auth/jwt/feishu-login", "", map[string]any{"feishuAccessToken": feishu.Data.AccessToken, "clientInfo": map[string]any{"platform": "easygateway", "userAgent": "EasyGateway", "timestamp": time.Now().UnixMilli()}}, &jwt)
	if err != nil || jwt.AccessToken == "" || jwt.User.ID == "" {
		return nil, errors.New("EasyCode 登录失败，请检查账号授权后重试")
	}
	if jwt.ExpiresIn <= 0 {
		jwt.ExpiresIn = 900
	}
	name := jwt.User.Name
	if name == "" {
		name = jwt.User.Email
	}
	return &Credential{AccessToken: jwt.AccessToken, RefreshToken: jwt.RefreshToken, ExpiresAt: time.Now().Add(time.Duration(jwt.ExpiresIn) * time.Second), AccountID: jwt.User.ID, Nickname: name}, nil
}

func (c *EasyCodeChannel) refresh(ctx context.Context, cred Credential) (Credential, error) {
	var res struct {
		Success bool `json:"success"`
		Data    struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresIn    int64  `json:"expiresIn"`
		} `json:"data"`
	}
	_, err := c.request(ctx, http.MethodPost, "/auth/jwt/refresh", "", map[string]string{"refreshToken": cred.RefreshToken}, &res)
	if err != nil || !res.Success || res.Data.AccessToken == "" {
		return Credential{}, errors.New("EasyCode 登录续期失败")
	}
	cred.AccessToken = res.Data.AccessToken
	if res.Data.RefreshToken != "" {
		cred.RefreshToken = res.Data.RefreshToken
	}
	if res.Data.ExpiresIn <= 0 {
		res.Data.ExpiresIn = 900
	}
	cred.ExpiresAt = time.Now().Add(time.Duration(res.Data.ExpiresIn) * time.Second)
	return cred, nil
}

func (c *EasyCodeChannel) request(ctx context.Context, method, path, token string, body, out any) (int, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return 0, errors.New("EasyCode 请求无效")
	}
	req.Header.Set("User-Agent", "EasyGateway")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, errors.New("无法连接 EasyCode 服务")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("EasyCode 服务返回 HTTP %d", resp.StatusCode)
	}
	if out != nil && json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out) != nil {
		return resp.StatusCode, errors.New("EasyCode 服务返回的数据无效")
	}
	return resp.StatusCode, nil
}

// Models returns only the models available to the currently selected account.
func (c *EasyCodeChannel) Models(ctx context.Context) ([]EasyCodeModel, error) {
	token := c.accounts.BearerToken()
	if token == "" {
		return nil, errors.New("请先登录 EasyCode")
	}
	var res struct {
		Success bool `json:"success"`
		Data    []struct {
			Name           string `json:"name"`
			DisplayName    string `json:"displayName"`
			MaxToken       int    `json:"maxToken"`
			Available      *bool  `json:"available"`
			SupportsImages *bool  `json:"supportsImages"`
		} `json:"data"`
	}
	status, err := c.request(ctx, http.MethodGet, "/web-api/models", token, nil, &res)
	if status == http.StatusUnauthorized && c.accounts.Refresh(ctx) {
		_, err = c.request(ctx, http.MethodGet, "/web-api/models", c.accounts.BearerToken(), nil, &res)
	}
	if err != nil {
		return nil, err
	}
	if !res.Success || res.Data == nil {
		return nil, errors.New("EasyCode 模型目录无效")
	}
	models := make([]EasyCodeModel, 0, len(res.Data))
	seen := make(map[string]bool)
	for _, item := range res.Data {
		id := strings.TrimSpace(item.Name)
		if id == "" || len(id) > 200 || strings.ContainsAny(id, "\r\n\t") || (item.Available != nil && !*item.Available) || seen[id] {
			continue
		}
		seen[id] = true
		name := strings.TrimSpace(item.DisplayName)
		if name == "" {
			name = id
		}
		window := item.MaxToken
		if window < 0 {
			window = 0
		}
		if window > 160000 {
			window = 160000
		}
		models = append(models, EasyCodeModel{ID: id, Name: name, ContextWindow: window, SupportsImages: item.SupportsImages != nil && *item.SupportsImages})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models, nil
}
