package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/nightgopher/easygateway/desktop/internal/connection"
)

func TestOverviewPublishesChannelsOnlyOnCurrentUIOperation(t *testing.T) {
	for _, mode := range []string{"current", "stale", "channels-unavailable", "dashboard-unavailable"} {
		t.Run(mode, func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/admin/dashboard":
					if mode == "dashboard-unavailable" {
						w.WriteHeader(http.StatusServiceUnavailable)
					}
					_, _ = w.Write([]byte(`{"summary":{"today_requests":12}}`))
				case "/admin/channels":
					close(started)
					<-release
					if mode == "channels-unavailable" {
						w.WriteHeader(http.StatusServiceUnavailable)
					}
					_, _ = w.Write([]byte(`{"channels":[{"channel":"new-host","accounts":[{"expired":true}]}]}`))
				default:
					_, _ = w.Write([]byte(`{"status":"ok"}`))
				}
			}))
			defer srv.Close()
			store, err := connection.New(filepath.Join(t.TempDir(), "connections.json"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Save(context.Background(), connection.Input{Name: "mock", URL: srv.URL, Token: "test-token"}); err != nil {
				t.Fatal(err)
			}
			a := New(store)
			queue := make(chan func(), 1)
			a.SetDispatcher(func(fn func()) { queue <- fn })
			defer a.Close()
			a.page = "overview"
			a.channels = []AccountChannel{{ID: "previous"}}
			a.reload()
			<-started
			close(release)
			deadline := time.After(3 * time.Second)
			var apply func()
		wait:
			for {
				select {
				case apply = <-queue:
					break wait
				case <-deadline:
					t.Fatal("overview request did not complete")
				default:
					// 模拟网络返回期间原生 UI 的概览渲染；race 检查覆盖后台写入。
					a.channelExpiryCounts()
					runtime.Gosched()
				}
			}
			if len(a.channels) != 1 || a.channels[0].ID != "previous" {
				t.Errorf("background request mutated UI state before dispatch: %+v", a.channels)
			}
			if mode == "stale" {
				a.stop()
				a.resetHost()
			}
			apply()
			switch mode {
			case "current":
				if len(a.channels) != 1 || a.channels[0].ID != "new-host" || !a.loaded || a.dashboard.Summary.TodayRequests != 12 {
					t.Fatalf("current operation did not publish overview: %+v", a.channels)
				}
			case "stale":
				if len(a.channels) != 0 || a.loaded {
					t.Fatal("late completion restored an old host")
				}
			case "channels-unavailable":
				if len(a.channels) != 1 || a.channels[0].ID != "previous" || !a.loaded || a.problem != "" {
					t.Fatal("optional channel failure changed existing channels or blocked overview")
				}
			case "dashboard-unavailable":
				if len(a.channels) != 1 || a.channels[0].ID != "previous" || a.loaded || a.problem == "" {
					t.Fatal("failed overview published partial state")
				}
			}
		})
	}
}
