package main

import (
	"context"
	"log"
	"os"
	"sync"
	"time"

	"github.com/nightgopher/easygateway/internal/channel"
	"github.com/nightgopher/easygateway/internal/provider"
)

// The account catalog is kept separate from providers.yaml. Updates and YAML
// reloads share a lock so neither can silently discard the other's changes.
type easyCodeCatalogAccount interface {
	Accounts() *channel.Accounts
	Models(context.Context) ([]channel.EasyCodeModel, error)
}

type easyCodeCatalog struct {
	mu             sync.Mutex
	account        easyCodeCatalogAccount
	router, native *provider.Router
	reloader       *provider.ConfigReloader
	workDir        string
	logger         *log.Logger
}

func newEasyCodeCatalog(account easyCodeCatalogAccount, router, native *provider.Router, reloader *provider.ConfigReloader, dir string, logger *log.Logger) *easyCodeCatalog {
	if dir == "" {
		dir, _ = os.Getwd()
	}
	return &easyCodeCatalog{account: account, router: router, native: native, reloader: reloader, workDir: dir, logger: logger}
}

func (c *easyCodeCatalog) channelChanged(ctx context.Context, name string) error {
	if name != "easycode" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	return c.sync(ctx, true)
}

func (c *easyCodeCatalog) sync(ctx context.Context, accountChanged bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var entries []provider.DynamicChannelModel
	var refreshErr error
	if c.account.Accounts().Len() > 0 {
		models, err := c.account.Models(ctx)
		if err != nil {
			refreshErr = err
			c.logger.Printf("EasyCode model catalog refresh failed: %v", err)
			if !accountChanged {
				return err
			}
		} else {
			for _, model := range models {
				name := "easycode/" + model.ID
				caps := []string{provider.CapabilityText, provider.CapabilityToolCall, provider.CapabilityStreaming, provider.CapabilityReasoning}
				modalities := []string{"text"}
				if model.SupportsImages {
					caps = append(caps, provider.CapabilityVision)
					modalities = append(modalities, "image")
				}
				cfg := &provider.Config{Name: name, URL: "https://api-code.deepvlab.ai/v1/chat/messages", Auth: c.account.Accounts(), RefreshUnauthorizedOnly: true}
				maxInput, maxOutput := model.ContextWindow, 32000
				if maxInput > 0 {
					if maxInput < maxOutput*2 {
						maxOutput = maxInput / 4
					}
					maxInput -= maxOutput
				}
				p := provider.NewDeepVProvider(cfg, c.workDir, model.ID).ConfigureAccountModel(caps)
				entries = append(entries, provider.DynamicChannelModel{Provider: p, Info: provider.ModelInfo{ID: name, Provider: name, Description: model.Name, Capabilities: caps, InputModalities: modalities, MaxInputTokens: maxInput, MaxOutputTokens: maxOutput}})
			}
		}
	}
	c.reloader.Synchronize(func() {
		c.native.ReplaceDynamicChannelModels("easycode", entries)
		c.router.ReplaceDynamicChannelModels("easycode", entries)
		if accountChanged {
			c.router.ResetDeepVAccountProviders()
		}
	})
	return refreshErr
}

func (c *easyCodeCatalog) run(parent context.Context) {
	refresh := func() {
		ctx, cancel := context.WithTimeout(parent, 25*time.Second)
		defer cancel()
		_ = c.sync(ctx, false)
	}
	refresh()
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-parent.Done():
			return
		case <-ticker.C:
			refresh()
		}
	}
}
