package Calendar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	workGraph "github.com/akashc777/OneCamp/domain/WorkGraph"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// The docs the file tests name: one anyone may open, and one nobody but its
// owner may (secretDoc), and a board.
const (
	briefDoc  = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	secretDoc = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	roomBoard = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
)

// withFiles is h with an opener that lets everyone open everything but
// secretDoc.
func withFiles(h *harness) *harness {
	h.s.opens = func(_ context.Context, _ *model.UserInfo, refs []FileRef) (map[FileRef]bool, error) {
		out := map[FileRef]bool{}
		for _, r := range refs {
			out[r] = r.UUID != secretDoc
		}
		return out, nil
	}
	return h
}

func doc(id string) FileRef   { return FileRef{Kind: "doc", UUID: id} }
func board(id string) FileRef { return FileRef{Kind: "board", UUID: id} }

func filesOf(e *Event) string {
	if e == nil {
		return "<no event>"
	}
	var out []string
	for _, f := range e.Files {
		out = append(out, f.Kind+":"+f.UUID)
	}
	return strings.Join(out, ",")
}

// fileChanges are the resource events a files change emitted: what each
// says, never a title.
func (h *harness) fileChanges() []map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []map[string]string
	for _, e := range h.emitted {
		if e.Kind == resourceEventBusiness.CalendarEventUpdated && e.Data["changed"] == "files" {
			out = append(out, e.Data)
		}
	}
	return out
}

// An event's files are its organiser's to add and remove, each a doc or a
// board they may open: added once however often it is added, read by
// everyone who reads the event, taken off, and back when added again. Each
// change is announced (file added or removed, its kind and id, never a
// title); one that changes nothing announces nothing.
func TestAnEventsFilesAreAddedReadRemovedAndBroughtBack(t *testing.T) {
	h := withFiles(newHarness(t))
	ctx := context.Background()
	sam, maya, jonas := h.member("Sam"), h.member("Maya"), h.member("Jonas")
	e := h.create(sam, "Design review", maya)

	got, err := h.s.AddFile(ctx, sam, e.ID, doc(strings.ToUpper(briefDoc)))
	if err != nil || filesOf(got) != "doc:"+briefDoc {
		t.Fatalf("Sam added the brief: %s %v", filesOf(got), err)
	}
	if got, err := h.s.AddFile(ctx, sam, e.ID, doc(briefDoc)); err != nil || filesOf(got) != "doc:"+briefDoc {
		t.Errorf("the brief again: %s %v", filesOf(got), err)
	}
	if got, err := h.s.AddFile(ctx, sam, e.ID, board(roomBoard)); err != nil || filesOf(got) != "doc:"+briefDoc+",board:"+roomBoard {
		t.Errorf("the board: %s %v", filesOf(got), err)
	}
	for _, who := range []*model.UserInfo{sam, maya} {
		if read, err := h.s.Get(ctx, who, e.ID); err != nil || filesOf(read) != "doc:"+briefDoc+",board:"+roomBoard {
			t.Errorf("%s reads %s %v", who.UserDgraphInfo.UserName, filesOf(read), err)
		}
	}
	if changes := h.fileChanges(); len(changes) != 2 || changes[0]["file"] != "added" || changes[0]["file_kind"] != "doc" ||
		changes[0]["file_uuid"] != briefDoc || changes[0]["title"] != "" {
		t.Errorf("announced %v", changes)
	}

	// Maya may not change it, Jonas isn't on it, and nobody adds what they
	// can't open, nor anything but a doc, a board or a task.
	if _, err := h.s.AddFile(ctx, maya, e.ID, doc(briefDoc)); !errors.Is(err, ErrOnlyCreatorChanges) {
		t.Errorf("Maya added a file: %v", err)
	}
	if _, err := h.s.RemoveFile(ctx, maya, e.ID, doc(briefDoc)); !errors.Is(err, ErrOnlyCreatorChanges) {
		t.Errorf("Maya removed a file: %v", err)
	}
	if _, err := h.s.AddFile(ctx, jonas, e.ID, doc(briefDoc)); !errors.Is(err, ErrEventNotFound) {
		t.Errorf("Jonas added a file: %v", err)
	}
	if _, err := h.s.AddFile(ctx, sam, e.ID, doc(secretDoc)); !errors.Is(err, ErrFileUnreadable) {
		t.Errorf("a doc Sam can't open: %v", err)
	}
	var input *EventInputError
	for _, bad := range []FileRef{{Kind: "project", UUID: briefDoc}, {Kind: "doc", UUID: "not-an-id"}, {Kind: "", UUID: briefDoc}} {
		if _, err := h.s.AddFile(ctx, sam, e.ID, bad); !errors.As(err, &input) {
			t.Errorf("%+v: %v", bad, err)
		}
	}

	got, err = h.s.RemoveFile(ctx, sam, e.ID, doc(briefDoc))
	if err != nil || filesOf(got) != "board:"+roomBoard {
		t.Fatalf("the brief taken off: %s %v", filesOf(got), err)
	}
	if got, err := h.s.RemoveFile(ctx, sam, e.ID, doc(briefDoc)); err != nil || filesOf(got) != "board:"+roomBoard {
		t.Errorf("taken off twice: %s %v", filesOf(got), err)
	}
	got, err = h.s.AddFile(ctx, sam, e.ID, doc(briefDoc))
	if err != nil || filesOf(got) != "board:"+roomBoard+",doc:"+briefDoc {
		t.Fatalf("the brief back: %s %v", filesOf(got), err)
	}
	changes := h.fileChanges()
	if len(changes) != 4 || changes[2]["file"] != "removed" || changes[3]["file"] != "added" {
		t.Errorf("announced %v", changes)
	}
	h.store.mu.Lock()
	rows := len(h.store.files[uuid.MustParse(e.ID)])
	h.store.mu.Unlock()
	if rows != 2 {
		t.Errorf("%d rows for two files: a file added again is its row brought back", rows)
	}
}

