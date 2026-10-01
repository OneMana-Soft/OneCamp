package business

import (
	"strings"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
)

func TestFloorSince(t *testing.T) {
	now := time.Now()
	floor := now.Add(-catchUpLookbackFloor)

	// No last-seen row → floor (never summarize beyond the lookback).
	if got := floorSince(time.Time{}, false, floor); !got.Equal(floor) {
		t.Errorf("missing last-seen should collapse to floor")
	}
	// Ancient last-seen → floor.
	ancient := now.Add(-90 * 24 * time.Hour)
	if got := floorSince(ancient, true, floor); !got.Equal(floor) {
		t.Errorf("ancient last-seen should collapse to floor")
	}
	// Recent last-seen (within the window) → itself.
	recent := now.Add(-1 * time.Hour)
	if got := floorSince(recent, true, floor); !got.Equal(recent) {
		t.Errorf("recent last-seen should be preserved, got %v", got)
	}
}

func TestChannelNameFor(t *testing.T) {
	user := &userModels.UserInfo{
		UserDgraphInfo: dgraphStruct.DgraphUser{
			Channels: []*dgraphStruct.DgraphChannel{
				{Uuid: "ch-1", Name: "engineering"},
				{Uuid: "ch-2", Name: "design"},
			},
		},
	}
	if got := channelNameFor(user, "ch-2"); got != "design" {
		t.Errorf("expected 'design', got %q", got)
	}
	if got := channelNameFor(user, "ch-missing"); got != "" {
		t.Errorf("unknown channel should yield empty name, got %q", got)
	}
}

func TestFormatCatchUpWindow(t *testing.T) {
	items := []ai.ScopedContent{
		{AuthorName: "Alice", ContentText: "Shipping Friday"},
		{AuthorName: "", ContentText: "agreed"}, // empty author → participant
		{AuthorName: "Bob", ContentText: "   "}, // blank → skipped
		{AuthorName: "Carol", ContentText: "what about QA?"},
	}
	out := formatContentWindow(items)

	if !strings.Contains(out, "Alice: Shipping Friday") {
		t.Errorf("missing Alice line:\n%s", out)
	}
	if !strings.Contains(out, "participant: agreed") {
		t.Errorf("empty author should render as participant:\n%s", out)
	}
	if strings.Contains(out, "Bob:") {
		t.Errorf("blank content should be skipped:\n%s", out)
	}
	// Chronological order preserved (Alice before Carol).
	if strings.Index(out, "Alice") > strings.Index(out, "Carol") {
		t.Errorf("order should be preserved chronologically:\n%s", out)
	}
}

// TestScopesAllowedCountsTheWholeMembership pins the number the recap reports as
// its boundary. The workspace recap sweeps three membership lists, and reporting
// only the channels would understate what was open to it.
func TestScopesAllowedCountsTheWholeMembership(t *testing.T) {
	channels := []string{"ch-1", "ch-2"}
	projects := []string{"pr-1"}
	groups := []string{"g-1", "g-2", "g-3"}

	if got := scopesAllowed(catchUpScopeWorkspace, channels, projects, groups); got != 6 {
		t.Errorf("workspace recap should report every scope it may touch, got %d want 6", got)
	}
}

// TestScopesAllowedIsOneForASingleScope covers the case the counting version gets
// wrong. A channel recap may read exactly the channel it was asked about — the
// reader's other memberships are not open to it — so the membership lists must
// not leak into the number a per-channel recap reports.
func TestScopesAllowedIsOneForASingleScope(t *testing.T) {
	channels := []string{"ch-1", "ch-2", "ch-3"}
	groups := []string{"g-1"}

	for _, scope := range []string{catchUpScopeChannel, catchUpScopeChat} {
		if got := scopesAllowed(scope, channels, nil, groups); got != 1 {
			t.Errorf("%s recap reads one scope, got %d", scope, got)
		}
	}
}

// TestScopesAllowedReportsNoneForAMemberOfNothing guards the empty workspace: a
// member who belongs to nothing must be told the recap read nothing, not handed
// a floor of one that would imply something was.
func TestScopesAllowedReportsNoneForAMemberOfNothing(t *testing.T) {
	if got := scopesAllowed(catchUpScopeWorkspace, nil, nil, nil); got != 0 {
		t.Errorf("a member of nothing has no scopes open to the recap, got %d", got)
	}
}
