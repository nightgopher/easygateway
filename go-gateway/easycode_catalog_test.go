package main

import (
	"context"
	"errors"
	"io"
	"log"
	"testing"

	"github.com/nightgopher/easygateway/internal/channel"
	"github.com/nightgopher/easygateway/internal/provider"
)

type catalogTestAccount struct {
	accounts *channel.Accounts
	models   []channel.EasyCodeModel
	err      error
}

func (a *catalogTestAccount) Accounts() *channel.Accounts { return a.accounts }
func (a *catalogTestAccount) Models(context.Context) ([]channel.EasyCodeModel, error) {
	return a.models, a.err
}

func TestEasyCodeCatalogAccountChanges(t *testing.T) {
	store := channel.NewStore(t.TempDir())
	if err := store.Save("easycode", []channel.Credential{{AccountID: "alice", AccessToken: "test"}}); err != nil {
		t.Fatal(err)
	}
	account := &catalogTestAccount{accounts: channel.NewAccounts("easycode", store, nil, nil), models: []channel.EasyCodeModel{{ID: "vision", Name: "Vision", ContextWindow: 100000, SupportsImages: true}, {ID: "text", Name: "Text", ContextWindow: 16000}}}
	logger := log.New(io.Discard, "", 0)
	router, native := provider.NewRouter(logger), provider.NewRouter(logger)
	reloader := provider.NewConfigReloader(router, "", nil, nil)
	catalog := newEasyCodeCatalog(account, router, native, reloader, "", logger)
	if err := catalog.channelChanged(context.Background(), "easycode"); err != nil {
		t.Fatal(err)
	}
	if router.ModelChannel("easycode/vision") != "easycode" || router.ModelChannel("easycode/text") != "easycode" {
		t.Fatal("account models not registered")
	}
	models := router.ListModelInfos()
	if len(models) != 2 || models[0].ID != "easycode/text" || models[0].MaxInputTokens+models[0].MaxOutputTokens > 16000 || len(models[0].InputModalities) != 1 {
		t.Fatalf("incorrect model metadata: %+v", models)
	}
	if len(models[1].InputModalities) != 2 {
		t.Fatal("vision capability missing")
	}
	account.err = errors.New("network error")
	if err := catalog.sync(context.Background(), false); err == nil {
		t.Fatal("expected catalog error")
	}
	if _, err := router.Route("easycode/text"); err != nil {
		t.Fatal("transient refresh removed last known catalog")
	}
	if err := account.accounts.RemoveAll(); err != nil {
		t.Fatal(err)
	}
	if err := catalog.channelChanged(context.Background(), "easycode"); err != nil {
		t.Fatal(err)
	}
	if _, err := router.Route("easycode/text"); err == nil {
		t.Fatal("logout retained account models")
	}
}
