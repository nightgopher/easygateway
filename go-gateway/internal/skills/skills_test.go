package skills

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// newTestRepo 造一个最小技能仓库：两个技能目录 + 生成的 registry。
func newTestRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "skills", "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "skills", "beta"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"alpha", "beta"} {
		if err := os.WriteFile(filepath.Join(repo, "skills", id, "SKILL.md"), []byte("---\nname: "+id+"\n---\nbody"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	registry := []Skill{
		{ID: "alpha", DisplayName: "Alpha", Tags: []string{"Utility"}, SourcePath: "skills/alpha"},
		{ID: "beta", DisplayName: "Beta", Tags: []string{"Utility"}, SourcePath: "skills/beta"},
	}
	raw, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "registry"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(RegistryPath(repo), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestLoadManifestMissingReturnsDefault(t *testing.T) {
	repo := newTestRepo(t)
	m, exists, err := LoadManifest(repo)
	if err != nil || exists {
		t.Fatalf("缺失清单应返回默认且 exists=false: %v %v", err, exists)
	}
	if len(m.Targets) != 1 || m.Targets[0] != "~/.agents/skills" {
		t.Fatalf("默认目标应为 ~/.agents/skills: %+v", m)
	}
}

func TestManifestRoundtrip(t *testing.T) {
	repo := newTestRepo(t)
	in := Manifest{Version: 1, Targets: []string{" ~/skills ", "", "~/skills"}, Enabled: []string{"alpha", "", "alpha"}}
	if err := SaveManifest(repo, in); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	m, exists, err := LoadManifest(repo)
	if err != nil || !exists {
		t.Fatalf("回读失败: %v %v", err, exists)
	}
	if len(m.Targets) != 1 || m.Targets[0] != "~/skills" {
		t.Fatalf("targets 应去重去空: %+v", m.Targets)
	}
	if len(m.Enabled) != 1 || m.Enabled[0] != "alpha" {
		t.Fatalf("enabled 应去重去空: %+v", m.Enabled)
	}
}

func TestSaveManifestRejectsUnknownSkill(t *testing.T) {
	repo := newTestRepo(t)
	err := SaveManifest(repo, Manifest{Version: 1, Targets: []string{"~/x"}, Enabled: []string{"ghost"}})
	if err == nil {
		t.Fatal("未知技能应被拒绝")
	}
}

func TestApplyLinksEnabledAndRemovesStale(t *testing.T) {
	repo := newTestRepo(t)
	target := filepath.Join(t.TempDir(), "skills")
	// 预置：一个指向 beta 的旧链接（将变为停用 → 应清理）和一个外部链接（不动）。
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(SkillsDir(repo), "beta"), filepath.Join(target, "beta")); err != nil {
		t.Fatal(err)
	}
	foreignDir := t.TempDir()
	if err := os.Symlink(foreignDir, filepath.Join(target, "foreign")); err != nil {
		t.Fatal(err)
	}

	report, err := Apply(repo, Manifest{Version: 1, Targets: []string{target}, Enabled: []string{"alpha"}})
	if err != nil {
		t.Fatalf("Apply 失败: %v", err)
	}
	rep := report.Targets[0]
	if len(rep.Linked) != 1 || rep.Linked[0] != "alpha" {
		t.Fatalf("alpha 应被链接: %+v", rep)
	}
	if !linkPointsTo(filepath.Join(target, "alpha"), filepath.Join(SkillsDir(repo), "alpha")) {
		t.Fatal("alpha 链接应指向仓库目录")
	}
	if len(rep.Removed) != 1 || rep.Removed[0] != "beta" {
		t.Fatalf("停用的 beta 链接应被清理: %+v", rep.Removed)
	}
	if _, err := os.Lstat(filepath.Join(target, "foreign")); err != nil {
		t.Fatal("外部链接不应被触碰")
	}

	// 幂等：重复 Apply 只报 Current。
	report, err = Apply(repo, Manifest{Version: 1, Targets: []string{target}, Enabled: []string{"alpha"}})
	if err != nil {
		t.Fatalf("重复 Apply 失败: %v", err)
	}
	rep = report.Targets[0]
	if len(rep.Linked) != 0 || len(rep.Current) != 1 || len(rep.Removed) != 0 {
		t.Fatalf("重复 Apply 应幂等: %+v", rep)
	}
}

func TestApplySkipsRealDirAndReportsMissingSkill(t *testing.T) {
	repo := newTestRepo(t)
	target := t.TempDir()
	// alpha 名字被真实目录占用 → 跳过；beta 的源目录丢失 → 报错。
	if err := os.MkdirAll(filepath.Join(target, "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(SkillsDir(repo), "beta")); err != nil {
		t.Fatal(err)
	}
	report, err := Apply(repo, Manifest{Version: 1, Targets: []string{target}, Enabled: []string{"alpha", "beta"}})
	if err != nil {
		t.Fatalf("Apply 失败: %v", err)
	}
	rep := report.Targets[0]
	if len(rep.Skipped) != 1 || rep.Skipped[0] != "alpha" {
		t.Fatalf("真实目录应被跳过: %+v", rep.Skipped)
	}
	if len(rep.Errors) != 1 {
		t.Fatalf("不存在的技能应报错: %+v", rep.Errors)
	}
}

func TestBuildStatusReflectsInstallState(t *testing.T) {
	repo := newTestRepo(t)
	target := filepath.Join(t.TempDir(), "skills")
	manifest := Manifest{Version: 1, Targets: []string{target}, Enabled: []string{"alpha"}}

	st, err := BuildStatus(repo, manifest)
	if err != nil {
		t.Fatalf("BuildStatus 失败: %v", err)
	}
	if st.InSync {
		t.Fatal("未同步前 in_sync 应为 false")
	}
	if _, exists := st.Skills[0].Installed[target]; !exists {
		t.Fatalf("installed 应包含目标目录: %+v", st.Skills[0].Installed)
	}

	if _, err := Apply(repo, manifest); err != nil {
		t.Fatalf("Apply 失败: %v", err)
	}
	st, err = BuildStatus(repo, manifest)
	if err != nil {
		t.Fatalf("BuildStatus 失败: %v", err)
	}
	if !st.InSync {
		t.Fatal("同步后 in_sync 应为 true")
	}

	// 手工造一个指向仓库的孤儿链接（技能已删除）→ stale。
	orphan := filepath.Join(repo, "skills", "orphan")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(orphan, filepath.Join(target, "orphan")); err != nil {
		t.Fatal(err)
	}
	st, err = BuildStatus(repo, manifest)
	if err != nil {
		t.Fatalf("BuildStatus 失败: %v", err)
	}
	if len(st.StaleLinks) != 1 || st.InSync {
		t.Fatalf("孤儿链接应被标记: %+v", st.StaleLinks)
	}
}

func TestLinkOwnedByRepoRejectsForeignPaths(t *testing.T) {
	repo := newTestRepo(t)
	tmp := t.TempDir()
	inside := filepath.Join(tmp, "inside")
	if err := os.Symlink(filepath.Join(SkillsDir(repo), "alpha"), inside); err != nil {
		t.Fatal(err)
	}
	if !linkOwnedByRepo(inside, SkillsDir(repo)) {
		t.Fatal("指向仓库的链接应被识别")
	}
	outside := filepath.Join(tmp, "outside")
	if err := os.Symlink(tmp, outside); err != nil {
		t.Fatal(err)
	}
	if linkOwnedByRepo(outside, SkillsDir(repo)) {
		t.Fatal("外部链接不应被识别为仓库所有")
	}
	// 前缀相近但不同的目录不算仓库内。
	if linkOwnedByRepo(inside, SkillsDir(repo)+"-other") {
		t.Fatal("前缀目录不应误判")
	}
}

// TestBuildStatusToleratesStaleEnabledIDs 验证启用清单引用 registry 已不存在的
// 技能时，状态读取降级为「未启用」而不是报错——技能面板不能被一条历史遗留
// 数据打成 500。
func TestBuildStatusToleratesStaleEnabledIDs(t *testing.T) {
	repo := newTestRepo(t)
	manifest := Manifest{Version: 1, Targets: []string{"~/x"}, Enabled: []string{"alpha", "ghost"}}
	st, err := BuildStatus(repo, manifest)
	if err != nil {
		t.Fatalf("过期启用项不应让 BuildStatus 失败: %v", err)
	}
	if len(st.Enabled) != 1 || st.Enabled[0] != "alpha" {
		t.Fatalf("过期启用项应被过滤，保留有效项: %+v", st.Enabled)
	}
}

// TestSaveManifestStillRejectsUnknownSkill 确认降级只发生在读路径，写入仍严格。
func TestSaveManifestStillRejectsUnknownSkill(t *testing.T) {
	repo := newTestRepo(t)
	err := SaveManifest(repo, Manifest{Version: 1, Targets: []string{"~/x"}, Enabled: []string{"ghost"}})
	if err == nil {
		t.Fatal("写入路径应继续拒绝目录外技能")
	}
}

// TestRegistryPathDiscovery 验证技能目录清单的两种仓库布局：根 registry 优先，
// 其次 easy-suite 式的 skills-data/registry，都不存在时回退传统路径。
func TestRegistryPathDiscovery(t *testing.T) {
	repo := t.TempDir()
	monorepo := filepath.Join(repo, "skills-data", "registry", "skills.json")
	if err := os.MkdirAll(filepath.Dir(monorepo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(monorepo, []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := RegistryPath(repo); got != monorepo {
		t.Fatalf("应发现 skills-data 布局: %s", got)
	}
	legacy := filepath.Join(repo, "registry", "skills.json")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := RegistryPath(repo); got != legacy {
		t.Fatalf("根 registry 存在时应优先: %s", got)
	}
}

// TestManifestOverride 验证启用清单可放到仓库之外：override 生效时不写仓库根，
// Load/Save 都走 override 路径；清空 override 后回到传统位置。
func TestManifestOverride(t *testing.T) {
	repo := newTestRepo(t)
	override := filepath.Join(t.TempDir(), "skills.enabled.json")
	SetManifestOverride(override)
	t.Cleanup(func() { SetManifestOverride("") })
	in := Manifest{Version: 1, Targets: []string{"~/x"}, Enabled: []string{"alpha"}}
	if err := SaveManifest(repo, in); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, manifestName)); !os.IsNotExist(err) {
		t.Fatalf("override 生效时不应写仓库内清单: %v", err)
	}
	m, exists, err := LoadManifest(repo)
	if err != nil || !exists || len(m.Enabled) != 1 || m.Enabled[0] != "alpha" {
		t.Fatalf("应从 override 读回清单: %v %v %v", m, exists, err)
	}
	if ManifestPath(repo) != override {
		t.Fatalf("ManifestPath 应返回 override: %s", ManifestPath(repo))
	}
	SetManifestOverride("")
	if ManifestPath(repo) != filepath.Join(repo, manifestName) {
		t.Fatalf("清空 override 后应回到仓库内路径: %s", ManifestPath(repo))
	}
}
