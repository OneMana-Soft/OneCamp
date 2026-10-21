package business

import (
	"context"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	conversationDomain "github.com/akashc777/OneCamp/domain/Conversation"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// TestChannelDecisionsFromTheGraphAreOneRead: deciding many channels from
// the graph reads them in one query (a Connected panel naming fifteen
// channels made fifteen round trips), and decides each by the one rule:
// a member posts, anyone reads a public channel, nobody else sees a private
// one (decided none, as before), and an archived channel is read only.
func TestChannelDecisionsFromTheGraphAreOneRead(t *testing.T) {
	const (
		mine     = "c1000000-0000-4000-8000-000000000001"
		public   = "c1000000-0000-4000-8000-000000000002"
		private  = "c1000000-0000-4000-8000-000000000003"
		archived = "c1000000-0000-4000-8000-000000000004"
	)
	savedSource, savedRead := accessSource, readChannelStandings
	t.Cleanup(func() { accessSource, readChannelStandings = savedSource, savedRead })
	accessSource = func(context.Context) conversationDomain.Choice { return conversationDomain.Choice{Index: false} }
	reads := 0
	pub, priv := false, true
	past := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	readChannelStandings = func(_ context.Context, ids []string, uid string) ([]*dgraphStruct.DgraphChannel, error) {
		reads++
		if uid != "0x1" {
			t.Errorf("read as %q", uid)
		}
		return []*dgraphStruct.DgraphChannel{
			{Uuid: mine, IsMember: 1, IsPrivate: &priv},
			{Uuid: public, IsPrivate: &pub},
			{Uuid: private, IsPrivate: &priv},
			{Uuid: archived, IsMember: 1, IsPrivate: &priv, DeletedAt: &past},
		}, nil
	}
	got, err := channelRules{}.DecideMany(context.Background(), accessBusiness.Subject{UserUID: "0x1", UserUUID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}, []string{mine, public, private, archived, "not-a-uuid"})
	if err != nil {
		t.Fatal(err)
	}
	if reads != 1 {
		t.Errorf("%d reads for four channels, want one", reads)
	}
	if got[mine].Level != LevelPost || got[public].Level != LevelRead || got[archived].Level != LevelRead {
		t.Errorf("decisions: %+v", got)
	}
	if got[private].Level != accessBusiness.LevelNone {
		t.Errorf("a private channel one isn't in: %+v", got[private])
	}
	if _, ok := got["not-a-uuid"]; ok {
		t.Error("a non-uuid is decided")
	}
}
