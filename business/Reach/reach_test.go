package business

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

const (
	maya  = "00000000-0000-4000-8000-0000000000a1"
	jonas = "00000000-0000-4000-8000-0000000000a2"
	sam   = "00000000-0000-4000-8000-0000000000a3"
	ada   = "00000000-0000-4000-8000-0000000000a4"
	scout = "00000000-0000-4000-8000-0000000000b1" // an agent
	zed   = "00000000-0000-4000-8000-0000000000a9" // not in the channel
)

// world is a channel's members, the graph's people and the workspace's teams.
type world struct {
	members   []string
	here      map[string]bool
	teams     map[string][]string
	membersOK bool
	limit     int
}

func stub(t *testing.T, w *world) {
	t.Helper()
	oldMembers, oldPeople, oldTeams, oldMay, oldLimit := memberIDs, readPeople, readTeams, membersMay, adminsOnlyAt
	t.Cleanup(func() {
		memberIDs, readPeople, readTeams, membersMay, adminsOnlyAt = oldMembers, oldPeople, oldTeams, oldMay, oldLimit
	})
	memberIDs = func(context.Context, string) ([]string, error) { return w.members, nil }
	readPeople = func(_ context.Context, ids []string) ([]*dgraphStruct.DgraphUser, error) {
		var out []*dgraphStruct.DgraphUser
		for _, id := range ids {
			u := &dgraphStruct.DgraphUser{Uid: "0x" + id[len(id)-2:], Uuid: id, IsBot: id == scout}
			if w.here[id] {
				one := 1
				u.DevicesConnected = &one
			}
			out = append(out, u)
		}
		return out, nil
	}
	readTeams = func(_ context.Context, ids []string) ([]*dgraphStruct.DgraphTeam, error) {
		var out []*dgraphStruct.DgraphTeam
		for _, id := range ids {
			if m, ok := w.teams[id]; ok {
				t := &dgraphStruct.DgraphTeam{Uuid: id}
				for _, u := range m {
					t.Members = append(t.Members, &dgraphStruct.DgraphUser{Uuid: u})
				}
				out = append(out, t)
			}
		}
		return out, nil
	}
	membersMay = func(context.Context, string) bool { return w.membersOK }
	adminsOnlyAt = func(context.Context) int { return w.limit }
}

func uuidsOf(us []*dgraphStruct.DgraphUser) []string {
	var out []string
	for _, u := range us {
		out = append(out, u.Uuid)
	}
	slices.Sort(out)
	return out
}

func chips(ids ...string) string {
	var b strings.Builder
	b.WriteString("<p>")
	for _, id := range ids {
		b.WriteString(`<span data-type="mention" data-id="` + id + `">@x</span> `)
	}
	b.WriteString("look</p>")
	return b.String()
}

// The chips the composer writes for a room and a team, each once; a person's
// chip, and anything else, isn't one of them.
func TestParseReadsBroadcastsAndTeams(t *testing.T) {
	got := Parse(chips("broadcast:channel", "broadcast:here", "group:Team-1", "broadcast:channel", maya+"@0x1", "broadcast:galaxy", "group:team-1"))
	if !slices.Equal(got.Broadcasts, []Broadcast{Channel, Here}) || !slices.Equal(got.Groups, []string{"team-1"}) {
		t.Errorf("parsed %+v", got)
	}
	if !Parse("<p>@channel in words is just words</p>").Empty() {
		t.Error("plain text asked for a broadcast")
	}
}

// Who may notify a whole channel: its managers and the workspace's admins
// always; members unless the workspace or the channel says not, or the
// channel is larger than the limit. A token acts as a member.
func TestWhoMayNotifyAWholeChannel(t *testing.T) {
	w := &world{membersOK: true}
	stub(t, w)
	ctx := context.Background()
	member := &userModels.UserInfo{}
	admin := &userModels.UserInfo{UserPostgresInfo: userModels.User{IsAdmin: true}}
	ch := &dgraphStruct.DgraphChannel{Uuid: "c-1", Name: "launch", MemberCount: 40}
	everyone := Asked{Broadcasts: []Broadcast{Channel}}

	if r := Check(ctx, member, ch, everyone); r != nil {
		t.Errorf("a member, by default: %v", r)
	}
	if r := Check(ctx, member, ch, Asked{Groups: []string{"t"}}); r != nil {
		t.Errorf("a team's mention has no rule: %v", r)
	}
	w.membersOK = false
	if r := Check(ctx, member, ch, everyone); r == nil || r.Msg != "Only #launch's managers can notify everyone in it." {
		t.Errorf("members may not: %v", r)
	}
	if r := Check(ctx, member, &dgraphStruct.DgraphChannel{Uuid: "c-1", Name: "launch", IsAdmin: 1}, everyone); r != nil {
		t.Errorf("a manager: %v", r)
	}
	if r := Check(ctx, admin, ch, everyone); r != nil {
		t.Errorf("an admin in the app: %v", r)
	}
	viaToken := helpers.WithActsThroughToken(ctx)
	if r := Check(viaToken, admin, ch, everyone); r == nil {
		t.Error("an admin's token notified a channel members may not")
	}
	if r := Check(viaToken, member, &dgraphStruct.DgraphChannel{Uuid: "c-1", Name: "launch", IsAdmin: 1}, everyone); r == nil {
		t.Error("a manager's token notified a channel members may not")
	}
	w.membersOK, w.limit = true, 25
	if r := Check(ctx, member, ch, Asked{Broadcasts: []Broadcast{Here}}); r == nil || r.Msg != "#launch has more than 25 members, so only its managers can notify all of it." {
		t.Errorf("past the limit: %v", r)
	}
	if r := Check(ctx, member, &dgraphStruct.DgraphChannel{Uuid: "c-2", MemberCount: 10}, everyone); r != nil {
		t.Errorf("under the limit: %v", r)
	}
}

