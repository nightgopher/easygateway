// Package console implements Gateway's desktop UI entirely with MyGo native controls.
package console

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/nightgopher/litellm-gateway/desktop/internal/ccswitchlocal"
	"github.com/nightgopher/litellm-gateway/desktop/internal/connection"
)

type App struct {
	Version                                                               string
	store                                                                 *connection.Store
	profiles                                                              connection.State
	page                                                                  string
	dispatch                                                              func(func())
	sequence                                                              uint64
	cancel                                                                context.CancelFunc
	busy, notice, problem                                                 string
	loaded                                                                bool
	dashboard                                                             Dashboard
	statsHistory                                                          StatsHistory
	statsDays                                                             int
	providers                                                             []Provider
	channels                                                              []AccountChannel
	channelLogin                                                          *ChannelLogin
	channelClient                                                         *connection.Client
	channelList                                                           ui.ListState
	channelSetup                                                          string
	OpenLoginURL                                                          func(string) error
	CopyText                                                              func(string)
	models                                                                []Model
	routes                                                                []Route
	logs                                                                  []Log
	logLimit, logFilter                                                   string
	logSelected                                                           int
	table                                                                 ui.ListState
	logSort                                                               ui.SortOrder
	logSplitHeight                                                        float32
	overviewSort                                                          ui.SortOrder
	overviewTable                                                         ui.ListState
	overviewFilter                                                        string
	lastNotice                                                            string
	listViews                                                             listViewState
	modelFilter, modelCategory                                            string
	skills                                                                Skills
	project, loadedProject, targets                                       string
	skillSelection                                                        map[string]bool
	skillDirty                                                            bool
	skillFilter, skillTag                                                 string
	skillScopeOpen                                                        bool
	skillExpanded                                                         map[string]bool
	skillSource                                                           string
	market                                                                MarketPage
	marketQuery, marketSort, marketSection                                string
	marketSections                                                        []MarketSection
	marketPage                                                            int
	marketInstalled                                                       []MarketInstalled
	marketExpanded                                                        map[string]bool
	modelPickerOpen                                                       bool
	modelPickerFilter                                                     string
	modelPickerList                                                       ui.ListState
	memories                                                              []Memory
	memoryFilter                                                          string
	memoryScope, memoryKey, memoryText                                    string
	prompt                                                                Prompt
	promptDraft                                                           string
	gatewayConfig                                                         GatewayConfigStatus
	gatewayConfigErr                                                      string
	corsConfig                                                            CORSConfig
	corsDraft                                                             string
	promptErr                                                             string
	configs                                                               map[string]ClientConfig
	configSelection                                                       map[string]map[string]bool
	ccswitch                                                              CCSwitchScan
	ccHomeDraft                                                           string
	ccSelection                                                           map[string]bool
	ccImported                                                            []string
	ccUpload                                                              *ccswitchlocal.Payload
	ccRemote                                                              bool
	ccLocalHome                                                           string // 本机 cc-switch 目录（空=默认 ~/.cc-switch，测试可注入）
	ccHint                                                                CCSwitchHint
	ccImportedModels                                                      map[string][]string
	ccHealth                                                              map[string]string
	ccUpdatedKeys                                                         map[string]bool
	chatInput, chatReply, chatUser, chatStatus                            string
	chatMarkdown                                                          markdownDocument
	connSelect                                                            string
	chatModels                                                            []string
	chatModelOptions                                                      map[string]ChatModelOption
	chatModel, chatDefault                                                string
	chatList                                                              ui.ScrollState
	editorOpen                                                            bool
	editorKind, editorTitle, editorID, editorName, editorURL, editorToken string
	editorOrder                                                           []string
	editorCaps                                                            map[string]bool
	editorModalities                                                      string
	deleteOpen                                                            bool
	deleteAction                                                          func()
	browserOpen                                                           bool
	browserKind                                                           string
	PickFolder                                                            func(done func(path string, ok bool))
	browserPath, browserParent                                            string
	browserDirs                                                           []string
}

