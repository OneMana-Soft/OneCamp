package business

import (
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// userWithChannels builds a user whose Dgraph record grants exactly these
// channels, which is what getAccessibleResourceUUIDs reads.
func userWithChannels(uuids ...string) *userModels.UserInfo {
	chans := make([]*dgraphStruct.DgraphChannel, 0, len(uuids))
	for _, u := range uuids {
		chans = append(chans, &dgraphStruct.DgraphChannel{Uuid: u})
	}
	u := &userModels.UserInfo{}
	u.UserDgraphInfo.Channels = chans
	return u
}

// The import maps channels the importer may not be a member of: a private Slack
// channel becomes a private OneCamp channel, and being the person who ran the
// import is not membership. Summarising one would leak its contents to someone
// who cannot open it, so the read window is the intersection and never the
// import's own list.
func TestDigestNeverReadsBeyondTheImportersOwnAccess(t *testing.T) {
	const mine, theirs = "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"

	got := visibleImportedChannels(userWithChannels(mine), []string{mine, theirs})

	if len(got) != 1 || got[0] != mine {
		t.Fatalf("visibleImportedChannels = %v, want exactly [%s]", got, mine)
	}
}

// A user with access to channels this import did not touch must not have them
// summarised: the digest describes what was imported, not the workspace.
func TestDigestDoesNotDescribeChannelsThisImportDidNotTouch(t *testing.T) {
	const imported, preexisting = "aaaaaaaa-0000-0000-0000-000000000000", "bbbbbbbb-0000-0000-0000-000000000000"

	got := visibleImportedChannels(userWithChannels(imported, preexisting), []string{imported})

	if len(got) != 1 || got[0] != imported {
		t.Fatalf("visibleImportedChannels = %v, want exactly [%s]", got, imported)
	}
}

// An importer with no overlapping access yields no window at all, which
// generateImportDigest turns into "no digest" without spending a model call.
func TestNoOverlapYieldsNothingToSummarise(t *testing.T) {
	got := visibleImportedChannels(userWithChannels("cccccccc-0000-0000-0000-000000000000"), []string{"dddddddd-0000-0000-0000-000000000000"})

	if len(got) != 0 {
		t.Fatalf("visibleImportedChannels = %v, want empty", got)
	}
}

// Order follows the imported list so a regenerated digest reads the same window.
func TestVisibleChannelsFollowImportOrder(t *testing.T) {
	a, b, c := "a0000000-0000-0000-0000-000000000000", "b0000000-0000-0000-0000-000000000000", "c0000000-0000-0000-0000-000000000000"

	// Accessible list is deliberately in a different order from the import's.
	got := visibleImportedChannels(userWithChannels(c, b, a), []string{a, b, c})

	if len(got) != 3 || got[0] != a || got[1] != b || got[2] != c {
		t.Fatalf("visibleImportedChannels = %v, want [%s %s %s]", got, a, b, c)
	}
}
