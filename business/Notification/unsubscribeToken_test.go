package notification

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	emailQueueModels "github.com/akashc777/OneCamp/models/postgres/NotificationEmailQueue"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	prefModels "github.com/akashc777/OneCamp/models/postgres/UserNotificationPreference"
	"github.com/google/uuid"
)

// No notification goes out that its reader can't stop (V-1241): a mail they
// didn't ask for by itself carries the unsubscribe link of their preference
// row, and one whose row has no token (a row that couldn't be read or made)
// is not queued. A reminder they asked to have emailed carries no link by
// design and still goes.
func TestANotificationWithoutAnUnsubscribeTokenIsNotSent(t *testing.T) {
	t.Setenv("FRONTEND_DOMAIN", "https://team.example.test")
	// No database: the email's logo setting can't be read, so it has none.
	nowhere, err := sql.Open("postgres", "host=127.0.0.1 port=1 user=none dbname=none sslmode=disable connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	oldDB := postgresInit.DBConn
	t.Cleanup(func() { postgresInit.DBConn = oldDB; nowhere.Close() })
	postgresInit.DBConn = &postgresInit.DB{SqlDB: nowhere, DBTimeout: time.Second}
	var queued []emailQueueModels.EnqueueInput
	old := enqueueEmail
	t.Cleanup(func() { enqueueEmail = old })
	enqueueEmail = func(in emailQueueModels.EnqueueInput) (bool, error) {
		queued = append(queued, in)
		return true, nil
	}
	ben := &userModels.User{Id: uuid.New(), EmailID: "ben@acme.test"}
	on := func(token string) *prefModels.UserNotificationPreference {
		return &prefModels.UserNotificationPreference{EmailEnabled: true, EmailDMs: true, UnsubscribeToken: token}
	}
	dm := Event{Type: EventChatDM, Title: "ana sent you a message", SkipOnlineCheck: true, DedupKeyParts: []string{uuid.NewString()}}
	ctx := context.Background()

	if dispatchPrepared(ctx, dm, ben, on("")) || len(queued) != 0 {
		t.Fatalf("a DM with no unsubscribe token was queued (%d)", len(queued))
	}

	if !dispatchPrepared(ctx, dm, ben, on("tok-ben")) || len(queued) != 1 {
		t.Fatalf("a DM with its token wasn't queued (%d)", len(queued))
	}
	var meta map[string]any
	if err := json.Unmarshal(queued[0].Metadata, &meta); err != nil || meta["unsubscribe_token"] != "tok-ben" {
		t.Errorf("the queued DM's token: %v (%v), want tok-ben", meta["unsubscribe_token"], err)
	}

	reminder := Event{Type: EventCalendarReminder, Title: "Stand-up in 10 minutes", Requested: true, SkipOnlineCheck: true, DedupKeyParts: []string{uuid.NewString()}}
	if !dispatchPrepared(ctx, reminder, ben, &prefModels.UserNotificationPreference{}) || len(queued) != 2 {
		t.Fatalf("a reminder they asked for, with no row, wasn't queued (%d)", len(queued))
	}
}
