package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type deepVTestAuth struct {
	mu        sync.Mutex
	token     string
	refreshes int
}

func TestDeepVLegacyAliasPrefersAccountAndFallsBackAfterLogout(t *testing.T) {
	auth := &deepVTestAuth{}
	var got []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Authorization"))
		fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
	}))
	defer server.Close()
	p := NewDeepVProvider(&Config{Name: "deepseek-flash", URL: server.URL, Auth: auth, RefreshUnauthorizedOnly: true}, t.TempDir(), "deepseek-flash").AllowLegacyCredential()
	p.tokenCache = &jwtToken{AccessToken: "legacy-secret", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	req := &Request{Model: "deepseek-flash"}
	if _, err := p.ForwardRequest(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	auth.mu.Lock()
	auth.token = "account-secret"
	auth.mu.Unlock()
	if _, err := p.ForwardRequest(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	auth.mu.Lock()
	auth.token = ""
	auth.mu.Unlock()
	if _, err := p.ForwardRequest(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "Bearer legacy-secret" || got[1] != "Bearer account-secret" || got[2] != "Bearer legacy-secret" {
		t.Fatal("incorrect account/legacy credential selection")
	}
}

func (a *deepVTestAuth) BearerToken() string { a.mu.Lock(); defer a.mu.Unlock(); return a.token }
func (a *deepVTestAuth) Refresh(context.Context) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refreshes++
	a.token = "new-secret"
	return true
}

func TestDeepVAccountRefreshAndStream(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint("stream=", streaming), func(t *testing.T) {
			auth := &deepVTestAuth{token: "old-secret"}
			attempts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts++
				var wire struct {
					Model string `json:"model"`
				}
				if json.NewDecoder(r.Body).Decode(&wire) != nil || wire.Model != "upstream-model" {
					t.Error("wrong upstream model")
				}
				if r.Header.Get("X-Git-Remotes") != "" || r.Header.Get("X-Git-Branch") != "" {
					t.Error("sent gateway git metadata")
				}
				if r.Header.Get("Authorization") == "Bearer old-secret" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				if r.Header.Get("Authorization") != "Bearer new-secret" {
					t.Error("wrong bearer token")
				}
				if streaming {
					if r.URL.Path != "/v1/chat/stream" {
						t.Error("wrong stream endpoint")
					}
					fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hello\"}]},\"finishReason\":\"STOP\"}]}\n\ndata: [DONE]\n\n")
				} else {
					fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"text":"hello"}]},"finishReason":"STOP"}]}`)
				}
			}))
			defer server.Close()
			p := NewDeepVProvider(&Config{Name: "easycode/upstream-model", URL: server.URL + "/v1/chat/messages", Auth: auth, RefreshUnauthorizedOnly: true}, t.TempDir(), "upstream-model").ConfigureAccountModel([]string{CapabilityText, CapabilityStreaming})
			if hasCapability(p.Capabilities(), CapabilityVision) {
				t.Fatal("text-only catalog model advertised vision")
			}
			var request Request
			if err := json.Unmarshal([]byte(`{"model":"easycode/upstream-model","messages":[{"role":"user","content":"hello"}]}`), &request); err != nil {
				t.Fatal(err)
			}
			if streaming {
				var output bytes.Buffer
				if err := p.ForwardStream(context.Background(), &request, &output); err != nil || !strings.Contains(output.String(), "hello") {
					t.Fatalf("stream failed: %v", err)
				}
			} else if _, err := p.ForwardRequest(context.Background(), &request); err != nil {
				t.Fatal(err)
			}
			if attempts != 2 || auth.refreshes != 1 {
				t.Fatalf("attempts %d refreshes %d", attempts, auth.refreshes)
			}
		})
	}
}

func TestDeepVAccountForbiddenDoesNotRotateAndRedactsToken(t *testing.T) {
	auth := &deepVTestAuth{token: "secret-sentinel"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":"secret-sentinel denied"}`)
	}))
	defer server.Close()
	p := NewDeepVProvider(&Config{Name: "easycode/model", URL: server.URL, Auth: auth, RefreshUnauthorizedOnly: true}, "", "model").ConfigureAccountModel([]string{CapabilityText})
	_, err := p.ForwardRequest(context.Background(), &Request{Model: "easycode/model"})
	if err == nil || strings.Contains(err.Error(), "secret-sentinel") || auth.refreshes != 0 {
		t.Fatalf("unsafe 403 handling: %v", err)
	}
}
