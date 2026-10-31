package business

// Old imports re-checked against their source (P-IMP-3D, 4D, 5D and 6D: design
// release-1010/design/permissions/REMEDIATION-P-IMP-3D-6D-2026-10-25.md, and
// main's rulings A to F). Before each provider's fix (its key in
// importFixes.go), an import let every member of each project it filled read
// what its source keeps to some people. Once per install a review is recorded
// for each gap of each project of each such import, archived ones too
// (RecheckCandidates; ruling G), and the import's own connection reads the
// source again (StartRechecks), so the fixed importer's rules narrow what
// came in, tasks in the trash included:
//   - a task the source keeps to some people is restricted to those it lets
//     in (settle and letIn, as an import does now), through the app's one
//     change of a task's restriction (taskBusiness.SetRestriction);
//   - an assignee the source doesn't let in comes off the task;
//   - a creator it doesn't let in stays, and is counted for the admin
//     (ruling C);
//   - a comment it keeps from some is hidden with this repair's own marker,
//     never deleted (ruling F).
// A task already restricted here keeps the people someone chose, and one
// someone opened after the re-check kept it stays open; a task the source no
// longer answers for is left and listed. Nothing is opened (but a task an
// admin's containment kept, which the source shows to everyone), and nobody
// is let in who couldn't read the task before. Each change is recorded before
// it is made (import_access_changes), pending until it is, so a later pass
// finishes it and an admin can undo an import's re-check for 30 days
// (UndoRecheck), whatever state its tasks and projects are in then. A task the
// pass fails on is tried again by the next passes, and named to the admins if
// it keeps failing. An import whose connection is
// gone, has expired or reaches another site goes to the admins
// (needs_admin), who re-check it with a connection of their own
// (RecheckWithConnection), keep it as it is, or keep its tasks to the
// project's admins until then (ContainImport: offered, never the default,
// ruling A). Comments held back from search (commentSearchRepair.go) are
// released once an import's re-check has settled (commentRelease.go). An old
// Basecamp import's re-check is of its people, not its tasks
// (recheckPeople.go).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	businessComment "github.com/akashc777/OneCamp/business/Comment"
	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	principalBusiness "github.com/akashc777/OneCamp/business/Principal"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	repairBusiness "github.com/akashc777/OneCamp/business/Repair"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/models/pgexec"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	reviewModel "github.com/akashc777/OneCamp/models/postgres/ImportReview"
	restrictionModel "github.com/akashc777/OneCamp/models/postgres/TaskRestriction"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/taskread"
)

const (
	recheckCandidatesKey = "imports.access_review_candidates"
	// recheckPage is how many tasks one read of the source covers.
	recheckPage = 50
	// recheckSample is how many of an import's tasks prove its connection
	// reaches the import's own source.
	recheckSample = 20
	// recheckFailures is how many passes in a row may fail to read the
	// source before an import goes to its admins.
	recheckFailures = 5
	// recheckWaits is how many times one read waits out the source's rate
	// limit before the pass gives up for now.
	recheckWaits = 5
	// listedTasks is how many tasks a review's summary names in each list.
	listedTasks = 50
	// UndoWindow is how long an admin may undo an import's re-check.
	UndoWindow = 30 * 24 * time.Hour
	// hiddenBy is the marker a comment the re-check hides carries in search,
	// which only its undo clears.
	hiddenBy = "recheck"
)

// Why a review needs an admin (its summary's reason).
const (
	reasonNoConnection      = "no_connection"
	reasonConnectionExpired = "connection_expired"
	reasonOwnerGone         = "owner_gone"
	reasonOtherSource       = "other_source"
	reasonSourceFailing     = "source_failing"
	reasonTasksFailing      = "tasks_failing" // the tasks the summary names (failed) kept failing here
)

// setRestriction is the one change of a task's restriction, and hideComment
// the one hiding of a task's comment. Seams for tests.
var (
	setRestriction = taskBusiness.SetRestriction
	hideComment    = taskBusiness.HideTaskComment
)

// Why a pass stops short of settling an import: tasks it failed on wait for
// the next pass, or more failed than a summary names.
var (
	errTasksFailing   = errors.New("tasks the re-check failed on wait for the next pass")
	errTooManyFailing = errors.New("more tasks failed than a pass keeps to try again")
)

// Why an admin's re-check, undo or containment is refused.
var (
	ErrNotRecheckable = errors.New("that import has nothing waiting for this")
	ErrRecheckRunning = errors.New("that import is being re-checked")
	ErrNoConnection   = errors.New("connect the import's tool as yourself first")
	ErrOtherSource    = errors.New("your connection reaches a different site than the import's")
)

// recheckGaps is a provider whose old imports are re-checked: the fix they
// came before, and the gaps its reviews are of: its tasks', its comments'
// ("" for a source that keeps no comment from some people), or instead its
// people's, for a source whose imports made members of people it keeps out
// of projects (recheckPeople.go).
type recheckGaps struct {
	fix, tasks, comments, people string
}

func (g recheckGaps) list() []string {
	return slices.DeleteFunc([]string{g.tasks, g.comments, g.people}, func(gap string) bool { return gap == "" })
}

var rechecks = map[string]recheckGaps{
	importModels.ProviderJira:     {fix: JiraRestrictionsFixed, tasks: reviewModel.GapJiraSecurity, comments: reviewModel.GapJiraComments},
	importModels.ProviderLinear:   {fix: LinearPrivateTeamsFixed, tasks: reviewModel.GapLinearTeams},
	importModels.ProviderBasecamp: {fix: BasecampClientsFixed, people: reviewModel.GapBasecampClients},
}

