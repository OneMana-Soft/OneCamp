package business

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	retentionBusiness "github.com/akashc777/OneCamp/business/Retention"
	domain "github.com/akashc777/OneCamp/domain/Board"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// Boards under retention (miro-44 head R, main's ruling 3 Nov 20:3x): a rule
// removes only boards in the trash longer than its days, never a live one, and
// never one a hold keeps: a board a held person made or edits, in the trash
// or not.
func TestARuleRemovesOnlyTrashedBoardsNoHoldKeeps(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	cutoff := now.AddDate(0, 0, -30)
	at := func(days int) *time.Time { d := now.AddDate(0, 0, -days); return &d }
	person := func(id string) *dgraphStruct.DgraphUser { return &dgraphStruct.DgraphUser{Uuid: id} }
	held := map[string]bool{"held-maker": true, "held-editor": true}
	epoch := time.Unix(0, 0).UTC()

	cases := []struct {
		name  string
		board *dgraphStruct.DgraphBoard
		want  bool
	}{
		{"a live board, however old", &dgraphStruct.DgraphBoard{CreatedBy: person("a"), CreatedAt: at(900)}, false},
		// A live node's deletion time is 0001-01-01 or the epoch, never unset
		// alone (helpers.IsSoftDeleted, dgraphquery.Live): both are before any
		// cutoff, and neither is the trash.
		{"a live board whose deletion time is 0001-01-01", &dgraphStruct.DgraphBoard{CreatedBy: person("a"), DeletedAt: &time.Time{}}, false},
		{"a live board whose deletion time is the epoch", &dgraphStruct.DgraphBoard{CreatedBy: person("a"), DeletedAt: &epoch}, false},
		{"a board in the trash past the rule's days", &dgraphStruct.DgraphBoard{CreatedBy: person("a"), DeletedAt: at(31)}, true},
		{"a board in the trash for fewer days than the rule's", &dgraphStruct.DgraphBoard{CreatedBy: person("a"), DeletedAt: at(29)}, false},
		{"a board trashed exactly at the cutoff", &dgraphStruct.DgraphBoard{CreatedBy: person("a"), DeletedAt: at(30)}, false},
		{"a held person's board in the trash", &dgraphStruct.DgraphBoard{CreatedBy: person("held-maker"), DeletedAt: at(400)}, false},
		{"a board a held person edits, in the trash", &dgraphStruct.DgraphBoard{CreatedBy: person("a"), EditingUser: []*dgraphStruct.DgraphUser{person("b"), person("held-editor")}, DeletedAt: at(400)}, false},
		{"a board whose maker isn't known, in the trash", &dgraphStruct.DgraphBoard{DeletedAt: at(400)}, true},
		{"a board a held person only reads, in the trash", &dgraphStruct.DgraphBoard{CreatedBy: person("a"), ReadingUser: []*dgraphStruct.DgraphUser{person("held-maker")}, DeletedAt: at(400)}, true},
	}
	for _, c := range cases {
		if got := removable(c.board, cutoff, held); got != c.want {
			t.Errorf("%s: removable = %v, want %v", c.name, got, c.want)
		}
	}
	if removable(nil, cutoff, held) {
		t.Error("no board: removable = true, want false")
	}
}

// The boards are business/Retention's second kind, with every part: a kind
// missing one is refused by Register, so the sweep would pass them by.
func TestBoardsAreARetentionKind(t *testing.T) {
	var got *retentionBusiness.Kind
	for _, k := range retentionBusiness.Kinds() {
		if k.Name == "boards" {
			k := k
			got = &k
		}
	}
	if got == nil {
		t.Fatal(`no retention kind "boards": its rule and a hold's search never reach boards`)
	}
	if got.Label != "boards" || got.Policy.Key != policyBusiness.BoardRetentionDays.Key || got.Policy.Default != 0 {
		t.Errorf("boards kind = label %q, policy %q (default %d); want \"boards\", %q, 0 (off until an admin sets it)",
			got.Label, got.Policy.Key, got.Policy.Default, policyBusiness.BoardRetentionDays.Key)
	}
	if strings.Join(got.Formats, ",") != "json" {
		t.Errorf("boards export as %v, want [json]", got.Formats)
	}
}

