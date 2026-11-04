package console

import (
	"context"
	"encoding/json"
	"fmt"
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

func fixture(t *testing.T) (*App, chan func(), chan string) {
	t.Helper()
	requests := make(chan string, 30)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer native-test" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method != "GET" {
			var body any
			json.NewDecoder(r.Body).Decode(&body)
			data, _ := json.Marshal(body)
			requests <- r.Method + " " + r.URL.Path + " " + string(data)
		}
		switch r.URL.Path {
		case "/admin/config/status":
			w.Write([]byte(`{"generation":7,"last_applied_at":"2026-10-22T17:00:00Z"}`))
		case "/admin/config/cors":
			w.Write([]byte(`{"allowed_origins":["https://trusted.example"]}`))
		case "/admin/fs/dirs":
			if r.URL.Query().Get("path") == "/tmp/root/sub" {
				w.Write([]byte(`{"path":"/tmp/root/sub","parent":"/tmp/root","dirs":[]}`))
			} else {
				w.Write([]byte(`{"path":"/tmp/root","parent":"/tmp","dirs":["sub"]}`))
			}
		case "/admin/models":
			w.Write([]byte(`{"models":[{"model":"coding","provider":"alpha","providers":["alpha","beta"],"status":"online","capabilities":["text","audio_transcription"],"input_modalities":["text"]}]}`))
		case "/admin/routes":
			w.Write([]byte(`{"routes":[{"model":"coding","providers":[{"name":"alpha"},{"name":"beta"}]}]}`))
		case "/admin/skills/market/browse":
			if r.URL.Query().Get("q") != "" || r.URL.Query().Get("section") == "3" {
				w.Write([]byte(`{"enabled":true,"skills":[],"total":0,"page":1}`))
				return
			}
			w.Write([]byte(`{"enabled":true,"skills":[` +
				`{"id":5,"name":"market-demo","display_name":"市场示例","description":"来自市场的演示技能","version":"1.0.0","install_count":9,"tarball_size":2048},` +
				`{"id":6,"name":"installed-demo","display_name":"已安装示例","description":"已经装好的技能","version":"0.9.0","install_count":4,"tarball_size":1024}` +
				`],"total":2,"page":1}`))
		case "/admin/skills/market/sections":
			w.Write([]byte(`{"enabled":true,"sections":[{"id":3,"name":"开发工具"},{"id":4,"name":"演示与设计"}]}`))
		case "/admin/skills/market/install-stream":
			w.Header().Set("Content-Type", "text/event-stream")
			if f, ok := w.(http.Flusher); ok {
				fmt.Fprintln(w, `data: {"phase":"resolving"}`)
				f.Flush()
				fmt.Fprintln(w, `data: {"phase":"downloading","bytes_done":1024,"bytes_total":2048}`)
				f.Flush()
				fmt.Fprintln(w, `data: {"phase":"done","name":"market-demo","display_name":"市场示例"}`)
				f.Flush()
			}
		case "/admin/skills/market/install-jobs":
			w.Write([]byte(`{"enabled":true,"job_id":"ij-1"}`))
		case "/admin/skills/market/installed":
			w.Write([]byte(`{"enabled":true,"skills_dir":"/tmp/skills","installed":[{"name":"installed-demo","display_name":"已安装示例","version":"0.9.0"}]}`))
		case "/admin/pi":
			w.Write([]byte(`{"path":"/Users/x/.easyagent/models.json","file_exists":true,"in_sync":true,
				"desired":[{"id":"glm-4.7"},{"id":"gpt-6.1-sol"}],"current_ids":["glm-4.7"]}`))
		case "/admin/pi/sync":
			w.Write([]byte(`{"path":"/Users/x/.easyagent/models.json","file_exists":true,"in_sync":true,
				"desired":[{"id":"glm-4.7"},{"id":"gpt-6.1-sol"}],"current_ids":["glm-4.7"]}`))
		case "/admin/ccswitch":
			if r.URL.Query().Get("home") == "__fail__" {
				w.WriteHeader(400)
				w.Write([]byte(`{"error":"未找到 cc-switch 配置"}`))
				return
			}
			w.Write([]byte(`{"home":"/Users/x/.cc-switch","source":"/Users/x/.cc-switch/cc-switch.db","importable":2,"skipped":1,"candidates":[` +
				`{"cc_name":"MyGLM","name":"myglm","app_type":"claude","type":"anthropic","url":"https://glmx.example.com/v1/messages","api_key_env":"CCS_MYGLM_API_KEY","models":["glm-4.7"],"status":"importable"},` +
				`{"cc_name":"Qwen","name":"qwen","app_type":"openclaw","type":"openai","url":"https://relay.example.com/v1/chat/completions","api_key_env":"CCS_QWEN_API_KEY","models":["qwen3.5-plus"],"status":"importable"},` +
				`{"cc_name":"ChatGPT","name":"chatgpt","app_type":"codex","status":"skipped","reason":"OAuth 订阅凭据"}` +
				`]}`))
		case "/admin/providers/glm-4.7/health-check":
			w.Write([]byte(`{"healthy":true,"probe_status":"online","detail":"200 OK","latency_ms":42.5}`))
		case "/admin/ccswitch/remote-scan":
			w.Write([]byte(`{"importable":2,"skipped":1,"uploaded":true,"source":"cc-switch config.json（桌面端上传）","candidates":[` +
				`{"cc_name":"MyGLM","name":"myglm","app_type":"claude","type":"anthropic","url":"https://glmx.example.com/v1/messages","api_key_env":"CCS_MYGLM_API_KEY","models":["glm-4.7"],"status":"importable"},` +
				`{"cc_name":"Loop Relay","name":"loop-relay","app_type":"claude","type":"anthropic","url":"http://localhost:3000/v1/messages","api_key_env":"CCS_LOOP_RELAY_API_KEY","models":["loop-model"],"loopback":true,"status":"importable"},` +
				`{"cc_name":"ChatGPT","name":"chatgpt","app_type":"codex","status":"skipped","reason":"OAuth 订阅凭据"}` +
				`]}`))
		case "/admin/ccswitch/remote-import":
			w.Write([]byte(`{"source":"cc-switch config.json（桌面端上传）","wrote":true,"imported":["myglm"],"providers_yaml":"providers.yaml","env_file":".env","candidates":[` +
				`{"cc_name":"MyGLM","name":"myglm","app_type":"claude","status":"skipped","reason":"端点已存在于 providers.yaml，无需重复导入"},` +
				`{"cc_name":"Loop Relay","name":"loop-relay","app_type":"claude","type":"anthropic","url":"http://localhost:3000/v1/messages","api_key_env":"CCS_LOOP_RELAY_API_KEY","models":["loop-model"],"loopback":true,"status":"importable"},` +
				`{"cc_name":"ChatGPT","name":"chatgpt","app_type":"codex","status":"skipped","reason":"OAuth 订阅凭据"}` +
				`]}`))
		case "/admin/ccswitch/import":
			w.Write([]byte(`{"source":"/Users/x/.cc-switch/cc-switch.db","wrote":true,"imported":["myglm"],"providers_yaml":"providers.yaml","env_file":".env","candidates":[` +
				`{"cc_name":"MyGLM","name":"myglm","app_type":"claude","status":"skipped","reason":"端点已存在于 providers.yaml，无需重复导入"},` +
				`{"cc_name":"Qwen","name":"qwen","app_type":"openclaw","type":"openai","url":"https://relay.example.com/v1/chat/completions","api_key_env":"CCS_QWEN_API_KEY","models":["qwen3.5-plus"],"status":"importable"},` +
				`{"cc_name":"ChatGPT","name":"chatgpt","app_type":"codex","status":"skipped","reason":"OAuth 订阅凭据"}` +
				`]}`))
		case "/healthz":
			w.Write([]byte(`{"status":"ok"}`))
		default:
			if strings.HasPrefix(r.URL.Path, "/admin/skills/market/install-jobs/") {
				w.Write([]byte(`{"enabled":true,"job":{"phase":"done","display_name":"市场示例"}}`))
				return
			}
			w.Write([]byte(`{"status":"ok","summary":{"today_requests":24,"success_rate":98.5},"providers":[],"models":[]}`))
		}
	}))
	t.Cleanup(srv.Close)
	store, err := connection.New(filepath.Join(t.TempDir(), "connections.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(context.Background(), connection.Input{Name: "测试网关", URL: srv.URL, Token: "native-test"}); err != nil {
		t.Fatal(err)
	}
	a := New(store)
	queue := make(chan func(), 100)
	a.SetDispatcher(func(fn func()) { queue <- fn })
	t.Cleanup(a.Close)
	return a, queue, requests
}

