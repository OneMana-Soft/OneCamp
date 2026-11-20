package business

import (
	"context"
	"fmt"
	"html"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	notifylevel "github.com/akashc777/OneCamp/business/NotifyLevel"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	emailService "github.com/akashc777/OneCamp/services/Email"
	"github.com/google/uuid"
)

// urgentTelling is one call to Tell: what, to whom and how.
type urgentTelling struct {
	n  notificationBusiness.Notice
	to notificationBusiness.NoticeRecipient
}

// urgentWorld is a project whose people's settings for it are settings (by
// user id: "all", "mention", "nothing"; none for someone taken off it), who of
// them can't read its tasks, and what was told. Tell is as the real one in
// this: a key told to a person is told to them once. The setting read leaves
// the actor to urgentHears.
type urgentWorld struct {
	settings map[string]string
	unread   map[string]bool
	mu       sync.Mutex
	told     []urgentTelling
	keys     map[string]bool
}

func newUrgentWorld(t *testing.T) *urgentWorld {
	t.Helper()
	w := &urgentWorld{settings: map[string]string{}, unread: map[string]bool{}, keys: map[string]bool{}}
	seams, hearing, eligible := urgentSeams, hearSeams, newTaskEligible
	t.Cleanup(func() { urgentSeams, hearSeams, newTaskEligible = seams, hearing, eligible })
	urgentSeams.async = func(_ string, f func()) { f() }
	newTaskEligible = func(_ context.Context, _ string, _ string, mentioned []string, onlyMentioned bool) ([]string, error) {
		if !onlyMentioned {
			t.Errorf("an Urgent task is for its assignee: asked by mention only")
		}
		var out []string
		for _, id := range mentioned {
			if s := w.settings[id]; s == "all" || s == "mention" {
				out = append(out, id)
			}
		}
		return out, nil
	}
	hearSeams.readers = func(_ context.Context, _ string, ids []string) (map[string]bool, error) {
		out := map[string]bool{}
		for _, id := range ids {
			if !w.unread[id] {
				out[id] = true
			}
		}
		return out, nil
	}
	urgentSeams.tell = func(_ context.Context, n notificationBusiness.Notice, to []notificationBusiness.NoticeRecipient) []string {
		w.mu.Lock()
		defer w.mu.Unlock()
		var told []string
		for _, r := range to {
			if w.keys[n.DedupKey+"|"+r.UserUUID] {
				continue
			}
			w.keys[n.DedupKey+"|"+r.UserUUID] = true
			w.told = append(w.told, urgentTelling{n, r})
			told = append(told, r.UserUUID)
		}
		return told
	}
	return w
}

// person is someone in the project, hearing of everything in it.
func (w *urgentWorld) person(setting string) *dgraphStruct.DgraphUser {
	u := &dgraphStruct.DgraphUser{Uid: "0x" + uuid.NewString()[:6], Uuid: uuid.NewString(), UserName: "p"}
	if setting != "" {
		w.settings[u.Uuid] = setting
	}
	return u
}

// made is a change by by making a task named name, given to assignee, Urgent.
func made(name string, assignee, by *dgraphStruct.DgraphUser) urgentChange {
	return urgentChange{
		task: &dgraphStruct.DgraphTask{Uuid: uuid.NewString(), Name: name, Priority: "high", Assignee: assignee,
			Project: &dgraphStruct.DgraphProject{Uuid: "launch", Name: "Spring launch"}},
		change: uuid.NewString(),
		by:     by,
	}
}

// to is what one person was told, and of those how many pushes and emails
// went (a telling that isn't InboxOnly).
func (w *urgentWorld) to(who string) (all []urgentTelling, reached int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, t := range w.told {
		if t.to.UserUUID == who {
			all = append(all, t)
			if !t.to.InboxOnly {
				reached++
			}
		}
	}
	return all, reached
}

