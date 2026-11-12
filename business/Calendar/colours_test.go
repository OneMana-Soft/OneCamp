package Calendar

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	preferencesBusiness "github.com/akashc777/OneCamp/business/Preferences"
	"github.com/google/uuid"
)

// An event's colour (gcal-1a head B) is one of the six camp hues, a colour
// of its own as "#rrggbb" in lower case, or none (its calendar's). Anything
// else is refused in words.
func TestAnEventTakesACampHueOrAColourOfItsOwn(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam := h.member("Sam")
	start, end := timed(h.clock, 30, "UTC")
	e, err := h.s.Create(ctx, sam, EventInput{Title: "Launch", Start: start, End: end, Hue: "#1a73e8"})
	if err != nil || e.Hue != "#1a73e8" {
		t.Fatalf("made in #1a73e8: %+v %v", e, err)
	}
	if got, err := h.s.Get(ctx, sam, e.ID); err != nil || got.Hue != "#1a73e8" {
		t.Errorf("read back: %q %v", got.Hue, err)
	}
	for _, ok := range []string{"sky", "#000000", "#ffffff", ""} {
		cur, _ := h.s.Get(ctx, sam, e.ID)
		if got, err := h.s.Update(ctx, sam, e.ID, EventPatch{Sequence: seq(cur.Sequence), Hue: str(ok)}); err != nil || got.Hue != ok {
			t.Errorf("%q: %+v %v", ok, got, err)
		}
	}
	var input *EventInputError
	for _, bad := range []string{"#1A73E8", "#1a73e", "#1a73e80", "1a73e8", "#gggggg", "red", "#1a73e8 "} {
		cur, _ := h.s.Get(ctx, sam, e.ID)
		if _, err := h.s.Update(ctx, sam, e.ID, EventPatch{Sequence: seq(cur.Sequence), Hue: str(bad)}); !errors.As(err, &input) ||
			!strings.Contains(err.Error(), "#") {
			t.Errorf("%q: %v, want a refusal that shows a colour like #1a73e8", bad, err)
		}
	}
}

// Your colours: a person's own colours, newest first, each once, at most
// twelve, the oldest dropped; each "#rrggbb" in lower case.
func TestYourColoursAreTheLatestTwelve(t *testing.T) {
	if !slices.Contains(preferencesBusiness.Keys(), PrefRecentColours) {
		t.Fatalf("%s isn't a preference", PrefRecentColours)
	}
	for _, ok := range []string{`[]`, `["#1a73e8"]`, `["#1a73e8","#000000","#ffffff"]`} {
		if err := recentColoursProblem(json.RawMessage(ok)); err != nil {
			t.Errorf("%s was refused: %v", ok, err)
		}
	}
	for _, bad := range []string{`"#1a73e8"`, `[1]`, `["#1A73E8"]`, `["sky"]`, `["#1a73e"]`, `{"c":"#1a73e8"}`} {
		var refused *preferencesBusiness.Refused
		if err := recentColoursProblem(json.RawMessage(bad)); !errors.As(err, &refused) || !strings.Contains(refused.Msg, "#") {
			t.Errorf("%s: %v, want a refusal in words", bad, err)
		}
	}
	in := []string{"#00000d", "#00000c", "#00000d", "#00000b", "#00000a", "#000009", "#000008", "#000007", "#000006", "#000005", "#000004", "#000003", "#000002", "#000001"}
	raw, _ := json.Marshal(in)
	kept, err := keptRecentColours(context.Background(), uuid.New(), raw)
	var got []string
	if err != nil || json.Unmarshal(kept, &got) != nil {
		t.Fatalf("%s %v", kept, err)
	}
	want := []string{"#00000d", "#00000c", "#00000b", "#00000a", "#000009", "#000008", "#000007", "#000006", "#000005", "#000004", "#000003", "#000002"}
	if !slices.Equal(got, want) {
		t.Errorf("kept %v, want %v", got, want)
	}
}