// drainDispatcher 执行 dispatcher 队列中待处理的 UI 回调（如 sheet 完成
// 回调经 dispatch 回填的字段赋值）。
func drainDispatcher(a *App, queue chan func()) {
	for {
		select {
		case fn := <-queue:
			fn()
		default:
			return
		}
	}
}

func await(t *testing.T, a *App, queue chan func()) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for a.busy != "" {
		select {
		case fn := <-queue:
			fn()
		case <-deadline:
			t.Fatal("UI operation timed out")
		}
	}
	if a.problem != "" {
		t.Fatal(a.problem)
	}
}
func click(t *testing.T, tt *ui.Tester, text string) {
	t.Helper()
	if err := tt.Click(text); err != nil {
		t.Fatal(err)
	}
}
func TestLateCompletionCannotRestoreOldHost(t *testing.T) {
	a, queue, _ := fixture(t)
	released := make(chan struct{})
	started := make(chan struct{})
	a.launch("slow", time.Second, func(context.Context) (func(), error) {
		close(started)
		<-released
		return func() { a.chatReply = "old host" }, nil
	})
	<-started
	a.stop()
	a.resetHost()
	close(released)
	select {
	case fn := <-queue:
		fn()
	case <-time.After(time.Second):
		t.Fatal("no completion")
	}
	if a.chatReply != "" || a.busy != "" {
		t.Fatal("stale host response applied")
	}
}