// A guest the organiser lets change the event adds and removes its files as
// the organiser does; an instance's files are its series'.
func TestAGuestWhoMayChangeAnEventChangesItsFiles(t *testing.T) {
	h := withFiles(newHarness(t))
	ctx := context.Background()
	sam, maya := h.member("Sam"), h.member("Maya")
	e := h.create(sam, "Planning", maya)
	yes := true
	if _, err := h.s.Update(ctx, sam, e.ID, EventPatch{Sequence: seq(e.Sequence), GuestsCanModify: &yes}); err != nil {
		t.Fatal(err)
	}
	if got, err := h.s.AddFile(ctx, maya, e.ID, doc(briefDoc)); err != nil || filesOf(got) != "doc:"+briefDoc {
		t.Fatalf("Maya, who may change it: %s %v", filesOf(got), err)
	}
	if got, err := h.s.RemoveFile(ctx, maya, e.ID, doc(briefDoc)); err != nil || filesOf(got) != "" {
		t.Errorf("Maya took it off: %s %v", filesOf(got), err)
	}
}

// An event carries at most maxEventFiles: one more is refused in words, and
// one already on it is no more.
func TestAnEventCarriesAtMost25Files(t *testing.T) {
	h := withFiles(newHarness(t))
	ctx := context.Background()
	sam := h.member("Sam")
	e := h.create(sam, "All hands")
	var first FileRef
	for i := 0; i < maxEventFiles; i++ {
		ref := doc(fmt.Sprintf("00000000-0000-4000-8000-%012d", i))
		if i == 0 {
			first = ref
		}
		if _, err := h.s.AddFile(ctx, sam, e.ID, ref); err != nil {
			t.Fatalf("file %d: %v", i, err)
		}
	}
	var input *EventInputError
	if _, err := h.s.AddFile(ctx, sam, e.ID, doc(briefDoc)); !errors.As(err, &input) || !strings.Contains(err.Error(), "25") {
		t.Errorf("a 26th file: %v", err)
	}
	if got, err := h.s.AddFile(ctx, sam, e.ID, first); err != nil || len(got.Files) != maxEventFiles {
		t.Errorf("one already on it, at the most: %d %v", len(got.Files), err)
	}
}

