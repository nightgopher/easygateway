//go:build windows

package main

import (
	"context"
	"github.com/nightgopher/easygateway/internal/provider"
	"log"
)

// Windows uses POST /admin/config/reload instead of Unix signals.
func watchConfigReload(context.Context, *provider.ConfigReloader, *log.Logger) {}