func TestUrgentTellsTheAssigneeOnce(t *testing.T) {
	w := newUrgentWorld(t)
	maya, ben := w.person("all"), w.person("all")
	c := made("Press kit", ben, maya)
	noteUrgent(context.Background(), c)
	got, reached := w.to(ben.Uuid)
	if len(got) != 1 || reached != 1 {
		t.Fatalf("Ben was told %d times, reached %d: want once", len(got), reached)
	}
	n, r := got[0].n, got[0].to
	if r.InboxOnly || r.Listed || r.NoEmail || r.NoPush || r.ThroughHolds {
		t.Errorf("Ben is told in the Inbox, by push and by email, through no hold: %+v", r)
	}
	if n.Kind != NoticeUrgent || n.Email != notificationBusiness.EventTaskUrgent || n.ResourceKind != "task" || n.ResourceUUID != c.task.Uuid {
		t.Errorf("notice %+v", n)
	}
	if n.Push == nil || n.Push.Type != firebaseInit.FIREBASE_PUSH_DATA_TYPE_TASK || n.Push.ID != c.task.Uuid || n.Path != "/app/task/"+c.task.Uuid {
		t.Errorf("the push and the email open the task: %+v %q", n.Push, n.Path)
	}
	if !strings.Contains(n.Title, "marked Press kit Urgent") || n.Actor != maya || !strings.Contains(n.DedupKey, c.change) {
		t.Errorf("title %q, actor %v, key %q", n.Title, n.Actor, n.DedupKey)
	}
	// The same change told again tells nobody again.
	noteUrgent(context.Background(), c)
	if got, _ := w.to(ben.Uuid); len(got) != 1 {
		t.Errorf("the same change told twice: %d", len(got))
	}
}

func TestUrgentNeverTellsTheActor(t *testing.T) {
	w := newUrgentWorld(t)
	maya := w.person("all")
	noteUrgent(context.Background(), made("Press kit", maya, maya))
	if len(w.told) != 0 {
		t.Fatalf("Maya made her own task Urgent and was told: %+v", w.told)
	}
}

func TestUrgentOnlyAChangeIntoUrgentTells(t *testing.T) {
	for _, c := range []struct {
		before, after string
		want          bool
	}{
		{"high", "urgent", true}, {"", "urgent", true}, {"low", "urgent", true},
		{"urgent", "urgent", false}, {"urgent", "high", false}, {"high", "low", false}, {"urgent", "", false},
	} {
		if got := becameUrgent(c.before, c.after); got != c.want {
			t.Errorf("%q -> %q: %v, want %v", c.before, c.after, got, c.want)
		}
	}
	// Urgent, High, Urgent again: two changes into Urgent, told twice.
	w := newUrgentWorld(t)
	maya, ben := w.person("all"), w.person("all")
	first := made("Press kit", ben, maya)
	again := first
	again.change = uuid.NewString()
	noteUrgent(context.Background(), first)
	noteUrgent(context.Background(), again)
	if got, reached := w.to(ben.Uuid); len(got) != 2 || reached != 2 {
		t.Fatalf("Ben was told %d times (%d reached), want twice", len(got), reached)
	}
}

