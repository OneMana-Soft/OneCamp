//go:build integration

package domain_test

import (
	"context"
	"slices"
	"testing"
	"time"

	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	"github.com/akashc777/OneCamp/tests/integration"
)

// The channel reads that started from every channel in the workspace (has(...)
// then a filter on membership) now start from the reader's own channels, or
// from the ids asked for, or from the trigram index on ch_name; what each
// returns is unchanged: membership, archive state, privacy, pages and counts.
func TestChannelReadsStartFromTheReader(t *testing.T) {
	ctx := context.Background()
	dg := integration.SetupDgraph(t)
	live, gone := "0001-01-01T00:00:00Z", time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	ch := func(id, name string, private bool, deleted string) map[string]any {
		return map[string]any{"uid": "_:" + id, "dgraph.type": "Channel", "ch_uuid": id + "-uuid", "ch_name": name, "ch_private": private, "ch_deleted_at": deleted}
	}
	uids := dg.Mutate(t, []map[string]any{
		ch("eng", "engineering", false, live),
		ch("eng2", "eng-oncall", true, live),
		ch("mkt", "marketing", false, live),
		ch("old", "old-engineering", false, gone),
		ch("sec", "secret-engine", true, live),
		{"uid": "_:me", "dgraph.type": "User", "user_uuid": "me-uuid", "user_name": "me",
			"user_channels": []map[string]string{{"uid": "_:eng"}, {"uid": "_:eng2"}, {"uid": "_:old"}}},
		{"uid": "_:other", "dgraph.type": "User", "user_uuid": "other-uuid", "user_name": "other",
			"user_channels": []map[string]string{{"uid": "_:mkt"}, {"uid": "_:sec"}}},
	})
	// The member edge the flag counts.
	dg.Mutate(t, []map[string]any{{"uid": uids["eng"], "ch_members": []map[string]string{{"uid": uids["me"]}}}})

	names := func(list []string) []string { slices.Sort(list); return list }

	active, total, err := channelDomain.GetUserActiveChannelListWithLatestPost(ctx, uids["me"], nil, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range active {
		got = append(got, c.Name)
	}
	if g := names(got); total != 2 || !slices.Equal(g, []string{"eng-oncall", "engineering"}) {
		t.Errorf("active: %v (total %d), want my two live channels", g, total)
	}
	page, total, err := channelDomain.GetUserActiveChannelListWithLatestPost(ctx, uids["me"], nil, 1, 1)
	if err != nil || total != 2 || len(page) != 1 {
		t.Errorf("second page of one: %d channels, total %d, err %v", len(page), total, err)
	}

	archived, total, err := channelDomain.GetUserArchivedChannelListWithLatestPost(ctx, uids["me"], nil, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(archived) != 1 || archived[0].Name != "old-engineering" {
		t.Errorf("archived: %d channels, total %d, want old-engineering alone", len(archived), total)
	}

	for _, c := range []struct {
		term string
		want []string
	}{
		{"engin", []string{"engineering", "old-engineering"}}, // not someone else's private channel
		{"en", []string{"eng-oncall", "engineering", "old-engineering"}},
		{"market", []string{"marketing"}}, // public, though not mine
	} {
		found, err := channelDomain.GetChannelListWithSearchText(ctx, uids["me"], c.term)
		if err != nil {
			t.Fatalf("search %q: %v", c.term, err)
		}
		var got []string
		for _, f := range found {
			got = append(got, f.Name)
		}
		if g := names(got); !slices.Equal(g, c.want) {
			t.Errorf("search %q = %v, want %v", c.term, g, c.want)
		}
	}

	flagged, err := channelDomain.GetChannelListWithMemberFlag(ctx, uids["me"], []string{uids["eng"], uids["mkt"]})
	if err != nil {
		t.Fatal(err)
	}
	member := map[string]uint8{}
	for _, f := range flagged {
		member[f.Name] = f.IsMember
	}
	if len(flagged) != 2 || member["engineering"] != 1 || member["marketing"] != 0 {
		t.Errorf("member flags: %v", member)
	}
	if none, err := channelDomain.GetChannelListWithMemberFlag(ctx, uids["me"], []string{uids["me"]}); err != nil || len(none) != 0 {
		t.Errorf("a uid that isn't a channel reads as no channel: %d, %v", len(none), err)
	}
}
