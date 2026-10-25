package handlers

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/archive"
	"github.com/nightgopher/easygateway/internal/metrics"
	"github.com/nightgopher/easygateway/internal/middleware"
	"github.com/nightgopher/easygateway/internal/provider"
)

func TestTruncatedStreamStopsFallbackAndRecordsInterruption(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			logger := log.New(io.Discard, "", 0)
			store := &capturingArchiveStore{}
			archiver := archive.NewArchiver(store, archive.Config{Enabled: true, MaxBodyKB: 256}, logger)
			defer archiver.Close()
			router := provider.NewRouter(logger)
			broken := &stubChatProvider{name: "broken", streamData: strings.Split(guardedSuccessSSE, "event: content_block_stop")[0]}
			good := &stubChatProvider{name: "good", streamData: guardedSuccessSSE}
			router.RegisterProvider("broken", broken)
			router.RegisterProvider("good", good)
			router.RegisterChain("coding", []string{"broken", "good"})
			collector := metrics.NewCollector()
			engine := gin.New()
			engine.Use(middleware.Logging(logger, collector))
			body := `{"model":"coding","stream":true,"messages":[{"role":"user","content":"hi"}]}`
			switch path {
			case "/v1/messages":
				h := NewMessageHandler(router, logger)
				h.SetArchiver(archiver)
				engine.POST(path, h.Handle)
			case "/v1/chat/completions":
				h := NewChatCompletionsHandler(router, logger)
				h.SetArchiver(archiver)
				engine.POST(path, h.Handle)
			case "/v1/responses":
				h := NewResponsesHandler(router, logger)
				h.SetArchiver(archiver)
				engine.POST(path, h.Handle)
				body = `{"model":"coding","stream":true,"input":"hi"}`
			}
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
			archiver.Close()
			if good.streamCalls != 0 || !strings.Contains(w.Body.String(), "without terminal event") {
				t.Fatalf("fallback=%d output=%s", good.streamCalls, w.Body.String())
			}
			if got := store.first().Status; got != archive.StatusInterrupted {
				t.Fatalf("archive status=%s", got)
			}
			stats := collector.GetModelStats()
			if len(stats) != 1 || stats[0].Successes != 0 || stats[0].Errors != 1 {
				t.Fatalf("truncated stream counted as success: %+v", stats)
			}
		})
	}
}
