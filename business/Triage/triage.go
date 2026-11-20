package business

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	taskKeys "github.com/akashc777/OneCamp/business/TaskKeys"
	taskWriter "github.com/akashc777/OneCamp/business/TaskWriter"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	triageModel "github.com/akashc777/OneCamp/models/postgres/Triage"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/taskread"
	taskReadGeneration "github.com/akashc777/OneCamp/models/taskread/generation"
	"github.com/google/uuid"
)

func init() {
	taskread.Waiting = waitingNow
}

// MaxWaiting bounds what waits in one project's triage. Past it, new work
// goes straight to the project, as with triage off, so the lists' filter of
// what waits stays short.
const MaxWaiting = 500

// MaxReason bounds a decision's reason, as triage_decisions keeps it.
const MaxReason = 500

// The longest a snooze lasts.
const maxSnooze = 365 * 24 * time.Hour

// Source is the door a task came in by.
type Source string

const (
	// FromForm is a form's visitor.
	FromForm Source = "form"
	// FromAPI is an API token or MCP, acting as its member.
	FromAPI Source = "api"
	// FromAgent is an AI teammate making work as its own principal.
	FromAgent Source = "agent"
	// FromGitHub is an issue opened on a linked repository.
	FromGitHub Source = "github"
	// FromChat is a task proposed from a conversation and made by a member.
	FromChat Source = "chat"
	// FromImport is a task an import brought in waiting in its source's
	// triage.
	FromImport Source = "import"
	// FromMail is a message that arrived in a project's connected mailbox
	// (business/TriageMail): a member's when the sender's address is one,
	// else an outsider's.
	FromMail Source = "mail"
)

// Sources is every door, as migration 595's check on triage_items.source
// takes them.
var Sources = []Source{FromForm, FromAPI, FromAgent, FromGitHub, FromChat, FromImport, FromMail}

// Requester is who asked for a task: a member, or a form's visitor by the
// name and email they gave (either may be empty).
type Requester struct {
	UserUUID *uuid.UUID
	Name     string
	Email    string
}

// Request is a task waiting in triage as its queue shows it: the item, and
// the task's name and status as the reader may read them.
type Request struct {
	triageModel.Item
	Name   string `json:"name"`
	Status string `json:"status"`
}

// Seams for tests.
var (
	triageOn = func(ctx context.Context, projectID string) bool {
		return policyBusiness.Policies.BoolIn(ctx, policyBusiness.ProjectsTriage, policyBusiness.Scope{Kind: policyBusiness.ScopeProject, ID: projectID})
	}
	needsPriority = func(ctx context.Context, projectID string) bool {
		return policyBusiness.Policies.BoolIn(ctx, policyBusiness.ProjectsTriagePriority, policyBusiness.Scope{Kind: policyBusiness.ScopeProject, ID: projectID})
	}
	primeProjects = func(ctx context.Context, ids []string) {
		policyBusiness.Policies.PrimeScopes(ctx, policyBusiness.ScopeProject, ids)
	}
	put           = triageModel.PutAll
	countWaiting  = triageModel.CountWaiting
	countAwake    = triageModel.CountAwake
	countAwakeFor = triageModel.CountAwakeFor
	waitingIn     = triageModel.Waiting
	allWaiting    = triageModel.AllWaiting
	keep          = triageModel.Decide
	projectFor    = func(ctx context.Context, projectID string, who *userModels.UserInfo) (*dgraphStruct.DgraphProject, error) {
		return projectBusiness.GetBasicDgraphProjectInfo(ctx, projectID, who.UserDgraphInfo.Uid)
	}
	standingIn = func(ctx context.Context, who *userModels.UserInfo, p *dgraphStruct.DgraphProject) (workAccess.Standing, error) {
		return workAccess.Of(ctx, who.UserDgraphInfo.Uuid, p)
	}
	readable = taskBusiness.ReadableTasks
	taskFor  = taskBusiness.TaskFor
	apply    = taskWriter.Tasks.Apply
	comment  = taskWriter.Comment
	emit     = func(ctx context.Context, e resourceEventBusiness.Event) { resourceEventBusiness.Bus.Emit(ctx, e) }
	now      = time.Now
	// Who may read a task now (the one rule), a notice, and a task's name
	// with its key.
	readersOf = taskDomain.Readers
	tell      = notificationBusiness.Tell
	named     = taskKeys.NamedTask
)

