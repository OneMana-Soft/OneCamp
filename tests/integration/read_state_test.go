//go:build integration
// +build integration

package integration_test

// Where you are in a conversation, and Mark unread from a message (chat A3),
// against real Postgres 12 and Dgraph: the first unread is the message named,
// what follows counts but your own, the sidebar's count agrees, a message of
// another conversation and someone who can't read it are refused alike, and
// each move is told to the mover's own activity topic (read.moved).
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestMarkUnreadPutsTheLineAtTheMessage' -v

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	readStateBusiness "github.com/akashc777/OneCamp/business/ReadState"
	postDomain "github.com/akashc777/OneCamp/domain/Post"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestMarkUnreadPutsTheLineAtTheMessage(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	broker := &noticeBroker{}
	integration.UseMqttClient(t, broker)
	t.Cleanup(func() { helpers.SettleBackground(10 * time.Second) })

	ids := map[string]uuid.UUID{"ada": uuid.New(), "bea": uuid.New(), "cal": uuid.New()}
	for name, id := range ids {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
	}
	launch, other := uuid.New(), uuid.New()
	for _, ch := range []uuid.UUID{launch, other} {
		if _, err := env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, $2, true)`, ch, "ch-"+ch.String()[:8]); err != nil {
			t.Fatal(err)
		}
	}
	group := "0123456789abcdef0123456789abcdef"
	const live = "0001-01-01T00:00:00Z"
	one := func(name string) map[string]any { return map[string]any{"uid": "_:" + name} }
	nodes := []map[string]any{}
	for name, id := range ids {
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
	}
	nodes = append(nodes,
		map[string]any{"dgraph.type": "Channel", "ch_uuid": launch.String(), "ch_name": "launch", "ch_private": true, "ch_deleted_at": live,
			"ch_members": []map[string]any{one("ada"), one("bea")}},
		map[string]any{"dgraph.type": "Channel", "ch_uuid": other.String(), "ch_name": "other", "ch_private": true, "ch_deleted_at": live,
			"ch_members": []map[string]any{one("ada"), one("bea")}},
		map[string]any{"dgraph.type": "Dm", "dm_grouping_id": group, "dm_participants": []map[string]any{one("ada"), one("bea"), one("cal")}},
	)
	uids := dg.Mutate(t, nodes)
	person := func(name string) *userModels.UserInfo {
		u := &userModels.UserInfo{}
		u.UserPostgresInfo.Id = ids[name]
		u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids[name], Uuid: ids[name].String(), UserName: name}
		return u
	}
	ada, cal := person("ada"), person("cal")

	// #launch: Ada, Bea, Bea, Ada, a second apart; one of Bea's in #other.
	t0 := time.Now().Add(-time.Hour)
	post := func(ch uuid.UUID, by string, at time.Time) string {
		id := uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO posts (id, post_channel, created_by, created_at, updated_at) VALUES ($1, $2, $3, $4, $4)`, id, ch, ids[by], at); err != nil {
			t.Fatal(err)
		}
		return id.String()
	}
	post(launch, "ada", t0)
	p2 := post(launch, "bea", t0.Add(time.Second))
	post(launch, "bea", t0.Add(2*time.Second))
	post(launch, "ada", t0.Add(3*time.Second))
	elsewhere := post(other, "bea", t0.Add(time.Second))

	channel := conversationBusiness.Ref{Kind: conversationBusiness.KindChannel, ID: launch.String()}
	if err := readStateBusiness.States.MarkRead(ctx, ada, channel); err != nil {
		t.Fatal(err)
	}
	if p, err := readStateBusiness.States.Position(ctx, ada, channel); err != nil || p.Unread != 0 || p.FirstUnread != "" || p.Since == nil {
		t.Fatalf("after reading: %+v %v", p, err)
	}

	// Mark unread from Bea's first: it and her second are unread, Ada's own not.
	if err := readStateBusiness.States.MarkUnread(ctx, ada, channel, p2); err != nil {
		t.Fatal(err)
	}
	p, err := readStateBusiness.States.Position(ctx, ada, channel)
	if err != nil || p.Unread != 2 || p.FirstUnread != p2 {
		t.Fatalf("after Mark unread from Bea's first: %+v %v", p, err)
	}
	// The sidebar's count says the same.
	if err, counts := postDomain.GetLatestPostInChannelCountByUserID(ctx, ids["ada"]); err != nil || counts[launch.String()] == nil || counts[launch.String()].PostCount != 2 {
		t.Fatalf("the sidebar's count: %+v %v", counts[launch.String()], err)
	}

	// A message of another conversation, and someone not in it, are refused alike.
	if err := readStateBusiness.States.MarkUnread(ctx, ada, channel, elsewhere); !errors.Is(err, conversationBusiness.ErrNotFound) {
		t.Errorf("a message of #other: %v", err)
	}
	if err := readStateBusiness.States.MarkUnread(ctx, cal, channel, p2); !errors.Is(err, conversationBusiness.ErrNotFound) {
		t.Errorf("Cal, not in #launch: %v", err)
	}
	if _, err := readStateBusiness.States.Position(ctx, cal, channel); !errors.Is(err, conversationBusiness.ErrNotFound) {
		t.Errorf("Cal's position in #launch: %v", err)
	}

	// A group the same way, through its chats.
	chat := func(by string, at time.Time) string {
		id := uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO chats (id, created_by, grp_id, created_at, updated_at) VALUES ($1, $2, $3, $4, $4)`, id, ids[by], group, at); err != nil {
			t.Fatal(err)
		}
		return id.String()
	}
	m1 := chat("bea", t0)
	chat("ada", t0.Add(time.Second))
	chat("cal", t0.Add(2*time.Second))
	g := conversationBusiness.ChatRef(group)
	if g.Kind != conversationBusiness.KindGroup {
		t.Fatalf("the group's ref: %+v", g)
	}
	if err := readStateBusiness.States.MarkRead(ctx, ada, g); err != nil {
		t.Fatal(err)
	}
	if err := readStateBusiness.States.MarkUnread(ctx, ada, g, m1); err != nil {
		t.Fatal(err)
	}
	if p, err := readStateBusiness.States.Position(ctx, ada, g); err != nil || p.Unread != 2 || p.FirstUnread != m1 {
		t.Fatalf("the group after Mark unread: %+v %v", p, err)
	}

	// Every move was told to Ada's own topic, the last one where she now is.
	helpers.SettleBackground(5 * time.Second)
	topic := helpers.GetMqttTopicForUserActivity(ids["ada"].String())
	var moves []readStateBusiness.MovedEvent
	broker.mu.Lock()
	for _, m := range broker.got {
		var msg struct {
			Type int8                         `json:"type"`
			Data readStateBusiness.MovedEvent `json:"data"`
		}
		if m.topic == topic && json.Unmarshal(m.payload, &msg) == nil && msg.Type == mqttStruct.MESSAGE_READ_MOVED {
			moves = append(moves, msg.Data)
		}
	}
	broker.mu.Unlock()
	if len(moves) != 4 {
		t.Fatalf("%d moves told, want 4: %+v", len(moves), moves)
	}
	if last := moves[3]; last.Kind != "group" || last.ID != group || last.Unread != 2 || last.FirstUnread != m1 {
		t.Errorf("the last move: %+v", last)
	}
	if first := moves[0]; first.Kind != "channel" || first.Unread != 0 {
		t.Errorf("the first move: %+v", first)
	}
}