func recheckProviders() []string {
	out := make([]string, 0, len(rechecks))
	for p := range rechecks {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

// recheckCounts is a review's summary: what its re-check found and did, and
// where it got to. A tasks' review keeps the cursor, the tasks to try again
// and who checked it; a comments' review its comments. Each task counts once,
// the pass that makes its change, however many passes read it.
type recheckCounts struct {
	Restricted           int            `json:"restricted"`
	Unassigned           int            `json:"unassigned"`
	AlreadyRestricted    int            `json:"already_restricted"`
	OpenedSince          int            `json:"opened_since"` // kept by a re-check, opened by someone since: left open
	Opened               int            `json:"opened"`       // contained tasks the source shows to everyone
	CreatorOutside       int            `json:"creator_outside"`
	CreatorOutsideTasks  []uuid.UUID    `json:"creator_outside_tasks,omitempty"`
	AssigneeOutside      int            `json:"assignee_outside"` // assigned here, but not the source's assignee and not let in
	AssigneeOutsideTasks []uuid.UUID    `json:"assignee_outside_tasks,omitempty"`
	CommentsHidden       int            `json:"comments_hidden"`
	GoneOrHidden         int            `json:"gone_or_hidden"`
	GoneOrHiddenTasks    []uuid.UUID    `json:"gone_or_hidden_tasks,omitempty"`
	Failed               []uuid.UUID    `json:"failed,omitempty"` // tasks a pass failed on, tried again by the next
	Retries              int            `json:"retries,omitempty"`
	Failures             int            `json:"failures,omitempty"`
	Reason               string         `json:"reason,omitempty"`
	CheckedBy            *uuid.UUID     `json:"checked_by,omitempty"`
	Cursor               *recheckCursor `json:"cursor,omitempty"`
	Clients              int            `json:"clients,omitempty"`         // people the source keeps out of projects, in one here: listed
	PeopleUnlisted       int            `json:"people_unlisted,omitempty"` // people the source no longer lists, in one here: listed
}

// recheckCursor is the last task a pass finished: its id map kind and source id.
type recheckCursor struct {
	Entity string `json:"entity"`
	Source string `json:"source"`
}

func countsOf(r *reviewModel.AccessReview) recheckCounts {
	var c recheckCounts
	if r != nil && len(r.Summary) > 0 {
		// A summary this code didn't write reads as nothing done yet.
		if err := json.Unmarshal(r.Summary, &c); err != nil {
			return recheckCounts{}
		}
	}
	return c
}

// list adds id to a summary's list while it names fewer than listedTasks.
func list(ids []uuid.UUID, id uuid.UUID) []uuid.UUID {
	if len(ids) < listedTasks && !slices.Contains(ids, id) {
		return append(ids, id)
	}
	return ids
}

// RecheckCandidates records, once per install, a review of each gap for each
// project (archived ones too) of each import that ran before its provider's
// fix: rechecking, or needs_admin when whoever ran it can't act any more or
// has no stored connection. A server writes them as it starts, before the
// re-check (StartRechecks) and the admins' notice, which waits while any is
// being re-checked. Its key names the providers it covers, so an install
// that ran it before a provider's re-check came records that provider's
// reviews too (a second recording adds nothing).
var RecheckCandidates = repairBusiness.Once[recheckCandidates]{
	Key: recheckCandidatesKey + ":" + strings.Join(recheckProviders(), ","), Name: "import.access-review-candidates", Tries: 12,
	Repair: writeRecheckCandidates,
}

// recheckCandidates is how many reviews RecheckCandidates recorded.
type recheckCandidates int64

func (n recheckCandidates) String() string {
	return fmt.Sprintf("%d reviews of what old imports let every project member read", int64(n))
}

func writeRecheckCandidates(ctx context.Context) (recheckCandidates, error) {
	var added int64
	var unchecked []uuid.UUID
	for _, provider := range recheckProviders() {
		g := rechecks[provider]
		before, err := FixedBefore(g.fix)
		if err != nil {
			return recheckCandidates(added), err
		}
		cands, err := reviewModel.AccessCandidates(ctx, provider, before)
		if err != nil {
			return recheckCandidates(added), err
		}
		n, err := reviewModel.AddAccessReviews(ctx, cands, g.list())
		added += n
		if err != nil {
			return recheckCandidates(added), err
		}
		if g.people != "" {
			unchecked = append(unchecked, needingAdmins(cands)...)
		}
	}
	// Imports of people that can't be re-checked from the start list everyone
	// they added for their admins, as one that stops does (toAdmins).
	if len(unchecked) > 0 {
		if err := listEveryone(ctx, unchecked); err != nil {
			return recheckCandidates(added), err
		}
	}
	return recheckCandidates(added), nil
}

// needingAdmins is the imports among cands whose reviews need an admin from
// the start, once each. Pure.
func needingAdmins(cands []reviewModel.AccessCandidate) []uuid.UUID {
	var ids []uuid.UUID
	for _, c := range cands {
		if c.Reason != "" && !slices.Contains(ids, c.ImportID) {
			ids = append(ids, c.ImportID)
		}
	}
	return ids
}

// StartRechecks re-checks, in the background from a server's start, each
// import with a review being re-checked, until none is left, ctx ends or the
// tries run out; the next start goes on where this one stopped. Each try
// records the reviews first while no start has (RecheckCandidates), and
// releases into search the comments of the imports that have settled.
func StartRechecks(ctx context.Context) {
	helpers.RetryInBackground(ctx, "import.access-recheck",
		helpers.Retries{First: 30 * time.Second, Cap: 30 * time.Minute, Tries: 48},
		func(ctx context.Context) error {
			if _, _, err := RecheckCandidates.Run(ctx); err != nil {
				return err
			}
			rep, err := recheckPending(ctx)
			if err != nil {
				helpers.LogErrorWithContext(ctx, "Old imports' access re-check (will retry): %v (%s)", err, rep)
				return err
			}
			if rep.Imports > 0 {
				helpers.LogInfoWithContext(ctx, "Old imports' access re-check: %s", rep)
			}
			return nil
		})
}

// recheckReport is what one pass over the imports being re-checked did.
type recheckReport struct{ Imports, Settled, Waiting int }

func (r recheckReport) String() string {
	return fmt.Sprintf("%d imports being re-checked: %d settled, %d waiting", r.Imports, r.Settled, r.Waiting)
}

// errRecheckWaits is an import whose re-check can't go on yet: it is still
// running, or another server is re-checking it.
var errRecheckWaits = errors.New("the import's re-check waits")

// recheckPending re-checks each import with a review being re-checked, and
// then releases the held-back comments of those that settled, whether or not
// another waits. It fails while any is left, naming each that failed and why,
// so its caller logs it and tries again.
func recheckPending(ctx context.Context) (recheckReport, error) {
	var rep recheckReport
	ids, err := reviewModel.ImportsInState(ctx, recheckProviders(), reviewModel.StateRechecking)
	if err != nil {
		return rep, err
	}
	var failed []error
	for _, id := range ids {
		rep.Imports++
		job, err := importModels.GetJob(ctx, id)
		if err == nil {
			err = recheckImport(ctx, job)
		}
		if err != nil {
			rep.Waiting++
			if !errors.Is(err, errRecheckWaits) {
				failed = append(failed, fmt.Errorf("import %s: %w", id, err))
			}
			continue
		}
		rep.Settled++
	}
	released := releaseSettled(ctx)
	if rep.Waiting > 0 {
		errs := append([]error{fmt.Errorf("%d imports still to re-check", rep.Waiting)}, failed...)
		return rep, errors.Join(append(errs, released)...)
	}
	return rep, released
}

// recheckLock is the advisory lock a server holds while it re-checks,
// undoes or contains an import, so of two servers, or an admin and the
// re-check, one works on it at a time.
func recheckLock(importID uuid.UUID) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("onecamp.import-recheck:" + importID.String()))
	return int64(h.Sum64())
}

// lockImport takes the import's recheckLock: errRecheckWaits when another
// holds it.
func lockImport(ctx context.Context, importID uuid.UUID) (func(), error) {
	release, held, err := pgexec.TryAdvisoryLock(ctx, recheckLock(importID))
	if err != nil {
		return nil, err
	}
	if !held {
		return nil, errRecheckWaits
	}
	return func() {
		if err := release(); err != nil {
			helpers.LogErrorWithContext(ctx, "Re-check of import %s: releasing its lock: %v", importID, err)
		}
	}, nil
}

// recheckUnit is one project of an import being re-checked: its reviews of
// tasks and of comments (nil when its source keeps no comment from some), or
// of people, with their counts so far.
type recheckUnit struct {
	project                                  uuid.UUID
	tasks, comments, people                  *reviewModel.AccessReview
	taskCounts, commentsCounts, peopleCounts recheckCounts
}

// lead is the counts of the review a unit's re-check leads with: its
// people's, or its tasks'.
func (u *recheckUnit) lead() *recheckCounts {
	if u.people != nil {
		return &u.peopleCounts
	}
	return &u.taskCounts
}

// recheck is one import being re-checked with the connection of cred
// (whoever ran the import, or the admin re-checking it: checker), acting as
// actor, from at.
type recheck struct {
	job     *importModels.Job
	source  importProvider.Rechecker
	cred    uuid.UUID
	actor   *userModels.UserInfo
	checker *uuid.UUID // the admin re-checking it; nil for the workspace's own pass
	changes map[changeKey]*reviewModel.AccessChange
	at      time.Time
	// taskErrs is why the pass failed on the tasks it failed on, the first
	// listedTasks of them, for its error.
	taskErrs []error
}

// changeKey names a change: its kind and the task or comment it is of.
type changeKey struct {
	kind   string
	entity uuid.UUID
}

