package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/egoist/mygo/ui"
	"github.com/nightgopher/litellm-gateway/desktop/internal/connection"
	"math"
	"sort"
	"strings"
	"time"
)

func (a *App) acceptSkills(d Skills) {
	a.skills = d
	a.targets = strings.Join(d.Targets, ", ")
	a.skillSelection = map[string]bool{}
	for _, id := range d.Enabled {
		a.skillSelection[id] = true
	}
	a.skillDirty = false
}
func sortedSelection(m map[string]bool) []string {
	ids := []string{}
	for id, v := range m {
		if v {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// skillsTagAll 是标签筛选里表示“不过滤”的哨兵选项。
const skillsTagAll = "全部标签"

// marketSectionAll 是市场分类筛选里表示“不过滤”的哨兵选项。
const marketSectionAll = "全部分类"

// marketSectionOptions 返回分类下拉选项：哨兵值 + 商城分类名。
func (a *App) marketSectionOptions() []string {
	if len(a.marketSections) == 0 {
		return nil
	}
	options := []string{marketSectionAll}
	for _, s := range a.marketSections {
		if s.Name != "" {
			options = append(options, s.Name)
		}
	}
	return options
}

// skillTagOptions 汇总技能目录里出现过的标签，供筛选下拉使用。
func (a *App) skillTagOptions() []string {
	tags := map[string]bool{}
	for _, s := range a.skills.Skills {
		for _, t := range s.Tags {
			if t != "" {
				tags[t] = true
			}
		}
	}
	if len(tags) == 0 {
		return nil
	}
	out := []string{skillsTagAll}
	for t := range tags {
		out = append(out, t)
	}
	sort.Strings(out[1:])
	return out
}

func skillHasTag(s Skill, tag string) bool {
	for _, t := range s.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

// skillInstallState 汇总一个技能在各安装目录的落盘状态和展示文案。
func (a *App) skillInstallState(s Skill) (text string, ok bool) {
	installed := 0
	for _, target := range a.skills.Targets {
		if s.Installed[target] {
			installed++
		}
	}
	switch {
	case len(a.skills.Targets) > 0 && installed == len(a.skills.Targets):
		return "已安装", true
	case installed > 0:
		return fmt.Sprintf("部分安装 %d/%d", installed, len(a.skills.Targets)), false
	default:
		return "未安装", false
	}
}

// 技能页的两种来源：技能库（custom-skills 仓库清单）与技能市场（远端商城）。
const (
	skillSourceRepo   = "技能库"
	skillSourceMarket = "技能市场"
)

func (a *App) skillsView(c *ui.Context) {
	sourceIdx := 0
	if a.skillSource == skillSourceMarket {
		sourceIdx = 1
	}
	ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
		if ui.Segmented(c, &sourceIdx, skillSourceRepo, skillSourceMarket).Label("技能来源").Changed() {
			if sourceIdx == 1 {
				a.skillSource = skillSourceMarket
			} else {
				a.skillSource = skillSourceRepo
			}
			a.loaded = false
			a.savePrefs()
			a.reload()
		}
		ui.Spacer(c)
		if a.skillSource == skillSourceRepo {
			ui.Textf(c, "%d 个已选", len(sortedSelection(a.skillSelection)))
		}
	})
	if a.notLoaded(c) {
		return
	}
	if a.skillSource == skillSourceMarket {
		a.marketView(c)
		return
	}
	a.repoSkillsView(c)
}

// marketView 渲染技能市场来源：搜索商城目录，安装到个人技能目录。
func (a *App) marketView(c *ui.Context) {
	if !a.market.Enabled {
		card(c, func() { ui.Text(c, "技能市场未启用").Bold(); muted(c, a.market.Hint) })
		return
	}
	ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
		ui.SearchField(c, &a.marketQuery).Label("搜索市场").Grow(1)
		if sections := a.marketSectionOptions(); len(sections) > 0 {
			if ui.Select(c, &a.marketSection, sections).Label("分类").Disabled(a.busy != "").Changed() {
				a.marketPage = 1
				a.listViews.marketList = ui.ListState{}
				a.savePrefs()
				a.reload()
			}
		}
		if ui.Select(c, &a.marketSort, []string{"推荐", "最多安装", "名称"}).Label("排序").Disabled(a.busy != "").Changed() {
			a.marketPage = 1
			a.reload()
		}
		ui.Button(c, "搜索").Label("搜索技能市场").Disabled(a.busy != "").OnClick(func() { a.marketPage = 1; a.reload() })
	})
	ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
		page := a.market.Page
		if page < 1 {
			page = 1
		}
		muted(c, fmt.Sprintf("共 %d 个技能 · 第 %d 页", a.market.Total, page))
		ui.Spacer(c)
		ui.Button(c, "上一页").Label("上一页市场结果").Disabled(a.busy != "" || a.market.Page <= 1).OnClick(func() { a.marketPage--; a.reload() })
		ui.Button(c, "下一页").Label("下一页市场结果").Disabled(a.busy != "" || len(a.market.Skills) == 0).OnClick(func() { a.marketPage++; a.reload() })
	})
	if len(a.market.Skills) == 0 {
		empty(c, "没有匹配的技能")
		return
	}
	ui.List(c, &a.listViews.marketList, len(a.market.Skills), func(i int) {
		a.marketRow(c, a.market.Skills[i])
	}).Grow(1)
}

