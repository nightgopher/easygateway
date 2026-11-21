// Package skills 管理技能仓库（custom-skills）与本地技能目录之间的安装关系。
//
// 事实源全部在仓库里：registry/skills.json 是技能目录（generate:registry 生成），
// skills.enabled.json 是启用清单（哪些技能装到哪些目录）。Apply 用符号链接把
// 启用的技能目录铺进目标目录：链接指向仓库内的 skills/<id>，仓库更新即时生效；
// 清理时只移除指向本仓库的链接，绝不碰用户自己的文件。
package skills

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Manifest 是 skills.enabled.json 的结构：targets 是安装目标目录（支持 ~ 前缀），
// enabled 是启用的技能 ID（须存在于 registry）。
type Manifest struct {
	Version int      `json:"version"`
	Targets []string `json:"targets"`
	Enabled []string `json:"enabled"`
}

const manifestName = "skills.enabled.json"

// DefaultManifest 返回清单缺失时的默认值：只认标准的 ~/.agents/skills 目录。
func DefaultManifest() Manifest {
	return Manifest{Version: 1, Targets: []string{"~/.agents/skills"}, Enabled: nil}
}

func RegistryPath(repo string) string { return filepath.Join(repo, "registry", "skills.json") }
func ManifestPath(repo string) string { return filepath.Join(repo, manifestName) }
func SkillsDir(repo string) string    { return filepath.Join(repo, "skills") }

// Skill 是 registry/skills.json 条目中面板需要的字段子集，
// 其余字段由 encoding/json 自动忽略。
type Skill struct {
	ID          string   `json:"id"`
	DisplayName string   `json:"displayName"`
	Description string   `json:"description"`
	Emoji       string   `json:"emoji"`
	Tags        []string `json:"tags"`
	Author      string   `json:"author"`
	SourcePath  string   `json:"sourcePath"`
	GithubURL   string   `json:"githubUrl"`
	LastUpdated string   `json:"lastUpdated"`
	Upstream    string   `json:"upstream"`
}

// LoadRegistry 读取技能目录清单；文件缺失或非法 JSON 返回错误。
func LoadRegistry(repo string) ([]Skill, error) {
	raw, err := os.ReadFile(RegistryPath(repo))
	if err != nil {
		return nil, fmt.Errorf("读取技能目录失败: %w", err)
	}
	var registry []Skill
	if err := json.Unmarshal(raw, &registry); err != nil {
		return nil, fmt.Errorf("解析 registry/skills.json 失败: %w", err)
	}
	seen := make(map[string]bool, len(registry))
	for _, skill := range registry {
		if !validID(skill.ID) || seen[skill.ID] {
			return nil, fmt.Errorf("技能目录包含非法或重复 ID: %q", skill.ID)
		}
		seen[skill.ID] = true
	}
	return registry, nil
}