// A new event carries the files it is made with (a duplicate's): each once,
// a doc or a board its writer may open, at most maxEventFiles. One it may
// not carry refuses the whole write, so nothing is made.
func TestANewEventCarriesItsFiles(t *testing.T) {
	h := withFiles(newHarness(t))
	ctx := context.Background()
	sam := h.member("Sam")
	start, end := timed(h.clock.Add(24*time.Hour), 30, "UTC")
	in := EventInput{Title: "Copy of Design review", Start: start, End: end,
		Files: []FileRef{doc(briefDoc), doc(strings.ToUpper(briefDoc)), board(roomBoard)}}
	got, err := h.s.Create(ctx, sam, in)
	if err != nil || filesOf(got) != "doc:"+briefDoc+",board:"+roomBoard {
		t.Fatalf("made with its files: %s %v", filesOf(got), err)
	}
	if read, err := h.s.Get(ctx, sam, got.ID); err != nil || filesOf(read) != filesOf(got) {
		t.Errorf("read back: %s %v", filesOf(read), err)
	}
	before := len(h.store.events)
	in.Files = []FileRef{doc(briefDoc), doc(secretDoc)}
	if _, err := h.s.Create(ctx, sam, in); !errors.Is(err, ErrFileUnreadable) {
		t.Errorf("with a doc Sam can't open: %v", err)
	}
	in.Files = nil
	for i := 0; i <= maxEventFiles; i++ {
		in.Files = append(in.Files, doc(fmt.Sprintf("00000000-0000-4000-8000-%012d", i)))
	}
	if _, err := h.s.Create(ctx, sam, in); err == nil || !strings.Contains(err.Error(), "25") {
		t.Errorf("with 26 files: %v", err)
	}
	if len(h.store.events) != before {
		t.Errorf("a refused write made an event")
	}
}