// marketRow 渲染一条市场技能：安装/卸载与详情展开。
func (a *App) marketRow(c *ui.Context, s MarketSkill) {
	name := s.DisplayName
	if name == "" {
		name = s.Name
	}
	_, installedAlready := a.marketInstalledByName(s.Name)
	ui.Column(c.Key(fmt.Sprintf("market-%d", s.ID))).Children(func() {
		row(c, func() {
			ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
				ui.Text(c, name).Bold().Grow(1).MinWidth(0).SingleLine()
				if s.Version != "" {
					muted(c, "v"+s.Version)
				}
				if installedAlready {
					chip(c, "已安装", okGreen)
					ui.Button(c, "卸载").Label("卸载技能 " + name).Disabled(a.busy != "").OnClick(func() {
						a.editorTitle = "从本机卸载「" + name + "」？将删除其安装目录，不影响手写技能。"
						a.deleteAction = func() { a.uninstallMarketSkill(s.Name, name) }
						a.deleteOpen = true
					})
				} else {
					ui.PrimaryButton(c, "安装").Label("安装技能 " + name).Disabled(a.busy != "").OnClick(func() { a.installMarketSkill(s.ID, name) })
				}
				label := "详情"
				if a.marketExpanded[s.Name] {
					label = "收起"
				}
				ui.Button(c, label).Label("市场技能详情 " + name).OnClick(func() {
					if a.marketExpanded == nil {
						a.marketExpanded = map[string]bool{}
					}
					a.marketExpanded[s.Name] = !a.marketExpanded[s.Name]
				})
			})
			if a.marketExpanded[s.Name] {
				ui.Text(c, s.Description).Selectable()
				parts := []string{}
				if s.Category != "" {
					parts = append(parts, s.Category)
				}
				if len(s.Tags) > 0 {
					parts = append(parts, strings.Join(s.Tags, " / "))
				}
				parts = append(parts, fmt.Sprintf("%d 次安装", s.InstallCount))
				if s.SizeBytes > 0 {
					parts = append(parts, shortNumber(s.SizeBytes))
				}
				meta(c, parts...)
			} else if s.Description != "" {
				ui.Text(c, s.Description).MaxLines(1).TextColor(c.Theme().TextMuted)
			}
		})
	})
}

// installMarketSkill 提交网关侧后台安装任务并轮询进度：任务归网关所有，
// 切换连接、甚至关闭桌面端都不影响安装本身；轮询超时只说明“本端不再
// 等待”，安装仍在后台进行。
func (a *App) installMarketSkill(id int, name string) {
	dispatch := a.dispatch
	a.work("正在安装技能", 10*time.Minute, func(ctx context.Context, client *connection.Client) (func(), error) {
		seq := a.sequence
		var start struct {
			JobID string `json:"job_id"`
			Error string `json:"error"`
		}
		if err := client.JSON(ctx, "POST", "/skills/market/install-jobs", map[string]int{"id": id}, &start); err != nil {
			return nil, err
		}
		if start.Error != "" {
			return nil, errors.New(start.Error)
		}
		if start.JobID == "" {
			return nil, errors.New("网关未返回安装任务号")
		}

		for {
			var job struct {
				Job struct {
					Phase      string `json:"phase"`
					BytesDone  int64  `json:"bytes_done"`
					BytesTotal int64  `json:"bytes_total"`
					Error      string `json:"error"`
				} `json:"job"`
				Error string `json:"error"`
			}
			if err := client.JSON(ctx, "GET", "/skills/market/install-jobs/"+start.JobID, nil, &job); err != nil {
				return nil, err
			}
			if job.Error != "" {
				return nil, errors.New(job.Error)
			}
			// 进度文案在任务协程里算好成副本，dispatch 闭包只写 UI 状态，
			// 不与工作协程共享可变变量。
			busyText := ""
			switch job.Job.Phase {
			case "downloading":
				if job.Job.BytesTotal > 0 {
					busyText = fmt.Sprintf("正在下载技能 %s / %s", shortNumber(job.Job.BytesDone), shortNumber(job.Job.BytesTotal))
				} else {
					busyText = "正在下载技能 " + shortNumber(job.Job.BytesDone)
				}
			case "verifying":
				busyText = "正在校验安装包"
			case "extracting":
				busyText = "正在解包安装"
			}
			if busyText != "" {
				busyTextCopy := busyText
				dispatch(func() {
					if seq == a.sequence {
						a.busy = busyTextCopy
					}
				})
			}
			if job.Job.Phase == "failed" {
				return nil, errors.New(job.Job.Error)
			}
			if job.Job.Phase == "done" {
				break
			}
			select {
			case <-ctx.Done():
				// 本端停止等待；网关后台任务继续执行。
				return func() {
					a.notice = "安装仍在后台进行，稍后刷新技能列表查看结果"
				}, nil
			case <-time.After(time.Second):
			}
		}
		return func() {
			a.reload()
			a.notice = "技能「" + name + "」已安装"
		}, nil
	})
}

// uninstallMarketSkill 删除市场安装的技能目录（网关侧校验安装清单）。
func (a *App) uninstallMarketSkill(name, display string) {
	a.work("正在卸载技能", 60*time.Second, func(ctx context.Context, client *connection.Client) (func(), error) {
		var result struct {
			Error string `json:"error"`
		}
		if err := client.JSON(ctx, "POST", "/skills/market/uninstall", map[string]string{"name": name}, &result); err != nil {
			return nil, err
		}
		if result.Error != "" {
			return nil, errors.New(result.Error)
		}
		return func() { a.reload(); a.notice = "技能「" + display + "」已卸载" }, nil
	})
}

