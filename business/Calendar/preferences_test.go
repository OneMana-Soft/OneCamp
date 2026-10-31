package Calendar

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	preferencesBusiness "github.com/akashc777/OneCamp/business/Preferences"
)

// The zones a person's day and week show beside their own (calendar head 6,
// gcal-3): up to two the server knows, each once, each with a short label
// or none. Anything else is refused in words for the person.
func TestOtherZonesAreKeptOnlyWhenTheyRead(t *testing.T) {
	if !slices.Contains(preferencesBusiness.Keys(), PrefZones) {
		t.Fatalf("%s isn't a preference", PrefZones)
	}
	for _, v := range []string{
		`[]`,
		`[{"zone":"Europe/London"}]`,
		`[{"zone":"Europe/London","label":"London office"},{"zone":"Asia/Tokyo","label":"東京"}]`,
	} {
		if err := zonesProblem(json.RawMessage(v)); err != nil {
			t.Errorf("%s was refused: %v", v, err)
		}
	}
	for v, why := range map[string]string{
		`{"zone":"Europe/London"}`: "a list of zones",
		`[{"zone":"Europe/London"},{"zone":"Asia/Tokyo"},{"zone":"America/New_York"}]`: "up to 2",
		`[{"label":"London"}]`:                          "Pick a time zone",
		`[{"zone":"Mars/Olympus_Mons"}]`:                "isn't a time zone this server knows",
		`[{"zone":"Local"}]`:                            "isn't a time zone this server knows",
		`[{"zone":"Asia/Tokyo"},{"zone":"Asia/Tokyo"}]`: "once",
		`[{"zone":"Asia/Tokyo","label":"` + strings.Repeat("é", maxZoneLabel+1) + `"}]`: "24 characters",
	} {
		var refused *preferencesBusiness.Refused
		if err := zonesProblem(json.RawMessage(v)); !errors.As(err, &refused) || !strings.Contains(refused.Msg, why) {
			t.Errorf("%s: %v, want a refusal that says %q", v, err, why)
		}
	}
}
