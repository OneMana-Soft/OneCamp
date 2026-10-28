//go:build integration

package business

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	chatDomain "github.com/akashc777/OneCamp/domain/Chat"
	conversationDomain "github.com/akashc777/OneCamp/domain/Conversation"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/akashc777/OneCamp/tests/integration"
)

// A run's moments ("I need your input", "done") reach the person who set it
// going, against real Postgres and Dgraph, only while they can still see where
// it happened:
//   - a delegated task's moment is pushed to whoever handed it over
//     (pushToDelegator, their devices) while they may read the task; taken off
//     its project since, they hear nothing, though the push would name the task;
//   - a durable run's moment pings whoever asked in a channel or a group only
//     while they can read the channel, or are in the group, now.
//
// This line's version of beta's tests (8a260528a, 38b378671). Here a delegated
// task's moment is a push to the delegator's devices alone: the desktop app
// half of beta's TestADelegatedTasksMomentReachesTheDesktopApp isn't on this
// line, so its delegator legs are asserted on the push.
//
// Run: go test -tags=integration ./business/AIAgent/ -run 'TestADelegatedTasksMomentReachesOnlyWhoMayReadIt|TestARunsMomentPingsOnlyWhoCanStillSeeWhereTheyAsked' -v
func TestADelegatedTasksMomentReachesOnlyWhoMayReadIt(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	integration.UseMqttClient(t, &typingBroker{})
	savedName := delegatedTaskName
	t.Cleanup(func() { delegatedTaskName = savedName })
	delegatedTaskName = func(context.Context, *model.AiAgent, string) string { return "Book the launch retro" }
	var mu sync.Mutex
	pushed := map[string][]string{} // by person, each push's title
	savedPush := pushToDelegator
	t.Cleanup(func() { pushToDelegator = savedPush })
	pushToDelegator = func(_ context.Context, userUUID string, data map[string]string) error {
		mu.Lock()
		defer mu.Unlock()
		pushed[userUUID] = append(pushed[userUUID], data[firebaseInit.FIREBASE_PUSH_DATA_TITLE])
		return nil
	}
	titles := func(who uuid.UUID) []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(pushed[who.String()])
	}

	// Mo is in the task's project. Lou handed a task over too, and has been
	// taken off the project since.
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
	// Each push goes on a goroutine of its own: wait for mo's, then as long
	// again for lou's, when it goes.
	for deadline := time.Now().Add(10 * time.Second); len(titles(mo)) == 0 && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(time.Second)
	want := []string{"Release Captain · Book the launch retro"}
	if got := titles(mo); !slices.Equal(got, want) {
		t.Errorf("the person who delegated the task was pushed %q, want %q", got, want)
	}
	if got := titles(lou); len(got) != 0 {
		t.Errorf("lou, taken off the task's project, was pushed %q", got)
	}
}

func TestARunsMomentPingsOnlyWhoCanStillSeeWhereTheyAsked(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	broker := &typingBroker{}
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
		notifyTriggerer(ctx, agent, inChannel, who.String(), "Done: the numbers are in the thread.")
		notifyTriggerer(ctx, agent, inGroup, who.String(), "Done: the numbers are in the thread.")
	}
	if busy := helpers.SettleBackground(10 * time.Second); len(busy) > 0 {
		t.Fatalf("still at work: %v", busy)
	}
	if got := broker.activities(helpers.GetMqttTopicForUserActivity(ana.String())); got != 2 {
		t.Errorf("ana, in the channel and the group, was pinged %d times, want 2", got)
	}
	if got := broker.activities(helpers.GetMqttTopicForUserActivity(cy.String())); got != 0 {
		t.Errorf("cy, who left the channel and the group, was pinged %d times with the agent's words", got)
	}
}

// activities is how many live activity items were published on topic.
func (b *typingBroker) activities(topic string) int {
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
