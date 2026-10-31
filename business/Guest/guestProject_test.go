package business

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/google/uuid"
)

func TestStatusLabel(t *testing.T) {
	custom, blank := "Waiting on client", "  "
	cases := []struct {
		status string
		custom *string
		want   string
	}{
		{dgraphStruct.TASK_STATUS_INPROGRESS, nil, "In progress"},
		{dgraphStruct.TASK_STATUS_INREVIEW, &custom, "Waiting on client"},
		{dgraphStruct.TASK_STATUS_TODO, &blank, "To do"},
		{"somethingNew", nil, "somethingNew"},
	}
	for _, c := range cases {
		if got := StatusLabel(c.status, c.custom); got != c.want {
			t.Errorf("StatusLabel(%q) = %q, want %q", c.status, got, c.want)
		}
	}
}

// A client never sees cancelled work: it is not one of the board's columns.
func TestGuestBoardLeavesOutCancelled(t *testing.T) {
	for _, s := range guestStatuses {
		if s.status == dgraphStruct.TASK_STATUS_CANCELED {
			t.Fatal("cancelled tasks must not be shown to a project's guest")
		}
	}
	if len(guestStatuses) != 5 {
		t.Fatalf("want backlog, to do, in progress, in review and done; got %d columns", len(guestStatuses))
	}
}

// A grant for anything but a project opens no project, and a view link can't
// comment, both before anything is read.
func TestProjectGrantScope(t *testing.T) {
	ctx := context.Background()
	channel := &guestModel.GuestGrant{ResourceType: guestModel.ResourceChannel, ResourceID: uuid.NewString(), Capability: guestModel.CapabilityPost}
	if _, err := GetGuestProject(ctx, channel); !errors.Is(err, ErrForbidden) {
		t.Errorf("a channel link opened a project: %v", err)
	}
	view := &guestModel.GuestGrant{ResourceType: guestModel.ResourceProject, ResourceID: uuid.NewString(), Capability: guestModel.CapabilityView}
	if err := CommentAsGuest(ctx, view, uuid.NewString(), "Priya", "hello"); !errors.Is(err, ErrForbidden) {
		t.Errorf("a view link commented: %v", err)
	}
	comment := &guestModel.GuestGrant{ResourceType: guestModel.ResourceProject, ResourceID: uuid.NewString(), Capability: guestModel.CapabilityComment}
	var in *ErrGuestInput
	if err := CommentAsGuest(ctx, comment, uuid.NewString(), "", "hello"); !errors.As(err, &in) {
		t.Errorf("a nameless comment was accepted: %v", err)
	}
}

func TestDateOfHidesUnsetDates(t *testing.T) {
	zero, epoch, real := time.Time{}, time.Unix(0, 0).UTC(), time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	if dateOf(nil) != nil || dateOf(&zero) != nil || dateOf(&epoch) != nil {
		t.Error("an unset date showed as a date")
	}
	if dateOf(&real) == nil {
		t.Error("a real date was hidden")
	}
}

// The client's timeline draws a card from its start to its due date, so the
// card carries both, and an unset one stays out.
func TestCardCarriesBothDates(t *testing.T) {
	start, due, zero := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC), time.Date(2026, 10, 9, 17, 0, 0, 0, time.UTC), time.Time{}
	c := cardOf(&dgraphStruct.DgraphTask{Uuid: "t", Name: "Plan", Status: "todo", StartDate: &start, DueDate: &due})
	if c.StartDate == nil || !c.StartDate.Equal(start) || c.DueDate == nil || !c.DueDate.Equal(due) {
		t.Fatalf("the card's dates: %v %v", c.StartDate, c.DueDate)
	}
	if c := cardOf(&dgraphStruct.DgraphTask{Uuid: "t", Status: "todo", StartDate: &zero, DueDate: &due}); c.StartDate != nil {
		t.Fatalf("an unset start date showed: %v", c.StartDate)
	}
}

