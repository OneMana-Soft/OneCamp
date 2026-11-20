package business

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	domain "github.com/akashc777/OneCamp/domain/Activity"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
)

// stubPageSettings answers a page's one read (pageSettings) with s, or err,
// and records each read by what it asked.
func stubPageSettings(t *testing.T, s domain.PageSettings, err error) *[]string {
	t.Helper()
	was := pageSettings
	t.Cleanup(func() { pageSettings = was })
	var reads []string
	pageSettings = func(_ context.Context, _ string, keys, channelIDs, grps []string) (domain.PageSettings, error) {
		reads = append(reads, fmt.Sprintf("keys=%v channels=%v chats=%v", keys, channelIDs, grps))
		return s, err
	}
	return &reads
}

// A page holding a told mention and a muted channel's item asks both of ONE
// read, and each filter takes its own answer: the mention leaves as told, the
// channel's item as muted. A page with neither reads nothing, and a failed
// read keeps the page whole (NOTIF M1 store budget: the feed's routes keep
// their store calls).
func TestAPageOfTheInboxIsReadOnceForBothFilters(t *testing.T) {
	const me = "11111111-1111-4111-8111-111111111111"
	told := dgraphModels.UnifiedActivityItem{ActivityType: "MENTION", Mention: &dgraphStruct.DgraphMentions{Post: &dgraphStruct.DgraphPost{Uuid: "p1"}}}
	page := []dgraphModels.UnifiedActivityItem{told, inChannel("launch", 1), inChannel("general", 2), reactionAt(3)}
	reads := stubPageSettings(t, domain.PageSettings{
		Held:      map[string]bool{"mention|post|p1|" + me: true},
		ByChannel: map[string]string{"launch": "block"},
	}, nil)
	if got, want := scopes(toldAndHeard(context.Background(), me, page)), []string{"general", "REACTION"}; !slices.Equal(got, want) {
		t.Errorf("kept %v, want %v", got, want)
	}
	if want := []string{"keys=[mention|post|p1|" + me + "] channels=[launch general] chats=[]"}; !slices.Equal(*reads, want) {
		t.Errorf("read %v, want one read of the page's mention and places", *reads)
	}
	*reads = nil
	if got := toldAndHeard(context.Background(), me, []dgraphModels.UnifiedActivityItem{reactionAt(1), noticeAt("n1", 2)}); len(got) != 2 || len(*reads) != 0 {
		t.Errorf("a page with no mention and no place kept %d of 2 after reads %v, want both and no read", len(got), *reads)
	}
	stubPageSettings(t, domain.PageSettings{}, errors.New("db down"))
	if got := toldAndHeard(context.Background(), me, page); len(got) != len(page) {
		t.Errorf("a read that failed kept %d of %d, want the page whole", len(got), len(page))
	}
}

// One side of the read empty and the other not, both ways: a page of held
// mentions and no place leaves only its told mentions out, and a page of places
// and no mention only its muted places; neither filter takes the other's answer.
func TestEachFilterOfTheInboxTakesOnlyItsOwnAnswer(t *testing.T) {
	const me = "11111111-1111-4111-8111-111111111111"
	mention := func(p string) dgraphModels.UnifiedActivityItem {
		return dgraphModels.UnifiedActivityItem{ActivityType: "MENTION", Mention: &dgraphStruct.DgraphMentions{Post: &dgraphStruct.DgraphPost{Uuid: p}}}
	}
	// Held keys and no places: a setting in the answer for a place the page
	// hasn't is no reason to drop anything.
	stubPageSettings(t, domain.PageSettings{Held: map[string]bool{"mention|post|p1|" + me: true}, ByChannel: map[string]string{"": "block"}}, nil)
	if got := toldAndHeard(context.Background(), me, []dgraphModels.UnifiedActivityItem{mention("p1"), mention("p2"), reactionAt(1)}); len(got) != 2 || got[0].Mention.Post.Uuid != "p2" {
		t.Errorf("held keys, no places: kept %d items, want p2 and the reaction", len(got))
	}
	// Places and no held keys: a held key that reads like the channel's id
	// leaves nothing out of the channel.
	stubPageSettings(t, domain.PageSettings{Held: map[string]bool{"launch": true}, ByChannel: map[string]string{"design": "block"}}, nil)
	if got := scopes(toldAndHeard(context.Background(), me, []dgraphModels.UnifiedActivityItem{inChannel("launch", 1), inChannel("design", 2)})); !slices.Equal(got, []string{"launch"}) {
		t.Errorf("places, no held keys: kept %v, want launch alone (design is muted)", got)
	}
}
