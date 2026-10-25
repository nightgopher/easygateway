package console

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/nightgopher/easygateway/desktop/internal/connection"
)

type ChannelAccount struct {
	AccountID        string `json:"account_id"`
	Nickname         string `json:"nickname"`
	ExpiresAt        string `json:"expires_at"`
	Expired          bool   `json:"expired"`
	RefreshAvailable bool   `json:"refresh_available"`
}
type AccountChannel struct {
	ModelHealth       map[string]ModelHealth `json:"model_health"`
	ID                string                 `json:"channel"`
	LoggedIn          bool                   `json:"logged_in"`
	Accounts          []ChannelAccount       `json:"accounts"`
	LoginAvailable    *bool                  `json:"login_available"`
	NeedsClientConfig bool                   `json:"needs_client_config"`
	LocalOnly         bool                   `json:"local_login_only"`
	SetupHint         string                 `json:"setup_hint"`
	Anonymous         bool                   `json:"anonymous_available"`
	Models            []string               `json:"models"`
}
type ChannelLogin struct {
	ID        string `json:"login_id"`
	Channel   string `json:"channel"`
	UserCode  string `json:"user_code"`
	URL       string `json:"login_url"`
	ExpiresIn int    `json:"expires_in"`
	Interval  int    `json:"interval"`
	expiresAt time.Time
}

func channelTitle(id string) string {
	names := map[string]string{"minimax": "MiniMax", "cline": "Cline", "lobster": "Lobster", "gemini": "Gemini", "codebuddy": "CodeBuddy", "workbuddy": "WorkBuddy", "trae": "TRAE", "loomy": "Loomy", "raccoon": "Raccoon 小浣熊", "opencode": "OpenCode"}
	if name := names[id]; name != "" {
		return name
	}
	return id
}
func orderChannels(channels []AccountChannel) []AccountChannel {
	sort.SliceStable(channels, func(i, j int) bool { return !channels[i].NeedsClientConfig && channels[j].NeedsClientConfig })
	return channels
}
func (ch AccountChannel) canLogin(local bool) bool {
	return (ch.LoginAvailable == nil || *ch.LoginAvailable) && (!ch.LocalOnly || local)
}
func loginPath(channel, id string) string {
	return "/channels/" + url.PathEscape(channel) + "/login/" + url.PathEscape(id)
}
func cancelLoginOn(client *connection.Client, channel, id string) {
	if client == nil || id == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.JSON(ctx, "DELETE", loginPath(channel, id), nil, nil)
	}()
}
func (a *App) cancelChannelSession() {
	if a.channelLogin != nil {
		cancelLoginOn(a.channelClient, a.channelLogin.Channel, a.channelLogin.ID)
		a.channelLogin = nil
		a.channelClient = nil
		a.notice = "已取消授权"
	}
}

// Login links never receive the gateway management token and are never persisted.
func validLoginURL(raw string, local bool) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || strings.ContainsAny(raw, "\r\n") {
		return false
	}
	host := u.Hostname()
	loopback := host == "localhost"
	if ip := net.ParseIP(host); ip != nil {
		loopback = ip.IsLoopback()
	}
	if loopback && !local {
		return false
	}
	return u.Scheme == "https" || (u.Scheme == "http" && loopback && local)
}
func (a *App) openChannelLogin() {
	if a.channelLogin == nil {
		return
	}
	if a.OpenLoginURL == nil || a.OpenLoginURL(a.channelLogin.URL) != nil {
		a.problem = "未能打开浏览器，请复制授权链接后打开。"
	}
}
func (a *App) startChannelLogin(ch AccountChannel) {
	if a.busy != "" || a.dispatch == nil || a.channelLogin != nil {
		return
	}
	if !ch.canLogin(a.localConnection()) {
		a.problem = "请先完成渠道配置，并在网关所在电脑发起回环登录。"
		return
	}
	client, err := a.store.Client()
	if err != nil {
		a.problem = err.Error()
		return
	}
	a.sequence++
	seq := a.sequence
	dispatch := a.dispatch
	local := a.localConnection()
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	a.cancel = cancel
	a.busy = "正在发起 " + channelTitle(ch.ID) + " 登录"
	a.problem = ""
	a.notice = ""
	go func() {
		var login ChannelLogin
		err := client.JSON(ctx, "POST", "/channels/"+url.PathEscape(ch.ID)+"/login", nil, &login)
		cancel()
		dispatch(func() {
			if seq != a.sequence {
				cancelLoginOn(client, ch.ID, login.ID)
				return
			}
			a.cancel = nil
			a.busy = ""
			if err != nil {
				cancelLoginOn(client, ch.ID, login.ID)
				if !errors.Is(err, context.Canceled) {
					a.problem = "登录未能发起：" + err.Error()
				}
				return
			}
			if login.ID == "" || login.Channel != ch.ID || !validLoginURL(login.URL, local) || login.ExpiresIn <= 0 || login.ExpiresIn > 3600 {
				cancelLoginOn(client, ch.ID, login.ID)
				a.problem = "网关返回的授权会话无效，请刷新或更新网关后重试。"
				return
			}
			login.expiresAt = time.Now().Add(time.Duration(login.ExpiresIn) * time.Second)
			a.channelLogin = &login
			a.channelClient = client
			a.pollChannelLogin()
			a.openChannelLogin()
		})
	}()
}
func (a *App) pollChannelLogin() {
	if a.channelLogin == nil || a.busy != "" {
		return
	}
	login := *a.channelLogin
	client := a.channelClient
	remaining := time.Until(login.expiresAt) + 30*time.Second
	if remaining <= 0 {
		a.cancelChannelSession()
		a.problem = "授权已过期，请重新登录。"
		return
	}
	interval := time.Duration(login.Interval) * time.Second
	if interval < time.Second {
		interval = time.Second
	}
	if interval > 10*time.Second {
		interval = 10 * time.Second
	}
	a.launch("等待 "+channelTitle(login.Channel)+" 浏览器授权", remaining, func(ctx context.Context) (func(), error) {
		for {
			var result struct {
				Status string `json:"status"`
			}
			if err := client.JSON(ctx, "GET", loginPath(login.Channel, login.ID), nil, &result); err != nil {
				return nil, err
			}
			switch result.Status {
			case "complete":
				var list struct {
					Channels []AccountChannel `json:"channels"`
				}
				err := client.JSON(ctx, "GET", "/channels", nil, &list)
				return func() {
					a.channelLogin = nil
					a.channelClient = nil
					if err == nil {
						a.channels = orderChannels(list.Channels)
						a.notice = channelTitle(login.Channel) + " 已登录；模型调用待验证。"
					} else {
						a.notice = "授权完成，请刷新查看账号状态。"
					}
				}, nil
			case "failed", "cancelled":
				return func() {
					a.channelLogin = nil
					a.channelClient = nil
					a.problem = "授权未完成或已过期，请重新登录。"
				}, nil
			case "pending":
			default:
				return nil, fmt.Errorf("无法识别授权状态，请取消后重新登录")
			}
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
	})
}
