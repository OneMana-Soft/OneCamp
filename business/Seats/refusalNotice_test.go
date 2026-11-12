package business

import (
	"context"
	"errors"
	"testing"
	"time"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	"github.com/akashc777/OneCamp/helpers"
	userModel "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

type told struct {
	n  notificationBusiness.Notice
	to []notificationBusiness.NoticeRecipient
}

func withSeams(t *testing.T, admins []*userModel.User) (*[]told, *time.Time) {
	t.Helper()
	oldAdmins, oldTell, oldNow, oldLimit := adminUsers, tell, now, helpers.SeatLimit
	t.Cleanup(func() { adminUsers, tell, now, helpers.SeatLimit = oldAdmins, oldTell, oldNow, oldLimit })
	helpers.SeatLimit = "25"
	t.Setenv("DEMO_MODE", "")
	var got []told
	adminUsers = func(context.Context) ([]*userModel.User, error) { return admins, nil }
	tell = func(_ context.Context, n notificationBusiness.Notice, to []notificationBusiness.NoticeRecipient) []string {
		got = append(got, told{n, to})
		return nil
	}
	clock := time.Date(2026, 11, 9, 21, 0, 0, 0, time.UTC)
	now = func() time.Time { return clock }
	return &got, &clock
}

func admin(email string) *userModel.User {
	return &userModel.User{Id: uuid.New(), EmailID: email}
}

// Someone's own way in met the free plan's limit: every admin hears of it in
// their Inbox, once a day for that person, with the way to the plan.
func TestSeatRefusalTellsEachAdminOncePerDay(t *testing.T) {
	a, b := admin("owner@acme.test"), admin("it@acme.test")
	got, clock := withSeams(t, []*userModel.User{a, b})
	if !TellAdminsSeatRefused(context.Background(), " Ana@Example.com ", "invitation") {
		t.Error("the admins were told, so the joiner may hear so")
	}
	if len(*got) != 1 {
		t.Fatalf("told %d times, want once", len(*got))
	}
	n, to := (*got)[0].n, (*got)[0].to
	if n.Kind != NoticeEvent || n.ResourceKind != NoticeKind {
		t.Errorf("kind %q/%q", n.Kind, n.ResourceKind)
	}
	person := uuid.NewSHA1(refusedNamespace, []byte("ana@example.com")).String()
	if n.ResourceUUID != person {
		t.Errorf("one resource per person, whatever the case they typed: %q", n.ResourceUUID)
	}
	// By the person's id, not their address: a key has room for an id, not
	// for every address someone might type.
	if n.DedupKey != "seat_refused:"+person+":2026-11-09" {
		t.Errorf("once a day per person: %q", n.DedupKey)
	}
	if n.Data[notificationBusiness.NoticeWords] != "ana@example.com" || n.Data["limit"] != "25" ||
		n.Data["line"] != "Remove the 25-person limit, or free a place." {
		t.Errorf("the address, shown only to whoever may open it, and the plan's limit: %v", n.Data)
	}
	if n.ResourceTitle != "Your free plan" {
		t.Errorf("the live item names it as the Inbox read back does: %q", n.ResourceTitle)
	}
	if n.Path != "/app/admin?tab=plan" || n.Title != "ana@example.com couldn't join: your free plan is full" ||
		n.Body != "Remove the 25-person limit, or free a place." {
		t.Errorf("what it says and where it opens: %q %q %q", n.Title, n.Body, n.Path)
	}
	if len(to) != 2 || to[0].UserUUID != a.Id.String() || !to[0].InboxOnly {
		t.Errorf("each admin, in the Inbox: %+v", to)
	}

	// The next day the same person is a new telling.
	*clock = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	TellAdminsSeatRefused(context.Background(), "ana@example.com", "sso")
	if k := (*got)[1].n.DedupKey; k != "seat_refused:"+person+":2026-11-10" {
		t.Errorf("a new day, a new key: %q", k)
	}
}

// Only someone who is an admin now can read it: anyone else's read of the
// same notice opens nothing and names nobody.
func TestSeatRefusalNoticeHiddenFromNonAdmin(t *testing.T) {
	old := activeAdmin
	t.Cleanup(func() { activeAdmin = old })
	ids := []string{uuid.NewSHA1(refusedNamespace, []byte("ana@example.com")).String()}

	activeAdmin = func(context.Context, string) bool { return false }
	readable, titles, err := noticeNames(context.Background(), uuid.NewString(), ids)
	if err != nil || len(readable) != 0 || len(titles) != 0 {
		t.Fatalf("a member: readable %v, titles %v, err %v", readable, titles, err)
	}

	activeAdmin = func(context.Context, string) bool { return true }
	readable, titles, _ = noticeNames(context.Background(), uuid.NewString(), ids)
	if !readable[ids[0]] || titles[ids[0]] != "Your free plan" {
		t.Fatalf("an admin: readable %v, titles %v", readable, titles)
	}
}

// The demo's people are the demo's: nobody is told of a refusal there, and a
// workspace with no limit has none to tell of.
func TestSeatRefusalQuietInDemo(t *testing.T) {
	got, _ := withSeams(t, []*userModel.User{admin("owner@acme.test")})
	t.Setenv("DEMO_MODE", "true")
	if TellAdminsSeatRefused(context.Background(), "ana@example.com", "signup") || len(*got) != 0 {
		t.Fatalf("the demo told %d", len(*got))
	}
	t.Setenv("DEMO_MODE", "")
	helpers.SeatLimit = ""
	if TellAdminsSeatRefused(context.Background(), "ana@example.com", "signup") || len(*got) != 0 {
		t.Fatalf("no limit, nothing to tell: told %d", len(*got))
	}
}

// With nobody to tell, the joiner isn't told that anybody was.
func TestSeatRefusalWithNoAdminTellsNobody(t *testing.T) {
	got, _ := withSeams(t, nil)
	if TellAdminsSeatRefused(context.Background(), "ana@example.com", "signup") || len(*got) != 0 {
		t.Fatalf("no admin, yet told %d", len(*got))
	}
}

// When who the admins are can't be read, nobody was told, so the joiner's line
// never says they were (V-907's "told" rests on this answer).
func TestSeatRefusalWithAdminsUnreadTellsNobody(t *testing.T) {
	got, _ := withSeams(t, []*userModel.User{admin("owner@acme.test")})
	adminUsers = func(context.Context) ([]*userModel.User, error) { return nil, errors.New("graph down") }
	if TellAdminsSeatRefused(context.Background(), "ana@example.com", "signup") || len(*got) != 0 {
		t.Fatalf("the admins couldn't be read, yet told %d", len(*got))
	}
}
