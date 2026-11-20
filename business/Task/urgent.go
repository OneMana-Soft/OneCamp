package business

// Urgent tells whom a task is given to at once, by push and by email, as
// Linear does (linear-04 item 1).
//
// A change tells only when it makes a task Urgent: Urgent again says nothing,
// and Urgent, then High, then Urgent again tells twice, since each change into
// Urgent is its own (the dedup key names the change's activity, so the same
// change told twice tells once). Making a task, an import, a sync or the demo's
// seed is no change into Urgent: a move of data (helpers.Quiet) tells nobody.
//
// Only the task's assignee is told, never whoever made the change, and only
// while they hear of the project's work for them (their setting for the project
// is everything or mentions; taking someone off a project takes their setting)
// and may read the task as one of its project's people (domain/Task
// MemberReaders): the one hearer check, AssigneeHears (hears.go). They are
// told as every notice tells (business/Notification Tell): in their Inbox, by
// push through their holds (a pause, quiet hours, focus time) and by email by
// their choices (the "tasks assigned to you" switch), so with the workspace's
// email off, by push alone.
//
// One request or one job is one batch: a bulk change, a rule's run (one task
// or each of many, or a due sweep's runs) and an Undo of a run. Each assignee
// is told of what the batch made Urgent for them once: one task by itself, two
// or more in one push and one email naming them, each task still its own Inbox
// item, which opens it.
//
// One action is told as one batch too: a bulk change and the rule runs its
// events set going (helpers.WithOrigin; the hand-off to the workers carries
// it). Each task is in its assignee's Inbox at once, and each assignee is
// pushed and emailed once when the action's window ends (urgentWait). The
// window is in memory, per process: on a split install the API and the workers
// each tell what they gathered. A graceful shutdown waits for an open window
// (helpers.Windows is work in flight); a crash or a hard kill inside one loses
// its push and email, never the Inbox item.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	notifylevel "github.com/akashc777/OneCamp/business/NotifyLevel"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// The Inbox kinds of being told a task is Urgent: one task, and the hidden row
// that keeps a batch's one push and email to a person once.
const (
	NoticeUrgent       = "task.urgent"
	NoticeUrgentDigest = "task.urgent.digest"
)

// urgentNamed is how many tasks a digest names before "and N more".
const urgentNamed = 10

// urgentChange is one task a change made Urgent: the task as it was read
// before the write (its name, project and assignee), the change's activity id,
// which names this change into Urgent, and who made it.
type urgentChange struct {
	task   *dgraphStruct.DgraphTask
	change string
	by     *dgraphStruct.DgraphUser
}

// becameUrgent is whether a change of priority from before to after makes the
// task Urgent. Pure.
func becameUrgent(before, after string) bool {
	return after == dgraphStruct.TASK_PRIORITY_URGENT && before != dgraphStruct.TASK_PRIORITY_URGENT
}

// urgentSeams are how people are told, and how the telling leaves the
// request: seams for tests.
var urgentSeams = struct {
	tell  func(ctx context.Context, n notificationBusiness.Notice, to []notificationBusiness.NoticeRecipient) []string
	async func(name string, f func())
}{
	tell:  notificationBusiness.Tell,
	async: helpers.GoSafeNamed,
}

type urgentKey struct{}

// urgentBatch is what a request or a job made Urgent so far, until it is
// told (done).
type urgentBatch struct {
	mu      sync.Mutex
	changes []urgentChange
	done    bool
}

