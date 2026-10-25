package handlers

import (
	"bytes"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/archive"
	"github.com/nightgopher/easygateway/internal/provider"
)

func TestArchiveSinkWriteReportsFullInputLengthAndStaysBounded(t *testing.T) {
	sink := newArchiveSink(64)
	payload := bytes.Repeat([]byte("x"), 128)
	n, err := sink.Write(payload)
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if n != len(payload) {
		t.Fatalf("Write() count = %d, want %d", n, len(payload))
	}
	if sink.Len() > 64 {
		t.Fatalf("sink length = %d, exceeds configured limit", sink.Len())
	}
	if !bytes.Contains(sink.Bytes(), []byte("stream archive middle truncated")) {
		t.Fatal("bounded stream archive is missing truncation marker")
	}
}

func TestArchiveSinkPreservesTerminalEventAtSmallCapacity(t *testing.T) {
	sink := newArchiveSink(64)
	_, _ = sink.Write(bytes.Repeat([]byte("x"), 128))
	_, _ = sink.Write([]byte("\ndata: [DONE]\n\n"))

	if sink.Len() > 64 {
		t.Fatalf("sink length = %d, exceeds configured limit", sink.Len())
	}
	if !bytes.HasSuffix(sink.Bytes(), []byte("data: [DONE]\n\n")) {
		t.Fatalf("bounded stream archive lost terminal event: %q", sink.Bytes())
	}
	status, reason := parseStreamEndState(sink.Bytes(), nil)
	if status != archive.StatusCompleted {
		t.Fatalf("stream status = %s, want completed (reason %q)", status, reason)
	}
}

func TestRedactSSETranscriptJoinsMultilineDataBeforeRedacting(t *testing.T) {
	raw := []byte("event: response.completed\n" +
		"data: {\"response\":\n" +
		"data: {\"usage\":{\"token\":\"multiline-secret\"}}}\n\n")
	out := string(redactSSETranscript(raw))
	if strings.Contains(out, "multiline-secret") {
		t.Fatalf("secret leaked from multiline SSE data: %s", out)
	}
	if !strings.Contains(out, "event: response.completed") {
		t.Fatalf("event metadata was lost: %s", out)
	}
}

func TestSanitizeErrorReasonScrubsCredentialFormats(t *testing.T) {
	reason := "upstream rejected api_key=plain-secret token=token-secret password=password-secret"
	out := sanitizeErrorReason(reason)
	for _, secret := range []string{"plain-secret", "token-secret", "password-secret"} {
		if strings.Contains(out, secret) {
			t.Errorf("secret %q leaked into error reason: %q", secret, out)
		}
	}
}

func TestArchiveErrorReasonOmitsUntrustedProviderMessage(t *testing.T) {
	err := &provider.ProviderError{
		Provider:   "ali",
		StatusCode: 401,
		Message:    "invalid api key: ali-plain-secret-without-prefix",
	}
	reason := archiveErrorReason(err)
	if strings.Contains(reason, "ali-plain-secret-without-prefix") {
		t.Fatalf("provider error message leaked into archive reason: %q", reason)
	}
	if !strings.Contains(reason, "HTTP 401") {
		t.Fatalf("archive reason lost status context: %q", reason)
	}
}

func TestParseStreamEndStateClassifiesPreBodyError(t *testing.T) {
	status, reason := parseStreamEndState(nil, errors.New("provider unavailable"))
	if status != archive.StatusError {
		t.Fatalf("empty stream status = %s, want error", status)
	}
	if reason == "" {
		t.Fatal("empty stream error reason should be populated")
	}

	status, _ = parseStreamEndState([]byte("data: partial\n\n"), errors.New("connection reset"))
	if status != archive.StatusInterrupted {
		t.Fatalf("partial stream status = %s, want interrupted", status)
	}
}

func TestUsageTrackingWriterExtractsAnthropicAndResponsesUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	dst := new(bytes.Buffer)
	tracker := newUsageTrackingWriter(dst, c)
	_, _ = tracker.Write([]byte("event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":7}}}\n\n"))
	_, _ = tracker.Write([]byte("event: response.completed\ndata: {\"response\":{\"usage\":{\"output_tokens\":11}}}\n\n"))
	tracker.Finish()
	if got := c.GetInt("request_input_tokens"); got != 7 {
		t.Fatalf("input tokens = %d, want 7", got)
	}
	if got := c.GetInt("request_output_tokens"); got != 11 {
		t.Fatalf("output tokens = %d, want 11", got)
	}
	if dst.Len() == 0 {
		t.Fatal("usage tracker did not forward stream bytes")
	}
}
