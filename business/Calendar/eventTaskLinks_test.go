package Calendar

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	workGraph "github.com/akashc777/OneCamp/domain/WorkGraph"
	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// EVLINK head A (gcal-15's recorded gap): a task links to an event as a doc
// does. The tasks the link tests name: one its readers may open, and one
// some of them may not (lockedTask).
const (
	launchTask = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	lockedTask = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
)

func task(id string) FileRef { return FileRef{Kind: "task", UUID: id} }

// opensAs is h with an opener by person: each opens everything but what
// hidden names for them by name.
func opensAs(h *harness, hidden map[string][]string) *harness {
	h.s.opens = func(_ context.Context, who *model.UserInfo, refs []FileRef) (map[FileRef]bool, error) {
		out := map[FileRef]bool{}
		for _, r := range refs {
			out[r] = !slices.Contains(hidden[who.UserDgraphInfo.UserName], r.UUID)
		}
		return out, nil
	}
	return h
}

// fileRowsOf is every row of event's links, removed ones included.
func (h *harness) fileRowsOf(event string) []memFile {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	return slices.Clone(h.store.files[uuid.MustParse(event)])
}

// A task links to an event as a doc does: its organiser links one they may
// open, everyone who may open it reads it back by kind and id, and the
// change is announced by kind and id, never a title. One the adder can't
// open is refused, and so is anyone who may not change the event. Unlinked,
// the row stays, marked; linked again, the same row is back.
func TestATaskLinksToAnEventAndUnlinksSoftly(t *testing.T) {
	h := opensAs(newHarness(t), map[string][]string{"Sam": {lockedTask}})
	ctx := context.Background()
	sam, maya := h.member("Sam"), h.member("Maya")
	e := h.create(sam, "Launch review", maya)

	got, err := h.s.AddFile(ctx, sam, e.ID, task(strings.ToUpper(launchTask)))
	if err != nil || filesOf(got) != "task:"+launchTask {
		t.Fatalf("Sam linked the launch task: %s %v", filesOf(got), err)
	}
	if read, err := h.s.Get(ctx, maya, e.ID); err != nil || filesOf(read) != "task:"+launchTask {
		t.Errorf("Maya reads %s %v", filesOf(read), err)
	}
	if c := h.fileChanges(); len(c) != 1 || c[0]["file"] != "added" || c[0]["file_kind"] != "task" || c[0]["file_uuid"] != launchTask || c[0]["title"] != "" {
		t.Errorf("announced %v", c)
	}
	if _, err := h.s.AddFile(ctx, sam, e.ID, task(lockedTask)); !errors.Is(err, ErrFileUnreadable) {
		t.Errorf("a task Sam can't open: %v", err)
	}
	if _, err := h.s.AddFile(ctx, maya, e.ID, task(launchTask)); !errors.Is(err, ErrOnlyCreatorChanges) {
		t.Errorf("Maya, who may not change it, linked a task: %v", err)
	}
	if _, err := h.s.RemoveFile(ctx, maya, e.ID, task(launchTask)); !errors.Is(err, ErrOnlyCreatorChanges) {
		t.Errorf("Maya, who may not change it, unlinked a task: %v", err)
	}
	if rows := h.fileRowsOf(e.ID); len(rows) != 1 || rows[0].Ref.String() != launchTask {
		t.Fatalf("rows %+v: a refused link has none", rows)
	}

	if got, err := h.s.RemoveFile(ctx, sam, e.ID, task(launchTask)); err != nil || filesOf(got) != "" {
		t.Fatalf("unlinked: %s %v", filesOf(got), err)
	}
	if rows := h.fileRowsOf(e.ID); len(rows) != 1 || !rows[0].gone {
		t.Errorf("unlinked, the row is kept and marked: %+v", rows)
	}
	if got, err := h.s.AddFile(ctx, sam, e.ID, task(launchTask)); err != nil || filesOf(got) != "task:"+launchTask {
		t.Fatalf("linked again: %s %v", filesOf(got), err)
	}
	if rows := h.fileRowsOf(e.ID); len(rows) != 1 || rows[0].gone || rows[0].Kind != "task" {
		t.Errorf("linked again, the same row is back: %+v", rows)
	}
}

