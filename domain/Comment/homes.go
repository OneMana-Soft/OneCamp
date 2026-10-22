package domain

// Where replies sit: the post or chat each answers, and that message's
// conversation. The repair of replies a Slack import filed in another
// conversation's thread reads it (business/SlackImport repair.go).

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/models/dgexec"
	"github.com/google/uuid"
)

// Home is where a reply sits.
type Home struct {
	UUID      string     `json:"comment_uuid"`
	Text      string     `json:"comment_text"`
	CreatedAt *time.Time `json:"comment_created_at"`
	DeletedAt *time.Time `json:"comment_deleted_at"`
	By        *struct {
		UUID string `json:"user_uuid"`
	} `json:"comment_by"`
	// Post is the channel post it answers, with its channel.
	Post *struct {
		UUID    string `json:"post_uuid"`
		Channel *struct {
			UUID string `json:"ch_uuid"`
		} `json:"post_channel"`
	} `json:"comment_post"`
	// Chat is the DM's or group's message it answers; Grouping its conversation.
	Chat *struct {
		UUID string `json:"chat_uuid"`
	} `json:"comment_chat"`
	Grouping string `json:"comment_chat_grouping_id"`
}

// homesBatch is how many replies one read asks about.
const homesBatch = 500

// Homes is where each of the replies named sits, by uuid, read a few
// hundred at a time. A reply the graph doesn't hold is absent.
func Homes(ctx context.Context, ids []uuid.UUID) (map[string]Home, error) {
	out := make(map[string]Home, len(ids))
	for start := 0; start < len(ids); start += homesBatch {
		end := min(start+homesBatch, len(ids))
		page, err := dgexec.Read[struct {
			Homes []Home `json:"homes"`
		}](ctx, `{
			homes(func: eq(comment_uuid, [`+dgraphquery.UUIDListOf(ids[start:end])+`])) {
				comment_uuid comment_text comment_created_at comment_deleted_at
				comment_by { user_uuid }
				comment_post { post_uuid post_channel { ch_uuid } }
				comment_chat { chat_uuid }
				comment_chat_grouping_id
			}
		}`, nil)
		if err != nil {
			return nil, err
		}
		for _, h := range page.Homes {
			out[h.UUID] = h
		}
	}
	return out, nil
}
