package console

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/nightgopher/easygateway/desktop/internal/connection"
)

func TestEasyCodeRemoteDesktopLoginAndCallback(t *testing.T) {
	const state = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	var completed atomic.Bool
	cancelled := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer admin-secret" {
			t.Error("wrong admin credential")
		}
		switch {
		case r.URL.Path == "/admin/health":
			io.WriteString(w, `{"status":"ok"}`)
		case r.URL.Path == "/admin/channels/easycode/login" && r.Method == http.MethodPost:
			var body struct {
				Callback string `json:"callback"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			_ = json.NewEncoder(w).Encode(ChannelLogin{ID: "login-1", Channel: "easycode", ExpiresIn: 60, Interval: 1, OAuth: &clientOAuth{State: state, Callback: body.Callback, Tenants: []oauthTenant{{AppID: "app-a", Label: "<script>bad</script>"}, {AppID: "app-b", Label: "Team"}}}})
		case r.URL.Path == "/admin/channels/easycode/login/login-1/callback":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["state"] != state+".1" || body["code"] != "code-fixture" {
				t.Error("callback changed authorization data")
			}
			completed.Store(true)
			w.WriteHeader(http.StatusAccepted)
			io.WriteString(w, `{"status":"pending"}`)
		case r.URL.Path == "/admin/channels/easycode/login/login-1" && r.Method == http.MethodDelete:
			cancelled <- struct{}{}
			io.WriteString(w, `{"status":"cancelled"}`)
		case r.URL.Path == "/admin/channels/easycode/login/login-1":
			if !completed.Load() {
				w.WriteHeader(http.StatusAccepted)
				io.WriteString(w, `{"status":"pending"}`)
			} else {
				io.WriteString(w, `{"status":"complete"}`)
			}
		case r.URL.Path == "/admin/channels":
			io.WriteString(w, `{"channels":[{"channel":"easycode","logged_in":true}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	store, err := connection.New(filepath.Join(t.TempDir(), "connections.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(context.Background(), connection.Input{Name: "remote", URL: server.URL, Token: "admin-secret"}); err != nil {
		t.Fatal(err)
	}
	a := New(store)
	a.page = "channels"
	// The profile shown by the UI is remote; the fixture transport remains local.
	a.profiles.Profiles[0].URL = "https://remote.example.test"
	if a.localConnection() {
		t.Fatal("fixture must exercise remote login")
	}
	queue := make(chan func(), 100)
	a.SetDispatcher(func(fn func()) { queue <- fn })
	var opened string
	a.OpenLoginURL = func(u string) error { opened = u; return nil }
	defer a.Close()
	a.loaded = true
	a.channels = []AccountChannel{{ID: "easycode"}}
	tt := ui.NewTester(a.View, 1100, 800)
	click(t, tt, "登录 EasyCode（飞书）")
	pumpChannels(t, a, queue, func() bool { return a.channelLogin != nil })
	if opened == "" || !strings.HasPrefix(opened, "http://127.0.0.1:") {
		t.Fatal("desktop did not start its own callback")
	}
	resp, err := http.Get(opened)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), "<script>") || !strings.Contains(string(body), "&lt;script&gt;") {
		t.Fatal("tenant label was not escaped")
	}
	base, _ := url.Parse(opened)
	base.Path, base.RawQuery = "/start", url.Values{"state": {state}, "tenant": {"1"}}.Encode()
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err = noRedirect.Get(base.String())
	if err != nil {
		t.Fatal(err)
	}
	location, err := resp.Location()
	resp.Body.Close()
	if err != nil || location.Host != "open.feishu.cn" || location.Query().Get("app_id") != "app-b" || location.Query().Get("state") != state+".1" {
		t.Fatal("wrong authorization URL")
	}
	callback, _ := url.Parse(location.Query().Get("redirect_uri"))
	callback.Host = strings.Replace(callback.Host, "localhost:", "127.0.0.1:", 1)
	callback.RawQuery = url.Values{"state": {"bad.1"}, "code": {"code-fixture"}}.Encode()
	resp, err = http.Get(callback.String())
	if err != nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatal("invalid state accepted", err)
	}
	resp.Body.Close()
	callback.RawQuery = url.Values{"state": {state + ".1"}, "code": {"code-fixture"}}.Encode()
	resp, err = http.Get(callback.String())
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatal("callback failed", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), "admin-secret") || strings.Contains(string(body), "code-fixture") {
		t.Fatal("callback response leaked credentials")
	}
	resp, err = http.Get(callback.String())
	if err != nil || resp.StatusCode != http.StatusConflict {
		t.Fatal("callback replay accepted", err)
	}
	resp.Body.Close()
	pumpChannels(t, a, queue, func() bool { return a.busy == "" })
	if !completed.Load() || a.channelLogin != nil || !a.channels[0].LoggedIn {
		t.Fatal("desktop did not finish login")
	}
	tt.Frame()
	if !tt.HasText("刷新模型") {
		t.Fatal("manual model refresh control missing")
	}

	completed.Store(false)
	a.startChannelLogin(a.channels[0])
	pumpChannels(t, a, queue, func() bool { return a.channelLogin != nil })
	cancelURL := opened
	tt.Frame()
	for i := 0; i < 40 && !tt.Focused("取消授权"); i++ {
		tt.Key(0, ui.KeyTab)
	}
	if !tt.Focused("取消授权") {
		t.Fatal("cancel is not keyboard reachable")
	}
	tt.Key(0, ui.KeyEnter)
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("server login was not cancelled")
	}
	client := &http.Client{Timeout: time.Second}
	if response, err := client.Get(cancelURL); err == nil {
		response.Body.Close()
		t.Fatal("cancel left callback listener running")
	}

	a.startChannelLogin(a.channels[0])
	pumpChannels(t, a, queue, func() bool { return a.channelLogin != nil })
	oldURL := opened
	var wrongHost atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/channels/easycode/login") {
			wrongHost.Store(true)
		}
		io.WriteString(w, `{"status":"ok"}`)
	}))
	defer other.Close()
	profiles, err := store.Save(context.Background(), connection.Input{Name: "other", URL: other.URL, Token: "other-secret"})
	if err != nil {
		t.Fatal(err)
	}
	a.selectConnection(profiles.ActiveID)
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("host switch did not cancel original session")
	}
	await(t, a, queue)
	if wrongHost.Load() || a.channelLogin != nil {
		t.Fatal("authorization leaked across hosts")
	}
	if response, err := client.Get(oldURL); err == nil {
		response.Body.Close()
		t.Fatal("host switch left callback listener running")
	}
}
