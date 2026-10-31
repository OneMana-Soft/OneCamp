package Calendar

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Calendar"
	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// Task blocks (calendar B1, gcal-new-task-blocks): time on the grid for a task
// the writer may read, read back with its task as the reader may read it.

const (
	briefTask  = "6f1d2c3b-4a5e-4f60-8a7b-9c0d1e2f3a4b"
	hiddenTask = "0a1b2c3d-4e5f-4a6b-8c7d-8e9f0a1b2c3d"
)

// tasksRead stands in for business/Task's read that checks access: the brief
// is readable (to readers alone, when they are set), the hidden task isn't.
// Each call is kept.
type tasksRead struct {
	calls [][]string
	uids  []string
	// name is the brief's name now.
	name    string
	readers []string
}

func stubTasks(t *testing.T) *tasksRead {
	t.Helper()
	r := &tasksRead{name: "Write the launch brief"}
	prev := readableTasks
	readableTasks = func(_ context.Context, ids []string, uid string) (map[string]taskBusiness.ReadableTask, error) {
		r.calls, r.uids = append(r.calls, ids), append(r.uids, uid)
		out := map[string]taskBusiness.ReadableTask{}
		for _, id := range ids {
			if id == briefTask && (len(r.readers) == 0 || slices.Contains(r.readers, uid)) {
				out[id] = taskBusiness.ReadableTask{Name: r.name, Status: "in_progress", ProjectUUID: "p-1", ProjectName: "Launch"}
			}
		}
		return out, nil
	}
	t.Cleanup(func() { readableTasks = prev })
	return r
}

// graphUIDs stands in for the work graph's uids of people known by their
// uuid alone (a feed's, an export's, a reminder's person).
func graphUIDs(t *testing.T, uids map[string]string) {
	t.Helper()
	prev := uidOf
	uidOf = func(_ context.Context, who accessBusiness.Subject) (string, error) {
		if who.UserUID != "" {
			return who.UserUID, nil
		}
		return uids[who.UserUUID], nil
	}
	t.Cleanup(func() { uidOf = prev })
}

func withGraphUID(who *model.UserInfo, uid string) *model.UserInfo {
	who.UserDgraphInfo.Uid = uid
	return who
}

func TestATaskBlockIsTimeForATaskTheWriterMayRead(t *testing.T) {
	h := newHarness(t)
	reads := stubTasks(t)
	sam := withGraphUID(h.member("Sam"), "0xsam")
	start, end := timed(h.clock.Add(2*time.Hour), 90, "UTC")
	e, err := h.s.Create(context.Background(), sam, EventInput{Kind: EventTaskBlock, TaskUUID: briefTask, Start: start, End: end})
	if err != nil {
		t.Fatalf("a block for a task Sam may read: %v", err)
	}
	if e.Kind != EventTaskBlock || e.Title != "Write the launch brief" || e.Transparency != transparencyOpaque {
		t.Errorf("the block is named for its task and busy: %+v", e)
	}
	if e.Task == nil || e.Task.UUID != briefTask || e.Task.Status != "in_progress" || e.Task.ProjectName != "Launch" {
		t.Errorf("the block reads back with its task: %+v", e.Task)
	}
	if len(reads.uids) == 0 || reads.uids[0] != "0xsam" {
		t.Errorf("the task's access is Sam's own: %v", reads.uids)
	}
	// A name of its own stays.
	named, err := h.s.Create(context.Background(), sam, EventInput{Kind: EventTaskBlock, TaskUUID: briefTask, Title: "Brief, first draft", Start: start, End: end})
	if err != nil || named.Title != "Brief, first draft" {
		t.Errorf("a block named by its writer: %v %+v", err, named)
	}
}

