package business

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	pollModel "github.com/akashc777/OneCamp/models/postgres/Poll"
	"github.com/google/uuid"
)

// A poll whose message went out but whose link to it failed to save is saved
// again until it lands: a DM's or group's poll is read and voted in through
// that link, and one write that failed orphaned it for good (audit C7).
func TestAPollsLinkToItsMessageIsSavedAgainUntilItLands(t *testing.T) {
	saved := setPost
	t.Cleanup(func() { setPost = saved })
	synctest.Test(t, func(t *testing.T) {
		var tries int
		var linked uuid.UUID
		setPost = func(_ context.Context, _, postID uuid.UUID) error {
			tries++
			if tries < 3 {
				return errors.New("connection reset")
			}
			linked = postID
			return nil
		}
		p := &pollModel.Poll{ID: uuid.New()}
		msg := uuid.New()
		linkPost(context.Background(), p, msg)
		if p.PostID == nil || *p.PostID != msg {
			t.Fatalf("the poll's view names message %v, want %v", p.PostID, msg)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if linked != msg || tries != 3 {
			t.Fatalf("after %d tries the saved link is %v, want %v on the third", tries, linked, msg)
		}
	})
}
