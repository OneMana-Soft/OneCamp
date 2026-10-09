package business

import (
	"strings"
	"testing"

	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
)

// A failed import's progress event reaches every admin's open page; Trello's
// network errors carry the key and token in their URL, and the event doesn't.
func TestAnImportsProgressCarriesNoURLQuery(t *testing.T) {
	failed := `Get "https://api.trello.com/1/members/me/boards?key=0123abcd&token=ATTA9876": dial tcp: lookup api.trello.com: no such host`
	p := &mqttStruct.MqttSlackImportProgress{JobId: "j1", Status: "failed", ErrorMessage: failed}
	payload, err := importProgressPayload(p)
	if err != nil {
		t.Fatal(err)
	}
	sent := string(payload)
	if strings.Contains(sent, "key=") || strings.Contains(sent, "ATTA9876") || !strings.Contains(sent, "api.trello.com/1/members/me/boards") {
		t.Errorf("broadcast: %s", sent)
	}
	if p.ErrorMessage != failed {
		t.Error("the caller's event was changed")
	}
}
