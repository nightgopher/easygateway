// Package modelsprune 实现「清理失败模型」：把最近一次探活被上游确定性
// 拒绝（401/402/403/404，见 provider.classifyProbeStatus）的模型从
// providers.yaml 移除，并清理 chains 里的悬空引用。
//
// 判据刻意保守：只认**未过期**的 ProbeOffline——429、5xx、网络抖动归
// degraded，可能是暂时受限，不参与清理；从未探活的模型不动。
//
// 与 providerpresets 一样做**文本级手术**（不解析后重序列化，注释与键序
// 原样保留）：模型块 = `      - id: X` 起至下一个同级条目或缩进回落，条目
// 上方紧邻的同缩进注释视为该条目的说明一并移除；模型清空后的 provider
// 整块移除；chains 下指向已删模型的引用行同步删除。产出由调用方先经真实
// 加载器解析验证再落盘。
package modelsprune

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/nightgopher/easygateway/internal/provider"
)

// Candidate 是一个待清理的失败模型。
type Candidate struct {
	Model    string `json:"model"`
	Provider string `json:"provider"`
	Reason   string `json:"reason"`
}

// Skipped 是未参与清理的模型及原因。
type Skipped struct {
	Model  string `json:"model"`
	Reason string `json:"reason"`
}

// ChainImpact 记录一条链受清理影响后的剩余成员（可能为空，空链在运行时
// 按未知模型报错，报告给用户自行处置）。
type ChainImpact struct {
	Chain     string   `json:"chain"`
	Remaining []string `json:"remaining"`
}

// Result 是一次清理的结果摘要。
type Result struct {
	Removed          []Candidate   `json:"removed"`
	Skipped          []Skipped     `json:"skipped"`
	ProvidersRemoved []string      `json:"providers_removed"`
	ChainsAffected   []ChainImpact `json:"chains_affected"`
}

// DeclaredModels 返回 providers.yaml 文本里声明的模型 id 集合。链别名与
// 运行时动态模型不在其中——它们不是可删除的 YAML 条目。
func DeclaredModels(config string) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(config, "\n") {
		if m := modelItem.FindStringSubmatch(line); m != nil {
			out[m[2]] = true
		}
	}
	return out
}