func TestUrgentBatchTellsEachAssigneeOneDigest(t *testing.T) {
	w := newUrgentWorld(t)
	maya, ben, sam, kay := w.person("all"), w.person("all"), w.person("mention"), w.person("all")
	ctx, tell := GatherUrgent(context.Background())
	inner, tellInner := GatherUrgent(ctx)
	var bens []string
	for i := range 5 {
		name := fmt.Sprintf("Ben's %d", i)
		bens = append(bens, name)
		noteUrgent(inner, made(name, ben, maya))
	}
	noteUrgent(ctx, made("Sam's 1", sam, maya), made("Sam's 2", sam, maya))
	noteUrgent(ctx, made("Kay's", kay, maya))
	tellInner()
	if len(w.told) != 0 {
		t.Fatalf("a batch told before it was done, or an inner one told: %d", len(w.told))
	}
	tell()

	got, reached := w.to(ben.Uuid)
	if len(got) != 6 || reached != 1 {
		t.Fatalf("Ben: %d tellings, %d pushed and emailed; want 5 in the Inbox and one digest", len(got), reached)
	}
	for _, g := range got[:5] {
		if !g.to.InboxOnly || g.n.Kind != NoticeUrgent {
			t.Errorf("each task its own Inbox item only: %+v", g)
		}
	}
	d := got[5]
	if !d.to.Listed || d.n.Kind != NoticeUrgentDigest || d.n.Email != notificationBusiness.EventTaskUrgent || !strings.Contains(d.n.Title, "marked 5 tasks Urgent") {
		t.Fatalf("Ben's digest: %+v %+v", d.to, d.n)
	}
	for _, name := range bens {
		if !strings.Contains(d.n.Body, name) {
			t.Errorf("the digest doesn't name %q: %q", name, d.n.Body)
		}
	}
	if got, reached := w.to(sam.Uuid); len(got) != 3 || reached != 1 || !strings.Contains(got[2].n.Title, "2 tasks") {
		t.Errorf("Sam (mentions only: it's for him): %d tellings, %d reached", len(got), reached)
	}
	if got, reached := w.to(kay.Uuid); len(got) != 1 || reached != 1 || got[0].n.Kind != NoticeUrgent || got[0].to.InboxOnly {
		t.Errorf("Kay, one task: told as one task, %+v", got)
	}
	// The same batch told again tells nobody again.
	before := len(w.told)
	tell()
	if len(w.told) != before {
		t.Errorf("a batch told twice")
	}
	// Its context lives on (an event's listeners hold it): what is made
	// Urgent after it was told is told by itself, and a batch opened in it
	// gathers afresh.
	late := made("Late", kay, maya)
	noteUrgent(ctx, late)
	if got, reached := w.to(kay.Uuid); len(got) != 2 || reached != 2 {
		t.Fatalf("a change after the batch was told: Kay told %d, reached %d", len(got), reached)
	}
	again, tellAgain := GatherUrgent(ctx)
	noteUrgent(again, made("K1", kay, maya), made("K2", kay, maya))
	tellAgain()
	if got, reached := w.to(kay.Uuid); len(got) != 5 || reached != 3 || !strings.Contains(got[4].n.Title, "2 tasks") {
		t.Errorf("a batch opened after: Kay told %d, reached %d", len(got), reached)
	}
}

func TestUrgentDigestNamesTenAndCountsTheRest(t *testing.T) {
	maya, ben := &dgraphStruct.DgraphUser{Uuid: "m", UserName: "Maya"}, &dgraphStruct.DgraphUser{Uuid: "b"}
	var cs []urgentChange
	for i := range 12 {
		cs = append(cs, made(fmt.Sprintf("T%d", i), ben, maya))
	}
	d := urgentDigest(cs)
	if !strings.HasSuffix(d.Body, "T9, and 2 more") || strings.Contains(d.Body, "T10") || d.Data["tasks"] != "12" {
		t.Errorf("body %q data %v", d.Body, d.Data)
	}
	if again := urgentDigest([]urgentChange{cs[1], cs[0]}); again.DedupKey != urgentDigest(cs[:2]).DedupKey {
		t.Errorf("a batch's key is its changes', in any order")
	}
	if one := urgentDigest(cs[:1]); one.Push.ID != cs[0].task.Uuid || !strings.Contains(one.Title, "marked T0 Urgent") {
		t.Errorf("a digest of one reads as the one task: %+v", one)
	}
}

