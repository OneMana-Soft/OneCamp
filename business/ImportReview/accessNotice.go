package business

// The admins' one Inbox notice about old imports' access (business/Import
// recheck.go): once the re-check has gone through every old import with the
// connection each was made with (ruling B), it tells each active admin how
// many it re-checked and how many need them, in counts only, never an issue
// or comment title, and sends them to the card in Settings, under Import and
// export.

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/google/uuid"

	followBusiness "github.com/akashc777/OneCamp/business/Follow"
	importBusiness "github.com/akashc777/OneCamp/business/Import"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	repairBusiness "github.com/akashc777/OneCamp/business/Repair"
	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
	reviewModel "github.com/akashc777/OneCamp/models/postgres/ImportReview"
)

// The access notice, its kind, and its title in the Inbox.
const (
	AccessNoticeEvent = "import.access_rechecked"
	AccessNoticeKind  = "import_access_review"
	accessNoticeTitle = "Earlier imports re-checked against their tools" // the card is "Earlier imports re-checked"
)

// accessNoticeID names the one access notice an install's admins get.
var accessNoticeID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("onecamp.import-access-review")).String()

// Seams for tests: whether the re-check's reviews are recorded yet, and the
// card's list the notice counts.
var (
	candidatesRecorded = func() (bool, error) { return configModel.IsDone(importBusiness.RecheckCandidates.Key) }
	accessList         = importBusiness.ImportAccessList
)

// errNotYet is a notice with nothing settled to tell yet: the reviews aren't
// recorded, or an import is still being re-checked. Its Once tries again.
var errNotYet = errors.New("the old imports' re-check hasn't gone through them yet")

func init() {
	followBusiness.RegisterNoticeKind(AccessNoticeKind, adminNotice(accessNoticeTitle))
}

// AccessNotice tells the admins, once per install, what the re-check of old
// imports' access did, as soon as it has gone through every one.
var AccessNotice = repairBusiness.Once[accessTold]{
	Key:    "imports.access_review_notice",
	Name:   "import.access-review-notice",
	Tries:  48,
	Repair: tellAccess,
}

// accessTold is what the notice said: how many imports, and how many need an
// admin.
type accessTold struct{ imports, needYou int }

func (t accessTold) String() string {
	if t.imports == 0 {
		return "no old import to tell the admins about"
	}
	return fmt.Sprintf("told the admins about %d old imports re-checked, %d needing them", t.imports, t.needYou)
}

// tellAccess tells the admins once every old import has been re-checked or
// waits for them, with its counts. With none, it tells nobody.
func tellAccess(ctx context.Context) (accessTold, error) {
	var told accessTold
	if done, err := candidatesRecorded(); err != nil || !done {
		return told, errors.Join(errNotYet, err)
	}
	imports, err := accessList(ctx)
	if err != nil {
		return told, err
	}
	restricted := 0
	for _, imp := range imports {
		if imp.State == reviewModel.StateRechecking {
			return accessTold{}, errNotYet
		}
		told.imports++
		if imp.State == reviewModel.StateNeedsAdmin {
			told.needYou++
		}
		for _, p := range imp.Projects {
			restricted += p.Restricted
		}
	}
	if told.imports == 0 {
		return told, nil
	}
	users, err := adminUsers(ctx)
	if err != nil {
		return told, err
	}
	recipients := notificationBusiness.InboxAdmins(users)
	if len(recipients) == 0 {
		return told, nil
	}
	body := "Each used the connection it was made with, and kept " + tasks(restricted) + " to fewer people. See Settings, under Import and export."
	if told.needYou > 0 {
		// In the card's words: one import "needs you" and is "it"; more "need you", "each".
		waiting := "1 needs you: choose what happens to it"
		if told.needYou > 1 {
			waiting = strconv.Itoa(told.needYou) + " need you: choose what happens to each"
		}
		body = "Each used the connection it was made with, where that still works. " + waiting + " in Settings, under Import and export."
	}
	tell(ctx, notificationBusiness.Notice{
		Kind:          AccessNoticeEvent,
		ResourceKind:  AccessNoticeKind,
		ResourceUUID:  accessNoticeID,
		ResourceTitle: accessNoticeTitle,
		Data: map[string]string{"imports": strconv.Itoa(told.imports), "need_you": strconv.Itoa(told.needYou),
			"restricted": strconv.Itoa(restricted)},
		DedupKey: "import-access-review",
		Title:    accessNoticeTitle,
		Body:     body,
		// The card that needs the admin, which the Import tab draws last: the web
		// lands a link to its id however late it draws.
		Path: "/app/admin?tab=import#import-access",
	}, recipients)
	return told, nil
}

func tasks(n int) string {
	if n == 1 {
		return "1 task"
	}
	return strconv.Itoa(n) + " tasks"
}