// recheckImport re-checks an import whose reviews are being re-checked: it
// proves the connection reaches the import's source, narrows each project's
// tasks and comments a page at a time, from where an earlier pass stopped,
// and settles the reviews, or hands them to the admins with why.
func recheckImport(ctx context.Context, job *importModels.Job) error {
	switch job.Status {
	case importModels.StatusPending, importModels.StatusValidating, importModels.StatusPlanned,
		importModels.StatusRunning, importModels.StatusPaused:
		return errRecheckWaits
	}
	unlock, err := lockImport(ctx, job.Id)
	if err != nil {
		return err
	}
	defer unlock()
	ctx = recheckContext(ctx)
	units, err := unitsOf(ctx, job)
	if err != nil || len(units) == 0 {
		return err
	}
	r := &recheck{job: job, checker: units[0].lead().CheckedBy, at: time.Now()}
	switch {
	case r.checker != nil:
		r.cred = *r.checker
	case job.TriggeredBy != nil:
		r.cred = *job.TriggeredBy
	default:
		return toAdmins(ctx, job, reasonOwnerGone)
	}
	if r.actor, err = userDomain.ActorByUUID(ctx, r.cred); errors.Is(err, userDomain.ErrNoActor) {
		return toAdmins(ctx, job, reasonOwnerGone)
	} else if err != nil {
		return err
	}
	g := rechecks[job.Provider]
	if g.people != "" {
		return r.recheckPeople(ctx, units)
	}
	rc, ok := importProvider.Get(job.Provider).(importProvider.Rechecker)
	if !ok || g.tasks == "" {
		return fmt.Errorf("%s has no re-check", job.Provider)
	}
	r.source = rc
	if err := sameSource(ctx, rc, job, r.cred); err != nil {
		return r.failed(ctx, units, err)
	}
	changes, err := reviewModel.AccessChangesOf(ctx, job.Id)
	if err != nil {
		return err
	}
	r.changes = make(map[changeKey]*reviewModel.AccessChange, len(changes))
	for i := range changes {
		r.changes[changeKey{changes[i].Kind, changes[i].Entity}] = &changes[i]
	}
	for _, u := range units {
		switch err := r.project(ctx, u); {
		case errors.Is(err, errTooManyFailing):
			return r.retryLater(ctx, units)
		case err != nil:
			return r.failed(ctx, units, err)
		}
	}
	if slices.ContainsFunc(units, func(u *recheckUnit) bool { return len(u.taskCounts.Failed) > 0 }) {
		return r.retryLater(ctx, units)
	}
	if _, err := reviewModel.MoveAccessReviews(ctx, job.Id, []string{reviewModel.StateRechecking}, reviewModel.StateRechecked,
		map[string]any{"failures": 0, "retries": 0}, nil); err != nil {
		return err
	}
	r.recordDone(ctx, units)
	return nil
}

// recheckContext is what a re-check reads and writes through: every task,
// restricted ones too, since it narrows them for everyone else.
func recheckContext(ctx context.Context) context.Context {
	return taskread.WithSystem(ctx)
}

// unitsOf reads an import's reviews being re-checked, by project.
func unitsOf(ctx context.Context, job *importModels.Job) ([]*recheckUnit, error) {
	revs, err := reviewModel.AccessReviewsOf(ctx, job.Id)
	if err != nil {
		return nil, err
	}
	g := rechecks[job.Provider]
	byProject := map[uuid.UUID]*recheckUnit{}
	var units []*recheckUnit
	for i := range revs {
		rev := &revs[i]
		if rev.State != reviewModel.StateRechecking {
			continue
		}
		u := byProject[rev.ProjectUUID]
		if u == nil {
			u = &recheckUnit{project: rev.ProjectUUID}
			byProject[rev.ProjectUUID] = u
			units = append(units, u)
		}
		switch rev.Gap {
		case g.tasks:
			u.tasks, u.taskCounts = rev, countsOf(rev)
		case g.comments:
			u.comments, u.commentsCounts = rev, countsOf(rev)
		case g.people:
			u.people, u.peopleCounts = rev, countsOf(rev)
		}
	}
	return slices.DeleteFunc(units, func(u *recheckUnit) bool { return u.tasks == nil && u.people == nil }), nil
}

// sameSource proves cred's connection reaches the import's own source: a
// sample of its tasks answers with the makers the import recorded, and mostly
// in the projects it recorded (answersAsRecorded). An import with no task
// left has nothing to prove.
func sameSource(ctx context.Context, rc importProvider.Rechecker, job *importModels.Job, cred uuid.UUID) error {
	sample, err := importModels.SampleMappedTasks(ctx, job.Id, recheckSample)
	if err != nil || len(sample) == 0 {
		return err
	}
	keys := make([]string, len(sample))
	for i, t := range sample {
		keys[i] = t.SourceID
	}
	got, err := readSource(ctx, rc, cred, keys, nil)
	if err != nil {
		return err
	}
	if !answersAsRecorded(sample, got) {
		return errOtherSource
	}
	return nil
}

// errOtherSource is a connection that reaches another site than the
// import's.
var errOtherSource = errors.New("the connection reaches another site")

// answersAsRecorded is whether the source answers for a sample of an
// import's tasks as the import recorded them, allowing for tasks moved since:
// most of the sample answers; each that answers was made by the person the
// import recorded (when it recorded one: a maker never changes at the
// source); and at least half of those that answer are still in the project
// the import recorded. Another site's tasks fail on their makers, or answer
// too few. Pure.
func answersAsRecorded(sample []importModels.MappedTask, got map[string]importProvider.RecheckedTask) bool {
	answered, inPlace := 0, 0
	for _, t := range sample {
		now, ok := got[t.SourceID]
		if !ok || now.Gone {
			continue
		}
		answered++
		if t.Author != "" && now.Task.CreatedBy != t.Author {
			return false
		}
		if now.Task.ProjectSourceID == t.ProjectSource {
			inPlace++
		}
	}
	return answered*2 > len(sample) && inPlace*2 >= answered
}

// readSource reads the tasks (and comments) from the source (waitOut).
func readSource(ctx context.Context, rc importProvider.Rechecker, cred uuid.UUID, keys []string, comments map[string][]string) (map[string]importProvider.RecheckedTask, error) {
	return waitOut(ctx, func() (map[string]importProvider.RecheckedTask, error) { return rc.Recheck(ctx, cred, keys, comments) })
}

// waitOut reads from a source, waiting out its rate limit up to recheckWaits
// times.
func waitOut[T any](ctx context.Context, read func() (T, error)) (T, error) {
	for waited := 0; ; waited++ {
		got, err := read()
		var limited *importProvider.ErrRateLimited
		if !errors.As(err, &limited) || waited == recheckWaits {
			return got, err
		}
		select {
		case <-ctx.Done():
			var none T
			return none, ctx.Err()
		case <-time.After(limited.RetryAfter):
		}
	}
}

// failed hands an import to its admins when its connection can't do the
// re-check (gone, expired, or another site's), or when its source has failed
// recheckFailures passes in a row; otherwise it counts the failure and
// answers it, so a later pass tries again.
func (r *recheck) failed(ctx context.Context, units []*recheckUnit, err error) error {
	switch {
	case errors.Is(err, importModels.ErrTokenNotFound), errors.Is(err, importModels.ErrTokenUnreadable):
		return toAdmins(ctx, r.job, reasonNoConnection)
	case errors.Is(err, importProvider.ErrTokenRejected):
		return toAdmins(ctx, r.job, reasonConnectionExpired)
	case errors.Is(err, errOtherSource):
		return toAdmins(ctx, r.job, reasonOtherSource)
	}
	failures := units[0].lead().Failures + 1
	if failures >= recheckFailures {
		return toAdmins(ctx, r.job, reasonSourceFailing)
	}
	if _, serr := reviewModel.MoveAccessReviews(ctx, r.job.Id, []string{reviewModel.StateRechecking}, reviewModel.StateRechecking,
		map[string]any{"failures": failures}, nil); serr != nil {
		return errors.Join(err, serr)
	}
	return err
}

