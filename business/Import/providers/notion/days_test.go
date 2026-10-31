package notion

import (
	"encoding/json"
	"testing"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// A date property with no time is a day, so it arrives as a calendar
// day (written where the team is), never as midnight UTC; one with a time is
// the instant Notion gives.
func TestANotionDateWithNoTimeArrivesAsADay(t *testing.T) {
	prop := func(start string) notionPageProperty {
		var p notionPageProperty
		if err := json.Unmarshal([]byte(`{"type":"date","date":{"start":"`+start+`"}}`), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	at, day := notionDue(prop("2026-10-22"))
	if day != (importProvider.Day{Year: 2026, Month: time.October, Day: 22}) || at != nil {
		t.Errorf("date only: day %v, instant %v; want the day 2026-10-22 and no instant", day, at)
	}
	at, day = notionDue(prop("2026-10-22T09:30:00.000+05:30"))
	if at == nil || !day.IsZero() || at.UTC().Format(time.RFC3339) != "2026-10-22T04:00:00Z" {
		t.Errorf("with a time: instant %v, day %v; want 2026-10-22T04:00:00Z", at, day)
	}
	if at, day = notionDue(notionPageProperty{}); at != nil || !day.IsZero() {
		t.Errorf("no date: instant %v, day %v; want none", at, day)
	}
}