func TestATaskBlockIsRefusedWithoutATaskItsWriterMayRead(t *testing.T) {
	h := newHarness(t)
	stubTasks(t)
	sam := withGraphUID(h.member("Sam"), "0xsam")
	ana := h.member("Ana")
	start, end := timed(h.clock.Add(2*time.Hour), 60, "UTC")
	for name, in := range map[string]EventInput{
		"no task":              {Kind: EventTaskBlock, Start: start, End: end},
		"not a task's uuid":    {Kind: EventTaskBlock, TaskUUID: "brief", Start: start, End: end},
		"a task Sam can't see": {Kind: EventTaskBlock, TaskUUID: hiddenTask, Start: start, End: end},
		"guests on a block":    {Kind: EventTaskBlock, TaskUUID: briefTask, Start: start, End: end, Attendees: []GuestInput{{UUID: personOf(ana).String()}}},
		"a task on an event":   {Kind: EventPlain, Title: "Sync", TaskUUID: briefTask, Start: start, End: end},
	} {
		_, err := h.s.Create(context.Background(), sam, in)
		var input *EventInputError
		if !errors.As(err, &input) {
			t.Errorf("%s: want an input error, got %v", name, err)
		}
	}
	if got, _, _ := h.s.Range(context.Background(), sam, h.clock, h.clock.Add(24*time.Hour)); len(got) != 0 {
		t.Errorf("nothing refused was kept: %d", len(got))
	}
}

func TestATaskBlockMadeAnotherKindLetsItsTaskGo(t *testing.T) {
	h := newHarness(t)
	stubTasks(t)
	sam := withGraphUID(h.member("Sam"), "0xsam")
	start, end := timed(h.clock.Add(2*time.Hour), 60, "UTC")
	e, err := h.s.Create(context.Background(), sam, EventInput{Kind: EventTaskBlock, TaskUUID: briefTask, Start: start, End: end})
	if err != nil {
		t.Fatal(err)
	}
	plain, seq := EventPlain, e.Sequence
	after, err := h.s.Update(context.Background(), sam, e.ID, EventPatch{Sequence: &seq, Kind: &plain})
	if err != nil {
		t.Fatalf("a block made an event: %v", err)
	}
	if after.Kind != EventPlain || after.Task != nil {
		t.Errorf("an event is no longer time for the task: %+v", after)
	}
	if row := h.store.events[uuid.MustParse(e.ID)]; row == nil || row.TaskUUID != nil {
		t.Errorf("the row lets the task go too: %+v", row)
	}
}

// The grid moves a block through the old update: a move keeps it a block for
// its task, and focus time made of it lets the task go.
func TestTheOldUpdateMovesABlockAndKeepsItsTask(t *testing.T) {
	h := newHarness(t)
	stubTasks(t)
	sam := withGraphUID(h.member("Sam"), "0xsam")
	start, end := timed(h.clock.Add(2*time.Hour), 60, "UTC")
	e, err := h.s.Create(context.Background(), sam, EventInput{Kind: EventTaskBlock, TaskUUID: briefTask, Start: start, End: end})
	if err != nil {
		t.Fatal(err)
	}
	off := false
	moved := adapter.CreateOrUpdateEventInput{Title: e.Title, StartTime: h.clock.Add(4 * time.Hour).Format(time.RFC3339), EndTime: h.clock.Add(5 * time.Hour).Format(time.RFC3339), IsFocus: &off, IsAway: &off}
	if err := h.s.legacyUpdateAt(context.Background(), e.ID, moved, sam); err != nil {
		t.Fatalf("a block moved by the grid: %v", err)
	}
	if row := h.store.events[uuid.MustParse(e.ID)]; row.Kind != string(EventTaskBlock) || row.TaskUUID == nil || row.StartAt == nil || !row.StartAt.Equal(h.clock.Add(4*time.Hour)) {
		t.Errorf("moved, still its task's block: %+v", row)
	}
	on := true
	focus := moved
	focus.IsFocus = &on
	if err := h.s.legacyUpdateAt(context.Background(), e.ID, focus, sam); err != nil {
		t.Fatalf("a block made focus time: %v", err)
	}
	if row := h.store.events[uuid.MustParse(e.ID)]; row.Kind != string(EventFocus) || row.TaskUUID != nil {
		t.Errorf("focus time names no task: %+v", row)
	}
}

