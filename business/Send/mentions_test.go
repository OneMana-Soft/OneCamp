package business

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"

	reachBusiness "github.com/akashc777/OneCamp/business/Reach"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// asked records what each reach was asked, and answers who it reaches.
type asked struct {
	how     []string // "channel", "thread", "among"
	checked []reachBusiness.Asked
	among   [][]string
	reads   []string // channels read again for their size
	convs   []string // conversations read for their people
}

func stubReach(t *testing.T, refuse bool) *asked {
	t.Helper()
	got := &asked{}
	old := []any{namedIn, reachInChannel, reachInThread, reachAmong, checkBroadcast, readChannel, readConversation}
	t.Cleanup(func() {
		namedIn = old[0].(func(context.Context, string) ([]*dgraphStruct.DgraphUser, error))
		reachInChannel = old[1].(func(context.Context, string, reachBusiness.Asked, *userModels.UserInfo) ([]*dgraphStruct.DgraphUser, error))
		reachInThread = old[2].(func(context.Context, string, reachBusiness.Asked, *userModels.UserInfo) ([]*dgraphStruct.DgraphUser, error))
		reachAmong = old[3].(func(context.Context, []string, reachBusiness.Asked, *userModels.UserInfo) ([]*dgraphStruct.DgraphUser, error))
		checkBroadcast = old[4].(func(context.Context, *userModels.UserInfo, *dgraphStruct.DgraphChannel, reachBusiness.Asked) *reachBusiness.Refusal)
		readChannel = old[5].(func(context.Context, uuid.UUID, string) (*dgraphStruct.DgraphChannel, error))
		readConversation = old[6].(func(context.Context, string, string) (*dgraphStruct.DgraphDm, error))
	})
	namedIn = func(context.Context, string) ([]*dgraphStruct.DgraphUser, error) {
		return []*dgraphStruct.DgraphUser{{Uid: "0x2", Uuid: "jonas"}}, nil
	}
	everyone := []*dgraphStruct.DgraphUser{{Uid: "0x2", Uuid: "jonas"}, {Uid: "0x3", Uuid: "sam"}}
	reachInChannel = func(_ context.Context, _ string, a reachBusiness.Asked, _ *userModels.UserInfo) ([]*dgraphStruct.DgraphUser, error) {
		got.how = append(got.how, "channel")
		return everyone, nil
	}
	reachInThread = func(_ context.Context, _ string, a reachBusiness.Asked, _ *userModels.UserInfo) ([]*dgraphStruct.DgraphUser, error) {
		got.how = append(got.how, "thread")
		return nil, nil
	}
	reachAmong = func(_ context.Context, people []string, a reachBusiness.Asked, _ *userModels.UserInfo) ([]*dgraphStruct.DgraphUser, error) {
		got.how, got.among = append(got.how, "among"), append(got.among, people)
		return []*dgraphStruct.DgraphUser{{Uid: "0x3", Uuid: "sam"}}, nil
	}
	checkBroadcast = func(_ context.Context, _ *userModels.UserInfo, ch *dgraphStruct.DgraphChannel, a reachBusiness.Asked) *reachBusiness.Refusal {
		got.checked = append(got.checked, a)
		if refuse {
			return &reachBusiness.Refusal{Msg: "Only #" + ch.Name + "'s managers can notify everyone in it."}
		}
		return nil
	}
	readChannel = func(_ context.Context, id uuid.UUID, _ string) (*dgraphStruct.DgraphChannel, error) {
		got.reads = append(got.reads, id.String())
		return &dgraphStruct.DgraphChannel{Uuid: id.String(), Name: "launch", MemberCount: 40}, nil
	}
	readConversation = func(_ context.Context, _ string, id string) (*dgraphStruct.DgraphDm, error) {
		got.convs = append(got.convs, id)
		return &dgraphStruct.DgraphDm{Participants: []*dgraphStruct.DgraphUser{{Uuid: "maya"}, {Uuid: "sam"}, {Uuid: "jonas"}}}, nil
	}
	return got
}

func uuids(us []*dgraphStruct.DgraphUser) []string {
	var out []string
	for _, u := range us {
		out = append(out, u.Uuid)
	}
	return out
}

const (
	everyoneChip = `<span data-type="mention" data-id="broadcast:channel">@channel</span>`
	hereChip     = `<span data-type="mention" data-id="broadcast:here">@here</span>`
	teamChip     = `<span data-type="mention" data-id="group:design">@design</span>`
	launchID     = "00000000-0000-4000-8000-00000000c001"
)

var maya = &userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uid: "0x1", Uuid: "maya"}}