// Hold puts a task that has just come in from outside its project's board
// (by one of the Sources) into the project's triage, when the project has triage
// on and room. The door calls it once the task is made; held false is the
// task being the team's work at once. An error is the item not stored: the
// task is made either way, so the door logs it rather than failing a request
// whose retry would make the task twice.
func Hold(ctx context.Context, projectID, taskID uuid.UUID, src Source, from Requester) (held bool, err error) {
	n, err := HoldAll(ctx, projectID, src, []Arrival{{TaskID: taskID, From: from}})
	return n == 1, err
}

// Arrival is one task a door has made, and who asked for it.
type Arrival struct {
	TaskID uuid.UUID
	From   Requester
}

// HoldAll is Hold for a batch from one door: one gate (triage on, one count
// of what waits) and one insert, however many arrive; what doesn't fit the
// room is the team's work at once. held is how many wait in triage, the
// first ones. Every door comes in here (Hold is its one-item case).
func HoldAll(ctx context.Context, projectID uuid.UUID, src Source, in []Arrival) (held int, err error) {
	if len(in) == 0 || !triageOn(ctx, projectID.String()) {
		return 0, nil
	}
	n, err := countWaiting(ctx, projectID)
	if err != nil {
		return 0, err
	}
	if room := MaxWaiting - n; len(in) > room {
		in = in[:max(room, 0)]
	}
	if len(in) == 0 {
		return 0, nil
	}
	items := make([]triageModel.Item, len(in))
	for i, a := range in {
		items[i] = triageModel.Item{
			TaskUUID: a.TaskID, ProjectUUID: projectID, Source: string(src),
			RequesterUUID: a.From.UserUUID, RequesterName: bounded(a.From.Name, 200), RequesterEmail: bounded(a.From.Email, 320),
		}
	}
	if err := put(ctx, items); err != nil {
		return 0, err
	}
	for _, a := range in {
		arrived(ctx, projectID, a.TaskID, src, a.From)
	}
	return len(in), nil
}

// Room is how many more a project's triage takes now (MaxWaiting less what
// waits), and whether its triage is on: a door that fetches its work (the
// mail door) asks first, so it takes no more than will wait. Off is no room.
func Room(ctx context.Context, projectID uuid.UUID) (room int, on bool, err error) {
	if !triageOn(ctx, projectID.String()) {
		return 0, false, nil
	}
	n, err := countWaiting(ctx, projectID)
	if err != nil {
		return 0, true, err
	}
	return max(MaxWaiting-n, 0), true, nil
}

// WaitsNote is how an answer that made a task says it waits in triage.
const WaitsNote = "; it waits in the project's triage until someone accepts it"

// Arrive is Hold for a door that has made its task: an item that didn't
// stick is logged as where's, and the task is the team's work at once (a
// retry would make the task twice). held is the task waiting in triage.
func Arrive(ctx context.Context, where string, projectID, taskID uuid.UUID, src Source, from Requester) (held bool) {
	held, err := Hold(ctx, projectID, taskID, src, from)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "%s: triage err: %+v", where, err)
	}
	return held
}

// keptWaiting keeps what waits in triage for the generation of the
// decision's rows (models/taskread/generation).
var keptWaiting = taskReadGeneration.NewCache[map[uuid.UUID]uuid.UUID]()

