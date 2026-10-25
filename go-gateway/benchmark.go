package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/nightgopher/easygateway/internal/benchmark"
	"github.com/nightgopher/easygateway/internal/piconfig"
)

func benchmarkModels(args []string) error {
	flags := flag.NewFlagSet("benchmark", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	endpoint := flags.String("endpoint", "http://127.0.0.1:4001", "Gateway base URL")
	models := flags.String("models", "coding", "Comma-separated model IDs or routing aliases")
	samples := flags.Int("samples", 3, "Samples per model (1..10, serial)")
	timeout := flags.Duration("timeout", 60*time.Second, "Total timeout per sample")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	parsed, err := url.Parse(*endpoint)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("endpoint must be an HTTP(S) base URL without credentials, query or fragment")
	}
	_ = godotenv.Load()
	key := os.Getenv("LITELLM_MASTER_KEY")
	if key == "" {
		key, err = piconfig.MasterKey(defaultGatewayHome())
		if err != nil {
			return fmt.Errorf("set LITELLM_MASTER_KEY or run from the gateway directory containing .env")
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	results, err := benchmark.Run(ctx, client, *endpoint, key, strings.Split(*models, ","), *samples, *timeout)
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if encodeErr := encoder.Encode(results); encodeErr != nil {
		return encodeErr
	}
	return err
}