func New(store *connection.Store) *App {
	a := &App{store: store, profiles: store.State(), page: "connections", logLimit: "50", logSelected: -1,
		logSplitHeight: 380,
		memoryScope:    "global", memoryFilter: "all", skillSelection: map[string]bool{}, skillTag: skillsTagAll,
		modelCategory: modelCategoryAll, marketSection: marketSectionAll,
		skillExpanded: map[string]bool{}, skillSource: skillSourceRepo, marketSort: "推荐", marketPage: 1, statsDays: 7,
		marketExpanded: map[string]bool{}, configs: map[string]ClientConfig{}, configSelection: map[string]map[string]bool{}}
	if a.profiles.ActiveID != "" {
		a.page = "overview"
	}
	return a
}
func (a *App) SetDispatcher(fn func(func())) { a.dispatch = fn }
func (a *App) Close()                        { a.stop(); a.dispatch = nil }
func (a *App) Start() {
	a.applyPrefs()
	if a.page != "connections" {
		a.reload()
	}
}

// applyPrefs 应用本地 UI 偏好；非法值回退默认。
func (a *App) applyPrefs() {
	prefs := a.store.LoadPrefs()
	if prefs.SkillSource == skillSourceRepo || prefs.SkillSource == skillSourceMarket {
		a.skillSource = prefs.SkillSource
	}
	for _, category := range modelCategories {
		if prefs.ModelCategory == category {
			a.modelCategory = prefs.ModelCategory
		}
	}
	if prefs.MarketSection != "" {
		a.marketSection = prefs.MarketSection
	}
	if prefs.ChatModel != "" {
		a.chatModel = prefs.ChatModel
	}
}

// savePrefs 把需要记住的 UI 状态写本地（尽力而为）。
func (a *App) savePrefs() {
	a.store.SavePrefs(connection.Prefs{
		ChatModel:     a.chatModel,
		SkillSource:   a.skillSource,
		ModelCategory: a.modelCategory,
		MarketSection: a.marketSection,
	})
}
func (a *App) ShowConnections() { a.navigate("connections") }
func (a *App) stop() {
	a.cancelChannelSession()
	a.sequence++
	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
	if a.busy == "助理回复中" {
		a.chatStatus = "已停止"
	}
	a.busy = ""
}
func (a *App) active() connection.Profile {
	for _, p := range a.profiles.Profiles {
		if p.ID == a.profiles.ActiveID {
			return p
		}
	}
	return connection.Profile{}
}
func (a *App) navigate(page string) {
	if a.skillDirty && page != "skills" {
		a.problem = "技能选择尚未保存，请先保存或撤销。"
		return
	}
	if page == a.page {
		return
	}
	a.stop()
	a.page = page
	a.problem = ""
	a.notice = ""
	a.loaded = false
	a.table = ui.ListState{}
	a.logSelected = -1
	if page != "connections" {
		a.reload()
	}
}

