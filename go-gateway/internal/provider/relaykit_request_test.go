package provider

import (
	"encoding/json"
	"testing"
)

func TestRelayKitRequestCompatibleText(t *testing.T) {
	req := &Request{Model: "model", MaxTokens: 128, Messages: []Message{{Role: "user", Content: NewStringContent("hello")}}}
	if err := req.SetRawField("vendor_extension", map[string]any{"mode": "precise"}); err != nil {
		t.Fatal(err)
	}
	legacy := toOpenAIRequestLegacy(req)
	got, ok := relayKitRequest(req, legacy)
	if !ok {
		t.Fatal("simple text request did not use RelayKit")
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["vendor_extension"]) != `{"mode":"precise"}` {
		t.Fatalf("extension lost: %s", b)
	}
}
