// Package business lets admins review the people old Todoist and Notion
// imports made members of their projects: until an install's importers were
// fixed (its own claim, importBusiness.MembershipFixed), those imports
// made every person they matched a member of every project they made. Once per
// install, Scan records each such person still in each such project and tells
// the admins in their Inbox. An admin then takes one off, takes a project's
// off, or keeps them. Nobody is taken off without an admin's say, and never
// through a token.
package business

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	followBusiness "github.com/akashc777/OneCamp/business/Follow"
	importBusiness "github.com/akashc777/OneCamp/business/Import"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	reviewModel "github.com/akashc777/OneCamp/models/postgres/ImportReview"
	userModel "github.com/akashc777/OneCamp/models/postgres/User"
)

// The Inbox notice the admins get, its kind, and the key that makes the scan
// run once per install.
const (
	NoticeEvent = "import.members_to_review"
	NoticeKind  = "import_member_review"
	noticeTitle = "People old imports added to projects"
	scanKey     = "import_member_review_scan"
)

// noticeID names the one notice an install's admins get about the reviews.
var noticeID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("onecamp.import-member-review")).String()

// Why a review action is refused.
var (
	ErrNotAdmin     = errors.New("only an admin reviews who an import added")
	ErrThroughToken = errors.New("who an import added is reviewed in the app, not through a token")
	ErrNoReview     = errors.New("there's no such review")
	ErrDecided      = errors.New("an admin has already decided that one")
	ErrCreator      = errors.New("the project's creator can't be taken off it")
)

// Seams for tests: who is in a project, how many are undecided, the admins,
// telling them, whether a reader is an admin now, a project's undecided
// reviews, and takeOff's reads and writes.
var (
	membersAmong = projectDomain.MembersAmong
	pending      = reviewModel.Pending
	undecided    = reviewModel.Undecided
	undecidedIn  = reviewModel.UndecidedIn
	adminUsers   = func(ctx context.Context) ([]*userModel.User, error) {
		users, _, err := userDomain.GetAllAdminUsers(ctx, 0, 200)
		return users, err
	}
	tell           = notificationBusiness.Tell
	activeAdmin    = userDomain.IsActiveAdminUUID
	projectWith    = projectBusiness.GetBasicDgraphProjectInfoWithGivenMemberUUID
	decide         = reviewModel.Decide
	undecide       = reviewModel.Undecide
	takeOffProject = projectBusiness.RemoveMemberFromProject
	recordChange   = projectBusiness.RecordMemberChange
)

// removeAllBatch is the most RemoveAll takes off in one call: each is a whole
// project removal, so a call ends well inside a proxy's limit (Cloudflare cuts
// a request at 100 s), and the app calls again while any are left.
var removeAllBatch = 100

func init() {
	followBusiness.RegisterNoticeKind(NoticeKind, noticeNames)
}

// StartScan runs Scan in the background from the server's start, until it
// succeeds, ctx ends, or the tries run out.
func StartScan(ctx context.Context) {
	helpers.RetryInBackground(ctx, "import.member-review",
		helpers.Retries{First: time.Minute, Cap: 30 * time.Minute, Tries: 24},
		func(ctx context.Context) error {
			added, ran, err := Scan(ctx)
			if err != nil {
				helpers.MessageLogs.ErrorLog.Printf("Import member review scan failed (will retry): %v", err)
				return err
			}
			if ran {
				helpers.MessageLogs.InfoLog.Printf("Import member review: %d people old imports added are to review", added)
			}
			return nil
		})
}

// Scan records, once per install, a review of each person an old Todoist or
// Notion import (first run before the importers' fix) made a member of one of
// its projects who is still in it, and
// tells the admins while any is undecided. ran is whether it ran this time.
// Two servers starting at once add each review once and tell each admin once.
func Scan(ctx context.Context) (added int64, ran bool, err error) {
	if done, derr := configModel.IsDone(scanKey); derr != nil || done {
		return 0, false, derr
	}
	before, err := importBusiness.FixedBefore(importBusiness.MembershipFixed)
	if err != nil {
		return 0, true, err
	}
	cands, err := reviewModel.Candidates(ctx, []string{importModels.ProviderTodoist, importModels.ProviderNotion}, before)
	if err != nil {
		return 0, true, err
	}
	still, err := stillMembers(ctx, cands)
	if err != nil {
		return 0, true, err
	}
	if added, err = reviewModel.Add(ctx, still); err != nil {
		return added, true, err
	}
	if err = TellAdmins(ctx); err != nil {
		return added, true, err
	}
	return added, true, configModel.UpsertConfig(scanKey, fmt.Sprintf("done: %d to review", added))
}

