package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/selfupdate"
)

// fullManifest 构造带必需字段的合法清单（FetchManifest 校验 version/url/size/signature）。
func fullManifest(version, notes string) selfupdate.Manifest {
	return selfupdate.Manifest{Version: version, Notes: notes, URL: "https://example.invalid/a.tar.gz", Size: 10, Signature: "c2ln"}
}

func newUpdateTestServer(t *testing.T, manifest selfupdate.Manifest) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(manifest)
	}))
	t.Cleanup(server.Close)
	return server
}

func newUpdateTestHandler(t *testing.T, version string, manifest selfupdate.Manifest, tempDir string, spawn func(string, []string, string) (int, error)) (*UpdateAdminHandler, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := NewUpdateAdminHandler(version)
	h.feed = newUpdateTestServer(t, manifest).URL
	h.dataDir = func() string { return tempDir }
	h.spawn = spawn
	router := gin.New()
	router.GET("/admin/update/check", h.HandleUpdateCheck)
	router.POST("/admin/update/apply", h.HandleUpdateApply)
	router.GET("/admin/update/status", h.HandleUpdateStatus)
	return h, router
}

func doUpdateJSON(t *testing.T, router *gin.Engine, method, path string) (int, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body
}

func TestUpdateCheckReportsLatest(t *testing.T) {
	h, router := newUpdateTestHandler(t, "0.3.19", fullManifest("0.3.20", "发版说明"), t.TempDir(), nil)
	code, body := doUpdateJSON(t, router, http.MethodGet, "/admin/update/check")
	if code != http.StatusOK {
		t.Fatalf("check status = %d body %v", code, body)
	}
	if body["update_available"] != true {
		t.Fatalf("update_available = %v, want true", body["update_available"])
	}
	latest := body["latest"].(map[string]any)
	if latest["version"] != "0.3.20" || latest["notes"] != "发版说明" {
		t.Fatalf("latest payload = %v", latest)
	}
	if h.displayVersion() != "0.3.19" {
		t.Fatalf("displayVersion = %q", h.displayVersion())
	}
}

func TestUpdateCheckUpToDate(t *testing.T) {
	_, router := newUpdateTestHandler(t, "v0.3.20", fullManifest("0.3.20", ""), t.TempDir(), nil)
	code, body := doUpdateJSON(t, router, http.MethodGet, "/admin/update/check")
	if code != http.StatusOK || body["update_available"] != false {
		t.Fatalf("check = %d %v, want up-to-date", code, body)
	}
}

func TestUpdateApplySpawnsDetachedAndWritesPID(t *testing.T) {
	dir := t.TempDir()
	var spawnedExe string
	var spawnedArgs []string
	spawn := func(exe string, args []string, logPath string) (int, error) {
		spawnedExe, spawnedArgs = exe, args
		if err := os.WriteFile(logPath, []byte("更新完成\n"), 0600); err != nil {
			return 0, err
		}
		return 4321, nil
	}
	_, router := newUpdateTestHandler(t, "0.3.19", fullManifest("0.3.20", ""), dir, spawn)

	code, body := doUpdateJSON(t, router, http.MethodPost, "/admin/update/apply")
	if code != http.StatusAccepted || body["started"] != true {
		t.Fatalf("apply = %d %v", code, body)
	}
	if spawnedExe == "" || len(spawnedArgs) != 1 || spawnedArgs[0] != "update" {
		t.Fatalf("spawned exe=%q args=%v", spawnedExe, spawnedArgs)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "gateway-update.pid"))
	if err != nil || strings.TrimSpace(string(raw)) != "4321" {
		t.Fatalf("pidfile = %q err %v", raw, err)
	}

	// status 能读到版本与日志尾部
	code, body = doUpdateJSON(t, router, http.MethodGet, "/admin/update/status")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if body["version"] != "0.3.19" {
		t.Fatalf("status version = %v", body["version"])
	}
	log := body["apply_log"].(map[string]any)
	if log["tail"] != "更新完成" {
		t.Fatalf("status tail = %v", log["tail"])
	}
}

func TestUpdateApplyRefusesWhenUpToDate(t *testing.T) {
	_, router := newUpdateTestHandler(t, "0.3.20", fullManifest("0.3.20", ""), t.TempDir(), nil)
	code, body := doUpdateJSON(t, router, http.MethodPost, "/admin/update/apply")
	if code != http.StatusOK || body["started"] != false {
		t.Fatalf("apply = %d %v, want refuse", code, body)
	}
}

func TestUpdateApplyGuardsRunningUpdate(t *testing.T) {
	dir := t.TempDir()
	// 写入一个存活 pid（当前测试进程自己），应被 409 拦截
	if err := os.WriteFile(filepath.Join(dir, "gateway-update.pid"), []byte("1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	spawn := func(string, []string, string) (int, error) {
		t.Fatal("不应再次 spawn")
		return 0, nil
	}
	_, router := newUpdateTestHandler(t, "0.3.19", fullManifest("0.3.20", ""), dir, spawn)
	code, body := doUpdateJSON(t, router, http.MethodPost, "/admin/update/apply")
	if code != http.StatusConflict {
		t.Fatalf("apply = %d %v, want 409", code, body)
	}
}

func TestUpdateApplyClearsStalePIDFile(t *testing.T) {
	dir := t.TempDir()
	// 残留 pid 指向极不可能存在的进程，应清理后正常 spawn
	if err := os.WriteFile(filepath.Join(dir, "gateway-update.pid"), []byte("2147483000\n"), 0600); err != nil {
		t.Fatal(err)
	}
	spawned := false
	spawn := func(string, []string, string) (int, error) {
		spawned = true
		return 12345, nil
	}
	_, router := newUpdateTestHandler(t, "0.3.19", fullManifest("0.3.20", ""), dir, spawn)
	code, body := doUpdateJSON(t, router, http.MethodPost, "/admin/update/apply")
	if code != http.StatusAccepted || !spawned {
		t.Fatalf("apply = %d %v spawned=%v, want accepted", code, body, spawned)
	}
}

func TestTailFileLimitsLinesAndSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway-update.log")
	var b strings.Builder
	for i := 0; i < 25; i++ {
		b.WriteString("line\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0600); err != nil {
		t.Fatal(err)
	}
	tail := tailFile(path, 15)
	if got := strings.Count(tail, "\n") + 1; got != 15 {
		t.Fatalf("tail lines = %d, want 15", got)
	}

	var big strings.Builder
	for big.Len() < 4000 {
		big.WriteString("x")
	}
	if err := os.WriteFile(path, []byte(big.String()), 0600); err != nil {
		t.Fatal(err)
	}
	if tail := tailFile(path, 15); len(tail) > 2000 {
		t.Fatalf("tail size = %d, want <= 2000", len(tail))
	}
}

func TestDisplayVersionDevFallsBackToPlainWhenNoVCS(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// 测试二进制带 vcs 信息时返回 dev (short)；无 vcs 信息时回退原样。
	h := NewUpdateAdminHandler("dev")
	v := h.displayVersion()
	if v != "dev" && !strings.HasPrefix(v, "dev (") {
		t.Fatalf("displayVersion(dev) = %q", v)
	}
	h2 := NewUpdateAdminHandler("v0.3.20")
	if h2.displayVersion() != "v0.3.20" {
		t.Fatalf("displayVersion(release) = %q", h2.displayVersion())
	}
}
