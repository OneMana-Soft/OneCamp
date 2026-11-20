package domain

import (
	"context"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	readStateModels "github.com/akashc777/OneCamp/models/postgres/ReadState"
)

// Asked is one person asked about one item (a message or a post, by its id)
// of a place (a DM's or group's grouping id, a channel's id), or about a
// Reply (a comment's id) in the item's thread: the email worker's question
// at the send.
type Asked struct {
	User, Place, Item, Reply string
}

// Standing is where an Asked person stands with its item now: their setting
// in the place ("" with none: the live path reads them as not in it), and
// whether the item is live and they haven't read it (models/postgres/
// ReadState's Unread rule: after their marker, live, not their own). With no
// marker they never opened the place, and a live item there is unread. For
// a Reply, the reply is live too and is after their read of the item's
// thread (thread_participants.last_read_at; the thread's id is its item's,
// business/ThreadIndex), or they never opened the thread.
type Standing struct {
	Level  string
	Unread bool
}

// StandingsOf is each of asked's Standing in t's places (t.Levels their
// settings, t.Marker their read markers, t.Rows the items), in its order, in
// one read. One with a malformed user, item or place id stands nowhere: no
// setting, not unread.
func StandingsOf(ctx context.Context, t readStateModels.Table, asked []Asked) ([]Standing, error) {
	out := make([]Standing, len(asked))
	var users, places, items, replies []string
	var at []int
	for i, a := range asked {
		if !wellFormed(a.User) || !wellFormed(a.Item) || (t.KeyOf == "uuid" && !wellFormed(a.Place)) || (a.Reply != "" && !wellFormed(a.Reply)) {
			continue
		}
		users, places, items, replies, at = append(users, a.User), append(places, a.Place), append(items, a.Item), append(replies, a.Reply), append(at, i)
	}
	if len(at) == 0 {
		return out, nil
	}
	type row struct {
		n int
		Standing
	}
	// A conversation with a table of its own (a channel) holds only while it
	// is there: in one deleted, nothing is quoted.
	live, place := "", ""
	if t.Places != "" {
		live, place = "g.id IS NOT NULL AND g.deleted_at IS NULL AND ", `
		LEFT JOIN `+t.Places+` g ON g.id = a.g`
	}
	got, err := pgexec.Query(ctx, `
		SELECT a.n::int, COALESCE(s.notification_type::text, ''),
			COALESCE(`+live+`m.id IS NOT NULL AND m.deleted_at IS NULL AND CASE WHEN a.c = ''
				THEN lsc.user_id IS NULL OR (`+t.Unread()+`)
				ELSE c.id IS NOT NULL AND c.deleted_at IS NULL AND (p.last_read_at IS NULL OR c.created_at > p.last_read_at) END, false)
		FROM unnest($1::uuid[], $2::`+t.KeyOf+`[], $3::uuid[], $4::text[]) WITH ORDINALITY AS a(u, g, r, c, n)
		LEFT JOIN `+t.Levels+` s ON s.user_id = a.u AND s.`+t.Key+` = a.g
		LEFT JOIN `+t.Rows+` m ON m.id = a.r AND m.`+t.RowKey+` = a.g
		LEFT JOIN `+t.Marker+` lsc ON lsc.user_id = a.u AND lsc.`+t.Key+` = a.g
		LEFT JOIN comments c ON c.id = NULLIF(a.c, '')::uuid
		LEFT JOIN thread_participants p ON a.c <> '' AND p.thread_id = a.r AND p.user_uuid = a.u`+place,
		func(r pgexec.Row) (x row, err error) {
			err = r.Scan(&x.n, &x.Level, &x.Unread)
			return x, err
		}, pgexec.Array(users), pgexec.Array(places), pgexec.Array(items), pgexec.Array(replies))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/StandingsOf could not read %d people's standing in %s err: %+v", len(at), t.Rows, err)
		return nil, err
	}
	for _, x := range got {
		out[at[x.n-1]] = x.Standing
	}
	return out, nil
}

func wellFormed(id string) bool {
	_, err := uuid.Parse(id)
	return err == nil
}
