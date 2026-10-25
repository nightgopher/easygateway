package handlers

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/skills"
)

func TestProjectSkillsIsolationAndPreservation(t *testing.T) {
	repo := newSkillsRepo(t)
	router := skillsTestRouter(t, repo)
	global := filepath.Join(t.TempDir(), "global")
	if err := skills.SaveManifest(repo, skills.Manifest{Version: 1, Targets: []string{global}, Enabled: []string{"beta"}}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(skills.ManifestPath(repo))
	project := t.TempDir()
	project, _ = filepath.EvalSymlinks(project)
	query := "?project=" + url.QueryEscape(project)
	own := filepath.Join(project, ".agents", "skills", "alpha")
	if err := os.MkdirAll(own, 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(own, "SKILL.md"), []byte("project authored"), 0644)
	other := t.TempDir()
	w := doJSON(t, router, http.MethodPut, "/admin/skills/config"+query, gin.H{"targets": []string{other}, "enabled": []string{"alpha", "beta"}})
	if w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	w = doJSON(t, router, http.MethodPost, "/admin/skills/sync"+query, nil)
	if w.Code != 200 {
		t.Fatalf("sync: %d %s", w.Code, w.Body.String())
	}
	var st struct {
		Targets []string            `json:"targets"`
		Local   []skills.LocalSkill `json:"local_skills"`
		Sync    skills.Report       `json:"sync"`
		InSync  bool                `json:"in_sync"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Targets) != 1 || st.Targets[0] != filepath.Join(project, ".agents", "skills") {
		t.Fatalf("target escape: %+v", st.Targets)
	}
	if len(st.Local) != 1 || st.Local[0].ID != "alpha" || st.InSync {
		t.Fatalf("conflict not preserved: %+v", st)
	}
	if len(st.Sync.Targets) != 1 || len(st.Sync.Targets[0].Skipped) != 1 {
		t.Fatalf("missing conflict: %+v", st.Sync)
	}
	if strings.Contains(w.Body.String(), `"errors":null`) || strings.Contains(w.Body.String(), `"removed":null`) {
		t.Fatal("null sync arrays")
	}
	content, _ := os.ReadFile(filepath.Join(own, "SKILL.md"))
	if string(content) != "project authored" {
		t.Fatal("custom skill overwritten")
	}
	if info, err := os.Lstat(filepath.Join(project, ".agents", "skills", "beta")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("project skill not linked", err)
	}
	after, _ := os.ReadFile(skills.ManifestPath(repo))
	if string(before) != string(after) {
		t.Fatal("global manifest modified")
	}
	entries, _ := os.ReadDir(other)
	if len(entries) != 0 {
		t.Fatal("caller target escaped project")
	}
	// Disabling removes only catalog-owned links, never the project-authored skill.
	doJSON(t, router, http.MethodPut, "/admin/skills/config"+query, gin.H{"targets": []string{other}, "enabled": []string{}})
	w = doJSON(t, router, http.MethodPost, "/admin/skills/sync"+query, nil)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(own, "SKILL.md")); err != nil {
		t.Fatal("custom skill removed")
	}
	if _, err := os.Lstat(filepath.Join(project, ".agents", "skills", "beta")); !os.IsNotExist(err) {
		t.Fatal("managed link not removed")
	}
}

func TestProjectSkillsRejectRedirectedWrites(t *testing.T) {
	repo := newSkillsRepo(t)
	for _, path := range []string{".agents", ".agents/skills", ".agents/skills.enabled.json"} {
		t.Run(path, func(t *testing.T) {
			project := t.TempDir()
			os.MkdirAll(filepath.Dir(filepath.Join(project, path)), 0755)
			os.Symlink(t.TempDir(), filepath.Join(project, path))
			w := doJSON(t, skillsTestRouter(t, repo), http.MethodPost, "/admin/skills/sync?project="+url.QueryEscape(project), nil)
			if w.Code != 400 {
				t.Fatalf("symlink accepted: %d", w.Code)
			}
		})
	}
	for _, project := range []string{"relative/path", "/", "/missing/project"} {
		if _, err := skills.ResolveScope(repo, project); err == nil {
			t.Fatalf("invalid project accepted: %q", project)
		}
	}
}
