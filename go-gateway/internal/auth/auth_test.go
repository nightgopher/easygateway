package auth

import (
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func newTestRouter(masterKey string) *gin.Engine {
	logger := log.New(os.Stderr, "", 0)
	e := gin.New()
	e.Use(BearerAuth(masterKey, logger))
	e.POST("/test", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	e.GET("/health", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	e.GET("/readyz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ready"}) })
	return e
}

func TestBearerAuthValidToken(t *testing.T) {
	e := newTestRouter("test-key")
	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("Expected 200, got %d", w.Code)
	}
}

func TestBearerAuthInvalidToken(t *testing.T) {
	e := newTestRouter("test-key")
	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	req.Header.Set("Authorization", "Bearer wrong-key")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("Expected 401, got %d", w.Code)
	}
}

func TestBearerAuthMissingHeader(t *testing.T) {
	e := newTestRouter("test-key")
	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("Expected 401, got %d", w.Code)
	}
}

func TestBearerAuthHealthPublic(t *testing.T) {
	e := newTestRouter("test-key")
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	// 故意不设置 Authorization header
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("Expected 200 (public endpoint), got %d", w.Code)
	}
}

func TestBearerAuthReadinessPublic(t *testing.T) {
	e := newTestRouter("test-key")
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("Expected 200 (public endpoint), got %d", w.Code)
	}
}

func TestRemovedBrowserPathsHaveNoAuthExemption(t *testing.T) {
	e := newTestRouter("test-key")
	for _, path := range []string{"/", "/dashboard", "/dashboard/models", "/assets/index.js", "/favicon.ico"} {
		for _, token := range []string{"", "test-key"} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			want := http.StatusUnauthorized
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
				want = http.StatusNotFound
			}
			w := httptest.NewRecorder()
			e.ServeHTTP(w, req)
			if w.Code != want {
				t.Errorf("%s: expected %d, got %d", path, want, w.Code)
			}
		}
	}
}

func TestBearerAuthAllowsConfiguredAdminTokenOnAdminPaths(t *testing.T) {
	logger := log.New(os.Stderr, "", 0)
	e := gin.New()
	e.Use(BearerAuthWithAdminToken("master-key", "admin-key", logger))
	e.GET("/admin/providers", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

	req := httptest.NewRequest(http.MethodGet, "/admin/providers", nil)
	req.Header.Set("Authorization", "Bearer admin-key")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected admin token to be accepted, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSecureTokenEqual(t *testing.T) {
	if !secureTokenEqual("same-token", "same-token") {
		t.Error("identical tokens should compare equal")
	}
	if secureTokenEqual("test-key", "wrong-key") {
		t.Error("different tokens should not compare equal")
	}
	if secureTokenEqual("", "x") || secureTokenEqual("x", "") {
		t.Error("empty vs non-empty should not compare equal")
	}
}

func TestBearerAuthRateLimitsFailures(t *testing.T) {
	e := newTestRouter("test-key")
	req := func() int {
		req := httptest.NewRequest(http.MethodPost, "/test", nil)
		req.Header.Set("Authorization", "Bearer wrong-key")
		w := httptest.NewRecorder()
		e.ServeHTTP(w, req)
		return w.Code
	}
	for i := 0; i < authFailureMax; i++ {
		if code := req(); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: expected 401, got %d", i+1, code)
		}
	}
	if code := req(); code != http.StatusTooManyRequests {
		t.Errorf("expected 429 after %d failures, got %d", authFailureMax, code)
	}
	// 有效 token 也应在拒绝窗口内被限流（防猜解优先）。
	valid := httptest.NewRequest(http.MethodPost, "/test", nil)
	valid.Header.Set("Authorization", "Bearer test-key")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, valid)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("valid token within blocked window should still be limited, got %d", w.Code)
	}
}

func TestFailureLimiterWindowExpires(t *testing.T) {
	f := newFailureLimiter(authFailureWindow, authFailureMax)
	now := time.Now()
	for i := 0; i < authFailureMax; i++ {
		f.recordFail("1.2.3.4", now)
	}
	if f.allow("1.2.3.4", now) {
		t.Error("limiter should block at the limit")
	}
	if !f.allow("1.2.3.4", now.Add(authFailureWindow+time.Second)) {
		t.Error("limiter should unblock after the window expires")
	}
	if !f.allow("5.6.7.8", now) {
		t.Error("other IPs must not be affected")
	}
}