// Work captures inputs on the UI thread; only the completion mutates UI state.
// Navigation, host switches and closing invalidate/cancel outstanding results.
func (a *App) work(label string, timeout time.Duration, task func(context.Context, *connection.Client) (func(), error)) {
	if a.busy != "" || a.dispatch == nil {
		return
	}
	client, err := a.store.Client()
	if err != nil {
		a.problem = err.Error()
		return
	}
	a.launch(label, timeout, func(ctx context.Context) (func(), error) { return task(ctx, client) })
}
func (a *App) launch(label string, timeout time.Duration, task func(context.Context) (func(), error)) {
	if a.busy != "" || a.dispatch == nil {
		return
	}
	a.sequence++
	seq := a.sequence
	dispatch := a.dispatch
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	a.cancel = cancel
	a.busy = label
	a.problem = ""
	a.notice = ""
	go func() {
		apply, err := task(ctx)
		cancel()
		dispatch(func() {
			if seq != a.sequence {
				return
			}
			a.cancel = nil
			a.busy = ""
			if err != nil {
				if !errors.Is(err, context.Canceled) {
					a.problem = err.Error()
					if label == "助理回复中" {
						a.chatStatus = "回复中断"
					}
				}
				return
			}
			if apply != nil {
				apply()
			}
		})
	}()
}
func (a *App) reload() {
	page, project, filter, limit, statsDays := a.page, a.loadedProject, a.memoryFilter, a.logLimit, a.statsDays
	source, marketQuery, marketSort, marketSection, marketPage := a.skillSource, a.marketQuery, a.marketSort, a.marketSection, a.marketPage
	a.work("正在读取", 35*time.Second, func(ctx context.Context, c *connection.Client) (func(), error) {
		switch page {
		case "overview":
			var d Dashboard
			err := c.JSON(ctx, "GET", "/dashboard", nil, &d)
			var channels struct {
				Channels []AccountChannel `json:"channels"`
			}
			// 渠道清单用于过期提醒；拉取失败不影响概览指标。
			channelsErr := c.JSON(ctx, "GET", "/channels", nil, &channels)
			// 历史趋势独立容错：旧版网关没有 /stats/history 时概览其余部分照常。
			var history StatsHistory
			historyErr := c.JSON(ctx, "GET", fmt.Sprintf("/stats/history?days=%d", statsDays), nil, &history)
			// 迁移引导：网关刚起步（provider 很少）时看看 cc-switch 能不能
			// 带来存量配置——先问网关主机，再退回本机检测（远程网关场景）。
			hint := CCSwitchHint{}
			if err == nil && len(d.Providers) <= 3 {
				var scan CCSwitchScan
				if scanErr := c.JSON(ctx, "GET", "/ccswitch", nil, &scan); scanErr == nil && scan.Importable > 0 {
					hint = CCSwitchHint{Visible: true, Importable: scan.Importable}
				} else if a.hasLocalCCSwitch() {
					hint = CCSwitchHint{Visible: true, Remote: true}
				}
			}
			return func() {
				// 后台直接写渠道状态会与原生渲染竞态，并绕过主机切换的序列校验。
				if channelsErr == nil {
					a.channels = channels.Channels
				}
				if historyErr == nil {
					a.statsHistory = history
				} else {
					a.statsHistory = StatsHistory{}
				}
				a.dashboard = d
				a.ccHint = hint
				a.loaded = true
			}, err
		case "models":
			var m struct {
				Models []Model `json:"models"`
			}
			var r struct {
				Routes []Route `json:"routes"`
			}
			if err := c.JSON(ctx, "GET", "/models", nil, &m); err != nil {
				return nil, err
			}
			err := c.JSON(ctx, "GET", "/routes", nil, &r)
			return func() { a.models = m.Models; a.routes = r.Routes; a.loaded = true }, err
		case "channels":
			var d struct {
				Channels []AccountChannel `json:"channels"`
			}
			err := c.JSON(ctx, "GET", "/channels", nil, &d)
			return func() { a.channels = orderChannels(d.Channels); a.loaded = true }, err
		case "providers":
			var d struct {
				Providers []Provider `json:"providers"`
			}
			err := c.JSON(ctx, "GET", "/providers", nil, &d)
			return func() { a.providers = d.Providers; a.loaded = true }, err
		case "logs":
			var d struct {
				Logs []Log `json:"logs"`
			}
			err := c.JSON(ctx, "GET", "/logs?limit="+url.QueryEscape(limit), nil, &d)
			return func() { a.logs = d.Logs; a.logSelected = -1; a.loaded = true }, err
		case "skills":
			if source == skillSourceMarket {
				// 分类列表尽力而为：拉取失败只影响筛选下拉，不影响浏览。
				var sections struct {
					Sections []MarketSection `json:"sections"`
				}
				if err2 := c.JSON(ctx, "GET", "/skills/market/sections", nil, &sections); err2 != nil {
					sections.Sections = nil
				}
				sectionID := 0
				for _, s := range sections.Sections {
					if s.Name == marketSection {
						sectionID = s.ID
					}
				}
				endpoint := fmt.Sprintf("/skills/market/browse?q=%s&sort=%s&page=%d",
					url.QueryEscape(marketQuery), url.QueryEscape(marketSortParam(marketSort)), marketPage)
				if sectionID > 0 {
					endpoint += fmt.Sprintf("&section=%d", sectionID)
				}
				var browse MarketPage
				err := c.JSON(ctx, "GET", endpoint, nil, &browse)
				if err == nil && browse.Error != "" {
					err = errors.New(browse.Error)
				}
				var local struct {
					Installed []MarketInstalled `json:"installed"`
				}
				if err == nil {
					if err2 := c.JSON(ctx, "GET", "/skills/market/installed", nil, &local); err2 != nil {
						err = err2
					}
				}
				return func() {
					a.market = browse
					a.marketSections = sections.Sections
					a.marketInstalled = local.Installed
					if a.marketSection != marketSectionAll && len(sections.Sections) > 0 {
						found := false
						for _, s := range sections.Sections {
							if s.Name == a.marketSection {
								found = true
							}
						}
						if !found {
							a.marketSection = marketSectionAll
						}
					}
					a.loaded = true
				}, err
			}
			var d Skills
			err := c.JSON(ctx, "GET", "/skills"+skillsQuery(project), nil, &d)
			if err == nil && d.Error != "" {
				err = errors.New(d.Error)
			}
			return func() { a.acceptSkills(d); a.loaded = true }, err
		case "memory":
			endpoint := "/memories?limit=200"
			if filter != "all" {
				endpoint += "&status=" + url.QueryEscape(filter)
			}
			var d struct {
				Memories []Memory `json:"memories"`
			}
			err := c.JSON(ctx, "GET", endpoint, nil, &d)
			return func() { a.memories = d.Memories; a.loaded = true }, err
		case "settings":
			var gatewayConfig GatewayConfigStatus
			gatewayErr := c.JSON(ctx, "GET", "/config/status", nil, &gatewayConfig)
			var cors CORSConfig
			if gatewayErr == nil {
				gatewayErr = c.JSON(ctx, "GET", "/config/cors", nil, &cors)
			}
			// 各面板尽力而为：助理人设拉取失败（如助理未启用返回 503）
			// 只影响人设卡片，不拖垮整个设置页；客户端同步与 cc-switch
			// 迁移扫描同样独立容错。
			var p Prompt
			promptErr := c.JSON(ctx, "GET", "/assistant/prompt", nil, &p)
			configs := map[string]ClientConfig{}
			for _, target := range []string{"pi", "zcode", "harness"} {
				var config ClientConfig
				if err := c.JSON(ctx, "GET", "/"+target, nil, &config); err != nil {
					config.Error = err.Error()
				}
				configs[target] = config
			}
			scan := CCSwitchScan{}
			if err := c.JSON(ctx, "GET", "/ccswitch"+ccswitchHomeQuery(a.ccHomeDraft), nil, &scan); err != nil {
				scan = CCSwitchScan{Error: "扫描失败：" + err.Error()}
			}
			return func() {
				a.gatewayConfig = gatewayConfig
				a.gatewayConfigErr = ""
				if gatewayErr != nil {
					a.gatewayConfigErr = "配置管理暂不可用，请检查网关版本或连接：" + gatewayErr.Error()
				}
				a.corsConfig = cors
				a.corsDraft = strings.Join(cors.AllowedOrigins, "\n")
				a.prompt = p
				a.promptDraft = p.Custom
				a.promptErr = ""
				if promptErr != nil {
					a.promptErr = promptErr.Error()
				}
				a.configs = configs
				a.initConfigSelection()
				a.acceptCCSwitchScan(scan)
				a.ccUpload = nil
				a.ccRemote = false
				a.loaded = true
			}, nil
		case "assistant":
			var m struct {
				Default string            `json:"default"`
				Models  []string          `json:"models"`
				Options []ChatModelOption `json:"model_options"`
			}
			err := c.JSON(ctx, "GET", "/assistant/models", nil, &m)
			return func() {
				a.chatModels = m.Models
				a.chatModelOptions = map[string]ChatModelOption{}
				for _, option := range m.Options {
					a.chatModelOptions[option.ID] = option
				}
				a.chatDefault = m.Default
				if a.chatModel != chatDefaultOption {
					found := false
					for _, id := range m.Models {
						if id == a.chatModel {
							found = true
						}
					}
					if !found {
						a.chatModel = chatDefaultOption
					}
				}
				a.loaded = true
			}, err
		}
		return nil, nil
	})
}
func (a *App) mutate(label, method, path string, body any) {
	a.work(label, 60*time.Second, func(ctx context.Context, c *connection.Client) (func(), error) {
		err := c.JSON(ctx, method, path, body, nil)
		return func() { a.reload(); a.notice = "已完成" }, err
	})
}
func (a *App) selectConnection(id string) {
	st, err := a.store.Select(id)
	if err != nil {
		a.problem = err.Error()
		return
	}
	a.stop()
	a.profiles = st
	a.resetHost()
	a.page = "overview"
	a.connSelect = a.active().Name
	a.reload()
}

