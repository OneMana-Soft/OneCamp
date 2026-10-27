package business

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	importBusiness "github.com/akashc777/OneCamp/business/Import"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	"github.com/akashc777/OneCamp/helpers"
	reviewModel "github.com/akashc777/OneCamp/models/postgres/ImportReview"
	userModel "github.com/akashc777/OneCamp/models/postgres/User"
)

// The access notice waits until the re-check has gone through every old
// import, then tells each active admin once, in counts only, never the demo's
// people, and nobody when there was nothing to re-check.
func TestTheAccessNoticeWaitsForTheRecheckThenTellsTheAdminsOnce(t *testing.T) {
	wasRecorded, wasList, wasAdmins, wasTell := candidatesRecorded, accessList, adminUsers, tell
	defer func() { candidatesRecorded, accessList, adminUsers, tell = wasRecorded, wasList, wasAdmins, wasTell }()
	admin := &userModel.User{Id: uuid.New(), EmailID: "ana@example.com"}
	visitor := &userModel.User{Id: uuid.New(), EmailID: "sam@" + strings.TrimPrefix(helpers.DemoCastEmailDomain, "@")}
	adminUsers = func(context.Context) ([]*userModel.User, error) { return []*userModel.User{admin, visitor}, nil }
	type call struct {
		n  notificationBusiness.Notice
		to []notificationBusiness.NoticeRecipient
	}
	var told []call
	tell = func(_ context.Context, n notificationBusiness.Notice, to []notificationBusiness.NoticeRecipient) []string {
		told = append(told, call{n, to})
		return nil
	}
	recorded := false
	candidatesRecorded = func() (bool, error) { return recorded, nil }
	var imports []importBusiness.ImportAccess
	accessList = func(context.Context) ([]importBusiness.ImportAccess, error) { return imports, nil }
	project := func(restricted int) []importBusiness.ImportAccessProject {
		p := importBusiness.ImportAccessProject{ProjectUUID: uuid.New(), Name: "Launch"}
		p.Restricted = restricted
		return []importBusiness.ImportAccessProject{p}
	}

	if _, err := tellAccess(context.Background()); !errors.Is(err, errNotYet) || len(told) != 0 {
		t.Fatalf("before the reviews are recorded: %v, told %d", err, len(told))
	}
	recorded = true
	imports = []importBusiness.ImportAccess{
		{ImportID: uuid.New(), State: reviewModel.StateRechecking, Projects: project(0)},
		{ImportID: uuid.New(), State: reviewModel.StateRechecked, Projects: project(3)},
	}
	if _, err := tellAccess(context.Background()); !errors.Is(err, errNotYet) || len(told) != 0 {
		t.Fatalf("while an import is re-checked: %v, told %d", err, len(told))
	}

	imports[0].State, imports[0].Reason = reviewModel.StateNeedsAdmin, "no_connection"
	got, err := tellAccess(context.Background())
	if err != nil || got.imports != 2 || got.needYou != 1 {
		t.Fatalf("once every import is re-checked or waits: %+v %v", got, err)
	}
	if len(told) != 1 {
		t.Fatalf("told %d times, want once", len(told))
	}
	n := told[0].n
	// It opens on the card that needs the admin, which the Import tab draws last
	// (the web lands a link to its id however late the card draws).
	if n.Kind != AccessNoticeEvent || n.ResourceKind != AccessNoticeKind || n.Path != "/app/admin?tab=import#import-access" ||
		n.Data["imports"] != "2" || n.Data["need_you"] != "1" || n.Data["restricted"] != "3" {
		t.Errorf("the notice: %+v", n)
	}
	// Its title and words are the card's: "Earlier imports re-checked", and one import that needs you is "it".
	if n.Title != "Earlier imports re-checked against their tools" {
		t.Errorf("its title: %q", n.Title)
	}
	if want := "Each used the connection it was made with, where that still works. 1 needs you: choose what happens to it in Settings, under Import and export."; n.Body != want {
		t.Errorf("its words: %q, want %q", n.Body, want)
	}
	if to := told[0].to; len(to) != 1 || to[0].UserUUID != admin.Id.String() || !to[0].InboxOnly {
		t.Errorf("told %+v, want the admin alone, in their Inbox", to)
	}

	// Two that need you are "each".
	told = nil
	imports[1].State, imports[1].Reason = reviewModel.StateNeedsAdmin, "owner_gone"
	if got, err := tellAccess(context.Background()); err != nil || got.needYou != 2 || len(told) != 1 {
		t.Fatalf("with two waiting: %+v %v, told %d", got, err, len(told))
	}
	if want := "Each used the connection it was made with, where that still works. 2 need you: choose what happens to each in Settings, under Import and export."; told[0].n.Body != want {
		t.Errorf("its words with two waiting: %q, want %q", told[0].n.Body, want)
	}

	told, imports = nil, nil
	if got, err := tellAccess(context.Background()); err != nil || got.imports != 0 || len(told) != 0 {
		t.Errorf("with no old import: %+v %v, told %d", got, err, len(told))
	}
}

// The access notice opens, by its title, only for someone who is an admin now.
func TestTheAccessNoticeOpensOnlyForAnAdmin(t *testing.T) {
	was := activeAdmin
	defer func() { activeAdmin = was }()
	activeAdmin = func(_ context.Context, id string) bool { return id == "admin" }
	names := adminNotice(accessNoticeTitle)
	if readable, _, _ := names(context.Background(), "member", []string{accessNoticeID}); readable[accessNoticeID] {
		t.Error("a member can open the access notice")
	}
	readable, titles, _ := names(context.Background(), "admin", []string{accessNoticeID})
	if !readable[accessNoticeID] || titles[accessNoticeID] != accessNoticeTitle {
		t.Errorf("an admin reads it as %v %q", readable[accessNoticeID], titles[accessNoticeID])
	}
}