// retryLater leaves an import whose pass failed on some of its tasks (their
// summaries name them: failed) being re-checked, for the next pass to try
// them again, answering why they failed, and hands it to its admins, the
// tasks named, once recheckFailures passes in a row have failed on some.
func (r *recheck) retryLater(ctx context.Context, units []*recheckUnit) error {
	retries := units[0].taskCounts.Retries + 1
	if retries >= recheckFailures {
		return toAdmins(ctx, r.job, reasonTasksFailing)
	}
	failing := errors.Join(append([]error{errTasksFailing}, r.taskErrs...)...)
	if _, err := reviewModel.MoveAccessReviews(ctx, r.job.Id, []string{reviewModel.StateRechecking}, reviewModel.StateRechecking,
		map[string]any{"retries": retries}, nil); err != nil {
		return errors.Join(failing, err)
	}
	return failing
}

// toAdmins hands an import's reviews being re-checked to its admins, with
// why, and records it. An import of people first lists everyone it added,
// as 510 does, since its source can't tell them apart now; a failure there
// leaves it being re-checked, for the next pass.
func toAdmins(ctx context.Context, job *importModels.Job, reason string) error {
	if rechecks[job.Provider].people != "" {
		if err := listEveryone(ctx, []uuid.UUID{job.Id}); err != nil {
			return err
		}
	}
	if _, err := reviewModel.MoveAccessReviews(ctx, job.Id, []string{reviewModel.StateRechecking}, reviewModel.StateNeedsAdmin,
		map[string]any{"reason": reason}, nil); err != nil {
		return err
	}
	if err := auditBusiness.RecordForPrincipal(ctx, nil, "", auditBusiness.ActorSystem, "import.access_needs_admin",
		auditBusiness.CategorySecurity, "An old import's access needs an admin", map[string]any{"import_id": job.Id.String(), "reason": reason}); err != nil {
		helpers.LogErrorWithContext(ctx, "Re-check of import %s: recording that it needs an admin: %v", job.Id, err)
	}
	return nil
}

// project re-checks a project's tasks: first those an earlier pass failed on,
// then a page at a time from the review's cursor, writing the counts, the
// cursor and the tasks to try again after each page.
func (r *recheck) project(ctx context.Context, u *recheckUnit) error {
	if retry := u.taskCounts.Failed; len(retry) > 0 {
		page, err := importModels.MappedTasksByID(ctx, r.job.Id, retry)
		if err != nil {
			return err
		}
		// One gone from the import since drops out.
		u.taskCounts.Failed = nil
		if err := r.writeAfter(ctx, u, r.page(ctx, u, page, false)); err != nil {
			return err
		}
	}
	for {
		after := recheckCursor{}
		if u.taskCounts.Cursor != nil {
			after = *u.taskCounts.Cursor
		}
		page, err := importModels.MappedTasksIn(ctx, r.job.Id, u.project, after.Entity, after.Source, recheckPage)
		if err != nil || len(page) == 0 {
			return err
		}
		if err := r.writeAfter(ctx, u, r.page(ctx, u, page, true)); err != nil {
			return err
		}
		if len(page) < recheckPage {
			return nil
		}
	}
}

// writeAfter writes a project's counts after a page, whether or not the page
// went to its end (pageErr), and answers what failed.
func (r *recheck) writeAfter(ctx context.Context, u *recheckUnit, pageErr error) error {
	if err := r.writeCounts(ctx, u); err != nil {
		return errors.Join(pageErr, err)
	}
	return pageErr
}

// page re-checks a page of a project's tasks, with one read of the source,
// one of their people and one of their restrictions. A task that fails is
// kept to try again (failed) while the summary has room to name it; one more
// stops the page before it (errTooManyFailing). advance moves the cursor past
// each task as it is done.
func (r *recheck) page(ctx context.Context, u *recheckUnit, page []importModels.MappedTask, advance bool) error {
	if len(page) == 0 {
		return nil
	}
	keys := make([]string, len(page))
	ids := make([]uuid.UUID, len(page))
	for i, t := range page {
		keys[i], ids[i] = t.SourceID, t.TaskID
	}
	var comments map[string][]importModels.MappedComment
	asked := map[string][]string{}
	if u.comments != nil {
		var err error
		if comments, err = importModels.MappedComments(ctx, r.job.Id, keys); err != nil {
			return err
		}
		for task, cs := range comments {
			for _, c := range cs {
				asked[task] = append(asked[task], c.SourceID)
			}
		}
	}
	got, err := readSource(ctx, r.source, r.cred, keys, asked)
	if err != nil {
		return err
	}
	matched, err := r.matched(ctx, got)
	if err != nil {
		return err
	}
	restrictions, err := restrictionModel.OfMany(ctx, ids)
	if err != nil {
		return err
	}
	for _, t := range page {
		if err := r.task(ctx, u, t, got[t.SourceID], matched, restrictions[t.TaskID], comments[t.SourceID]); err != nil {
			helpers.LogErrorWithContext(ctx, "Re-check of import %s: task %s: %v", r.job.Id, t.TaskID, err)
			if len(r.taskErrs) < listedTasks {
				r.taskErrs = append(r.taskErrs, fmt.Errorf("task %s: %w", t.TaskID, err))
			}
			if len(u.taskCounts.Failed) >= listedTasks {
				return errTooManyFailing
			}
			u.taskCounts.Failed = append(u.taskCounts.Failed, t.TaskID)
		}
		if advance {
			u.taskCounts.Cursor = &recheckCursor{Entity: t.Entity, Source: t.SourceID}
		}
	}
	return nil
}

// matched is how each person of the page's tasks the source keeps to some
// came across in the import: one read.
func (r *recheck) matched(ctx context.Context, got map[string]importProvider.RecheckedTask) (map[string]importModels.PersonMatch, error) {
	var asked []string
	for _, now := range got {
		if now.Gone || now.Task.Restricted == "" {
			continue
		}
		p := peopleOf(&now.Task)
		for _, s := range append([]string{p.Creator, p.Assignee}, p.Audience...) {
			if s != "" && !slices.Contains(asked, s) {
				asked = append(asked, s)
			}
		}
	}
	if len(asked) == 0 {
		return nil, nil
	}
	return importModels.PeopleMatched(ctx, r.job.Id, r.job.Provider, r.job.SourceWorkspaceName, asked)
}

func (r *recheck) writeCounts(ctx context.Context, u *recheckUnit) error {
	counts := map[uuid.UUID]any{u.tasks.ID: u.taskCounts}
	if u.comments != nil {
		counts[u.comments.ID] = u.commentsCounts
	}
	return reviewModel.SetAccessSummaries(ctx, counts)
}

// taskHere is a task here as the re-check finds it: whether it is kept to
// some people, whether by this import's re-check or an admin's containment
// (ours, contained), whether someone opened it since one of those kept it
// (openedSince), and its assignee.
type taskHere struct {
	restricted, ours, contained, openedSince bool
	assignee                                 uuid.UUID
}

// taskPlan is what the re-check does to one task (outcome: restricted, or
// opened from an admin's containment; the rest leave it), and for one it
// restricts, whom the source lets in besides whoever ran the import (grants,
// before letIn), whether its assignee comes off, whether its maker stays
// though the source doesn't let them in, and whether it is assigned here to
// someone the source doesn't have as its assignee (reassigned).
type taskPlan struct {
	outcome        string
	grants         []uuid.UUID
	unassign       bool
	creatorOutside bool
	reassigned     bool
}

