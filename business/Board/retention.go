package business

// Boards under retention rules and holds (miro-44 head R; main's ruling, 3 Nov
// 20:3x): boards become business/Retention's second kind, after the calendar.
// A rule removes for good only boards in the trash longer than its days, never
// a live one, and never one a hold keeps. An admin searches a hold's boards
// and exports them, deleted ones included, as JSON with their drawings.
//
// HOW A PURGE STAYS WHOLE. A board lives in the graph, its search entries in
// OpenSearch, and its files, snapshots and people's rows in Postgres and
// object storage; no one transaction spans them. So a purge writes its outbox
// events first, held, in their own committed transaction; then removes the
// boards from the graph (asking again, in the same request, which are still
// in the trash and held by nobody); then releases the events. The events'
// sinks forget, for each board they name, only what is gone from the graph,
// so a repeat, a late run or a board restored in between does no harm. A
// purge that stops between its two writes leaves its events held, and the
// topic's resolver releases them if any of their boards went, or cancels
// them. The sweep's one audit row counts what went (business/Retention).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/business/Board/scene"
	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
	placesBusiness "github.com/akashc777/OneCamp/business/Places"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	retentionBusiness "github.com/akashc777/OneCamp/business/Retention"
	domain "github.com/akashc777/OneCamp/domain/Board"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	globalSearchModels "github.com/akashc777/OneCamp/models/openSearch/GlobalSearch"
	"github.com/akashc777/OneCamp/models/pgexec"
	postgressStruct "github.com/akashc777/OneCamp/models/postgres"
	attachmentModels "github.com/akashc777/OneCamp/models/postgres/Attachment"
	snapshotModels "github.com/akashc777/OneCamp/models/postgres/BoardSnapshot"
	placesModel "github.com/akashc777/OneCamp/models/postgres/Places"
	retentionModel "github.com/akashc777/OneCamp/models/postgres/Retention"
	"github.com/google/uuid"
)

// removable is whether a rule with cutoff removes board: it went to the trash
// before cutoff, and no held person made it or edits it. A board in the trash
// is still a held person's: a hold keeps what they had, deleted or not. A
// reader's board isn't theirs; a board whose maker isn't known is nobody's.
func removable(board *dgraphStruct.DgraphBoard, cutoff time.Time, held map[string]bool) bool {
	// A live board's deletion time is 0001-01-01 or the epoch: before any
	// cutoff, and not the trash.
	if board == nil || !helpers.IsSoftDeleted(board.DeletedAt) || !board.DeletedAt.Before(cutoff) {
		return false
	}
	if board.CreatedBy != nil && held[board.CreatedBy.Uuid] {
		return false
	}
	for _, editor := range board.EditingUser {
		if editor != nil && held[editor.Uuid] {
			return false
		}
	}
	return true
}

// The boards' retention names: the kind, its items as a count reads, and the
// outbox topic a purge's events go on, with its two sinks (each retried on
// its own: search being down never redoes the files).
const (
	retentionKind    = "boards"
	retentionLabel   = "boards"
	purgedTopic      = "board.purged"
	kindPurged       = "boards.purged"
	purgedSearchSink = "board.purged.search"
	purgedFilesSink  = "board.purged.files"
	// purgedPage is how many boards one outbox event names.
	purgedPage = 100
	// boardAIIndex holds a board's text for the AI, by board_uuid.
	boardAIIndex = "ai_embeddings"
	// boardExportPart is the most boards one part of a hold's export holds,
	// and boardExportBytes the most it writes past its first board: a board's
	// drawing can be megabytes, where an event is a few hundred bytes.
	boardExportPart  = 100
	boardExportBytes = 64 << 20
)

func init() {
	retentionBusiness.Register(retentionBusiness.Kind{
		Name:    retentionKind,
		Label:   retentionLabel,
		Policy:  policyBusiness.BoardRetentionDays,
		Count:   countRemovable,
		Purge:   purgeRemovable,
		Search:  heldSearch,
		Export:  heldExport,
		Formats: []string{"json"},
	})
	outboxBusiness.Sinks.Register(outboxBusiness.Sink{Topic: purgedTopic, Name: purgedSearchSink, Run: forgetPurgedSearch})
	outboxBusiness.Sinks.Register(outboxBusiness.Sink{Topic: purgedTopic, Name: purgedFilesSink, Run: forgetPurgedFiles})
	outboxBusiness.Sinks.Resolve(purgedTopic, resolvePurged)
}

