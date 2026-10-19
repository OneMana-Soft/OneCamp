package business

import (
	"context"
	"fmt"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	conversationDomain "github.com/akashc777/OneCamp/domain/Conversation"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	models "github.com/akashc777/OneCamp/models/postgres/Conversation"
	"github.com/google/uuid"
)

// The graph's reader and the index's decide every channel alike, for every
// way someone can stand in one; the one difference is the old manager's edge
// without membership (access.go's header), which only the graph reads.
func TestTheTwoReadersDecideEveryChannelAlike(t *testing.T) {
	for _, member := range []bool{false, true} {
		for _, manager := range []bool{false, true} {
			for _, private := range []bool{false, true} {
				for _, announcement := range []bool{false, true} {
					for _, archived := range []bool{false, true} {
						ch := dgraphStruct.DgraphChannel{Uuid: "c", IsPrivate: ptr(private)}
						if member {
							ch.IsMember = 1
						}
						if manager {
							ch.IsAdmin = 1
						}
						if announcement {
							ch.PostPolicy = "admins_only"
						}
						if archived {
							ch.DeletedAt = gone
						}
						policy := "everyone"
						if announcement {
							policy = "admins_only"
						}
						fact := models.ChannelFact{Ref: "c", Private: private, PostPolicy: policy, Archived: archived, Member: member, Manager: member && manager}
						graph := channelDecision(&ch)
						index := decideChannel(indexChannelStanding(fact))
						name := fmt.Sprintf("member %v manager %v private %v announcement %v archived %v", member, manager, private, announcement, archived)
						if manager && !member && private {
							if graph.Level != LevelRead || index.Level != accessBusiness.LevelNone {
								t.Errorf("%s: the old edge: graph %s, index %s", name, graph.Level, index.Level)
							}
							continue
						}
						if graph.Level != index.Level || graph.Was != index.Was || graph.Archived != index.Archived {
							t.Errorf("%s: graph %+v, index %+v", name, graph, index)
						}
						if call := decideChannelCall(indexChannelStanding(fact)); call.Level != channelCallDecision(&ch).Level {
							t.Errorf("%s: a call: graph %s, index %s", name, channelCallDecision(&ch).Level, call.Level)
						}
					}
				}
			}
		}
	}
}

// A DM decides alike whoever the other person is.
func TestTheTwoReadersDecideEveryDMAlike(t *testing.T) {
	for _, c := range []struct {
		name   string
		graph  *dgraphStruct.DgraphUser
		person models.Person
		found  bool
	}{
		{"a person", &dgraphStruct.DgraphUser{Uid: "0x2", Uuid: "b"}, models.Person{Active: true}, true},
		{"someone deactivated", &dgraphStruct.DgraphUser{Uid: "0x2", Uuid: "b", DeletedAt: gone}, models.Person{}, true},
		{"an external identity", &dgraphStruct.DgraphUser{Uid: "0x2", Uuid: "b", IsExternal: true}, models.Person{Active: true, External: true}, true},
		{"the automation bot", &dgraphStruct.DgraphUser{Uid: "0x2", Uuid: "b", IsExternal: true, IsBot: true}, models.Person{Active: true, External: true, Bot: true}, true},
		{"nobody", nil, models.Person{}, false},
	} {
		if g, i := dmDecision(c.graph), decideDM(canReceive(c.person, c.found)); g.Level != i.Level || g.Was != i.Was {
			t.Errorf("%s: graph %+v, index %+v", c.name, g, i)
		}
	}
}

// indexMode makes every decision read the index, with its reads counted.
type indexMode struct {
	channelReads, groupReads, peopleReads int
	channels                              map[string]models.ChannelFact
	groups                                map[string]bool
	people                                map[uuid.UUID]models.Person
}

