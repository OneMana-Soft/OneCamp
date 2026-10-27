package business

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	domain "github.com/akashc777/OneCamp/domain/Attachment"
	"github.com/google/uuid"
)

// heldFiles is a test's answers for ArchiveFilesOf's reads, and what it wrote.
type heldFiles struct {
	held    domain.Held
	rows    map[uuid.UUID]domain.Row
	holders map[uuid.UUID][]domain.HolderNode
	imports map[uuid.UUID][]domain.ImportNode
	fail    string // the step that fails: "graph", "rows", "holders" or "imports"
	stamped [][]uuid.UUID
	at      time.Time
}

var errDown = errors.New("down")

// seams swaps ArchiveFilesOf's reads and writes for h's.
func (h *heldFiles) seams(t *testing.T) {
	t.Helper()
	prevHeld, prevRows, prevHolders, prevImports, prevArchive := archiveHeldFiles, rowsOf, liveHolders, importNodesOf, archiveRows
	t.Cleanup(func() {
		archiveHeldFiles, rowsOf, liveHolders, importNodesOf, archiveRows = prevHeld, prevRows, prevHolders, prevImports, prevArchive
	})
	failing := func(step string) error {
		if h.fail == step {
			return errDown
		}
		return nil
	}
	archiveHeldFiles = func(context.Context, domain.Holder, uuid.UUID, time.Time) (domain.Held, error) {
		return h.held, failing("graph")
	}
	rowsOf = func(context.Context, []uuid.UUID) (map[uuid.UUID]domain.Row, error) {
		return h.rows, failing("rows")
	}
	liveHolders = func(context.Context, []uuid.UUID) (map[uuid.UUID][]domain.HolderNode, error) {
		return h.holders, failing("holders")
	}
	importNodesOf = func(context.Context, []uuid.UUID) (map[uuid.UUID][]domain.ImportNode, error) {
		return h.imports, failing("imports")
	}
	archiveRows = func(_ context.Context, ids []uuid.UUID, at time.Time) (int, error) {
		h.stamped, h.at = append(h.stamped, ids), at
		return len(ids), nil
	}
}

// A deleted item's files go with it, at the time given, but for one an item
// still there holds: its pair (a reply's copy), an item the file's uploader
// holds it in, or an item written before requests stopped naming files (19
// Oct), whoever wrote it, or with no time at all. Someone else's node naming
// the file's id since then keeps nothing, nor does the uploader's own node
// when the row records no uploader.
func TestADeletedItemsFilesGoButForOnesAnItemStillThereHolds(t *testing.T) {
	mia, ann := uuid.New(), uuid.New()
	since := requestsNamedFilesUntil.Add(time.Minute)
	ids := make([]uuid.UUID, 10)
	for i := range ids {
		ids[i] = uuid.New()
	}
	own, onCopy, forwarded, named, untimed, namedBefore, namedAtTheCut, noUploader, oldNoUploader, notShown :=
		ids[0], ids[1], ids[2], ids[3], ids[4], ids[5], ids[6], ids[7], ids[8], ids[9]
	at := time.Date(2026, 10, 26, 6, 0, 0, 0, time.UTC)
	by := func(ids ...uuid.UUID) map[uuid.UUID]bool {
		out := map[uuid.UUID]bool{}
		for _, id := range ids {
			out[id] = true
		}
		return out
	}
	uploaded := domain.Row{By: mia, At: since.Add(-time.Hour)}
	h := &heldFiles{
		held: domain.Held{Files: ids, Paired: map[uuid.UUID]bool{onCopy: true}},
		rows: map[uuid.UUID]domain.Row{own: uploaded, forwarded: uploaded, named: uploaded, untimed: uploaded, namedBefore: uploaded,
			namedAtTheCut: uploaded, noUploader: {At: since}, notShown: uploaded},
		holders: map[uuid.UUID][]domain.HolderNode{
			forwarded:     {{Uid: 1, By: by(mia), At: since}},
			named:         {{Uid: 2, By: by(ann), At: since}},
			untimed:       {{Uid: 3, By: by()}},
			namedBefore:   {{Uid: 4, By: by(ann), At: requestsNamedFilesUntil.Add(-time.Minute)}, {Uid: 5, By: by(ann), At: since}},
			namedAtTheCut: {{Uid: 6, By: by(ann), At: requestsNamedFilesUntil}},
			noUploader:    {{Uid: 7, By: by(mia), At: since}},
			oldNoUploader: {{Uid: 8, By: by(), At: requestsNamedFilesUntil.Add(-24 * time.Hour)}},
		},
	}
	h.seams(t)
	taken, err := ArchiveFilesOf(context.Background(), PostFiles, uuid.New(), at)
	if err != nil {
		t.Fatal(err)
	}
	want := []uuid.UUID{own, named, namedAtTheCut, noUploader, notShown}
	if len(h.stamped) != 1 || !slices.Equal(h.stamped[0], want) || !h.at.Equal(at) || taken != len(want) {
		t.Fatalf("stamped rows %v at %v (%d): want the post's own, the ones named since 19 Oct by someone else or at the cut, "+
			"the one with no uploader and the one nothing shows; not the copy's, the forward's or the older items'", h.stamped, h.at, taken)
	}
}