// waitingNow is what waits in triage for the lists' filter (taskread
// Waiting): every waiting task whose project has triage on. Turning a
// project's triage off lets what waits into its lists, undecided; turning it
// on again puts it back in the queue.
func waitingNow(ctx context.Context) (map[string]bool, error) {
	all, err := keptWaiting.Get(ctx, "", allWaiting)
	if errors.Is(err, triageModel.ErrTooManyWaiting) {
		helpers.LogErrorWithContext(ctx, "business/Triage waitingNow: %v", err)
	}
	if err != nil || len(all) == 0 {
		return nil, err
	}
	projects := map[string]bool{}
	for _, p := range all {
		projects[p.String()] = true
	}
	ids := make([]string, 0, len(projects))
	for p := range projects {
		ids = append(ids, p)
	}
	primeProjects(ctx, ids)
	for p := range projects {
		projects[p] = triageOn(ctx, p)
	}
	out := make(map[string]bool, len(all))
	for task, p := range all {
		if projects[p.String()] {
			out[task.String()] = true
		}
	}
	return out, nil
}

// On is whether a project has triage on: its projects.triage policy, set for
// it or for the workspace.
func On(ctx context.Context, projectID uuid.UUID) bool {
	return triageOn(ctx, projectID.String())
}

type queue struct{}

// mayIn checks who may do need in the project, as the Task Writer does: one
// who can't see it is told it isn't there.
func mayIn(ctx context.Context, who *userModels.UserInfo, projectID uuid.UUID, need workAccess.Need) error {
	if who == nil {
		return taskBusiness.ErrTaskNotVisible
	}
	p, err := projectFor(ctx, projectID.String(), who)
	if err != nil {
		return err
	}
	if p == nil || p.Uuid == "" {
		return taskBusiness.ErrTaskNotVisible
	}
	s, err := standingIn(ctx, who, p)
	if err != nil {
		return err
	}
	return can(s, need)
}

// allowed is mayIn for a standing already read (a route's check, handed
// down): a standing in another project is told this one isn't there. Pure.
func allowed(s workAccess.Standing, projectID uuid.UUID, need workAccess.Need) error {
	if !strings.EqualFold(s.ProjectUUID, projectID.String()) {
		return taskBusiness.ErrTaskNotVisible
	}
	return can(s, need)
}

// can is whether standing s may do need: one who can't see the project is
// told it isn't there. Pure.
func can(s workAccess.Standing, need workAccess.Need) error {
	if !s.Can(workAccess.NeedView) {
		return taskBusiness.ErrTaskNotVisible
	}
	return s.Check(need)
}

func (queue) Waiting(ctx context.Context, who *userModels.UserInfo, projectID uuid.UUID) ([]Request, error) {
	if err := mayIn(ctx, who, projectID, workAccess.NeedView); err != nil {
		return nil, err
	}
	if !triageOn(ctx, projectID.String()) {
		return nil, nil
	}
	items, err := waitingIn(ctx, projectID, MaxWaiting)
	if err != nil || len(items) == 0 {
		return nil, err
	}
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.TaskUUID.String()
	}
	tasks, err := readable(ctx, ids, who.UserDgraphInfo.Uid)
	if err != nil {
		return nil, err
	}
	out := make([]Request, 0, len(items))
	for _, it := range items {
		// A task deleted while it waited, or kept from this reader, isn't shown.
		if t, ok := tasks[it.TaskUUID.String()]; ok {
			out = append(out, Request{Item: it, Name: t.Name, Status: t.Status})
		}
	}
	return out, nil
}