// LoadManifest 读取启用清单；文件缺失时返回默认清单（exists=false）。
func LoadManifest(repo string) (Manifest, bool, error) {
	raw, err := os.ReadFile(ManifestPath(repo))
	if os.IsNotExist(err) {
		return DefaultManifest(), false, nil
	}
	if err != nil {
		return DefaultManifest(), false, fmt.Errorf("读取启用清单失败: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return DefaultManifest(), true, fmt.Errorf("解析 %s 失败: %w", manifestName, err)
	}
	if m.Version == 0 {
		m.Version = 1
	}
	return normalize(m), true, nil
}

// SaveManifest 校验并写入启用清单：enabled 里的 ID 必须都在 registry 中。
func SaveManifest(repo string, m Manifest) error {
	registry, err := LoadRegistry(repo)
	if err != nil {
		return err
	}
	m = normalize(m)
	if err := validateManifest(m, registry); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	// Publish a complete file so readers and interrupted writes cannot observe
	// a truncated manifest. Rename also avoids following an existing symlink.
	file, err := os.CreateTemp(repo, ".skills-enabled-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(append(data, '\n')); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), ManifestPath(repo))
}

func validID(id string) bool {
	return id != "" && strings.TrimSpace(id) == id && id != "." && id != ".." &&
		!strings.ContainsAny(id, "/\\:\x00") && filepath.Base(id) == id
}

func validateManifest(m Manifest, registry []Skill) error {
	if m.Version != 1 {
		return fmt.Errorf("不支持的清单版本: %d", m.Version)
	}
	known := make(map[string]bool, len(registry))
	for _, skill := range registry {
		known[skill.ID] = true
	}
	for _, id := range m.Enabled {
		if !validID(id) || !known[id] {
			return fmt.Errorf("启用的技能不在目录中或 ID 非法: %q", id)
		}
	}
	if len(m.Enabled) > 0 && len(m.Targets) == 0 {
		return fmt.Errorf("启用技能时至少需要一个目标目录")
	}
	for _, target := range m.Targets {
		if !filepath.IsAbs(expandTilde(target)) {
			return fmt.Errorf("目标目录须为绝对路径或 ~/ 路径: %q", target)
		}
	}
	return nil
}

// normalize 去空格、丢空项、按原顺序去重。
func normalize(m Manifest) Manifest {
	m.Targets = dedupe(m.Targets)
	m.Enabled = dedupe(m.Enabled)
	return m
}

func dedupe(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// ─── 状态视图 ────────────────────────────────────────────────────────────────

// SkillInstall 是单个技能在面板上的展示行；Installed 以展开后的目标目录为键。
type SkillInstall struct {
	Skill
	Enabled   bool            `json:"enabled"`
	Installed map[string]bool `json:"installed"`
}

// Status 是技能面板的完整状态。
type Status struct {
	Repo           string         `json:"repo"`
	RegistryPath   string         `json:"registry_path"`
	ManifestPath   string         `json:"manifest_path"`
	ManifestExists bool           `json:"manifest_exists"`
	Targets        []string       `json:"targets"`
	Enabled        []string       `json:"enabled"`
	InSync         bool           `json:"in_sync"`
	StaleLinks     []string       `json:"stale_links"`
	Skills         []SkillInstall `json:"skills"`
}

// BuildStatus 汇总目录清单、启用清单和各目标目录的实际链接状态。
func BuildStatus(repo string, m Manifest) (Status, error) {
	var err error
	repo, err = filepath.Abs(expandTilde(repo))
	if err != nil {
		return Status{}, err
	}
	m = normalize(m)
	registry, err := LoadRegistry(repo)
	if err != nil {
		return Status{}, err
	}
	// 状态读取对过期启用项降级：registry 中已不存在的 ID 视为未启用，
	// 不让一条历史遗留数据把整个技能面板打成 500。写入路径（SaveManifest
	// 与 Apply）仍严格校验，下次保存会自然清掉这些条目。
	known := make(map[string]bool, len(registry))
	for _, skill := range registry {
		known[skill.ID] = true
	}
	enabledIDs := make([]string, 0, len(m.Enabled))
	for _, id := range m.Enabled {
		if known[id] {
			enabledIDs = append(enabledIDs, id)
		}
	}
	m.Enabled = enabledIDs
	if err := validateManifest(m, registry); err != nil {
		return Status{}, err
	}
	enabled := make(map[string]bool, len(m.Enabled))
	for _, id := range m.Enabled {
		enabled[id] = true
	}
	targets := expandAll(m.Targets)

	st := Status{
		Repo:           repo,
		RegistryPath:   RegistryPath(repo),
		ManifestPath:   ManifestPath(repo),
		ManifestExists: true,
		Targets:        targets,
		Enabled:        m.Enabled,
		StaleLinks:     []string{},
		Skills:         make([]SkillInstall, 0, len(registry)),
	}
	if _, err := os.Stat(ManifestPath(repo)); os.IsNotExist(err) {
		st.ManifestExists = false
	}

	inSync := true
	for _, s := range registry {
		row := SkillInstall{Skill: s, Enabled: enabled[s.ID], Installed: map[string]bool{}}
		for _, t := range targets {
			ok := linkPointsTo(filepath.Join(t, s.ID), filepath.Join(SkillsDir(repo), s.ID))
			row.Installed[t] = ok
			if enabled[s.ID] && !ok {
				inSync = false
			}
		}
		st.Skills = append(st.Skills, row)
	}

	for _, t := range targets {
		entries, err := os.ReadDir(t)
		if err != nil {
			if !os.IsNotExist(err) {
				inSync = false
			}
			continue
		}
		for _, e := range entries {
			if enabled[e.Name()] {
				continue
			}
			link := filepath.Join(t, e.Name())
			if isSymlink(link) && linkOwnedByRepo(link, SkillsDir(repo)) {
				st.StaleLinks = append(st.StaleLinks, filepath.Join(t, e.Name()))
				inSync = false
			}
		}
	}
	st.InSync = inSync
	return st, nil
}

// ─── 同步落盘 ────────────────────────────────────────────────────────────────

// TargetReport 是单个目标目录的同步结果；Skipped 是与本仓库冲突的同名真实
// 条目或指向别处的符号链接，Apply 永远不覆盖它们。
type TargetReport struct {
	Target  string   `json:"target"`
	Linked  []string `json:"linked"`
	Current []string `json:"current"`
	Removed []string `json:"removed"`
	Skipped []string `json:"skipped"`
	Errors  []string `json:"errors"`
}

// Report 是一轮 Apply 的完整结果。
type Report struct {
	Targets []TargetReport `json:"targets"`
}

// Apply 把启用清单落到各目标目录：为启用的技能建链接、清理指向本仓库但已
// 停用（或已从仓库删除）的旧链接。非本仓库的条目一律不动。
func Apply(repo string, m Manifest) (Report, error) {
	var err error
	repo, err = filepath.Abs(expandTilde(repo))
	if err != nil {
		return Report{}, err
	}
	registry, err := LoadRegistry(repo)
	if err != nil {
		return Report{}, err
	}
	m = normalize(m)
	if err := validateManifest(m, registry); err != nil {
		return Report{}, err
	}
	repoSkills := SkillsDir(repo)

	report := Report{Targets: make([]TargetReport, 0, len(m.Targets))}
	for _, raw := range m.Targets {
		target := expandTilde(raw)
		rep := TargetReport{Target: target, Linked: []string{}, Current: []string{}, Removed: []string{}, Skipped: []string{}, Errors: []string{}}

		if err := os.MkdirAll(target, 0o755); err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("创建目录失败: %v", err))
			report.Targets = append(report.Targets, rep)
			continue
		}

		enabled := make(map[string]bool, len(m.Enabled))
		for _, id := range m.Enabled {
			enabled[id] = true
			src := filepath.Join(repoSkills, id)
			if info, err := os.Stat(src); err != nil || !info.IsDir() {
				rep.Errors = append(rep.Errors, fmt.Sprintf("%s: 仓库中不存在该技能目录", id))
				continue
			}
			link := filepath.Join(target, id)
			existing, err := os.Lstat(link)
			switch {
			case os.IsNotExist(err):
				if err := os.Symlink(src, link); err != nil {
					rep.Errors = append(rep.Errors, fmt.Sprintf("%s: 建立链接失败: %v", id, err))
					continue
				}
				rep.Linked = append(rep.Linked, id)
			case err != nil:
				rep.Errors = append(rep.Errors, fmt.Sprintf("%s: %v", id, err))
			case existing.Mode()&os.ModeSymlink == 0:
				rep.Skipped = append(rep.Skipped, id)
			case linkPointsTo(link, src):
				rep.Current = append(rep.Current, id)
			default:
				rep.Skipped = append(rep.Skipped, id)
			}
		}

		entries, err := os.ReadDir(target)
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("读取目录失败: %v", err))
		}
		if err == nil {
			for _, e := range entries {
				if enabled[e.Name()] {
					continue
				}
				link := filepath.Join(target, e.Name())
				if !isSymlink(link) || !linkOwnedByRepo(link, repoSkills) {
					continue
				}
				if err := os.Remove(link); err != nil {
					rep.Errors = append(rep.Errors, fmt.Sprintf("清理 %s 失败: %v", e.Name(), err))
					continue
				}
				rep.Removed = append(rep.Removed, e.Name())
			}
		}
		report.Targets = append(report.Targets, rep)
	}
	return report, nil
}