// An Urgent digest's email shows each task's name as it was written: a name
// with markup in it reads as those characters, in the email's HTML and its
// text alike, and no tag of it is live.
func TestUrgentDigestEmailShowsANameAsWritten(t *testing.T) {
	w := newUrgentWorld(t)
	maya, ben := w.person("all"), w.person("all")
	odd := `<b>Ship</b> & "launch" <img src=x>`
	ctx, tell := GatherUrgent(context.Background())
	noteUrgent(ctx, made(odd, ben, maya), made("Fix login", ben, maya))
	tell()

	got, reached := w.to(ben.Uuid)
	if len(got) != 3 || reached != 1 || got[2].n.Kind != NoticeUrgentDigest {
		t.Fatalf("Ben: %d tellings, %d pushed and emailed; want two in the Inbox and one digest", len(got), reached)
	}
	d := got[2].n
	page, text, err := emailService.Render(emailService.TemplateData{Title: d.Title, Body: d.Body})
	if err != nil {
		t.Fatal(err)
	}
	for _, live := range []string{"<b>Ship</b>", "<img src=x>"} {
		if strings.Contains(page, live) {
			t.Errorf("the email's HTML has %q live, from a task's name", live)
		}
	}
	if shown := html.UnescapeString(page); !strings.Contains(shown, odd) || !strings.Contains(shown, "Fix login") {
		t.Errorf("the email's HTML doesn't read the names as written (%q, then %q)", odd, "Fix login")
	}
	if !strings.Contains(text, odd) || !strings.Contains(text, "Fix login") {
		t.Errorf("the email's text doesn't show %q as written: %q", odd, text)
	}
}

func TestUrgentTellsNobodyWhoMayNotHear(t *testing.T) {
	w := newUrgentWorld(t)
	maya := w.person("all")
	cantRead := w.person("all")
	w.unread[cantRead.Uuid] = true
	muted, takenOff := w.person("nothing"), w.person("")
	ctx := context.Background()
	noteUrgent(ctx, made("Restricted", cantRead, maya), made("Muted", muted, maya), made("Off the project", takenOff, maya))
	noteUrgent(ctx, made("Nobody's", nil, maya))
	noProject := made("Mine", w.person("all"), maya)
	noProject.task.Project = nil
	noteUrgent(ctx, noProject)
	if len(w.told) != 0 {
		t.Fatalf("told: %+v", w.told)
	}
}

func TestUrgentMoveOfDataTellsNobody(t *testing.T) {
	w := newUrgentWorld(t)
	maya, ben := w.person("all"), w.person("all")
	noteUrgent(QuietMove(context.Background()), made("Imported", ben, maya))
	ctx, tell := GatherUrgent(QuietMove(context.Background()))
	noteUrgent(ctx, made("Synced", ben, maya))
	tell()
	if len(w.told) != 0 {
		t.Fatalf("an import or a sync told: %+v", w.told)
	}
}

// windowed makes the Urgent window's timer one the test fires: what an
// action gathered is pushed and emailed when its fire is called.
func windowed(t *testing.T) *[]func() {
	t.Helper()
	was := urgentWindow
	t.Cleanup(func() { urgentWindow = was })
	fires := &[]func(){}
	urgentWindow = &helpers.Windows[urgentReach]{Wait: urgentWait, Flush: reachUrgent,
		Start: func(_ time.Duration, f func()) { *fires = append(*fires, f) }}
	return fires
}

