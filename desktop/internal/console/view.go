package console

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/nightgopher/easygateway/desktop/internal/connection"
)

var pages = []struct{ ID, Title, Subtitle string }{
	{"overview", "运行概览", "网关流量、模型与上游健康状态"},
	{"models", "模型与路由", "配置模型能力和 Provider 回退顺序"},
	{"providers", "Provider", "管理上游连接、探测与熔断器"},
	{"channels", "账号渠道", "登录上游账号、查看授权状态"},
	{"logs", "请求日志", "查看请求结果和每次上游尝试"},
	{"skills", "技能", "搜索、启用并同步技能到全局与项目目录"},
	{"memory", "记忆", "确认、停用和管理助理记忆"},
	{"assistant", "助理", "与网关助理对话，实时查看回复和工具状态"},
	{"settings", "设置", "客户端模型同步与助理人设"},
	{"connections", "网关连接", "管理本机与远程网关的连接"},
}

func (a *App) View(c *ui.Context) {
	theme := *c.Theme()
	theme.Accent = ui.Hex("#a75720")
	theme.AccentText = ui.Hex("#ffffff")
	theme.AccentHover = ui.Hex("#8a441a")
	theme.AccentPressed = ui.Hex("#703716")
	theme.Focus = theme.Accent.Alpha(.4)
	theme.Selection = theme.Accent.Alpha(.25)
	if !theme.Dark {
		theme.Background = ui.Hex("#f7f8fa")
		theme.Surface = ui.Hex("#eef0f3")
		theme.Border = ui.Hex("#dce0e5")
		theme.TextMuted = ui.Hex("#5b6470")
	}
	theme.Radius = 8
	c.SetTheme(&theme)
	vibrant := c.Vibrancy()
	if vibrant {
		c.Root().Background(ui.Transparent)
	}
	if c.Shortcut(ui.Cmd, ui.KeyR) && a.busy == "" && !a.skillDirty && a.page != "connections" && a.page != "assistant" {
		a.reload()
	}
	if a.notice != "" && a.notice != a.lastNotice {
		c.Toast(a.notice)
		a.lastNotice = a.notice
	} else if a.notice == "" {
		a.lastNotice = ""
	}
	root := ui.Column(c).Fill()
	if !vibrant {
		root = root.Background(theme.Background)
	}
	root.Children(func() {
		bar := c.TitleBar()
		topRow := ui.Row(c).Height(max(bar.Height, 48)).Padding(0, 16, 0, bar.Left+16).Gap(12).DragWindow().BorderWidth(0, 0, 1, 0).BorderColor(theme.Border)
		if !vibrant {
			topRow = topRow.Background(theme.Surface)
		}
		topRow.Children(func() {
			ui.Box(c).Size(28, 28).Center().Radius(8).Background(theme.Accent).AlignSelf(ui.Center).Children(func() {
				ui.Text(c, "E").Bold().FontSize(15).TextColor(theme.AccentText)
			})
			ui.Text(c, "EasyGateway").Bold().FontSize(16)
			ui.Spacer(c)
			profile := a.active()
			if profile.ID != "" {
				dot := theme.TextMuted
				switch {
				case a.problem != "":
					dot = theme.Danger
				case a.busy != "":
					dot = theme.Accent
				case a.loaded:
					dot = okGreen
				}
				ui.Box(c).Size(9, 9).Radius(999).Background(dot).AlignSelf(ui.Center)
				// 连接快速切换：与 EasyAgent 一致，标题栏直接切换活动网关。
				names := make([]string, 0, len(a.profiles.Profiles))
				inList := false
				for _, p := range a.profiles.Profiles {
					names = append(names, p.Name)
					if p.Name == a.connSelect {
						inList = true
					}
				}
				if !inList {
					a.connSelect = profile.Name
				}
				if len(names) > 1 {
					if ui.Select(c, &a.connSelect, names).Label("活动网关").Disabled(a.busy != "").Changed() {
						for _, p := range a.profiles.Profiles {
							if p.Name == a.connSelect {
								a.selectConnection(p.ID)
								break
							}
						}
					}
					ui.Text(c, profile.URL).SingleLine().TextColor(theme.TextMuted)
				} else {
					ui.Text(c, profile.Name+" · "+profile.URL).SingleLine().TextColor(theme.TextMuted)
				}
			}
		})
		expiredChannels, expiringChannels := a.channelExpiryCounts()
		candidateMemories := 0
		for _, m := range a.memories {
			if m.Status == "candidate" {
				candidateMemories++
			}
		}
		troubledProviders := 0
		for _, pr := range a.providers {
			if pr.Status == "offline" || pr.State == "open" {
				troubledProviders++
			}
		}
		ui.Row(c).Grow(1).AlignItems(ui.Stretch).Children(func() {
			sidebarCol := ui.Column(c).Width(208).BorderWidth(0, 1, 0, 0).BorderColor(theme.Border)
			if !vibrant {
				sidebarCol = sidebarCol.Background(theme.Surface)
			}
			sidebarCol.Children(func() {
				current := a.page
				sidebar := ui.Sidebar(c, &current, func() {
					ui.SidebarSection(c, "控制台", nil, func() {
						for _, p := range pages[:5] {
							item := ui.SidebarItem(c, p.ID, nil, p.Title)
							switch p.ID {
							case "channels":
								if expiredChannels > 0 {
									item.Children(func() { ui.Badge(c, fmt.Sprint(expiredChannels)).Background(theme.Danger).TextColor(ui.Hex("#ffffff")) })
								} else if expiringChannels > 0 {
									item.Children(func() { ui.Badge(c, fmt.Sprint(expiringChannels)).Background(theme.Accent).TextColor(theme.AccentText) })
								}
							case "providers":
								if troubledProviders > 0 {
									item.Children(func() {
										ui.Badge(c, fmt.Sprint(troubledProviders)).Background(theme.Danger).TextColor(ui.Hex("#ffffff"))
									})
								}
							}
						}
					})
					ui.SidebarSection(c, "个人工作区", nil, func() {
						for _, p := range pages[5:9] {
							item := ui.SidebarItem(c, p.ID, nil, p.Title)
							if p.ID == "memory" && candidateMemories > 0 {
								item.Children(func() {
									ui.Badge(c, fmt.Sprint(candidateMemories)).Background(theme.Accent).TextColor(theme.AccentText)
								})
							}
						}
					})
					ui.SidebarSection(c, "连接", nil, func() { ui.SidebarItem(c, "connections", nil, "网关连接") })
				}).Grow(1).Padding(12, 8)
				if sidebar.Changed() {
					if a.active().ID == "" && current != "connections" {
						a.problem = "请先添加一个网关连接"
					} else {
						a.navigate(current)
					}
				}
				ui.Text(c, "原生桌面端 · "+a.Version).FontSize(12).TextColor(theme.TextMuted).Padding(16)
			})
			// 内容列限宽居中：管理后台的信息密度在 1000px 左右最舒适，
			// 超宽屏下表单和列表拉满会稀疏难读（又宽又矮的观感主要来源）。
			ui.Column(c).Grow(1).MinWidth(0).MaxWidth(1040).Margin(0, ui.Auto).Padding(24, 28).Gap(16).Background(theme.Background).Children(func() {
				ui.Row(c).Gap(16).Children(func() {
					ui.Column(c).Grow(1).Gap(4).Children(func() {
						for _, p := range pages {
							if p.ID == a.page {
								ui.Text(c, p.Title).FontSize(26).Bold()
								muted(c, p.Subtitle)
							}
						}
					})
					if a.page != "connections" && a.page != "assistant" {
						ui.Button(c, "刷新").Disabled(a.busy != "" || a.skillDirty).OnClick(a.reload)
					}
					if a.busy != "" && !(a.page == "assistant" && a.busy == "助理回复中") {
						ui.Button(c, "取消").OnClick(a.stop)
					}
				})
				ui.Box(c).Height(3).Children(func() {
					if a.busy != "" {
						ui.Progress(c, -1).Label(a.busy)
					}
				})
				if a.problem != "" {
					ui.Row(c).Gap(12).Padding(12).Radius(8).Background(theme.Danger.Alpha(.1)).Children(func() {
						ui.Text(c, a.problem).TextColor(theme.Danger).Grow(1).Selectable()
						ui.Button(c, "关闭提示").OnClick(func() { a.problem = "" })
					})
				}
				if a.notice != "" {
					ui.Row(c).Gap(12).Padding(12).Radius(8).Background(theme.Accent.Alpha(.08)).Children(func() {
						ui.Text(c, a.notice).TextColor(theme.Accent).Grow(1).Selectable()
						ui.Button(c, "收起").OnClick(func() { a.notice = "" })
					})
				}
				// 载入进行中才整体占位；载入失败时页面工具栏保持可用
				// （技能页切来源、模型页换分类都能直接自愈），数据区由
				// 各页的 unloadedOrEmpty 给出明确提示。
				if a.page != "connections" && !a.loaded && a.busy != "" {
					ui.Column(c).Grow(1).Center().Gap(12).Children(func() {
						ui.Spinner(c).Size(28, 28)
						muted(c, "正在载入网关数据")
					})
					return
				}
				switch a.page {
				case "connections":
					a.connectionsView(c)
				case "overview":
					a.overviewView(c)
				case "models":
					a.modelsView(c)
				case "channels":
					a.channelsView(c)
				case "providers":
					a.providersView(c)
				case "logs":
					a.logsView(c)
				case "skills":
					a.skillsView(c)
				case "memory":
					a.memoryView(c)
				case "assistant":
					a.assistantView(c)
				case "settings":
					a.settingsView(c)
				}
			})
		})
	})
	a.editor(c)
	a.browserModal(c)
	a.modelPickerModal(c)
	a.pruneDialog(c)
	if ui.AlertDialog(c, &a.deleteOpen, "确认移除？", a.editorTitle, "取消", "移除") == 1 && a.deleteAction != nil {
		action := a.deleteAction
		a.deleteAction = nil
		action()
	}
}
func muted(c *ui.Context, text string) { ui.Text(c, text).TextColor(c.Theme().TextMuted) }
func chip(c *ui.Context, text string, color ui.Color) {
	ui.Box(c).Padding(2, 10).Radius(999).Background(color.Alpha(.13)).Shrink(0).Children(func() {
		ui.Text(c, text).TextColor(color).FontSize(12).SingleLine()
	})
}
func card(c *ui.Context, body func()) {
	ui.Column(c).Padding(18).Gap(12).Background(c.Theme().Surface).Border(1, c.Theme().Border).Radius(12).Children(body)
}
func row(c *ui.Context, body func()) {
	ui.Column(c).Padding(14, 4, 14, 0).Gap(10).BorderWidth(0, 0, 1, 0).BorderColor(c.Theme().Border.Alpha(.7)).Children(body)
}
func meta(c *ui.Context, parts ...string) {
	ui.Row(c).Gap(16).Children(func() {
		for _, part := range parts {
			ui.Text(c, part).TextColor(c.Theme().TextMuted).SingleLine()
		}
	})
}
func empty(c *ui.Context, text string) {
	ui.Column(c).Grow(1).Center().Children(func() { ui.Text(c, text).FontSize(14).TextColor(c.Theme().TextMuted) })
}

