//go:build integration
// +build integration

package integration_test

// "Catch me up" recaps what the reader saw unread as they opened the
// conversation, against a real Postgres.
//
// Opening a channel or a conversation moves its last-seen to now, and the
// recap read that same last-seen afterwards: it found nothing, said so, and
// the banner went away for an hour (SA-BUG-7). The client now says how many
// were unread as it opened, and the recap starts just before the oldest of
// them: the reader's own messages and deleted ones don't count, as they don't
// in the unread count. The recap keeps what is newer than a whole second.
//
// Run: go test -tags=integration ./tests/integration/ -run TestCatchUpStartsAtWhatWasUnread -v

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	lastSeenChannelDomain "github.com/akashc777/OneCamp/domain/LastSeenChannel"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestCatchUpStartsAtWhatWasUnread(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	me, maya := uuid.New(), uuid.New()
	for id, name := range map[uuid.UUID]string{me: "me", maya: "maya"} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, name+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
	}
	ch := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, 'launch', false)`, ch); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Second)
	at := func(m int) time.Time { return base.Add(time.Duration(m) * time.Minute) }
	post := func(by uuid.UUID, m int, deleted bool) {
		var deletedAt any
		if deleted {
			deletedAt = at(m).Add(time.Second)
		}
		if _, err := env.PG.Exec(`INSERT INTO posts (id, post_channel, created_by, created_at, deleted_at) VALUES ($1, $2, $3, $4, $5)`,
			uuid.New(), ch, by, at(m), deletedAt); err != nil {
			t.Fatal(err)
		}
	}
	// Twenty from Maya a minute apart; three of the reader's own and one of
	// Maya's, deleted, among the newest.
	for m := 0; m < 20; m++ {
		post(maya, m, false)
	}
	post(me, 15, false)
	post(me, 17, false)
	post(me, 19, false)
	post(maya, 18, true)
	// Opening the channel moved last-seen to now.
	if _, err := env.PG.Exec(`INSERT INTO last_seen_channel (user_id, channel_id, user_last_seen) VALUES ($1, $2, NOW())`, me, ch); err != nil {
		t.Fatal(err)
	}

	boundary, found, err := lastSeenChannelDomain.UnreadBoundaryInChannel(ctx, me, ch, 12)
	if err != nil || !found {
		t.Fatalf("finding where 12 unread start: %v, %v", found, err)
	}
	// The recap keeps what is newer than a second before the oldest unread.
	since := boundary.Add(-time.Second).Unix()
	var recapped int
	if err := env.PG.QueryRow(`SELECT count(*) FROM posts
		WHERE post_channel = $1 AND created_by <> $2 AND deleted_at IS NULL AND EXTRACT(EPOCH FROM created_at)::bigint > $3`,
		ch, me, since).Scan(&recapped); err != nil {
		t.Fatal(err)
	}
	if recapped != 12 {
		t.Errorf("12 unread on opening recap %d messages from others; want exactly those 12", recapped)
	}
	if !boundary.Equal(at(8)) {
		t.Errorf("the 12 unread start at %v; want Maya's message at minute 8", boundary)
	}
	// More than the channel holds: no boundary, and the recap keeps last-seen.
	if _, found, err := lastSeenChannelDomain.UnreadBoundaryInChannel(ctx, me, ch, 50); err != nil || found {
		t.Errorf("50 unread in a channel of 20: found %v, err %v; want none", found, err)
	}
}
