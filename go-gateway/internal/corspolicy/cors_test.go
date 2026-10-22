package corspolicy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestCORSDefaultAllowlistPreflightAndInvalidUpdate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	policy := New()
	e := gin.New()
	e.Use(policy.Middleware())
	e.GET("/models", func(c *gin.Context) { c.Status(http.StatusOK) })
	request := func(method, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/models", nil)
		r.Header.Set("Origin", origin)
		if method == http.MethodOptions {
			r.Header.Set("Access-Control-Request-Method", "GET")
			r.Header.Set("Access-Control-Request-Headers", "Authorization")
		}
		w := httptest.NewRecorder()
		e.ServeHTTP(w, r)
		return w
	}
	for _, method := range []string{http.MethodGet, http.MethodOptions} {
		if got := request(method, "https://any.example").Header().Get("Access-Control-Allow-Origin"); got != "*" {
			t.Fatalf("default %s: %q", method, got)
		}
	}
	if err := policy.Apply(Config{AllowedOrigins: []string{"https://TRUSTED.example:443/"}}); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodGet, http.MethodOptions} {
		w := request(method, "https://trusted.example")
		if w.Code >= 400 || w.Header().Get("Access-Control-Allow-Origin") != "https://trusted.example" || w.Header().Get("Vary") == "" {
			t.Fatalf("allowed %s: %d %v", method, w.Code, w.Header())
		}
		w = request(method, "https://trusted.example.evil.invalid")
		if w.Code != http.StatusForbidden || w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("untrusted %s: %d %v", method, w.Code, w.Header())
		}
	}
	if err := policy.Apply(Config{AllowedOrigins: []string{"*"}}); err == nil {
		t.Fatal("wildcard list accepted")
	}
	if got := request(http.MethodGet, "https://evil.invalid").Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatal("invalid update opened policy")
	}
	if w := request(http.MethodGet, ""); w.Code != http.StatusOK {
		t.Fatal("native request blocked")
	}
}

func TestRejectNonOriginURLs(t *testing.T) {
	for _, origin := range []string{"null", "ftp://example.com", "https://example.com/path", "https://user:secret@example.com", "https://example.com?key=secret", "https://*.example.com", "https://example.com#x", ""} {
		if _, err := NormalizeOrigins([]string{origin}); err == nil {
			t.Fatalf("accepted %q", origin)
		}
	}
}