// unloadedOrEmpty 处理页面数据区的空态：载入失败给重试提示并返回 true；
// 正常载入但 count == 0 时展示 emptyText 并返回 true；有数据返回 false。
// count 必须传真实数据量，否则有数据时也会误渲染空文案。
func (a *App) unloadedOrEmpty(c *ui.Context, count int, emptyText string) bool {
	if !a.loaded {
		empty(c, "数据未加载，点击右上角“刷新”重试。")
		return true
	}
	if count == 0 {
		empty(c, emptyText)
		return true
	}
	return false
}

// notLoaded 仅处理载入失败的情况（数据区自身的空态由页面自行处理）。
func (a *App) notLoaded(c *ui.Context) bool {
	if !a.loaded {
		empty(c, "数据未加载，点击右上角“刷新”重试。")
		return true
	}
	return false
}
func (a *App) connectionsView(c *ui.Context) {
	ui.Row(c).Gap(12).Children(func() {
		ui.PrimaryButton(c, "添加连接").Disabled(a.busy != "").OnClick(func() { a.editConnection(connection.Profile{}) })
		muted(c, "Token 保存在本机受保护配置中")
	})
	if len(a.profiles.Profiles) == 0 {
		card(c, func() {
			ui.Text(c, "连接你的网关").FontSize(22).Bold()
			muted(c, "填写网关地址和管理 Token。验证通过后，即可管理模型、路由和工作区。")
			muted(c, "支持本机、远程主机与带路径前缀的反向代理。")
		})
		return
	}
	ui.List(c, nil, len(a.profiles.Profiles), func(i int) {
		p := a.profiles.Profiles[i]
		ui.Column(c.Key(p.ID)).Children(func() {
			row(c, func() {
				ui.Row(c).Gap(12).Children(func() {
					ui.Column(c).Grow(1).Gap(6).Children(func() {
						ui.Text(c, p.Name).FontSize(18).Bold()
						ui.Text(c, p.URL).Selectable().TextColor(c.Theme().TextMuted)
					})
					if p.ID == a.profiles.ActiveID {
						chip(c, "当前连接", c.Theme().Accent)
					} else {
						ui.PrimaryButton(c, "使用").Label("使用 " + p.Name).Disabled(a.busy != "").OnClick(func() { a.selectConnection(p.ID) })
					}
					ui.Button(c, "编辑").Label("编辑 " + p.Name).Disabled(a.busy != "").OnClick(func() { a.editConnection(p) })
					ui.Button(c, "移除").Label("移除 " + p.Name).Disabled(a.busy != "").OnClick(func() {
						a.editorTitle = "移除连接「" + p.Name + "」及其本机 Token，不会删除网关数据。"
						a.deleteAction = func() {
							st, err := a.store.Delete(p.ID)
							if err != nil {
								a.problem = err.Error()
								return
							}
							a.stop()
							a.profiles = st
							a.resetHost()
						}
						a.deleteOpen = true
					})
				})
			})
		})
	}).Grow(1)
}
func (a *App) editConnection(p connection.Profile) {
	a.editorKind = "connection"
	a.editorID = p.ID
	a.editorName = p.Name
	a.editorURL = p.URL
	a.editorToken = ""
	a.editorOpen = true
	a.problem = ""
}
func input(c *ui.Context, label string, value *string, hint string) {
	ui.Field(c, label, func() { ui.TextInput(c, value).Placeholder(hint).FillWidth() })
}

