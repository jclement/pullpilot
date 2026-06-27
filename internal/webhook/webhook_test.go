package webhook

import (
	"encoding/json"
	"testing"
)

// TestRegistrationDecodesWorkerResponse guards the Go client against drifting
// from the relay's JSON keys. The Worker returns camelCase
// (webhookId/pokeUrl/listenUrl); a mismatch silently yields an empty listen URL
// and the daemon never connects ("unexpected url scheme").
func TestRegistrationDecodesWorkerResponse(t *testing.T) {
	body := `{"webhookId":"wh_abc","pokeUrl":"https://relay/v1/poke/wh_abc","listenUrl":"wss://relay/v1/listen/wh_abc","ttlDays":180}`
	var r registration
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatal(err)
	}
	if r.WebhookID != "wh_abc" {
		t.Errorf("WebhookID = %q, want wh_abc", r.WebhookID)
	}
	if r.ListenURL != "wss://relay/v1/listen/wh_abc" {
		t.Errorf("ListenURL = %q (empty means the client would dial an invalid URL)", r.ListenURL)
	}
	if r.PokeURL == "" {
		t.Error("PokeURL is empty")
	}
}