// seedSkills 配置一个可用的技能库，供技能页交互测试使用。
func seedSkills(a *App) {
	a.skills = Skills{Configured: true, Targets: []string{"/tmp/skills"}, Skills: []Skill{
		{ID: "review", Name: "代码评审", Description: "检查实现与边界", Author: "hwj", Tags: []string{"开发工具"}},
		{ID: "slides", Name: "演示制作", Description: "生成演示幻灯片", Tags: []string{"演示与设计"}},
	}, Manifest: "/tmp/manifest.json"}
	a.acceptSkills(a.skills)
}

func TestSkillsSaveSendsConfigThenSync(t *testing.T) {
	a, queue, requests := fixture(t)
	a.page = "skills"
	a.loaded = true
	seedSkills(a)
	tt := ui.NewTester(a.View, 1100, 800)
	click(t, tt, "代码评审") // 勾选启用
	if !a.skillDirty {
		t.Fatal("selection change should mark the manifest dirty")
	}
	click(t, tt, "保存并同步")
	await(t, a, queue)
	config, sync := <-requests, <-requests
	if !strings.Contains(config, "PUT /admin/skills/config") || !strings.Contains(config, `"enabled":["review"]`) {
		t.Fatal("config PUT missing or wrong:", config)
	}
	if !strings.Contains(sync, "POST /admin/skills/sync") {
		t.Fatal("sync POST missing:", sync)
	}
	if a.skillDirty {
		t.Fatal("successful save should clear the dirty flag")
	}
}

func TestSkillsMarketUninstallRequiresConfirm(t *testing.T) {
	a, queue, requests := fixture(t)
	a.page = "skills"
	a.loaded = true
	a.skillSource = skillSourceMarket
	a.reload()
	await(t, a, queue)
	tt := ui.NewTester(a.View, 1100, 800)
	click(t, tt, "卸载技能 已安装示例")
	tt.Frame()
	if !tt.HasText("从本机卸载") {
		t.Fatal("uninstall confirmation dialog missing")
	}
	click(t, tt, "移除")
	await(t, a, queue)
	select {
	case got := <-requests:
		if !strings.Contains(got, "POST /admin/skills/market/uninstall") || !strings.Contains(got, `"name":"installed-demo"`) {
			t.Fatal("uninstall request wrong:", got)
		}
	default:
		t.Fatal("uninstall request not sent")
	}
}

