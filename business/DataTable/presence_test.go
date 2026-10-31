package business

import (
	"testing"
	"time"
)

// Presence on a table (tables A7): who is there, and when others hear of it.
func TestWhoIsOnATable(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	at := func(ago time.Duration) int64 { return now.Add(-ago).UnixMilli() }
	live, gone := livePresence([]Here{
		{User: "b", At: at(10 * time.Second)},
		{User: "a", At: at(44 * time.Second)},
		{User: "c", At: at(46 * time.Second)},
	}, now)
	if len(live) != 2 || live[0].User != "a" || live[1].User != "b" {
		t.Errorf("those heard from in 45 s, in order: %+v", live)
	}
	if len(gone) != 1 || gone[0] != "c" {
		t.Errorf("someone silent for 46 s has gone: %v", gone)
	}

	people := withPresence([]Here{{User: "b"}, {User: "a"}}, Here{User: "c", Row: "r"}, false)
	if len(people) != 3 || people[2].User != "c" || people[2].Row != "r" {
		t.Errorf("someone coming is put in: %+v", people)
	}
	if left := withPresence(people, Here{User: "a"}, true); len(left) != 2 || left[0].User != "b" {
		t.Errorf("someone leaving is taken out: %+v", left)
	}
}

func TestOthersHearOfAMoveNotAHeartbeat(t *testing.T) {
	was := &Here{User: "a", Row: "r1", Field: "f1"}
	cases := []struct {
		name string
		was  *Here
		me   Here
		gone bool
		want bool
	}{
		{"a heartbeat at the same cell", was, Here{User: "a", Row: "r1", Field: "f1"}, false, false},
		{"another cell", was, Here{User: "a", Row: "r2", Field: "f1"}, false, true},
		{"starting to edit", was, Here{User: "a", Row: "r1", Field: "f1", Editing: true}, false, true},
		{"coming", nil, Here{User: "a"}, false, true},
		{"going", was, Here{User: "a"}, true, true},
		{"going, never there", nil, Here{User: "a"}, true, false},
	}
	for _, c := range cases {
		if got := presenceMoved(c.was, c.me, c.gone); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
	if idOrNothing("<script>") != "" || idOrNothing("6f1d2c3b-4a59-4e6f-8a7b-9c0d1e2f3a4b") == "" {
		t.Error("a cell is named by ids only")
	}
}
