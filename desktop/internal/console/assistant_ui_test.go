package console

import (
	"context"
	"fmt"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/nightgopher/easygateway/desktop/internal/connection"
)

const assistantMarkdownFixture = "## 网关状态\n\n**已恢复**，可以继续调用。\n\n| 时间 | 模型 | 问题 |\n|---|---|---|\n| 10:58 | glm-model | DNS 解析超时 |\n| 11:01 | deepseek-model | 请求已取消 |\n\n### 建议\n\n1. 检查本机 DNS。\n2. 重试一个简短请求。\n\n```sh\ncurl http://localhost:4001/healthz\n```\n\n[文档](https://example.com/docs) · [非法链接](javascript:alert(1))"

func TestAssistantNativeMarkupAndPersonalControls(t *testing.T) {
	a, _, _ := fixture(t)
	a.page, a.loaded = "assistant", true
	a.chatUser, a.chatReply, a.chatStatus = "检查今天的网关状态", assistantMarkdownFixture, "回复完成"
	for _, size := range []struct{ w, h int }{{1280, 900}, {900, 640}} {
		for _, dark := range []bool{false, true} {
			tt := ui.NewTester(a.View, size.w, size.h)
			tt.SetDark(dark)
			for _, label := range []string{"反馈备注", "有帮助", "需改进", "最近的助理反馈"} {
				if tt.HasText(label) {
					t.Fatal("consumer feedback still visible", label)
				}
			}
			if !tt.HasText("网关状态") || !tt.HasText("已恢复") || !tt.HasText("DNS 解析超时") || tt.HasText("**已恢复**") {
				t.Fatal("reply is not native rich text")
			}
			click(t, tt, "复制回复")
			if tt.Clipboard() != assistantMarkdownFixture {
				t.Fatal("copy lost reply content")
			}
			if dir := os.Getenv("GATEWAY_QA_SCREENSHOTS"); dir != "" {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				file, err := os.Create(filepath.Join(dir, fmt.Sprintf("assistant-%dx%d-dark-%v.png", size.w, size.h, dark)))
				if err != nil {
					t.Fatal(err)
				}
				if err := png.Encode(file, tt.Image()); err != nil {
					t.Fatal(err)
				}
				file.Close()
			}
		}
	}
}

func TestMarkdownLinksUseSafeNativeActions(t *testing.T) {
	var document markdownDocument
	tt := ui.NewTester(func(c *ui.Context) {
		document.render(c, "阅读 [文档](https://example.com/docs)。\n\n[非法链接](javascript:alert(1))")
	}, 600, 400)
	click(t, tt, "文档")
	if urls := tt.OpenedURLs(); len(urls) != 1 || urls[0] != "https://example.com/docs" {
		t.Fatal("native link did not open", urls)
	}
	click(t, tt, "非法链接")
	if len(tt.OpenedURLs()) != 1 {
		t.Fatal("executable model-generated URL opened")
	}
}

func TestAssistantEditorKeepsCaretUndoCompositionAndCancellation(t *testing.T) {
	started := make(chan struct{}, 1)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/assistant/chat" {
			w.Write([]byte(`{"status":"ok"}`))
			return
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"type":"text_delta","content":"部分回复"}`)
		fmt.Fprintln(w)
		w.(http.Flusher).Flush()
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer srv.Close()
	store, _ := connection.New(filepath.Join(t.TempDir(), "connections.json"))
	if _, err := store.Save(context.Background(), connection.Input{Name: "Stream", URL: srv.URL, Token: "native-test"}); err != nil {
		t.Fatal(err)
	}
	a := New(store)
	q := make(chan func(), 100)
	a.SetDispatcher(func(fn func()) { q <- fn })
	defer a.Close()
	a.page, a.loaded = "assistant", true
	tt := ui.NewTester(a.View, 1100, 800)
	click(t, tt, "助理消息")
	tt.Type("甲乙丙")
	tt.Key(0, ui.KeyLeft)
	tt.Key(0, ui.KeyLeft)
	tt.Key(ui.Shift, ui.KeyEnter)
	if a.chatInput != "甲\n乙丙" {
		t.Fatal("newline ignored caret", a.chatInput)
	}
	tt.Key(ui.Cmd, ui.KeyZ)
	if a.chatInput != "甲乙丙" {
		t.Fatal("newline lost native undo", a.chatInput)
	}
	tt.Compose("候选", 2)
	tt.Key(0, ui.KeyEnter)
	if requests.Load() != 0 || a.busy != "" {
		t.Fatal("IME candidate confirmation sent chat")
	}
	tt.Type("完成") // Commit the composing text.
	tt.Key(0, ui.KeyEnter)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("Enter did not send")
	}
	deadline := time.After(2 * time.Second)
	for a.chatReply == "" {
		select {
		case fn := <-q:
			fn()
		case <-deadline:
			t.Fatal("stream not displayed")
		}
	}
	tt.Frame()
	click(t, tt, "停止回复")
	if a.busy != "" || a.chatStatus != "已停止" || a.chatReply != "部分回复" || requests.Load() != 1 {
		t.Fatal("cancel lost partial reply or sent twice")
	}
}

func TestSettingsDoesNotDependOnFeedbackAPI(t *testing.T) {
	var feedback atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "feedback") {
			feedback.Store(true)
			http.Error(w, "unsupported", 500)
			return
		}
		w.Write([]byte(`{"status":"ok","prompt":"test","custom":"test"}`))
	}))
	defer srv.Close()
	store, _ := connection.New(filepath.Join(t.TempDir(), "connections.json"))
	_, err := store.Save(context.Background(), connection.Input{Name: "settings", URL: srv.URL, Token: "native-test"})
	if err != nil {
		t.Fatal(err)
	}
	a := New(store)
	q := make(chan func(), 100)
	a.SetDispatcher(func(fn func()) { q <- fn })
	defer a.Close()
	a.page = "settings"
	a.reload()
	await(t, a, q)
	if feedback.Load() || !a.loaded || a.problem != "" {
		t.Fatal("settings still requires feedback API")
	}
}