func TestDirtySkillsPreventScopeAndHostNavigation(t *testing.T) {
	a, _, _ := fixture(t)
	a.page = "skills"
	a.skillDirty = true
	a.loadedProject = "/old/project"
	tt := ui.NewTester(a.View, 1000, 800)
	a.navigate("connections")
	if a.page != "skills" || a.loadedProject != "/old/project" || a.problem == "" {
		t.Fatal("unsaved skill selection lost")
	}
	tt.Frame()
	if !tt.HasText("技能选择尚未保存") {
		t.Fatal("missing unsaved selection guidance")
	}
}

func TestAssistantUsesAuthoritativeFinalReply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/admin/health" {
			w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"type\":\"text_delta\",\"content\":\"partial\"}\n\ndata: {\"type\":\"done\",\"content\":\"complete reply\"}\n\n"))
	}))
	defer srv.Close()
	s, _ := connection.New(filepath.Join(t.TempDir(), "connections.json"))
	if _, err := s.Save(context.Background(), connection.Input{Name: "Stream", URL: srv.URL, Token: "test-token"}); err != nil {
		t.Fatal(err)
	}
	a := New(s)
	queue := make(chan func(), 20)
	a.SetDispatcher(func(fn func()) { queue <- fn })
	defer a.Close()
	a.page = "assistant"
	a.chatInput = "hello"
	a.sendChat()
	await(t, a, queue)
	if a.chatReply != "complete reply" || a.chatStatus != "回复完成" {
		t.Fatal(a.chatReply, a.chatStatus)
	}
}

func TestPrefsApplyAndRejectInvalid(t *testing.T) {
	dir := t.TempDir()
	store, err := connection.New(filepath.Join(dir, "connections.json"))
	if err != nil {
		t.Fatal(err)
	}
	store.SavePrefs(connection.Prefs{
		ChatModel:     "glm-5.3-flash",
		SkillSource:   skillSourceMarket,
		ModelCategory: "向量",
		MarketSection: "开发工具",
	})
	a := New(store)
	a.applyPrefs()
	if a.chatModel != "glm-5.3-flash" || a.skillSource != skillSourceMarket || a.modelCategory != "向量" || a.marketSection != "开发工具" {
		t.Fatalf("prefs not applied: %+v", a)
	}
	// 非法值回退默认；未设置的不覆盖。
	store.SavePrefs(connection.Prefs{SkillSource: "bogus", ModelCategory: "bogus"})
	b := New(store)
	b.applyPrefs()
	if b.skillSource != skillSourceRepo || b.modelCategory != modelCategoryAll {
		t.Fatalf("invalid prefs should fall back: %q %q", b.skillSource, b.modelCategory)
	}
}

func TestAssistantModelPickerSearchesChannelWithoutRenamingModel(t *testing.T) {
	a, _, _ := fixture(t)
	a.page = "assistant"
	a.loaded = true
	a.chatModels = []string{"mimo-model", "other-model"}
	a.chatModelOptions = map[string]ChatModelOption{"mimo-model": {ID: "mimo-model", Provider: "bound-provider", Channel: "cline"}}
	a.modelPickerOpen = true
	tt := ui.NewTester(a.View, 1280, 850)
	for _, query := range []string{"CLINE", "bound-provider"} {
		a.modelPickerFilter = query
		tt.Frame()
		if !tt.HasText("mimo-model") || tt.HasText("other-model") {
			t.Fatal("metadata filter failed", query)
		}
	}
	click(t, tt, "选择模型 mimo-model")
	tt.Frame()
	if a.chatModel != "mimo-model" || a.modelPickerOpen {
		t.Fatal("model ID changed")
	}
}