const chatDefaultOption = "网关默认"

// localConnection 判断当前连接的网关是否跑在本机（回环地址）——只有
// 这种情况下原生目录面板选择的路径才属于网关主机。
func (a *App) localConnection() bool {
	u, err := url.Parse(strings.TrimSpace(a.active().URL))
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// pickLocalFolder 用原生面板（sheet 挂主窗口）选择本机目录；取消或未
// 注入面板能力（无 cgo 构建）时保持原值不变。
func (a *App) pickLocalFolder() {
	if a.PickFolder == nil {
		return
	}
	a.PickFolder(func(path string, ok bool) {
		if !ok {
			return
		}
		if trimmed := strings.TrimSpace(path); trimmed != "" {
			a.dispatch(func() { a.project = trimmed })
		}
	})
}

// routeCandidates 返回模型配置过、但尚未加入当前回退链的 Provider。
func (a *App) routeCandidates(model string, order []string) []string {
	inChain := map[string]bool{}
	for _, name := range order {
		inChain[name] = true
	}
	candidates := []string{}
	seen := map[string]bool{}
	for _, r := range a.routes {
		if r.Model != model {
			continue
		}
		for _, p := range r.Providers {
			if !inChain[p.Name] && !seen[p.Name] {
				seen[p.Name] = true
				candidates = append(candidates, p.Name)
			}
		}
	}
	return candidates
}

// routeOrderFor 返回模型当前回退链顺序；无路由时回退到模型自带 Provider。
func (a *App) routeOrderFor(model string) []string {
	for _, r := range a.routes {
		if r.Model == model {
			names := make([]string, 0, len(r.Providers))
			for _, p := range r.Providers {
				names = append(names, p.Name)
			}
			return names
		}
	}
	for _, m := range a.models {
		if m.Name == model {
			return append([]string(nil), m.Providers...)
		}
	}
	return nil
}

func (a *App) chatModelParam() string {
	if a.chatModel == chatDefaultOption {
		return ""
	}
	return a.chatModel
}

func (a *App) resetHost() {
	a.listViews = listViewState{}
	a.loaded = false
	a.dashboard = Dashboard{}
	a.models = nil
	a.routes = nil
	a.providers = nil
	a.channels = nil
	a.channelSetup = ""
	a.channelList = ui.ListState{}
	a.logs = nil
	a.memories = nil
	a.skills = Skills{}
	a.skillDirty = false
	a.loadedProject = ""
	a.project = ""
	a.targets = ""
	a.skillSelection = map[string]bool{}
	a.skillFilter, a.skillTag = "", skillsTagAll
	a.skillScopeOpen = false
	a.skillExpanded = map[string]bool{}
	a.skillSource = skillSourceRepo
	a.market = MarketPage{}
	a.marketQuery, a.marketSort, a.marketSection = "", "推荐", marketSectionAll
	a.marketSections = nil
	a.marketPage = 1
	a.marketInstalled = nil
	a.marketExpanded = map[string]bool{}
	a.modelPickerOpen = false
	a.modelPickerFilter = ""
	a.modelPickerList = ui.ListState{}
	a.modelCategory = modelCategoryAll
	a.prompt = Prompt{}
	a.promptDraft = ""
	a.gatewayConfig = GatewayConfigStatus{}
	a.gatewayConfigErr = ""
	a.corsConfig = CORSConfig{}
	a.corsDraft = ""
	a.configs = map[string]ClientConfig{}
	a.configSelection = map[string]map[string]bool{}
	a.chatMarkdown = markdownDocument{}
	a.chatReply = ""
	a.chatInput = ""
	a.chatUser = ""
	a.chatStatus = ""
	a.chatModels = nil
	a.chatModelOptions = nil
	a.chatModel = chatDefaultOption
	a.chatDefault = ""
	a.table = ui.ListState{}
	a.notice = ""
	a.problem = ""
	a.browserOpen = false
	a.browserPath, a.browserParent = "", ""
	a.browserDirs = nil
}
func skillsQuery(project string) string {
	if project == "" {
		return ""
	}
	return "?project=" + url.QueryEscape(project)
}

// marketSortParam 把市场排序选项的中文标签映射到商城 API 的 sort 值。
func marketSortParam(label string) string {
	switch label {
	case "最多安装":
		return "installs"
	case "名称":
		return "name"
	default:
		return "featured"
	}
}

// marketInstalledByName 按商城技能目录名查本机安装记录。
func (a *App) marketInstalledByName(name string) (MarketInstalled, bool) {
	for _, m := range a.marketInstalled {
		if m.Name == name {
			return m, true
		}
	}
	return MarketInstalled{}, false
}

// filteredChatModels 按搜索词筛助理模型：前缀匹配优先、其后包含，
// 均不分大小写；无过滤词时返回含“网关默认”的全量目录。
func (a *App) filteredChatModels() []string {
	all := append([]string{chatDefaultOption}, a.chatModels...)
	q := strings.ToLower(strings.TrimSpace(a.modelPickerFilter))
	if q == "" {
		return all
	}
	var starts, contains []string
	for _, m := range all {
		l := strings.ToLower(m)
		switch {
		case strings.HasPrefix(l, q):
			starts = append(starts, m)
		case strings.Contains(l, q) || strings.Contains(strings.ToLower(a.chatModelOptions[m].Provider), q) || strings.Contains(strings.ToLower(a.chatModelOptions[m].Channel), q):
			contains = append(contains, m)
		}
	}
	return append(starts, contains...)
}

// channelExpiryCounts 汇总渠道账号凭据的失效与临期数量（24 小时内到期）。
func (a *App) channelExpiryCounts() (expired, expiring int) {
	for _, ch := range a.channels {
		for _, account := range ch.Accounts {
			if account.Expired {
				expired++
				continue
			}
			if expiry, err := time.Parse(time.RFC3339, account.ExpiresAt); err == nil {
				if until := time.Until(expiry); until > 0 && until < 24*time.Hour {
					expiring++
				}
			}
		}
	}
	return expired, expiring
}

func split(value string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, s := range strings.Split(value, ",") {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			out = append(out, s)
			seen[s] = true
		}
	}
	return out
}