// What a task counts as.
const (
	outcomeRestricted  = "restricted"
	outcomeAlready     = "already_restricted"
	outcomeOpenedSince = "opened_since"
	outcomeOpened      = "opened"
	outcomeOpen        = "open"
	outcomeGone        = "gone_or_hidden"
)

// planTask is what the re-check does to a task here (here) that its source
// has as now, whose people settle as s, and whose source assignee came across
// as srcAssignee. A task someone restricted here keeps their choice, and one
// someone opened after a re-check or containment kept it stays open; one the
// source no longer answers for is left. Pure.
func planTask(now importProvider.RecheckedTask, here taskHere, s settled, srcAssignee uuid.UUID) taskPlan {
	switch {
	case now.Gone:
		return taskPlan{outcome: outcomeGone}
	case here.openedSince:
		return taskPlan{outcome: outcomeOpenedSince}
	case here.restricted && !here.ours:
		return taskPlan{outcome: outcomeAlready}
	case now.Task.Restricted == "" && here.contained:
		return taskPlan{outcome: outcomeOpened}
	case now.Task.Restricted == "":
		return taskPlan{outcome: outcomeOpen}
	}
	return taskPlan{
		outcome:        outcomeRestricted,
		grants:         s.grants,
		unassign:       !s.assignee && here.assignee != uuid.Nil && here.assignee == srcAssignee,
		creatorOutside: !s.creator,
		reassigned:     here.assignee != uuid.Nil && here.assignee != srcAssignee,
	}
}

// task re-checks one task and its comments, counting what it changes the
// pass it changes it. It answers why it couldn't finish a task, which the
// pass then tries again.
func (r *recheck) task(ctx context.Context, u *recheckUnit, t importModels.MappedTask, now importProvider.RecheckedTask,
	matched map[string]importModels.PersonMatch, restriction restrictionModel.Restriction, comments []importModels.MappedComment) error {
	dg, err := taskDomain.GetDgraphBasicTaskInfoByUUID(ctx, t.TaskID.String(), r.actor.UserDgraphInfo.Uid)
	if err != nil {
		return err
	}
	if dg == nil || dg.Uuid == "" {
		return fmt.Errorf("the graph has no task %s", t.TaskID)
	}
	ours, openedSince := ourChange(restriction, r.changes[changeKey{reviewModel.ChangeRestricted, t.TaskID}])
	here := taskHere{restricted: restriction.Restricted, ours: ours != nil, contained: ours != nil && containedBy(ours), openedSince: openedSince}
	if dg.Assignee != nil {
		here.assignee, _ = uuid.Parse(dg.Assignee.Uuid)
	}
	p := peopleOf(&now.Task)
	s := settle(p, matched, r.importer())
	plan := planTask(now, here, s, matched[p.Assignee].ID)
	if err := r.apply(ctx, u, t, dg, plan, ours); err != nil {
		return err
	}
	if now.Gone || u.comments == nil {
		return nil
	}
	return r.hideKept(ctx, u, dg, now, comments)
}

// ourChange is a task's recorded restriction change (ours) as a pass reads
// it against the task's restriction here: still ours while the task is kept
// as it left it, or the change is still to be made (pending) on a task that
// is open or restricted on its own; nil once someone has restricted the task
// otherwise, their choice now; and ours, opened since (openedSince), once
// someone has opened a task it kept. Pure.
func ourChange(r restrictionModel.Restriction, ours *reviewModel.AccessChange) (*reviewModel.AccessChange, bool) {
	switch {
	case ours == nil, keptByUs(r, ours):
		return ours, false
	case r.Restricted:
		return nil, false
	case pendingChange(ours):
		return ours, false
	}
	return ours, true
}

// keptByUs is whether a task is restricted as a recorded change (ch) of the
// re-check's, or of an admin's containment, keeps it: as the change left it
// (restrictedAsLeft), or on its own while the change is still to be made,
// since a pass that stopped between writing the people it lets in and
// restricting the task leaves it on the people before. Pure.
func keptByUs(r restrictionModel.Restriction, ch *reviewModel.AccessChange) bool {
	return ch != nil && r.Restricted && (restrictedAsLeft(r, ch) || (r.Via == uuid.Nil && pendingChange(ch)))
}

// importer is whoever ran the import: the fixed importer lets them in.
func (r *recheck) importer() uuid.UUID {
	if r.job.TriggeredBy == nil {
		return uuid.Nil
	}
	return *r.job.TriggeredBy
}

// containedBy is whether a restriction change is an admin's containment:
// it let nobody in by name.
func containedBy(c *reviewModel.AccessChange) bool {
	var after struct {
		Contained bool `json:"contained"`
	}
	return json.Unmarshal(c.After, &after) == nil && after.Contained
}

// pendingChange is whether a recorded restriction is still to be made: it is
// recorded pending before it is made, and settled once it is
// (reviewModel.SettleAccessChange), so an open task with a pending change is
// one a pass failed on or stopped at, never someone's choice.
func pendingChange(c *reviewModel.AccessChange) bool {
	var after struct {
		Pending bool `json:"pending"`
	}
	return json.Unmarshal(c.After, &after) == nil && after.Pending
}

// apply makes a task's plan, recording each change before it is made. A task
// counts the pass that first makes its change: one ours already kept is kept
// again to whom the source lets in now, uncounted.
func (r *recheck) apply(ctx context.Context, u *recheckUnit, t importModels.MappedTask, dg *dgraphStruct.DgraphTask, plan taskPlan, ours *reviewModel.AccessChange) error {
	c := &u.taskCounts
	switch plan.outcome {
	case outcomeGone:
		c.GoneOrHidden++
		c.GoneOrHiddenTasks = list(c.GoneOrHiddenTasks, t.TaskID)
		return nil
	case outcomeAlready:
		c.AlreadyRestricted++
		return nil
	case outcomeOpenedSince:
		c.OpenedSince++
		return nil
	case outcomeOpen:
		return nil
	case outcomeOpened:
		opened, err := setRestriction(ctx, dg, false, nil, r.changer(ctx), r.at)
		if err != nil {
			return err
		}
		if _, err := reviewModel.UndoAccessChanges(ctx, []uuid.UUID{ours.ID}); err != nil {
			return err
		}
		if opened {
			c.Opened++
		}
		return nil
	}
	creator := uuid.Nil
	if dg.CreatedBy != nil {
		creator, _ = uuid.Parse(dg.CreatedBy.Uuid)
	}
	grants, err := letIn(ctx, u.project, creator, r.importer(), plan.grants)
	if err != nil {
		return err
	}
	first := ours == nil || pendingChange(ours)
	after := map[string]any{"people": grants, "pending": true}
	if ours != nil {
		err = reviewModel.SetAccessChangeAfter(ctx, ours.ID, after)
	} else {
		_, err = reviewModel.AddAccessChange(ctx, u.tasks.ID, reviewModel.ChangeRestricted, t.TaskID, map[string]any{"restricted": false}, after)
	}
	if err != nil {
		return err
	}
	if _, err := setRestriction(ctx, dg, true, grants, r.changer(ctx), r.at); err != nil {
		return err
	}
	if err := reviewModel.SettleAccessChange(ctx, u.tasks.ID, reviewModel.ChangeRestricted, t.TaskID); err != nil {
		return err
	}
	if first {
		c.Restricted++
		if plan.creatorOutside {
			c.CreatorOutside++
			c.CreatorOutsideTasks = list(c.CreatorOutsideTasks, t.TaskID)
		}
		// Assigned here to someone the source has no part for: they read it
		// as its assignee, so the admin hears of them.
		if plan.reassigned {
			if a, _ := uuid.Parse(dg.Assignee.Uuid); a != creator && !slices.Contains(grants, a) {
				c.AssigneeOutside++
				c.AssigneeOutsideTasks = list(c.AssigneeOutsideTasks, t.TaskID)
			}
		}
	}
	if !plan.unassign {
		return nil
	}
	if _, err := reviewModel.AddAccessChange(ctx, u.tasks.ID, reviewModel.ChangeUnassigned, t.TaskID,
		map[string]any{"assignee": dg.Assignee.Uuid}, nil); err != nil {
		return err
	}
	// A task in the trash keeps its deletion time: restoring it brings it back without them.
	if helpers.IsSoftDeleted(dg.DeletedAt) {
		err = taskBusiness.AssignInTrash(ctx, dg, nil, &r.actor.UserDgraphInfo)
	} else {
		err = taskBusiness.UpdateTaskAssigneeByTaskUUID(ctx, t.TaskID, nil, dg.Assignee.Uid, dg.Uid, dg, &r.actor.UserDgraphInfo)
	}
	if err != nil {
		return err
	}
	c.Unassigned++
	return nil
}