func TestCCSwitchMigrationPanelImportsSelection(t *testing.T) {
	a, queue, requests := fixture(t)
	a.page = "settings"
	a.reload()
	await(t, a, queue)
	tt := ui.NewTester(a.View, 1100, 1500)

	if !tt.HasText("从 cc-switch 迁移") {
		t.Fatal("迁移面板未渲染")
	}
	if !tt.HasText("2 可导入 · 1 跳过") {
		t.Fatal("计数 chip 未渲染")
	}
	if !tt.HasText("OAuth 订阅凭据") {
		t.Fatal("跳过原因未渲染")
	}
	if !tt.HasText("CCS_MYGLM_API_KEY") {
		t.Fatal("密钥变量名未渲染")
	}

	// 清空后只勾 MyGLM，验证请求只包含所选条目。
	click(t, tt, "清空")
	click(t, tt, "MyGLM")
	click(t, tt, "迁移所选（1）")
	await(t, a, queue)
	tt.Frame()
	select {
	case got := <-requests:
		if got != `POST /admin/ccswitch/import {"include":["myglm"]}` {
			t.Fatalf("import request = %s", got)
		}
	default:
		t.Fatal("导入请求未发送")
	}
	if !tt.HasText("本次已导入：myglm") {
		t.Fatal("导入结果未渲染")
	}
	if !tt.HasText("已导入") {
		t.Fatal("已导入条目未渲染")
	}

	// 导入后体检：对导入供应商的模型实例发起健康检查并回显在线状态。
	click(t, tt, "体检导入供应商")
	await(t, a, queue)
	tt.Frame()
	select {
	case got := <-requests:
		if !strings.HasPrefix(got, "POST /admin/providers/glm-4.7/health-check") {
			t.Fatalf("health check request = %s", got)
		}
	default:
		t.Fatal("health check not sent")
	}
	if !tt.HasText("在线") {
		t.Fatal("体检结果未渲染")
	}

	// 一键同步客户端：按各目标当前选择分发模型。
	click(t, tt, "一键同步客户端")
	await(t, a, queue)
	tt.Frame()
	select {
	case got := <-requests:
		if got != `POST /admin/pi/sync {"model_ids":["glm-4.7"]}` {
			t.Fatalf("sync-all request = %s", got)
		}
	default:
		t.Fatal("sync-all not sent")
	}
}

func TestCCSwitchRemoteUploadFlow(t *testing.T) {
	a, queue, requests := fixture(t)
	// 模拟远程网关：主机扫描失败 + 本机 cc-switch 目录指向测试夹具。
	a.ccHomeDraft = "__fail__"
	localHome := t.TempDir()
	localLegacy := `{"apps":{"claude":{"current":"p1","providers":{"p1":{"id":"p1","name":"MyGLM",
		"settingsConfig":{"env":{"ANTHROPIC_BASE_URL":"https://glmx.example.com","ANTHROPIC_AUTH_TOKEN":"sk-local","ANTHROPIC_MODEL":"glm-4.7"}},"meta":{}}}}}}`
	if err := os.WriteFile(filepath.Join(localHome, "config.json"), []byte(localLegacy), 0o600); err != nil {
		t.Fatal(err)
	}
	a.ccLocalHome = localHome
	a.page = "settings"
	a.reload()
	await(t, a, queue)
	tt := ui.NewTester(a.View, 1100, 1500)

	if !tt.HasText("未就绪") {
		t.Fatal("主机扫描失败状态未渲染")
	}
	click(t, tt, "从本机 cc-switch 上传")
	await(t, a, queue)
	tt.Frame()

	select {
	case got := <-requests:
		if !strings.HasPrefix(got, "POST /admin/ccswitch/remote-scan") || !strings.Contains(got, "legacy_json") || !strings.Contains(got, "settingsConfig") {
			t.Fatalf("remote scan request = %s", got)
		}
	default:
		t.Fatal("remote scan not sent")
	}
	if !tt.HasText("本机上传") || !tt.HasText("回环地址") {
		t.Fatal("远程模式标识或回环标记未渲染")
	}

	// 取消回环条目，只导 MyGLM。
	click(t, tt, "Loop Relay")
	click(t, tt, "迁移所选（1）")
	await(t, a, queue)
	tt.Frame()
	select {
	case got := <-requests:
		if !strings.HasPrefix(got, "POST /admin/ccswitch/remote-import") || !strings.Contains(got, `"include":["myglm"]`) {
			t.Fatalf("remote import request = %s", got)
		}
	default:
		t.Fatal("remote import not sent")
	}
	if !tt.HasText("本次已导入：myglm") {
		t.Fatal("远程导入结果未渲染")
	}
}