// modelPickerModal 助理模型选择器：搜索框即输即滤，列表虚拟化，
// 为上百个模型设计；当前选择带标记，空结果有明确提示。
// 过滤词在打开与每个关闭路径上重置（Esc/点背景关闭由下次打开兜底）。
func (a *App) modelPickerModal(c *ui.Context) {
	ui.Modal(c, &a.modelPickerOpen, func() {
		ui.Column(c).Width(640).Gap(14).Children(func() {
			ui.Text(c, "选择模型").FontSize(20).Bold()
			if ui.SearchField(c, &a.modelPickerFilter).Label("搜索模型").FillWidth().AutoFocus().Changed() {
				a.modelPickerList = ui.ListState{}
			}
			models := a.filteredChatModels()
			muted(c, fmt.Sprintf("共 %d 个模型，显示 %d 个", len(a.chatModels)+1, len(models)))
			if len(models) == 0 {
				empty(c, "没有匹配的模型")
			} else {
				ui.List(c, &a.modelPickerList, len(models), func(i int) {
					name := models[i]
					current := name == a.chatModel
					ui.Row(c.Key(name)).Gap(10).AlignItems(ui.Center).Padding(0, 0, 4).Children(func() {
						ui.Button(c, name).Label("选择模型 " + name).Grow(1).OnClick(func() {
							a.chatModel = name
							a.closeModelPicker()
							a.savePrefs()
						})
						if channel := a.chatModelOptions[name].Channel; channel != "" {
							muted(c, channel)
						}
						if current {
							chip(c, "当前", okGreen)
						}
					})
				}).Grow(1).MinHeight(200).MaxHeight(420)
			}
			ui.Row(c).Gap(8).Justify(ui.End).Children(func() {
				ui.Button(c, "取消").OnClick(a.closeModelPicker)
			})
		})
	})
}

func (a *App) closeModelPicker() {
	a.modelPickerOpen = false
	a.modelPickerFilter = ""
	a.modelPickerList = ui.ListState{}
}

func (a *App) repoSkillsView(c *ui.Context) {
	if !a.skills.Configured {
		card(c, func() { ui.Text(c, "技能库尚未配置").Bold(); muted(c, a.skills.Hint) })
		return
	}
	ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
		ui.SearchField(c, &a.skillFilter).Label("搜索技能").Grow(1)
		if tags := a.skillTagOptions(); len(tags) > 0 {
			ui.Select(c, &a.skillTag, tags).Label("标签筛选").Disabled(a.busy != "")
		}
	})
	// 安装范围（全局目录或项目路径）是低频配置，默认收起；摘要常驻显示。
	ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
		var summary string
		switch {
		case a.loadedProject != "":
			summary = "项目范围 · " + a.loadedProject
		case len(split(a.targets)) > 0:
			summary = "全局范围 · " + strings.Join(split(a.targets), ", ")
		default:
			summary = "全局范围 · 未设置安装目录"
		}
		ui.Text(c, summary).TextColor(c.Theme().TextMuted).SingleLine()
		ui.Spacer(c)
		if a.skillScopeOpen {
			ui.Button(c, "收起").Label("收起安装范围设置").OnClick(func() { a.skillScopeOpen = false })
		} else {
			ui.Button(c, "安装范围").Label("展开安装范围设置").OnClick(func() { a.skillScopeOpen = true })
		}
	})
	if a.skillScopeOpen {
		card(c, func() {
			ui.Text(c, "安装范围").Bold()
			ui.Row(c).Gap(10).Children(func() {
				ui.TextInput(c, &a.project).Grow(1).Label("项目路径").Placeholder("留空管理全局技能；填写网关主机上的项目绝对路径").Disabled(a.busy != "" || a.skillDirty)
				if a.localConnection() && a.PickFolder != nil {
					ui.Button(c, "本机选择").Label("本机选择目录").Disabled(a.busy != "" || a.skillDirty).OnClick(a.pickLocalFolder)
				}
				ui.Button(c, "浏览").Label("浏览项目路径").Disabled(a.busy != "" || a.skillDirty).OnClick(a.openBrowser)
				ui.Button(c, "应用").Label("应用读取范围").Disabled(a.busy != "" || a.skillDirty).OnClick(func() { a.loadedProject = strings.TrimSpace(a.project); a.loaded = false; a.reload() })
			})
			if a.loadedProject != "" {
				muted(c, "项目范围："+a.loadedProject+"（技能安装到该项目的 .agents/skills）")
			} else {
				ui.Row(c).Gap(10).Children(func() {
					if ui.TextInput(c, &a.targets).Grow(1).Label("全局安装目录").Disabled(a.busy != "").Changed() {
						a.skillDirty = true
					}
				})
				muted(c, "多个安装目录用逗号分隔；改动安装目录需保存后生效。")
			}
		})
	}
	filtered, changed := a.listViews.skills.apply(a.skills.Skills, a.skillFilter, skillSearchText)
	if changed {
		a.listViews.skillList = ui.ListState{}
	}
	visible := []Skill{}
	for i := 0; i < filtered.Len(); i++ {
		if s := filtered.At(i); a.skillTag == skillsTagAll || skillHasTag(s, a.skillTag) {
			visible = append(visible, s)
		}
	}
	if len(a.skills.Skills) == 0 {
		empty(c, "技能库中暂无技能")
	} else if len(visible) == 0 {
		empty(c, "没有匹配的技能")
	} else {
		ui.List(c, &a.listViews.skillList, len(visible), func(i int) {
			a.skillRow(c, visible[i])
		}).Grow(1)
	}
	if len(a.skills.Local) > 0 {
		muted(c, fmt.Sprintf("另有 %d 个本地自定义技能，同步时保留。", len(a.skills.Local)))
	}
	ui.Row(c).Gap(8).Children(func() {
		ui.Column(c).Grow(1).Children(func() {
			muted(c, a.skills.Manifest)
			if a.skillDirty {
				ui.Text(c, "有未保存的选择；切换安装范围前请保存或撤销。").TextColor(c.Theme().Accent)
			}
		})
		ui.Button(c, "撤销选择").Disabled(a.busy != "" || !a.skillDirty).OnClick(func() { a.acceptSkills(a.skills) })
		ui.PrimaryButton(c, "保存并同步").Disabled(a.busy != "" || len(split(a.targets)) == 0).OnClick(a.saveSkills)
	})
}

