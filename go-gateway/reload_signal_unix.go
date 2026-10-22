//go:build !windows

package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/weijian/go-llm-gateway/internal/provider"
)

func watchConfigReload(ctx context.Context, reloader *provider.ConfigReloader, logger *log.Logger) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGHUP)
	go func() {
		defer signal.Stop(signals)
		for {
			select {
			case <-ctx.Done():
				return
			case <-signals:
				if _, err := reloader.Reload(); err != nil {
					logger.Printf("Configuration reload failed; keeping previous configuration: %v", err)
				} else {
					logger.Print("Configuration reloaded")
				}
			}
		}
	}()
}