func TestBlocksReadTheirTasksInOneReadAndKeepTheirTaskWhenItIsHidden(t *testing.T) {
	h := newHarness(t)
	reads := stubTasks(t)
	sam := withGraphUID(h.member("Sam"), "0xsam")
	start, end := timed(h.clock.Add(2*time.Hour), 60, "UTC")
	if _, err := h.s.Create(context.Background(), sam, EventInput{Kind: EventTaskBlock, TaskUUID: briefTask, Start: start, End: end}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Create(context.Background(), sam, EventInput{Title: "Standup", Start: start, End: end}); err != nil {
		t.Fatal(err)
	}
	// A block whose task Sam could read when it was made, and can't now.
	hidden := blank("UTC")
	_ = setWhen(hidden, start, end)
	id := uuid.MustParse(hiddenTask)
	hidden.Kind, hidden.Title, hidden.TaskUUID, hidden.CreatedBy = string(EventTaskBlock), "Old work", &id, personOf(sam)
	if _, _, err := h.s.create(context.Background(), sam, hidden, nil, false); err != nil {
		t.Fatal(err)
	}
	reads.calls = nil
	got, _, err := h.s.Range(context.Background(), sam, h.clock, h.clock.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(reads.calls) != 1 || len(reads.calls[0]) != 2 {
		t.Errorf("one read for both blocks' tasks: %v", reads.calls)
	}
	byTitle := map[string]Event{}
	for _, e := range got {
		byTitle[e.Title] = e
	}
	if b := byTitle["Write the launch brief"]; b.Task == nil || b.Task.Name != "Write the launch brief" {
		t.Errorf("the readable task: %+v", b.Task)
	}
	if b := byTitle["Old work"]; b.Task == nil || b.Task.UUID != hiddenTask || b.Task.Name != "" {
		t.Errorf("the hidden task, by its uuid alone: %+v", b.Task)
	}
	if e := byTitle["Standup"]; e.Task != nil {
		t.Errorf("an event names no task: %+v", e.Task)
	}
	// The old answers the grid reads carry it too.
	old, err := h.s.legacyRange(context.Background(), sam, h.clock, h.clock.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	tasks := 0
	for _, ev := range old {
		if ev.Task != nil {
			tasks++
		}
	}
	if tasks != 2 {
		t.Errorf("the grid's answers carry both blocks' tasks: %d", tasks)
	}
}

// A block is busy like any opaque event: Find a time, booking pages and the
// assistant don't offer its time.
func TestATaskBlockIsBusy(t *testing.T) {
	h := newHarness(t)
	stubTasks(t)
	sam := withGraphUID(h.member("Sam"), "0xsam")
	start, end := timed(h.clock.Add(2*time.Hour), 60, "UTC")
	if _, err := h.s.Create(context.Background(), sam, EventInput{Kind: EventTaskBlock, TaskUUID: briefTask, Start: start, End: end}); err != nil {
		t.Fatal(err)
	}
	busy, err := h.s.busyEvents(context.Background(), personOf(sam).String(), h.clock, h.clock.Add(24*time.Hour))
	if err != nil || len(busy) != 1 {
		t.Fatalf("the block is busy: %v %d", err, len(busy))
	}
}

// Audit C17: a block given no title keeps none. Wherever it is read (the
// grid, a chip, a feed), it is called by its task's name for whoever may read
// the task and "Task block" for anyone else; a renamed task's blocks follow
// it; and an update that sends back the title it read keeps none.
func TestATaskBlockIsCalledByItsTaskOnlyForThoseWhoMayReadIt(t *testing.T) {
	h := newHarness(t)
	feedsOn(t, true)
	reads := stubTasks(t)
	reads.readers = []string{"0xsam"}
	sam, maya := withGraphUID(h.member("Sam"), "0xsam"), withGraphUID(h.member("Maya"), "0xmaya")
	graphUIDs(t, map[string]string{personOf(sam).String(): "0xsam", personOf(maya).String(): "0xmaya"})
	ctx := context.Background()
	start, end := timed(h.clock.Add(2*time.Hour), 90, "UTC")
	e, err := h.s.Create(ctx, sam, EventInput{Kind: EventTaskBlock, TaskUUID: briefTask, Start: start, End: end})
	if err != nil {
		t.Fatal(err)
	}
	if row := h.store.events[uuid.MustParse(e.ID)]; row.Title != "" {
		t.Fatalf("the block keeps a copy of its task's name: %q", row.Title)
	}
	// Nor when the form sends the task's name as the title.
	named, err := h.s.Create(ctx, sam, EventInput{Kind: EventTaskBlock, TaskUUID: briefTask, Title: "Write the launch brief", Start: start, End: end})
	if err != nil {
		t.Fatal(err)
	}
	if row := h.store.events[uuid.MustParse(named.ID)]; row.Title != "" {
		t.Fatalf("a block titled with its task's name keeps a copy: %q", row.Title)
	}
	if err := h.s.Delete(ctx, sam, named.ID, "", nil); err != nil {
		t.Fatal(err)
	}
	own, _ := h.store.personalCalendar(ctx, personOf(sam), "UTC")
	if err := h.s.Share(ctx, sam, own.String(), personOf(maya).String(), models.ShareDetails); err != nil {
		t.Fatal(err)
	}
	grid := func(who *model.UserInfo) string {
		t.Helper()
		got, _, err := h.s.CalendarEvents(ctx, who, []string{own.String()}, h.clock, h.clock.Add(24*time.Hour))
		if err != nil || len(got) != 1 {
			t.Fatalf("the grid: %+v %v", got, err)
		}
		return got[0].Title
	}
	if got := grid(sam); got != "Write the launch brief" {
		t.Errorf("Sam's block is called %q", got)
	}
	if got := grid(maya); got != taskBlockTitle {
		t.Errorf("Maya, who may not read the task, sees %q", got)
	}
	// A chip, a link or a notice calls it as the grid does.
	chips, err := h.s.eventChips(ctx, subjectOf(maya), []string{e.ID})
	if err != nil || chips[e.ID].Title != taskBlockTitle {
		t.Errorf("Maya's chip: %+v %v", chips[e.ID], err)
	}
	// So do feeds of Sam's calendar: his own and Maya's.
	feedOf := func(who *model.UserInfo) string {
		t.Helper()
		made, err := h.s.MakeFeed(ctx, who, own.String(), models.FeedFull)
		if err != nil {
			t.Fatal(err)
		}
		body, err := h.s.FeedICS(ctx, tokenOf(t, made))
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	if body := feedOf(sam); !strings.Contains(body, "SUMMARY:Write the launch brief") {
		t.Errorf("Sam's feed:\n%s", body)
	}
	if body := feedOf(maya); strings.Contains(body, "launch brief") || !strings.Contains(body, "SUMMARY:"+taskBlockTitle) {
		t.Errorf("Maya's feed:\n%s", body)
	}
	// A renamed task's block follows it.
	reads.name = "Write the launch brief, v2"
	if got := grid(sam); got != "Write the launch brief, v2" {
		t.Errorf("after the rename Sam's block is called %q", got)
	}
	// The grid's move sends back the title it showed: none is kept.
	off := false
	moved := adapter.CreateOrUpdateEventInput{Title: grid(sam), StartTime: h.clock.Add(4 * time.Hour).Format(time.RFC3339), EndTime: h.clock.Add(5 * time.Hour).Format(time.RFC3339), IsFocus: &off, IsAway: &off}
	if err := h.s.legacyUpdateAt(ctx, e.ID, moved, sam); err != nil {
		t.Fatal(err)
	}
	if row := h.store.events[uuid.MustParse(e.ID)]; row.Title != "" {
		t.Errorf("the move kept %q", row.Title)
	}
	// A title of Sam's own is kept, and is the block's for everyone.
	row := h.store.events[uuid.MustParse(e.ID)]
	title, seq := "Brief, first draft", row.Sequence
	if _, err := h.s.Update(ctx, sam, e.ID, EventPatch{Title: &title, Sequence: &seq}); err != nil {
		t.Fatal(err)
	}
	if got := grid(maya); got != "Brief, first draft" {
		t.Errorf("Maya sees %q once Sam names it", got)
	}
}

// Audit C17: a block given no title reminds its person by its task's name.
func TestATaskBlockRemindsByItsTasksName(t *testing.T) {
	h, rem := reminding(t)
	stubTasks(t)
	sam := withGraphUID(h.member("Sam"), "0xsam")
	graphUIDs(t, map[string]string{personOf(sam).String(): "0xsam"})
	ctx := context.Background()
	start, end := timed(h.clock.Add(2*time.Hour), 60, "UTC")
	e, err := h.s.Create(ctx, sam, EventInput{Kind: EventTaskBlock, TaskUUID: briefTask, Start: start, End: end})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.s.planReminders(ctx, uuid.MustParse(e.ID), nil); err != nil {
		t.Fatal(err)
	}
	got := captureNotices(t)
	j, ok := rem.next(sam, e.ID, models.ReminderNext)
	if !ok {
		t.Fatal("the block reminds nobody")
	}
	h.clock = j.at.Add(30 * time.Second)
	if err := h.s.fireReminder(ctx, j.person, j.p); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 1 || !strings.HasPrefix((*got)[0].n.Title, "Write the launch brief in ") || (*got)[0].n.ResourceTitle != "Write the launch brief" {
		t.Fatalf("the reminder: %+v", *got)
	}
}