// The permission model (MAIN 13:4x 11 Nov): a linked doc or task shows on an
// event only to someone who may see both the event and it. Ken, a guest who
// can't open the hidden task or the secret doc, reads the event without
// them on every read the panel, /v1 and MCP answer with (one event, a
// range, a calendar's events): no id, no count, no placeholder. An event
// whose links he can open none of carries "files": [] for him. A read whose
// check fails shows nothing, never everything.
func TestAReaderWhoCantOpenALinkedThingSeesNothingOfIt(t *testing.T) {
	h := opensAs(newHarness(t), map[string][]string{"Ken": {lockedTask, secretDoc}})
	ctx := context.Background()
	sam, maya, ken := h.member("Sam"), h.member("Maya"), h.member("Ken")
	e := h.create(sam, "Board meeting", maya, ken)
	only := h.create(sam, "Payroll review", maya, ken)
	for _, ref := range []FileRef{doc(briefDoc), task(lockedTask), doc(secretDoc)} {
		if _, err := h.s.AddFile(ctx, sam, e.ID, ref); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.s.AddFile(ctx, sam, only.ID, task(lockedTask)); err != nil {
		t.Fatal(err)
	}
	all := "doc:" + briefDoc + ",task:" + lockedTask + ",doc:" + secretDoc
	for _, who := range []*model.UserInfo{sam, maya} {
		if read, err := h.s.Get(ctx, who, e.ID); err != nil || filesOf(read) != all {
			t.Errorf("%s reads %s %v", who.UserDgraphInfo.UserName, filesOf(read), err)
		}
	}

	read, err := h.s.Get(ctx, ken, e.ID)
	if err != nil || filesOf(read) != "doc:"+briefDoc {
		t.Errorf("Ken reads %s %v", filesOf(read), err)
	}
	from, to := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	ranged, _, err := h.s.Range(ctx, ken, from, to)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"get": read, "range": ranged})
	for _, hidden := range []string{lockedTask, secretDoc} {
		if strings.Contains(string(raw), hidden) {
			t.Errorf("Ken's reads carry %s: %s", hidden, raw)
		}
	}
	var listed int
	for _, ev := range ranged {
		switch ev.Title {
		case "Board meeting":
			listed++
			if filesOf(&ev) != "doc:"+briefDoc {
				t.Errorf("Ken's range: the board meeting's links %s", filesOf(&ev))
			}
		case "Payroll review":
			listed++
			if one, _ := json.Marshal(ev); !strings.Contains(string(one), `"files":[]`) {
				t.Errorf("Ken's range: the payroll review, whose links he can open none of: %s", one)
			}
		}
	}
	if listed != 2 {
		t.Errorf("Ken's range listed %d of the two events", listed)
	}

	h.s.opens = func(context.Context, *model.UserInfo, []FileRef) (map[FileRef]bool, error) {
		return nil, errors.New("the graph is down")
	}
	if read, err := h.s.Get(ctx, maya, e.ID); err != nil || read == nil || filesOf(read) != "" || read.Files == nil {
		t.Errorf("a read whose check fails: %s %v", filesOf(read), err)
	}
}

// A guest invited by email reads the event on their own page, and someone
// who sees only that its organiser is busy reads "Busy": neither carries a
// linked task, nor its id.
func TestAGuestByEmailAndABusyViewSeeNoLinkedTask(t *testing.T) {
	h := opensAs(newHarness(t), nil)
	ctx := context.Background()
	sam := h.member("Sam")
	e := h.withEmails(sam, "Vendor call", []string{"Alex Kim <alex@example.org>"})
	read, err := h.s.AddFile(ctx, sam, e.ID, task(launchTask))
	if err != nil || filesOf(read) != "task:"+launchTask {
		t.Fatalf("linked: %s %v", filesOf(read), err)
	}
	alex := answerOf(h.store.guests[uuid.MustParse(e.ID)], models.GuestKey{Email: "alex@example.org"})
	secret, err := helpers.DecryptSecret(alex.LinkSealed)
	if err != nil {
		t.Fatal(err)
	}
	page, err := h.s.invitation(ctx, secret, h.clock)
	if err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]any{"Alex's page": page, "a busy view": busyView(*read)} {
		raw, _ := json.Marshal(v)
		if strings.Contains(string(raw), launchTask) {
			t.Errorf("%s carries the linked task: %s", name, raw)
		}
	}
}

// A meeting's node links the tasks it carries, beside the docs, so a task's
// Connected panel and backlinks show the meeting; an event of one person
// alone with a task on it is a node for that task, and one unlinked leaves
// it.
func TestAMeetingsNodeLinksTheTasksItCarries(t *testing.T) {
	h := opensAs(newHarness(t), nil)
	ctx := context.Background()
	sam := h.member("Sam")
	cal, err := h.s.store.personalCalendar(ctx, personOf(sam), "UTC")
	if err != nil {
		t.Fatal(err)
	}
	alone := h.meeting(sam, cal, nil)
	for _, ref := range []FileRef{task(launchTask), board(roomBoard), doc(briefDoc)} {
		if _, err := h.s.AddFile(ctx, sam, alone.ID.String(), ref); err != nil {
			t.Fatal(err)
		}
	}
	p, ok := h.nodes(alone.ID)[alone.ID.String()]
	if !ok {
		t.Fatal("no node")
	}
	want := []workGraph.Ref{{Kind: workGraph.KindTask, UUID: launchTask}, {Kind: workGraph.KindDoc, UUID: briefDoc}}
	if got := p.Edges[workGraph.RefersTo]; !slices.Equal(got, want) {
		t.Errorf("links %v, want %v", got, want)
	}
	for _, ref := range []FileRef{doc(briefDoc), task(launchTask)} {
		if _, err := h.s.RemoveFile(ctx, sam, alone.ID.String(), ref); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := h.nodes(alone.ID)[alone.ID.String()]; ok {
		t.Error("the task unlinked, it still has a node")
	}
}
