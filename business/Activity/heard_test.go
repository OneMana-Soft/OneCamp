package business

import (
	"context"
	"errors"
	"slices"
	"testing"

	domain "github.com/akashc777/OneCamp/domain/Activity"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
)

func scopes(items []dgraphModels.UnifiedActivityItem) []string {
	out := make([]string, 0, len(items))
	for i := range items {
		_, id := activityScope(&items[i])
		if id == "" {
			id = items[i].ActivityType
		}
		out = append(out, id)
	}
	return out
}

// Ada set #launch and her DM with Maya to "Nothing", #design to "Mentions"
// and #general to "Everything"; she has no setting for #new. Her Inbox keeps
// what each lets tell her, and the badge counts the same page (feedPage).
func TestTheInboxKeepsWhatTheSettingLetsTellThem(t *testing.T) {
	reads := stubPageSettings(t, domain.PageSettings{
		ByChannel: map[string]string{"launch": "block", "design": "mention", "general": "all"},
		ByChat:    map[string]string{"dm-maya": "block", "dm-ben": "mention"},
	}, nil)
	page := []dgraphModels.UnifiedActivityItem{
		inChannel("launch", 1), inChannel("design", 2), inChannel("general", 3), inChannel("new", 4),
		inChat("dm-maya", 5), inChat("dm-ben", 6), reactionAt(7), noticeAt("n1", 8),
	}
	got := scopes(toldAndHeard(context.Background(), "ada", page))
	if want := []string{"design", "general", "new", "dm-ben", "REACTION", "NOTICE"}; !slices.Equal(got, want) {
		t.Fatalf("kept %v, want %v", got, want)
	}
	if want := []string{"keys=[] channels=[launch design general new] chats=[dm-maya dm-ben]"}; !slices.Equal(*reads, want) {
		t.Fatalf("read %v, want one read of the page's places alone", *reads)
	}
}

func TestTheInboxReadsNoSettingsForAPageWithNoPlace(t *testing.T) {
	reads := stubPageSettings(t, domain.PageSettings{}, nil)
	page := []dgraphModels.UnifiedActivityItem{reactionAt(1), noticeAt("n1", 2)}
	if got := toldAndHeard(context.Background(), "ada", page); len(got) != 2 || len(*reads) != 0 {
		t.Fatalf("kept %d of 2 after %d reads, want both and no read", len(got), len(*reads))
	}
	if got := scopes(toldAndHeard(context.Background(), "ada", []dgraphModels.UnifiedActivityItem{inChat("dm-maya", 1)})); !slices.Equal(*reads, []string{"keys=[] channels=[] chats=[dm-maya]"}) || len(got) != 1 {
		t.Fatalf("a page of one conversation read %v and kept %v, want the conversations alone and the item (no setting keeps it)", *reads, got)
	}
}

func TestTheInboxKeepsItsPageWhenTheSettingsCantBeRead(t *testing.T) {
	stubPageSettings(t, domain.PageSettings{}, errors.New("db down"))
	page := []dgraphModels.UnifiedActivityItem{inChannel("launch", 1), inChat("dm-maya", 2)}
	if got := toldAndHeard(context.Background(), "ada", page); len(got) != 2 {
		t.Fatalf("kept %d of 2, want the page whole", len(got))
	}
}