// One action's Urgent changes (gap b): a bulk change, and the rule runs its
// events set going, each a batch of its own, are one action
// (helpers.WithOrigin). Each task is in its assignee's Inbox at once; when the
// action's window ends, each assignee is pushed and emailed once, naming all
// of theirs.
func TestUrgentOneActionTellsEachAssigneeOnce(t *testing.T) {
	w := newUrgentWorld(t)
	fires := windowed(t)
	maya, ben, kay := w.person("all"), w.person("all"), w.person("all")
	ctx := helpers.WithOrigin(context.Background(), "bulk-1")
	noteUrgent(ctx, made("Ben's 1", ben, maya), made("Kay's", kay, maya))
	for _, name := range []string{"Ben's 2", "Ben's 3"} {
		run, tell := GatherUrgent(ctx)
		noteUrgent(run, made(name, ben, maya))
		tell()
	}
	got, reached := w.to(ben.Uuid)
	if len(got) != 3 || reached != 0 {
		t.Fatalf("Ben before the action's window ends: %d tellings, %d pushed and emailed; want 3 in the Inbox, none yet", len(got), reached)
	}
	for _, g := range got {
		if !g.to.InboxOnly || g.n.Kind != NoticeUrgent {
			t.Errorf("each task its own Inbox item only: %+v", g)
		}
	}
	if len(*fires) != 1 {
		t.Fatalf("one action, %d windows", len(*fires))
	}
	(*fires)[0]()

	got, reached = w.to(ben.Uuid)
	if len(got) != 4 || reached != 1 {
		t.Fatalf("Ben: %d tellings, %d pushed and emailed; want one digest", len(got), reached)
	}
	d := got[3]
	if !d.to.Listed || d.n.Kind != NoticeUrgentDigest || !strings.Contains(d.n.Title, "marked 3 tasks Urgent") {
		t.Fatalf("Ben's digest: %+v %+v", d.to, d.n)
	}
	for _, name := range []string{"Ben's 1", "Ben's 2", "Ben's 3"} {
		if !strings.Contains(d.n.Body, name) {
			t.Errorf("the digest doesn't name %q: %q", name, d.n.Body)
		}
	}
	// Kay, one task: her push and email read as, and open, the one task.
	got, reached = w.to(kay.Uuid)
	if len(got) != 2 || reached != 1 || !got[1].to.Listed || !strings.Contains(got[1].n.Title, "marked Kay's Urgent") || got[1].n.Path != got[0].n.Path {
		t.Fatalf("Kay, one task: %d tellings, %d reached, %+v", len(got), reached, got)
	}
	// The window told is over: its timer again tells nobody again.
	before := len(w.told)
	(*fires)[0]()
	if len(w.told) != before {
		t.Errorf("an action's window told twice")
	}
	// Changes of no action are told as they come: two batches, two pushes.
	sam := w.person("all")
	noteUrgent(context.Background(), made("Sam's 1", sam, maya))
	noteUrgent(context.Background(), made("Sam's 2", sam, maya))
	if got, reached := w.to(sam.Uuid); len(got) != 2 || reached != 2 || len(*fires) != 1 {
		t.Errorf("no action: Sam told %d, reached %d, %d windows", len(got), reached, len(*fires))
	}
}

// A change told already (the hand-off delivers an event at least once, so a
// rule can run twice) is not told again: in a batch, or in an action's window,
// the push and email name only what was newly told (CR-271 Y4).
func TestUrgentAChangeToldIsNeverPushedAgain(t *testing.T) {
	w := newUrgentWorld(t)
	maya, ben := w.person("all"), w.person("all")
	a, b, d, c := made("A", ben, maya), made("B", ben, maya), made("D", ben, maya), made("C", ben, maya)
	for _, cs := range [][]urgentChange{{a, b}, {a, d}} {
		ctx, tell := GatherUrgent(context.Background())
		noteUrgent(ctx, cs...)
		tell()
	}
	got, reached := w.to(ben.Uuid)
	if len(got) != 5 || reached != 2 || !strings.Contains(got[4].n.Title, "marked D Urgent") {
		t.Fatalf("A told twice: %d tellings, %d reached, last %q; want A once and D's push naming D alone", len(got), reached, got[len(got)-1].n.Title)
	}
	fires := windowed(t)
	ctx := helpers.WithOrigin(context.Background(), "bulk-1")
	noteUrgent(ctx, a)
	noteUrgent(ctx, c)
	if len(*fires) != 1 {
		t.Fatalf("%d windows, want the one C opened", len(*fires))
	}
	(*fires)[0]()
	got, reached = w.to(ben.Uuid)
	if len(got) != 7 || reached != 3 || !strings.Contains(got[6].n.Title, "marked C Urgent") {
		t.Fatalf("an action's window: %d tellings, %d reached, last %q; want C's push naming C alone", len(got), reached, got[len(got)-1].n.Title)
	}
}

