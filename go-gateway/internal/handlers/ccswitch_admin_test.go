package handlers

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/nightgopher/easygateway/internal/provider"
)

const ccswitchFixtureConfig = `{
  "apps": {
    "claude": {
      "current": "p1",
      "providers": {
        "p1": {
          "id": "p1",
          "name": "MyGLM",
          "settingsConfig": {"env": {
            "ANTHROPIC_BASE_URL": "https://glmx.example.com",
            "ANTHROPIC_AUTH_TOKEN": "sk-handler-claude",
            "ANTHROPIC_DEFAULT_SONNET_MODEL": "glm-4.7"
          }},
          "category": "custom",
          "meta": {}
        }
      }
    }
  }
}`

// setupCCSwitchFixture 准备 cc-switch home 与网关运行目录（providers.yaml /
// .env），并让测试进程在该目录运行——handler 写的就是 cwd 下这两个文件。
func setupCCSwitchFixture(t *testing.T) string {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ccHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(ccHome, "config.json"), []byte(ccswitchFixtureConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	gatewayDir := t.TempDir()
	t.Chdir(gatewayDir)
	if err := os.WriteFile("providers.yaml", []byte("providers: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(".env", []byte("LITELLM_MASTER_KEY=abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return ccHome
}

func TestCCSwitchHandlerScanRedactsSecrets(t *testing.T) {
	ccHome := setupCCSwitchFixture(t)
	router := provider.NewRouter(log.New(os.Stderr, "", 0))
	handler := NewCCSwitchHandler(ccHome, 4001, router, log.New(os.Stderr, "", 0))

	engine := gin.New()
	engine.GET("/admin/ccswitch", handler.HandleScan)

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/admin/ccswitch", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"status":"importable"`) {
		t.Fatalf("no importable candidate: %s", body)
	}
	if strings.Contains(body, "sk-handler-claude") {
		t.Fatalf("response leaks API key: %s", body)
	}
}

func TestCCSwitchHandlerImport(t *testing.T) {
	ccHome := setupCCSwitchFixture(t)
	router := provider.NewRouter(log.New(os.Stderr, "", 0))
	handler := NewCCSwitchHandler(ccHome, 4001, router, log.New(os.Stderr, "", 0))

	engine := gin.New()
	engine.POST("/admin/ccswitch/import", handler.HandleImport)

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/admin/ccswitch/import", bytes.NewBufferString(`{}`)))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"wrote":true`) || strings.Contains(body, "sk-handler-claude") {
		t.Fatalf("unexpected body: %s", body)
	}

	// 文件落地：providers.yaml 新增条目，.env 写入密钥。
	yamlRaw, err := os.ReadFile("providers.yaml")
	if err != nil || !strings.Contains(string(yamlRaw), "myglm") {
		t.Fatalf("providers.yaml missing import: %v\n%s", err, yamlRaw)
	}
	envRaw, _ := os.ReadFile(".env")
	if !strings.Contains(string(envRaw), "CCS_MYGLM_API_KEY=") {
		t.Fatalf(".env missing key:\n%s", envRaw)
	}

	// 运行时注册生效：模型可路由。
	found := false
	for _, name := range router.ListProviders() {
		if name == "glm-4.7" {
			found = true
		}
	}
	if !found {
		t.Fatalf("model not registered live: %v", router.ListProviders())
	}
	os.Unsetenv("CCS_MYGLM_API_KEY")
}

// remoteRowsPayload 模拟桌面端上传的 cc-switch providers 行。
const remoteRowsBody = `{"rows":[
	{"id":"p1","app_type":"claude","name":"MyGLM","is_current":true,
	 "settings_config":"{\"env\":{\"ANTHROPIC_BASE_URL\":\"https://glmx.example.com\",\"ANTHROPIC_AUTH_TOKEN\":\"sk-remote-claude\",\"ANTHROPIC_MODEL\":\"glm-4.7\"}}","meta":"{}"},
	{"id":"p2","app_type":"claude","name":"Loop Relay",
	 "settings_config":"{\"env\":{\"ANTHROPIC_BASE_URL\":\"http://localhost:3000\",\"ANTHROPIC_AUTH_TOKEN\":\"sk-loop\",\"ANTHROPIC_MODEL\":\"loop-model\"}}","meta":"{}"}
]}`

func TestCCSwitchRemoteScanAndImport(t *testing.T) {
	ccHome := setupCCSwitchFixture(t)
	router := provider.NewRouter(log.New(os.Stderr, "", 0))
	handler := NewCCSwitchHandler(ccHome, 4001, router, log.New(os.Stderr, "", 0))

	engine := gin.New()
	engine.POST("/admin/ccswitch/remote-scan", handler.HandleRemoteScan)
	engine.POST("/admin/ccswitch/remote-import", handler.HandleRemoteImport)

	// 预览：不写文件、不泄密钥、回环条目带标记。
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/admin/ccswitch/remote-scan", strings.NewReader(remoteRowsBody)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("scan status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"importable":2`) || strings.Contains(body, "sk-remote-claude") {
		t.Fatalf("scan body = %s", body)
	}
	if !strings.Contains(body, `"loopback":true`) {
		t.Fatalf("loopback flag missing: %s", body)
	}
	if _, err := os.Stat("providers.yaml.pre-import.bak"); !os.IsNotExist(err) {
		t.Fatal("remote scan must not write backups")
	}

	// 导入：写文件并即时注册。
	recorder = httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/admin/ccswitch/remote-import",
		strings.NewReader(`{"rows":[{"id":"p1","app_type":"claude","name":"MyGLM","settings_config":"{\"env\":{\"ANTHROPIC_BASE_URL\":\"https://glmx.example.com\",\"ANTHROPIC_AUTH_TOKEN\":\"sk-remote-claude\",\"ANTHROPIC_MODEL\":\"glm-4.7\"}}","meta":"{}"}],"include":["myglm"]}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("import status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	body = recorder.Body.String()
	if !strings.Contains(body, `"wrote":true`) || !strings.Contains(body, `"imported":["myglm"]`) {
		t.Fatalf("import body = %s", body)
	}
	yamlRaw, err := os.ReadFile("providers.yaml")
	if err != nil || !strings.Contains(string(yamlRaw), "myglm") {
		t.Fatalf("providers.yaml missing remote import: %v\n%s", err, yamlRaw)
	}
	found := false
	for _, name := range router.ListProviders() {
		if name == "glm-4.7" {
			found = true
		}
	}
	if !found {
		t.Fatalf("remote import not registered live: %v", router.ListProviders())
	}
	os.Unsetenv("CCS_MYGLM_API_KEY")
}
