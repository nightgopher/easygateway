package middleware

import (
	"fmt"
	"net"
	"strings"

	"github.com/gin-gonic/gin"
)

// ConfigureTrustedProxies applies the gateway's client-IP trust boundary.
//
// gin trusts every proxy by default (0.0.0.0/0 and ::/0), so Context.ClientIP()
// returns whatever the client put in X-Forwarded-For. Two things depend on that
// value being real: the auth failure limiter, which is what stops online
// guessing of the master key, and the source address recorded in request logs.
// With the default an attacker rotates a header per attempt and both stop
// working, so the default here is to trust nobody: ClientIP() reports the
// socket peer.
//
// A deployment behind a reverse proxy sets trustedProxies to that proxy's
// address or CIDR (for example "127.0.0.1" for a same-host nginx). The list is
// passed through unchanged; validation is gin's.
func ConfigureTrustedProxies(engine *gin.Engine, trustedProxies []string) error {
	if len(trustedProxies) == 0 {
		return engine.SetTrustedProxies(nil)
	}
	cleaned := make([]string, 0, len(trustedProxies))
	for _, raw := range trustedProxies {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		// Reject the wildcard forms explicitly: accepting 0.0.0.0/0 here would
		// silently restore the spoofable default this function exists to remove.
		if ip := net.ParseIP(value); ip != nil && ip.IsUnspecified() {
			return fmt.Errorf("trusted proxy %q trusts every source address; list the proxy explicitly", value)
		}
		if _, cidr, err := net.ParseCIDR(value); err == nil {
			if ones, bits := cidr.Mask.Size(); ones == 0 && bits > 0 {
				return fmt.Errorf("trusted proxy %q trusts every source address; list the proxy explicitly", value)
			}
		}
		cleaned = append(cleaned, value)
	}
	if len(cleaned) == 0 {
		return engine.SetTrustedProxies(nil)
	}
	return engine.SetTrustedProxies(cleaned)
}