// What retention reaches, seams for tests.
var (
	heldPeople      = retentionModel.Held
	countTrashed    = domain.CountTrashedBoardsBefore
	trashedBoards   = domain.TrashedBoardsBefore
	purgeBoardNodes = domain.PurgeBoardNodes
	boardsInGraph   = domain.BoardUUIDsInGraph
	heldBoards      = domain.HeldBoards
	// writePurged writes a purge's events held, in a transaction of their
	// own, committed before the graph's delete.
	writePurged = func(ctx context.Context, es []outboxBusiness.Entry) ([]int64, error) {
		var ids []int64
		err := pgexec.InTx(ctx, func(ctx context.Context) error {
			tx, err := pgexec.Current(ctx)
			if err != nil {
				return err
			}
			ids, err = outboxBusiness.AddMany(ctx, tx, es)
			return err
		})
		return ids, err
	}
	// releasePurged releases a purge's events once its boards are gone, and
	// delivers them now; those it can't release the drain releases later.
	releasePurged = func(ctx context.Context, ids []int64) {
		if err := outboxBusiness.ReleaseMany(ctx, ids); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Board retention: %d purge events wait for the outbox's drain: %v", len(ids), err)
			return
		}
		outboxBusiness.DeliverMany(ids)
	}
	forgetSearchWhere = globalSearchModels.ForgetWhere
	archiveBoardFiles = attachmentModels.ArchivePlaces
	dropNotes         = forgetBoardsNotes
	dropPolls         = forgetBoardsPolls
	dropSnapshots     = snapshotModels.DeleteBoardsSnapshots
	removeObjects     = removeSnapshotObjects
	forgetPlaces      = placesBusiness.Forget
	forgetRows        = placesModel.ForgetResourceRows
)

// countRemovable is the boards' Count: how many a rule with cutoff removes
// now, held people's kept.
func countRemovable(ctx context.Context, cutoff time.Time) (int64, error) {
	held, err := heldPeople(ctx)
	if err != nil {
		return 0, err
	}
	return countTrashed(ctx, cutoff, held)
}

// purgeRemovable is the boards' Purge: at most limit of the boards a rule with
// cutoff removes, for good, and what they leave elsewhere through the outbox
// (the file's header says in what order, and why). It answers how many are
// gone. A purge that can't read who is held removes nothing.
func purgeRemovable(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	held, err := heldPeople(ctx)
	if err != nil {
		return 0, err
	}
	boards, err := trashedBoards(ctx, cutoff, held, limit)
	if err != nil {
		return 0, err
	}
	keep := make(map[string]bool, len(held))
	for _, p := range held {
		keep[p.String()] = true
	}
	var uids, ids []string
	for _, b := range boards {
		if removable(b, cutoff, keep) {
			uids = append(uids, b.Uid)
			ids = append(ids, b.Uuid)
		}
	}
	if len(ids) == 0 {
		return 0, nil
	}
	events, err := writePurged(ctx, purgedEntries(ids))
	if err != nil {
		return 0, err
	}
	if err := purgeBoardNodes(ctx, uids, cutoff, held); err != nil {
		return 0, err
	}
	left, err := boardsInGraph(ctx, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Board retention: which purged boards went couldn't be read, so all %d are counted: %v", len(ids), err)
	}
	releasePurged(ctx, events)
	return int64(len(ids) - len(left)), nil
}

// purgedEntries are a purge's outbox events: one per purgedPage boards. Pure.
func purgedEntries(ids []string) []outboxBusiness.Entry {
	var es []outboxBusiness.Entry
	for from := 0; from < len(ids); from += purgedPage {
		page := ids[from:min(from+purgedPage, len(ids))]
		es = append(es, outboxBusiness.Entry{Topic: purgedTopic, Kind: kindPurged, Key: "board.purged:" + page[0],
			Entities: map[string]string{"ids": strings.Join(page, ",")}})
	}
	return es
}