// purgeSeams fakes what a purge reaches, and records the order it reaches it.
type purgeSeams struct {
	calls     []string
	held      []uuid.UUID
	heldErr   error
	boards    []*domain.TrashedBoard
	nodesErr  error
	left      map[string]bool
	written   []outboxBusiness.Entry
	released  []int64
	purgedIDs []string
}

func (s *purgeSeams) install(t *testing.T) {
	t.Helper()
	oldHeld, oldTrashed, oldWrite, oldNodes, oldIn, oldRelease := heldPeople, trashedBoards, writePurged, purgeBoardNodes, boardsInGraph, releasePurged
	t.Cleanup(func() {
		heldPeople, trashedBoards, writePurged, purgeBoardNodes, boardsInGraph, releasePurged = oldHeld, oldTrashed, oldWrite, oldNodes, oldIn, oldRelease
	})
	heldPeople = func(context.Context) ([]uuid.UUID, error) {
		s.calls = append(s.calls, "held")
		return s.held, s.heldErr
	}
	trashedBoards = func(_ context.Context, _ time.Time, held []uuid.UUID, limit int) ([]*domain.TrashedBoard, error) {
		s.calls = append(s.calls, "trashed")
		return s.boards, nil
	}
	writePurged = func(_ context.Context, es []outboxBusiness.Entry) ([]int64, error) {
		s.calls = append(s.calls, "events")
		s.written = es
		return []int64{7}, nil
	}
	purgeBoardNodes = func(_ context.Context, uids []string, _ time.Time, _ []uuid.UUID) error {
		s.calls = append(s.calls, "nodes")
		s.purgedIDs = uids
		return s.nodesErr
	}
	boardsInGraph = func(_ context.Context, ids []string) (map[string]bool, error) {
		s.calls = append(s.calls, "left")
		return s.left, nil
	}
	releasePurged = func(_ context.Context, ids []int64) { s.calls = append(s.calls, "release"); s.released = ids }
}

func trashedBoard(uid, id, maker string, deleted time.Time, editors ...string) *domain.TrashedBoard {
	b := &domain.TrashedBoard{}
	b.Uid, b.Uuid, b.DeletedAt = uid, id, &deleted
	b.CreatedBy = &dgraphStruct.DgraphUser{Uuid: maker}
	for _, e := range editors {
		b.EditingUser = append(b.EditingUser, &dgraphStruct.DgraphUser{Uuid: e})
	}
	return b
}

// A purge writes its search and files events (held, committed) BEFORE the
// graph's delete, which can't be rolled back, releases them after it, and
// answers how many boards are gone from the graph. A board the graph's filter
// let through though a hold keeps it is still not removed.
func TestAPurgeWritesItsEventsBeforeTheGraphDelete(t *testing.T) {
	cutoff := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	old := cutoff.AddDate(0, 0, -5)
	editor := uuid.New()
	s := &purgeSeams{held: []uuid.UUID{editor}, boards: []*domain.TrashedBoard{
		trashedBoard("0x1", "b1", "a", old),
		trashedBoard("0x2", "b2", "a", old, editor.String()),
		trashedBoard("0x3", "b3", "a", old),
	}, left: map[string]bool{}}
	s.install(t)
	n, err := purgeRemovable(context.Background(), cutoff, 500)
	if err != nil || n != 2 {
		t.Fatalf("purgeRemovable = %d, %v; want 2, nil", n, err)
	}
	if got := strings.Join(s.calls, ","); got != "held,trashed,events,nodes,left,release" {
		t.Errorf("a purge ran %s; want held,trashed,events,nodes,left,release (events committed before the graph's delete)", got)
	}
	if strings.Join(s.purgedIDs, ",") != "0x1,0x3" {
		t.Errorf("the graph's delete took %v; want [0x1 0x3]: a held editor's board stays", s.purgedIDs)
	}
	if len(s.written) != 1 || s.written[0].Topic != purgedTopic || s.written[0].Entities["ids"] != "b1,b3" {
		t.Errorf("events = %+v; want one %s event naming b1,b3", s.written, purgedTopic)
	}
	if len(s.released) != 1 || s.released[0] != 7 {
		t.Errorf("released %v; want [7]", s.released)
	}

	// A board restored between the read and the delete stays in the graph,
	// and isn't counted.
	s2 := &purgeSeams{boards: []*domain.TrashedBoard{trashedBoard("0x1", "b1", "a", old), trashedBoard("0x3", "b3", "a", old)},
		left: map[string]bool{"b3": true}}
	s2.install(t)
	if n, err := purgeRemovable(context.Background(), cutoff, 500); err != nil || n != 1 {
		t.Errorf("with b3 restored mid-purge, purgeRemovable = %d, %v; want 1, nil", n, err)
	}
}