// stillMembers keeps the candidates who are still in their project: one read
// of the graph per project.
func stillMembers(ctx context.Context, cands []reviewModel.Candidate) ([]reviewModel.Candidate, error) {
	var projects []uuid.UUID
	byProject := map[uuid.UUID][]reviewModel.Candidate{}
	for _, c := range cands {
		if _, seen := byProject[c.ProjectUUID]; !seen {
			projects = append(projects, c.ProjectUUID)
		}
		byProject[c.ProjectUUID] = append(byProject[c.ProjectUUID], c)
	}
	var out []reviewModel.Candidate
	for _, p := range projects {
		cs := byProject[p]
		ids := make([]string, len(cs))
		for i, c := range cs {
			ids[i] = c.UserID.String()
		}
		in, err := membersAmong(ctx, p.String(), ids)
		if err != nil {
			return nil, err
		}
		for _, c := range cs {
			if in[c.UserID.String()] {
				out = append(out, c)
			}
		}
	}
	return out, nil
}

// TellAdmins tells each admin, once in their Inbox, that old imports added
// people to projects for them to review, while any is undecided. The key is
// the install's, so a second pass tells nobody twice.
func TellAdmins(ctx context.Context) error {
	n, err := pending(ctx)
	if err != nil || n == 0 {
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
	tell(ctx, notificationBusiness.Notice{
		Kind:          NoticeEvent,
		ResourceKind:  NoticeKind,
		ResourceUUID:  noticeID,
		ResourceTitle: noticeTitle,
		Data:          map[string]string{"pending": strconv.Itoa(n)},
		DedupKey:      "import-member-review",
		Title:         "Old Todoist or Notion imports added " + people(n) + " to projects",
		Body:          "Choose who stays in Settings, under Import and export.",
		Path:          "/app/admin?tab=import",
	}, recipients)
	return nil
}

func people(n int) string {
	if n == 1 {
		return "1 person"
	}
	return strconv.Itoa(n) + " people"
}

// noticeNames is how an admin's Inbox names the notice.
var noticeNames = adminNotice(noticeTitle)

// adminNotice names one of this package's notices in an admin's Inbox as
// title, openable only by someone who is an admin now.
func adminNotice(title string) followBusiness.NoticeKind {
	return func(ctx context.Context, readerUUID string, ids []string) (map[string]bool, map[string]string, error) {
		readable, titles := map[string]bool{}, map[string]string{}
		if !activeAdmin(ctx, readerUUID) {
			return readable, titles, nil
		}
		for _, id := range ids {
			readable[id] = true
			titles[id] = title
		}
		return readable, titles, nil
	}
}

// admin is the admin acting on a review: refused through a token (agents and
// routines never decide who stays) and to anyone who isn't an admin.
func admin(ctx context.Context) (userModel.UserInfo, error) {
	if helpers.ActsThroughToken(ctx) {
		return userModel.UserInfo{}, ErrThroughToken
	}
	u, ok := ctx.Value(helpers.UserInfoContextKey).(userModel.UserInfo)
	if !ok || !u.UserPostgresInfo.IsAdmin {
		return userModel.UserInfo{}, ErrNotAdmin
	}
	return u, nil
}

// List reads, for an admin, each project with reviews and its counts, and
// how many people are undecided in all.
func List(ctx context.Context) ([]reviewModel.ProjectReviews, int, error) {
	if _, err := admin(ctx); err != nil {
		return nil, 0, err
	}
	projects, err := reviewModel.Projects(ctx)
	if err != nil {
		return nil, 0, err
	}
	pending := 0
	for _, p := range projects {
		pending += p.Undecided
	}
	if projects == nil {
		projects = []reviewModel.ProjectReviews{} // none is [] to the web, not null
	}
	return projects, pending, nil
}

// People reads, for an admin, a page of a project's people by name, from after
// the review the last page ended on (nil for the first), and whether more follow.
func People(ctx context.Context, projectUUID uuid.UUID, after *uuid.UUID) ([]reviewModel.Review, bool, error) {
	if _, err := admin(ctx); err != nil {
		return nil, false, err
	}
	revs, more, err := reviewModel.People(ctx, projectUUID, after)
	if revs == nil && err == nil {
		revs = []reviewModel.Review{}
	}
	return revs, more, err
}

// Remove takes the person a review names off its project, through the
// project's own removal, and records that an admin did.
func Remove(r *http.Request, id uuid.UUID) error {
	ctx := r.Context()
	a, err := admin(ctx)
	if err != nil {
		return err
	}
	rev, found, err := reviewModel.Get(ctx, id)
	switch {
	case err != nil:
		return err
	case !found:
		return ErrNoReview
	case rev.Outcome != "":
		return ErrDecided
	}
	return takeOff(r, a, rev)
}

// RemoveAll takes up to a batch of the people still undecided in a project off
// it, each through Remove's path, and answers how many went and how many are
// still undecided. Called again while any are left and some went, it ends with
// everyone off but the project's creator, who can't be.
func RemoveAll(r *http.Request, projectUUID uuid.UUID) (removed, left int, err error) {
	ctx := r.Context()
	a, err := admin(ctx)
	if err != nil {
		return 0, 0, err
	}
	revs, err := undecided(ctx, projectUUID, removeAllBatch)
	if err != nil {
		return 0, 0, err
	}
	for _, rev := range revs {
		switch err := takeOff(r, a, rev); {
		case err == nil:
			removed++
		case errors.Is(err, ErrDecided), errors.Is(err, ErrCreator):
			// Another admin decided this one meanwhile, or it can't be: the
			// rest still go.
		default:
			return removed, 0, err
		}
	}
	left, err = undecidedIn(ctx, projectUUID)
	return removed, left, err
}

// Keep keeps everyone still undecided in a project, and answers how many.
func Keep(r *http.Request, projectUUID uuid.UUID) (int, error) {
	a, err := admin(r.Context())
	if err != nil {
		return 0, err
	}
	n, err := reviewModel.KeepAll(r.Context(), projectUUID, a.UserPostgresInfo.Id)
	if err != nil || n == 0 {
		return int(n), err
	}
	auditBusiness.Record(r, "import.review.kept", auditBusiness.CategorySecurity, "Kept the people an old import added to a project",
		map[string]interface{}{"project_uuid": projectUUID.String(), "kept": n})
	return int(n), nil
}

// takeOff claims the review for the admin, then takes its person off the
// project; a taking off that fails gives the claim back.
func takeOff(r *http.Request, a userModel.UserInfo, rev reviewModel.Review) error {
	ctx := r.Context()
	p, err := projectWith(ctx, rev.ProjectUUID.String(), a.UserDgraphInfo.Uid, rev.UserID.String())
	if err != nil {
		return err
	}
	in := p != nil && len(p.Members) > 0
	if in && projectBusiness.IsCreatorOf(p, p.Members[0].Uid) {
		return ErrCreator
	}
	won, err := decide(ctx, rev.ID, reviewModel.OutcomeRemoved, a.UserPostgresInfo.Id)
	if err != nil {
		return err
	}
	if !won {
		return ErrDecided
	}
	if !in {
		return nil // out of it already: nothing to take off
	}
	m := p.Members[0]
	if err := takeOffProject(ctx, m.Uid, m.Uuid, p.Uid, p.Uuid); err != nil {
		// Given back on a context the request's end can't cancel: a proxy that
		// cut the request is what failed the removal, and the claim must not
		// outlive it, or the review stays decided with the person still in.
		return errors.Join(err, undecide(context.WithoutCancel(ctx), rev.ID, a.UserPostgresInfo.Id))
	}
	recordChange(r, resourceEventBusiness.ProjectMemberRemoved, "import.review.member_removed",
		"Took someone an old import added off a project", p.Uuid, m.Uuid, workAccess.RoleNone)
	return nil
}
