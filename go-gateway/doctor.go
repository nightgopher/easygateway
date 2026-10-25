package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/nightgopher/easygateway/internal/piconfig"
	"github.com/nightgopher/easygateway/internal/provider"
)

func doctorCommand(args []string) error {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	endpoint := flags.String("endpoint", "http://127.0.0.1:4001", "Gateway base URL")
	probe := flags.Bool("probe-anonymous", false, "Probe anonymous chat models with max_tokens=1; paid models are never probed")
	timeout := flags.Duration("timeout", 75*time.Second, "Overall timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	u, err := url.Parse(*endpoint)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("endpoint must be an HTTP(S) URL without credentials, query or fragment")
	}
	_ = godotenv.Load()
	key := os.Getenv("ADMIN_TOKEN")
	if key == "" {
		key = os.Getenv("LITELLM_MASTER_KEY")
	}
	if key == "" {
		key, err = piconfig.MasterKey(defaultGatewayHome())
		if err != nil {
			return fmt.Errorf("set ADMIN_TOKEN or LITELLM_MASTER_KEY, or run from the gateway directory containing .env")
		}
	}
	body, _ := json.Marshal(map[string]bool{"probe_anonymous": *probe})
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(*endpoint, "/")+"/admin/models/doctor", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("doctor returned HTTP %d", resp.StatusCode)
	}
	_, err = io.Copy(os.Stdout, io.LimitReader(resp.Body, 4<<20))
	return err
}

func startModelDoctor(ctx context.Context, r *provider.Router, interval time.Duration, probe bool, logger *log.Logger) {
	if interval <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				checkCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
				_, err := r.Doctor(checkCtx, probe)
				cancel()
				if err != nil && ctx.Err() == nil {
					logger.Printf("Model doctor: %v", err)
				}
			}
		}
	}()
}
