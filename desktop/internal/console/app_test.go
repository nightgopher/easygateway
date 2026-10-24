package console

import (
	"context"
	"encoding/json"
	"fmt"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/nightgopher/litellm-gateway/desktop/internal/connection"
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
func TestNativeModelEditorSendsCapabilitiesAndRoute(t *testing.T) {
	a, queue, requests := fixture(t)
	a.page = "models"
	a.reload()
	await(t, a, queue)
	tt := ui.NewTester(a.View, 1100, 800)
	click(t, tt, "coding 能力设置")
	click(t, tt, "vision")
	click(t, tt, "保存")
	await(t, a, queue)
	tt.Frame()
	select {
	case got := <-requests:
		if !strings.Contains(got, `PUT /admin/models/coding {"capabilities":["audio_transcription","text","vision"],"input_modalities":["text"]}`) {
			t.Fatal(got)
		}
	default:
		t.Fatal("model save not sent")
	}
	click(t, tt, "coding 路由顺序")
	click(t, tt, "下移 alpha") // [alpha, beta] → [beta, alpha]
	tt.Frame()
	click(t, tt, "保存")
	await(t, a, queue)
	select {
	case got := <-requests:
		if !strings.Contains(got, `PUT /admin/routes/coding {"providers":["beta","alpha"]}`) {
			t.Fatal(got)
		}
	default:
		t.Fatal("route save not sent")
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
func TestNativeViewsAndVirtualLists(t *testing.T) {
	a, _, _ := fixture(t)
	a.loaded = true
	a.dashboard = Dashboard{Summary: Summary{TodayRequests: 1250, SuccessRate: 99.5, Uptime: "2h", ActiveModels: 8}, Providers: []Provider{{Name: "alpha", Status: "online", Requests: 1250}}}
	a.providers = []Provider{{Name: "alpha", State: "closed", Status: "online", HasProbe: true, ProbeDetail: "上游可用"}}
	for i := 0; i < 1000; i++ {
		a.models = append(a.models, Model{Name: strings.Repeat("m", 12) + shortNumber(int64(i)), Provider: "alpha", Status: "online", Capabilities: []string{"text"}})
	}
	a.skills = Skills{Configured: true, Targets: []string{"/tmp/skills"}, Skills: []Skill{{ID: "test-skill", Name: "代码评审", Description: "检查实现与边界"}}, Manifest: "/tmp/manifest.json"}
	a.acceptSkills(a.skills)
	a.memories = []Memory{{ID: 1, Scope: "global", Status: "candidate", Statement: "偏好原生 Go 界面"}}
	a.logs = []Log{{Timestamp: "2026-10-10 12:00", Model: "coding", Provider: "alpha", RequestID: "test", Code: 200}}
	for _, target := range []string{"pi", "zcode", "harness"} {
		a.configs[target] = ClientConfig{Path: "/tmp/" + target + ".json", Desired: []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}{{ID: "coding"}}}
	}
	a.initConfigSelection()
	for _, page := range pages {
		a.page = page.ID
		for _, size := range []struct{ w, h int }{{1280, 850}, {900, 680}} {
			tt := ui.NewTester(a.View, size.w, size.h)
			for _, dark := range []bool{false, true} {
				tt.SetDark(dark)
				if !tt.HasText(page.Title) {
					t.Fatal("missing page", page.ID)
				}
				if page.ID == "models" && len(tt.Texts()) > 200 {
					t.Fatal("list not virtualized", len(tt.Texts()))
				}
				if dir := os.Getenv("GATEWAY_QA_SCREENSHOTS"); dir != "" && size.w == 1280 {
					os.MkdirAll(dir, 0700)
					name := page.ID + "-light.png"
					if dark {
						name = page.ID + "-dark.png"
					}
					f, err := os.Create(filepath.Join(dir, name))
					if err != nil {
						t.Fatal(err)
					}
					if err = png.Encode(f, tt.Image()); err != nil {
						t.Fatal(err)
					}
					f.Close()
				}
			}
		}
	}
}

func TestSkillsProjectBrowserPicksHostDirectory(t *testing.T) {
	a, queue, _ := fixture(t)
	a.page = "skills"
	a.loaded = true
	seedSkills(a)
	tt := ui.NewTester(a.View, 1100, 800)
	click(t, tt, "展开安装范围设置")
	click(t, tt, "浏览项目路径")
	await(t, a, queue)
	tt.Frame()
	if a.browserPath != "/tmp/root" || !a.browserOpen {
		t.Fatal("browser did not open at gateway home:", a.browserPath)
	}
	click(t, tt, "打开 sub")
	await(t, a, queue)
	tt.Frame()
	click(t, tt, "选择此目录")
	if a.browserOpen || a.project != "/tmp/root/sub" {
		t.Fatal("directory not picked:", a.project, a.browserOpen)
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

func TestLocalNativePickFillsProjectPath(t *testing.T) {
	a, queue, _ := fixture(t)
	a.page = "skills"
	a.loaded = true
	seedSkills(a)
	if !a.localConnection() {
		t.Fatal("fixture connection should count as local")
	}
	previous := a.PickFolder
	a.PickFolder = func(done func(path string, ok bool)) { done("/Users/demo/project", true) }
	defer func() { a.PickFolder = previous }()
	tt := ui.NewTester(a.View, 1100, 800)
	click(t, tt, "展开安装范围设置")
	click(t, tt, "本机选择目录")
	drainDispatcher(a, queue)
	if a.project != "/Users/demo/project" {
		t.Fatal("native pick not applied:", a.project)
	}
	a.PickFolder = func(done func(path string, ok bool)) { done("", false) }
	click(t, tt, "本机选择目录")
	drainDispatcher(a, queue)
	if a.project != "/Users/demo/project" {
		t.Fatal("cancel must keep the old value:", a.project)
	}
}

func TestSkillsSearchAndTagFilter(t *testing.T) {
	a, _, _ := fixture(t)
	a.page = "skills"
	a.loaded = true
	seedSkills(a)
	tt := ui.NewTester(a.View, 1100, 800)
	a.skillFilter = "评审"
	tt.Frame()
	if !tt.HasText("代码评审") {
		t.Fatal("search filter hid the matching skill")
	}
	if tt.HasText("演示制作") {
		t.Fatal("search filter did not hide unmatched skill")
	}
	a.skillFilter = ""
	a.skillTag = "演示与设计"
	tt.Frame()
	if !tt.HasText("演示制作") {
		t.Fatal("tag filter hid the matching skill")
	}
	if tt.HasText("代码评审") {
		t.Fatal("tag filter did not hide untagged skill")
	}
}

func TestSkillsRowDetailExpansion(t *testing.T) {
	a, _, _ := fixture(t)
	a.page = "skills"
	a.loaded = true
	seedSkills(a)
	tt := ui.NewTester(a.View, 1100, 800)
	if tt.HasText("作者 hwj") {
		t.Fatal("detail metadata should stay hidden before expansion")
	}
	click(t, tt, "技能详情 review")
	tt.Frame()
	if !tt.HasText("作者 hwj") {
		t.Fatal("expanded row should show skill metadata")
	}
	click(t, tt, "技能详情 review")
	tt.Frame()
	if tt.HasText("作者 hwj") {
		t.Fatal("collapsed row should hide skill metadata")
	}
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

func TestSkillsMarketBrowseAndInstall(t *testing.T) {
	a, queue, requests := fixture(t)
	a.page = "skills"
	a.loaded = true
	a.skillSource = skillSourceMarket
	a.reload()
	await(t, a, queue)
	tt := ui.NewTester(a.View, 1100, 800)
	if !tt.HasText("市场示例") || !tt.HasText("已安装示例") {
		t.Fatal("market browse rows missing")
	}
	if !tt.HasText("已安装") {
		t.Fatal("installed chip missing for installed skill")
	}
	click(t, tt, "安装技能 市场示例")
	await(t, a, queue)
	select {
	case got := <-requests:
		if !strings.Contains(got, "POST /admin/skills/market/install") || !strings.Contains(got, `"id":5`) {
			t.Fatal("install request wrong:", got)
		}
	default:
		t.Fatal("install request not sent")
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

func TestSkillsMarketDisabledShowsHint(t *testing.T) {
	a, _, _ := fixture(t)
	a.page = "skills"
	a.loaded = true
	a.skillSource = skillSourceMarket
	a.market = MarketPage{Hint: "技能市场未启用：设置 SKILLS_MARKET_ENABLED 后重启网关"}
	tt := ui.NewTester(a.View, 1100, 800)
	if !tt.HasText("技能市场未启用") {
		t.Fatal("disabled market hint missing")
	}
	if tt.HasText("搜索市场") {
		t.Fatal("market controls should stay hidden when disabled")
	}
}

func TestSkillsMarketSectionFilter(t *testing.T) {
	a, queue, _ := fixture(t)
	a.page = "skills"
	a.loaded = true
	a.skillSource = skillSourceMarket
	a.reload()
	await(t, a, queue)
	tt := ui.NewTester(a.View, 1100, 800)
	// Select 未展开时只渲染当前值；分类选项已装进下拉即表现为选中“全部分类”。
	if !tt.HasText("全部分类") {
		t.Fatal("section select missing")
	}
	a.marketSection = "开发工具" // fixture 对 section=3 返回空清单
	a.reload()
	await(t, a, queue)
	tt.Frame()
	if !tt.HasText("没有匹配的技能") {
		t.Fatal("section filter did not narrow the browse list")
	}
	a.marketSection = marketSectionAll
	a.reload()
	await(t, a, queue)
	tt.Frame()
	if !tt.HasText("市场示例") {
		t.Fatal("resetting the section lost browse results")
	}
}

// 上次载入失败时页面工具保持可用：来源切换可直接自愈，不必先刷新。
func TestFailedLoadKeepsPageToolbar(t *testing.T) {
	a, queue, _ := fixture(t)
	a.page = "skills"
	a.loaded = false
	a.problem = "get /admin/skills: connection refused"
	tt := ui.NewTester(a.View, 1100, 800)
	if !tt.HasText("数据未加载") {
		t.Fatal("retry hint missing on failed load")
	}
	if !tt.HasText("技能来源") || !tt.HasText("技能库") {
		t.Fatal("source switcher should stay available on failure")
	}
	a.skillSource = skillSourceMarket
	a.reload()
	await(t, a, queue)
	tt.Frame()
	if !tt.HasText("市场示例") {
		t.Fatal("switching source should reload data and self-heal")
	}
}

func TestModelPickerSearchFilterAndChoose(t *testing.T) {
	a, _, _ := fixture(t)
	a.page = "assistant"
	a.loaded = true
	for i := 0; i < 20; i++ {
		a.chatModels = append(a.chatModels, fmt.Sprintf("cloudcode/gemini-3.8-flash-%03d", i))
		a.chatModels = append(a.chatModels, fmt.Sprintf("mini-max/m%03d-model", i))
	}
	a.chatModel = chatDefaultOption
	tt := ui.NewTester(a.View, 1280, 850)
	click(t, tt, "选择模型，当前 "+chatDefaultOption)
	tt.Frame()
	if !a.modelPickerOpen {
		t.Fatal("picker should open")
	}
	if !tt.HasText("共 41 个模型，显示 41 个") {
		t.Fatal("picker count missing")
	}
	if !tt.HasText("当前") {
		t.Fatal("current selection chip missing")
	}
	a.modelPickerFilter = "gem"
	tt.Frame()
	if !tt.HasText("cloudcode/gemini-3.8-flash-000") {
		t.Fatal("filter hid matching models")
	}
	if tt.HasText("mini-max/m000-model") {
		t.Fatal("filter kept non-matching models")
	}
	click(t, tt, "选择模型 cloudcode/gemini-3.8-flash-003")
	tt.Frame()
	if a.chatModel != "cloudcode/gemini-3.8-flash-003" || a.modelPickerOpen {
		t.Fatal("choosing did not apply and close:", a.chatModel, a.modelPickerOpen)
	}
	if a.modelPickerFilter != "" {
		t.Fatal("filter should reset after closing")
	}
}

func TestModelPickerEmptyState(t *testing.T) {
	a, _, _ := fixture(t)
	a.page = "assistant"
	a.loaded = true
	a.chatModels = []string{"glm-5.3-flash"}
	a.modelPickerOpen = true
	a.modelPickerFilter = "不存在的模型"
	tt := ui.NewTester(a.View, 1280, 850)
	if !tt.HasText("没有匹配的模型") {
		t.Fatal("empty state missing")
	}
}

func TestModelsCategoryFilter(t *testing.T) {
	a, _, _ := fixture(t)
	a.page = "models"
	a.loaded = true
	a.models = []Model{
		{Name: "chat", Provider: "alpha", Status: "online", Capabilities: []string{"text", "tool_calling"}},
		{Name: "BAAI/bge-m3", Provider: "alpha", Status: "online", Capabilities: []string{"embedding", "text"}},
		{Name: "asr-v3", Provider: "alpha", Status: "online", Capabilities: []string{"audio_transcription", "text"}},
		{Name: "vision-chat", Provider: "beta", Status: "online", Capabilities: []string{"text", "vision", "reasoning"}},
	}
	tt := ui.NewTester(a.View, 1280, 850)
	if !tt.HasText("BAAI/bge-m3") || !tt.HasText("asr-v3") {
		t.Fatal("all models should show without a category")
	}
	a.modelCategory = "向量"
	tt.Frame()
	if !tt.HasText("BAAI/bge-m3") {
		t.Fatal("embedding category hid the embedding model")
	}
	if tt.HasText("asr-v3") || tt.HasText("vision-chat") {
		t.Fatal("embedding category kept non-embedding models")
	}
	a.modelCategory = "语音转写"
	tt.Frame()
	if !tt.HasText("asr-v3") || tt.HasText("BAAI/bge-m3") {
		t.Fatal("transcription category filter wrong")
	}
	a.modelCategory = modelCategoryAll
	tt.Frame()
	if !tt.HasText("chat") || !tt.HasText("vision-chat") {
		t.Fatal("resetting the category lost models")
	}
}

func TestMemoryScopeKeyControls(t *testing.T) {
	a, queue, _ := fixture(t)
	a.page = "memory"
	a.loaded = true
	a.memories = []Memory{{ID: 7, Scope: "client", ScopeKey: "claude-code", Status: "active", Statement: "x"}}
	a.memoryScope = "client"
	tt := ui.NewTester(a.View, 1100, 800)
	tt.Frame()
	click(t, tt, "客户端")
	if !tt.HasText("claude-code") || !tt.HasText("pi") {
		t.Fatal("client suggestions missing")
	}
	a.memoryScope = "project"
	saved := a.PickFolder
	a.PickFolder = func(done func(path string, ok bool)) { done("/tmp/picked-native", true) }
	tt.Frame()
	if !tt.HasText("浏览记忆项目") || !tt.HasText("本机选择记忆项目") {
		t.Fatal("project scope controls missing")
	}
	click(t, tt, "本机选择记忆项目")
	drainDispatcher(a, queue)
	a.PickFolder = saved
	tt.Frame()
	if a.memoryKey != "/tmp/picked-native" {
		t.Fatal("native pick did not fill memory key:", a.memoryKey)
	}
	a.profiles.Profiles[0].URL = "https://gateway.example.com"
	tt.Frame()
	click(t, tt, "浏览记忆项目")
	await(t, a, queue)
	tt.Frame()
	click(t, tt, "选择此目录")
	if a.browserOpen || a.memoryKey != "/tmp/root" {
		t.Fatal("remote browser did not fill memory key:", a.memoryKey, a.browserOpen)
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

func TestClientSyncBadgeTracksEditedSelection(t *testing.T) {
	a, _, _ := fixture(t)
	a.page = "settings"
	a.loaded = true
	for _, target := range []string{"pi", "zcode", "harness"} {
		cfg := ClientConfig{Exists: true, InSync: true, Current: []string{"coding"}}
		cfg.Desired = append(cfg.Desired, struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}{ID: "coding"})
		a.configs[target] = cfg
	}
	a.initConfigSelection()
	tt := ui.NewTester(a.View, 1280, 1200)
	if tt.HasText("待同步") {
		t.Fatal("matching selections should be synced")
	}
	click(t, tt, "清空 EasyAgent")
	tt.Frame()
	if !tt.HasText("待同步") {
		t.Fatal("changed selection still claims to be synced")
	}
	click(t, tt, "全选 EasyAgent")
	tt.Frame()
	if tt.HasText("待同步") {
		t.Fatal("restored selection should match the saved configuration")
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

func TestChannelExpiryMarksRender(t *testing.T) {
	a, _, _ := fixture(t)
	a.page = "channels"
	a.loaded = true
	a.channels = []AccountChannel{{
		ID:       "minimax",
		LoggedIn: true,
		Accounts: []ChannelAccount{
			{AccountID: "a1", Nickname: "过期账号", Expired: true},
			{AccountID: "a2", Nickname: "临期账号", ExpiresAt: time.Now().Add(2 * time.Hour).Format(time.RFC3339)},
		},
	}}
	tt := ui.NewTester(a.View, 1280, 850)
	if !tt.HasText("已失效") || !tt.HasText("即将过期") {
		t.Fatalf("expiry chips missing: %v", tt.Texts())
	}
}

func TestOverviewShowsTokenMetricAndExpiryWarning(t *testing.T) {
	a, _, _ := fixture(t)
	a.loaded = true
	rate := 98.5
	a.dashboard = Dashboard{Summary: Summary{TodayRequests: 24, SuccessRate: rate, TodayTokens: 1523000}}
	a.channels = []AccountChannel{{
		ID:       "minimax",
		LoggedIn: true,
		Accounts: []ChannelAccount{{AccountID: "a1", Expired: true}},
	}}
	tt := ui.NewTester(a.View, 1280, 850)
	if !tt.HasText("今日 Tokens") || !tt.HasText("1.5M") {
		t.Fatal("token metric missing")
	}
	if !tt.HasText("渠道凭据提醒") || !tt.HasText("1 个已失效") {
		t.Fatal("expiry warning missing")
	}
	click(t, tt, "去处理")
	if a.page != "channels" {
		t.Fatal("warning button did not navigate to channels")
	}
}

func TestConnectionSwitcherInTitleBar(t *testing.T) {
	a, queue, _ := fixture(t)
	localURL := a.profiles.Profiles[0].URL
	localID := a.profiles.ActiveID
	if _, err := a.store.Save(context.Background(), connection.Input{Name: "迷你主机", URL: localURL, Token: "native-test"}); err != nil {
		t.Fatal(err)
	}
	a.profiles = a.store.State()
	a.page = "overview"
	a.loaded = true
	if a.profiles.ActiveID == localID {
		t.Fatal("fixture setup: latest save should be active")
	}
	tt := ui.NewTester(a.View, 1280, 850)
	if !tt.HasText("迷你主机") {
		t.Fatal("title bar switcher should show the active connection")
	}
	// 点开下拉并选择另一个连接。
	click(t, tt, "迷你主机")
	tt.Frame()
	click(t, tt, "测试网关")
	await(t, a, queue)
	if a.profiles.ActiveID != localID {
		t.Fatalf("switcher did not change the active connection: %s", a.profiles.ActiveID)
	}
	if a.connSelect != "测试网关" || a.page != "overview" {
		t.Fatalf("switch state wrong: %q page=%s", a.connSelect, a.page)
	}
	await(t, a, queue)
	if !a.loaded {
		t.Fatal("overview should reload for the newly selected gateway")
	}
}

// 有数据时不得渲染“没有匹配的模型”（曾因空态判定缺陷在列表上方误渲染）。
func TestModelsPageNoFalseEmptyState(t *testing.T) {
	a, _, _ := fixture(t)
	a.page = "models"
	a.loaded = true
	a.models = []Model{
		{Name: "chat", Provider: "alpha", Status: "online", Capabilities: []string{"text"}},
		{Name: "BAAI/bge-m3", Provider: "alpha", Status: "online", Capabilities: []string{"embedding", "text"}},
	}
	tt := ui.NewTester(a.View, 1280, 850)
	if tt.HasText("没有匹配的模型") {
		t.Fatalf("models present but empty state rendered: %v", tt.Texts())
	}
	if !tt.HasText("chat") {
		t.Fatal("models missing")
	}
	// 数据为空（无匹配）时空态保留。
	a.modelCategory = "视频"
	tt.Frame()
	if !tt.HasText("没有匹配的模型") {
		t.Fatal("empty state missing for zero matches")
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

func TestOverviewShowsMigrationHint(t *testing.T) {
	a, queue, _ := fixture(t)
	a.page = "overview"
	a.reload()
	await(t, a, queue)
	tt := ui.NewTester(a.View, 1280, 900)
	if !tt.HasText("检测到 cc-switch 配置") || !tt.HasText("2 个 cc-switch 条目可导入") {
		t.Fatal("迁移引导卡未渲染")
	}
	click(t, tt, "去迁移")
	await(t, a, queue)
	tt.Frame()
	if a.page != "settings" || !tt.HasText("从 cc-switch 迁移") {
		t.Fatalf("page = %s", a.page)
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
