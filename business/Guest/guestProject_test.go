package business

import (
	"context"
	"errors"
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
