package handlers

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/skills"
)

func skillsTestRouter(t *testing.T, repo string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	h := NewSkillsHandler(repo, log.New(io.Discard, "", 0))
	engine.GET("/admin/skills", h.HandleStatus)
	engine.GET("/admin/skills/:id", h.HandleDetail)
	engine.PUT("/admin/skills/config", h.HandleUpdateConfig)
	engine.POST("/admin/skills/sync", h.HandleSync)
	return engine
}

// newSkillsRepo 造一个含两个技能的最小仓库（与 internal/skills 的测试仓库同构）。
func newSkillsRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for _, id := range []string{"alpha", "beta"} {
		if err := os.MkdirAll(filepath.Join(repo, "skills", id), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, "skills", id, "SKILL.md"), []byte("body"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	registry := []skills.Skill{
		{ID: "alpha", DisplayName: "Alpha", SourcePath: "skills/alpha"},
		{ID: "beta", DisplayName: "Beta", SourcePath: "skills/beta"},
	}
	raw, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "registry"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skills.RegistryPath(repo), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestSkillsUnconfiguredReturnsHint(t *testing.T) {
	engine := skillsTestRouter(t, "")
	w := doJSON(t, engine, http.MethodGet, "/admin/skills", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("未配置应仍返回 200: %d", w.Code)
	}
	var body struct {
		Configured bool   `json:"configured"`
		Hint       string `json:"hint"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Configured || body.Hint == "" {
		t.Fatalf("未配置应带提示: %+v", body)
	}
}

func TestSkillsConfigSyncAndStatus(t *testing.T) {
	repo := newSkillsRepo(t)
	engine := skillsTestRouter(t, repo)
	target := filepath.Join(t.TempDir(), "skills")

	// 保存清单：启用 alpha。
	w := doJSON(t, engine, http.MethodPut, "/admin/skills/config", gin.H{
		"targets": []string{target},
		"enabled": []string{"alpha"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("保存清单失败: %d %s", w.Code, w.Body.String())
	}

	// 同步前 in_sync=false。
	w = doJSON(t, engine, http.MethodGet, "/admin/skills", nil)
	var status struct {
		InSync  bool     `json:"in_sync"`
		Enabled []string `json:"enabled"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.InSync || len(status.Enabled) != 1 || status.Enabled[0] != "alpha" {
		t.Fatalf("同步前状态不正确: %+v", status)
	}

	// 同步 → alpha 链接落盘。
	w = doJSON(t, engine, http.MethodPost, "/admin/skills/sync", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("同步失败: %d %s", w.Code, w.Body.String())
	}
	info, err := os.Lstat(filepath.Join(target, "alpha"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("alpha 应以符号链接落盘: %v", err)
	}

	// 同步后 in_sync=true，且清单文件已写进仓库。
	w = doJSON(t, engine, http.MethodGet, "/admin/skills", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if !status.InSync {
		t.Fatalf("同步后 in_sync 应为 true: %s", w.Body.String())
	}
	if _, _, err := skills.LoadManifest(repo); err != nil {
		t.Fatalf("清单应已写入仓库: %v", err)
	}
}

func TestSkillsDetailRejectsTraversal(t *testing.T) {
	repo := newSkillsRepo(t)
	engine := skillsTestRouter(t, repo)
	w := doJSON(t, engine, http.MethodGet, "/admin/skills/..%2F..%2Fetc", nil)
	if w.Code == http.StatusOK {
		t.Fatalf("路径穿越应被拒绝: %d", w.Code)
	}
	w = doJSON(t, engine, http.MethodGet, "/admin/skills/alpha", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("详情应可用: %d %s", w.Code, w.Body.String())
	}
}

func TestSkillsUpdateConfigRejectsUnknownSkill(t *testing.T) {
	repo := newSkillsRepo(t)
	engine := skillsTestRouter(t, repo)
	w := doJSON(t, engine, http.MethodPut, "/admin/skills/config", gin.H{
		"targets": []string{"~/x"},
		"enabled": []string{"ghost"},
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("未知技能应 400: %d", w.Code)
	}
}

func TestSkillsConfigReturnsNormalizedState(t *testing.T) {
	repo := newSkillsRepo(t)
	engine := skillsTestRouter(t, repo)
	target := t.TempDir()
	w := doJSON(t, engine, http.MethodPut, "/admin/skills/config", gin.H{
		"targets": []string{" " + target + " ", target}, "enabled": []string{" alpha ", "alpha"},
	})
	var status skills.Status
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || len(status.Targets) != 1 || len(status.Enabled) != 1 || !status.Skills[0].Enabled {
		t.Fatalf("response must match saved normalized config: %s", w.Body.String())
	}
	for _, body := range []gin.H{{}, {"targets": []string{target}}, {"enabled": []string{}}} {
		w = doJSON(t, engine, http.MethodPut, "/admin/skills/config", body)
		if w.Code != 400 {
			t.Fatalf("incomplete config should be rejected: %s", w.Body.String())
		}
	}
	saved, _, err := skills.LoadManifest(repo)
	if err != nil || len(saved.Enabled) != 1 {
		t.Fatal("incomplete request cleared config")
	}
}

func TestSkillsUnconfiguredDetailReturnsHint(t *testing.T) {
	w := doJSON(t, skillsTestRouter(t, ""), http.MethodGet, "/admin/skills/alpha", nil)
	if w.Code != 200 {
		t.Fatalf("unconfigured detail should return hint: %s", w.Body.String())
	}
}