// fileRefsOf is files as an event keeps them: each a doc or a board by a
// uuid in lower case, once, in the order given, at most maxEventFiles; else
// a refusal in words.
func TestFileRefsAreDocsOrBoardsOnceEachAtMost25(t *testing.T) {
	got, err := fileRefsOf([]FileRef{doc(" " + strings.ToUpper(briefDoc) + " "), board(roomBoard), doc(briefDoc), {Kind: " Doc ", UUID: roomBoard}})
	if err != nil || len(got) != 3 || got[0] != doc(briefDoc) || got[1] != board(roomBoard) || got[2] != doc(roomBoard) {
		t.Errorf("%+v %v", got, err)
	}
	if got, err := fileRefsOf(nil); err != nil || len(got) != 0 {
		t.Errorf("none: %+v %v", got, err)
	}
	var input *EventInputError
	for name, in := range map[string][]FileRef{
		"a project":  {{Kind: "project", UUID: briefDoc}},
		"no id":      {{Kind: "doc"}},
		"a bad id":   {{Kind: "board", UUID: "board-1"}},
		"twenty-six": nil,
	} {
		if name == "twenty-six" {
			for i := 0; i <= maxEventFiles; i++ {
				in = append(in, doc(fmt.Sprintf("00000000-0000-4000-8000-%012d", i)))
			}
		}
		if _, err := fileRefsOf(in); !errors.As(err, &input) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Someone who sees only that an event's owner is busy sees none of its
// files. A busy view, and a full view of an event that carries none, say so
// as an empty list ("files": []), never null, so the web never meets a null.
func TestABusyViewCarriesNoFiles(t *testing.T) {
	v := busyView(Event{ID: "e", Title: "Acquisition talks", Notes: "terms", Files: []FileRef{doc(briefDoc)}})
	if len(v.Files) != 0 || v.Notes != "" || v.Title != "Busy" {
		t.Errorf("%+v", v)
	}
	h := newHarness(t)
	sam := h.member("Sam")
	e := h.create(sam, "Lunch")
	read, err := h.s.Get(context.Background(), sam, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	for name, view := range map[string]Event{"a busy view": v, "a full view of an event without files": *read} {
		raw, err := json.Marshal(view)
		if err != nil || !strings.Contains(string(raw), `"files":[]`) {
			t.Errorf("%s: %s %v", name, raw, err)
		}
	}
}

// "This and following" makes a fresh series from the instance on, and it
// carries the files its series did: the same ones, put on by the same person
// at the same time, so a later instance still shows them, and the part that
// ended keeps its own. Each part's files are then its own. Stopping the
// repeat there, the instance on its own carries them (taking its own
// change's row); and the following part deleted and brought back from the
// trash has them.
func TestThisAndFollowingKeepsTheSeriesFiles(t *testing.T) {
	ctx := context.Background()
	lon := zone(t, "Europe/London")
	from, to := time.Date(2026, 10, 19, 0, 0, 0, 0, lon), time.Date(2026, 12, 1, 0, 0, 0, 0, lon)
	want := "doc:" + briefDoc + ",board:" + roomBoard
	stand := func(t *testing.T) (*harness, *model.UserInfo, *model.UserInfo, *Event, []Event) {
		h := withFiles(newHarness(t))
		sam, maya := h.member("Sam"), h.member("Maya")
		e := h.series(sam, "Stand-up", "FREQ=WEEKLY;COUNT=6", time.Date(2026, 10, 20, 9, 15, 0, 0, lon), 15, maya)
		for _, f := range []FileRef{doc(briefDoc), board(roomBoard)} {
			if _, err := h.s.AddFile(ctx, sam, e.ID, f); err != nil {
				t.Fatal(err)
			}
		}
		return h, sam, maya, e, h.between(sam, from, to)
	}
	everyInstance := func(t *testing.T, h *harness, who *model.UserInfo, want string) {
		t.Helper()
		got := h.between(who, from, to)
		if len(got) != 6 {
			t.Fatalf("%d instances", len(got))
		}
		for _, g := range got {
			if filesOf(&g) != want {
				t.Errorf("%s reads %s (%s) with %q", who.UserDgraphInfo.UserName, g.ID, g.Title, filesOf(&g))
			}
		}
	}

	t.Run("a change from the third on", func(t *testing.T) {
		h, sam, maya, e, in := stand(t)
		p := patchAt(in[2], ScopeFollowing)
		p.Location = str("Room 2")
		fresh, err := h.s.Update(ctx, sam, in[2].ID, p)
		if err != nil || fresh.ID == e.ID || filesOf(fresh) != want {
			t.Fatalf("the new series: %s %v", filesOf(fresh), err)
		}
		everyInstance(t, h, maya, want)
		was, now := h.store.files[uuid.MustParse(e.ID)], h.store.files[uuid.MustParse(fresh.ID)]
		if len(now) != len(was) {
			t.Fatalf("the new series' rows: %+v, the series': %+v", now, was)
		}
		for i := range was {
			if now[i].Kind != was[i].Kind || now[i].Ref != was[i].Ref || now[i].AddedBy != was[i].AddedBy ||
				!now[i].AddedAt.Equal(was[i].AddedAt) || now[i].gone {
				t.Errorf("row %d: %+v, from %+v", i, now[i], was[i])
			}
		}
		// Each part's files are its own now.
		if _, err := h.s.RemoveFile(ctx, sam, fresh.ID, doc(briefDoc)); err != nil {
			t.Fatal(err)
		}
		for _, g := range h.between(maya, from, to) {
			wantHere := want
			if g.SeriesID == fresh.ID {
				wantHere = "board:" + roomBoard
			}
			if filesOf(&g) != wantHere {
				t.Errorf("after the brief came off the new part, %s reads %q", g.ID, filesOf(&g))
			}
		}
	})

	t.Run("the repeat stopped at a changed instance", func(t *testing.T) {
		h, sam, _, _, in := stand(t)
		room := patchAt(in[2], ScopeThis)
		room.Location = str("Room 9")
		changed, err := h.s.Update(ctx, sam, in[2].ID, room)
		if err != nil {
			t.Fatal(err)
		}
		p := patchAt(*changed, ScopeFollowing)
		p.Title, p.Recurrence = str("Last one"), &Recurrence{Rule: ""}
		last, err := h.s.Update(ctx, sam, in[2].ID, p)
		if err != nil || last.Recurrence != nil || filesOf(last) != want {
			t.Fatalf("on its own: %s %v", filesOf(last), err)
		}
		if read, err := h.s.Get(ctx, sam, last.ID); err != nil || filesOf(read) != want {
			t.Errorf("read back: %s %v", filesOf(read), err)
		}
	})

	t.Run("the following deleted, and brought back", func(t *testing.T) {
		h, sam, maya, _, in := stand(t)
		if err := h.s.Delete(ctx, sam, in[2].ID, ScopeFollowing, nil); err != nil {
			t.Fatal(err)
		}
		trash, err := h.s.Trash(ctx, sam, nil)
		if err != nil || len(trash) != 1 || filesOf(&trash[0]) != want {
			t.Fatalf("the trash: %+v %v", trash, err)
		}
		if _, err := h.s.Restore(ctx, sam, trash[0].ID); err != nil {
			t.Fatal(err)
		}
		everyInstance(t, h, maya, want)
	})
}

// An instance its series no longer has comes back from the trash as an event
// on its own, with the series' guests and the series' files (the same ones,
// put on by the same person at the same time).
func TestAnInstanceThatComesBackOnItsOwnKeepsTheSeriesFiles(t *testing.T) {
	h := withFiles(newHarness(t))
	ctx := context.Background()
	lon := zone(t, "Europe/London")
	sam, maya := h.member("Sam"), h.member("Maya")
	e := h.series(sam, "Stand-up", "FREQ=WEEKLY;BYDAY=TU", time.Date(2026, 10, 20, 9, 15, 0, 0, lon), 15, maya)
	if _, err := h.s.AddFile(ctx, sam, e.ID, doc(briefDoc)); err != nil {
		t.Fatal(err)
	}
	in := h.between(sam, time.Date(2026, 10, 19, 0, 0, 0, 0, lon), time.Date(2026, 11, 1, 0, 0, 0, 0, lon))
	off := patchAt(in[1], ScopeThis)
	off.Title = str("Offsite")
	if _, err := h.s.Update(ctx, sam, in[1].ID, off); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Update(ctx, sam, e.ID, EventPatch{Sequence: seq(0), Recurrence: &Recurrence{Rule: "FREQ=WEEKLY;BYDAY=WE"}}); err != nil {
		t.Fatal(err)
	}
	trash, err := h.s.Trash(ctx, sam, nil)
	if err != nil || len(trash) != 1 || filesOf(&trash[0]) != "doc:"+briefDoc {
		t.Fatalf("the trash: %+v %v", trash, err)
	}
	back, err := h.s.Restore(ctx, sam, trash[0].ID)
	if err != nil || back.SeriesID != "" || filesOf(back) != "doc:"+briefDoc {
		t.Fatalf("back on its own: %s %v", filesOf(back), err)
	}
	if read, err := h.s.Get(ctx, maya, back.ID); err != nil || filesOf(read) != "doc:"+briefDoc {
		t.Errorf("Maya reads it with %s %v", filesOf(read), err)
	}
}

// A meeting's node links the docs it carries as files, beside those its
// notes link, so a doc's backlinks show the meeting; an event of one person
// alone with a doc on it is a node for that doc. A board isn't a node's link
// (workGraph.RefTargets), and a file taken off leaves the node.
func TestAMeetingsNodeLinksTheDocsItCarries(t *testing.T) {
	h := withFiles(newHarness(t))
	ctx := context.Background()
	sam := h.member("Sam")
	cal, err := h.s.store.personalCalendar(ctx, personOf(sam), "UTC")
	if err != nil {
		t.Fatal(err)
	}
	e := h.meeting(sam, cal, func(e *models.Event) {
		e.Notes, e.NotesFormat = docChip("doc", planDoc), notesHTML
	})
	for _, ref := range []FileRef{doc(briefDoc), board(roomBoard), doc(planDoc)} {
		if _, err := h.s.AddFile(ctx, sam, e.ID.String(), ref); err != nil {
			t.Fatal(err)
		}
	}
	p, ok := h.nodes(e.ID)[e.ID.String()]
	if !ok {
		t.Fatal("no node")
	}
	links := slices.Clone(p.Edges[workGraph.RefersTo])
	slices.SortFunc(links, func(a, b workGraph.Ref) int { return strings.Compare(a.UUID, b.UUID) })
	if want := []workGraph.Ref{{Kind: workGraph.KindDoc, UUID: planDoc}, {Kind: workGraph.KindDoc, UUID: briefDoc}}; !slices.Equal(links, want) {
		t.Errorf("links %v, want %v", links, want)
	}

	alone := h.meeting(sam, cal, nil)
	if _, ok := h.nodes(alone.ID)[alone.ID.String()]; ok {
		t.Fatal("an event of one person alone has a node")
	}
	if _, err := h.s.AddFile(ctx, sam, alone.ID.String(), doc(briefDoc)); err != nil {
		t.Fatal(err)
	}
	if p, ok := h.nodes(alone.ID)[alone.ID.String()]; !ok || len(p.Edges[workGraph.RefersTo]) != 1 {
		t.Errorf("with a doc on it: %+v %v", p, ok)
	}
	if _, err := h.s.RemoveFile(ctx, sam, alone.ID.String(), doc(briefDoc)); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.nodes(alone.ID)[alone.ID.String()]; ok {
		t.Error("the doc taken off, it still has a node")
	}
}

// The audit log says a file was added to or removed from an event, with its
// kind and id, and never a title.
func TestTheAuditSaysAFileWasAddedOrRemoved(t *testing.T) {
	for file, want := range map[string]string{"added": "Added a file to an event", "removed": "Removed a file from an event"} {
		summary, meta := auditRow(resourceEventBusiness.Event{Kind: resourceEventBusiness.CalendarEventUpdated, ResourceKind: KindEvent,
			ResourceUUID: "e1", Actor: resourceEventBusiness.Actor{UserUUID: "u1"},
			Data: map[string]string{"kind": "event", "changed": "files", "file": file, "file_kind": "doc", "file_uuid": briefDoc}})
		if summary != want || meta["file_kind"] != "doc" || meta["file_uuid"] != briefDoc {
			t.Errorf("%s: %q %v", file, summary, meta)
		}
	}
}