func (queue) Decide(ctx context.Context, who *userModels.UserInfo, taskID uuid.UUID, c Choice) error {
	if who == nil {
		return taskBusiness.ErrTaskNotVisible
	}
	c.Reason = bounded(strings.TrimSpace(c.Reason), MaxReason)
	d := triageModel.Decision{TaskUUID: taskID, Decision: string(c.Decision), Reason: c.Reason, DecidedBy: who.UserPostgresInfo.Id}
	switch c.Decision {
	case Accept, Decline:
	case Duplicate:
		if c.DuplicateOf == uuid.Nil || c.DuplicateOf == taskID {
			return fmt.Errorf("%w: a duplicate names the task it repeats", ErrBadChoice)
		}
		d.DuplicateOf = &c.DuplicateOf
	case Snooze:
		at := now()
		if !c.Until.After(at) || c.Until.Sub(at) > maxSnooze {
			return fmt.Errorf("%w: a snooze lasts until a time within a year", ErrBadChoice)
		}
		until := c.Until.UTC()
		d.SnoozedUntil = &until
	default:
		return fmt.Errorf("%w: a decision is accept, duplicate, decline or snooze", ErrBadChoice)
	}
	// Deciding changes the task: who decides must be able to.
	task, _, err := taskFor(ctx, taskID, who, workAccess.NeedEditTask)
	if err != nil {
		return err
	}
	if task == nil || task.Project == nil {
		return taskBusiness.ErrTaskNotVisible
	}
	projectID, err := uuid.Parse(task.Project.Uuid)
	if err != nil {
		return taskBusiness.ErrTaskNotVisible
	}
	if !triageOn(ctx, projectID.String()) {
		return ErrTriageOff
	}
	if c.Decision == Accept && task.Priority == "" && needsPriority(ctx, projectID.String()) {
		return ErrNeedsPriority
	}
	d.ProjectUUID = projectID
	var original *dgraphStruct.DgraphTask
	if c.Decision == Duplicate {
		if original, _, err = taskFor(ctx, c.DuplicateOf, who, workAccess.NeedComment); err != nil {
			return fmt.Errorf("%w: the task it repeats isn't one you can see", ErrBadChoice)
		}
	}
	// The task changes while its item is held, before the decision is kept:
	// of two people deciding at once, the second finds it decided and changes
	// nothing; a change refused (a status that waits for approval, say) leaves
	// the task waiting.
	change := func(ctx context.Context) error {
		switch c.Decision {
		case Accept:
			if c.Status != "" {
				return setStatus(ctx, who, taskID, c.Status)
			}
		case Duplicate, Decline:
			return setStatus(ctx, who, taskID, dgraphStruct.TASK_STATUS_CANCELED)
		}
		return nil
	}
	found, requester, err := keep(ctx, d, change)
	if err != nil {
		return err
	}
	if !found {
		return ErrNotWaiting
	}
	switch c.Decision {
	case Duplicate:
		link(ctx, who, taskID, original)
		say(ctx, who, original, fmt.Sprintf("Asked for again in triage: “%s” was marked a duplicate of this.", task.Name))
	case Decline:
		words := "Declined in triage."
		if c.Reason != "" {
			words = "Declined in triage: " + c.Reason
		}
		say(ctx, who, task, words)
	}
	emit(ctx, resourceEventBusiness.Event{
		Kind: resourceEventBusiness.TriageDecided, ResourceKind: "task", ResourceUUID: taskID.String(),
		Actor: resourceEventBusiness.Actor{UserUUID: who.UserDgraphInfo.Uuid}, At: now(),
		Data: map[string]string{"decision": string(c.Decision), "project_uuid": projectID.String()},
	})
	tellRequester(ctx, who, task, requester, c, original)
	return nil
}