// pruneDialog 是「清理失败模型」的确认弹窗：列出候选与失败原因，
// 用户确认后才真正执行移除。
func (a *App) pruneDialog(c *ui.Context) {
	ui.Modal(c, &a.pruneOpen, func() {
		ui.Column(c).Width(560).Gap(14).Children(func() {
			ui.Text(c, "清理失败模型").FontSize(22).Bold()
			muted(c, "以下模型的最近一次探活被上游确定性拒绝（401/402/403/404，含模型不存在、无权限、凭据失效）。移除会编辑网关的 providers.yaml（自动备份）并热重载；429/5xx 等暂时性失败不会出现在这里。")
			ui.List(c, nil, len(a.pruneCandidates), func(i int) {
				candidate := a.pruneCandidates[i]
				ui.Column(c.Key(candidate.Model)).Gap(2).Padding(0, 0, 8, 0).Children(func() {
					ui.Text(c, candidate.Model).Bold().Selectable()
					muted(c, "渠道 "+candidate.Provider+" · "+candidate.Reason)
				})
			}).Grow(1).MinHeight(120).MaxHeight(320)
			ui.Row(c).Gap(8).Justify(ui.End).Children(func() {
				ui.Button(c, "取消").Disabled(a.busy != "").OnClick(func() { a.pruneOpen = false })
				ui.PrimaryButton(c, "确认移除").Disabled(a.busy != "").OnClick(a.pruneExecute)
			})
		})
	})
}

