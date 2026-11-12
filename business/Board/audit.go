package business

// Boards in the admin audit log (miro-44): who a board was shared with, every
// export of it, and every restore from the trash, one row each under
// "boards", in the hash chain an admin reads on every plan. Taking a place on a board away, or lowering it, is
// recorded before it happens (UpdateBoardPermissions, RecordedTaking), and an
// external link's making and turning off by the guest-link routes.
//
// The events carry the board's title and whether it is private, so the row is
// written without reading the board again. A private board is never named:
// admins can't open it, so its row says "a private board" and carries its id;
// a board whose privacy isn't known is treated as private.

import (
	"context"
	"errors"
	"fmt"
	labelBusiness "github.com/akashc777/OneCamp/business/Label"
	"strconv"
	"strings"
	"time"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	domain "github.com/akashc777/OneCamp/domain/Board"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// KindBoard is a board, as a resource event names it.
const KindBoard = "board"

// boardAuditedKinds are the board's events the log records, each with how its
// summary reads.
var boardAuditedKinds = map[resourceEventBusiness.Kind]string{
	resourceEventBusiness.BoardShared:   "Shared %s",
	resourceEventBusiness.BoardExported: "Exported %s",
	resourceEventBusiness.BoardRestored: "Restored %s from Deleted boards",
}

// boardsAudit is the boards' events in the admin audit log.
var boardsAudit = auditBusiness.EventSink{
	Name:     "board.audit",
	Category: auditBusiness.CategoryBoards,
	Wants:    boardAudited,
	Row: func(_ context.Context, e resourceEventBusiness.Event) (string, map[string]interface{}) {
		return boardAuditRow(e)
	},
}

func init() { auditBusiness.RegisterEventSink(boardsAudit) }

func boardAudited(e resourceEventBusiness.Event) bool {
	_, ok := boardAuditedKinds[e.Kind]
	return ok && e.ResourceKind == KindBoard
}

// boardAuditRow is an event's summary and metadata. Pure.
func boardAuditRow(e resourceEventBusiness.Event) (string, map[string]interface{}) {
	private := e.Data["private"] != "false"
	name := "a private board"
	if !private {
		name = "a board"
		if title := strings.TrimSpace(e.Data["title"]); title != "" {
			name = "‘" + title + "’"
		}
	}
	summary := fmt.Sprintf(boardAuditedKinds[e.Kind], name)
	switch e.Kind {
	case resourceEventBusiness.BoardShared:
		if len(e.Subjects) > 0 {
			summary += " with " + auditBusiness.PeopleWho(len(e.Subjects), e.Data["level"])
		}
	case resourceEventBusiness.BoardExported:
		if format := e.Data["format"]; format != "" {
			words, ok := boardExportFormats[format]
			if !ok {
				words = format
			}
			summary += " as " + words
		}
		if e.Data["via"] == "browser" {
			summary += ", in the browser"
		}
	}
	return summary, auditBusiness.EventMeta(e, private)
}

// boardEventData is what every board event carries for its row: the title,
// and whether the board is private, so the row never names a private one.
func boardEventData(board *dgraphStruct.DgraphBoard) map[string]string {
	return map[string]string{"title": board.Title, "private": strconv.FormatBool(board.IsPrivate != nil && *board.IsPrivate)}
}

// boardActor is who made a board change, from the request it came with.
func boardActor(ctx context.Context) resourceEventBusiness.Actor {
	if p := auditBusiness.ContextPrincipal(ctx, ""); p.ID != nil {
		return resourceEventBusiness.Actor{UserUUID: p.ID.String()}
	}
	return resourceEventBusiness.Actor{System: true}
}

// emitBoardShared tells a change's grants and raises, once it is made: one
// board.shared per level given, naming the people given it.
func emitBoardShared(ctx context.Context, board *dgraphStruct.DgraphBoard, shifts []auditBusiness.AccessShift) {
	people := map[string][]string{}
	var levels []string
	for _, s := range auditBusiness.Given(shifts) {
		if _, seen := people[s.To]; !seen {
			levels = append(levels, s.To)
		}
		people[s.To] = append(people[s.To], s.UserUUID)
	}
	actor, now := boardActor(ctx), time.Now()
	for _, level := range levels {
		data := boardEventData(board)
		data["level"] = level
		resourceEventBusiness.Bus.Emit(ctx, resourceEventBusiness.Event{Kind: resourceEventBusiness.BoardShared, ResourceKind: KindBoard,
			ResourceUUID: board.Uuid, Subjects: people[level], Actor: actor, At: now, Data: data})
	}
}

// boardExportFormats are the files the canvas makes in the browser (web
// components/board/boardExport.tsx, its Format), each in the words its row
// says it with, the words the route's 400 lists them in.
var boardExportFormats = map[string]string{"pdf": "a PDF", "picture": "a picture", "csv": "a CSV of its notes", "file": "a board file"}

var (
	// ErrBoardNotFound is a board that isn't there for this person: no board
	// has that uuid, or it is deleted, or private and not shared with them.
	// The answer never tells these apart. It is helpers.ErrNotFound too, and
	// its route answers 404.
	ErrBoardNotFound = helpers.NotFound("board not found")
	// ErrBoardExportFormat is a format the canvas doesn't make.
	ErrBoardExportFormat = errors.New("not a format a board exports to")
)

// exportBoardRead is the board as the person reads it; a seam for tests.
var exportBoardRead = domain.GetBasicDgraphBoardByUUID

// RecordBoardExport tells the log that a person made an export of a board in
// their browser, where the canvas makes its files. Only a board they can read
// (CanRead: not deleted; public, or theirs, or shared with them) is told; any
// other answers ErrBoardNotFound and tells nothing, so the log never claims an
// export of a board someone can't open.
func RecordBoardExport(ctx context.Context, boardUUID, format, userUID, userUUID string) error {
	if _, ok := boardExportFormats[format]; !ok {
		return ErrBoardExportFormat
	}
	board, err := exportBoardRead(ctx, boardUUID, userUID)
	if errors.Is(err, helpers.ErrNotFound) {
		return ErrBoardNotFound
	}
	if err != nil {
		return err
	}
	if !CanRead(board, userUUID) {
		return ErrBoardNotFound
	}
	// A label with no_export refuses it before the browser makes the file
	// (the web awaits this tell first); the refusal is a row of its own.
	if err := labelBusiness.Allows(ctx, KindBoard, board.Uuid, labelBusiness.RuleNoExport); err != nil {
		return err
	}
	data := boardEventData(board)
	data["format"], data["via"] = format, "browser"
	resourceEventBusiness.Bus.Emit(ctx, resourceEventBusiness.Event{Kind: resourceEventBusiness.BoardExported, ResourceKind: KindBoard,
		ResourceUUID: board.Uuid, Actor: boardActor(ctx), At: time.Now(), Data: data})
	return nil
}
