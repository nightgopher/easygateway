package console

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nightgopher/easygateway/desktop/internal/connection"
)

type oauthTenant struct {
	AppID string `json:"app_id"`
	Label string `json:"label"`
}
type clientOAuth struct {
	State    string        `json:"state"`
	Callback string        `json:"callback"`
	Tenants  []oauthTenant `json:"tenants"`
}

type localOAuth struct {
	listener net.Listener
	server   *http.Server
	callback string
	port     string
	mu       sync.Mutex
	claimed  bool
}

func listenLocalOAuth() (*localOAuth, error) {
	for port := 7863; port <= 7873; port++ {
		listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err == nil {
			return &localOAuth{listener: listener, port: strconv.Itoa(port), callback: fmt.Sprintf("http://localhost:%d/callback", port)}, nil
		}
	}
	return nil, errors.New("无法启动本机飞书登录回调，请关闭占用端口的程序后重试")
}

func (s *localOAuth) close() {
	if s == nil {
		return
	}
	if s.server != nil {
		_ = s.server.Close()
	}
	_ = s.listener.Close()
}

// Finish stops accepting callbacks while allowing an in-flight browser
// response to complete, even if the poll observes success immediately.
func (s *localOAuth) finish() {
	if s == nil {
		return
	}
	_ = s.listener.Close()
	if s.server != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if s.server.Shutdown(ctx) != nil {
				_ = s.server.Close()
			}
		}()
	}
}

var oauthPage = template.Must(template.New("feishu").Parse(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>EasyGateway 飞书登录</title></head><body><main><h1>登录 EasyCode</h1><p>选择飞书组织并完成授权，模型使用该账号的额度。</p>{{range $i, $tenant := .Tenants}}<p><a href="/start?state={{$.State}}&amp;tenant={{$i}}">{{if $tenant.Label}}{{$tenant.Label}}{{else}}飞书登录{{end}}</a></p>{{end}}</main></body></html>`))

func (s *localOAuth) start(o clientOAuth, client *connection.Client, channel, id string) (string, error) {
	if o.Callback != s.callback || len(o.State) != 64 || len(o.Tenants) == 0 || len(o.Tenants) > 100 {
		return "", errors.New("网关返回的飞书登录会话无效")
	}
	for _, t := range o.Tenants {
		if t.AppID == "" || len(t.AppID) > 200 || strings.Trim(t.AppID, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-") != "" {
			return "", errors.New("网关返回的飞书组织无效")
		}
	}
	s.server = &http.Server{ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 45 * time.Second}
	s.server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; form-action 'none'; base-uri 'none'; frame-ancestors 'none'")
		host, port, err := net.SplitHostPort(r.Host)
		if err != nil || (host != "localhost" && host != "127.0.0.1") || port != s.port || r.Method != http.MethodGet {
			http.Error(w, "Invalid request", http.StatusBadRequest)
			return
		}
		q := r.URL.Query()
		sameState := func(state string) bool {
			return len(state) == len(o.State) && subtle.ConstantTimeCompare([]byte(state), []byte(o.State)) == 1
		}
		switch r.URL.Path {
		case "/":
			if !sameState(q.Get("state")) {
				http.Error(w, "Invalid login request", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_ = oauthPage.Execute(w, o)
		case "/start":
			i, err := strconv.Atoi(q.Get("tenant"))
			if !sameState(q.Get("state")) || err != nil || i < 0 || i >= len(o.Tenants) {
				http.Error(w, "Invalid login request", http.StatusBadRequest)
				return
			}
			params := url.Values{"app_id": {o.Tenants[i].AppID}, "redirect_uri": {o.Callback}, "response_type": {"code"}, "scope": {"contact:user.employee_id:readonly"}, "state": {o.State + "." + strconv.Itoa(i)}}
			http.Redirect(w, r, "https://open.feishu.cn/open-apis/authen/v1/authorize?"+params.Encode(), http.StatusFound)
		case "/callback":
			state := q.Get("state")
			idx := strings.LastIndexByte(state, '.')
			if idx < 0 || !sameState(state[:idx]) {
				http.Error(w, "Invalid authorization state", http.StatusBadRequest)
				return
			}
			i, err := strconv.Atoi(state[idx+1:])
			if err != nil || i < 0 || i >= len(o.Tenants) || state[idx+1:] != strconv.Itoa(i) || len(q.Get("code")) > 4096 {
				http.Error(w, "Invalid authorization state", http.StatusBadRequest)
				return
			}
			s.mu.Lock()
			if s.claimed {
				s.mu.Unlock()
				http.Error(w, "Authorization already received", http.StatusConflict)
				return
			}
			s.claimed = true
			s.mu.Unlock()
			ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
			defer cancel()
			err = client.JSON(ctx, http.MethodPost, loginPath(channel, id)+"/callback", map[string]string{"state": state, "code": q.Get("code"), "error": q.Get("error")}, nil)
			if err != nil {
				http.Error(w, "登录未完成，请返回 EasyGateway 重试。", http.StatusBadGateway)
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = fmt.Fprint(w, "已收到授权，请返回 EasyGateway 查看登录结果。")
		default:
			http.NotFound(w, r)
		}
	})
	go func() { _ = s.server.Serve(s.listener) }()
	return "http://127.0.0.1:" + s.port + "/?state=" + url.QueryEscape(o.State), nil
}