// gonePurged is which of the boards ev names are gone from the graph.
func gonePurged(ctx context.Context, ev outboxBusiness.Event) ([]string, error) {
	var ids []string
	for _, id := range strings.Split(ev.Entities["ids"], ",") {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	left, err := boardsInGraph(ctx, ids)
	if err != nil {
		return nil, err
	}
	var gone []string
	for _, id := range ids {
		if !left[id] {
			gone = append(gone, id)
		}
	}
	return gone, nil
}

// forgetPurgedSearch is the search sink: the entries of purged boards, their
// comments' and their AI text's, removed for good.
func forgetPurgedSearch(ctx context.Context, ev outboxBusiness.Event) error {
	gone, err := gonePurged(ctx, ev)
	if err != nil || len(gone) == 0 {
		return err
	}
	// One delete over the three indices: each entry is matched by the field
	// that names its board (its own id, its comment's board, its AI text's).
	indices := []string{openSearchStruct.BOARD_INDEX, openSearchStruct.COMMENT_INDEX, boardAIIndex}
	if _, err := forgetSearchWhere(ctx, indices, []string{"_id", "comment_board_id", "board_uuid"}, gone); err != nil {
		return fmt.Errorf("the search entries of %d purged boards: %w", len(gone), err)
	}
	return nil
}

// forgetPurgedFiles is the files sink: what Postgres and object storage hold
// of purged boards. Their files are archived, so the attachments purge takes
// the bytes as it does a doc's; their presenter notes, polls (with their
// votes), snapshots and live drawing go now; and so do people's places, views, saves and guest links on
// them.
func forgetPurgedFiles(ctx context.Context, ev outboxBusiness.Event) error {
	gone, err := gonePurged(ctx, ev)
	if err != nil || len(gone) == 0 {
		return err
	}
	if _, err := archiveBoardFiles(ctx, postgressStruct.ATTACHMENT_SRC_BOARD, gone, time.Now()); err != nil {
		return fmt.Errorf("the files of %d purged boards: %w", len(gone), err)
	}
	// The notes before the snapshots: a failed notes delete then retries with
	// the snapshots' rows, and so their object keys, still there.
	if err := dropNotes(ctx, gone); err != nil {
		return fmt.Errorf("the presenter notes of %d purged boards: %w", len(gone), err)
	}
	if err := dropPolls(ctx, gone); err != nil {
		return fmt.Errorf("the polls of %d purged boards: %w", len(gone), err)
	}
	keys, err := dropSnapshots(ctx, gone)
	if err != nil {
		return fmt.Errorf("the snapshots of %d purged boards: %w", len(gone), err)
	}
	for _, id := range gone {
		keys = append(keys, liveStateObjectKey(id))
	}
	removeObjects(ctx, keys)
	if err := forgetPlaces(ctx, retentionKindResource, gone); err != nil {
		return fmt.Errorf("people's places on %d purged boards: %w", len(gone), err)
	}
	return forgetRows(ctx, retentionKindResource, gone)
}

// retentionKindResource is a board as people's rows name it.
const retentionKindResource = "board"

// resolvePurged decides a purge's event its writer never released: written if
// any board it names is gone from the graph (the sinks forget only those),
// cancelled if every one is still there.
func resolvePurged(ctx context.Context, ev outboxBusiness.Event) (bool, error) {
	gone, err := gonePurged(ctx, ev)
	if err != nil {
		return false, err
	}
	return len(gone) > 0, nil
}

// heldBoardQuery is q as the graph takes it: its words escaped for a regexp.
func heldBoardQuery(q retentionBusiness.Query, after string) (domain.HeldBoardQuery, error) {
	out := domain.HeldBoardQuery{From: q.From, To: q.To, After: after}
	if strings.TrimSpace(q.Text) == "" {
		return out, nil
	}
	text, err := dgraphquery.SanitizeSearchTerm(q.Text)
	if err != nil && err != dgraphquery.ErrEmpty {
		return out, fmt.Errorf("%w: search for at most %d characters", retentionBusiness.ErrBadRequest, dgraphquery.MaxSearchTermLen)
	}
	out.Text = text
	return out, nil
}

// deletedAt is a board's trash time, or nil for a live one.
func deletedAt(b *dgraphStruct.DgraphBoard) *time.Time {
	if b == nil || !helpers.IsSoftDeleted(b.DeletedAt) {
		return nil
	}
	return b.DeletedAt
}

// personName is how a hold's search and export name someone on a board.
func personName(u *dgraphStruct.DgraphUser) string {
	if u == nil {
		return ""
	}
	if u.UserFullName != "" {
		return u.UserFullName
	}
	return u.UserName
}

// heldSearch is the boards' Search: people's boards matching q, deleted ones
// included. A private board is listed too: the searcher is an admin under a
// hold, and the hold's audit row names what was searched.
func heldSearch(ctx context.Context, people []uuid.UUID, q retentionBusiness.Query, limit int) ([]retentionBusiness.Item, error) {
	hq, err := heldBoardQuery(q, "")
	if err != nil {
		return nil, err
	}
	boards, err := heldBoards(ctx, people, hq, limit, false)
	if err != nil {
		return nil, err
	}
	out := make([]retentionBusiness.Item, 0, len(boards))
	for _, b := range boards {
		title := b.Title
		if strings.TrimSpace(title) == "" {
			title = "Untitled board"
		}
		var detail []string
		if who := personName(b.CreatedBy); who != "" {
			detail = append(detail, "Made by "+who)
		}
		if b.IsPrivate != nil && *b.IsPrivate {
			detail = append(detail, "Private board")
		}
		out = append(out, retentionBusiness.Item{ID: b.Uuid, Title: title, Detail: strings.Join(detail, " · "), DeletedAt: deletedAt(b)})
	}
	return out, nil
}

// heldPersonOut is someone on a board in a hold's export.
type heldPersonOut struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// heldBoardOut is one board in a hold's export: everything the graph says of
// it, who it was shared with, and its drawing's elements by id.
type heldBoardOut struct {
	ID         string          `json:"id"`
	Title      string          `json:"title"`
	Snippet    string          `json:"snippet,omitempty"`
	Private    bool            `json:"private"`
	CreatedAt  *time.Time      `json:"created_at,omitempty"`
	UpdatedAt  *time.Time      `json:"updated_at,omitempty"`
	DeletedAt  *time.Time      `json:"deleted_at,omitempty"`
	CreatedBy  *heldPersonOut  `json:"created_by,omitempty"`
	Editors    []heldPersonOut `json:"editors"`
	Commenters []heldPersonOut `json:"commenters"`
	Readers    []heldPersonOut `json:"readers"`
	Elements   map[string]any  `json:"elements"`
	// ElementsError says why a drawing couldn't be read, so an export never
	// passes an unreadable board off as an empty one.
	ElementsError string `json:"elements_error,omitempty"`
}

func peopleOut(us []*dgraphStruct.DgraphUser) []heldPersonOut {
	out := []heldPersonOut{}
	for _, u := range us {
		if u != nil {
			out = append(out, heldPersonOut{ID: u.Uuid, Name: personName(u)})
		}
	}
	return out
}

func asHeldBoard(b *dgraphStruct.DgraphBoard) heldBoardOut {
	out := heldBoardOut{ID: b.Uuid, Title: b.Title, Snippet: b.Snippet, Private: b.IsPrivate != nil && *b.IsPrivate,
		CreatedAt: b.CreatedAt, UpdatedAt: b.UpdatedAt, DeletedAt: deletedAt(b), Editors: peopleOut(b.EditingUser),
		Commenters: peopleOut(b.CommentingUser), Readers: peopleOut(b.ReadingUser), Elements: map[string]any{}}
	if b.CreatedBy != nil {
		out.CreatedBy = &heldPersonOut{ID: b.CreatedBy.Uuid, Name: personName(b.CreatedBy)}
	}
	if b.State == "" && b.StateKey != "" {
		out.ElementsError = "the board's drawing couldn't be read from storage"
		return out
	}
	roots, err := scene.StateRoots(b.State)
	if err != nil {
		out.ElementsError = "the board's drawing couldn't be read: " + err.Error()
		return out
	}
	if els := roots["elements"]; els != nil {
		out.Elements = els
	}
	return out
}

// heldExport is the boards' Export: one part of people's boards matching q as
// one JSON file, deleted ones included, each with its drawing. A part holds at
// most boardExportPart boards, and stops early past boardExportBytes; either
// way it says so, and Next is the board it ended on.
func heldExport(ctx context.Context, w io.Writer, people []uuid.UUID, q retentionBusiness.Query, format string, part retentionBusiness.Part) (retentionBusiness.Written, error) {
	if part.After != "" {
		if _, err := uuid.Parse(part.After); err != nil {
			return retentionBusiness.Written{}, fmt.Errorf("%w: that part isn't one this hold's export offered; export it again from the start", retentionBusiness.ErrBadRequest)
		}
	}
	hq, err := heldBoardQuery(q, part.After)
	if err != nil {
		return retentionBusiness.Written{}, err
	}
	size := min(part.Size, boardExportPart)
	if size <= 0 {
		size = boardExportPart
	}
	// One more than the part holds says whether more remain past it.
	boards, err := heldBoards(ctx, people, hq, size+1, true)
	if err != nil {
		return retentionBusiness.Written{}, err
	}
	var written retentionBusiness.Written
	more := len(boards) > size
	if more {
		boards = boards[:size]
	}
	// The part's drawings, read side by side from object storage before any
	// is written (it has no read of many).
	resolveBoardStates(ctx, boards)
	var body []json.RawMessage
	bytes := 0
	for i, b := range boards {
		one, err := json.Marshal(asHeldBoard(b))
		if err != nil {
			return retentionBusiness.Written{}, err
		}
		body = append(body, one)
		bytes += len(one)
		if bytes > boardExportBytes && i < len(boards)-1 {
			more = true
			boards = boards[:i+1]
			break
		}
	}
	written.N = len(boards)
	if more && written.N > 0 {
		written.Next = boards[written.N-1].Uuid
	}
	file := map[string]any{"part": part.Number, "more": written.Next != "", "boards": body}
	if body == nil {
		file["boards"] = []json.RawMessage{}
	}
	if note := retentionBusiness.PartNote(retentionLabel, part, written); note != "" {
		file["note"] = note
	}
	return written, json.NewEncoder(w).Encode(file)
}