// Candidates 从模型目录筛出清理候选：最近一次探活为确定性拒绝且当前不可用。
func Candidates(catalog []provider.CatalogModel) []Candidate {
	var out []Candidate
	for _, entry := range catalog {
		h := entry.Health
		if !h.Available && h.Status == "unavailable" && h.ProbeStatus == provider.ProbeOffline {
			out = append(out, Candidate{
				Model:    entry.Info.ID,
				Provider: entry.Info.Provider,
				Reason:   h.Reason,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

var (
	modelItem   = regexp.MustCompile(`^(\s*)- id:\s*(\S+)\s*$`)
	providerRow = regexp.MustCompile(`^(\s*)- name:\s*(\S+)\s*$`)
	listItem    = regexp.MustCompile(`^\s*-\s*(\S+)\s*$`)
)

func blank(line string) bool   { return strings.TrimSpace(line) == "" }
func comment(line string) bool { return strings.HasPrefix(strings.TrimLeft(line, " "), "#") }
func indentOf(line string) int { return len(line) - len(strings.TrimLeft(line, " ")) }

// RemoveModels 从 providers.yaml 文本移除指定模型，返回清理后的文本与摘要。
// 不在 YAML 里声明的模型（运行时动态目录）记入 Skipped，不产生任何改动。
func RemoveModels(config string, models []string) (string, Result, error) {
	result := Result{}
	targets := make(map[string]bool, len(models))
	for _, m := range models {
		targets[m] = true
	}
	lines := strings.Split(config, "\n")
	drop := make([]bool, len(lines))

	// 模型块定位：条目行向上吞并紧邻的同缩进注释，向下到下一个同级条目
	// 或第一条缩进回落的非空行。
	modelIndent := -1
	found := make(map[string]bool, len(models))
	for i, line := range lines {
		m := modelItem.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		id, cur := m[2], indentOf(m[1])
		if modelIndent < 0 {
			modelIndent = cur
		}
		if cur != modelIndent || !targets[id] {
			continue
		}
		if found[id] {
			return "", result, fmt.Errorf("model %q appears more than once in providers.yaml", id)
		}
		found[id] = true
		start := i
		for start > 0 && comment(lines[start-1]) && indentOf(lines[start-1]) >= cur {
			start--
		}
		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			if blank(lines[j]) {
				continue
			}
			if modelItem.MatchString(lines[j]) || indentOf(lines[j]) < cur {
				end = j
				break
			}
		}
		for j := start; j < end; j++ {
			drop[j] = true
		}
	}

	for _, m := range models {
		if !found[m] {
			result.Skipped = append(result.Skipped, Skipped{Model: m, Reason: "not declared in providers.yaml (dynamic or runtime model)"})
		}
	}

	// provider 块级联：块内模型条目全部被删（或本来就没有）且本次删除触及
	// 该块时整块移除，避免留下零模型的空壳 provider。
	i := 0
	for i < len(lines) {
		pm := providerRow.FindStringSubmatch(lines[i])
		if pm == nil {
			i++
			continue
		}
		name, cur := pm[2], indentOf(pm[1])
		start := i
		for start > 0 && comment(lines[start-1]) && indentOf(lines[start-1]) >= cur {
			start--
		}
		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			next := lines[j]
			if blank(next) {
				continue
			}
			if nm := providerRow.FindStringSubmatch(next); nm != nil && indentOf(nm[1]) == cur {
				end = j
				break
			}
			if next[0] != ' ' && !comment(next) { // 顶层键（如 chains:）
				end = j
				break
			}
		}
		hasModel, touched := false, false
		for j := start; j < end; j++ {
			if modelItem.MatchString(lines[j]) && !drop[j] {
				hasModel = true
			}
			if drop[j] {
				touched = true
			}
		}
		if touched && !hasModel {
			for j := start; j < end; j++ {
				drop[j] = true
			}
			result.ProvidersRemoved = append(result.ProvidersRemoved, name)
		}
		i = end
	}

	var kept []string
	for j, line := range lines {
		if !drop[j] {
			kept = append(kept, line)
		}
	}
	next := strings.Join(kept, "\n")
	for strings.Contains(next, "\n\n\n") {
		next = strings.ReplaceAll(next, "\n\n\n", "\n\n")
	}

	removedModels := make([]string, 0, len(found))
	for m := range found {
		removedModels = append(removedModels, m)
	}
	sort.Strings(removedModels)

	next = removeChainReferences(next, removedModels, &result)

	for _, id := range removedModels {
		result.Removed = append(result.Removed, Candidate{Model: id})
	}
	return next, result, nil
}

// removeChainReferences 在顶层 chains: 区间内移除指向已删模型的引用行，
// 并记录每条受影响链的剩余成员。单遍扫描，同时维护删除标记与统计。
func removeChainReferences(config string, removed []string, result *Result) string {
	gone := make(map[string]bool, len(removed))
	for _, m := range removed {
		gone[m] = true
	}
	lines := strings.Split(config, "\n")
	drop := make([]bool, len(lines))
	inChains, chainName := false, ""
	remaining := make(map[string][]string)
	affected := make(map[string]bool)
	for i, line := range lines {
		if !strings.HasPrefix(line, " ") && !blank(line) {
			inChains = strings.HasPrefix(line, "chains:")
			chainName = ""
			continue
		}
		if !inChains || blank(line) || comment(line) {
			continue
		}
		if strings.HasSuffix(strings.TrimRight(line, " "), ":") && indentOf(line) == 2 {
			chainName = strings.TrimSpace(strings.TrimSuffix(line, ":"))
			continue
		}
		item := listItem.FindStringSubmatch(line)
		if item == nil {
			continue
		}
		if gone[item[1]] {
			drop[i] = true
			if chainName != "" {
				affected[chainName] = true
			}
		} else if chainName != "" {
			remaining[chainName] = append(remaining[chainName], item[1])
		}
	}
	// 成员清空的链：键行（及其上方紧邻的注释）一并移除。留着空键既通不过
	// 配置校验（chain must not be empty），也没有任何路由意义。
	dropChainKey := make(map[int]bool)
	for chain := range affected {
		if len(remaining[chain]) != 0 {
			continue
		}
		for i, line := range lines {
			if inChainsAt(lines, i) == chain && strings.HasSuffix(strings.TrimRight(line, " "), ":") && indentOf(line) == 2 {
				dropChainKey[i] = true
				for j := i - 1; j >= 0 && comment(lines[j]) && indentOf(lines[j]) >= 2; j-- {
					dropChainKey[j] = true
				}
			}
		}
	}

	var kept []string
	for i, line := range lines {
		if !drop[i] && !dropChainKey[i] {
			kept = append(kept, line)
		}
	}
	for chain := range affected {
		result.ChainsAffected = append(result.ChainsAffected, ChainImpact{Chain: chain, Remaining: remaining[chain]})
	}
	sort.Slice(result.ChainsAffected, func(i, j int) bool { return result.ChainsAffected[i].Chain < result.ChainsAffected[j].Chain })
	sort.Strings(result.ProvidersRemoved)
	next := strings.Join(kept, "\n")
	for strings.Contains(next, "\n\n\n") {
		next = strings.ReplaceAll(next, "\n\n\n", "\n\n")
	}
	return next
}

// inChainsAt 返回第 i 行所属的链名（不在 chains 区间内返回空串）。
func inChainsAt(lines []string, at int) string {
	chain, inChains := "", false
	for i := 0; i <= at; i++ {
		line := lines[i]
		if !strings.HasPrefix(line, " ") && !blank(line) {
			inChains = strings.HasPrefix(line, "chains:")
			chain = ""
			continue
		}
		if inChains && strings.HasSuffix(strings.TrimRight(line, " "), ":") && indentOf(line) == 2 {
			chain = strings.TrimSpace(strings.TrimSuffix(line, ":"))
		}
	}
	return chain
}
