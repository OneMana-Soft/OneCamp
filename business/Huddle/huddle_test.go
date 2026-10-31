package business

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

const (
	maya    = "11111111-1111-4111-8111-111111111111"
	jonas   = "22222222-2222-4222-8222-222222222222"
	session = "33333333-3333-4333-8333-333333333333"
	channel = "44444444-4444-4444-8444-444444444444"
)

var started = time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)

type written struct {
	where, client string
	post          *postAdapter.InputCreateOrUpdatePostInfo
	chat          *chatAdapter.ChatInfo
}

// fake answers the session as given, Maya as the starter, and keeps what is
// written and attached. Each write answers the post id the client id names,
// as the durable path answers a repeat with the first post.
func fake(t *testing.T, s *callStateBusiness.Session) (*[]written, *[]uuid.UUID) {
	t.Helper()
	old := seams
	t.Cleanup(func() { seams = old })
	var writes []written
	var attached []uuid.UUID
	ids := map[string]string{}
	idFor := func(client string) string {
		if ids[client] == "" {
			ids[client] = uuid.NewString()
		}
		return ids[client]
	}
	seams.session = func(context.Context, string) (*callStateBusiness.Session, error) { return s, nil }
	seams.actor = func(_ context.Context, id uuid.UUID) (*userModels.UserInfo, error) {
		u := &userModels.UserInfo{}
		u.UserDgraphInfo = dgraphStruct.DgraphUser{Uuid: id.String(), UserName: "Maya Chen", Uid: "0x1"}
		return u, nil
	}
	seams.writeChannel = func(_ context.Context, _ *userModels.UserInfo, in *postAdapter.InputCreateOrUpdatePostInfo, client string) (string, error) {
		writes = append(writes, written{where: "channel", client: client, post: in})
		return idFor(client), nil
	}
	seams.writeDM = func(_ context.Context, _ *userModels.UserInfo, in *chatAdapter.ChatInfo, client string) (string, error) {
		writes = append(writes, written{where: "dm", client: client, chat: in})
		return idFor(client), nil
	}
	seams.writeGroup = func(_ context.Context, _ *userModels.UserInfo, in *chatAdapter.ChatInfo, client string) (string, error) {
		writes = append(writes, written{where: "group", client: client, chat: in})
		return idFor(client), nil
	}
	seams.attach = func(_ context.Context, _ *callStateBusiness.Session, post uuid.UUID) (bool, error) {
		won := s.PostUUID == ""
		if won {
			s.PostUUID = post.String()
		}
		attached = append(attached, post)
		return won, nil
	}
	published = nil
	seams.publish = func(_ context.Context, room string) { published = append(published, room) }
	return &writes, &attached
}

// published is the rooms told their call changed, as fake records them.
var published []string

func callIn(kind, id string) *callStateBusiness.Session {
	return &callStateBusiness.Session{ID: session, Kind: "huddle", Room: id, Resource: callStateBusiness.Ref{Kind: kind, ID: id}, Live: true, StartedAt: started}
}

func startedEvent() resourceEventBusiness.Event {
	return resourceEventBusiness.Event{
		Kind: resourceEventBusiness.CallStarted, Actor: resourceEventBusiness.Actor{UserUUID: maya},
		Data: map[string]string{"session": session, "kind": "huddle"},
	}
}

// A channel's call writes one post into the channel as whoever started it:
// a huddle, carrying the call's facts, written once by its client id, and
// the call takes it.
func TestAChannelsCallWritesItsPostAndTakesIt(t *testing.T) {
	s := callIn("channel", channel)
	writes, attached := fake(t, s)
	run(context.Background(), startedEvent())
	if len(*writes) != 1 || (*writes)[0].where != "channel" {
		t.Fatalf("writes: %+v", *writes)
	}
	w := (*writes)[0]
	if w.client != "call:"+session || w.post.ChannelUuid != channel || w.post.ChannelUUID.String() != channel || w.post.Kind != helpers.KindHuddle {
		t.Errorf("the post: %+v (client %q)", w.post, w.client)
	}
	var facts Facts
	if err := json.Unmarshal([]byte(w.post.System), &facts); err != nil || facts.Session != session || !facts.Started.Equal(started) || facts.Ended != nil {
		t.Errorf("the facts: %q, %v", w.post.System, err)
	}
	if w.post.HTMLText != "<p>Maya Chen started a call</p>" {
		t.Errorf("the words: %q", w.post.HTMLText)
	}
	if len(*attached) != 1 || s.PostUUID != (*attached)[0].String() {
		t.Errorf("attached %v, the call's post %q", *attached, s.PostUUID)
	}
	// Its people's apps learn the call has its post (and so its chat) now.
	if len(published) != 1 || published[0] != channel {
		t.Errorf("published %v", published)
	}

	// Again (a retry, or a second instance): the call has its post already.
	run(context.Background(), startedEvent())
	if len(*writes) != 1 {
		t.Errorf("a second post was written: %+v", *writes)
	}
}

