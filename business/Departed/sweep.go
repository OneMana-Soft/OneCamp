package business

import (
	"context"
	"strconv"
	"strings"

	followBusiness "github.com/akashc777/OneCamp/business/Follow"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	userModel "github.com/akashc777/OneCamp/models/postgres/User"
)

// The Inbox notice an admin gets about someone who left with work under their
// name: its kind, and the kind of thing it is about.
const (
	NoticeEvent = "member.left_work"
	NoticeKind  = "departed_member"
)

// SweepName is the sweep that tells admins.
const SweepName = "guest.link.departed"

func init() {
	// RELEASE-LINE ONLY (v2.75.0 / v1.60.0): the sweep isn't registered. Its
	// notice sends admins to Settings > Docs, which this release's web hasn't
	// got (docs C6-2's web); beta registers it here, and the release that
	// carries that web registers it again (releaseLine_test.go holds it).
	followBusiness.RegisterNoticeKind(NoticeKind, noticeNames)
}

// Seams for tests: the admins, telling them, and whether a reader is one.
var (
	adminUsers = func(ctx context.Context) ([]*userModel.User, error) {
		users, _, err := userDomain.GetAllAdminUsers(ctx, 0, 200)
		return users, err
	}
	tell        = notificationBusiness.Tell
	activeAdmin = userDomain.IsActiveAdminUUID
)

// TellAdmins tells each admin, once in their Inbox, about each person who
// left with docs or links still under their name. Each telling has its own
// key (Tell writes a notice once per key and person), so a pass that finds the
// same person again tells nobody twice. People of the demo's own are left out
// (List), and so are admins of it.
func TellAdmins(ctx context.Context) error {
	people, err := List(ctx)
	if err != nil || len(people) == 0 {
		return err
	}
	users, err := adminUsers(ctx)
	if err != nil {
		return err
	}
	recipients := notificationBusiness.InboxAdmins(users)
	if len(recipients) == 0 {
		return nil
	}
	for _, p := range people {
		tell(ctx, notificationBusiness.Notice{
			Kind:          NoticeEvent,
			ResourceKind:  NoticeKind,
			ResourceUUID:  p.UserUUID,
			ResourceTitle: p.Name,
			// "work" is the summary in words, so the Inbox says it without
			// a formatter of its own on its first load.
			Data:     map[string]string{"docs": strconv.Itoa(p.Docs), "links": strconv.Itoa(p.Links), "work": Summary(p.Docs, p.Links)},
			DedupKey: "departed:" + strings.ToLower(p.UserUUID),
			Title:    p.Name + " left with " + Summary(p.Docs, p.Links) + " under their name",
			Body:     "Hand them on in Settings, under Docs.",
			Path:     "/app/admin?tab=docs",
		}, recipients)
	}
	return nil
}

// noticeNames is how an admin's Inbox names a notice about someone who left:
// by their name, and openable only by someone who is an admin now.
func noticeNames(ctx context.Context, readerUUID string, ids []string) (map[string]bool, map[string]string, error) {
	readable, titles := map[string]bool{}, map[string]string{}
	if !activeAdmin(ctx, readerUUID) {
		return readable, titles, nil
	}
	named, err := names(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	for _, id := range ids {
		readable[id] = true
		if n := named[strings.ToLower(id)]; n != "" {
			titles[id] = n
		} else {
			titles[id] = "Someone who left"
		}
	}
	return readable, titles, nil
}