// A purge whose graph delete fails leaves its events held, for the resolver
// to cancel; one that can't read who is held removes nothing.
func TestAPurgeThatFailsRemovesNothingItCantStandBehind(t *testing.T) {
	cutoff := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	s := &purgeSeams{boards: []*domain.TrashedBoard{trashedBoard("0x1", "b1", "a", cutoff.AddDate(0, 0, -1))}, nodesErr: errors.New("dgraph down")}
	s.install(t)
	if _, err := purgeRemovable(context.Background(), cutoff, 500); err == nil {
		t.Error("a failed graph delete answered no error")
	}
	if s.released != nil {
		t.Errorf("a failed graph delete released its events %v; they stay held for the resolver", s.released)
	}

	s2 := &purgeSeams{heldErr: errors.New("postgres down")}
	s2.install(t)
	if _, err := purgeRemovable(context.Background(), cutoff, 500); err == nil {
		t.Error("a purge that couldn't read the holds answered no error")
	}
	if strings.Join(s2.calls, ",") != "held" {
		t.Errorf("a purge that couldn't read the holds went on to %v", s2.calls)
	}

	s3 := &purgeSeams{}
	s3.install(t)
	if n, err := purgeRemovable(context.Background(), cutoff, 500); n != 0 || err != nil || strings.Join(s3.calls, ",") != "held,trashed" {
		t.Errorf("nothing to remove: %d, %v after %v; want 0, nil after held,trashed (no event written)", n, err, s3.calls)
	}
}