// openBrowser 打开网关主机目录浏览器；起始路径用当前输入框的值，
// 留空时由网关返回其主目录。本机网关浏览的就是本机文件系统，
// 远程网关则与 Termius 的远程浏览同构——路径语义始终属于网关主机。
func (a *App) openBrowser() {
	a.openBrowserFor("project", a.project)
}
func (a *App) openBrowserFor(kind, start string) {
	a.browserKind = kind
	a.browserOpen = true
	a.browseHost(strings.TrimSpace(start))
}
func (a *App) browseHost(path string) {
	a.work("正在读取目录", 20*time.Second, func(ctx context.Context, c *connection.Client) (func(), error) {
		var d struct {
			Path   string   `json:"path"`
			Parent string   `json:"parent"`
			Dirs   []string `json:"dirs"`
		}
		endpoint := "/fs/dirs"
		if path != "" {
			endpoint += "?path=" + url.QueryEscape(path)
		}
		if err := c.JSON(ctx, "GET", endpoint, nil, &d); err != nil {
			return nil, err
		}
		return func() { a.browserPath, a.browserParent, a.browserDirs = d.Path, d.Parent, d.Dirs }, nil
	})
}
func (a *App) pickBrowserDir() {
	if a.browserPath == "" {
		return
	}
	a.applyPickedDir(a.browserPath)
	a.browserOpen = false
}
func (a *App) applyPickedDir(path string) {
	switch a.browserKind {
	case "memory":
		a.memoryKey = path
	default:
		a.project = path
	}
}

