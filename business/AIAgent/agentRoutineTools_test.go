package business

import (
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

func TestParseHHMM(t *testing.T) {
	ok := map[string][2]int{"09:00": {9, 0}, "9:5": {9, 5}, "23:59": {23, 59}, " 7:30 ": {7, 30}}
	for in, want := range ok {
		h, m, valid := parseHHMM(in)
		if !valid || h != want[0] || m != want[1] {
			t.Fatalf("parseHHMM(%q) = (%d,%d,%v), want (%d,%d,true)", in, h, m, valid, want[0], want[1])
		}
	}
	for _, bad := range []string{"", "9", "24:00", "10:60", "-1:00", "ab:cd", "9:00:00"} {
		if _, _, valid := parseHHMM(bad); valid {
			t.Fatalf("parseHHMM(%q) should be invalid", bad)
		}
	}
}

func TestParseRoutineFireMinute(t *testing.T) {
	// Explicit at_minute_utc wins.
	if got := parseRoutineFireMinute(map[string]string{"at_minute_utc": "615"}); got != 615 {
		t.Fatalf("at_minute_utc: got %d", got)
	}
	// Local time + offset is converted (09:00 at -420 → 16:00 UTC = 960).
	if got := parseRoutineFireMinute(map[string]string{"time": "09:00", "tz_offset_minutes": "-420"}); got != 960 {
		t.Fatalf("time+offset: got %d, want 960", got)
	}
	// Time with no offset is treated as UTC.
	if got := parseRoutineFireMinute(map[string]string{"time": "06:30"}); got != 390 {
		t.Fatalf("time no offset: got %d, want 390", got)
	}
	// Nothing usable → 09:00 UTC default.
	if got := parseRoutineFireMinute(map[string]string{}); got != defaultRoutineMinuteUTC {
		t.Fatalf("default: got %d, want %d", got, defaultRoutineMinuteUTC)
	}
	// Junk falls back to default.
	if got := parseRoutineFireMinute(map[string]string{"at_minute_utc": "xyz", "time": "bad"}); got != defaultRoutineMinuteUTC {
		t.Fatalf("junk: got %d, want %d", got, defaultRoutineMinuteUTC)
	}
}

func TestDescribeCadence(t *testing.T) {
	if got := describeCadence("FREQ=DAILY", 540); got != "daily at 09:00 UTC" {
		t.Fatalf("daily: %q", got)
	}
	if got := describeCadence("FREQ=WEEKLY;BYDAY=MO,WE,FR", 870); got != "weekly on MO,WE,FR at 14:30 UTC" {
		t.Fatalf("weekly byday: %q", got)
	}
	if got := describeCadence("FREQ=WEEKLY", 0); got != "weekly at 00:00 UTC" {
		t.Fatalf("weekly: %q", got)
	}
	if got := describeCadence("FREQ=HOURLY", 0); got != "every hour" {
		t.Fatalf("hourly: %q", got)
	}
	if got := describeCadence("FREQ=HOURLY;INTERVAL=6", 0); got != "every 6 hours" {
		t.Fatalf("hourly interval: %q", got)
	}
}

func TestRoutineInScope(t *testing.T) {
	chID := uuid.New()
	chRoutine := &model.AgentRoutine{ChannelId: &chID}
	grpRoutine := &model.AgentRoutine{GroupId: "grp-1"}

	// Same channel matches; different channel / group does not.
	if !routineInScope(chRoutine, agentRunScope{ChannelID: chID.String()}) {
		t.Fatalf("same channel should be in scope")
	}
	if routineInScope(chRoutine, agentRunScope{ChannelID: uuid.New().String()}) {
		t.Fatalf("different channel should be out of scope")
	}
	if routineInScope(chRoutine, agentRunScope{GroupID: "grp-1"}) {
		t.Fatalf("channel routine should not match a group scope")
	}
	// Same group matches.
	if !routineInScope(grpRoutine, agentRunScope{GroupID: "grp-1"}) {
		t.Fatalf("same group should be in scope")
	}
	if routineInScope(grpRoutine, agentRunScope{GroupID: "grp-2"}) {
		t.Fatalf("different group should be out of scope")
	}
	// No scope → never in scope.
	if routineInScope(chRoutine, agentRunScope{}) {
		t.Fatalf("empty scope should never match")
	}
}
