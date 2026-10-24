package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	piagent "github.com/nightgopher/easyagent/sdk/agent"
)

const streamContractDelta = `data: {"choices":[{"index":0,"delta":{"content":"部分回复"},"finish_reason":null}]}` + "\n\n"

func newStreamContractAssistant(t *testing.T, handler http.HandlerFunc) *Assistant {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected SDK request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("SDK request missing configured authentication")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	a, err := New(Config{Model: "test-model", BaseURL: srv.URL, APIKey: "test-key"},
		newTestMemStore(t), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestChatSSECompletionContract(t *testing.T) {
	for _, tc := range []struct {
		name     string
		terminal string
		wantErr  string
	}{
		{
			name:     "done",
			terminal: "data: [DONE]\n\n",
		},
		{
			name:     "finish_reason",
			terminal: `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
		},
		{
			name:    "truncated_without_terminal",
			wantErr: "terminal",
		},
		{
			name:     "explicit_error_before_done",
			terminal: `data: {"error":{"message":"quota exhausted"}}` + "\n\ndata: [DONE]\n\n",
			wantErr:  "quota exhausted",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			a := newStreamContractAssistant(t, func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				fmt.Fprint(w, streamContractDelta+tc.terminal)
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var deltas strings.Builder
			final, err := a.Chat(ctx, "测试", "", func(update StreamUpdate) {
				if update.Type == "text_delta" {
					deltas.WriteString(update.Content)
				}
			})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("completed stream failed: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
			if final != "部分回复" || deltas.String() != "部分回复" {
				t.Fatalf("partial output lost: final=%q deltas=%q", final, deltas.String())
			}
			if got := requests.Load(); got != 1 {
				t.Fatalf("stream with output must not be replayed: %d requests", got)
			}
		})
	}
}

func TestChatSSECancellationReturnsContextError(t *testing.T) {
	a := newStreamContractAssistant(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, streamContractDelta)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	final, err := a.Chat(ctx, "测试取消", "", func(update StreamUpdate) {
		if update.Type == "text_delta" {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation should remain a context error, got %v", err)
	}
	if final != "部分回复" {
		t.Fatalf("cancelled partial output lost: %q", final)
	}
}

func TestChatWaitsForSDKCompletionBeforeRetry(t *testing.T) {
	var requests atomic.Int32
	a := newStreamContractAssistant(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, streamContractDelta)
		if requests.Add(1) == 1 {
			fmt.Fprint(w, `data: {"error":{"message":"first turn failed"}}`+"\n\n")
			return
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	entered := make(chan struct{})
	released := make(chan struct{})
	var firstEnd atomic.Bool
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(released) }) }
	// EventAgentEnd precedes SDK unsubscribe/channel close. Keep that cleanup
	// pending through the public subscription API, without timing the provider.
	unsubscribe := a.agents["test-model"].Subscribe(func(_ context.Context, event piagent.AgentEvent) {
		if _, ok := event.(piagent.EventAgentEnd); ok && firstEnd.CompareAndSwap(false, true) {
			close(entered)
			<-released
		}
	})
	defer unsubscribe()
	defer release()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	type result struct {
		text string
		err  error
	}
	returned := make(chan result, 1)
	go func() {
		text, err := a.Chat(ctx, "第一轮", "", nil)
		returned <- result{text: text, err: err}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("SDK did not reach its completion gate")
	}
	select {
	case got := <-returned:
		t.Fatalf("Chat returned before SDK channel close: text=%q err=%v", got.text, got.err)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case got := <-returned:
		if got.text != "部分回复" || got.err == nil || !strings.Contains(got.err.Error(), "first turn failed") {
			t.Fatalf("failed turn should retain partial result: text=%q err=%v", got.text, got.err)
		}
	case <-ctx.Done():
		t.Fatal("Chat did not finish after SDK completion was released")
	}
	final, err := a.Chat(ctx, "立即重试", "", nil)
	if err != nil || final != "部分回复" {
		t.Fatalf("retry after completed failure should be accepted: text=%q err=%v", final, err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("want one request per turn, got %d", got)
	}
}

func TestDynamicModelCatalogControlsNewAndExistingAgents(t *testing.T) {
	var sentModel string
	a := newStreamContractAssistant(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		sentModel = body.Model
		fmt.Fprint(w, streamContractDelta+"data: [DONE]\n\n")
	})
	options := []ModelOption{{ID: "new-model", Channel: "cline"}}
	a.catalog = func() []ModelOption { return options }
	if ids := a.Models(); len(ids) != 1 || ids[0] != "new-model" {
		t.Fatal(ids)
	}
	text, err := a.Chat(t.Context(), "hi", "new-model", nil)
	if err != nil || text != "部分回复" || sentModel != "new-model" {
		t.Fatalf("new catalog model unusable: %q %v %q", text, err, sentModel)
	}
	options = nil // logout or disable; an already-built agent must also be rejected.
	sentModel = ""
	if _, err = a.Chat(t.Context(), "hi", "new-model", nil); err == nil || sentModel != "" {
		t.Fatal("removed model still routed")
	}
	if _, err = a.Chat(t.Context(), "hi", "", nil); err == nil {
		t.Fatal("unavailable default allowed")
	}
}