func newIndexMode(t *testing.T) *indexMode {
	t.Helper()
	m := &indexMode{channels: map[string]models.ChannelFact{}, groups: map[string]bool{}, people: map[uuid.UUID]models.Person{}}
	swapVar(t, &accessSource, func(context.Context) conversationDomain.Choice { return conversationDomain.Choice{Index: true} })
	swapVar(t, &indexChannels, func(_ context.Context, _ uuid.UUID, refs []string) (map[string]models.ChannelFact, error) {
		m.channelReads++
		out := map[string]models.ChannelFact{}
		for _, r := range refs {
			if f, ok := m.channels[r]; ok {
				out[r] = f
			}
		}
		return out, nil
	})
	swapVar(t, &indexGroups, func(_ context.Context, _ uuid.UUID, refs []string) (map[string]bool, error) {
		m.groupReads++
		out := map[string]bool{}
		for _, r := range refs {
			if in, ok := m.groups[r]; ok {
				out[r] = in
			}
		}
		return out, nil
	})
	swapVar(t, &indexPeople, func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]models.Person, error) {
		m.peopleReads++
		out := map[uuid.UUID]models.Person{}
		for _, id := range ids {
			if p, ok := m.people[id]; ok {
				out[id] = p
			}
		}
		return out, nil
	})
	return m
}

// From the index, a whole list of channels or groups is one read, whatever its
// length.
func TestTheIndexDecidesAListInOneRead(t *testing.T) {
	m := newIndexMode(t)
	me := uuid.New()
	m.people[me] = models.Person{Active: true}
	a, b, c := uuid.NewString(), uuid.NewString(), uuid.NewString()
	m.channels[a] = models.ChannelFact{Ref: a, Member: true, PostPolicy: "everyone"}
	m.channels[b] = models.ChannelFact{Ref: b, Private: true, PostPolicy: "everyone"}
	who := accessBusiness.Subject{UserUUID: me.String()}

	got, err := channelRules{}.DecideMany(context.Background(), who, []string{a, b, c})
	if err != nil {
		t.Fatal(err)
	}
	if m.channelReads != 1 {
		t.Fatalf("%d reads of the index for three channels", m.channelReads)
	}
	if got[a].Level != LevelPost || got[b].Level != accessBusiness.LevelNone {
		t.Fatalf("decisions: %+v", got)
	}
	if _, ok := got[c]; ok {
		t.Fatalf("a channel the index hasn't got was decided: %+v", got[c])
	}

	g1, g2 := "0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210"
	m.groups[g1], m.groups[g2] = true, false
	gs, err := groupRules{}.DecideMany(context.Background(), who, []string{g1, g2})
	if err != nil || m.groupReads != 1 || gs[g1].Level != LevelPost || gs[g2].Level != accessBusiness.LevelNone {
		t.Fatalf("groups: %+v (%d reads, err %v)", gs, m.groupReads, err)
	}
}

// From the index, someone deactivated decides nothing anywhere.
func TestFromTheIndexSomeoneDeactivatedDecidesNothing(t *testing.T) {
	m := newIndexMode(t)
	me, them := uuid.New(), uuid.New()
	m.people[me] = models.Person{Active: false}
	m.people[them] = models.Person{Active: true}
	ch := uuid.NewString()
	m.channels[ch] = models.ChannelFact{Ref: ch, Member: true, Manager: true, PostPolicy: "everyone"}
	who := accessBusiness.Subject{UserUUID: me.String()}
	ctx := context.Background()
	if got, err := (channelRules{}).DecideMany(ctx, who, []string{ch}); err != nil || len(got) != 0 {
		t.Fatalf("a channel: %+v %v", got, err)
	}
	dm := me.String() + " " + them.String()
	if got, err := (dmRules{}).DecideMany(ctx, who, []string{dm}); err != nil || len(got) != 0 {
		t.Fatalf("a DM: %+v %v", got, err)
	}
}

// From the index, a DM is its two people's, and posts while the other can
// receive a message.
func TestFromTheIndexADMIsItsTwoPeoples(t *testing.T) {
	m := newIndexMode(t)
	me, them, gone, stranger := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	m.people[me] = models.Person{Active: true}
	m.people[them] = models.Person{Active: true}
	m.people[gone] = models.Person{Active: false}
	who := accessBusiness.Subject{UserUUID: me.String()}
	live, left, notMine := me.String()+" "+them.String(), gone.String()+" "+me.String(), them.String()+" "+stranger.String()
	got, err := (dmRules{}).DecideMany(context.Background(), who, []string{live, left, notMine})
	if err != nil {
		t.Fatal(err)
	}
	if got[live].Level != LevelPost || got[left].Level != LevelRead || got[notMine].Level != accessBusiness.LevelNone {
		t.Fatalf("decisions: %+v", got)
	}
	if m.peopleReads != 2 {
		t.Fatalf("the people were read %d times (one for me, one for the others)", m.peopleReads)
	}
}