// A failed read or write stops before any row is stamped, and says so: the
// delete answers its error, and running it again finishes the job.
func TestAFailedStepStampsNoRow(t *testing.T) {
	file := uuid.New()
	for _, step := range []string{"graph", "rows", "holders", "imports"} {
		h := &heldFiles{held: domain.Held{Files: []uuid.UUID{file}}, fail: step}
		h.seams(t)
		if _, err := ArchiveFilesOf(context.Background(), ChatFiles, uuid.New(), time.Now()); !errors.Is(err, errDown) {
			t.Fatalf("%s failing: %v", step, err)
		}
		if len(h.stamped) != 0 {
			t.Fatalf("%s failing stamped %v", step, h.stamped)
		}
	}
}

// An item that holds no file, or only files its pair still shows, asks
// nothing more and stamps nothing.
func TestNothingToTakeOffAsksNothing(t *testing.T) {
	shown := uuid.New()
	for _, held := range []domain.Held{{}, {Files: []uuid.UUID{shown}, Paired: map[uuid.UUID]bool{shown: true}}} {
		h := &heldFiles{held: held, fail: "rows"}
		h.seams(t)
		if taken, err := ArchiveFilesOf(context.Background(), CommentFiles, uuid.New(), time.Now()); err != nil || taken != 0 || len(h.stamped) != 0 {
			t.Fatalf("held %+v: %d, %v, stamped %v", held, taken, err, h.stamped)
		}
	}
}

// A file an import made: the import's file stage wrote each file twice, the
// node its item lists and, before it, one more with the very same time that
// nothing lists. Once the listed node is stamped deleted that other node
// holds nothing, whoever's item went; a node written after the listed one
// still holds by the rules, a copy that carries its time included; and an
// export's time is never older than the import that made the file (its row).
func TestAnImportsOtherNodeOfAFileHoldsNothing(t *testing.T) {
	mia, ann := uuid.New(), uuid.New()
	ids := make([]uuid.UUID, 7)
	for i := range ids {
		ids[i] = uuid.New()
	}
	twin, forwarded, stillListed, copied, exported, untimed, beforeTheCut := ids[0], ids[1], ids[2], ids[3], ids[4], ids[5], ids[6]
	export := time.Date(2025, 3, 4, 10, 15, 0, 123456000, time.UTC)
	importedAt := requestsNamedFilesUntil.Add(48 * time.Hour)
	importedEarlier := requestsNamedFilesUntil.Add(-9 * 24 * time.Hour)
	byMia, byAnn := map[uuid.UUID]bool{mia: true}, map[uuid.UUID]bool{ann: true}
	rows := map[uuid.UUID]domain.Row{}
	for _, id := range ids {
		rows[id] = domain.Row{By: mia, At: importedAt}
	}
	rows[copied], rows[beforeTheCut] = domain.Row{By: mia, At: importedEarlier}, domain.Row{By: mia, At: importedEarlier}
	stampedAt := func(uid uint64, at time.Time) []domain.ImportNode {
		return []domain.ImportNode{{Uid: uid, At: at, Stamped: true}}
	}
	h := &heldFiles{
		held: domain.Held{Files: ids},
		rows: rows,
		holders: map[uuid.UUID][]domain.HolderNode{
			// The import's other node: its item's node is stamped.
			twin: {{Uid: 10, At: export, By: byMia}},
			// The same, and the uploader's forward of the item, since.
			forwarded: {{Uid: 10, At: export, By: byMia}, {Uid: 30, At: importedAt.Add(time.Hour), By: byMia}},
			// The import's item still lists its node.
			stillListed: {{Uid: 10, At: export, By: byMia}, {Uid: 20, At: export, By: byMia}},
			// Ann's message of before 19 Oct naming the file with its node's
			// time, written after it, of an import made before then.
			copied: {{Uid: 10, At: export, By: byMia}, {Uid: 40, At: export, By: byAnn}},
			// Ann's node with an export's time, of an import made since.
			exported: {{Uid: 50, At: export, By: byAnn}},
			// A node with no time keeps the rule for one.
			untimed: {{Uid: 10, By: map[uuid.UUID]bool{}}},
			// Ann's node with an export's time, of an import made before 19 Oct.
			beforeTheCut: {{Uid: 60, At: export, By: byAnn}},
		},
		imports: map[uuid.UUID][]domain.ImportNode{
			twin:         stampedAt(20, export),
			forwarded:    stampedAt(20, export),
			stillListed:  {{Uid: 20, At: export}},
			copied:       stampedAt(20, export),
			exported:     {},
			untimed:      {{Uid: 20, Stamped: true}},
			beforeTheCut: {},
		},
	}
	h.seams(t)
	taken, err := ArchiveFilesOf(context.Background(), ChatFiles, uuid.New(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	want := []uuid.UUID{twin, exported}
	if len(h.stamped) != 1 || !slices.Equal(h.stamped[0], want) || taken != len(want) {
		t.Fatalf("stamped rows %v (%d): want the twin's and the one only an export's time held; not the forwarded, "+
			"the still listed, the copied, the untimed or the one imported before 19 Oct", h.stamped, taken)
	}
}
