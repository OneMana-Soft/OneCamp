//go:build integration
// +build integration

package integration_test

// A reminder that fires waits in Later, against real Postgres, Redis and
// Dgraph. It was only a toast for an open tab and a push, so the desktop app,
// an install without Firebase and anyone offline when it fired never saw it.
// Now it is a Later item for whoever it reminds, due the moment it fired and
// already told of, so it sends nothing more; one that repeats comes back to
// the top of Later each time rather than piling up.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAFiredReminderWaitsInLater -v

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	commandAdapter "github.com/akashc777/OneCamp/adapter/Command"
	commandBusiness "github.com/akashc777/OneCamp/business/Command"
	schedulerBusiness "github.com/akashc777/OneCamp/business/Scheduler"
	jobDomain "github.com/akashc777/OneCamp/domain/ScheduledJob"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	jobModel "github.com/akashc777/OneCamp/models/postgres/ScheduledJob"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAFiredReminderWaitsInLater(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	commandBusiness.SeedBuiltinCommands(ctx)

	var mu sync.Mutex
	var kinds []string
	defer firebaseInit.ObservePushesForTest(func(data map[string]string, tokens []string) {
		mu.Lock()
		kinds = append(kinds, data[firebaseInit.FIREBASE_PUSH_DATA_TYPE])
		mu.Unlock()
	})()

	people := map[string]uuid.UUID{}
	for handle, name := range map[string]string{"sam": "Sam Rivera", "maya": "Maya Chen"} {
		id := uuid.New()
		people[handle] = id
		for _, q := range []string{
			`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $3, NOW(), NOW())`,
			`INSERT INTO users_fcm_token (user_id, device_id, fcm_token) VALUES ($1, 'phone', 'tok-' || $2)`,
		} {
			args := []any{id, handle}
			if strings.Contains(q, "$3") {
				args = append(args, name)
			}
			if _, err := env.PG.Exec(q, args...); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		dg.Mutate(t, map[string]any{"dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
	}
	pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, people["sam"])
	if err != nil || pg == nil {
		t.Fatalf("load sam: %v", err)
	}
	gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, people["sam"].String())
	if err != nil || gr == nil {
		t.Fatalf("load sam's node: %v", err)
	}
	sam := userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr}

	set := func(text string) *jobModel.ScheduledJob {
		t.Helper()
		if _, err := commandBusiness.Execute(ctx, sam, commandAdapter.ExecuteCommandRequest{Command: "remind", Text: text, Timezone: "UTC"}); err != nil {
			t.Fatal(err)
		}
		jobs, err := jobDomain.ListByUser(ctx, people["sam"], jobModel.JobTypeReminder, []string{jobModel.StatusPending}, 25)
		if err != nil || len(jobs) == 0 {
			t.Fatalf("no reminder saved (%v)", err)
		}
		for _, j := range jobs {
			var p map[string]string
			_ = json.Unmarshal([]byte(j.Payload), &p)
			if text == "me to "+p["text"]+" in 5 minutes" || text == "@maya to "+p["text"]+" in 5 minutes" {
				return j
			}
		}
		t.Fatalf("the reminder %q wasn't saved", text)
		return nil
	}
	type later struct {
		itemType, title, link string
		due, told             bool
	}
	laterOf := func(handle string) []later {
		t.Helper()
		rows, err := env.PG.Query(`SELECT item_type, title, link, remind_at <= NOW(), reminded_at IS NOT NULL
			FROM saved_items WHERE user_id = $1 AND done_at IS NULL`, people[handle])
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []later
		for rows.Next() {
			var l later
			if err := rows.Scan(&l.itemType, &l.title, &l.link, &l.due, &l.told); err != nil {
				t.Fatal(err)
			}
			out = append(out, l)
		}
		return out
	}
	fire := func(job *jobModel.ScheduledJob) {
		t.Helper()
		schedulerBusiness.RunClaimed(ctx, job)
		time.Sleep(500 * time.Millisecond) // anything else it sent would be on its way by now
	}

	t.Run("a reminder for yourself waits in your Later, due now", func(t *testing.T) {
		fire(set("me to stretch in 5 minutes"))
		want := []later{{"reminder", "⏰ Reminder: stretch", "/app/later", true, true}}
		if got := laterOf("sam"); len(got) != 1 || got[0] != want[0] {
			t.Errorf("sam's Later: %+v, want %+v", got, want)
		}
	})

	t.Run("a reminder for a teammate waits in theirs", func(t *testing.T) {
		fire(set("@maya to review the deck in 5 minutes"))
		want := later{"reminder", "⏰ Sam Rivera asked me to remind you: review the deck", "/app/later", true, true}
		if got := laterOf("maya"); len(got) != 1 || got[0] != want {
			t.Errorf("maya's Later: %+v, want %+v", got, want)
		}
	})

	t.Run("it sends nothing more than the reminder's own push", func(t *testing.T) {
		mu.Lock()
		defer mu.Unlock()
		for _, k := range kinds {
			if k != firebaseInit.FIREBASE_PUSH_DATA_TYPE_REMINDER {
				t.Errorf("a %q push besides the reminders' own: %v", k, kinds)
			}
		}
		if len(kinds) != 2 {
			t.Errorf("%d pushes for two reminders: %v", len(kinds), kinds)
		}
	})

	t.Run("a reminder that fires again comes back to the top, not twice", func(t *testing.T) {
		runAt := time.Now().Add(time.Hour)
		repeat, err := schedulerBusiness.Enqueue(ctx, schedulerBusiness.EnqueueInput{JobType: jobModel.JobTypeReminder,
			UserUUID: people["sam"], PayloadJSON: `{"text":"water the plants","target_type":"self","created_by":"` + people["sam"].String() + `"}`,
			RunAt: runAt})
		if err != nil {
			t.Fatal(err)
		}
		job := &jobModel.ScheduledJob{Id: repeat, JobType: jobModel.JobTypeReminder, UserUuid: people["sam"], RunAt: runAt,
			Payload: `{"text":"water the plants","target_type":"self","created_by":"` + people["sam"].String() + `"}`}
		fire(job)
		if _, err := env.PG.Exec(`UPDATE saved_items SET done_at = NOW() WHERE user_id = $1 AND title LIKE '%plants'`, people["sam"]); err != nil {
			t.Fatal(err)
		}
		// It fires again at its next time: a repeating job is the same job,
		// moved on by Reschedule. The same run fired twice does its work once
		// (business/Scheduler/ranOnce.go), as a retried run must.
		job.RunAt = runAt.Add(24 * time.Hour)
		fire(job)
		n := 0
		for _, l := range laterOf("sam") {
			if l.title == "⏰ Reminder: water the plants" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%d open Later items for one repeating reminder, want 1", n)
		}
	})
}
