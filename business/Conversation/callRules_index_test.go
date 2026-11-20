package business

import (
	"context"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	models "github.com/akashc777/OneCamp/models/postgres/Conversation"
	"github.com/google/uuid"
)

// From the index, a list of call rooms is one read of each kind, whatever its
// length (CR-375 F2): a table's call links and GET /calls/linkable ask about
// up to 100 rooms a page. Its channels' rooms are one read of the channels,
// its groups' one read of the groups, and a DM's room none (its two people
// are its room); a room the index hasn't got is left to the call's invites.
func TestFromTheIndexCallRoomsAreOneReadAKind(t *testing.T) {
	m := newIndexMode(t)
	invites := 0
	swapVar(t, &roomsAdmittedTo, func(context.Context, uuid.UUID) (map[string]bool, error) {
		invites++
		return map[string]bool{}, nil
	})
	me, them := uuid.New(), uuid.New()
	m.people[me] = models.Person{Active: true}
	who := accessBusiness.Subject{UserUUID: me.String()}
	led, in, shut, gone := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	m.channels[led] = models.ChannelFact{Ref: led, Member: true, Manager: true, PostPolicy: "everyone"}
	m.channels[in] = models.ChannelFact{Ref: in, Member: true, PostPolicy: "everyone"}
	m.channels[shut] = models.ChannelFact{Ref: shut, Private: true, PostPolicy: "everyone"}
	g1, g2, g3 := "0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210", "00112233445566778899aabbccddeeff"
	m.groups[g1], m.groups[g2] = true, false
	dm := me.String() + " " + them.String()
	others := them.String() + " " + uuid.NewString() // a DM between two other people

	got, err := callRules{}.DecideMany(context.Background(), who, []string{led, g1, in, dm, g2, shut, g3, gone, others})
	if err != nil {
		t.Fatal(err)
	}
	if m.channelReads != 1 || m.groupReads != 1 {
		t.Fatalf("%d reads of the channels and %d of the groups for four channel rooms and three group rooms, want 1 and 1", m.channelReads, m.groupReads)
	}
	if got[led].Level != LevelModerate || got[in].Level != LevelPost || got[shut].Level != accessBusiness.LevelNone {
		t.Fatalf("channel calls: %+v %+v %+v", got[led], got[in], got[shut])
	}
	if got[g1].Level != LevelPost || got[g2].Level != accessBusiness.LevelNone || got[dm].Level != LevelPost {
		t.Fatalf("group and DM calls: %+v %+v %+v", got[g1], got[g2], got[dm])
	}
	if got[others].Level != accessBusiness.LevelNone {
		t.Fatalf("a DM between two other people's call: %+v, want none", got[others])
	}
	for _, room := range []string{g3, gone} {
		if d, ok := got[room]; ok {
			t.Fatalf("a room the index hasn't got was decided: %s %+v", room, d)
		}
	}
	if invites != 1 {
		t.Fatalf("the invites were read %d times, want once for the rooms left", invites)
	}

	// A list of DMs' rooms alone reads neither channels nor groups.
	m.channelReads, m.groupReads = 0, 0
	if got, err := (callRules{}).DecideMany(context.Background(), who, []string{dm}); err != nil || got[dm].Level != LevelPost || m.channelReads+m.groupReads != 0 {
		t.Fatalf("a DM's call: %+v (%v), %d channel reads, %d group reads", got[dm], err, m.channelReads, m.groupReads)
	}
}