// skillRow 渲染单个技能行：勾选启用、紧凑摘要，详情按需展开。
func (a *App) skillRow(c *ui.Context, s Skill) {
	name := s.Name
	if name == "" {
		name = s.ID
	}
	if s.Emoji != "" {
		name = s.Emoji + " " + name
	}
	stateText, installed := a.skillInstallState(s)
	ui.Column(c.Key(s.ID)).Children(func() {
		row(c, func() {
			ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
				value := a.skillSelection[s.ID]
				if ui.Checkbox(c, &value, name).Grow(1).Disabled(a.busy != "").Changed() {
					a.skillSelection[s.ID] = value
					a.skillDirty = true
				}
				if installed {
					chip(c, stateText, okGreen)
				} else if stateText != "未安装" {
					chip(c, stateText, c.Theme().Accent)
				} else {
					muted(c, stateText)
				}
				label := "详情"
				if a.skillExpanded[s.ID] {
					label = "收起"
				}
				ui.Button(c, label).Label("技能详情 " + s.ID).OnClick(func() {
					if a.skillExpanded == nil {
						a.skillExpanded = map[string]bool{}
					}
					a.skillExpanded[s.ID] = !a.skillExpanded[s.ID]
				})
			})
			if a.skillExpanded[s.ID] {
				ui.Text(c, s.Description).Selectable()
				if len(s.Tags) > 0 {
					ui.Row(c).Wrap().Gap(8).Children(func() {
						for _, tag := range s.Tags {
							chip(c, tag, c.Theme().TextMuted)
						}
					})
				}
				parts := []string{}
				if s.Author != "" {
					parts = append(parts, "作者 "+s.Author)
				}
				if s.Updated != "" {
					parts = append(parts, "更新 "+s.Updated)
				}
				if s.GithubURL != "" {
					parts = append(parts, s.GithubURL)
				}
				if len(parts) > 0 {
					meta(c, parts...)
				}
			} else if s.Description != "" {
				ui.Text(c, s.Description).MaxLines(1).TextColor(c.Theme().TextMuted)
			}
		})
	})
}

