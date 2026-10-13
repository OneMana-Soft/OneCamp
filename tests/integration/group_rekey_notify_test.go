//go:build integration
// +build integration

package integration_test

// A group conversation keeps telling its people after someone is added, and a
// direct conversation tells both people, against real Postgres, Redis and
// Dgraph.
//
// A group's id is a hash of its people, so adding someone moves the group to
// a new id. Its messages, files and read marks moved, but not its people's
// notification settings, which are all that says who hears of a message, nor
// the messages queued to be sent to it later: once anyone was added, nobody
// was told of anything in the group again (no push, email, Activity item or
// call ring), and a queued message failed when its time came. A new direct
// conversation's two settings were one insert, which failed whole when either
// row was already there, and left the other person never told.
//
// Groups and conversations left that way before the fix get their settings
// back at the next start (StartChatSettingsRepair).
//
// Run: go test -tags=integration ./tests/integration/ -run TestAGroupKeepsTellingItsPeople -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	scheduledMessageBusiness "github.com/akashc777/OneCamp/business/ScheduledMessage"
	chatController "github.com/akashc777/OneCamp/controllers/Chat"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	chatNotificationDomain "github.com/akashc777/OneCamp/domain/UserChatNotification"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAGroupKeepsTellingItsPeople(t *testing.T) {
	ctx := context.Background()
	t.Setenv("RESEND_API_KEY", "re_integration_not_a_key") // notification email on; nothing is sent from a test
	t.Setenv("EMAIL_ESSENTIAL_ONLY", "")
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[],"updated":0,"failures":[]}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client

	var mu sync.Mutex
	var pushes []sentPush
	defer firebaseInit.ObservePushesForTest(func(data map[string]string, tokens []string) {
		mu.Lock()
		pushes = append(pushes, sentPush{data, tokens})
		mu.Unlock()
	})()

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	people := map[string]uuid.UUID{}
	byID := map[string]string{}
	uids := map[string]string{}
	for _, name := range []string{"ada", "bo", "cy", "dee"} {
		id := uuid.New()
		people[name], byID[id.String()] = id, name
		exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name)
		exec(`INSERT INTO users_fcm_token (user_id, device_id, fcm_token) VALUES ($1, 'phone', 'tok-' || $2)`, id, name)
		exec(`INSERT INTO users_notification_preferences (user_id, unsubscribe_token) VALUES ($1, 'unsub-' || $2)`, id, name)
		got := dg.Mutate(t, map[string]any{"uid": "_:u", "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
		uids[name] = got["u"]
	}
	info := func(name string) userModels.UserInfo {
		t.Helper()
		pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, people[name])
		if err != nil || pg == nil {
			t.Fatalf("load %s: %v", name, err)
		}
		gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, people[name].String())
		if err != nil || gr == nil {
			t.Fatalf("load %s's node: %v", name, err)
		}
		return userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr}
	}
	as := func(name string) context.Context {
		t.Helper()
		return context.WithValue(ctx, helpers.UserInfoContextKey, info(name))
	}
	call := func(name string, handler http.HandlerFunc, body any) (int, string) {
		t.Helper()
		raw, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(as(name)))
		return rec.Code, rec.Body.String()
	}
	groupOf := func(names ...string) string {
		var ids []string
		for _, n := range names {
			ids = append(ids, people[n].String())
		}
		id, err := helpers.GenerateGroupID(ids)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	settings := func(grp string) map[string]string {
		t.Helper()
		rows, err := env.PG.Query(`SELECT user_id::text, notification_type::text FROM users_chat_notification WHERE grp_id = $1`, grp)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var id, level string
			_ = rows.Scan(&id, &level)
			out[byID[id]] = level
		}
		return out
	}
	mention := func(name string) string {
		return fmt.Sprintf(`<span data-type="mention" data-id="%s@%s">@%s</span>`, people[name], uids[name], name)
	}
	// say has bo write to grp and answers whose phones it reached, and who was
	// emailed of it.
	// Only this message's notices are counted, by its id: one an earlier
	// message sent late, under load, is not this one's.
	say := func(grp, html string) (pushed, emailed []string) {
		t.Helper()
		code, body := call("bo", chatController.CreateGroupChat, chatAdapter.ChatInfo{GrpUuid: grp, TextHtml: html})
		var sent struct {
			Data struct {
				Uuid string `json:"uuid"`
			} `json:"data"`
		}
		_ = json.Unmarshal([]byte(body), &sent)
		if code != http.StatusOK || sent.Data.Uuid == "" {
			t.Fatalf("bo's message: %d %s", code, body)
		}
		time.Sleep(time.Second) // its notices are on their way by now
		mu.Lock()
		for _, p := range pushes {
			if p.data[firebaseInit.FIREBASE_PUSH_DATA_TYPE] == firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHAT &&
				p.data[firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID] == sent.Data.Uuid {
				for _, tok := range p.tokens {
					pushed = append(pushed, strings.TrimPrefix(tok, "tok-"))
				}
			}
		}
		mu.Unlock()
		rows, err := env.PG.Query(`SELECT user_id::text FROM notification_email_queue WHERE strpos(dedup_key, $1) > 0`, sent.Data.Uuid)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			_ = rows.Scan(&id)
			emailed = append(emailed, byID[id])
		}
		slices.Sort(pushed)
		slices.Sort(emailed)
		return pushed, emailed
	}

	// Ada starts a group with Bo and Cy (people are named by their graph
	// uids); Cy hears of mentions only.
	if code, body := call("ada", chatController.CreateGroupChat, chatAdapter.ChatInfo{
		Participants: []string{uids["ada"], uids["bo"], uids["cy"]}, TextHtml: "<p>Kickoff</p>"}); code != http.StatusOK {
		t.Fatalf("ada's group: %d %s", code, body)
	}
	before := groupOf("ada", "bo", "cy")
	exec(`UPDATE users_chat_notification SET notification_type = 'mention' WHERE grp_id = $1 AND user_id = $2`, before, people["cy"])
	// And queues a message to it for later.
	ada := info("ada")
	body, _ := json.Marshal(chatAdapter.ChatInfo{GrpUuid: before, TextHtml: "<p>Standup notes are up</p>"})
	queued, err := scheduledMessageBusiness.Schedule(ctx, &ada, scheduledMessageBusiness.KindGroup, body, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("queue a message: %v", err)
	}

	// Ada adds Dee: the group moves to the id of its four people.
	if code, body := call("ada", chatController.AddParticipantToGroupChat, chatAdapter.InputAddParticipantToGroupChat{
		GrpUuid: before, UserUuid: people["dee"].String()}); code != http.StatusOK {
		t.Fatalf("adding dee: %d %s", code, body)
	}
	after := groupOf("ada", "bo", "cy", "dee")

	t.Run("the settings move with the group, and the person added hears of everything", func(t *testing.T) {
		want := map[string]string{"ada": "all", "bo": "all", "cy": "mention", "dee": "all"}
		if got := settings(after); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("settings under the new id: %v, want %v", got, want)
		}
		if got := settings(before); len(got) != 0 {
			t.Errorf("settings left under the old id: %v", got)
		}
	})

	t.Run("a mention in the group reaches everyone it names", func(t *testing.T) {
		pushed, emailed := say(after, "<p>"+mention("ada")+" "+mention("cy")+" "+mention("dee")+" plan for Friday?</p>")
		if want := []string{"ada", "cy", "dee"}; !slices.Equal(pushed, want) || !slices.Equal(emailed, want) {
			t.Errorf("pushed to %v and emailed %v, want %v", pushed, emailed, want)
		}
	})

	t.Run("the message queued for later goes to the group as it now is", func(t *testing.T) {
		var target, inBody string
		if err := env.PG.QueryRow(`SELECT payload->>'target', payload->'body'->>'grp_id' FROM scheduled_jobs WHERE id = $1`, queued.ID).Scan(&target, &inBody); err != nil {
			t.Fatal(err)
		}
		if target != after || inBody != after {
			t.Errorf("the queued message names %s and %s, want %s", target, inBody, after)
		}
	})

	t.Run("a group silenced before the fix is told again after the repair", func(t *testing.T) {
		// Bo's group with Cy and Dee, as a re-key left groups: its people in
		// it, none with a setting for it.
		if code, body := call("bo", chatController.CreateGroupChat, chatAdapter.ChatInfo{
			Participants: []string{uids["bo"], uids["cy"], uids["dee"]}, TextHtml: "<p>Hi</p>"}); code != http.StatusOK {
			t.Fatalf("bo's group: %d %s", code, body)
		}
		silenced := groupOf("bo", "cy", "dee")
		exec(`DELETE FROM users_chat_notification WHERE grp_id = $1`, silenced)
		if pushed, _ := say(silenced, "<p>Anyone there?</p>"); len(pushed) != 0 {
			t.Fatalf("a silenced group still told %v", pushed)
		}
		given, err := chatNotificationDomain.GiveMissingChatSettings(ctx)
		if err != nil || given != 3 {
			t.Fatalf("the repair gave %d settings (%v), want 3", given, err)
		}
		if want := map[string]string{"bo": "all", "cy": "all", "dee": "all"}; fmt.Sprint(settings(silenced)) != fmt.Sprint(want) {
			t.Errorf("settings after the repair: %v", settings(silenced))
		}
		if pushed, _ := say(silenced, "<p>Back?</p>"); !slices.Equal(pushed, []string{"cy", "dee"}) {
			t.Errorf("after the repair, pushed to %v, want cy and dee", pushed)
		}
		// Run again, it gives nothing and changes nothing.
		if again, err := chatNotificationDomain.GiveMissingChatSettings(ctx); err != nil || again != 0 {
			t.Errorf("a second repair gave %d (%v)", again, err)
		}
	})

	t.Run("a direct conversation's settings are each written whatever the other's", func(t *testing.T) {
		grp := helpers.GetGroupingId(people["ada"].String(), people["dee"].String())
		exec(`INSERT INTO users_chat_notification (user_id, grp_id, notification_type) VALUES ($1, $2, 'mention')`, people["ada"], grp)
		if err := chatNotificationDomain.CreateChatNotificationType(ctx, people["ada"].String(), people["dee"].String(), grp, "all", time.Now()); err != nil {
			t.Fatalf("with Ada's setting already there: %v", err)
		}
		if want := map[string]string{"ada": "mention", "dee": "all"}; fmt.Sprint(settings(grp)) != fmt.Sprint(want) {
			t.Errorf("settings: %v, want %v", settings(grp), want)
		}
	})
}