// hideKept hides each of a task's comments that its source keeps from some
// of the people who see the task, and counts those the source no longer has.
// It answers what failed, once it has tried each.
func (r *recheck) hideKept(ctx context.Context, u *recheckUnit, dg *dgraphStruct.DgraphTask, now importProvider.RecheckedTask, comments []importModels.MappedComment) error {
	c := &u.commentsCounts
	project := ""
	if dg.Project != nil {
		project = dg.Project.Uuid
	}
	var failed error
	for _, mc := range comments {
		if now.KeptComments[mc.SourceID] == "" {
			continue
		}
		at := r.at
		// Recorded pending, settled once hidden: a comment counts once, by
		// the pass that hides it, however many passes retry its task.
		ch := r.changes[changeKey{reviewModel.ChangeCommentHidden, mc.CommentID}]
		first := ch == nil || pendingChange(ch)
		if ch != nil {
			at = hiddenAt(ch, at)
		} else if _, err := reviewModel.AddAccessChange(ctx, u.comments.ID, reviewModel.ChangeCommentHidden, mc.CommentID, nil,
			map[string]any{"hidden_at": at, "task": dg.Uuid, "pending": true}); err != nil {
			failed = errors.Join(failed, fmt.Errorf("recording comment %s: %w", mc.CommentID, err))
			continue
		}
		hidden, err := hideComment(ctx, mc.CommentID, project, at, hiddenBy)
		if err == nil && first {
			err = reviewModel.SettleAccessChange(ctx, u.comments.ID, reviewModel.ChangeCommentHidden, mc.CommentID)
		}
		switch {
		case err != nil:
			failed = errors.Join(failed, fmt.Errorf("comment %s: %w", mc.CommentID, err))
		case hidden && first:
			c.CommentsHidden++
		}
	}
	if failed == nil {
		// Counted once, by the pass that finishes the task's comments.
		c.GoneOrHidden += len(now.MissingComments)
	}
	return failed
}

// hiddenAt is when a recorded comment change hid its comment, or or for a
// record that doesn't say.
func hiddenAt(c *reviewModel.AccessChange, or time.Time) time.Time {
	var after struct {
		HiddenAt time.Time `json:"hidden_at"`
	}
	if json.Unmarshal(c.After, &after) != nil || after.HiddenAt.IsZero() {
		return or
	}
	return after.HiddenAt
}

// changer is who the re-check changes tasks as: the person whose connection
// read the source, recorded as the workspace acting (an admin's re-check, as
// that admin).
func (r *recheck) changer(ctx context.Context) taskBusiness.Changer {
	return taskBusiness.Changer{By: r.cred, UserUUID: r.actor.UserDgraphInfo.Uuid,
		Record: r.record(ctx)}
}

// record writes an audit record of the re-check's: the workspace's own pass
// (ActorSystem), or the admin who asked for it, naming the import and whose
// connection read the source (ruling B).
func (r *recheck) record(ctx context.Context) func(action, summary string, meta map[string]any) {
	return recordAs(ctx, r.checker, r.job.Id, map[string]any{"credential_owner": r.cred.String()})
}

// recordAs is an audit record of an old import's re-check, undo or
// containment: by admin (nil: the workspace acting on its own), naming the
// import and what extra adds. Failing to write it is logged: the change it
// records is a narrowing, or an admin's own undo.
func recordAs(ctx context.Context, admin *uuid.UUID, importID uuid.UUID, extra map[string]any) func(action, summary string, meta map[string]any) {
	kind := auditBusiness.ActorSystem
	if admin != nil {
		kind = auditBusiness.ActorHuman
	}
	return func(action, summary string, meta map[string]any) {
		meta["import_id"] = importID.String()
		for k, v := range extra {
			meta[k] = v
		}
		if err := auditBusiness.RecordForPrincipal(ctx, admin, "", kind, action, auditBusiness.CategorySecurity, summary, meta); err != nil {
			helpers.LogErrorWithContext(ctx, "Old import %s: recording %s: %v", importID, action, err)
		}
	}
}

// recordDone records an import's settled re-check with its counts.
func (r *recheck) recordDone(ctx context.Context, units []*recheckUnit) {
	total := map[string]any{"projects": len(units)}
	for _, u := range units {
		for k, n := range map[string]int{"restricted": u.taskCounts.Restricted, "unassigned": u.taskCounts.Unassigned,
			"already_restricted": u.taskCounts.AlreadyRestricted, "opened_since": u.taskCounts.OpenedSince, "opened": u.taskCounts.Opened,
			"creator_outside": u.taskCounts.CreatorOutside, "assignee_outside": u.taskCounts.AssigneeOutside,
			"gone_or_hidden": u.taskCounts.GoneOrHidden, "comments_hidden": u.commentsCounts.CommentsHidden,
			"comments_gone_or_hidden": u.commentsCounts.GoneOrHidden, "clients": u.peopleCounts.Clients,
			"people_unlisted": u.peopleCounts.PeopleUnlisted} {
			prev, _ := total[k].(int)
			total[k] = prev + n
		}
	}
	r.record(ctx)("import.access_rechecked", "Re-checked an old import's access against its source", total)
}

// RecheckWithConnection re-checks, with the admin's own connection to the
// import's tool (by), an import whose reviews wait for an admin, or that an
// admin's containment keeps: the connection must reach the import's own site.
// It proves that at once and re-checks in the background, as by: a later
// re-check replaces a containment. The admin's own sight at the source
// applies, so what they can't see there counts as gone or hidden.
func RecheckWithConnection(ctx context.Context, importID, by uuid.UUID) error {
	job, err := jobAwaitingAdmin(ctx, importID, reviewModel.StateNeedsAdmin, reviewModel.StateContained)
	if err != nil {
		return err
	}
	switch err := proveSource(recheckContext(ctx), job, by); {
	case errors.Is(err, importModels.ErrTokenNotFound), errors.Is(err, importModels.ErrTokenUnreadable),
		errors.Is(err, importProvider.ErrTokenRejected):
		return ErrNoConnection
	case errors.Is(err, errOtherSource):
		return ErrOtherSource
	case err != nil:
		return err
	}
	moved, err := reviewModel.MoveAccessReviews(ctx, job.Id, []string{reviewModel.StateNeedsAdmin, reviewModel.StateContained},
		reviewModel.StateRechecking, map[string]any{"reason": nil, "failures": 0, "retries": 0, "checked_by": by}, &by)
	if err != nil || moved == 0 {
		if err == nil {
			err = ErrRecheckRunning
		}
		return err
	}
	inBackground(ctx, "re-check", job.Id, func(ctx context.Context) (fmt.Stringer, error) {
		return recheckReport{Imports: 1, Settled: 1}, recheckImport(ctx, job)
	})
	return nil
}