// Two instances that raced both write and attach the same post: the
// durable write answers the client id's first post.
func TestARacedStartAttachesOnePost(t *testing.T) {
	s := callIn("channel", channel)
	writes, attached := fake(t, s)
	post(context.Background(), startedEvent(), s)
	other := *s
	other.PostUUID = ""
	post(context.Background(), startedEvent(), &other)
	if len(*writes) != 2 || len(*attached) != 2 || (*attached)[0] != (*attached)[1] {
		t.Errorf("writes %d, attached %v", len(*writes), *attached)
	}
	if len(published) != 1 {
		t.Errorf("the call was published %d times, want once, by the instance that attached", len(published))
	}
}

// A DM's call writes to the other person; a group's, into the group.
func TestADMsAndAGroupsCallWriteTheirPost(t *testing.T) {
	writes, _ := fake(t, callIn("dm", maya+" "+jonas))
	run(context.Background(), startedEvent())
	if len(*writes) != 1 || (*writes)[0].where != "dm" || (*writes)[0].chat.ToUuid != jonas || (*writes)[0].chat.Kind != helpers.KindHuddle {
		t.Errorf("a DM: %+v", *writes)
	}
	writes, _ = fake(t, callIn("group", "0123456789abcdef0123456789abcdef"))
	run(context.Background(), startedEvent())
	if len(*writes) != 1 || (*writes)[0].where != "group" || (*writes)[0].chat.GrpUuid != "0123456789abcdef0123456789abcdef" {
		t.Errorf("a group: %+v", *writes)
	}
}

// A meeting has no conversation, so no post; a write that fails leaves the
// call without one, and nothing is attached.
func TestNoPostForAMeetingOrAFailedWrite(t *testing.T) {
	writes, attached := fake(t, callIn("meeting", "m1"))
	run(context.Background(), startedEvent())
	if len(*writes) != 0 || len(*attached) != 0 {
		t.Errorf("a meeting: %+v %v", *writes, *attached)
	}
	s := callIn("channel", channel)
	_, attached = fake(t, s)
	seams.writeChannel = func(context.Context, *userModels.UserInfo, *postAdapter.InputCreateOrUpdatePostInfo, string) (string, error) {
		return "", errors.New("an announcement channel: only admins post")
	}
	run(context.Background(), startedEvent())
	if len(*attached) != 0 || s.PostUUID != "" {
		t.Errorf("a failed write attached %v", *attached)
	}
	if wants(resourceEventBusiness.Event{Kind: resourceEventBusiness.CallStarted, Data: map[string]string{"kind": "meeting"}}) {
		t.Error("a meeting's start is wanted")
	}
}