func TestReviewText(t *testing.T) {
	if text, _, err := ReviewText("approved", ""); err != nil || text != "Approved." {
		t.Errorf("approve: %q %v", text, err)
	}
	if text, note, err := ReviewText("changes", "  Make the logo bigger "); err != nil || text != "Changes requested: Make the logo bigger" || note != "Make the logo bigger" {
		t.Errorf("changes: %q %q %v", text, note, err)
	}
	var in *ErrGuestInput
	for _, c := range [][2]string{{"changes", ""}, {"maybe", "x"}, {"approved", strings.Repeat("x", 2001)}} {
		if _, _, err := ReviewText(c[0], c[1]); !errors.As(err, &in) {
			t.Errorf("%q: want an input error, got %v", c, err)
		}
	}
}

// A view-only link can't give a verdict.
func TestReviewNeedsTheCommentLink(t *testing.T) {
	view := &guestModel.GuestGrant{ResourceType: guestModel.ResourceProject, ResourceID: uuid.NewString(), Capability: guestModel.CapabilityView}
	if _, err := ReviewAsGuest(context.Background(), view, uuid.NewString(), "Priya", "approved", ""); !errors.Is(err, ErrForbidden) {
		t.Errorf("a view link reviewed: %v", err)
	}
}

// What a client is asked never holds their board: with approvals out of the
// build, after a failed read, or for a link only to view, which can't answer
// so is asked nothing and costs no read.
func TestWhatAClientIsAskedNeverHoldsTheirBoard(t *testing.T) {
	old := ClientAsks
	t.Cleanup(func() { ClientAsks = old })
	ctx, project := context.Background(), uuid.New()
	comment := &guestModel.GuestGrant{Id: uuid.New(), Capability: guestModel.CapabilityComment}
	view := &guestModel.GuestGrant{Id: uuid.New(), Capability: guestModel.CapabilityView}

	ClientAsks = nil
	if got := asksFor(ctx, comment, project); got != nil {
		t.Errorf("without approvals: %+v", got)
	}
	reads := 0
	ClientAsks = func(_ context.Context, grant, p uuid.UUID) (map[string]ClientAsk, error) {
		reads++
		if grant != comment.Id || p != project {
			t.Errorf("read link %s in project %s", grant, p)
		}
		return nil, errors.New("postgres is down")
	}
	if got := asksFor(ctx, comment, project); got != nil || reads != 1 {
		t.Errorf("a failed read: %+v after %d reads", got, reads)
	}
	if got := asksFor(ctx, view, project); got != nil || reads != 1 {
		t.Errorf("a link only to view: %+v after %d reads", got, reads)
	}
}

// A task's ask reads as its client reads the team: who asked by the name
// rule without their address, and "Someone" when they couldn't be read.
func TestATasksAskReadsAsItsClientReadsTheTeam(t *testing.T) {
	due := time.Date(2026, 10, 30, 11, 30, 0, 0, time.UTC)
	at := due.Add(-48 * time.Hour)
	asks := map[string]ClientAsk{
		"layout": {Ask: "The layout", DueAt: &due, AskedAt: at, AskedBy: &dgraphStruct.DgraphUser{UserFullName: "Ben Ito", EmailID: "ben@acme.test"}},
		"move":   {ToName: "Client sign-off", AskedAt: at},
	}
	a := waitingOf(asks, "layout")
	if a == nil || a.Ask != "The layout" || a.DueAt == nil || !a.DueAt.Equal(due) || !a.AskedAt.Equal(at) || a.AskedBy != "Ben Ito" || a.ToLabel != "" {
		t.Errorf("the ask: %+v", a)
	}
	if a := waitingOf(asks, "move"); a == nil || a.ToLabel != "Client sign-off" || a.AskedBy != "Someone" {
		t.Errorf("the held move: %+v", a)
	}
	if a := waitingOf(asks, "other"); a != nil {
		t.Errorf("a task nothing waits on: %+v", a)
	}
}
