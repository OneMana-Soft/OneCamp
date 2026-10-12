package business

import (
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
)

// The feed wrote each item's time, and so the next page's beforeTime cursor,
// in whole seconds. The next page asks for items before the cursor, so every
// older item within the cursor's second was skipped.
func TestActivityTimesKeepTheirFractionsAndSortAsTimes(t *testing.T) {
	base := time.Date(2026, 10, 10, 6, 49, 13, 0, time.UTC)
	at := func(ns int) *time.Time { v := base.Add(time.Duration(ns)); return &v }
	mentions := []*dgraphStruct.DgraphMentions{
		{CreatedAt: at(135206157)},
		{CreatedAt: at(580166492)},
		{CreatedAt: at(0)}, // exactly on the second: "…13Z"
	}
	comments := []*dgraphStruct.DgraphComment{{CreatedAt: at(500000000)}} // "…13.5Z"
	reactions := []*dgraphModels.ReactionsActivity{{DgraphReaction: dgraphStruct.DgraphReaction{AddedAt: at(269044907)}}}

	got := mergeActivities(mentions, comments, reactions)
	want := []string{
		"2026-10-10T06:49:13.580166492Z",
		"2026-10-10T06:49:13.5Z",
		"2026-10-10T06:49:13.269044907Z",
		"2026-10-10T06:49:13.135206157Z",
		"2026-10-10T06:49:13Z",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d items, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Time != w {
			t.Errorf("item %d: time %q, want %q (newest first, to the nanosecond)", i, got[i].Time, w)
		}
		if got[i].Priority == "" || got[i].ActorKind == "" {
			t.Errorf("item %d is not scored: %+v", i, got[i])
		}
	}

	// A page's last time is the next cursor: it reads back exactly, and so
	// does a cursor an older client sends in whole seconds.
	for _, cursor := range []string{got[3].Time, "2026-10-10T06:49:13Z"} {
		parsed, err := time.Parse(activityTimeLayout, cursor)
		if err != nil {
			t.Fatalf("cursor %q: %v", cursor, err)
		}
		if parsed.Format(activityTimeLayout) != cursor {
			t.Errorf("cursor %q came back as %q", cursor, parsed.Format(activityTimeLayout))
		}
	}
	if activityTime(nil) != "" {
		t.Error("an item with no time has no time")
	}
}
