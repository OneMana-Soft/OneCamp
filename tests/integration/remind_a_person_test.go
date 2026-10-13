//go:build integration
// +build integration

package integration_test

// "/remind @maya …" reminds Maya, against real Postgres, Redis and Dgraph.
//
// The command kept "@maya" as a name and never found her, said it would remind
// her, and when the time came reminded the person who set it. Now the person
// is found when the reminder is set: one picked from the composer's list by
// the id the list gives (the text shows only their name, which another
// person's handle can match), one typed by their handle; and nobody who can't
// be reminded is accepted. The reminder reaches that person, saying who
// asked. One set before this fix holds only the handle, and finds the person
// when it fires; when nobody can be reached by then, it comes back to whoever
// set it, saying so.
//
// Run: go test -tags=integration ./tests/integration/ -run TestARemindForAPersonRemindsThem -v

import (
	"context"
	"encoding/json"
	"slices"
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

func TestARemindForAPersonRemindsThem(t *testing.T) {
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
	var pushes []sentPush
	defer firebaseInit.ObservePushesForTest(func(data map[string]string, tokens []string) {
		mu.Lock()
		pushes = append(pushes, sentPush{data, tokens})
		mu.Unlock()
	})()

	// Sam sets reminders. Maya Chen (@mchen) and Maya Patel (@maya) are
	// teammates; @gone was deactivated. People go by their names in the
	// graph and by their handles in Postgres.
	people := map[string]uuid.UUID{}
	names := map[string]string{"sam": "Sam Rivera", "mchen": "Maya Chen", "maya": "Maya Patel", "gone": "Gone"}
	for _, handle := range []string{"sam", "mchen", "maya", "gone"} {
		id := uuid.New()
		people[handle] = id
		for _, q := range []string{
			`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $3, NOW(), NOW())`,
			`INSERT INTO users_fcm_token (user_id, device_id, fcm_token) VALUES ($1, 'phone', 'tok-' || $2)`,
		} {
			args := []any{id, handle}
			if strings.Contains(q, "$3") {
				args = append(args, names[handle])
			}
			if _, err := env.PG.Exec(q, args...); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
	}
	if _, err := env.PG.Exec(`UPDATE users SET deleted_at = NOW() WHERE id = $1`, people["gone"]); err != nil {
		t.Fatal(err)
	}
	dg.Mutate(t, []map[string]any{
		{"dgraph.type": "User", "user_uuid": people["sam"].String(), "user_name": names["sam"]},
		{"dgraph.type": "User", "user_uuid": people["mchen"].String(), "user_name": names["mchen"]},
		{"dgraph.type": "User", "user_uuid": people["maya"].String(), "user_name": names["maya"]},
		{"dgraph.type": "User", "user_uuid": people["gone"].String(), "user_name": names["gone"],
			"user_deleted_at": time.Now().Add(-time.Hour).Format(time.RFC3339)},
	})
	pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, people["sam"])
	if err != nil || pg == nil {
		t.Fatalf("load sam: %v", err)
	}
	gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, people["sam"].String())
	if err != nil || gr == nil {
		t.Fatalf("load sam's node: %v", err)
	}
	sam := userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr}

	remind := func(text string, picked ...string) string {
		t.Helper()
		mentions := map[string]string{}
		for _, handle := range picked {
			mentions[strings.ToLower(names[handle])] = people[handle].String()
		}
		resp, err := commandBusiness.Execute(ctx, sam, commandAdapter.ExecuteCommandRequest{Command: "remind", Text: text, Timezone: "UTC", Mentions: mentions})
		if err != nil || resp == nil {
			t.Fatalf("/remind %s: %v", text, err)
		}
		return resp.Text
	}
	jobs := func() []*jobModel.ScheduledJob {
		t.Helper()
		out, err := jobDomain.ListByUser(ctx, people["sam"], jobModel.JobTypeReminder, []string{jobModel.StatusPending}, 25)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	// fire runs a reminder now and says whose devices it reached, and what it said.
	fire := func(job *jobModel.ScheduledJob) (to []string, said string) {
		t.Helper()
		mu.Lock()
		pushes = nil
		mu.Unlock()
		schedulerBusiness.RunClaimed(ctx, job)
		waitFor(t, "the reminder's push", func() bool {
			mu.Lock()
			defer mu.Unlock()
			for _, p := range pushes {
				if p.data[firebaseInit.FIREBASE_PUSH_DATA_TYPE] == firebaseInit.FIREBASE_PUSH_DATA_TYPE_REMINDER {
					to, said = slices.Clone(p.tokens), p.data[firebaseInit.FIREBASE_PUSH_DATA_BODY]
					return true
				}
			}
			return false
		})
		return to, said
	}
	payload := func(job *jobModel.ScheduledJob) map[string]string {
		var p map[string]string
		_ = json.Unmarshal([]byte(job.Payload), &p)
		return p
	}

	// The reminder saved for what, after the ones before.
	newest := func(what string) *jobModel.ScheduledJob {
		t.Helper()
		for _, j := range jobs() {
			if payload(j)["text"] == what {
				return j
			}
		}
		t.Fatalf("no reminder saved for %q", what)
		return nil
	}

	t.Run("a teammate picked from the list is reminded, told who asked", func(t *testing.T) {
		// "@Maya Chen" in the text, and "maya" is Maya Patel's handle: the
		// list says which Maya was picked.
		if said := remind("@Maya Chen to review the deck in 5 minutes", "mchen"); !strings.Contains(said, "remind Maya Chen") {
			t.Fatalf("the answer: %q", said)
		}
		job := newest("review the deck")
		if payload(job)["target_id"] != people["mchen"].String() {
			t.Fatalf("saved for %v", payload(job))
		}
		to, said := fire(job)
		if !slices.Equal(to, []string{"tok-mchen"}) || said != "⏰ Sam Rivera asked me to remind you: review the deck" {
			t.Errorf("it reached %v saying %q, want Maya Chen's phone and who asked", to, said)
		}
	})

	t.Run("a teammate typed by handle is reminded", func(t *testing.T) {
		if said := remind("@maya to send the slides in 5 minutes"); !strings.Contains(said, "remind Maya Patel") {
			t.Fatalf("the answer: %q", said)
		}
		if to, said := fire(newest("send the slides")); !slices.Equal(to, []string{"tok-maya"}) || said != "⏰ Sam Rivera asked me to remind you: send the slides" {
			t.Errorf("it reached %v saying %q, want Maya Patel's phone", to, said)
		}
	})

	t.Run("a handle that names nobody who can be reminded is refused", func(t *testing.T) {
		before := len(jobs())
		for _, handle := range []string{"nobody", "gone"} {
			if said := remind("@" + handle + " to call the bank in 5 minutes"); !strings.Contains(said, "Couldn't find @"+handle) {
				t.Errorf("@%s: %q", handle, said)
			}
		}
		if n := len(jobs()); n != before {
			t.Errorf("%d reminders saved for nobody", n-before)
		}
	})

	t.Run("a reminder set before the fix finds its person when it fires", func(t *testing.T) {
		for _, tc := range []struct {
			handle, wantTo, wantSaid string
		}{
			{"mchen", "tok-mchen", "⏰ Sam Rivera asked me to remind you: send the invoice"},
			{"gone", "tok-sam", "⏰ Reminder for @gone: send the invoice (not delivered: I couldn't reach @gone, so it came to you)"},
		} {
			raw, _ := json.Marshal(map[string]string{"text": "send the invoice", "target_type": "user",
				"target_name": tc.handle, "created_by": people["sam"].String()})
			id, err := schedulerBusiness.Enqueue(ctx, schedulerBusiness.EnqueueInput{JobType: jobModel.JobTypeReminder,
				UserUUID: people["sam"], PayloadJSON: string(raw), RunAt: time.Now().Add(time.Hour)})
			if err != nil {
				t.Fatal(err)
			}
			var job *jobModel.ScheduledJob
			for _, j := range jobs() {
				if j.Id == id {
					job = j
				}
			}
			if job == nil {
				t.Fatalf("the reminder for @%s wasn't saved", tc.handle)
			}
			if to, said := fire(job); !slices.Equal(to, []string{tc.wantTo}) || said != tc.wantSaid {
				t.Errorf("@%s: it reached %v saying %q, want %s %q", tc.handle, to, said, tc.wantTo, tc.wantSaid)
			}
		}
	})

	t.Run("their own handle is a reminder for themselves", func(t *testing.T) {
		remind("@sam to stretch in 5 minutes")
		mine := newest("stretch")
		if payload(mine)["target_type"] != "self" {
			t.Fatalf("saved as %v", payload(mine))
		}
		if to, said := fire(mine); !slices.Equal(to, []string{"tok-sam"}) || said != "⏰ Reminder: stretch" {
			t.Errorf("it reached %v saying %q", to, said)
		}
	})
}
