//go:build integration
// +build integration

package integration_test

// The chat programme's day-0 contracts answer as today's code does, against
// real Postgres, Dgraph and Redis (design/chat-vs-slack/PROGRAM.md §5, and §9's
// "contracts guarded"): the chat kinds of the Access Resolver decide as the
// handlers' membership checks, Pages reads today's latest page, Messages.Write
// sends as /po/createPost does (once per client id), and ReadState marks a
// channel read as /ch/markSeen does.
//
// Run: go test -tags=integration ./tests/integration/ -run TestChatDay0ContractsAnswerAsToday -v

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	messagePipelineBusiness "github.com/akashc777/OneCamp/business/MessagePipeline"
	postBusiness "github.com/akashc777/OneCamp/business/Post"
	presenceBusiness "github.com/akashc777/OneCamp/business/Presence"
	readStateBusiness "github.com/akashc777/OneCamp/business/ReadState"
	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestChatDay0ContractsAnswerAsToday(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	// Messages are indexed in the background; give them somewhere to go.
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[]}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	old := opensearchInit.OpenSearchClient
	opensearchInit.OpenSearchClient = client
	t.Cleanup(func() { opensearchInit.OpenSearchClient = old })

	// Ada manages #launch (private) and #news (announcements); Bea is in both;
	// Dan is in a group with them; Cal is in nothing but #general's audience.
	ids := map[string]uuid.UUID{"ada": uuid.New(), "bea": uuid.New(), "cal": uuid.New(), "dan": uuid.New()}
	for name, id := range ids {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
	}
	launch, general, news := uuid.New(), uuid.New(), uuid.New()
	for ch, private := range map[uuid.UUID]bool{launch: true, general: false, news: false} {
		if _, err := env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, $2, $3)`, ch, "ch-"+ch.String()[:8], private); err != nil {
			t.Fatal(err)
		}
	}
	const live = "0001-01-01T00:00:00Z"
	at := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	rootPost, groupMessage := uuid.NewString(), uuid.NewString()
	group := "0123456789abcdef0123456789abcdef"
	dm := helpers.GetGroupingId(ids["ada"].String(), ids["bea"].String())
	one := func(name string) map[string]any { return map[string]any{"uid": "_:" + name} }
	nodes := []map[string]any{}
	for name, id := range ids {
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
	}
	nodes = append(nodes,
		map[string]any{"uid": "_:launch", "dgraph.type": "Channel", "ch_uuid": launch.String(), "ch_name": "launch", "ch_private": true,
			"ch_deleted_at": live, "ch_members": []map[string]any{one("ada"), one("bea")}, "ch_moderators": []map[string]any{one("ada")},
			"ch_posts": []map[string]any{{"uid": "_:root", "dgraph.type": "Post", "post_uuid": rootPost, "post_text": "<p>the plan</p>",
				"post_by": one("bea"), "post_channel": one("launch"), "post_created_at": at, "post_deleted_at": live}}},
		map[string]any{"uid": "_:general", "dgraph.type": "Channel", "ch_uuid": general.String(), "ch_name": "general", "ch_private": false,
			"ch_deleted_at": live, "ch_members": []map[string]any{one("ada")}},
		map[string]any{"uid": "_:news", "dgraph.type": "Channel", "ch_uuid": news.String(), "ch_name": "news", "ch_private": false,
			"ch_post_policy": "admins_only", "ch_deleted_at": live, "ch_members": []map[string]any{one("ada"), one("bea")},
			"ch_moderators": []map[string]any{one("ada")}},
		map[string]any{"uid": "_:group", "dgraph.type": "Dm", "dm_grouping_id": group,
			"dm_participants": []map[string]any{one("ada"), one("bea"), one("dan")},
			"dm_chats": []map[string]any{{"uid": "_:gmsg", "dgraph.type": "Chat", "chat_uuid": groupMessage, "chat_body_text": "<p>lunch?</p>",
				"chat_from": one("ada"), "chat_dm": one("group"), "chat_created_at": at, "chat_deleted_at": live}}},
		map[string]any{"uid": "_:dm", "dgraph.type": "Dm", "dm_grouping_id": dm, "dm_participants": []map[string]any{one("ada"), one("bea")}},
	)
	uids := dg.Mutate(t, nodes)
	person := func(name string) *userModels.UserInfo {
		u := &userModels.UserInfo{}
		u.UserPostgresInfo.Id = ids[name]
		u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids[name], Uuid: ids[name].String(), UserName: name}
		return u
	}
	ada, bea, cal, dan := person("ada"), person("bea"), person("cal"), person("dan")

	// The kinds decide as the handlers' membership checks.
	ref := func(kind, id string) conversationBusiness.Ref { return conversationBusiness.Ref{Kind: kind, ID: id} }
	for _, c := range []struct {
		name string
		who  *userModels.UserInfo
		ref  conversationBusiness.Ref
		want accessBusiness.Level // LevelNone: not found
	}{
		{"Ada manages #launch", ada, ref("channel", launch.String()), conversationBusiness.LevelModerate},
		{"Bea posts in #launch", bea, ref("channel", launch.String()), conversationBusiness.LevelPost},
		{"Cal can't see #launch", cal, ref("channel", launch.String()), accessBusiness.LevelNone},
		{"Cal reads #general", cal, ref("channel", general.String()), conversationBusiness.LevelRead},
		{"Ada posts in #general", ada, ref("channel", general.String()), conversationBusiness.LevelPost},
		{"Bea replies in #news, an announcement channel", bea, ref("channel", news.String()), conversationBusiness.LevelReply},
		{"Ada manages #news", ada, ref("channel", news.String()), conversationBusiness.LevelModerate},
		{"Bea replies in #launch's thread", bea, ref("thread", rootPost), conversationBusiness.LevelReply},
		{"Ada manages #launch's thread", ada, ref("thread", rootPost), conversationBusiness.LevelModerate},
		{"Cal can't see #launch's thread", cal, ref("thread", rootPost), accessBusiness.LevelNone},
		{"Dan posts in the group", dan, ref("group", group), conversationBusiness.LevelPost},
		{"Cal can't see the group", cal, ref("group", group), accessBusiness.LevelNone},
		{"Dan replies in the group's thread", dan, ref("thread", groupMessage), conversationBusiness.LevelReply},
		{"Cal can't see the group's thread", cal, ref("thread", groupMessage), accessBusiness.LevelNone},
		{"Ada posts in her DM with Bea", ada, ref("dm", dm), conversationBusiness.LevelPost},
		{"Cal can't see Ada and Bea's DM", cal, ref("dm", dm), accessBusiness.LevelNone},
		{"Bea joins #launch's call", bea, ref("call", launch.String()), conversationBusiness.LevelPost},
		{"Cal doesn't join #general's call", cal, ref("call", general.String()), accessBusiness.LevelNone},
		{"Dan joins the group's call", dan, ref("call", group), conversationBusiness.LevelPost},
	} {
		d, err := conversationBusiness.Resolve(ctx, c.who, c.ref)
		switch {
		case c.want == accessBusiness.LevelNone && !errors.Is(err, conversationBusiness.ErrNotFound):
			t.Errorf("%s: %+v %v, want not found", c.name, d, err)
		case c.want != accessBusiness.LevelNone && (err != nil || d.Level != c.want):
			t.Errorf("%s: %+v %v, want %s", c.name, d, err, c.want)
		}
	}

	// Pages is today's latest page, for someone who can read it.
	page, err := conversationBusiness.Pages.Page(ctx, bea, ref("channel", launch.String()), nil)
	today, terr := postBusiness.GetLatestPosts(ctx, launch.String())
	if err != nil || terr != nil || len(page.Posts) != len(today.Posts) || len(page.Posts) != 1 || page.Posts[0].Uuid != rootPost || page.HasMore != today.HasMore {
		t.Errorf("the latest page: %+v %v, today's %+v %v", page, err, today, terr)
	}
	if _, err := conversationBusiness.Pages.Page(ctx, cal, ref("channel", launch.String()), nil); !errors.Is(err, conversationBusiness.ErrNotFound) {
		t.Errorf("Cal read #launch's page: %v", err)
	}

	// A send is today's send, once per client id.
	send := messagePipelineBusiness.Write{Op: messagePipelineBusiness.OpSend, To: ref("channel", launch.String()),
		Draft: messagePipelineBusiness.Draft{HTML: "<p>shipping friday</p>", ClientMsgID: "local-day0-1"}}
	first, err := messagePipelineBusiness.Messages.Write(ctx, bea, send)
	if err != nil || first.ID == "" {
		t.Fatalf("Bea's send: %+v %v", first, err)
	}
	again, err := messagePipelineBusiness.Messages.Write(ctx, bea, send)
	if err != nil || again.ID != first.ID {
		t.Errorf("a send tried again with its client id is the same send: %+v %v, first %+v", again, err, first)
	}
	var stored int
	if err := env.PG.QueryRow(`SELECT count(*) FROM posts WHERE id = $1`, first.ID).Scan(&stored); err != nil || stored != 1 {
		t.Errorf("the post is stored %d times (%v), want once", stored, err)
	}
	_, err = messagePipelineBusiness.Messages.Write(ctx, cal, send)
	if rj, ok := sendBusiness.AsRejection(err); !ok || rj.Status != http.StatusForbidden {
		t.Errorf("Cal's send to #launch: %v, want the route's 403", err)
	}
	if _, err := messagePipelineBusiness.Messages.Write(ctx, bea, messagePipelineBusiness.Write{Op: messagePipelineBusiness.OpEdit, To: send.To, Message: first.ID}); !errors.Is(err, messagePipelineBusiness.ErrUnavailable) {
		t.Errorf("an edit is still the route's: %v", err)
	}

	// Marking read is /ch/markSeen's, for someone the channel lets read it.
	if err := readStateBusiness.States.MarkRead(ctx, cal, ref("channel", launch.String())); !errors.Is(err, conversationBusiness.ErrNotFound) {
		t.Errorf("Cal marked #launch read: %v", err)
	}
	if _, err := env.PG.Exec(`DELETE FROM last_seen_channel WHERE user_id = $1`, ids["bea"]); err != nil {
		t.Fatal(err)
	}
	if err := readStateBusiness.States.MarkRead(ctx, bea, ref("channel", launch.String())); err != nil {
		t.Fatalf("Bea marking #launch read: %v", err)
	}
	var marked int
	if err := env.PG.QueryRow(`SELECT count(*) FROM last_seen_channel WHERE user_id = $1 AND channel_id = $2`, ids["bea"], launch).Scan(&marked); err != nil || marked != 1 {
		t.Errorf("Bea's marker for #launch: %d rows (%v)", marked, err)
	}

	// Presence is today's fields: Bea set no status and has no device.
	if s, err := presenceBusiness.People.State(ctx, bea.UserDgraphInfo.Uuid); err != nil || s.Connected || s.Emoji != "" {
		t.Errorf("Bea's presence: %+v %v", s, err)
	}
}
