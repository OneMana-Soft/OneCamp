package business

import (
	"encoding/json"
	"testing"
)

// A template's listed form, kept when it is saved so the Templates list never
// reads a payload (audit C11): a board's drawing, a doc's preview, and any
// other kind as it is.
func TestATemplatesListedFormIsWhatTheListShows(t *testing.T) {
	board := boardPayload(t,
		map[string]any{"id": "n1", "type": "rectangle", "x": 0, "y": 0, "width": 200, "height": 120, "backgroundColor": "#FFF4B8", "customData": map[string]any{"kind": "note"}},
		map[string]any{"id": "n1-label", "type": "text", "x": 10, "y": 10, "width": 100, "height": 20, "containerId": "n1", "text": "Ship it"},
	)
	var drawn BoardPayload
	if err := json.Unmarshal([]byte(listedForm(KindBoard, board)), &drawn); err != nil {
		t.Fatal(err)
	}
	if drawn.Title != "Sprint retro" || drawn.Count != 2 || len(drawn.Preview) != 1 || len(drawn.Elements) != 0 {
		t.Errorf("a board is listed as %+v, want its drawing (a title, a count of 2, the note drawn) and no elements", drawn)
	}

	doc := `{"title":"Weekly notes","body":"<p>Agenda for the week</p>"}`
	var listed DocPayload
	if err := json.Unmarshal([]byte(listedForm(KindDoc, doc)), &listed); err != nil {
		t.Fatal(err)
	}
	if listed.Body != "" || listed.Preview == "" || listed.Title != "Weekly notes" {
		t.Errorf("a doc is listed as %+v, want its title and preview and no body", listed)
	}

	if got := listedForm("agent", `{"name":"Release captain"}`); got != `{"name":"Release captain"}` {
		t.Errorf("an agent is listed as %s, want it as it is", got)
	}
	for _, kind := range []string{KindBoard, KindDoc, "agent"} {
		if got := listedForm(kind, ""); got != "{}" {
			t.Errorf("an empty %s payload is listed as %q, want {}", kind, got)
		}
	}
}