// Who a broadcast or a team's mention reaches: never the writer, never a bot,
// never anyone outside the channel.
func TestWhoAMessageReaches(t *testing.T) {
	w := &world{
		members:   []string{maya, jonas, sam, ada, scout},
		here:      map[string]bool{jonas: true, scout: true},
		teams:     map[string][]string{"design": {sam, zed, maya}, "eng": {jonas, ada, maya}, "ops": {ada}},
		membersOK: true,
	}
	stub(t, w)
	ctx := context.Background()
	as := func(id string) *userModels.UserInfo {
		return &userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uid: "0x" + id[len(id)-2:], Uuid: id}}
	}
	got, err := InChannel(ctx, "c-1", Asked{Broadcasts: []Broadcast{Channel}}, as(maya))
	if err != nil || !slices.Equal(uuidsOf(got), []string{jonas, sam, ada}) {
		t.Errorf("@channel by Maya reached %v (%v): everyone but her and the agent", uuidsOf(got), err)
	}
	if got, _ := InChannel(ctx, "c-1", Asked{Broadcasts: []Broadcast{Everyone}}, as(maya)); !slices.Equal(uuidsOf(got), []string{jonas, sam, ada}) {
		t.Errorf("@everyone reached %v", uuidsOf(got))
	}
	if got, _ := InChannel(ctx, "c-1", Asked{Broadcasts: []Broadcast{Here}}, as(maya)); !slices.Equal(uuidsOf(got), []string{jonas}) {
		t.Errorf("@here reached %v: Jonas alone is here (the agent is a bot)", uuidsOf(got))
	}
	if got, _ := InChannel(ctx, "c-1", Asked{Groups: []string{"design"}}, as(maya)); !slices.Equal(uuidsOf(got), []string{sam}) {
		t.Errorf("@design reached %v: Sam, never Zed, who isn't in the channel", uuidsOf(got))
	}
	if got, _ := InChannel(ctx, "c-1", Asked{Broadcasts: []Broadcast{Here}, Groups: []string{"eng"}}, as(maya)); !slices.Equal(uuidsOf(got), []string{jonas, ada}) {
		t.Errorf("@here and @eng reached %v: Jonas, here, and Ada, of the team", uuidsOf(got))
	}
	// In a thread a broadcast reaches nobody; a team's mention reaches as ever.
	if got, _ := InThread(ctx, "c-1", Asked{Broadcasts: []Broadcast{Channel}, Groups: []string{"eng"}}, as(jonas)); !slices.Equal(uuidsOf(got), []string{maya, ada}) {
		t.Errorf("in a thread: %v", uuidsOf(got))
	}
	// A DM's or group's people: a team's mention among them, never a broadcast.
	if got, _ := AmongPeople(ctx, []string{maya, jonas, zed}, Asked{Broadcasts: []Broadcast{Channel}, Groups: []string{"design", "eng"}}, as(maya)); !slices.Equal(uuidsOf(got), []string{jonas, zed}) {
		t.Errorf("among a group's people: %v", uuidsOf(got))
	}
	// A team the writer isn't in reaches nobody; a workspace admin's does.
	if got, _ := InChannel(ctx, "c-1", Asked{Groups: []string{"ops"}}, as(maya)); got != nil {
		t.Errorf("@ops by Maya, not in it, reached %v", uuidsOf(got))
	}
	admin := as(maya)
	admin.UserPostgresInfo.IsAdmin = true
	if got, _ := InChannel(ctx, "c-1", Asked{Groups: []string{"ops"}}, admin); !slices.Equal(uuidsOf(got), []string{ada}) {
		t.Errorf("@ops by an admin reached %v", uuidsOf(got))
	}
	if got, _ := InChannel(ctx, "c-1", Asked{}, as(maya)); got != nil {
		t.Errorf("nothing asked reached %v", uuidsOf(got))
	}
}

// Who a message names and who it reaches are told once each.
func TestMergeTellsEachPersonOnce(t *testing.T) {
	a, b, c := &dgraphStruct.DgraphUser{Uid: "0x1"}, &dgraphStruct.DgraphUser{Uid: "0x2"}, &dgraphStruct.DgraphUser{Uid: "0x1"}
	got := Merge([]*dgraphStruct.DgraphUser{a}, []*dgraphStruct.DgraphUser{b, c, nil})
	if len(got) != 2 || got[0] != a || got[1] != b {
		t.Errorf("merged %v", got)
	}
	if got := Merge([]*dgraphStruct.DgraphUser{a}, nil); len(got) != 1 {
		t.Errorf("merged nothing into %v", got)
	}
}
