package business

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// A channel is marked while a guest's link opens it: a live link, and the
// channel itself live. Archived and deleted channels' links open nothing.
func TestMarkGuestAccessMarksLiveChannelsWithLiveLinks(t *testing.T) {
	archivedAt := time.Now().Add(-time.Hour)
	dgraphZero := time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC) // how the graph keeps "not deleted"
	shared := &dgraphStruct.DgraphChannel{Uuid: "shared"}
	sharedZero := &dgraphStruct.DgraphChannel{Uuid: "shared-zero", DeletedAt: &dgraphZero}
	private := &dgraphStruct.DgraphChannel{Uuid: "private"}
	archived := &dgraphStruct.DgraphChannel{Uuid: "archived", DeletedAt: &archivedAt}
	noID := &dgraphStruct.DgraphChannel{}

	markGuestAccess([]*dgraphStruct.DgraphChannel{shared, nil, sharedZero, private, archived, noID},
		map[string]bool{"shared": true, "shared-zero": true, "archived": true, "": true})

	for name, c := range map[string]struct {
		ch   *dgraphStruct.DgraphChannel
		want bool
	}{
		"a channel with a live link":              {shared, true},
		"one the graph keeps as never deleted":    {sharedZero, true},
		"a channel with no live link":             {private, false},
		"an archived channel with a link left on": {archived, false},
		"a channel with no id":                    {noID, false},
	} {
		if c.ch.HasGuestAccess != c.want {
			t.Errorf("%s: has_guest_access %v, want %v", name, c.ch.HasGuestAccess, c.want)
		}
	}
}

// With guest access off no link opens anything: nothing is marked, and the
// links aren't even looked up (no database here, so a lookup would panic).
func TestMarkGuestAccessMarksNothingWithGuestAccessOff(t *testing.T) {
	t.Setenv("GUEST_ACCESS_ENABLED", "false")
	chs := []*dgraphStruct.DgraphChannel{{Uuid: "shared"}, {Uuid: "other"}}
	MarkGuestAccess(context.Background(), chs...)
	for _, ch := range chs {
		if ch.HasGuestAccess {
			t.Errorf("%s marked with guest access off", ch.Uuid)
		}
	}
	MarkGuestAccess(context.Background()) // no channels: nothing to do
}

// The marker is only ever read, never stored: unmarked, the field is not in
// the channel's JSON at all, so no write of a channel puts it in the graph.
func TestHasGuestAccessIsNeverWrittenUnset(t *testing.T) {
	raw, err := json.Marshal(&dgraphStruct.DgraphChannel{Uuid: "c1", Name: "launch"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "has_guest_access") {
		t.Errorf("an unmarked channel writes the marker: %s", raw)
	}
	raw, _ = json.Marshal(&dgraphStruct.DgraphChannel{Uuid: "c1", HasGuestAccess: true})
	if !strings.Contains(string(raw), `"has_guest_access":true`) {
		t.Errorf("a marked channel doesn't say so: %s", raw)
	}
}