// tellRequester tells the member who asked for a task how triage decided it,
// in their Inbox, while they may read the task now (taskDomain.Readers, the
// one rule, as business/Approvals tells its requester): a decline with its
// reason, a duplicate naming the original while they may read it too
// (originalFor). A form's visitor or an issue's opener is nobody the
// workspace knows; one who decided it themselves knows;
// a snooze decides nothing yet. The words are plain text, as people wrote
// them: the Inbox and the email escape what they show.
func tellRequester(ctx context.Context, who *userModels.UserInfo, task *dgraphStruct.DgraphTask, requester *uuid.UUID, c Choice, original *dgraphStruct.DgraphTask) {
	if requester == nil || *requester == who.UserPostgresInfo.Id || c.Decision == Snooze {
		return
	}
	to := requester.String()
	readers, err := readersOf(ctx, task.Uuid, []string{to})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Triage: who may read task %s couldn't be read, so nobody is told: %+v", task.Uuid, err)
		return
	}
	if !readers[to] {
		return
	}
	title := named(ctx, task.Uuid, task.Name)
	by := who.UserDgraphInfo.DisplayName()
	words, body := by+" accepted your request "+title, c.Reason
	switch c.Decision {
	case Decline:
		words = by + " declined your request " + title
	case Duplicate:
		words = by + " marked your request " + title + " a duplicate"
		body = "It repeats " + originalFor(ctx, to, original) + "."
		if c.Reason != "" {
			body += " " + c.Reason
		}
	}
	tell(ctx, notificationBusiness.Notice{
		Kind: "work.triage.decided", ResourceKind: workAccess.KindTask, ResourceUUID: task.Uuid, ResourceTitle: title,
		Actor: &who.UserDgraphInfo, Data: map[string]string{"decision": string(c.Decision)},
		DedupKey: "triage|" + task.Uuid + "|" + string(c.Decision),
		Title:    words, Body: body, Path: "/app/task/" + task.Uuid,
	}, []notificationBusiness.NoticeRecipient{{UserUUID: to, Reason: "subject"}})
}

// originalFor is how a duplicate's notice names its original to the person
// told: by its key and name while they may read it too (the one rule, as for
// the task itself), else as "another task", since the decider need only
// comment on the original and it may be restricted or in a project the
// requester isn't in.
func originalFor(ctx context.Context, to string, original *dgraphStruct.DgraphTask) string {
	readers, err := readersOf(ctx, original.Uuid, []string{to})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Triage: who may read task %s couldn't be read, so the notice doesn't name it: %+v", original.Uuid, err)
	}
	if err != nil || !readers[to] {
		return "another task"
	}
	return named(ctx, original.Uuid, original.Name)
}

// setStatus moves a task into a status through the Task Writer, by the
// status's key or name.
func setStatus(ctx context.Context, who *userModels.UserInfo, taskID uuid.UUID, status string) error {
	v, _ := json.Marshal(status)
	_, _, err := apply(ctx, who, taskID, []taskWriter.Op{{Kind: taskWriter.OpStatus, Value: v}}, 0)
	return err
}

// link merges a duplicate into its original once the decision is kept: the
// task is linked as duplicating it, through the Task Writer as the person
// deciding (who may be refused a link on a task they can only comment on).
// The decision stands if the link fails, so it is logged.
func link(ctx context.Context, who *userModels.UserInfo, taskID uuid.UUID, original *dgraphStruct.DgraphTask) {
	v, _ := json.Marshal(map[string]string{"kind": taskDomain.LinkDuplicates, "task": original.Uuid, "direction": "out"})
	if _, _, err := apply(ctx, who, taskID, []taskWriter.Op{{Kind: taskWriter.OpRelation, Value: v}}, 0); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Triage: task %s, marked a duplicate of %s by %s, wasn't linked to it (the decision stands): %+v",
			taskID, original.Uuid, who.UserDgraphInfo.Uuid, err)
	}
}

// say comments on a task once a decision is kept. The decision stands if the
// comment fails, so it is logged rather than turned into a failure that a
// retry would meet as "not waiting".
func say(ctx context.Context, who *userModels.UserInfo, task *dgraphStruct.DgraphTask, words string) {
	id, err := uuid.Parse(task.Uuid)
	if err == nil {
		_, err = comment(ctx, who, id, words)
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Triage: %s's comment on task %s after a decision wasn't kept (the decision stands): %+v",
			who.UserDgraphInfo.Uuid, task.Uuid, err)
	}
}

// bounded cuts s to at most n bytes on a character boundary.
func bounded(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