// When the call ends, its post's facts gain the end and the length, in a
// write of the facts alone: the card is never edited.
func TestTheCallsEndGoesOnItsPost(t *testing.T) {
	ended := started.Add(23*time.Minute + 5*time.Second)
	for _, c := range []struct{ kind, id, store string }{{"channel", channel, "post"}, {"dm", maya + " " + jonas, "chat"}, {"group", "0123456789abcdef0123456789abcdef", "chat"}} {
		s := callIn(c.kind, c.id)
		s.Live, s.EndedAt, s.PostUUID = false, &ended, uuid.NewString()
		fake(t, s)
		var store, id, system string
		seams.setPostFacts = func(_ context.Context, post, facts string) error { store, id, system = "post", post, facts; return nil }
		seams.setChatFacts = func(_ context.Context, chat, facts string) error { store, id, system = "chat", chat, facts; return nil }
		run(context.Background(), resourceEventBusiness.Event{Kind: resourceEventBusiness.CallEnded, Data: map[string]string{"session": session, "kind": "huddle"}})
		var facts Facts
		if err := json.Unmarshal([]byte(system), &facts); err != nil || store != c.store || id != s.PostUUID || facts.Ended == nil || !facts.Ended.Equal(ended) || facts.Seconds != 23*60+5 || facts.Session != session {
			t.Errorf("%s: %s %s %q, %v", c.kind, store, id, system, err)
		}
	}
	// A call that never got its post: nothing to write.
	s := callIn("channel", channel)
	s.Live, s.EndedAt = false, &ended
	fake(t, s)
	wrote := false
	seams.setPostFacts = func(context.Context, string, string) error { wrote = true; return nil }
	run(context.Background(), resourceEventBusiness.Event{Kind: resourceEventBusiness.CallEnded, Data: map[string]string{"session": session, "kind": "huddle"}})
	if wrote {
		t.Error("facts written for a call with no post")
	}
}

// repairable answers the session as the sweep would, live, without a post,
// not refused, and keeps what the sweep noted (CHAT-LEAD-2's review: the sink
// runs once, so a write that failed is the sweep's).
func repairable(t *testing.T, s *callStateBusiness.Session) *[]string {
	t.Helper()
	var noted []string
	refused := false
	seams.postless = func(context.Context, time.Time, int, int) ([]string, error) {
		if s.PostUUID != "" || refused {
			return nil, nil
		}
		return []string{s.ID}, nil
	}
	seams.noteTry = func(_ context.Context, _ string, r bool) error {
		if r {
			refused = true
			noted = append(noted, "refused")
		} else {
			noted = append(noted, "try")
		}
		return nil
	}
	return &noted
}

// A call whose post wasn't written (a hiccup, or a restart between the start
// and the sink) gets it from the sweep, as its starter, once.
func TestTheSweepWritesACallsMissingPost(t *testing.T) {
	ctx := context.Background()
	s := callIn("channel", channel)
	s.StartedBy = maya
	writes, attached := fake(t, s)
	noted := repairable(t, s)
	write := seams.writeChannel
	seams.writeChannel = func(context.Context, *userModels.UserInfo, *postAdapter.InputCreateOrUpdatePostInfo, string) (string, error) {
		return "", errors.New("the graph is away")
	}
	if err := repair(ctx); err != nil {
		t.Fatal(err)
	}
	if len(*attached) != 0 || len(*noted) != 1 || (*noted)[0] != "try" {
		t.Fatalf("a hiccup: attached %v, noted %v", *attached, *noted)
	}
	seams.writeChannel = write
	if err := repair(ctx); err != nil {
		t.Fatal(err)
	}
	if len(*writes) != 1 || (*writes)[0].client != "call:"+session || len(*attached) != 1 || s.PostUUID == "" {
		t.Fatalf("the repair: writes %+v, attached %v", *writes, *attached)
	}
	if err := repair(ctx); err != nil || len(*writes) != 1 {
		t.Errorf("a third pass wrote again: %+v, %v", *writes, err)
	}
}

// A starter who may not post there is refused for good: the sweep tries once,
// records the refusal, and never asks again.
func TestTheSweepRecordsARefusalOnce(t *testing.T) {
	ctx := context.Background()
	s := callIn("channel", channel)
	s.StartedBy = maya
	fake(t, s)
	noted := repairable(t, s)
	tried := 0
	seams.writeChannel = func(context.Context, *userModels.UserInfo, *postAdapter.InputCreateOrUpdatePostInfo, string) (string, error) {
		tried++
		return "", &sendBusiness.Rejection{Status: 403, Msg: "Only admins post in this channel"}
	}
	for i := 0; i < 3; i++ {
		if err := repair(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if tried != 1 || len(*noted) != 1 || (*noted)[0] != "refused" {
		t.Errorf("tried %d, noted %v", tried, *noted)
	}
}