// ─── 链接判定 ────────────────────────────────────────────────────────────────

func isSymlink(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSymlink != 0
}

// linkPointsTo 报告 path 是否为指向 want 的链接；两端都做 EvalSymlinks，
// 兼容相对链接与 dangling 之外的常见形态。
func linkPointsTo(path, want string) bool {
	if !isSymlink(path) {
		return false
	}
	resolvedWant, err := filepath.EvalSymlinks(want)
	if err != nil {
		return false
	}
	resolvedLink, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	return filepath.Clean(resolvedLink) == filepath.Clean(resolvedWant)
}

// linkOwnedByRepo 报告链接是否指向仓库的 skills 目录（含 dangling 的情况）。
func linkOwnedByRepo(path, repoSkills string) bool {
	raw, err := os.Readlink(path)
	if err != nil {
		return false
	}
	target := raw
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(path), raw)
	}
	target = filepath.Clean(target)
	repoSkills = filepath.Clean(repoSkills)
	if !strings.HasSuffix(repoSkills, string(filepath.Separator)) {
		repoSkills += string(filepath.Separator)
	}
	return strings.HasPrefix(target+string(filepath.Separator), repoSkills)
}

func expandAll(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, expandTilde(p))
	}
	return out
}

func expandTilde(p string) string {
	if p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return p
	}
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}
