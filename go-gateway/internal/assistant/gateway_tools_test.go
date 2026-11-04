package assistant

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"
)

func TestChatRejectsUnknownModel(t *testing.T) {
	a, err := New(Config{
		Model: "m-default", Models: []string{"m-alt"},
		BaseURL: "http://127.0.0.1:1", APIKey: "k",
	}, newTestMemStore(t), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Chat(context.Background(), "hi", "m-nope", nil)
	if err == nil || !strings.Contains(err.Error(), "m-nope") || !strings.Contains(err.Error(), "m-alt") {
		t.Fatal("expected unknown-model error listing available models, got:", err)
	}
}