// clientSuggestions 汇总记忆范围键的可选客户端：已有 client 作用域
// 记忆里的键 + 已知客户端默认项。
func (a *App) clientSuggestions() []string {
	clients := []string{}
	seen := map[string]bool{}
	add := func(v string) {
		if v != "" && !seen[v] {
			seen[v] = true
			clients = append(clients, v)
		}
	}
	for _, m := range a.memories {
		if m.Scope == "client" {
			add(m.ScopeKey)
		}
	}
	for _, c := range []string{"claude-code", "codex", "pi", "zcode", "harness"} {
		add(c)
	}
	return clients
}
func configInSync(cfg ClientConfig, choices map[string]bool) bool {
	if !cfg.Exists || len(cfg.MissingEntries) > 0 || len(cfg.Current) != len(sortedSelection(choices)) {
		return false
	}
	for _, id := range cfg.Current {
		if !choices[id] {
			return false
		}
	}
	return true
}
func (a *App) initConfigSelection() {
	a.configSelection = map[string]map[string]bool{}
	for target, cfg := range a.configs {
		choices := map[string]bool{}
		for _, m := range cfg.Desired {
			choices[m.ID] = !cfg.Exists
			for _, id := range cfg.Current {
				if id == m.ID {
					choices[m.ID] = true
				}
			}
		}
		a.configSelection[target] = choices
	}
}

