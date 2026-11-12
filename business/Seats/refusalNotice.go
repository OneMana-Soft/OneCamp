package business

// When the free plan's limit refuses someone, the admins hear of it.
//
// A joiner who meets the limit is told why (helpers.SeatLimitError), but
// nobody else was: an admin found out when someone asked why they couldn't
// get in. Now every admin is told in their Inbox, once a day for each person,
// with the way to the plan. Only someone's own way in tells (sign-up, an
// invitation, SSO's first sign-in, SCIM: business/User joinAsMember); an
// admin's own Activate or invitation meets the limit in front of them.

import (
	"context"
	"strconv"
	"time"

	followBusiness "github.com/akashc777/OneCamp/business/Follow"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	userModel "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// The Inbox notice: its kind, and the kind of thing it is about (a person who
// couldn't join, by an id made from their address).
const (
	NoticeEvent = "member.seat_refused"
	NoticeKind  = "seat_refused"
)

// refusedNamespace makes a person's id from their address, so each person is
// one resource however often, and in whatever case, they try.
var refusedNamespace = uuid.MustParse("6f1c9a52-3b7e-4d0a-9c1e-5a8b2f4d7e30")

func init() {
	userBusiness.OnSeatRefused(TellAdminsSeatRefused)
	followBusiness.RegisterNoticeKind(NoticeKind, noticeNames)
}

// Seams for tests: the admins, telling them, the clock, and whether a reader
// is an admin now.
var (
	adminUsers = func(ctx context.Context) ([]*userModel.User, error) {
		users, _, err := userDomain.GetAllAdminUsers(ctx, 0, 200)
		return users, err
	}
	tell        = notificationBusiness.Tell
	now         = time.Now
	activeAdmin = userDomain.IsActiveAdminUUID
)

// TellAdminsSeatRefused tells every admin that email couldn't join because
// the plan is full, and answers whether it did (or had, earlier that day: the
// notice is written once). Nothing on the demo, whose people are its own; a
// failure is logged, never the joiner's to see.
func TellAdminsSeatRefused(ctx context.Context, email, method string) bool {
	limit := helpers.MemberSeatLimit()
	email = helpers.NormalizeEmail(email)
	if helpers.DemoMode() || limit <= 0 || email == "" {
		return false
	}
	users, err := adminUsers(ctx)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Seats: who the admins are couldn't be read, so a seat refusal for %s goes untold: %+v", method, err)
		return false
	}
	recipients := notificationBusiness.InboxAdmins(users)
	if len(recipients) == 0 {
		return false
	}
	person := uuid.NewSHA1(refusedNamespace, []byte(email)).String()
	line := "Remove the " + strconv.Itoa(limit) + "-person limit, or free a place."
	tell(ctx, notificationBusiness.Notice{
		Kind:         NoticeEvent,
		ResourceKind: NoticeKind,
		ResourceUUID: person,
		// The address is shown only to a reader who may open the notice now
		// (an admin); the line under it is in the server's words, with the
		// plan's own figure, so the Inbox says it without a number of its own.
		Data:     map[string]string{notificationBusiness.NoticeWords: email, "limit": strconv.Itoa(limit), "line": line},
		DedupKey: "seat_refused:" + person + ":" + now().UTC().Format(time.DateOnly),
		Title:    email + " couldn't join: your free plan is full",
		Body:     line,
		Path:     "/app/admin?tab=plan",
		// What it is about, as the Inbox read back names it (noticeNames).
		ResourceTitle: refusalTitle,
	}, recipients)
	return true
}

// refusalTitle is what a seat refusal is about, to an admin.
const refusalTitle = "Your free plan"

// noticeNames is how an Inbox names a seat refusal: "Your free plan", and
// openable only by someone who is an admin now.
func noticeNames(ctx context.Context, readerUUID string, ids []string) (map[string]bool, map[string]string, error) {
	readable, titles := map[string]bool{}, map[string]string{}
	if !activeAdmin(ctx, readerUUID) {
		return readable, titles, nil
	}
	for _, id := range ids {
		readable[id] = true
		titles[id] = refusalTitle
	}
	return readable, titles, nil
}