// openBatch is ctx's batch while it is still gathering.
func openBatch(ctx context.Context) *urgentBatch {
	b, ok := ctx.Value(urgentKey{}).(*urgentBatch)
	if !ok {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.done {
		return nil
	}
	return b
}

// GatherUrgent makes ctx one batch (the file's comment) and answers the
// function that tells it, to be called when the request or the job is done.
// Inside a batch still gathering, it joins it: the outer one tells. A context
// outlives its job (an event's listeners run on after it, holding its values),
// so a batch already told takes nothing more: what comes after is told by
// itself, never lost.
func GatherUrgent(ctx context.Context) (context.Context, func()) {
	if openBatch(ctx) != nil {
		return ctx, func() {}
	}
	b := &urgentBatch{}
	return context.WithValue(ctx, urgentKey{}, b), func() {
		b.mu.Lock()
		changes := b.changes
		b.changes, b.done = nil, true
		b.mu.Unlock()
		tellUrgent(ctx, changes)
	}
}

// noteUrgent tells of changes now, as one batch, or adds them to ctx's batch.
func noteUrgent(ctx context.Context, changes ...urgentChange) {
	if len(changes) == 0 || helpers.Quiet(ctx) {
		return
	}
	if b, ok := ctx.Value(urgentKey{}).(*urgentBatch); ok {
		b.mu.Lock()
		if !b.done {
			b.changes = append(b.changes, changes...)
			b.mu.Unlock()
			return
		}
		b.mu.Unlock()
	}
	tellUrgent(ctx, changes)
}

// urgentWait is how long one action's Urgent changes gather before its
// assignees are pushed and emailed: a bulk change and the rule runs its events
// set going land within it.
const urgentWait = 5 * time.Second

// urgentReach is what one batch of an action put in an assignee's Inbox, to be
// pushed and emailed with the action's other batches when its window ends.
type urgentReach struct {
	ctx  context.Context
	who  string
	told []urgentChange
}

// urgentWindow gathers each action's batches, one window an action.
var urgentWindow = &helpers.Windows[urgentReach]{Wait: urgentWait, Flush: reachUrgent}

// tellUrgent tells a batch's assignees, off the request: the write has
// answered. A batch of an action (helpers.OriginOf) waits for the action's
// other batches before its push and email.
func tellUrgent(ctx context.Context, changes []urgentChange) {
	if len(changes) == 0 || helpers.Quiet(ctx) {
		return
	}
	ctx = context.WithoutCancel(ctx)
	if origin, ok := helpers.OriginOf(ctx); ok {
		urgentSeams.async("business/Task tellUrgent", func() { gatherUrgent(ctx, origin, changes) })
		return
	}
	urgentSeams.async("business/Task tellUrgent", func() { tellUrgentNow(ctx, changes) })
}

// tellUrgentNow tells each assignee of a batch, once, of the tasks it made
// Urgent for them that they hear of (urgentHears).
func tellUrgentNow(ctx context.Context, changes []urgentChange) {
	byWho, order := urgentByWho(ctx, changes)
	for _, who := range order {
		cs := byWho[who]
		if len(cs) == 1 {
			urgentSeams.tell(ctx, urgentNotice(cs[0]), []notificationBusiness.NoticeRecipient{{UserUUID: who, Reason: "assignee"}})
			continue
		}
		reachUrgentOnce(ctx, who, recordUrgent(ctx, who, cs))
	}
}

// gatherUrgent puts an action's batch in each assignee's Inbox at once, so a
// restart loses no Inbox item, and leaves the push and email to the action's
// window (reachUrgent).
func gatherUrgent(ctx context.Context, origin string, changes []urgentChange) {
	byWho, order := urgentByWho(ctx, changes)
	reach := make([]urgentReach, 0, len(order))
	for _, who := range order {
		if told := recordUrgent(ctx, who, byWho[who]); len(told) > 0 {
			reach = append(reach, urgentReach{ctx: ctx, who: who, told: told})
		}
	}
	urgentWindow.Add(origin, reach...)
}

// reachUrgent is an action's window ending: each assignee is pushed and
// emailed once of what all its batches put in their Inbox.
func reachUrgent(_ string, reach []urgentReach) {
	byWho := map[string][]urgentChange{}
	ctxs := map[string]context.Context{}
	var order []string
	for _, r := range reach {
		if _, seen := byWho[r.who]; !seen {
			order = append(order, r.who)
			ctxs[r.who] = r.ctx
		}
		byWho[r.who] = append(byWho[r.who], r.told...)
	}
	urgentSeams.async("business/Task reachUrgent", func() {
		for _, who := range order {
			reachUrgentOnce(ctxs[who], who, byWho[who])
		}
	})
}

// urgentByWho is a batch's changes by the assignee who hears of each
// (urgentHears), in the order first heard.
func urgentByWho(ctx context.Context, changes []urgentChange) (map[string][]urgentChange, []string) {
	byWho := map[string][]urgentChange{}
	var order []string
	for _, c := range changes {
		who, ok := urgentHears(ctx, c)
		if !ok {
			continue
		}
		if _, seen := byWho[who]; !seen {
			order = append(order, who)
		}
		byWho[who] = append(byWho[who], c)
	}
	return byWho, order
}

// recordUrgent puts each of cs in who's Inbox as its own item, told once by its
// change, and answers those newly told.
func recordUrgent(ctx context.Context, who string, cs []urgentChange) []urgentChange {
	var told []urgentChange
	for _, c := range cs {
		if len(urgentSeams.tell(ctx, urgentNotice(c), []notificationBusiness.NoticeRecipient{{UserUUID: who, Reason: "assignee", InboxOnly: true}})) > 0 {
			told = append(told, c)
		}
	}
	return told
}

// reachUrgentOnce pushes and emails who once of the tasks told them
// (urgentDigest: one task reads as itself).
func reachUrgentOnce(ctx context.Context, who string, told []urgentChange) {
	if len(told) > 0 {
		urgentSeams.tell(ctx, urgentDigest(told), []notificationBusiness.NoticeRecipient{{UserUUID: who, Reason: "assignee", Listed: true}})
	}
}

// urgentHears is the assignee c's task has, when they are to hear of it: the
// one hearer check (AssigneeHears), never whoever made the change. A guest
// given the task hears it was made Urgent: they read its priority.
func urgentHears(ctx context.Context, c urgentChange) (string, bool) {
	return AssigneeHears(ctx, c.task, userUUIDOf(c.by), GuestsHear)
}

// urgentWho is how a notice names who made the change.
func urgentWho(by *dgraphStruct.DgraphUser) string {
	if by != nil {
		if name := strings.TrimSpace(by.DisplayName()); name != "" {
			return name
		}
	}
	return "Someone"
}

// urgentNotice is one task made Urgent, as its assignee is told. Pure.
func urgentNotice(c urgentChange) notificationBusiness.Notice {
	t := c.task
	name := t.Name
	if name == "" {
		name = "a task"
	}
	body := ""
	if t.Project != nil {
		body = t.Project.Name
	}
	return notificationBusiness.Notice{
		Kind:          NoticeUrgent,
		ResourceKind:  "task",
		ResourceUUID:  t.Uuid,
		ResourceTitle: t.Name,
		Actor:         c.by,
		DedupKey:      NoticeUrgent + "|" + t.Uuid + "|" + c.change,
		Title:         urgentWho(c.by) + " marked " + name + " Urgent",
		Body:          body,
		Path:          "/app/task/" + t.Uuid,
		Email:         notificationBusiness.EventTaskUrgent,
		Push:          &notificationBusiness.Push{Type: firebaseInit.FIREBASE_PUSH_DATA_TYPE_TASK, ID: t.Uuid},
		// Its email is asked again at the send: the task, for its assignee.
		About: &notificationBusiness.About{Kind: notificationBusiness.AboutTask, Item: t.Uuid, Heard: notifylevel.Named},
	}
}

// urgentDigest is the one push and email of tasks a batch made Urgent for one
// person, naming them: its row is hidden (Listed), each task being its own
// Inbox item, and its key is the changes', so the same batch tells once. Pure.
func urgentDigest(cs []urgentChange) notificationBusiness.Notice {
	names := make([]string, 0, urgentNamed+1)
	tasks := make([]string, 0, urgentNamed)
	ids := make([]string, 0, len(cs))
	for i, c := range cs {
		ids = append(ids, c.change)
		if i < urgentNamed {
			tasks = append(tasks, c.task.Uuid)
			name := c.task.Name
			if name == "" {
				name = "a task"
			}
			names = append(names, name)
		}
	}
	if more := len(cs) - urgentNamed; more > 0 {
		names = append(names, fmt.Sprintf("and %d more", more))
	}
	slices.Sort(ids)
	first := cs[0]
	n := notificationBusiness.Notice{
		Kind:         NoticeUrgentDigest,
		ResourceKind: "task",
		ResourceUUID: first.task.Uuid,
		Actor:        first.by,
		Data:         map[string]string{"tasks": fmt.Sprint(len(cs))},
		DedupKey:     NoticeUrgentDigest + "|" + helpers.HashString(strings.Join(ids, ",")),
		Title:        fmt.Sprintf("%s marked %d tasks Urgent", urgentWho(first.by), len(cs)),
		Body:         strings.Join(names, ", "),
		Path:         "/app/inbox",
		Email:        notificationBusiness.EventTaskUrgent,
		Push:         &notificationBusiness.Push{Type: firebaseInit.FIREBASE_PUSH_DATA_TYPE_TASK},
		// Its email is asked again at the send of each task it names, and is
		// sent only while every one still holds.
		About: &notificationBusiness.About{Kind: notificationBusiness.AboutTask, Item: tasks[0], Items: tasks, Heard: notifylevel.Named},
	}
	// One left to tell (the others were told already): it reads, and opens,
	// as the one task.
	if len(cs) == 1 {
		one := urgentNotice(first)
		n.Title, n.Body, n.Path, n.Push, n.About = one.Title, one.Body, one.Path, one.Push, one.About
	}
	return n
}