// ─── cc-switch 迁移 ─────────────────────────────────────────────────────────

// ccswitchHomeQuery 把自定义 cc-switch 目录拼进扫描查询；空串用网关主机默认。
func ccswitchHomeQuery(home string) string {
	home = strings.TrimSpace(home)
	if home == "" {
		return ""
	}
	return "?home=" + url.QueryEscape(home)
}

// acceptCCSwitchScan 应用一次扫描结果并重置勾选：可导入条目默认全选。
func (a *App) acceptCCSwitchScan(scan CCSwitchScan) {
	a.ccswitch = scan
	a.ccSelection = map[string]bool{}
	for _, candidate := range scan.Candidates {
		if candidate.Status == "importable" {
			a.ccSelection[candidate.Name] = true
		}
	}
}

// ccSelectedNames 按扫描顺序返回勾选的可导入条目名。
func (a *App) ccSelectedNames() []string {
	names := []string{}
	for _, candidate := range a.ccswitch.Candidates {
		if candidate.Status == "importable" && a.ccSelection[candidate.Name] {
			names = append(names, candidate.Name)
		}
	}
	return names
}

// uploadLocalCCSwitch 读取本机 cc-switch 配置并上传到网关预览。网关在远程
// 主机、cc-switch 装在本机时走这条路；解析与转换仍在网关侧完成。
func (a *App) uploadLocalCCSwitch() {
	if a.busy != "" {
		return
	}
	a.work("正在读取并上传本机 cc-switch", 35*time.Second, func(ctx context.Context, c *connection.Client) (func(), error) {
		home := a.ccLocalHome
		if home == "" {
			home = ccswitchlocal.DefaultHome()
		}
		payload, err := ccswitchlocal.Scan(home)
		if err != nil {
			return nil, err
		}
		var scan CCSwitchScan
		if err := c.JSON(ctx, "POST", "/ccswitch/remote-scan", payload, &scan); err != nil {
			return nil, err
		}
		label := payload.Label
		return func() {
			a.ccUpload = payload
			a.ccRemote = true
			scan.Home = home
			scan.Source = label
			a.acceptCCSwitchScan(scan)
		}, nil
	})
}

// importCCSwitch 迁移勾选条目并刷新扫描结果；网关侧写入 providers.yaml 与
// .env 后即时注册，导入的模型立刻可路由。本机上传模式下走 remote-import，
// 随请求重传上传数据（网关不在请求间保存任何状态）。
func (a *App) importCCSwitch() {
	include := a.ccSelectedNames()
	if len(include) == 0 || a.busy != "" {
		return
	}
	endpoint, body := "/ccswitch/import", any(map[string]any{"include": include})
	if a.ccRemote && a.ccUpload != nil {
		endpoint = "/ccswitch/remote-import"
		body = struct {
			ccswitchlocal.Payload
			Include []string `json:"include"`
		}{Payload: *a.ccUpload, Include: include}
	}
	// 体检需要每个待导 provider 的模型实例名；导入后的扫描里这些条目会变
	// 成「已存在」，模型清单丢失，所以在请求前先记下来。
	importedModels := map[string][]string{}
	for _, candidate := range a.ccswitch.Candidates {
		if candidate.Status == "importable" && a.ccSelection[candidate.Name] {
			importedModels[candidate.Name] = candidate.Models
		}
	}
	a.work("正在迁移 cc-switch 供应商", 60*time.Second, func(ctx context.Context, c *connection.Client) (func(), error) {
		var result CCSwitchImportResult
		if err := c.JSON(ctx, "POST", endpoint, body, &result); err != nil {
			return nil, err
		}
		if result.Error != "" {
			return nil, errors.New(result.Error)
		}
		if !result.Wrote || len(result.Imported) == 0 {
			return nil, errors.New("网关没有写入变更：所选条目可能已被导入过")
		}
		return func() {
			a.ccImported = result.Imported
			a.ccImportedModels = importedModels
			a.ccHealth = nil
			a.ccUpdatedKeys = map[string]bool{}
			for _, env := range result.UpdatedKeys {
				a.ccUpdatedKeys[env] = true
			}
			a.acceptCCSwitchScan(CCSwitchScan{
				Home:       a.ccswitch.Home,
				Source:     result.Source,
				Candidates: result.Candidates,
				Importable: ccCountStatus(result.Candidates, "importable"),
				Skipped:    ccCountStatus(result.Candidates, "skipped"),
			})
			a.notice = fmt.Sprintf("已从 cc-switch 导入 %d 个供应商，模型即刻可用", len(result.Imported))
		}, nil
	})
}