// saveSkills 保存启用清单（如有改动）并同步符号链接。同步是幂等的
// 目录对齐，因此不再单独提供“仅保存清单”；修复链接也走同一按钮。
func (a *App) saveSkills() {
	body := map[string]any{"targets": split(a.targets), "enabled": sortedSelection(a.skillSelection)}
	query := skillsQuery(a.loadedProject)
	dirty := a.skillDirty
	a.work("正在保存技能", 60*time.Second, func(ctx context.Context, c *connection.Client) (func(), error) {
		if dirty {
			var saved Skills
			if err := c.JSON(ctx, "PUT", "/skills/config"+query, body, &saved); err != nil {
				return nil, err
			}
			if saved.Error != "" {
				return nil, errors.New(saved.Error)
			}
		}
		var result Skills
		if err := c.JSON(ctx, "POST", "/skills/sync"+query, nil, &result); err != nil {
			return nil, err
		}
		if result.Error != "" {
			return nil, errors.New(result.Error)
		}
		return func() {
			a.acceptSkills(result)
			a.notice = "技能同步完成"
			if result.Sync != nil {
				for _, report := range result.Sync.Targets {
					if len(report.Errors) > 0 {
						a.problem += report.Target + "：" + strings.Join(report.Errors, "；") + "\n"
					}
					if len(report.Skipped) > 0 {
						a.notice += "；保留 " + strings.Join(report.Skipped, ", ")
					}
				}
			}
		}, nil
	})
}
func (a *App) memoryView(c *ui.Context) {
	ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
		ui.Text(c, "状态筛选").TextColor(c.Theme().TextMuted)
		if ui.Select(c, &a.memoryFilter, []string{"all", "candidate", "active", "retired"}).Label("记忆状态").Disabled(a.busy != "").Changed() {
			a.reload()
		}
		ui.Spacer(c)
		muted(c, fmt.Sprintf("最近 %d 条", len(a.memories)))
	})
	ui.List(c, nil, len(a.memories), func(i int) {
		m := a.memories[i]
		ui.Column(c.Key(fmt.Sprint(m.ID))).Padding(4, 0).Children(func() {
			card(c, func() {
				ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
					status(c, m.Status)
					muted(c, memoryScopeLabel(m))
					ui.Spacer(c)
					meta(c, m.Updated)
					if m.Status == "candidate" {
						ui.Button(c, "确认生效").Label(fmt.Sprintf("确认记忆 %d", m.ID)).Disabled(a.busy != "").OnClick(func() { a.mutate("正在确认记忆", "POST", fmt.Sprintf("/memories/%d/confirm", m.ID), nil) })
					}
					if m.Status != "retired" {
						ui.Button(c, "停用").Label(fmt.Sprintf("停用记忆 %d", m.ID)).Disabled(a.busy != "").OnClick(func() { a.mutate("正在停用记忆", "POST", fmt.Sprintf("/memories/%d/retire", m.ID), nil) })
					}
					ui.Button(c, "删除").Label(fmt.Sprintf("删除记忆 %d", m.ID)).Disabled(a.busy != "").OnClick(func() {
						a.editorTitle = "永久删除这条记忆，无法撤销：\n" + m.Statement
						a.deleteAction = func() { a.mutate("正在删除记忆", "DELETE", fmt.Sprintf("/memories/%d", m.ID), nil) }
						a.deleteOpen = true
					})
				})
				ui.Text(c, m.Statement).Selectable()
				meta(c, "来源 "+m.Source, fmt.Sprintf("置信度 %.2f", m.Confidence), fmt.Sprintf("命中 %d 次", m.Hits))
			})
		})
	}).Grow(1).Children(func() {
		if a.unloadedOrEmpty(c, len(a.memories), "没有此状态的记忆") {
			return
		}
	})
	card(c, func() {
		ui.Text(c, "添加记忆").Bold()
		scopeIdx := 0
		switch a.memoryScope {
		case "client":
			scopeIdx = 1
		case "project":
			scopeIdx = 2
		}
		ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
			if ui.Segmented(c, &scopeIdx, "global", "client", "project").Label("记忆范围").Disabled(a.busy != "").Changed() {
				a.memoryScope = []string{"global", "client", "project"}[scopeIdx]
			}
			switch a.memoryScope {
			case "client":
				ui.Combobox(c, &a.memoryKey, a.clientSuggestions()).Grow(1).Label("客户端")
			case "project":
				ui.TextInput(c, &a.memoryKey).Label("项目路径").Placeholder("网关主机上的项目绝对路径").Grow(1).Disabled(a.busy != "")
			default:
				muted(c, "全局记忆对所有客户端生效，不需要范围键")
			}
		})
		if a.memoryScope == "project" {
			ui.Row(c).Gap(10).Children(func() {
				if a.localConnection() && a.PickFolder != nil {
					ui.Button(c, "本机选择记忆项目").Disabled(a.busy != "").OnClick(a.pickMemoryProjectFolder)
				}
				ui.Button(c, "浏览记忆项目").Disabled(a.busy != "").OnClick(func() { a.openBrowserFor("memory", a.memoryKey) })
			})
		}
		ui.TextInput(c, &a.memoryText).Label("记忆内容").Placeholder("输入希望助理记住的事实").Disabled(a.busy != "")
		ui.Row(c).Justify(ui.End).Children(func() {
			ui.PrimaryButton(c, "添加记忆").Disabled(a.busy != "" || strings.TrimSpace(a.memoryText) == "" || (a.memoryScope != "global" && strings.TrimSpace(a.memoryKey) == "")).OnClick(a.addMemory)
		})
	})
}

