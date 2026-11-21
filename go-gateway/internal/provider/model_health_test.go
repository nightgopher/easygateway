package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDoctorNeverCallsPaidModelsAndBoundsAnonymousProbe(t *testing.T) {
	var anonymous, paid atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if r.Header.Get("Authorization") != "" {
			paid.Add(1)
		} else {
			anonymous.Add(1)
		}
		if req.MaxTokens != 1 {
			t.Errorf("unbounded probe: max_tokens=%d", req.MaxTokens)
		}
		if req.Model == "broken" {
			w.WriteHeader(400)
			io.WriteString(w, `{"error":{"message":"ModelProtocolUnsupported TOP_SECRET"}}`)
			return
		}
		io.WriteString(w, `{"id":"test","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()
	t.Setenv("DOCTOR_PAID_TEST_KEY", "paid-secret")
	body := fmt.Sprintf("providers:\n - name: anonymous\n   type: openai\n   url: %s\n   allow_no_key: true\n   models: [{id: free}, {id: broken}]\n - name: paid\n   type: openai\n   url: %s\n   api_key_env: DOCTOR_PAID_TEST_KEY\n   models: [{id: paid}]\n", upstream.URL, upstream.URL)
	r := NewRouter(nil)
	if _, err := SetupProvidersFromConfig(r, writeTempProvidersConfig(t, body), r.logger); err != nil {
		t.Fatal(err)
	}
	if report, err := r.Doctor(context.Background(), false); err != nil || report.Probed != 0 {
		t.Fatalf("auth-only report=%+v err=%v", report, err)
	}
	if anonymous.Load() != 0 || paid.Load() != 0 {
		t.Fatal("default doctor made an inference request")
	}
	report, err := r.Doctor(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if report.Probed != 2 || anonymous.Load() != 2 || paid.Load() != 0 {
		t.Fatalf("calls: probes=%d anonymous=%d paid=%d", report.Probed, anonymous.Load(), paid.Load())
	}
	if report.Models["free"].Status != "available" || report.Models["paid"].Status != "unknown" || report.Models["broken"].Available {
		t.Fatalf("health=%+v", report.Models)
	}
	if report.Models["broken"].LastProbeAt == nil || report.Models["broken"].ProbeExpiresAt == nil || strings.Contains(report.Models["broken"].Reason, "TOP_SECRET") {
		t.Fatal("missing timestamp or unredacted upstream detail")
	}
	expired := r.modelHealthsAt(time.Now().Add(time.Hour))
	if expired["broken"].Status != "unknown" || !expired["broken"].Available {
		t.Fatalf("expired failure treated as permanent: %+v", expired["broken"])
	}
}