// hasLocalCCSwitch 报告本机是否装有 cc-switch（概览引导用，只做存在性检查）。
func (a *App) hasLocalCCSwitch() bool {
	home := a.ccLocalHome
	if home == "" {
		home = ccswitchlocal.DefaultHome()
	}
	_, err := ccswitchlocal.Scan(home)
	return err == nil
}

// checkImportedProviders 对刚导入的供应商逐个跑一次网关健康检查（每个
// provider 用它绑定的第一个模型实例探测），把在线/降级/离线结果展示在
// 导入成功卡上——坏密钥与不可达的回环端点当场现形。
func (a *App) checkImportedProviders() {
	if a.busy != "" || len(a.ccImportedModels) == 0 {
		return
	}
	type healthResult struct{ name, status string }
	targets := make([]struct{ name, model string }, 0, len(a.ccImportedModels))
	for name, models := range a.ccImportedModels {
		if len(models) > 0 {
			targets = append(targets, struct{ name, model string }{name, models[0]})
		}
	}
	a.work("正在体检导入的供应商", 60*time.Second, func(ctx context.Context, c *connection.Client) (func(), error) {
		results := map[string]string{}
		for _, target := range targets {
			var probe struct {
				Status string `json:"probe_status"`
			}
			if err := c.JSON(ctx, "POST", "/providers/"+target.model+"/health-check", nil, &probe); err != nil {
				results[target.name] = "unknown"
				continue
			}
			if probe.Status == "" {
				probe.Status = "unknown"
			}
			results[target.name] = probe.Status
		}
		return func() {
			a.ccHealth = results
			a.notice = "供应商体检完成"
		}, nil
	})
}

// syncAllClients 把当前模型选择一次性同步到全部客户端（复用设置页各目标
// 的既有选择与 /sync 接口），导入完成后一键把模型分发下去。
func (a *App) syncAllClients() {
	if a.busy != "" {
		return
	}
	type syncTarget struct {
		id    string
		model []string
	}
	targets := []syncTarget{}
	for _, target := range []string{"pi", "zcode", "harness"} {
		if ids := sortedSelection(a.configSelection[target]); len(ids) > 0 {
			targets = append(targets, syncTarget{target, ids})
		}
	}
	if len(targets) == 0 {
		return
	}
	a.work("正在同步全部客户端", 60*time.Second, func(ctx context.Context, c *connection.Client) (func(), error) {
		updated := map[string]ClientConfig{}
		for _, target := range targets {
			var result ClientConfig
			if err := c.JSON(ctx, "POST", "/"+target.id+"/sync", map[string]any{"model_ids": target.model}, &result); err != nil {
				result.Error = err.Error()
			}
			updated[target.id] = result
		}
		return func() {
			for id, result := range updated {
				a.configs[id] = result
			}
			a.notice = "已同步全部客户端"
		}, nil
	})
}

func ccCountStatus(candidates []CCSwitchCandidate, status string) int {
	count := 0
	for _, candidate := range candidates {
		if candidate.Status == status {
			count++
		}
	}
	return count
}

// ccImportedNames 返回本次会话内已导入的条目名集合（导入后扫描里它们会
// 变成「端点已存在」，用这个集合还原成功状态）。
func (a *App) ccImportedNames() map[string]bool {
	names := map[string]bool{}
	for _, name := range a.ccImported {
		names[name] = true
	}
	return names
}
func shortNumber(n int64) string {
	if n >= 1000000 {
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	}
	if n >= 1000 {
		return fmt.Sprintf("%.1fK", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}
func percent(n *float64) string {
	if n == nil {
		return "未报告"
	}
	return fmt.Sprintf("%.1f%%", *n)
}
