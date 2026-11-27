package privacy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/gwkeys"
)

func TestPrivacyGatewayRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	g, err := NewGate(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	g.SetEnabled(true)
	secret := "sk-" + strings.Repeat("x", 24)
	e := gin.New()
	e.Use(g.Middleware())
	e.POST("/v1/chat/completions", func(c *gin.Context) {
		body, _ := io.ReadAll(c.Request.Body)
		if bytes.Contains(body, []byte(secret)) {
			t.Error("upstream received secret")
		}
		var request struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Fatal(err)
		}
		c.JSON(200, gin.H{"choices": []any{gin.H{"message": gin.H{"content": request.Messages[0].Content}}}})
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"demo","messages":[{"role":"user","content":"`+secret+`"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), secret) {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}
func TestPrivacyRejectsUnsupportedInput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	g, _ := NewGate(1 << 20)
	g.SetEnabled(true)
	e := gin.New()
	e.Use(g.Middleware())
	called := false
	e.POST("/v1/messages", func(c *gin.Context) { called = true })
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[{"content":[{"type":"image","source":{"data":"abc"}}]}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	if called || w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d", w.Code)
	}
}
func TestPrivacyGatewayStreamsToolArguments(t *testing.T) {
	gin.SetMode(gin.TestMode)
	g, _ := NewGate(1 << 20)
	g.SetEnabled(true)
	secret := "sk-" + strings.Repeat("x", 24)
	e := gin.New()
	e.Use(g.Middleware())
	e.POST("/v1/chat/completions", func(c *gin.Context) {
		body, _ := io.ReadAll(c.Request.Body)
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		token := req.Messages[0].Content
		c.Header("Content-Type", "text/event-stream")
		for _, part := range []string{token[:12], token[12:]} {
			b, _ := json.Marshal(gin.H{"choices": []any{gin.H{"index": 0, "delta": gin.H{"tool_calls": []any{gin.H{"index": 0, "function": gin.H{"arguments": part}}}}}}})
			_, _ = c.Writer.Write([]byte("data: " + string(b) + "\n\n"))
			c.Writer.Flush()
		}
		_, _ = c.Writer.Write([]byte("data: [DONE]\n\n"))
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"demo","stream":true,"messages":[{"role":"user","content":"`+secret+`"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), secret) {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}

func TestSettingsPersistAndDefaultOff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "privacy.json")
	g, _ := NewGate(0)
	settings, err := OpenSettings(path, g)
	if err != nil {
		t.Fatal(err)
	}
	if g.Enabled() {
		t.Fatal("privacy defaulted on")
	}
	if err := settings.Set(true); err != nil {
		t.Fatal(err)
	}
	other, _ := NewGate(0)
	_, err = OpenSettings(path, other)
	if err != nil || !other.Enabled() {
		t.Fatalf("settings not restored: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("incorrect permissions")
	}
}

func TestMalformedSavedSettingsFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "privacy.json")
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	g, _ := NewGate(0)
	if _, err := OpenSettings(path, g); err == nil {
		t.Fatal("invalid settings accepted")
	}
}

func TestPrivacyMasksStructuredStateKey(t *testing.T) {
	e, _ := NewEngine()
	s := e.NewSession("user")
	secret := "unusual-key-123456"
	body := `{"state":{"api_key":"` + secret + `"}}`
	b, err := s.MaskJSON([]byte(body))
	if err != nil || strings.Contains(string(b), secret) {
		t.Fatalf("credential leaked: %v", err)
	}
}

func TestPreviewIsLocalAndDoesNotExposeSecret(t *testing.T) {
	gin.SetMode(gin.TestMode)
	g, _ := NewGate(0)
	s, _ := OpenSettings(filepath.Join(t.TempDir(), "privacy.json"), g)
	e := gin.New()
	e.POST("/preview", s.HandlePreview)
	secret := "sk-" + strings.Repeat("z", 24)
	payload, _ := json.Marshal(map[string]string{"text": "key=" + secret})
	req := httptest.NewRequest("POST", "/preview", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	if w.Code != 200 || strings.Contains(w.Body.String(), secret) || !strings.Contains(w.Body.String(), "SECRET_") {
		t.Fatalf("unexpected result %d %s", w.Code, w.Body.String())
	}
}

func TestPrivacyAuthenticatedIdentityIsolation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	g, _ := NewGate(0)
	g.SetEnabled(true)
	e := gin.New()
	e.Use(func(c *gin.Context) { c.Set(gwkeys.ContextKey, c.GetHeader("X-Test-Identity")); c.Next() })
	e.Use(g.Middleware())
	var token string
	e.POST("/v1/messages", func(c *gin.Context) {
		body, _ := io.ReadAll(c.Request.Body)
		var req struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(body, &req)
		if c.GetHeader("X-Test-Identity") == "one" {
			token = req.Text
			c.JSON(200, gin.H{"text": "ok"})
			return
		}
		c.JSON(200, gin.H{"text": token})
	})
	secret := "sk-" + strings.Repeat("x", 24)
	send := func(id, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Test-Identity", id)
		w := httptest.NewRecorder()
		e.ServeHTTP(w, req)
		return w
	}
	send("one", `{"text":"`+secret+`"}`)
	w := send("two", `{"text":"hi"}`)
	if strings.Contains(w.Body.String(), secret) || !strings.Contains(w.Body.String(), "SECRET_") {
		t.Fatalf("cross-identity restore: %s", w.Body.String())
	}
}
