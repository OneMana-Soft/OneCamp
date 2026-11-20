package domain

import (
	"context"

	"github.com/akashc777/OneCamp/models/pgexec"
	followModels "github.com/akashc777/OneCamp/models/postgres/Follow"
)

// PageSettings is what a page of a reader's Inbox asks of Postgres
// (ReaderPageSettings): which of its told-mention keys name a notice the inbox
// shows (Held, by the key as asked), and the reader's own setting for each of
// its channels (ByChannel) and conversations (ByChat), each by lower-case id.
// A place with no setting is in neither.
type PageSettings struct {
	Held              map[string]bool
	ByChannel, ByChat map[string]string
}

// pageRow is a row of readerPageSettings. Its kind says which select it came
// from: "held" (a notice) or "level" (a setting), and a setting's place whose
// it is: "channel" or "chat". pageSettingsOf splits the rows on these alone.
type pageRow struct{ kind, place, id, level string }

// readerPageSettings is the page's one statement: the held notices among the
// stored keys ($2), as followModels.ReasonListed ($3) leaves a listed notice
// unshown, then reader's ($1) settings for the page's channels ($4) and its
// conversations ($5, by lower-case id). Each select reads by its own index.
const readerPageSettings = `
	SELECT 'held', '', dedup_key::text, ''
	FROM resource_notices
	WHERE dedup_key = ANY($2::text[]) AND reason <> $3
	UNION ALL
	SELECT 'level', 'channel', channel_id::text, notification_type::text
	FROM users_channel_notification
	WHERE user_id = $1::uuid AND channel_id = ANY($4::uuid[])
	UNION ALL
	SELECT 'level', 'chat', lower(grp_id), notification_type::text
	FROM users_chat_notification
	WHERE user_id = $1::uuid AND grp_id = ANY($5::text[])`

// ReaderPageSettings reads what a page of reader's Inbox asks (PageSettings)
// in one statement: keys are its told mentions' keys (each a telling's key and
// reader's id, as business/Notification Tell writes them), channelIDs and
// grpIDs its places. Nothing is read when the page has none of them. A listed
// notice isn't held; a retracted one is (what it told of was deleted, and the
// feed keeps the graph's mention of it left out).
func ReaderPageSettings(ctx context.Context, reader string, keys, channelIDs, grpIDs []string) (PageSettings, error) {
	if len(keys) == 0 && len(channelIDs) == 0 && len(grpIDs) == 0 {
		return pageSettingsOf(nil, nil), nil
	}
	stored, asked := followModels.StoredKeys(keys)
	rows, err := pgexec.Query(ctx, readerPageSettings, func(r pgexec.Row) (p pageRow, err error) {
		err = r.Scan(&p.kind, &p.place, &p.id, &p.level)
		return p, err
	}, reader, pgexec.Array(stored), followModels.ReasonListed, pgexec.Array(channelIDs), pgexec.Array(grpIDs))
	if err != nil {
		return PageSettings{}, err
	}
	return pageSettingsOf(rows, asked), nil
}

// pageSettingsOf is the rows of readerPageSettings as PageSettings, split on
// each row's kind and place, never on its shape: a held row names a notice by
// its stored key (asked, back to the key as asked), a level row a place.
func pageSettingsOf(rows []pageRow, asked map[string]string) PageSettings {
	s := PageSettings{Held: map[string]bool{}, ByChannel: map[string]string{}, ByChat: map[string]string{}}
	for _, r := range rows {
		switch {
		case r.kind == "held":
			if k, ok := asked[r.id]; ok {
				s.Held[k] = true
			}
		case r.kind == "level" && r.place == "channel":
			s.ByChannel[r.id] = r.level
		case r.kind == "level" && r.place == "chat":
			s.ByChat[r.id] = r.level
		}
	}
	return s
}
