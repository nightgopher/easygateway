package console

import (
	"context"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/nightgopher/easygateway/desktop/internal/connection"
)

func TestGatewayOriginsNativeEditingUndoCancelAndSave(t *testing.T) {
	a, q, requests := fixture(t)
	a.page = "settings"
	a.reload()
	await(t, a, q)
	if a.gatewayConfig.Generation != 7 || a.corsDraft != "https://trusted.example" {
		t.Fatalf("config not loaded: %+v %s", a.gatewayConfig, a.corsDraft)
	}
	tt := ui.NewTester(a.gatewaySettingsView, 900, 600)
	click(t, tt, "允许的跨域来源")
	tt.Key(ui.Cmd, ui.KeyA)
	tt.Type("https://new.example\nhttp://localhost:5173")
	if a.corsDraft != "https://new.example\nhttp://localhost:5173" {
		t.Fatal("multiline input lost", a.corsDraft)
	}
	tt.Key(ui.Cmd, ui.KeyZ)
	if a.corsDraft != "https://trusted.example" {
		t.Fatal("native undo lost original origins", a.corsDraft)
	}
	tt.Key(ui.Cmd, ui.KeyA)
	tt.Type("https://discard.example")
	click(t, tt, "撤销来源修改")
	if a.corsDraft != "https://trusted.example" {
		t.Fatal("cancel did not restore origins")
	}
	select {
	case req := <-requests:
		t.Fatal("cancel sent request", req)
	default:
	}
	click(t, tt, "允许的跨域来源")
	tt.Key(ui.Cmd, ui.KeyA)
	tt.Type("https://new.example\nhttp://localhost:5173")
	click(t, tt, "保存跨域来源")
	await(t, a, q)
	select {
	case req := <-requests:
		if !strings.Contains(req, `PUT /admin/config/cors {"allowed_origins":["https://new.example","http://localhost:5173"]}`) {
			t.Fatal(req)
		}
	case <-time.After(time.Second):
		t.Fatal("save did not send request")
	}
	if a.corsDraft != "https://trusted.example" || a.notice != "跨域来源已保存并热加载" {
		t.Fatal("save result not applied")
	}
	tt.Frame()
	if dir := os.Getenv("GATEWAY_QA_SCREENSHOTS"); dir != "" {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		f, err := os.Create(filepath.Join(dir, "gateway-origins.png"))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := png.Encode(f, tt.Image()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGatewayOriginsHostSwitchCancelsAndDiscardsOldSave(t *testing.T) {
	started := make(chan struct{}, 1)
	canceled := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			io.Copy(io.Discard, r.Body)
			started <- struct{}{}
			select {
			case <-r.Context().Done():
				canceled <- struct{}{}
			case <-release:
			}
			return
		}
		w.Write([]byte(`{"generation":99}`))
	}))
	defer server.Close()
	defer close(release)
	store, err := connection.New(filepath.Join(t.TempDir(), "connections.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(context.Background(), connection.Input{Name: "old", URL: server.URL, Token: "test"}); err != nil {
		t.Fatal(err)
	}
	a := New(store)
	q := make(chan func(), 10)
	a.SetDispatcher(func(fn func()) { q <- fn })
	defer a.Close()
	a.corsDraft = "https://old-host.example"
	a.saveCORS()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("save did not start")
	}
	a.stop()
	a.resetHost()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("host switch did not cancel old request")
	}
	select {
	case fn := <-q:
		fn()
	case <-time.After(time.Second):
		t.Fatal("old completion not delivered")
	}
	if a.corsDraft != "" || a.gatewayConfig.Generation != 0 || len(a.corsConfig.AllowedOrigins) > 0 {
		t.Fatal("old save contaminated new host")
	}
}

func TestModelHealthRendersReasonAndProbeExpiry(t *testing.T) {
	a, _, _ := fixture(t)
	a.page, a.loaded = "models", true
	a.models = []Model{{Name: "jev-1.13-free", Provider: "opencode-systemone-free", Status: "offline", Capabilities: []string{"systemone"}, Health: ModelHealth{Reason: "上游免费额度耗尽", LastProbeAt: "2026-10-22T17:00:00Z", ProbeExpiresAt: "2026-10-22T17:15:00Z"}}}
	tt := ui.NewTester(a.View, 1100, 800)
	if !tt.HasText("离线") || !tt.HasText("上游免费额度耗尽") || !tt.HasText("探活时间 2026-10-22T17:00:00Z · 有效至 2026-10-22T17:15:00Z") {
		t.Fatal("health evidence not rendered")
	}
}

func TestSystemOneCategoryExcludesTextModels(t *testing.T) {
	a, _, _ := fixture(t)
	a.page, a.loaded, a.modelCategory = "models", true, "结构化判断"
	a.models = []Model{{Name: "jev-1.13-free", Provider: "opencode-systemone-free", Capabilities: []string{"systemone"}}, {Name: "chat-model", Capabilities: []string{"text", "streaming"}}}
	tt := ui.NewTester(a.View, 1100, 800)
	if !tt.HasText("jev-1.13-free") || tt.HasText("chat-model") {
		t.Fatal("native category mixed decision and chat models")
	}
	if modelInCategory(a.models[0], "文本") || !modelInCategory(a.models[0], "结构化判断") {
		t.Fatal("System One should remain separate from text generation")
	}
}