// proveSource proves cred's connection reaches the import's own source, by
// what its re-check reads: its people (samePeople) or its tasks
// (sameSource).
func proveSource(ctx context.Context, job *importModels.Job, cred uuid.UUID) error {
	source := importProvider.Get(job.Provider)
	if rechecks[job.Provider].people != "" {
		rp, ok := source.(importProvider.PeopleRechecker)
		if !ok {
			return ErrNotRecheckable
		}
		_, err := samePeople(ctx, rp, job, cred)
		return err
	}
	rc, ok := source.(importProvider.Rechecker)
	if !ok {
		return ErrNotRecheckable
	}
	return sameSource(ctx, rc, job, cred)
}

// jobAwaitingAdmin is the import an admin acts on, refused when it has no
// review in one of states, or one being re-checked.
func jobAwaitingAdmin(ctx context.Context, importID uuid.UUID, states ...string) (*importModels.Job, error) {
	revs, err := reviewModel.AccessReviewsOf(ctx, importID)
	if err != nil {
		return nil, err
	}
	waiting := false
	for _, r := range revs {
		if r.State == reviewModel.StateRechecking {
			return nil, ErrRecheckRunning
		}
		waiting = waiting || slices.Contains(states, r.State)
	}
	if !waiting {
		return nil, ErrNotRecheckable
	}
	return importModels.GetJob(ctx, importID)
}

// inBackground runs an admin's action on an import after its request ends,
// logging what it did or why it stopped.
func inBackground(ctx context.Context, what string, importID uuid.UUID, do func(context.Context) (fmt.Stringer, error)) {
	helpers.GoSafeNamed("import."+what, func() {
		done, err := do(context.WithoutCancel(ctx))
		switch {
		case errors.Is(err, errRecheckWaits):
			helpers.LogInfoWithContext(ctx, "Admin's %s of import %s: another server is working on it", what, importID)
		case err != nil:
			helpers.LogErrorWithContext(ctx, "Admin's %s of import %s stopped: %v", what, importID, err)
		default:
			helpers.LogInfoWithContext(ctx, "Admin's %s of import %s: %s", what, importID, done)
			releaseInBackground(ctx)
		}
	})
}

// RecheckUndone is what an admin's undo of an import's re-check put back:
// tasks opened to their project again, assignees given their task back,
// comments shown again; the changes left as they are because someone changed
// the task or comment since; and those older than UndoWindow, left too.
type RecheckUndone struct {
	Opened     int `json:"opened"`
	Reassigned int `json:"reassigned"`
	Shown      int `json:"shown"`
	LeftAlone  int `json:"left_alone"`
	Expired    int `json:"expired"`
}

func (u RecheckUndone) String() string {
	return fmt.Sprintf("%d tasks opened, %d assignees back, %d comments shown; %d changed since and %d too old, left as they are",
		u.Opened, u.Reassigned, u.Shown, u.LeftAlone, u.Expired)
}

// UndoRecheck puts back, as the admin by, what an import's re-check or an
// admin's containment changed in the last UndoWindow, where it is still as
// the change left it, and marks its reviews undone, with what it put back.
// It refuses at once an import with nothing to undo, or one being
// re-checked, and undoes in the background.
func UndoRecheck(ctx context.Context, importID, by uuid.UUID) error {
	job, err := jobAwaitingAdmin(ctx, importID, reviewModel.StateRechecked, reviewModel.StateContained)
	if err != nil {
		return err
	}
	if rechecks[job.Provider].tasks == "" {
		// A re-check of people changes nothing to put back: it lists them
		// for an admin, who decides about each (business/ImportReview).
		return ErrNotRecheckable
	}
	inBackground(ctx, "undo", job.Id, func(ctx context.Context) (fmt.Stringer, error) {
		return undoRecheck(ctx, job.Id, by)
	})
	return nil
}

// undoRecheck is UndoRecheck's work, under the import's lock.
func undoRecheck(ctx context.Context, importID, by uuid.UUID) (done RecheckUndone, err error) {
	unlock, err := lockImport(ctx, importID)
	if err != nil {
		return done, err
	}
	defer unlock()
	ctx = recheckContext(ctx)
	admin, err := userDomain.ActorByUUID(ctx, by)
	if err != nil {
		return done, err
	}
	changes, err := reviewModel.AccessChangesOf(ctx, importID)
	if err != nil {
		return done, err
	}
	who := taskBusiness.Changer{By: by, UserUUID: admin.UserDgraphInfo.Uuid, Record: recordAs(ctx, &by, importID, nil)}
	restrictedBy := map[uuid.UUID]*reviewModel.AccessChange{}
	for i := range changes {
		if changes[i].Kind == reviewModel.ChangeRestricted {
			restrictedBy[changes[i].Entity] = &changes[i]
		}
	}
	now := time.Now()
	var putBack []uuid.UUID
	defer func() {
		// What was put back is recorded once, even when a later change failed.
		if _, rerr := reviewModel.UndoAccessChanges(ctx, putBack); rerr != nil && err == nil {
			err = rerr
		}
	}()
	for i := range changes {
		ch := &changes[i]
		if now.Sub(ch.CreatedAt) > UndoWindow {
			done.Expired++
			continue
		}
		put, err := undoChange(ctx, ch, restrictedBy[ch.Entity], who, &admin.UserDgraphInfo)
		if err != nil {
			return done, err
		}
		if !put {
			done.LeftAlone++
			continue
		}
		putBack = append(putBack, ch.ID)
		switch ch.Kind {
		case reviewModel.ChangeRestricted:
			done.Opened++
		case reviewModel.ChangeUnassigned:
			done.Reassigned++
		case reviewModel.ChangeCommentHidden:
			done.Shown++
		}
	}
	_, err = reviewModel.MoveAccessReviews(ctx, importID, []string{reviewModel.StateRechecked, reviewModel.StateContained},
		reviewModel.StateUndone, map[string]any{"undone": done}, &by)
	return done, err
}

// undoChange puts one change back where its task or comment is still as the
// change left it, whatever the state of the task or its project (in the
// trash, or archived: it comes back with them, ruling G), and answers whether
// it did. kept is the restriction change of the change's task, if any.
func undoChange(ctx context.Context, ch, kept *reviewModel.AccessChange, who taskBusiness.Changer, admin *dgraphStruct.DgraphUser) (bool, error) {
	switch ch.Kind {
	case reviewModel.ChangeRestricted:
		dg, err := readTask(ctx, ch.Entity, admin.Uid)
		if err != nil || dg == nil {
			return false, err
		}
		r, err := restrictionModel.Of(ctx, ch.Entity)
		if err != nil || !keptByUs(r, ch) {
			return false, err
		}
		return setRestriction(ctx, dg, false, nil, who, time.Now())
	case reviewModel.ChangeUnassigned:
		dg, err := readTask(ctx, ch.Entity, admin.Uid)
		if err != nil || dg == nil || dg.Assignee != nil || dg.Project == nil {
			return false, err
		}
		// An assignee reads a restricted task, so on one someone has
		// restricted since they'd read it past the people that person chose:
		// they come back only to a task that is open, or still kept as the
		// re-check left it.
		if r, err := restrictionModel.Of(ctx, ch.Entity); err != nil || (r.Restricted && !keptByUs(r, kept)) {
			return false, err
		}
		var before struct {
			Assignee string `json:"assignee"`
		}
		if err := json.Unmarshal(ch.Before, &before); err != nil || before.Assignee == "" {
			return false, nil
		}
		// Someone who has left the project since, or can't be given work
		// any more, stays off it.
		in, err := projectBusiness.MembersAmong(ctx, dg.Project.Uuid, []string{before.Assignee})
		if err != nil || !in[before.Assignee] {
			return false, err
		}
		person, err := userDomain.GetDgraphUserInfoByUUID(ctx, before.Assignee)
		if err != nil || person == nil || !principalBusiness.CanBeAssigned(person).Allowed {
			return false, err
		}
		if helpers.IsSoftDeleted(dg.DeletedAt) {
			return true, taskBusiness.AssignInTrash(ctx, dg, person, admin)
		}
		return true, taskBusiness.UpdateTaskAssigneeByTaskUUID(taskBusiness.DataMove(ctx), ch.Entity, person, "", dg.Uid, dg, admin)
	case reviewModel.ChangeCommentHidden:
		var after struct {
			HiddenAt time.Time `json:"hidden_at"`
			Task     uuid.UUID `json:"task"`
		}
		if err := json.Unmarshal(ch.After, &after); err != nil || after.HiddenAt.IsZero() {
			return false, nil
		}
		dg, err := readTask(ctx, after.Task, admin.Uid)
		if err != nil || dg == nil {
			return false, err
		}
		project := ""
		if dg.Project != nil {
			project = dg.Project.Uuid
		}
		return taskBusiness.ShowTaskComment(ctx, ch.Entity, dg.Uuid, project, after.HiddenAt, hiddenBy, awayOf(dg))
	}
	return false, nil
}

