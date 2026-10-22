// Package corspolicy holds the live browser-origin policy. Empty allowlists
// retain the gateway's historical wildcard behavior.
package corspolicy

import (
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
)

type Config struct {
	AllowedOrigins []string `yaml:"allowed_origins,omitempty" json:"allowed_origins"`
}

func NormalizeOrigins(origins []string) ([]string, error) {
	result := []string{}
	seen := map[string]bool{}
	for _, raw := range origins {
		raw = strings.TrimSpace(raw)
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || strings.ContainsAny(raw, "*\\\r\n?#") {
			return nil, fmt.Errorf("CORS origins must be HTTP(S) origins without credentials, paths, wildcards, query or fragment; use an empty list for all origins")
		}
		u.Host = strings.ToLower(u.Host)
		if (u.Scheme == "https" && u.Port() == "443") || (u.Scheme == "http" && u.Port() == "80") {
			u.Host = u.Hostname()
			if strings.Contains(u.Host, ":") {
				u.Host = "[" + u.Host + "]"
			}
		}
		u.Path, u.RawPath = "", ""
		origin := u.String()
		if !seen[origin] {
			result = append(result, origin)
			seen[origin] = true
		}
	}
	return result, nil
}

type Policy struct {
	mu      sync.RWMutex
	config  Config
	handler gin.HandlerFunc
}

func New() *Policy { p := &Policy{}; _ = p.Apply(Config{}); return p }

// Apply takes a validated configuration; callers validate before committing it.
func (p *Policy) Apply(config Config) error {
	origins, err := NormalizeOrigins(config.AllowedOrigins)
	if err != nil {
		return err
	}
	allowed := origins
	if len(allowed) == 0 {
		allowed = []string{"*"}
	}
	handler := cors.New(cors.Config{
		AllowOrigins:     allowed,
		AllowMethods:     []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Authorization", "Accept", "X-Requested-With", "X-Request-ID", "X-AI-Source", "X-Conversation-ID", "X-Session-ID"},
		ExposeHeaders:    []string{"Content-Length", "Content-Type", "X-Request-ID", "X-Upstream-Request-ID"},
		AllowCredentials: false, MaxAge: 12 * time.Hour,
	})
	p.mu.Lock()
	p.config = Config{AllowedOrigins: origins}
	p.handler = handler
	p.mu.Unlock()
	return nil
}

func (p *Policy) Config() Config {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return Config{AllowedOrigins: append([]string{}, p.config.AllowedOrigins...)}
}

func (p *Policy) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) { p.mu.RLock(); handler := p.handler; p.mu.RUnlock(); handler(c) }
}