// pickMemoryProjectFolder 用原生面板（sheet 挂主窗口）选择记忆的项目
// 路径；取消或未注入面板能力时保持原值不变。
func (a *App) pickMemoryProjectFolder() {
	if a.PickFolder == nil {
		return
	}
	a.PickFolder(func(path string, ok bool) {
		if !ok {
			return
		}
		if trimmed := strings.TrimSpace(path); trimmed != "" {
			a.dispatch(func() { a.memoryKey = trimmed })
		}
	})
}
func (a *App) addMemory() {
	key := strings.TrimSpace(a.memoryKey)
	if a.memoryScope == "global" {
		key = ""
	}
	body := map[string]any{"scope_type": a.memoryScope, "scope_key": key, "statement": strings.TrimSpace(a.memoryText), "confidence": 1}
	a.work("正在添加记忆", 35*time.Second, func(ctx context.Context, c *connection.Client) (func(), error) {
		err := c.JSON(ctx, "POST", "/memories", body, nil)
		return func() { a.memoryText = ""; a.reload(); a.notice = "记忆已添加" }, err
	})
}
func (a *App) settingsView(c *ui.Context) {
	ui.Scroll(c).Grow(1).Gap(16).Children(func() {
		card(c, func() {
			ui.Text(c, "助理人设").FontSize(18).Bold()
			muted(c, "留空使用默认提示词。配置保存在当前连接的网关主机。")
			if a.promptErr != "" {
				ui.Text(c, a.promptErr).TextColor(c.Theme().Danger)
			}
			ui.TextArea(c, &a.promptDraft).Height(150).Label("自定义助理提示词").Placeholder(a.prompt.Default).Disabled(a.busy != "")
			ui.Row(c).Gap(8).Children(func() {
				ui.Button(c, "恢复默认").Disabled(a.busy != "").OnClick(func() { a.promptDraft = "" })
				ui.PrimaryButton(c, "保存人设").Disabled(a.busy != "").OnClick(func() {
					draft := a.promptDraft
					a.work("正在保存人设", 35*time.Second, func(ctx context.Context, client *connection.Client) (func(), error) {
						var p Prompt
						err := client.JSON(ctx, "PUT", "/assistant/prompt", map[string]string{"prompt": draft}, &p)
						return func() { a.prompt = p; a.notice = "人设已保存" }, err
					})
				})
			})
		})
		a.ccswitchView(c)
		ui.Text(c, "同步模型到客户端").FontSize(18).Bold()
		muted(c, "写入当前网关主机上的客户端配置；其他客户端配置按服务端规则保留。")
		for _, target := range []string{"pi", "zcode", "harness"} {
			cfg := a.configs[target]
			choices := a.configSelection[target]
			display := clientTargetNames[target]
			ui.Column(c.Key(target)).Children(func() {
				card(c, func() {
					ui.Row(c).Children(func() {
						ui.Text(c, display).FontSize(18).Bold()
						ui.Spacer(c)
						if configInSync(cfg, choices) {
							chip(c, "已同步", okGreen)
						} else {
							chip(c, "待同步", c.Theme().Accent)
						}
					})
					if cfg.Error != "" {
						ui.Text(c, cfg.Error).TextColor(c.Theme().Danger)
						return
					}
					ui.Text(c, cfg.Path).Selectable().TextColor(c.Theme().TextMuted)
					ui.Row(c).Wrap().Gap(12).Children(func() {
						for _, m := range cfg.Desired {
							value := choices[m.ID]
							if ui.Checkbox(c.Key(m.ID), &value, m.ID).Disabled(a.busy != "").Changed() {
								choices[m.ID] = value
							}
						}
					})
					removed := []string{}
					for _, id := range cfg.Current {
						if !choices[id] {
							removed = append(removed, id)
						}
					}
					if len(removed) > 0 {
						muted(c, "同步后移除："+strings.Join(removed, ", "))
					}
					if len(cfg.Skipped) > 0 {
						muted(c, "跳过缺失条目："+strings.Join(cfg.Skipped, ", "))
					}
					ui.Row(c).Gap(8).Children(func() {
						ui.Button(c, "全选").Label("全选 " + display).Disabled(a.busy != "").OnClick(func() {
							for _, m := range cfg.Desired {
								choices[m.ID] = true
							}
						})
						ui.Button(c, "清空").Label("清空 " + display).Disabled(a.busy != "").OnClick(func() {
							for id := range choices {
								choices[id] = false
							}
						})
						ui.PrimaryButton(c, "同步 "+display).Disabled(a.busy != "" || len(sortedSelection(choices)) == 0).OnClick(func() {
							ids := sortedSelection(choices)
							a.work("正在同步 "+target, 35*time.Second, func(ctx context.Context, client *connection.Client) (func(), error) {
								var result ClientConfig
								err := client.JSON(ctx, "POST", "/"+target+"/sync", map[string]any{"model_ids": ids}, &result)
								if err == nil && result.Error != "" {
									err = errors.New(result.Error)
								}
								return func() { a.configs[target] = result; a.notice = target + " 模型配置已同步" }, err
							})
						})
					})
				})
			})
		}
		a.gatewaySettingsView(c)
	})
}

// clientTargetNames 是设置页客户端同步面板的显示名；pi 目标即自研
// EasyAgent 的 models.json，按产品名展示。
var clientTargetNames = map[string]string{
	"pi":      "EasyAgent",
	"zcode":   "ZCode",
	"harness": "Harness",
}