func (a *App) editor(c *ui.Context) {
	wasOpen := a.editorOpen
	ui.Modal(c, &a.editorOpen, func() {
		ui.Column(c).Width(560).Gap(16).Children(func() {
			title := "模型能力"
			if a.editorKind == "connection" {
				title = "网关连接"
			} else if a.editorKind == "route" {
				title = "回退路由"
			}
			ui.Text(c, title).FontSize(22).Bold()
			ui.Fieldset(c, "", func() {
				switch a.editorKind {
				case "connection":
					input(c, "连接名称", &a.editorName, "例如：本机网关")
					input(c, "网关地址", &a.editorURL, "http://127.0.0.1:4001")
					ui.Field(c, "管理 Token", func() {
						ui.TextInput(c, &a.editorToken).Password().FillWidth().Placeholder("ADMIN_TOKEN / EASYGATEWAY_MASTER_KEY")
					})
					if a.editorID != "" {
						muted(c, "Token 留空保留已有值。填写新值即可替换。")
					}
				case "model":
					ui.Text(c, a.editorID).Bold().Selectable()
					ui.Row(c).Wrap().Gap(12).Children(func() {
						for _, cap := range capabilityChoices(a.editorCaps) {
							value := a.editorCaps[cap]
							if ui.Checkbox(c.Key(cap), &value, cap).Changed() {
								a.editorCaps[cap] = value
							}
						}
					})
					input(c, "输入模态", &a.editorModalities, "text, image, audio, video, file")
				case "route":
					ui.Text(c, a.editorID).Bold().Selectable()
					muted(c, "从上到下依次尝试；某一档失败自动落到下一档。")
					if len(a.editorOrder) == 0 {
						muted(c, "回退链为空。")
					}
					for i, name := range a.editorOrder {
						last := i == len(a.editorOrder)-1
						ui.Row(c.Key("entry-" + name)).Gap(8).AlignItems(ui.Center).Children(func() {
							ui.Text(c, fmt.Sprintf("%d. %s", i+1, name)).Grow(1).MinWidth(0).SingleLine()
							if i > 0 {
								ui.Button(c, "上移 "+name).Disabled(a.busy != "").OnClick(func() {
									a.editorOrder[i-1], a.editorOrder[i] = a.editorOrder[i], a.editorOrder[i-1]
								})
							}
							if !last {
								ui.Button(c, "下移 "+name).Disabled(a.busy != "").OnClick(func() {
									a.editorOrder[i+1], a.editorOrder[i] = a.editorOrder[i], a.editorOrder[i+1]
								})
							}
							ui.Button(c, "移除 "+name).Disabled(a.busy != "").OnClick(func() {
								a.editorOrder = append(a.editorOrder[:i], a.editorOrder[i+1:]...)
							})
						})
					}
					if candidates := a.routeCandidates(a.editorID, a.editorOrder); len(candidates) > 0 {
						muted(c, "可加入回退链：")
						ui.Row(c).Wrap().Gap(8).Children(func() {
							for _, name := range candidates {
								ui.Button(c, "添加 "+name).Disabled(a.busy != "").OnClick(func() {
									a.editorOrder = append(a.editorOrder, name)
								})
							}
						})
					}
				}
			}).Disabled(a.busy != "")
			if a.problem != "" {
				ui.Text(c, a.problem).TextColor(c.Theme().Danger)
			}
			ui.Row(c).Gap(8).Justify(ui.End).Children(func() {
				ui.Button(c, "取消编辑").Disabled(a.busy != "").OnClick(func() { a.editorOpen = false; a.editorToken = "" })
				if a.editorKind == "connection" {
					ui.Button(c, "测试连接").Disabled(a.busy != "").OnClick(func() { a.saveConnection(false) })
				}
				ui.PrimaryButton(c, "保存").Disabled(a.busy != "").OnClick(a.saveEditor)
			})
		})
	})
	if wasOpen && !a.editorOpen && a.editorKind == "connection" {
		a.stop()
		a.editorToken = ""
	}
}

// browserModal 网关主机目录浏览器：点击目录名进入，"选择此目录"把当前
// 路径写回项目路径输入框。本机与远程连接走同一套 Admin API。
func (a *App) browserModal(c *ui.Context) {
	ui.Modal(c, &a.browserOpen, func() {
		theme := c.Theme()
		ui.Column(c).Width(620).Gap(14).Children(func() {
			ui.Text(c, "浏览网关主机目录").FontSize(20).Bold()
			muted(c, "浏览的是当前连接的网关主机上的目录；本机网关即本机目录。")
			ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
				ui.Text(c, a.browserPath).FontSize(13).Grow(1).MinWidth(0).SingleLine().TextColor(theme.TextMuted)
				if a.browserParent != "" {
					ui.Button(c, "上级").Label("返回上级目录").Disabled(a.busy != "").OnClick(func() { a.browseHost(a.browserParent) })
				}
			})
			if len(a.browserDirs) == 0 {
				empty(c, "此目录没有子目录")
			}
			ui.List(c, nil, len(a.browserDirs), func(i int) {
				name := a.browserDirs[i]
				ui.Row(c.Key(name)).Padding(0, 0, 4).Children(func() {
					ui.Button(c, "打开 "+name).OnClick(func() { a.browseHost(a.browserPath + "/" + name) })
				})
			}).Grow(1).MinHeight(180).MaxHeight(380)
			ui.Row(c).Gap(8).Justify(ui.End).Children(func() {
				ui.Button(c, "取消浏览").OnClick(func() { a.browserOpen = false })
				ui.PrimaryButton(c, "选择此目录").Disabled(a.busy != "" || a.browserPath == "").OnClick(a.pickBrowserDir)
			})
		})
	})
}
func (a *App) saveConnection(save bool) {
	in := connection.Input{ID: a.editorID, Name: a.editorName, URL: a.editorURL, Token: a.editorToken}
	a.launch("正在验证连接", 20*time.Second, func(ctx context.Context) (func(), error) {
		if !save {
			err := a.store.Test(ctx, in)
			return func() { a.notice = "连接验证成功" }, err
		}
		commit, err := a.store.VerifySave(ctx, in)
		return func() {
			state, commitErr := commit()
			if commitErr != nil {
				a.problem = commitErr.Error()
				return
			}
			a.editorOpen = false
			a.editorToken = ""
			a.profiles = state
			a.resetHost()
			a.page = "overview"
			a.reload()
		}, err
	})
}