// readTask reads a task as the system, in the trash too; nil when the graph
// has no such task.
func readTask(ctx context.Context, taskID uuid.UUID, readerUID string) (*dgraphStruct.DgraphTask, error) {
	dg, err := taskDomain.GetDgraphBasicTaskInfoByUUID(ctx, taskID.String(), readerUID)
	if errors.Is(err, helpers.ErrNotFound) || (err == nil && (dg == nil || dg.Uuid == "")) {
		return nil, nil
	}
	return dg, err
}

// awayOf is where a task is out of sight, for a comment that comes back on
// it: archived with its project, whose archive took the comment's workspace
// memory too, and in the trash since it went there; or in sight. Pure.
func awayOf(dg *dgraphStruct.DgraphTask) businessComment.Away {
	var away businessComment.Away
	if dg.Project != nil && helpers.IsSoftDeleted(dg.Project.DeletedAt) {
		away = businessComment.Away{Since: *dg.Project.DeletedAt, Archived: true}
	}
	if helpers.IsSoftDeleted(dg.DeletedAt) {
		away.Since = *dg.DeletedAt
	}
	return away
}

// restrictedAsLeft is whether a task is still restricted as a recorded
// change left it: on its own (not with a task above it), to exactly the
// people the change let in. Pure.
func restrictedAsLeft(r restrictionModel.Restriction, ch *reviewModel.AccessChange) bool {
	var after struct {
		People []uuid.UUID `json:"people"`
	}
	return r.Restricted && r.Via == uuid.Nil && json.Unmarshal(ch.After, &after) == nil && sameIDs(r.Grants, after.People)
}

// sameIDs is whether a and b hold the same people. Pure.
func sameIDs(a, b []uuid.UUID) bool {
	if len(a) != len(b) {
		return false
	}
	for _, id := range a {
		if !slices.Contains(b, id) {
			return false
		}
	}
	return true
}

// ContainImport keeps, as the admin by, each task of an import whose
// reviews wait for an admin to the project's admins, its assignee and its
// creator until the import is re-checked (a restriction that lets nobody in
// by name), but those someone restricted already, and marks its reviews
// contained, with how many it kept. Each is recorded first, so UndoRecheck
// opens them again, and a later re-check replaces it. It refuses at once an
// import that doesn't wait for an admin, and contains in the background.
func ContainImport(ctx context.Context, importID, by uuid.UUID) error {
	job, err := jobAwaitingAdmin(ctx, importID, reviewModel.StateNeedsAdmin)
	if err != nil {
		return err
	}
	if rechecks[job.Provider].tasks == "" {
		// An import of people has no tasks to keep: its people are listed
		// for an admin already (toAdmins).
		return ErrNotRecheckable
	}
	inBackground(ctx, "containment", job.Id, func(ctx context.Context) (fmt.Stringer, error) {
		n, err := containImport(ctx, job, by)
		return contained(n), err
	})
	return nil
}

// contained is how many tasks a containment kept.
type contained int

func (n contained) String() string {
	return fmt.Sprintf("%d tasks kept to their projects' admins", int(n))
}

// containImport is ContainImport's work, under the import's lock.
func containImport(ctx context.Context, job *importModels.Job, by uuid.UUID) (int, error) {
	unlock, err := lockImport(ctx, job.Id)
	if err != nil {
		return 0, err
	}
	defer unlock()
	ctx = recheckContext(ctx)
	admin, err := userDomain.ActorByUUID(ctx, by)
	if err != nil {
		return 0, err
	}
	revs, err := reviewModel.AccessReviewsOf(ctx, job.Id)
	if err != nil {
		return 0, err
	}
	who := taskBusiness.Changer{By: by, UserUUID: admin.UserDgraphInfo.Uuid, Record: recordAs(ctx, &by, job.Id, nil)}
	g, kept := rechecks[job.Provider], 0
	for i := range revs {
		rev := &revs[i]
		if rev.Gap != g.tasks || rev.State != reviewModel.StateNeedsAdmin {
			continue
		}
		n, err := containProject(ctx, job.Id, rev, who, &admin.UserDgraphInfo)
		kept += n
		if err != nil {
			return kept, err
		}
	}
	_, err = reviewModel.MoveAccessReviews(ctx, job.Id, []string{reviewModel.StateNeedsAdmin}, reviewModel.StateContained,
		map[string]any{"contained": kept}, &by)
	return kept, err
}

// containProject keeps one project's tasks of an import to its admins,
// a page at a time, sub-tasks first.
func containProject(ctx context.Context, importID uuid.UUID, rev *reviewModel.AccessReview, who taskBusiness.Changer, admin *dgraphStruct.DgraphUser) (int, error) {
	kept, after := 0, recheckCursor{}
	for {
		page, err := importModels.MappedTasksIn(ctx, importID, rev.ProjectUUID, after.Entity, after.Source, recheckPage)
		if err != nil || len(page) == 0 {
			return kept, err
		}
		ids := make([]uuid.UUID, len(page))
		for i, t := range page {
			ids[i] = t.TaskID
		}
		restrictions, err := restrictionModel.OfMany(ctx, ids)
		if err != nil {
			return kept, err
		}
		for _, t := range page {
			if restrictions[t.TaskID].Restricted {
				continue
			}
			// One in the trash too: a restore brings it back as it is.
			dg, err := readTask(ctx, t.TaskID, admin.Uid)
			if err != nil {
				return kept, err
			}
			if dg == nil {
				continue
			}
			if _, err := reviewModel.AddAccessChange(ctx, rev.ID, reviewModel.ChangeRestricted, t.TaskID,
				map[string]any{"restricted": false}, map[string]any{"people": []uuid.UUID{}, "contained": true, "pending": true}); err != nil {
				return kept, err
			}
			if _, err := setRestriction(ctx, dg, true, nil, who, time.Now()); err != nil {
				return kept, err
			}
			if err := reviewModel.SettleAccessChange(ctx, rev.ID, reviewModel.ChangeRestricted, t.TaskID); err != nil {
				return kept, err
			}
			kept++
		}
		last := page[len(page)-1]
		after = recheckCursor{Entity: last.Entity, Source: last.SourceID}
		if len(page) < recheckPage {
			return kept, nil
		}
	}
}