// ccswitchView 渲染「从 cc-switch 迁移」面板：扫描预览、勾选与一键导入。
// 扫描在网关主机上执行（默认 ~/.cc-switch，可指定路径）；没有 cc-switch 或
// 网关版本过低时给出明确指引而不是报错堆栈。
func (a *App) ccswitchView(c *ui.Context) {
	theme := c.Theme()
	imported := a.ccImportedNames()
	ui.Text(c, "从 cc-switch 迁移").FontSize(18).Bold()
	muted(c, "读取网关主机上的 cc-switch 配置，把已配置的供应商与模型迁入网关；cc-switch 数据只读不动，密钥只写入网关 .env。")
	card(c, func() {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "cc-switch 供应商").Bold()
			if a.ccRemote {
				chip(c, "本机上传", theme.Accent)
			}
			ui.Spacer(c)
			if a.ccswitch.Error != "" {
				chip(c, "未就绪", theme.Danger)
			} else if a.ccswitch.Importable > 0 {
				chip(c, fmt.Sprintf("%d 可导入 · %d 跳过", a.ccswitch.Importable, a.ccswitch.Skipped), okGreen)
			} else {
				chip(c, "没有可导入条目", theme.TextMuted)
			}
		})

		if a.ccswitch.Error != "" {
			ui.Text(c, a.ccswitch.Error).TextColor(theme.Danger)
			muted(c, "扫描的是网关主机上的 ~/.cc-switch，那里没有找到 cc-switch。cc-switch 装在这台电脑上时可直接上传迁移；装在其他机器时，可在那台机器的网关目录运行 llm-gateway migrate cc-switch，或在下方填写网关主机上的配置目录重试。")
			ui.Row(c).Gap(8).Children(func() {
				ui.PrimaryButton(c, "从本机 cc-switch 上传").Disabled(a.busy != "").OnClick(func() { a.uploadLocalCCSwitch() })
			})
		} else if a.ccswitch.Source != "" {
			ui.Text(c, a.ccswitch.Source).Selectable().TextColor(theme.TextMuted)
			if a.ccRemote {
				muted(c, "条目来自这台电脑的 cc-switch；带「回环地址」标记的上游仅在网关主机可达时有效。")
			}
		}

		if len(imported) > 0 {
			names := make([]string, 0, len(imported))
			for name := range imported {
				names = append(names, name)
			}
			sort.Strings(names)
			ui.Text(c, "本次已导入："+strings.Join(names, "、")+"，模型即刻可用（见「模型」页）。").TextColor(okGreen)
			if len(a.ccUpdatedKeys) > 0 {
				ui.Text(c, "其中部分密钥与 cc-switch 不一致，已更新并即时生效。").TextColor(okGreen)
			}
			ui.Row(c).Gap(8).Children(func() {
				ui.Button(c, "体检导入供应商").Disabled(a.busy != "" || len(a.ccImportedModels) == 0).OnClick(func() {
					a.checkImportedProviders()
				})
				ui.Button(c, "一键同步客户端").Disabled(a.busy != "").OnClick(func() {
					a.syncAllClients()
				})
			})
		}

		importable := 0
		for _, candidate := range a.ccswitch.Candidates {
			candidate := candidate
			if imported[candidate.Name] {
				ui.Column(c.Key("imported-" + candidate.Name)).Children(func() {
					ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
						ui.Text(c, "✓ "+candidate.CCName+" 已导入").TextColor(okGreen)
						if candidate.APIKeyEnv != "" && a.ccUpdatedKeys[candidate.APIKeyEnv] {
							chip(c, "密钥已更新", theme.Accent)
						}
						if status := a.ccHealth[candidate.Name]; status != "" {
							switch status {
							case "online":
								chip(c, "在线", okGreen)
							case "offline":
								chip(c, "离线", theme.Danger)
							case "degraded":
								chip(c, "降级", theme.Danger)
							default:
								chip(c, "未知", theme.TextMuted)
							}
						}
					})
					muted(c, candidate.URL)
				})
				continue
			}
			if candidate.Status != "importable" {
				continue
			}
			importable++
			ui.Column(c.Key(candidate.Name)).Children(func() {
				ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
					value := a.ccSelection[candidate.Name]
					if ui.Checkbox(c.Key("check"), &value, candidate.CCName).Disabled(a.busy != "").Changed() {
						a.ccSelection[candidate.Name] = value
					}
					chip(c, candidate.AppType, theme.TextMuted)
					if candidate.Type != "" {
						chip(c, candidate.Type, theme.Accent)
					}
					if candidate.Loopback {
						chip(c, "回环地址", theme.Danger)
					}
					ui.Spacer(c)
				})
				ui.Text(c, candidate.URL).Selectable().TextColor(theme.TextMuted)
				parts := []string{}
				if len(candidate.Models) > 0 {
					parts = append(parts, "模型："+strings.Join(candidate.Models, ", "))
				}
				if candidate.APIKeyEnv != "" {
					parts = append(parts, "密钥："+candidate.APIKeyEnv)
				}
				if len(parts) > 0 {
					muted(c, strings.Join(parts, " · "))
				}
			})
		}

		skipped := 0
		for _, candidate := range a.ccswitch.Candidates {
			if candidate.Status == "importable" || imported[candidate.Name] {
				continue
			}
			skipped++
			if skipped <= 8 {
				muted(c, "✘ "+candidate.CCName+"："+candidate.Reason)
			}
		}
		if skipped > 8 {
			muted(c, fmt.Sprintf("……另有 %d 条跳过", skipped-8))
		}

		if importable > 0 || a.ccswitch.Error == "" {
			ui.Row(c).Gap(8).Children(func() {
				ui.Button(c, "全选").Disabled(a.busy != "").OnClick(func() {
					for _, candidate := range a.ccswitch.Candidates {
						if candidate.Status == "importable" {
							a.ccSelection[candidate.Name] = true
						}
					}
				})
				ui.Button(c, "清空").Disabled(a.busy != "").OnClick(func() {
					for name := range a.ccSelection {
						a.ccSelection[name] = false
					}
				})
				ui.PrimaryButton(c, fmt.Sprintf("迁移所选（%d）", len(a.ccSelectedNames()))).
					Disabled(a.busy != "" || len(a.ccSelectedNames()) == 0).
					OnClick(func() { a.importCCSwitch() })
			})
		}

		ui.Row(c).Gap(8).Children(func() {
			ui.TextInput(c, &a.ccHomeDraft).Grow(1).Label("cc-switch 目录（可选）").
				Placeholder("默认网关主机 ~/.cc-switch；可填其他绝对路径").Disabled(a.busy != "")
			ui.Button(c, "重新扫描").Disabled(a.busy != "").OnClick(func() {
				a.ccImported = nil
				a.ccUpload = nil
				a.ccRemote = false
				a.reload()
			})
		})
	})
}
func bubble(c *ui.Context, who, text string, user bool) {
	theme := c.Theme()
	background := theme.Surface
	labelColor := theme.TextMuted
	if user {
		background = theme.Accent.Alpha(.08)
		labelColor = theme.Accent
	}
	ui.Column(c).Padding(14).Gap(6).Background(background).Border(1, theme.Border).Radius(10).Children(func() {
		ui.Text(c, who).Bold().FontSize(13).TextColor(labelColor)
		ui.Text(c, text).Selectable()
	})
}
func (a *App) assistantView(c *ui.Context) {
	ui.Row(c).Wrap().Gap(12).AlignItems(ui.Center).Children(func() {
		// 模型目录上百条：搜索选择走弹窗（Combobox 点击后输入会追加
		// 到当前值上，无匹配时弹层静默消失，体验不可接受）。
		if a.chatModel == "" {
			a.chatModel = chatDefaultOption
		}
		ui.Button(c, "模型 · "+a.chatModel).Label("选择模型，当前 " + a.chatModel).Disabled(a.busy != "").OnClick(func() {
			a.modelPickerFilter = ""
			a.reload()
			a.modelPickerList = ui.ListState{}
			a.modelPickerOpen = true
		})
		if a.chatDefault != "" && a.chatModel == chatDefaultOption {
			muted(c, "默认 "+a.chatDefault)
		}
		ui.Spacer(c)
		if a.chatReply != "" {
			ui.Button(c, "复制回复").OnClick(func() { c.WriteClipboard(a.chatReply) })
		}
		ui.Button(c, "清空对话").Disabled(a.busy != "" || (a.chatUser == "" && a.chatReply == "")).OnClick(func() {
			a.chatUser, a.chatReply, a.chatStatus = "", "", ""
			a.chatMarkdown = markdownDocument{}
			a.chatList = ui.ScrollState{}
		})
	})
	ui.Scroll(c).Key("assistant-transcript").Grow(1).TrackScroll(&a.chatList).Padding(8, 14, 8, 0).Gap(20).Selectable().Children(func() {
		if a.chatUser == "" {
			ui.Text(c, "你的网关助理").FontSize(22).Bold()
			muted(c, "可以询问模型、路由与运行状态，或整理工作区记忆。")
			ui.Row(c).Wrap().Gap(8).Children(func() {
				for _, suggestion := range []string{"今天网关运行得怎么样？", "哪些 Provider 熔断了？", "总结待确认的记忆"} {
					prompt := suggestion
					ui.Button(c, prompt).OnClick(func() { a.chatInput = prompt })
				}
			})
		}
		if a.chatUser != "" {
			bubble(c, "你", a.chatUser, true)
		}
		if a.chatReply != "" {
			ui.Column(c).Key("assistant-reply").Gap(10).MinWidth(0).Children(func() {
				ui.Text(c, "助理").Bold().FontSize(13).TextColor(c.Theme().TextMuted)
				a.chatMarkdown.render(c, a.chatReply, a.busy == "助理回复中")
			})
		}
	})
	// Let the native editor insert Shift+Enter at the caret, with selection and undo.
	input := ui.TextArea(c.Key("assistant-composer"), &a.chatInput).Lines(2, 6).Label("助理消息").Placeholder("询问模型、路由或运行状态…").Disabled(a.busy != "")
	composing := input.Composing()
	input.OnTextKey(func(k ui.TextKey) bool {
		if composing || a.busy != "" || k.Key != ui.KeyEnter || (k.Mods != 0 && k.Mods != ui.Cmd) {
			return false
		}
		a.chatInput = k.Text()
		a.sendChat()
		k.SetText(a.chatInput)
		return true
	})
	ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
		if a.chatStatus != "" {
			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				if a.busy == "助理回复中" {
					ui.Spinner(c).Size(12, 12)
				}
				ui.Text(c, a.chatStatus).FontSize(12).TextColor(c.Theme().TextMuted)
			})
		}
		ui.Spacer(c)
		ui.Text(c, "Enter 发送 · Shift+Enter 换行").FontSize(12).TextColor(c.Theme().TextMuted)
		if a.busy == "助理回复中" {
			ui.Button(c, "停止回复").OnClick(a.stop)
		} else {
			ui.PrimaryButton(c, "发送").Disabled(a.busy != "" || strings.TrimSpace(a.chatInput) == "").OnClick(a.sendChat)
		}
	})
}
func (a *App) sendChat() {
	if a.busy != "" {
		return
	}
	message := strings.TrimSpace(a.chatInput)
	if message == "" {
		return
	}
	a.chatUser = message
	a.chatInput = ""
	a.chatReply = ""
	a.chatStatus = "正在回复"
	a.chatList.Y = math.MaxFloat32
	seq := a.sequence + 1
	dispatch := a.dispatch
	a.work("助理回复中", 10*time.Minute, func(ctx context.Context, c *connection.Client) (func(), error) {
		done := false
		var streamError error
		size := 0
		body := map[string]string{"message": message}
		if model := a.chatModelParam(); model != "" {
			body["model"] = model
		}
		err := c.Stream(ctx, "/assistant/chat", body, func(data []byte) bool {
			var e Event
			if json.Unmarshal(data, &e) != nil {
				return true
			}
			size += len(e.Content)
			if size > 8<<20 {
				return false
			}
			dispatch(func() {
				if seq != a.sequence {
					return
				}
				follow := a.chatList.Y >= a.chatList.MaxY
				switch e.Type {
				case "text_delta":
					a.chatReply += e.Content
				case "tool_start":
					a.chatStatus = "正在调用 " + e.Tool
				case "tool_end":
					a.chatStatus = e.Tool + " 已完成"
				case "error":
					a.problem = e.Content
				case "done":
					if e.Content != "" {
						a.chatReply = e.Content
					}
					a.chatStatus = "回复完成"
				}
				if follow {
					a.chatList.Y = math.MaxFloat32
				}
			})
			if e.Type == "error" {
				streamError = errors.New("助理请求失败：" + e.Content)
			}
			if e.Type == "done" || e.Type == "error" {
				done = true
				return false
			}
			return true
		})
		if err == nil && streamError != nil {
			err = streamError
		}
		if err == nil && !done {
			err = errors.New("助理流式回复未完成，请重试")
		}
		return func() { a.chatStatus = "回复完成" }, err
	})
}