func capabilityChoices(chosen map[string]bool) []string {
	choices := append([]string{}, capabilities...)
	known := map[string]bool{}
	for _, cap := range choices {
		known[cap] = true
	}
	unknown := []string{}
	for cap := range chosen {
		if !known[cap] {
			unknown = append(unknown, cap)
		}
	}
	sort.Strings(unknown)
	choices = append(choices, unknown...)
	return choices
}

var capabilities = []string{"text", "vision", "video", "file", "audio", "tool_calling", "streaming", "reasoning"}

func (a *App) saveEditor() {
	if a.editorKind == "connection" {
		a.saveConnection(true)
		return
	}
	endpoint := "/models/" + url.PathEscape(a.editorID)
	body := map[string]any{"capabilities": sortedSelection(a.editorCaps), "input_modalities": split(a.editorModalities)}
	if a.editorKind == "route" {
		if len(a.editorOrder) == 0 {
			a.problem = "回退链不能为空；至少保留一个条目，或取消编辑。"
			return
		}
		endpoint = "/routes/" + url.PathEscape(a.editorID)
		body = map[string]any{"providers": a.editorOrder}
	}
	a.work("正在保存", 35*time.Second, func(ctx context.Context, client *connection.Client) (func(), error) {
		err := client.JSON(ctx, "PUT", endpoint, body, nil)
		return func() { a.editorOpen = false; a.reload(); a.notice = "配置已保存" }, err
	})
}

var okGreen = ui.Hex("#21835a")

func formatLatency(ms float64) string {
	if ms >= 1000 {
		return fmt.Sprintf("%.1f s", ms/1000)
	}
	return fmt.Sprintf("%.0f ms", ms)
}

func statusColor(c *ui.Context, value string) ui.Color {
	if value == "online" || value == "active" || value == "closed" {
		return okGreen
	}
	if value == "offline" || value == "open" {
		return c.Theme().Danger
	}
	if value == "degraded" || value == "half_open" || value == "candidate" {
		return c.Theme().Accent
	}
	return c.Theme().TextMuted
}
func status(c *ui.Context, value string) {
	translations := map[string]string{"online": "在线", "offline": "离线", "unknown": "未探测", "degraded": "降级", "idle": "空闲", "closed": "正常", "open": "熔断", "half_open": "试探中", "active": "已生效", "candidate": "待确认", "retired": "已停用"}
	text := translations[value]
	if text == "" {
		text = value
	}
	chip(c, text, statusColor(c, value))
}