// The purged boards' sinks forget only boards gone from the graph: their
// search entries (the board's, its comments', its AI text), and what
// Postgres and object storage hold of them, their presenter notes and polls
// included.
// The resolver releases a purge's held events only when its boards did go.
func TestPurgedSinksForgetOnlyBoardsGoneFromTheGraph(t *testing.T) {
	oldIn, oldForget, oldArchive, oldNotes, oldPolls, oldSnaps, oldObj, oldPlaces, oldRows := boardsInGraph, forgetSearchWhere, archiveBoardFiles, dropNotes, dropPolls, dropSnapshots, removeObjects, forgetPlaces, forgetRows
	t.Cleanup(func() {
		boardsInGraph, forgetSearchWhere, archiveBoardFiles, dropNotes, dropPolls, dropSnapshots, removeObjects, forgetPlaces, forgetRows = oldIn, oldForget, oldArchive, oldNotes, oldPolls, oldSnaps, oldObj, oldPlaces, oldRows
	})
	left := map[string]bool{"b2": true}
	boardsInGraph = func(context.Context, []string) (map[string]bool, error) { return left, nil }
	var got []string
	forgetSearchWhere = func(_ context.Context, indices, fields, values []string) (int, error) {
		got = append(got, "search "+strings.Join(indices, "+")+" "+strings.Join(fields, "|")+"="+strings.Join(values, ","))
		return len(values), nil
	}
	archiveBoardFiles = func(_ context.Context, src string, ids []string, _ time.Time) (int64, error) {
		got = append(got, "files "+src+"="+strings.Join(ids, ","))
		return 0, nil
	}
	dropNotes = func(_ context.Context, ids []string) error {
		got = append(got, "notes "+strings.Join(ids, ","))
		return nil
	}
	dropPolls = func(_ context.Context, ids []string) error {
		got = append(got, "polls "+strings.Join(ids, ","))
		return nil
	}
	dropSnapshots = func(_ context.Context, ids []string) ([]string, error) {
		got = append(got, "snapshots "+strings.Join(ids, ","))
		return []string{"boardSnapshot/b1/s1.gz"}, nil
	}
	removeObjects = func(_ context.Context, keys []string) { got = append(got, "objects "+strings.Join(keys, ",")) }
	forgetPlaces = func(_ context.Context, kind string, ids []string) error {
		got = append(got, "places "+kind+"="+strings.Join(ids, ","))
		return nil
	}
	forgetRows = func(_ context.Context, kind string, ids []string) error {
		got = append(got, "rows "+kind+"="+strings.Join(ids, ","))
		return nil
	}
	ev := outboxBusiness.Event{Topic: purgedTopic, Entities: map[string]string{"ids": "b1,b2"}}
	if err := forgetPurgedSearch(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if err := forgetPurgedFiles(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	want := []string{
		// One delete over the three indices, each entry matched by the field that names its board (the store-loop guard).
		"search boards+comments+ai_embeddings _id|comment_board_id|board_uuid=b1",
		"files board=b1",
		// The presenter notes before the snapshots, so a failed notes delete strands no object key.
		"notes b1",
		// Its polls on a slide, their votes with them.
		"polls b1",
		"snapshots b1",
		// The snapshots' objects and the live drawing's, removed in one call.
		"objects boardSnapshot/b1/s1.gz,boardState/b1.gz",
		"places board=b1",
		"rows board=b1",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the sinks did:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}

	left = map[string]bool{"b1": true, "b2": true}
	got = nil
	_ = forgetPurgedSearch(context.Background(), ev)
	_ = forgetPurgedFiles(context.Background(), ev)
	if len(got) != 0 {
		t.Errorf("with every board still in the graph the sinks did %v; want nothing", got)
	}
	if written, err := resolvePurged(context.Background(), ev); err != nil || written {
		t.Errorf("resolver with every board still there = %v, %v; want false (cancel)", written, err)
	}
	left = map[string]bool{"b2": true}
	if written, err := resolvePurged(context.Background(), ev); err != nil || !written {
		t.Errorf("resolver with b1 gone = %v, %v; want true (release)", written, err)
	}

	// A notes delete that fails stops the sink before the snapshots, so its
	// retry finds their rows and object keys again.
	dropNotes = func(context.Context, []string) error { return errors.New("postgres is down") }
	got = nil
	if err := forgetPurgedFiles(context.Background(), ev); err == nil || strings.Join(got, ",") != "files board=b1" {
		t.Errorf("a failed notes delete: %v after %v; want an error after files board=b1 alone", err, got)
	}
}

// A hold's boards export as one JSON file a part at a time, each board with
// its drawing; a cursor the export never gave is a bad request.
func TestAHoldsBoardsExportInParts(t *testing.T) {
	old := heldBoards
	t.Cleanup(func() { heldBoards = old })
	made := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	trashed := made.AddDate(0, 0, 3)
	var asked []string
	heldBoards = func(_ context.Context, people []uuid.UUID, q domain.HeldBoardQuery, limit int, full bool) ([]*dgraphStruct.DgraphBoard, error) {
		asked = append(asked, q.After)
		all := []*dgraphStruct.DgraphBoard{
			{Uuid: "11111111-1111-1111-1111-111111111111", Title: "Launch plan", CreatedAt: &made, DeletedAt: &time.Time{}},
			{Uuid: "22222222-2222-2222-2222-222222222222", Title: "Old map", CreatedAt: &made, DeletedAt: &trashed},
			{Uuid: "33333333-3333-3333-3333-333333333333", Title: "Retro", CreatedAt: &made},
		}
		var out []*dgraphStruct.DgraphBoard
		for _, b := range all {
			if b.Uuid > q.After && len(out) < limit {
				out = append(out, b)
			}
		}
		return out, nil
	}
	var buf strings.Builder
	w, err := heldExport(context.Background(), &buf, []uuid.UUID{uuid.New()}, retentionBusiness.Query{}, "json", retentionBusiness.Part{Number: 1, Size: 2})
	if err != nil {
		t.Fatal(err)
	}
	if w.N != 2 || w.Next != "22222222-2222-2222-2222-222222222222" {
		t.Errorf("part 1 wrote %+v; want 2 boards and the next after the second", w)
	}
	var file struct {
		Part   int  `json:"part"`
		More   bool `json:"more"`
		Note   string
		Boards []struct {
			ID        string     `json:"id"`
			Title     string     `json:"title"`
			DeletedAt *time.Time `json:"deleted_at"`
		} `json:"boards"`
	}
	if err := json.Unmarshal([]byte(buf.String()), &file); err != nil {
		t.Fatalf("part 1 isn't JSON: %v\n%s", err, buf.String())
	}
	if file.Part != 1 || !file.More || file.Note == "" || len(file.Boards) != 2 {
		t.Errorf("part 1 = %+v; want part 1 of more, with a note, holding 2 boards", file)
	}
	if file.Boards[0].DeletedAt != nil || file.Boards[1].DeletedAt == nil {
		t.Errorf("deleted_at = %v, %v; want none for the live board, the trash time for the trashed one", file.Boards[0].DeletedAt, file.Boards[1].DeletedAt)
	}
	buf.Reset()
	w, err = heldExport(context.Background(), &buf, []uuid.UUID{uuid.New()}, retentionBusiness.Query{}, "json", retentionBusiness.Part{Number: 2, Size: 2, After: w.Next})
	if err != nil || w.N != 1 || w.Next != "" {
		t.Errorf("part 2 = %+v, %v; want the last board and no next", w, err)
	}
	if _, err := heldExport(context.Background(), &buf, nil, retentionBusiness.Query{}, "json", retentionBusiness.Part{Number: 2, Size: 2, After: "not-a-cursor"}); !errors.Is(err, retentionBusiness.ErrBadRequest) {
		t.Errorf("a cursor the export never gave: %v; want ErrBadRequest", err)
	}
}

// A hold's search lists a held person's boards, deleted ones with their time.
func TestAHoldsSearchListsBoardsDeletedOnesIncluded(t *testing.T) {
	old := heldBoards
	t.Cleanup(func() { heldBoards = old })
	trashed := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	private := true
	heldBoards = func(_ context.Context, _ []uuid.UUID, q domain.HeldBoardQuery, _ int, full bool) ([]*dgraphStruct.DgraphBoard, error) {
		if full || q.Text != "plan" {
			t.Errorf("search asked full=%v text=%q; want no drawings, the escaped term", full, q.Text)
		}
		return []*dgraphStruct.DgraphBoard{
			{Uuid: "b1", Title: "Launch plan", CreatedBy: &dgraphStruct.DgraphUser{UserFullName: "Priya Raman"}, DeletedAt: &time.Time{}},
			{Uuid: "b2", Title: "", IsPrivate: &private, DeletedAt: &trashed},
		}, nil
	}
	items, err := heldSearch(context.Background(), []uuid.UUID{uuid.New()}, retentionBusiness.Query{Text: " plan "}, 50)
	if err != nil || len(items) != 2 {
		t.Fatalf("heldSearch = %v, %v", items, err)
	}
	if items[0].ID != "b1" || items[0].Title != "Launch plan" || items[0].DeletedAt != nil || !strings.Contains(items[0].Detail, "Priya Raman") {
		t.Errorf("a live board = %+v", items[0])
	}
	if items[1].Title != "Untitled board" || items[1].DeletedAt == nil || !strings.Contains(items[1].Detail, "Private") {
		t.Errorf("a private trashed board = %+v", items[1])
	}
}
