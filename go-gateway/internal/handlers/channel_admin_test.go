package handlers

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/channel"
	"github.com/nightgopher/easygateway/internal/provider"
)

type loginTestChannel struct {
	name              string
	accounts          *channel.Accounts
	release, canceled chan struct{}
}

func (c *loginTestChannel) Name() string                { return c.name }
func (c *loginTestChannel) Accounts() *channel.Accounts { return c.accounts }
func (c *loginTestChannel) BeginLogin(context.Context) (channel.LoginSession, error) {
	return &loginTestSession{c}, nil
}

type loginTestSession struct{ c *loginTestChannel }

func (s *loginTestSession) Channel() string { return s.c.name }
func (s *loginTestSession) DeviceCode() channel.DeviceCode {
	return channel.DeviceCode{VerificationURI: "https://auth.example.test", ExpiresIn: time.Minute}
}
func (s *loginTestSession) Wait(ctx context.Context) (*channel.Credential, error) {
	select {
	case <-ctx.Done():
		close(s.c.canceled)
		return nil, ctx.Err()
	case <-s.c.release:
		return &channel.Credential{AccessToken: "credential-secret-sentinel", Nickname: "test"}, nil
	}
}
func channelAdminFixture(t *testing.T) (*ChannelAdminHandler, *gin.Engine, *loginTestChannel) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c := &loginTestChannel{name: "minimax", accounts: channel.NewAccounts("minimax", channel.NewStore(t.TempDir()), nil, nil), release: make(chan struct{}), canceled: make(chan struct{})}
	r := channel.NewRegistry()
	r.Register(c)
	h := NewChannelAdminHandler(r, log.New(io.Discard, "", 0))
	e := gin.New()
	e.GET("/channels", h.HandleList)
	e.POST("/channels/:channel/login", h.HandleLoginStart)
	e.GET("/channels/:channel/login/:id", h.HandleLoginPoll)
	e.DELETE("/channels/:channel/login/:id", h.HandleLoginCancel)
	return h, e, c
}
func adminRequest(e *gin.Engine, method, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w
}
func startTestLogin(t *testing.T, e *gin.Engine) string {
	t.Helper()
	w := adminRequest(e, "POST", "/channels/minimax/login")
	var out struct {
		ID string `json:"login_id"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.ID == "" {
		t.Fatal(w.Code, w.Body.String())
	}
	return out.ID
}
func TestChannelLoginCancelStopsSessionAndIsIdempotent(t *testing.T) {
	h, e, c := channelAdminFixture(t)
	id := startTestLogin(t, e)
	w := adminRequest(e, "DELETE", "/channels/minimax/login/"+id)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	select {
	case <-c.canceled:
	case <-time.After(time.Second):
		t.Fatal("server session did not cancel")
	}
	h.mu.Lock()
	remaining := len(h.pendings)
	h.mu.Unlock()
	if remaining != 0 {
		t.Fatal("pending session retained")
	}
	if w = adminRequest(e, "GET", "/channels/minimax/login/"+id); w.Code != 404 {
		t.Fatal("cancelled session still pollable")
	}
	if w = adminRequest(e, "DELETE", "/channels/minimax/login/"+id); w.Code != 200 {
		t.Fatal("cancel not idempotent")
	}
	if c.Accounts().Len() != 0 {
		t.Fatal("cancel added an account")
	}
}
func TestChannelLoginSuccessClearsOnlyItsStaleCircuits(t *testing.T) {
	h, e, c := channelAdminFixture(t)
	r := provider.NewRouter(log.New(io.Discard, "", 0))
	h.SetRouter(r)
	for _, name := range []string{"owned", "other"} {
		r.RegisterProvider(name, provider.NewOpenAIProvider(&provider.Config{Name: name, URL: "http://example.test"}))
		for i := 0; i < 3; i++ {
			r.RecordProviderFailure(name, &provider.ProviderError{StatusCode: 401})
		}
	}
	r.SetProviderChannel("owned", "minimax")
	r.SetProviderChannel("other", "cline")
	id := startTestLogin(t, e)
	close(c.release)
	h.mu.Lock()
	pending := h.pendings[id]
	h.mu.Unlock()
	select {
	case <-pending.done:
	case <-time.After(time.Second):
		t.Fatal("login not done")
	}
	w := adminRequest(e, "GET", "/channels/minimax/login/"+id)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"complete"`) || strings.Contains(w.Body.String(), "credential-secret-sentinel") {
		t.Fatal(w.Body.String())
	}
	own, _ := r.ProviderStatus("owned")
	other, _ := r.ProviderStatus("other")
	if own.State != provider.CircuitClosed || other.State != provider.CircuitOpen {
		t.Fatal("wrong circuits reset", own.State, other.State)
	}
	w = adminRequest(e, "GET", "/channels")
	if !strings.Contains(w.Body.String(), `"models":["owned"]`) || !strings.Contains(w.Body.String(), `"login_available":true`) {
		t.Fatal(w.Body.String())
	}
}
