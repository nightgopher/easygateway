package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/nightgopher/easygateway/desktop/internal/connection"
)

func channelsFixture(t *testing.T) (*App, chan func(), *atomic.Bool, chan string) {
	t.Helper()
	ready := new(atomic.Bool)
	cancels := make(chan string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer channel-test-key" {
			t.Error("wrong management credential")
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/admin/channels":
			json.NewEncoder(w).Encode(map[string]any{"channels": []any{map[string]any{"channel": "gemini", "needs_client_config": true, "login_available": false, "setup_hint": "请配置 GEMINI_CHANNEL_CLIENT_ID"}, map[string]any{"channel": "minimax", "login_available": true, "logged_in": ready.Load(), "accounts": []any{}}}})
		case r.URL.Path == "/admin/channels/minimax/login" && r.Method == "POST":
			json.NewEncoder(w).Encode(ChannelLogin{ID: "session-1", Channel: "minimax", URL: "https://auth.example.test/authorize", UserCode: "TEST-CODE", ExpiresIn: 60, Interval: 1})
		case strings.HasSuffix(r.URL.Path, "/login/session-1") && r.Method == "DELETE":
			cancels <- r.Header.Get("Authorization")
			w.Write([]byte(`{"status":"cancelled"}`))
		case strings.HasSuffix(r.URL.Path, "/login/session-1"):
			status := "pending"
			if ready.Load() {
				status = "complete"
			} else {
				w.WriteHeader(http.StatusAccepted)
			}
			json.NewEncoder(w).Encode(map[string]string{"status": status})
		default:
			w.Write([]byte(`{"status":"ok","providers":[],"models":[]}`))
		}
	}))
	t.Cleanup(server.Close)
	store, err := connection.New(filepath.Join(t.TempDir(), "connections.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(context.Background(), connection.Input{Name: "channel host", URL: server.URL, Token: "channel-test-key"}); err != nil {
		t.Fatal(err)
	}
	a := New(store)
	a.page = "channels"
	queue := make(chan func(), 100)
	a.SetDispatcher(func(fn func()) { queue <- fn })
	a.OpenLoginURL = func(string) error { return nil }
	a.CopyText = func(string) {}
	t.Cleanup(a.Close)
	a.reload()
	await(t, a, queue)
	return a, queue, ready, cancels
}
func pumpChannels(t *testing.T, a *App, queue chan func(), done func() bool) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for !done() {
		select {
		case fn := <-queue:
			fn()
		case <-timeout:
			t.Fatal("channel operation timed out")
		}
	}
}
func TestNativeChannelLoginOpensBrowserCopiesCodeAndCompletes(t *testing.T) {
	a, q, ready, _ := channelsFixture(t)
	var opened, copied string
	a.OpenLoginURL = func(url string) error { opened = url; return nil }
	a.CopyText = func(text string) { copied = text }
	tt := ui.NewTester(a.View, 1100, 800)
	click(t, tt, "登录 MiniMax")
	pumpChannels(t, a, q, func() bool { return a.channelLogin != nil })
	tt.Frame()
	if opened != "https://auth.example.test/authorize" || !tt.HasText("等待 MiniMax 授权") {
		t.Fatal("authorization UI missing", opened)
	}
	click(t, tt, "复制授权码")
	if copied != "TEST-CODE" {
		t.Fatal("wrong code copied")
	}
	ready.Store(true)
	pumpChannels(t, a, q, func() bool { return a.busy == "" })
	tt.Frame()
	if a.channelLogin != nil || !a.channels[0].LoggedIn || !strings.Contains(a.notice, "模型调用待验证") {
		t.Fatal("success state inaccurate", a.notice)
	}
}
func TestNativeChannelCancelAndMissingConfiguration(t *testing.T) {
	a, q, _, cancels := channelsFixture(t)
	tt := ui.NewTester(a.View, 1100, 800)
	if a.channels[0].ID != "minimax" {
		t.Fatal("configured channels not prioritized")
	}
	click(t, tt, "Gemini 配置说明")
	tt.Frame()
	if !tt.HasText("请配置 GEMINI_CHANNEL_CLIENT_ID") {
		t.Fatal("missing setup guidance")
	}
	click(t, tt, "登录 MiniMax")
	pumpChannels(t, a, q, func() bool { return a.channelLogin != nil })
	tt.Frame()
	for i := 0; i < 40 && !tt.Focused("取消授权"); i++ {
		tt.Key(0, ui.KeyTab)
	}
	if !tt.Focused("取消授权") {
		t.Fatal("cancel authorization is not keyboard reachable")
	}
	tt.Key(0, ui.KeyEnter)
	if a.channelLogin != nil || a.busy != "" {
		t.Fatal("login not cleared")
	}
	select {
	case token := <-cancels:
		if token != "Bearer channel-test-key" {
			t.Fatal("wrong host token")
		}
	case <-time.After(time.Second):
		t.Fatal("server cancel not sent")
	}
}
func TestChannelHostSwitchCancelsOriginalHostAndDiscardsResult(t *testing.T) {
	a, q, _, cancels := channelsFixture(t)
	a.startChannelLogin(a.channels[0])
	pumpChannels(t, a, q, func() bool { return a.channelLogin != nil })
	var wrongHost atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/channels/") {
			wrongHost.Store(true)
		}
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer other.Close()
	st, err := a.store.Save(context.Background(), connection.Input{Name: "other", URL: other.URL, Token: "other-key"})
	if err != nil {
		t.Fatal(err)
	}
	a.selectConnection(st.ActiveID)
	await(t, a, q)
	select {
	case <-cancels:
	case <-time.After(time.Second):
		t.Fatal("old server session not canceled")
	}
	drainDispatcher(a, q)
	if wrongHost.Load() || a.channelLogin != nil || len(a.channels) != 0 {
		t.Fatal("old authorization contaminated new host")
	}
}
func TestChannelLoginLinkBoundary(t *testing.T) {
	for _, tt := range []struct {
		url          string
		local, valid bool
	}{
		{"https://auth.example.test/login", false, true}, {"http://127.0.0.1:1234/login", true, true}, {"http://localhost:1234/login", false, false}, {"javascript:alert(1)", true, false}, {"file:///tmp/token", true, false}, {"https://user:password@example.test/login", true, false}, {"http://example.test/login", true, false},
	} {
		if validLoginURL(tt.url, tt.local) != tt.valid {
			t.Errorf("invalid decision: %+v", tt)
		}
	}
}
