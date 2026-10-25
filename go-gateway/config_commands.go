package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/joho/godotenv"
	"github.com/nightgopher/easygateway/internal/provider"
)

// These instances are installed by setupDeepVProviders, outside providers.yaml.
func externalConfigProviders() []string {
	return []string{"deepseek-flash", "deepv-glm-5.3-flash", "claude-haiku-5-5", "mimo-v2.6-pro", "copilot"}
}

func checkConfigCommand(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("config check", flag.ContinueOnError)
	path := flags.String("file", "providers.yaml", "Providers YAML path")
	allowMissing := flags.Bool("allow-missing-keys", false, "Report missing API keys as warnings for a partial installation")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	_ = godotenv.Load(filepath.Join(filepath.Dir(*path), ".env"))
	cfg, err := provider.LoadProvidersConfig(*path)
	if err != nil {
		return err
	}
	report := provider.ValidateProvidersConfig(cfg, provider.ValidationOptions{CheckEnvironment: true, AllowMissingKeys: *allowMissing, ExternalProviders: externalConfigProviders()})
	for _, d := range report.Diagnostics {
		fmt.Fprintf(out, "%s %s: %s\n", d.Level, d.Path, d.Message)
	}
	if err := report.Err(); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%d providers / %d models / %d chains，全部通过\n", report.Providers, report.Models, report.Chains)
	return err
}

func runConfigCommand(args []string) error {
	if len(args) == 0 || args[0] != "check" {
		return fmt.Errorf("usage: easygateway config check [--file PATH] [--allow-missing-keys]")
	}
	return checkConfigCommand(args[1:], os.Stdout)
}
