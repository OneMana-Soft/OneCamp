package business

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/akashc777/OneCamp/services/collab"
)

// A link's end, as an open live view is told it: its own end or the cap's,
// whichever comes first; a standing link's own alone; none for neither.
func TestALinksEndIsTheSoonerOfItsOwnAndTheCaps(t *testing.T) {
	ctx := context.Background()
	made := time.Now().Add(-10 * day)
	own, late, capped := made.Add(5*day), made.Add(90*day), made.Add(30*day)
	link := func(ends *time.Time) *guestModel.GuestGrant {
		return &guestModel.GuestGrant{ResourceType: guestModel.ResourceDoc, CreatedAt: made, ExpiresAt: ends}
	}
	is := func(got *time.Time, want time.Time) bool { return got != nil && got.Equal(want) }

	withCap(t, 0)
	if got := LinkEnds(ctx, link(nil)); got != nil {
		t.Fatalf("no end and no cap: it ends at %v", got)
	}
	if got := LinkEnds(ctx, link(&own)); !is(got, own) {
		t.Fatalf("no cap: it ends at %v, want its own end", got)
	}
	withCap(t, 30)
	if got := LinkEnds(ctx, link(&late)); !is(got, capped) {
		t.Fatalf("its own end after the cap's: it ends at %v, want the cap's", got)
	}
	if got := LinkEnds(ctx, link(&own)); !is(got, own) {
		t.Fatalf("its own end before the cap's: it ends at %v, want its own", got)
	}
	if got := LinkEnds(ctx, link(nil)); !is(got, capped) {
		t.Fatalf("no end of its own: it ends at %v, want the cap's", got)
	}
	slug := "dana"
	room := &guestModel.GuestGrant{ResourceType: guestModel.ResourceMeeting, Slug: &slug, CreatedAt: made}
	if got := LinkEnds(ctx, room); got != nil {
		t.Fatalf("a standing room is never capped: it ends at %v", got)
	}
}

// Guest access turned off: every live doc and board link's guests lose their
// live view at once (guest-<grant id> on the collaboration service), and
// nothing is asked for a meeting's link, which opens none.
func TestTurningGuestAccessOffEndsEveryLinksLiveView(t *testing.T) {
	doc, board, meeting := uuid.New(), uuid.New(), uuid.New()
	prevGrants, prevClose := activeGrants, collab.ClosePeople
	t.Cleanup(func() { activeGrants, collab.ClosePeople = prevGrants, prevClose })
	activeGrants = func(context.Context) ([]*guestModel.GuestGrant, error) {
		return []*guestModel.GuestGrant{
			{Id: doc, ResourceType: guestModel.ResourceDoc},
			{Id: board, ResourceType: guestModel.ResourceBoard},
			{Id: meeting, ResourceType: guestModel.ResourceMeeting},
		}, nil
	}
	asked := make(chan []string, 1)
	collab.ClosePeople = func(_ context.Context, ids []string) error {
		asked <- ids
		return nil
	}
	endLinkSessions(context.Background())
	select {
	case ids := <-asked:
		if want := []string{"guest-" + doc.String(), "guest-" + board.String()}; !slices.Equal(ids, want) {
			t.Fatalf("closed the live views of %v, want %v", ids, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("turning guest access off closed no live view")
	}
}
