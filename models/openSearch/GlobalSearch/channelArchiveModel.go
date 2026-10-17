package models

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/akashc777/OneCamp/helpers"
)

// channelArchiveScript marks an entry as in an archived channel, or clears
// the mark.
const channelArchiveScript = `if (params.archived) { ctx._source.channel_archived = true; } else if (ctx._source.containsKey('channel_archived')) { ctx._source.remove('channel_archived'); } else { ctx.op = 'noop'; }`

// channelArchiveBody is the update-by-query body that marks (or unmarks) the
// posts, replies and files of a channel. Pure.
func channelArchiveBody(channelUUID string, archived bool) string {
	var clauses []string
	for _, field := range []string{"post_ch_id", "comment_channel_id", "attachment_channel_id"} {
		clauses = append(clauses, exactClauses(field, []string{channelUUID})...)
	}
	script, _ := json.Marshal(map[string]any{
		"source": channelArchiveScript,
		"lang":   "painless",
		"params": map[string]any{"archived": archived},
	})
	return fmt.Sprintf(`{"script": %s, "query": %s}`, script, anyOf(clauses))
}

// MarkChannelArchivedInOpenSearch marks every post, reply and file of a
// channel as in an archived channel (channel_archived), or, archived false,
// clears the mark. Archiving used to mark them deleted, which took a
// channel's history out of search for the people still in it; they find it
// now, marked Archived. Settled, as every update-by-query is.
func MarkChannelArchivedInOpenSearch(ctx context.Context, channelUUID string, archived bool) error {
	if channelUUID == "" {
		return nil
	}
	if _, err := updateByQuerySettled(context.Background(), []string{"posts", "comments", "attachments"}, channelArchiveBody(channelUUID, archived)); err != nil {
		helpers.LogErrorWithContext(ctx, "models/MarkChannelArchivedInOpenSearch failed: %v", err)
		return err
	}
	return nil
}
