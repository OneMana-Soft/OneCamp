package business

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	pollModel "github.com/akashc777/OneCamp/models/postgres/Poll"
	"github.com/google/uuid"
)

// setPost writes which message a poll was posted as; a seam for the test.
var setPost = pollModel.SetPost

// linkPostRetries is how patiently a poll's message is written down when the
// first write fails: six more tries, doubling from a second, over about a minute.
var linkPostRetries = helpers.Retries{First: time.Second, Cap: 30 * time.Second, Tries: 6}

// linkPost records postID as the message p was posted as. The message has
// already gone out, so a failed write is no reason to tell the sender the poll
// failed (they would post it again): it is tried again in the background, and
// each failure is logged. Until it lands, a DM's or group's poll can't be read
// or voted in (conversationOf goes through its message); it was left that way
// for good, with nothing logged (audit C7).
func linkPost(ctx context.Context, p *pollModel.Poll, postID uuid.UUID) {
	p.PostID = &postID
	write := func(ctx context.Context) error {
		err := setPost(ctx, p.ID, postID)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/Poll linkPost: poll %s's message %s: %+v", p.ID, postID, err)
		}
		return err
	}
	if write(ctx) == nil {
		return
	}
	helpers.RetryInBackground(context.WithoutCancel(ctx), "poll.link-post", linkPostRetries, write)
}
