package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// clientIPHandler echoes Context.ClientIP() so the tests can assert what the
// auth failure limiter and the request log would see.
func clientIPHandler(t *testing.T, trusted []string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	if err := ConfigureTrustedProxies(engine, trusted); err != nil {
		t.Fatalf("ConfigureTrustedProxies(%v): %v", trusted, err)
	}
	engine.GET("/v1/models", func(c *gin.Context) {
		c.String(http.StatusOK, c.ClientIP())
	})
	return engine
}

func serveClientIP(t *testing.T, engine *gin.Engine, forwarded string) string {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.RemoteAddr = "203.0.113.9:34567"
	if forwarded != "" {
		request.Header.Set("X-Forwarded-For", forwarded)
	}
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	return response.Body.String()
}

// The default must be "trust nobody": otherwise an attacker rotates a forged
// X-Forwarded-For per attempt and the auth failure limiter never trips.
func TestDefaultTrustsNoProxyAndIgnoresForwardedHeader(t *testing.T) {
	engine := clientIPHandler(t, nil)

	if got := serveClientIP(t, engine, "1.2.3.4"); got != "203.0.113.9" {
		t.Fatalf("ClientIP() = %q, want the socket peer 203.0.113.9", got)
	}
	if got := serveClientIP(t, engine, ""); got != "203.0.113.9" {
		t.Fatalf("ClientIP() = %q, want the socket peer", got)
	}
}

func TestTrustedProxyHonorsForwardedHeader(t *testing.T) {
	engine := clientIPHandler(t, []string{"127.0.0.1/32", "203.0.113.0/24"})

	if got := serveClientIP(t, engine, "198.51.100.7"); got != "198.51.100.7" {
		t.Fatalf("ClientIP() = %q, want the forwarded address for a trusted peer", got)
	}
}

func TestUntrustedPeerCannotSpoofDespiteConfiguredProxies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	if err := ConfigureTrustedProxies(engine, []string{"10.0.0.1"}); err != nil {
		t.Fatalf("ConfigureTrustedProxies: %v", err)
	}
	engine.GET("/v1/models", func(c *gin.Context) { c.String(http.StatusOK, c.ClientIP()) })

	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.RemoteAddr = "203.0.113.9:34567"
	request.Header.Set("X-Forwarded-For", "1.2.3.4")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)

	if got := response.Body.String(); got != "203.0.113.9" {
		t.Fatalf("ClientIP() = %q, want the untrusted socket peer", got)
	}
}

func TestWildcardTrustedProxyIsRejected(t *testing.T) {
	for _, wildcard := range []string{"0.0.0.0/0", "::/0", "0.0.0.0", "::"} {
		gin.SetMode(gin.TestMode)
		engine := gin.New()
		if err := ConfigureTrustedProxies(engine, []string{wildcard}); err == nil {
			t.Fatalf("ConfigureTrustedProxies(%q) accepted a wildcard, which restores the spoofable default", wildcard)
		}
	}
}

func TestBlankTrustedProxyEntriesAreIgnored(t *testing.T) {
	engine := clientIPHandler(t, []string{"  ", ""})

	if got := serveClientIP(t, engine, "1.2.3.4"); got != "203.0.113.9" {
		t.Fatalf("ClientIP() = %q, want the socket peer once the list is empty", got)
	}
}

func TestInvalidTrustedProxyIsRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	if err := ConfigureTrustedProxies(engine, []string{"not-an-address"}); err == nil {
		t.Fatal("ConfigureTrustedProxies accepted a non-address entry")
	}
}