// memoryScopeLabel 把记忆的范围与范围键拼成展示标签，global 无键时只显范围。
func memoryScopeLabel(m Memory) string {
	if m.ScopeKey == "" {
		return m.Scope
	}
	return m.Scope + " · " + m.ScopeKey
}
func (a *App) overviewView(c *ui.Context) {
	if a.notLoaded(c) {
		return
	}
	s := a.dashboard.Summary
	theme := c.Theme()
	if a.ccHint.Visible {
		card(c, func() {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, "检测到 cc-switch 配置").Bold()
				ui.Spacer(c)
				chip(c, "一键迁移", theme.Accent)
			})
			if a.ccHint.Remote {
				muted(c, "这台电脑上装有 cc-switch；网关刚起步，可以把已配置的供应商与模型直接迁入（设置页支持本机上传到远程网关）。")
			} else {
				muted(c, fmt.Sprintf("网关主机上有 %d 个 cc-switch 条目可导入：供应商、模型与密钥一步迁入，cc-switch 数据只读不动。", a.ccHint.Importable))
			}
			ui.Row(c).Gap(8).Children(func() {
				ui.PrimaryButton(c, "去迁移").OnClick(func() { a.navigate("settings") })
			})
		})
	}
	metrics := []struct{ Title, Value string }{
		{"今日请求", shortNumber(int64(s.TodayRequests))},
		{"成功率", fmt.Sprintf("%.1f%%", s.SuccessRate)},
		{"今日 Tokens", shortNumber(s.TodayTokens)},
		{"平均延迟", formatLatency(s.AvgLatency)},
	}
	ui.Row(c).Background(theme.Surface).Radius(12).Border(1, theme.Border).AlignItems(ui.Center).Children(func() {
		for i, metric := range metrics {
			if i > 0 {
				ui.Box(c).Width(1).AlignSelf(ui.Stretch).Background(theme.Border)
			}
			ui.Column(c.Key(metric.Title)).Grow(1).MinWidth(0).Padding(16, 20).Gap(6).Children(func() {
				ui.Text(c, metric.Title).FontSize(13).TextColor(theme.TextMuted)
				ui.Text(c, metric.Value).Bold().FontSize(22).SingleLine()
			})
		}
	})
	// 渠道凭据过期提醒：把“用到才发现 401”提前到概览。
	if expired, expiring := a.channelExpiryCounts(); expired > 0 || expiring > 0 {
		var parts []string
		if expired > 0 {
			parts = append(parts, fmt.Sprintf("%d 个已失效", expired))
		}
		if expiring > 0 {
			parts = append(parts, fmt.Sprintf("%d 个 24 小时内过期", expiring))
		}
		ui.Row(c).Gap(12).Padding(12, 16).Radius(10).Background(theme.Accent.Alpha(.08)).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "渠道凭据提醒："+strings.Join(parts, "，")+"。").TextColor(theme.Accent).Grow(1)
			ui.Button(c, "去处理").OnClick(func() { a.navigate("channels") })
		})
	}

	// 用量趋势：数据来自网关持久化的按天聚合（/stats/history），跨重启可查。
	// 旧版网关无此端点时拉取失败，statsHistory 为空即整卡隐藏。
	if len(a.statsHistory.Days) > 0 {
		card(c, func() {
			ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
				ui.Text(c, "用量趋势").FontSize(16).Bold()
				muted(c, a.statsHistory.From+" ~ "+a.statsHistory.To)
				ui.Spacer(c)
				for _, opt := range []struct {
					label string
					days  int
				}{{"近 7 天", 7}, {"近 30 天", 30}} {
					if a.statsDays == opt.days {
						ui.PrimaryButton(c, opt.label).Disabled(a.busy != "")
					} else {
						ui.Button(c, opt.label).Disabled(a.busy != "").OnClick(func() { a.statsDays = opt.days; a.reload() })
					}
				}
			})
			maxReq := 1
			for _, day := range a.statsHistory.Days {
				if day.Requests > maxReq {
					maxReq = day.Requests
				}
			}
			for _, day := range a.statsHistory.Days {
				d := day
				ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
					if len(d.Date) >= 10 {
						muted(c, d.Date[5:]) // YYYY-MM-DD -> MM-DD
					}
					ui.Meter(c, float64(d.Requests), 0, float64(maxReq), nil).Label(shortNumber(int64(d.Requests)) + " 次请求").Grow(1).MinWidth(0)
					ui.Text(c, shortNumber(d.TotalTokens)+" tok").FontSize(12).TextColor(theme.TextMuted)
					if d.Errors > 0 {
						ui.Text(c, fmt.Sprintf("%d 错", d.Errors)).FontSize(12).TextColor(theme.Danger)
					}
				})
			}
			if len(a.statsHistory.Models) > 0 {
				ui.Divider(c)
				ui.Row(c).Gap(10).Children(func() {
					ui.Text(c, "模型用量").FontSize(13).Bold()
					ui.Spacer(c)
				})
				for i, m := range a.statsHistory.Models {
					if i >= 5 {
						break
					}
					ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
						ui.Text(c, m.Model).FontSize(13).SingleLine().Grow(1).MinWidth(0)
						if m.Provider != "" {
							muted(c, m.Provider)
						}
						ui.Text(c, shortNumber(m.TotalTokens)+" tok · "+strconv.Itoa(m.Requests)+" 次").FontSize(12).TextColor(theme.TextMuted)
					})
				}
			}
		})
	}

	// 服务健康与缓存卡片
	card(c, func() {
		ui.Row(c).Gap(24).AlignItems(ui.Center).Children(func() {
			ui.Column(c).Grow(1).Gap(6).Children(func() {
				ui.Row(c).AlignItems(ui.Center).Children(func() {
					ui.Text(c, "服务可用性").FontSize(13).TextColor(theme.TextMuted)
					ui.Spacer(c)
					ui.Textf(c, "%.1f%%", s.SuccessRate).FontSize(13).Bold()
				})
				ui.Meter(c, s.SuccessRate, 0, 100, &ui.MeterLevels{Critical: 80, Warning: 95}).Label("成功率").FillWidth()
			})
			if s.CacheHitRate != nil {
				ui.Column(c).Grow(1).Gap(6).Children(func() {
					ui.Row(c).AlignItems(ui.Center).Children(func() {
						ui.Text(c, "缓存命中率").FontSize(13).TextColor(theme.TextMuted)
						ui.Spacer(c)
						ui.Text(c, percent(s.CacheHitRate)).FontSize(13).Bold()
					})
					ui.Meter(c, *s.CacheHitRate, 0, 100, nil).Label("缓存命中率").FillWidth()
				})
			}
		})
		ui.Row(c).Gap(20).Children(func() {
			muted(c, "运行时间  "+s.Uptime)
			if s.CacheHitRate != nil {
				muted(c, "缓存读取  "+shortNumber(s.CacheRead))
			}
			if s.ActiveModels > 0 {
				muted(c, fmt.Sprintf("活跃模型  %d 个", s.ActiveModels))
			}
		})
	})

	tally := map[string]int{}
	for _, p := range a.dashboard.Providers {
		switch {
		case p.Status == "online":
			tally["online"]++
		case p.Status == "offline":
			tally["offline"]++
		case p.Status == "degraded":
			tally["degraded"]++
		default:
			tally["unknown"]++
		}
	}
	ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
		ui.Text(c, "上游健康").FontSize(18).Bold()
		if tally["online"] > 0 {
			chip(c, fmt.Sprintf("%d 在线", tally["online"]), okGreen)
		}
		if tally["degraded"] > 0 {
			chip(c, fmt.Sprintf("%d 降级", tally["degraded"]), theme.Accent)
		}
		if tally["offline"] > 0 {
			chip(c, fmt.Sprintf("%d 离线", tally["offline"]), theme.Danger)
		}
		if tally["unknown"] > 0 {
			chip(c, fmt.Sprintf("%d 未探测", tally["unknown"]), theme.TextMuted)
		}
		ui.Spacer(c)
		ui.SearchField(c, &a.overviewFilter).Placeholder("搜索 Provider...").Width(200)
	})

	if len(a.dashboard.Providers) == 0 {
		empty(c, "网关暂未配置 Provider")
		return
	}

	var providers []Provider
	filter := strings.ToLower(strings.TrimSpace(a.overviewFilter))
	for _, p := range a.dashboard.Providers {
		if filter != "" && !strings.Contains(strings.ToLower(p.Name), filter) && !strings.Contains(strings.ToLower(p.Status), filter) {
			continue
		}
		providers = append(providers, p)
	}

	if len(providers) == 0 {
		empty(c, "没有匹配的 Provider")
		return
	}

	if a.overviewSort.Column != "" {
		desc := a.overviewSort.Descending
		sort.SliceStable(providers, func(i, j int) bool {
			p1, p2 := providers[i], providers[j]
			if desc {
				p1, p2 = p2, p1
			}
			var less bool
			switch a.overviewSort.Column {
			case "Provider":
				less = p1.Name < p2.Name
			case "状态":
				less = p1.Status < p2.Status
			case "请求":
				less = p1.Requests < p2.Requests
			case "延迟":
				less = p1.AvgLatency < p2.AvgLatency
			default:
				return false
			}
			return less
		})
	} else {
		statusWeight := func(st string, req int) int {
			if req > 0 {
				return 4
			}
			switch st {
			case "degraded", "offline":
				return 3
			case "online":
				return 2
			default:
				return 1
			}
		}
		sort.SliceStable(providers, func(i, j int) bool {
			w1, w2 := statusWeight(providers[i].Status, providers[i].Requests), statusWeight(providers[j].Status, providers[j].Requests)
			if w1 != w2 {
				return w1 > w2
			}
			return providers[i].Name < providers[j].Name
		})
	}

	overviewCols := []ui.TableColumn{
		{Title: "Provider", Sortable: true},
		{Title: "状态", Width: 110, Sortable: true},
		{Title: "请求", Width: 90, Sortable: true},
		{Title: "延迟", Width: 110, Sortable: true},
	}
	a.overviewTable.Sort = &a.overviewSort
	ui.Table(c, &a.overviewTable, overviewCols, len(providers), func(row, col int) {
		p := providers[row]
		switch col {
		case 0:
			ui.Text(c, p.Name).Bold()
		case 1:
			status(c, p.Status)
		case 2:
			ui.Text(c, fmt.Sprint(p.Requests))
		case 3:
			ui.Text(c, formatLatency(p.AvgLatency))
		}
	}).Grow(1)
}
func (a *App) modelsView(c *ui.Context) {
	ui.Row(c).Wrap().Gap(8).Children(func() {
		ui.Button(c, "探活匿名模型").Disabled(a.busy != "").OnClick(a.probeAnonymousModels)
		ui.Button(c, "清理失败模型").Disabled(a.busy != "").OnClick(a.prunePreview)
		muted(c, "批量探活只调用匿名模型；付费与账号模型需单独验证。清理仅针对探活被确定性拒绝（401/402/403/404）的模型。")
	})
	ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
		ui.SearchField(c, &a.modelFilter).Label("搜索模型").Grow(1)
		if ui.Select(c, &a.modelCategory, modelCategories).Label("能力分类").Changed() {
			a.listViews.modelList = ui.ListState{}
			a.savePrefs()
		}
	})
	filtered, changed := a.listViews.models.apply(a.models, a.modelFilter, modelSearchText)
	if changed {
		a.listViews.modelList = ui.ListState{}
	}
	visible := []Model{}
	for i := 0; i < filtered.Len(); i++ {
		if m := filtered.At(i); modelInCategory(m, a.modelCategory) {
			visible = append(visible, m)
		}
	}
	if a.unloadedOrEmpty(c, len(visible), "没有匹配的模型") {
		return
	}
	ui.List(c, &a.listViews.modelList, len(visible), func(i int) {
		m := visible[i]
		ui.Column(c.Key(m.Name)).Children(func() {
			row(c, func() {
				ui.Row(c).Gap(12).Children(func() {
					ui.Column(c).Grow(1).Gap(6).Children(func() {
						ui.Text(c, m.Name).FontSize(18).Bold().Selectable()
						muted(c, m.Provider+" · "+strings.Join(m.Capabilities, " / "))
					})
					status(c, m.Status)
					ui.Button(c, "能力设置").Label(m.Name + " 能力设置").Disabled(a.busy != "").OnClick(func() {
						a.editorKind = "model"
						a.editorID = m.Name
						a.editorCaps = map[string]bool{}
						for _, cap := range m.Capabilities {
							a.editorCaps[cap] = true
						}
						a.editorModalities = strings.Join(m.Modalities, ", ")
						a.editorOpen = true
					})
					if chain := a.routeOrderFor(m.Name); len(chain) > 1 {
						// 只有存在多档回退链的模型（如 coding）才需要调整顺序；
						// 单 Provider 模型没有回退可言，不显示入口。
						ordered := append([]string(nil), chain...)
						ui.Button(c, "路由顺序").Label(m.Name + " 路由顺序").Disabled(a.busy != "").OnClick(func() {
							a.editorKind = "route"
							a.editorID = m.Name
							a.editorOrder = ordered
							a.editorOpen = true
						})
					}
				})
				meta(c, "请求 "+shortNumber(int64(m.Requests)), "Tokens "+shortNumber(m.Tokens), "延迟 "+formatLatency(m.AvgLatency), "缓存命中 "+percent(m.CacheHitRate))
				if m.Health.Reason != "" {
					muted(c, m.Health.Reason)
				}
				if m.Health.LastProbeAt != "" {
					muted(c, "探活时间 "+m.Health.LastProbeAt+" · 有效至 "+m.Health.ProbeExpiresAt)
				}
			})
		})
	}).Grow(1)
}
func (a *App) providersView(c *ui.Context) {
	if a.unloadedOrEmpty(c, len(a.providers), "没有 Provider") {
		return
	}
	ui.List(c, nil, len(a.providers), func(i int) {
		p := a.providers[i]
		ui.Column(c.Key(p.Name)).Children(func() {
			row(c, func() {
				ui.Row(c).Gap(12).Children(func() {
					ui.Text(c, p.Name).FontSize(18).Bold().Grow(1)
					status(c, p.Status)
					status(c, p.State)
					enabled := p.Enabled == nil || *p.Enabled
					if ui.Checkbox(c, &enabled, "启用").Label(p.Name + " 启用").Disabled(a.busy != "").Changed() {
						a.mutate("正在更新 Provider", "PATCH", "/providers/"+url.PathEscape(p.Name), map[string]any{"enabled": enabled})
					}
					ui.Button(c, "探测").Label("探测 " + p.Name).Disabled(a.busy != "").OnClick(func() {
						a.mutate("正在探测上游", "POST", "/providers/"+url.PathEscape(p.Name)+"/health-check", nil)
					})
					ui.Button(c, "重置熔断").Label("重置熔断 " + p.Name).Disabled(a.busy != "").OnClick(func() { a.mutate("正在重置熔断器", "POST", "/providers/"+url.PathEscape(p.Name)+"/reset", nil) })
				})
				meta(c, fmt.Sprintf("请求 %d", p.Requests), fmt.Sprintf("错误 %d", p.Errors), "延迟 "+formatLatency(p.AvgLatency))
				if p.HasProbe {
					ui.Text(c, "最近探测："+p.ProbeDetail).Selectable()
					muted(c, p.LastProbe)
				} else {
					muted(c, "尚未主动探测；状态仅反映熔断器。")
				}
				if p.NextRetry != "" {
					muted(c, "下次重试："+p.NextRetry)
				}
			})
		})
	}).Grow(1)
}
func (a *App) logsView(c *ui.Context) {
	limitIdx := 0
	switch a.logLimit {
	case "100":
		limitIdx = 1
	case "200":
		limitIdx = 2
	}
	ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
		ui.SearchField(c, &a.logFilter).Label("搜索日志").Grow(1)
		if ui.Segmented(c, &limitIdx, "50", "100", "200").Label("日志条数").Disabled(a.busy != "").Changed() {
			a.logLimit = []string{"50", "100", "200"}[limitIdx]
			a.reload()
		}
	})
	filtered, changed := a.listViews.logs.apply(a.logs, a.logFilter, logSearchText)
	if changed {
		a.logSelected = -1
		a.table = ui.ListState{}
	}
	if a.unloadedOrEmpty(c, filtered.Len(), "暂无匹配的请求日志") {
		return
	}
	cols := []ui.TableColumn{
		{Title: "时间", Width: 170, Sortable: true},
		{Title: "模型", Sortable: true},
		{Title: "Provider", Width: 150, Sortable: true},
		{Title: "状态", Width: 65, Sortable: true},
		{Title: "延迟", Width: 100, Sortable: true},
	}
	order := make([]int, filtered.Len())
	for i := range order {
		order[i] = i
	}
	if a.logSort.Column != "" {
		desc := a.logSort.Descending
		sort.SliceStable(order, func(i, j int) bool {
			aLog := filtered.At(order[i])
			bLog := filtered.At(order[j])
			if desc {
				aLog, bLog = bLog, aLog
			}
			var less bool
			switch a.logSort.Column {
			case "时间":
				less = aLog.Timestamp < bLog.Timestamp
			case "模型":
				less = aLog.Model < bLog.Model
			case "Provider":
				less = aLog.Provider < bLog.Provider
			case "状态":
				less = aLog.Code < bLog.Code
			case "延迟":
				less = aLog.Latency < bLog.Latency
			default:
				return false
			}
			return less
		})
	}
	a.table.Sort = &a.logSort
	a.table.Selected = &a.logSelected
	a.table.Key = func(i int) any {
		l := filtered.At(order[i])
		return l.RequestID + l.Timestamp + fmt.Sprint(order[i])
	}
	renderTable := func() {
		ui.Table(c, &a.table, cols, filtered.Len(), func(row, col int) {
			l := filtered.At(order[row])
			switch col {
			case 0:
				ui.Text(c, l.Timestamp).SingleLine()
			case 1:
				ui.Text(c, l.Model).SingleLine()
			case 2:
				ui.Text(c, l.Provider).SingleLine()
			case 3:
				ui.Text(c, fmt.Sprint(l.Code))
			case 4:
				ui.Text(c, formatLatency(l.Latency))
			}
		}).Grow(1).MinHeight(130)
	}
	renderDetail := func() {
		if a.logSelected < 0 || a.logSelected >= len(order) {
			return
		}
		l := filtered.At(order[a.logSelected])
		ui.Scroll(c).Grow(1).Gap(6).Padding(12).Background(c.Theme().Surface).Radius(8).Selectable().Children(func() {
			ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
				ui.Text(c, l.Method+" "+l.Path+" · "+l.RequestID).Bold()
				ui.Spacer(c)
				ui.Button(c, "复制 ID").OnClick(func() {
					if a.CopyText != nil {
						a.CopyText(l.RequestID)
						c.Toast("已复制 Request ID")
					}
				})
			})
			meta(c, "输入 "+shortNumber(l.Input), "输出 "+shortNumber(l.Output)+" tokens", "缓存读取 "+shortNumber(l.CacheRead), fmt.Sprintf("流式 %t", l.Stream))
			if l.Error != "" {
				ui.Text(c, l.Error).TextColor(c.Theme().Danger)
			}
			for _, p := range l.Attempts {
				ui.Row(c).Gap(10).Children(func() {
					ui.Text(c, p.Provider).SingleLine()
					muted(c, p.Status)
					muted(c, fmt.Sprintf("HTTP %d", p.Code))
					muted(c, formatLatency(p.Latency))
					if p.Error != "" {
						ui.Text(c, p.Error).TextColor(c.Theme().Danger).SingleLine()
					}
				})
			}
		})
	}
	if a.logSelected >= 0 && a.logSelected < len(order) {
		if a.logSplitHeight <= 0 {
			a.logSplitHeight = 360
		}
		ui.SplitVertical(c, &a.logSplitHeight, renderTable, renderDetail).Grow(1)
	} else {
		renderTable()
	}
}
