//go:build integration

package business

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	activityBusiness "github.com/akashc777/OneCamp/business/Activity"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	chatDomain "github.com/akashc777/OneCamp/domain/Chat"
	conversationDomain "github.com/akashc777/OneCamp/domain/Conversation"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/akashc777/OneCamp/tests/integration"
)

// A run's moments ("I need your input", "done") reach the person who set it
// going, against real Postgres and Dgraph, only while they can still see where
// it happened:
//   - a delegated task's moment reaches whoever handed it over by the one way
//     a push goes out (notificationBusiness.Deliver), so someone with no
//     device, who uses only the desktop app, is told too (it went to their
//     devices alone, audit B3), while they may read the task; taken off its
//     project since, they hear nothing, though the push would name the task;
//   - a durable run's moment pings whoever asked in a channel or a group only
//     while they can read the channel, or are in the group, now.
//
// Run: go test -tags=integration ./business/AIAgent/ -run 'TestADelegatedTasksMomentReachesTheDesktopApp|TestARunsMomentPingsOnlyWhoCanStillSeeWhereTheyAsked' -v
func TestADelegatedTasksMomentReachesTheDesktopApp(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	broker := &recordingBroker{}
	integration.UseMqttClient(t, broker)
	savedName := delegatedTaskName
	t.Cleanup(func() { delegatedTaskName = savedName })
	delegatedTaskName = func(context.Context, *model.AiAgent, string) string { return "Book the launch retro" }

	// Mo is in the task's project and has no device: the desktop app only.
	// Lou handed a task over too, and has been taken off the project since.
	mo, lou := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{mo, lou} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id) VALUES ($1, $2)`, id, id.String()[:8]+"@example.test"); err != nil {
			t.Fatal(err)
		}
	}
	project, task := uuid.NewString(), uuid.NewString()
	dg.Mutate(t, []map[string]any{
		{"uid": "_:mo", "user_uuid": mo.String(), "user_name": "mo"},
		{"uid": "_:lou", "user_uuid": lou.String(), "user_name": "lou"},
		{"uid": "_:p", "project_uuid": project, "project_name": "Launch", "project_members": []map[string]any{{"uid": "_:mo"}}},
		{"uid": "_:t", "task_uuid": task, "task_name": "Book the launch retro", "task_project": map[string]any{"uid": "_:p"}},
	})

	agent := &model.AiAgent{Id: uuid.New(), Name: "Release Captain"}
	notifyTaskDelegator(agent, task, mo.String(), "Done — booked for Friday.")
	notifyTaskDelegator(agent, task, lou.String(), "Done — booked for Friday.")
	if busy := helpers.SettleBackground(10 * time.Second); len(busy) > 0 {
		t.Fatalf("still at work: %v", busy)
	}
	want := []string{"Release Captain · Book the launch retro"}
	if got := broker.noticeTitles(helpers.GetMqttTopicForUserActivity(mo.String())); !slices.Equal(got, want) {
		t.Errorf("the desktop app of the person who delegated the task was told %q, want %q", got, want)
	}
	if got := broker.noticeTitles(helpers.GetMqttTopicForUserActivity(lou.String())); len(got) != 0 {
		t.Errorf("lou, taken off the task's project, was told %q", got)
	}
}

func TestARunsMomentPingsOnlyWhoCanStillSeeWhereTheyAsked(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	broker := &recordingBroker{}
	integration.UseMqttClient(t, broker)

	// Ana is in the private channel and in a group of four; Cy asked in both
	// and has left them since: out of the channel, and out of the group as
	// leaving takes someone out (the conversations index and the graph), still
	// holding the setting for it everyone in it had, as leavers did.
	ana, bo, cy, dee := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	four := []string{ana.String(), bo.String(), cy.String(), dee.String()}
	for _, id := range []uuid.UUID{ana, bo, cy, dee} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id) VALUES ($1, $2)`, id, id.String()[:8]+"@example.test"); err != nil {
			t.Fatal(err)
		}
	}
	channel := uuid.NewString()
	if _, err := env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, 'exec', true)`, channel); err != nil {
		t.Fatal(err)
	}
	group, err := helpers.GenerateGroupID(four)
	if err != nil {
		t.Fatal(err)
	}
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:ana", "user_uuid": ana.String(), "user_name": "ana"},
		{"uid": "_:bo", "user_uuid": bo.String(), "user_name": "bo"},
		{"uid": "_:cy", "user_uuid": cy.String(), "user_name": "cy"},
		{"uid": "_:dee", "user_uuid": dee.String(), "user_name": "dee"},
		{"uid": "_:exec", "ch_uuid": channel, "ch_name": "exec", "ch_private": true,
			"ch_members": []map[string]any{{"uid": "_:ana"}}},
		{"uid": "_:group", "dm_grouping_id": group,
			"dm_participants": []map[string]any{{"uid": "_:ana"}, {"uid": "_:bo"}, {"uid": "_:cy"}, {"uid": "_:dee"}}},
	})
	conversationDomain.Opened(ctx, conversationDomain.KindGroup, group, ana.String(), four, nil)
	for _, id := range four {
		if _, err := env.PG.Exec(`INSERT INTO users_chat_notification (user_id, grp_id, notification_type) VALUES ($1, $2, 'all')`, id, group); err != nil {
			t.Fatal(err)
		}
	}
	if err := conversationDomain.Remove(ctx, conversationDomain.KindGroup, group, []string{cy.String()}, func() error {
		return chatDomain.LeaveGroup(ctx, group, uids["cy"])
	}); err != nil {
		t.Fatalf("cy leaves the group: %v", err)
	}
	var kept int
	if err := env.PG.QueryRow(`SELECT count(*) FROM users_chat_notification WHERE user_id = $1 AND grp_id = $2`, cy, group).Scan(&kept); err != nil || kept != 1 {
		t.Fatalf("cy holds %d settings for the group they left (%v), want the one they had", kept, err)
	}

	agent := &model.AiAgent{Id: uuid.New(), Name: "Release Captain"}
	inChannel := Surface{Kind: SurfaceChannelPost, ChannelID: channel, PostID: uuid.NewString()}
	inGroup := Surface{Kind: SurfaceGroupChat, GroupID: group, MessageID: uuid.NewString()}
	for _, who := range []uuid.UUID{ana, cy} {
		notifyTriggerer(ctx, agent, inChannel, who.String(), NoticeAgentReplied, "Done: the numbers are in the thread.")
		notifyTriggerer(ctx, agent, inGroup, who.String(), NoticeAgentReplied, "Done: the numbers are in the thread.")
	}
	if busy := helpers.SettleBackground(10 * time.Second); len(busy) > 0 {
		t.Fatalf("still at work: %v", busy)
	}
	if got := broker.notices(helpers.GetMqttTopicForUserActivity(ana.String())); got != 2 {
		t.Errorf("ana, in the channel and the group, was pinged %d times by a notice, want 2", got)
	}
	if got := broker.notices(helpers.GetMqttTopicForUserActivity(cy.String())) + broker.activities(helpers.GetMqttTopicForUserActivity(cy.String())); got != 0 {
		t.Errorf("cy, who left the channel and the group, was pinged %d times with the agent's words", got)
	}
	// C2: a moment was a live item alone, never stored, so the Inbox never
	// listed it and the badge (counted from what the Inbox lists) never
	// counted it, while the live +1 did. It is ana's Inbox notice now:
	// stored, listed with the agent's words and the place, and counted.
	if got := broker.activities(helpers.GetMqttTopicForUserActivity(ana.String())); got != 0 {
		t.Errorf("ana was sent %d live activity items no list holds", got)
	}
	var stored int
	if err := env.PG.QueryRow(`SELECT count(*) FROM resource_notices WHERE user_uuid = $1 AND kind = $2`, ana, NoticeAgentReplied).Scan(&stored); err != nil || stored != 2 {
		t.Errorf("ana has %d stored notices of the run's moments (%v), want 2", stored, err)
	}
	var cyStored int
	if err := env.PG.QueryRow(`SELECT count(*) FROM resource_notices WHERE user_uuid = $1`, cy).Scan(&cyStored); err != nil || cyStored != 0 {
		t.Errorf("cy has %d stored notices (%v), want none", cyStored, err)
	}
	page := activityBusiness.WithNotices(ctx, dgraphModels.UnifiedActivityPagination{}, ana.String(), time.Now().Add(time.Minute), 20)
	places := map[string]bool{}
	for _, it := range page.Activities {
		n := it.Notice
		if n == nil || n.Kind != NoticeAgentReplied || !n.CanOpen || n.Data[notificationBusiness.NoticeWords] != "Done: the numbers are in the thread." || n.Agent == nil {
			t.Errorf("ana's Inbox lists %+v; want the agent's reply, from the agent, with its words, openable", n)
			continue
		}
		places[n.ResourceKind+":"+n.Title] = true
	}
	if len(page.Activities) != 2 || !places["channel:exec"] || !places["conversation:"] {
		t.Errorf("ana's Inbox lists %d moments in %v; want the one in #exec and the one in the group", len(page.Activities), places)
	}
	if n, err := activityBusiness.UnreadActivityCount(ctx, ana, uids["ana"]); err != nil || n != 2 {
		t.Errorf("ana's badge counts %d (%v); want her 2 moments", n, err)
	}
}

// notices is how many live Inbox notices were published on topic.
func (b *recordingBroker) notices(topic string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, m := range b.sent {
		var msg struct {
			Type int8 `json:"type"`
		}
		if m.topic == topic && json.Unmarshal(m.body, &msg) == nil && msg.Type == mqttStruct.MESSAGE_NOTICE {
			n++
		}
	}
	return n
}

// noticeTitles is the title of each desktop notice published on topic.
func (b *recordingBroker) noticeTitles(topic string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var titles []string
	for _, m := range b.sent {
		var msg struct {
			Type int8                         `json:"type"`
			Data mqttStruct.MqttDesktopNotice `json:"data"`
		}
		if m.topic == topic && json.Unmarshal(m.body, &msg) == nil && msg.Type == mqttStruct.MESSAGE_DESKTOP_NOTICE {
			titles = append(titles, msg.Data.Data[firebaseInit.FIREBASE_PUSH_DATA_TITLE])
		}
	}
	return titles
}

// activities is how many live activity items were published on topic.
func (b *recordingBroker) activities(topic string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, m := range b.sent {
		var msg struct {
			Type int8 `json:"type"`
		}
		if m.topic == topic && json.Unmarshal(m.body, &msg) == nil && msg.Type == mqttStruct.MESSAGE_ACTIVITY {
			n++
		}
	}
	return n
}