func TestBulkPriorityTellsItsBatchOnce(t *testing.T) {
	w := newUrgentWorld(t)
	maya, ben := w.person("all"), w.person("all")
	seams := bulkSeams
	t.Cleanup(func() { bulkSeams = seams })
	bulkSeams.touch = func(context.Context, []uuid.UUID, time.Time) error { return nil }
	bulkSeams.write = func(context.Context, []*dgraphStruct.DgraphTask, ...string) error { return nil }
	bulkSeams.index = func([]*dgraphStruct.DgraphTask, ...string) {}
	var tasks []*dgraphStruct.DgraphTask
	for i, p := range []string{"high", "low", "urgent"} {
		c := made(fmt.Sprintf("B%d", i), ben, maya)
		c.task.Uid, c.task.Priority = fmt.Sprintf("0x%d", i+1), p
		tasks = append(tasks, c.task)
	}
	res := &BulkResult{}
	if err := bulkFields(context.Background(), tasks, BulkChange{Op: BulkPriority, Value: "urgent"}, maya, res); err != nil {
		t.Fatal(err)
	}
	got, reached := w.to(ben.Uuid)
	if len(got) != 3 || reached != 1 || !strings.Contains(got[2].n.Title, "marked 2 tasks Urgent") || strings.Contains(got[2].n.Body, "B2") {
		t.Fatalf("Ben: %d tellings, %d reached: want the two made Urgent in one digest", len(got), reached)
	}
	// Made High, nothing is told.
	before := len(w.told)
	if err := bulkFields(context.Background(), tasks, BulkChange{Op: BulkPriority, Value: "high"}, maya, &BulkResult{}); err != nil {
		t.Fatal(err)
	}
	if len(w.told) != before {
		t.Errorf("a change out of Urgent told")
	}
}

// An Urgent email is asked again at its send (business/Notification
// verdicts): of its one task, or of each task a digest names, for the
// assignee as someone the task names; a task mention, of its task and the
// comment it is in.
func TestAnUrgentOrMentionEmailIsAskedAgainOfItsTasks(t *testing.T) {
	maya, ben := &dgraphStruct.DgraphUser{Uuid: "m", UserName: "Maya"}, &dgraphStruct.DgraphUser{Uuid: "b"}
	var cs []urgentChange
	var named []string
	for i := range 12 {
		cs = append(cs, made(fmt.Sprintf("T%d", i), ben, maya))
		if i < urgentNamed {
			named = append(named, cs[i].task.Uuid)
		}
	}
	one := &notificationBusiness.About{Kind: notificationBusiness.AboutTask, Item: cs[0].task.Uuid, Heard: notifylevel.Named}
	for _, c := range []struct {
		name string
		got  *notificationBusiness.About
		want *notificationBusiness.About
	}{
		{"one task", urgentNotice(cs[0]).About, one},
		{"a digest of one", urgentDigest(cs[:1]).About, one},
		{"a digest of twelve", urgentDigest(cs).About,
			&notificationBusiness.About{Kind: notificationBusiness.AboutTask, Item: named[0], Items: named, Heard: notifylevel.Named}},
		{"a mention in the task", taskMentionNotice(maya, cs[0].task, "", "<p>@Ben</p>").About, one},
		{"a mention in its comment", taskMentionNotice(maya, cs[0].task, "c1", "<p>@Ben</p>").About,
			&notificationBusiness.About{Kind: notificationBusiness.AboutTask, Item: cs[0].task.Uuid, Reply: "c1", Heard: notifylevel.Named}},
	} {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s: asked again as %+v, want %+v", c.name, c.got, c.want)
		}
	}
}
