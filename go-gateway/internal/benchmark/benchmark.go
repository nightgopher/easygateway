// Package benchmark measures selected models through the gateway without recording prompts or responses.
package benchmark

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/nightgopher/easygateway/internal/provider"
)

type Sample struct {
	Success         bool     `json:"success"`
	StatusCode      int      `json:"status_code"`
	FirstOutputMs   *float64 `json:"first_output_ms,omitempty"`
	TotalMs         float64  `json:"total_ms"`
	OutputTokens    int      `json:"output_tokens,omitempty"`
	TokensPerSecond *float64 `json:"tokens_per_second,omitempty"`
	Error           string   `json:"error,omitempty"`
}
type Result struct {
	Model              string   `json:"model"`
	Samples            []Sample `json:"samples"`
	SuccessRate        float64  `json:"success_rate"`
	StoppedOnRateLimit bool     `json:"stopped_on_rate_limit"`
}

// Run uses serial, bounded requests and stops a model's samples on the first 429.
func Run(ctx context.Context, client *http.Client, endpoint, key string, models []string, samples int, timeout time.Duration) ([]Result, error) {
	if samples < 1 || samples > 10 || timeout <= 0 {
		return nil, fmt.Errorf("samples must be 1..10 and timeout must be positive")
	}
	results := make([]Result, 0, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			return nil, fmt.Errorf("empty model")
		}
		result := Result{Model: model, Samples: make([]Sample, 0, samples)}
		successes := 0
		for i := 0; i < samples; i++ {
			if ctx.Err() != nil {
				return results, ctx.Err()
			}
			sample := measure(ctx, client, endpoint, key, model, timeout)
			result.Samples = append(result.Samples, sample)
			if sample.Success {
				successes++
			}
			if sample.StatusCode == 429 {
				result.StoppedOnRateLimit = true
				break
			}
		}
		result.SuccessRate = float64(successes) / float64(len(result.Samples))
		results = append(results, result)
	}
	return results, nil
}
func measure(ctx context.Context, client *http.Client, endpoint, key, model string, timeout time.Duration) (sample Sample) {
	started := time.Now()
	defer func() { sample.TotalMs = float64(time.Since(started).Microseconds()) / 1000 }()
	child, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	body, _ := json.Marshal(map[string]any{"model": model, "stream": true, "max_tokens": 256, "messages": []map[string]string{{"role": "user", "content": "Reply with exactly OK."}}})
	req, err := http.NewRequestWithContext(child, http.MethodPost, strings.TrimRight(endpoint, "/")+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		sample.Error = "invalid endpoint"
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		sample.Error = "request failed or timed out"
		return
	}
	defer resp.Body.Close()
	sample.StatusCode = resp.StatusCode
	if resp.StatusCode != http.StatusOK {
		sample.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		return
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		sample.Error = "expected SSE response"
		return
	}
	scan := bufio.NewScanner(resp.Body)
	scan.Buffer(make([]byte, 4096), 2<<20)
	var frame strings.Builder
	var first time.Time
	completed := false
	consume := func() bool {
		raw := []byte(frame.String())
		progress, end := provider.ModelStreamProgress(raw)
		if progress && first.IsZero() {
			first = time.Now()
			ms := float64(time.Since(started).Microseconds()) / 1000
			sample.FirstOutputMs = &ms
		}
		for _, line := range strings.Split(frame.String(), "\n") {
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			var event struct {
				Type  string `json:"type"`
				Usage struct {
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event) == nil {
				if event.Type == "error" {
					sample.Error = "upstream stream error"
					return true
				}
				if event.Usage.OutputTokens > sample.OutputTokens {
					sample.OutputTokens = event.Usage.OutputTokens
				}
			}
		}
		completed = completed || end
		frame.Reset()
		return end
	}
	for scan.Scan() {
		line := scan.Text()
		if line == "" {
			if consume() {
				break
			}
		} else {
			frame.WriteString(line)
			frame.WriteByte('\n')
		}
	}
	if !completed && frame.Len() > 0 {
		consume()
	}
	if scan.Err() != nil {
		sample.Error = "stream failed or timed out"
		return
	}
	if sample.Error != "" {
		return
	}
	if !completed {
		sample.Error = "stream ended without completion"
		return
	}
	sample.Success = true
	if !first.IsZero() && sample.OutputTokens > 0 {
		elapsed := time.Since(first).Seconds()
		if elapsed > 0 {
			rate := float64(sample.OutputTokens) / elapsed
			sample.TokensPerSecond = &rate
		}
	}
	return
}
