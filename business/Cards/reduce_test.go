package business

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// Reduce keeps the subset a card draws, within its limits, and says what it
// left out; it never fails.

func reduce(t *testing.T, raw string) (Card, []string) {
	t.Helper()
	return Reduce(json.RawMessage(raw))
}

func TestACardKeepsWhatItDrawsInTheShapeTheWebDraws(t *testing.T) {
	card, dropped := reduce(t, `{"blocks": [
		{"type": "header", "text": {"type": "plain_text", "text": "Deploy 214"}},
		{"type": "section", "text": {"type": "mrkdwn", "text": "*web* to production"}, "fields": [{"type": "mrkdwn", "text": "*By* Sam"}]},
		{"type": "divider"},
		{"type": "image", "image_url": "https://cdn.example.com/graph.png", "alt_text": "Errors, flat"},
		{"type": "context", "elements": [{"type": "plain_text", "text": "2 minutes ago"}]},
		{"type": "actions", "elements": [
			{"type": "button", "text": {"type": "plain_text", "text": "Roll back"}, "action_id": "rollback", "value": "214", "style": "danger"},
			{"type": "static_select", "action_id": "env", "placeholder": {"type": "plain_text", "text": "Where"}, "options": [{"text": {"type": "plain_text", "text": "Staging"}, "value": "staging"}]},
			{"type": "button", "text": {"type": "plain_text", "text": "Open"}, "url": "https://ci.example.com/214"}
		]}
	]}`)
	if len(dropped) != 0 {
		t.Fatalf("dropped %v", dropped)
	}
	if len(card.Blocks) != 6 || card.Version != CardVersion {
		t.Fatalf("%d blocks, version %d", len(card.Blocks), card.Version)
	}
	acts := card.Actions()
	if len(acts) != 3 || acts[0].Style != "danger" || acts[1].Type != ElementSelect || acts[1].Options[0].Text != "Staging" || acts[2].URL == "" {
		t.Fatalf("actions %+v", acts)
	}
	// The web draws the same JSON adapter/Command's blocks always had.
	out, _ := json.Marshal(card.Blocks[1])
	if !strings.Contains(string(out), `"text":{"type":"mrkdwn","text":"*web* to production"}`) {
		t.Fatalf("a section reads %s", out)
	}
}

func TestACardLeavesOutWhatItCantDrawAndSaysSo(t *testing.T) {
	card, dropped := reduce(t, `[
		{"type": "input", "label": "Name"},
		{"type": "image", "image_url": "http://cdn.example.com/a.png"},
		{"type": "image", "image_url": "https://10.0.0.5/a.png"},
		{"type": "section", "text": {"type": "plain_text", "text": "Hi"}, "accessory": {"type": "button"}},
		{"type": "actions", "elements": [{"type": "datepicker", "action_id": "d"}, {"type": "button", "text": "Go"}]}
	]`)
	if len(card.Blocks) != 1 || card.Blocks[0].Type != BlockSection {
		t.Fatalf("kept %+v", card.Blocks)
	}
	want := []string{"a input block", "an image: not https", "an image: a private address", "a section's accessory", "a datepicker element", "a button with no action_id", "an actions block with nothing to press"}
	got := strings.Join(dropped, " | ")
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("dropped %q, not saying %q", got, w)
		}
	}
}

func TestACardHoldsItsLimits(t *testing.T) {
	var blocks []string
	for i := 0; i < 60; i++ {
		blocks = append(blocks, `{"type": "divider"}`)
	}
	card, dropped := reduce(t, "["+strings.Join(blocks, ",")+"]")
	if len(card.Blocks) != MaxBlocks || !strings.Contains(strings.Join(dropped, " "), "10 blocks over the 50") {
		t.Fatalf("%d blocks, dropped %v", len(card.Blocks), dropped)
	}

	var buttons []string
	for i := 0; i < 30; i++ {
		buttons = append(buttons, fmt.Sprintf(`{"type": "button", "text": "B%d", "action_id": "b%d"}`, i, i))
	}
	card, _ = reduce(t, `[{"type": "actions", "elements": [`+strings.Join(buttons, ",")+`]}]`)
	if n := len(card.Actions()); n != MaxActions {
		t.Fatalf("%d actions kept", n)
	}

	long := strings.Repeat("x", MaxText+50)
	card, dropped = reduce(t, `[{"type": "section", "text": {"type": "plain_text", "text": "`+long+`"}}]`)
	if n := len([]rune(card.Blocks[0].Text.Text)); n != MaxText || !strings.HasSuffix(card.Blocks[0].Text.Text, "…") || len(dropped) != 1 {
		t.Fatalf("a long text is %d characters, dropped %v", n, dropped)
	}
}

func TestACardFromNothingOrNonsenseIsEmpty(t *testing.T) {
	for _, raw := range []string{``, `null`, `{"blocks": []}`} {
		if card, dropped := reduce(t, raw); len(card.Blocks) != 0 || len(dropped) != 0 {
			t.Errorf("%q: %+v %v", raw, card, dropped)
		}
	}
	if card, dropped := reduce(t, `{"blocks": "no"}`); len(card.Blocks) != 0 || len(dropped) != 1 {
		t.Errorf("nonsense: %+v %v", card, dropped)
	}
}
