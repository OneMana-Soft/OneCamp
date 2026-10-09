package business

import (
	"context"
	"errors"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// A workspace of one member and one admin, where the member is in one channel
// and can read one doc, one board and one table, and nothing else.
func testSources(t *testing.T, member, admin uuid.UUID, memberChannel, readableDoc, readableBoard, viewableTable string) sources {
	t.Helper()
	users := map[uuid.UUID]*userModels.User{
		member: {Id: member},
		admin:  {Id: admin, IsAdmin: true},
	}
	memberTopics := map[string]bool{}
	msg, typing := helpers.GetMqttTopicForChannel(memberChannel)
	memberTopics[member.String()+msg] = true
	memberTopics[member.String()+typing] = true
	return sources{
		activeUser: func(_ context.Context, id uuid.UUID) (*userModels.User, error) {
			return users[id], nil
		},
		memberTopic: func(_ context.Context, userUUID, topic string) (bool, error) {
			return memberTopics[userUUID+topic], nil
		},
		readsDoc: func(_ context.Context, userUUID, docID string) (bool, error) {
			return userUUID == member.String() && docID == readableDoc, nil
		},
		readsBoard: func(_ context.Context, userUUID, boardID string) (bool, error) {
			return userUUID == member.String() && boardID == readableBoard, nil
		},
		viewsTable: func(_ context.Context, user *userModels.User, tableID uuid.UUID) (bool, error) {
			return user.Id == member && tableID.String() == viewableTable, nil
		},
	}
}

func TestOnlyWhatAPersonCanReadIsSubscribable(t *testing.T) {
	t.Setenv("JWT_SECRET", "access-test-secret")
	member, admin, stranger := uuid.New(), uuid.New(), uuid.New()
	channel, otherChannel := uuid.NewString(), uuid.NewString()
	doc, privateDoc := uuid.NewString(), uuid.NewString()
	board, privateBoard := uuid.NewString(), uuid.NewString()
	table, privateTable := uuid.NewString(), uuid.NewString()
	s := testSources(t, member, admin, channel, doc, board, table)

	ask := func(who uuid.UUID, topic, action string) bool {
		return s.allowed(context.Background(), Request{Username: "user_" + who.String(), Topic: topic, Action: action})
	}
	msg, typing := helpers.GetMqttTopicForChannel(channel)
	otherMsg, otherTyping := helpers.GetMqttTopicForChannel(otherChannel)

	for _, c := range []struct {
		name   string
		who    uuid.UUID
		topic  string
		action string
		want   bool
	}{
		{"their channel's messages", member, msg, "subscribe", true},
		{"their channel's typing", member, typing, "subscribe", true},
		{"a channel they're not in", member, otherMsg, "subscribe", false},
		{"typing in a channel they're not in", member, otherTyping, "subscribe", false},
		{"their own notifications", member, helpers.GetMqttTopicForUserActivity(member.String()), "subscribe", true},
		{"someone else's notifications", member, helpers.GetMqttTopicForUserActivity(admin.String()), "subscribe", false},
		{"who's online", member, helpers.GetPublicUsersStatusTopic(), "subscribe", true},
		{"a doc they can read", member, helpers.GetMqttTopicForDoc(doc), "subscribe", true},
		{"a private doc", member, helpers.GetMqttTopicForDoc(privateDoc), "subscribe", false},
		{"a board they can read", member, helpers.GetMqttTopicForBoard(board), "subscribe", true},
		{"a private board", member, helpers.GetMqttTopicForBoard(privateBoard), "subscribe", false},
		{"a table they can see", member, helpers.GetMqttTopicForTable(table), "subscribe", true},
		{"someone's private table", member, helpers.GetMqttTopicForTable(privateTable), "subscribe", false},
		{"the admin broadcast, as a member", member, helpers.GetMqttTopicForAdminBroadcast(), "subscribe", false},
		{"the admin broadcast, as an admin", admin, helpers.GetMqttTopicForAdminBroadcast(), "subscribe", true},

		// The ways round it.
		{"every channel's messages", member, "message/#", "subscribe", false},
		{"every doc", member, "doc/#", "subscribe", false},
		{"one level of everything", member, "+/" + msg[len("message/"):], "subscribe", false},
		{"everything", member, "#", "subscribe", false},
		{"a shared subscription", member, "$share/g/" + otherMsg, "subscribe", false},
		{"system topics", member, "$SYS/#", "subscribe", false},
		{"publishing to their own channel", member, msg, "publish", false},
		{"publishing who's online", member, helpers.GetPublicUsersStatusTopic(), "publish", false},
		{"an action that isn't one", member, msg, "all", false},
		{"someone who isn't a member", stranger, helpers.GetPublicUsersStatusTopic(), "subscribe", false},
		{"a doc named by something other than a uuid", member, helpers.GetMqttTopicForDoc("../" + doc), "subscribe", false},
		{"a doc topic made from a channel id", member, helpers.GetMqttTopicForDoc(channel), "subscribe", false},
	} {
		if got := ask(c.who, c.topic, c.action); got != c.want {
			t.Errorf("%s: allowed=%v, want %v", c.name, got, c.want)
		}
	}

	for name, username := range map[string]string{
		"the backend's name":            "backend",
		"no prefix":                     member.String(),
		"another prefix":                "guest_" + member.String(),
		"an id written another way":     "user_" + "{" + member.String() + "}",
		"an id in capitals":             "user_" + "A" + member.String()[1:],
		"an empty id":                   "user_",
		"the dashboard":                 "dashboard",
		"an id with something after it": "user_" + member.String() + "x",
	} {
		if s.allowed(context.Background(), Request{Username: username, Topic: helpers.GetPublicUsersStatusTopic(), Action: "subscribe"}) {
			t.Errorf("%s was let in", name)
		}
	}
}

func TestALookupThatFailsIsANo(t *testing.T) {
	t.Setenv("JWT_SECRET", "access-test-secret")
	member := uuid.New()
	broken := errors.New("database down")
	s := sources{
		activeUser: func(context.Context, uuid.UUID) (*userModels.User, error) {
			return &userModels.User{Id: member}, nil
		},
		memberTopic: func(context.Context, string, string) (bool, error) { return true, broken },
		readsDoc:    func(context.Context, string, string) (bool, error) { return true, broken },
		readsBoard:  func(context.Context, string, string) (bool, error) { return true, broken },
		viewsTable: func(context.Context, *userModels.User, uuid.UUID) (bool, error) {
			return true, broken
		},
	}
	id := uuid.NewString()
	for _, topic := range []string{
		helpers.GetMqttTopicForChannelMessage(id), helpers.GetMqttTopicForDoc(id),
		helpers.GetMqttTopicForBoard(id), helpers.GetMqttTopicForTable(id),
	} {
		if s.allowed(context.Background(), Request{Username: "user_" + member.String(), Topic: topic, Action: "subscribe"}) {
			t.Errorf("%s was allowed on a failed lookup", topic)
		}
	}

	// A person who can't be looked up, or isn't active, gets nothing at all.
	s.activeUser = func(context.Context, uuid.UUID) (*userModels.User, error) { return nil, broken }
	if s.allowed(context.Background(), Request{Username: "user_" + member.String(), Topic: helpers.GetPublicUsersStatusTopic(), Action: "subscribe"}) {
		t.Error("allowed when the person couldn't be looked up")
	}
	s.activeUser = func(context.Context, uuid.UUID) (*userModels.User, error) { return &userModels.User{}, nil }
	if s.allowed(context.Background(), Request{Username: "user_" + member.String(), Topic: helpers.GetPublicUsersStatusTopic(), Action: "subscribe"}) {
		t.Error("allowed for a person who isn't active")
	}

	// Nor does an external person or a bot: neither signs in, so a broker
	// token naming one was minted while it wrongly could.
	for name, u := range map[string]*userModels.User{
		"an external person": {Id: member, IsExternal: true},
		"a bot":              {Id: member, IsExternal: true, IsBot: true},
	} {
		s.activeUser = func(context.Context, uuid.UUID) (*userModels.User, error) { return u, nil }
		if s.allowed(context.Background(), Request{Username: "user_" + member.String(), Topic: helpers.GetPublicUsersStatusTopic(), Action: "subscribe"}) {
			t.Errorf("allowed for %s", name)
		}
	}
}