// A channel post tells the people it names and everyone its broadcast
// reaches, each once; a refused broadcast is a 403 in the rule's words and
// reaches nobody.
func TestAChannelPostsMentionsReachTheRoom(t *testing.T) {
	got := stubReach(t, false)
	launch := &dgraphStruct.DgraphChannel{Uuid: launchID, Name: "launch", MemberCount: 40}
	people, err := Mentions(context.Background(), maya, Where{Channel: launch}, ptr("<p>"+everyoneChip+" ship it</p>"))
	if err != nil || !slices.Equal(uuids(people), []string{"jonas", "sam"}) {
		t.Fatalf("told %v (%v): Jonas once, and Sam", uuids(people), err)
	}
	if len(got.checked) != 1 || len(got.reads) != 0 {
		t.Errorf("checked %v, read the channel again %v: once, from the post's own read", got.checked, got.reads)
	}

	got = stubReach(t, true)
	_, err = Mentions(context.Background(), maya, Where{Channel: launch}, ptr("<p>"+everyoneChip+"</p>"))
	var rj *Rejection
	if !errors.As(err, &rj) || rj.Status != http.StatusForbidden || rj.Msg != "Only #launch's managers can notify everyone in it." {
		t.Fatalf("a refused broadcast: %v", err)
	}
	if slices.Contains(got.how, "channel") {
		t.Error("a refused broadcast was resolved")
	}
	// Plain words ask nothing.
	if people, err := Mentions(context.Background(), maya, Where{Channel: launch}, ptr("<p>@channel in words</p>")); err != nil || !slices.Equal(uuids(people), []string{"jonas"}) {
		t.Errorf("plain words told %v (%v)", uuids(people), err)
	}
}

// An edit is asked only of the broadcasts it adds: one that keeps the
// broadcast it was sent with is never refused for it, and still reaches the
// room, so nobody's Mentions loses it. A post's own read of its channel has
// no size, so the channel is read again before the rule is asked.
func TestAnEditIsAskedOnlyOfWhatItAdds(t *testing.T) {
	got := stubReach(t, true)
	launch := &dgraphStruct.DgraphChannel{Uuid: launchID, Name: "launch"}
	before := "<p>" + everyoneChip + " ship it</p>"
	people, err := Mentions(context.Background(), maya, Where{Channel: launch, Before: before}, ptr("<p>"+everyoneChip+" ship it today</p>"))
	if err != nil || len(people) != 2 {
		t.Fatalf("an edit keeping @channel: %v (%v)", uuids(people), err)
	}
	if len(got.checked) != 0 {
		t.Errorf("asked %v of an edit that adds nothing", got.checked)
	}
	_, err = Mentions(context.Background(), maya, Where{Channel: launch, Before: before}, ptr("<p>"+everyoneChip+hereChip+"</p>"))
	if _, ok := AsRejection(err); !ok || len(got.checked) != 1 || !slices.Equal(got.checked[0].Broadcasts, []reachBusiness.Broadcast{reachBusiness.Here}) {
		t.Fatalf("an edit adding @here: %v, asked %v", err, got.checked)
	}
	if !slices.Equal(got.reads, []string{launchID}) {
		t.Errorf("read the channel again %v", got.reads)
	}
}

// In a thread a broadcast reaches nobody and is never refused; a team's
// mention reaches as it does in the channel.
func TestAThreadReplyIsNeverABroadcast(t *testing.T) {
	got := stubReach(t, true)
	launch := &dgraphStruct.DgraphChannel{Uuid: launchID, Name: "launch", MemberCount: 40}
	people, err := Mentions(context.Background(), maya, Where{Channel: launch, Thread: true}, ptr("<p>"+everyoneChip+teamChip+"</p>"))
	if err != nil || !slices.Equal(uuids(people), []string{"jonas"}) || !slices.Equal(got.how, []string{"thread"}) || len(got.checked) != 0 {
		t.Fatalf("a thread reply told %v (%v), reached %v, asked %v", uuids(people), err, got.how, got.checked)
	}
}

// In a DM or a group a team's mention reaches its people there, read only
// when one is mentioned; a broadcast reaches nobody.
func TestATeamsMentionInAGroup(t *testing.T) {
	got := stubReach(t, false)
	people, err := Mentions(context.Background(), maya, Where{Conversation: "g-1"}, ptr("<p>"+teamChip+"</p>"))
	if err != nil || !slices.Equal(uuids(people), []string{"jonas", "sam"}) || !slices.Equal(got.convs, []string{"g-1"}) || !slices.Equal(got.among[0], []string{"maya", "sam", "jonas"}) {
		t.Fatalf("a group's team mention: %v (%v), read %v, among %v", uuids(people), err, got.convs, got.among)
	}
	got = stubReach(t, false)
	if _, err := Mentions(context.Background(), maya, Where{Conversation: "g-1"}, ptr("<p>"+everyoneChip+"</p>")); err != nil || len(got.convs) != 0 || len(got.how) != 0 {
		t.Errorf("a broadcast in a group read %v and reached %v (%v)", got.convs, got.how, err)
	}
	if _, err := Mentions(context.Background(), maya, Where{People: []string{"maya", "jonas"}}, ptr("<p>"+teamChip+"</p>")); err != nil || len(got.convs) != 0 || !slices.Equal(got.among[0], []string{"maya", "jonas"}) {
		t.Errorf("a DM's team mention: read %v, among %v (%v)", got.convs, got.among, err)
	}
}

func ptr(s string) *string { return &s }
