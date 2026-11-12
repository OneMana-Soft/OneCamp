package Calendar

// An event's files (gcal-1a head B; Google attaches Drive files): the OneCamp
// docs, boards and tasks an event carries, by reference (migrations/575; a
// task since 591, EVLINK head A). Nothing is uploaded here. A file uploaded in the doc tree (docs F1, POST /doc/files)
// is a doc node, so it is carried as a doc: /entities (business/EntityLink
// readDocs) and CanReadRef read it as one.
//
// WHO (the permission model, MAIN 13:4x 11 Nov). Whoever may change the event
// (mayChange: its organiser, or a guest it lets change it) puts files on and
// takes them off, and puts on only what they may open themselves: nobody
// attaches what they can't see. A file shows on an event only to someone who
// may see both the event and it: each reader's read of the event (the panel,
// /v1 and MCP alike: views) carries only the files they may open, and nothing
// of the rest (no id, no count, no placeholder). Both are one rule,
// entityLinkBusiness.ReadableRefs, read once for every file a read carries.
// A busy view carries none (busyView), nor a guest's page by email
// (InvitationView). The event never names one: each reader's own chip read
// (POST /entities) names it as they may.
//
// HOW. A repeating event's files are its series', on every instance. A file
// taken off is marked, and putting it on again brings the same row back.
// "This and following" carries the series' files onto the new series it
// makes, and an instance that comes back from the trash on its own carries
// them too (filesTo: the same things, put on by the same people at the same
// times); the part that ended keeps its own.
// Each change is announced (event.updated, "changed": "files"), so the work
// graph keeps the meeting's links (graphNodes.go: a doc's backlinks show the
// meeting), the audit log says a file was added or removed (audit.go), and
// the event's readers are told live (MESSAGE_CALENDAR_LIVE). A files change
// never moves the event's sequence, so a form open on it still saves. A new
// event is made with its files (EventInput.Files: Duplicate's); the
// iCalendar export and Google sync leave them out.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	entityLinkBusiness "github.com/akashc777/OneCamp/business/EntityLink"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// FileRef names one OneCamp thing an event carries: a doc (a page, or a file
// uploaded in the doc tree), a board or a task, by its uuid.
type FileRef struct {
	Kind string `json:"kind"`
	UUID string `json:"uuid"`
}

// The kinds of thing an event carries, as /entities names them.
const (
	fileDoc   = entityLinkBusiness.RefDoc
	fileBoard = entityLinkBusiness.RefBoard
	fileTask  = entityLinkBusiness.RefTask
)

// fileKinds are the kinds of thing an event carries.
var fileKinds = []string{fileDoc, fileBoard, fileTask}

// maxEventFiles is how many files one event carries.
const maxEventFiles = 25

var (
	// ErrFileUnreadable is a file its adder may not open: a refusal
	// (EventRefusal), one value, so errors.Is finds it.
	ErrFileUnreadable = refusal("You can add only a doc, a board or a task you can open.")
	// errFilesFull is one file past maxEventFiles.
	errFilesFull = inputError(fmt.Sprintf("An event carries up to %d files. Take one off to add another.", maxEventFiles))
)

// opensRefs is which of refs who may open: the rule that gates every link to
// a doc, a board or a task, and every read of one
// (entityLinkBusiness.ReadableRefs), in one read per kind.
func opensRefs(ctx context.Context, who *model.UserInfo, refs []FileRef) (map[FileRef]bool, error) {
	asked := make([]entityLinkBusiness.Ref, 0, len(refs))
	for _, r := range refs {
		asked = append(asked, entityLinkBusiness.Ref{Kind: r.Kind, ID: r.UUID})
	}
	open, err := entityLinkBusiness.ReadableRefs(ctx, accessBusiness.Subject{UserUID: who.UserDgraphInfo.Uid, UserUUID: who.UserDgraphInfo.Uuid}, asked)
	if err != nil {
		return nil, err
	}
	out := make(map[FileRef]bool, len(open))
	for r := range open {
		out[FileRef{Kind: r.Kind, UUID: r.ID}] = true
	}
	return out, nil
}

// fileRef is ref as an event keeps it: a doc, a board or a task, by its uuid
// in lower case; else why not, in words. Pure.
func fileRef(ref FileRef) (FileRef, error) {
	kind := strings.ToLower(strings.TrimSpace(ref.Kind))
	if !slices.Contains(fileKinds, kind) {
		return FileRef{}, inputError("A file on an event is a doc, a board or a task.")
	}
	id := strings.ToLower(strings.TrimSpace(ref.UUID))
	if !helpers.IsCanonicalUUID(id) {
		return FileRef{}, inputError("That file's id isn't one OneCamp gave.")
	}
	return FileRef{Kind: kind, UUID: id}, nil
}

// fileRefsOf is files as an event keeps them: each one checked (fileRef),
// once, in the order given, at most maxEventFiles. Pure.
func fileRefsOf(in []FileRef) ([]FileRef, error) {
	out := make([]FileRef, 0, len(in))
	for _, r := range in {
		f, err := fileRef(r)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	if len(out) > maxEventFiles {
		return nil, errFilesFull
	}
	return out, nil
}

// mayOpenAll is nil when who may open every one of refs, and
// ErrFileUnreadable when there is one they can't.
func (s *service) mayOpenAll(ctx context.Context, who *model.UserInfo, refs []FileRef) error {
	if len(refs) == 0 {
		return nil
	}
	open, err := s.opens(ctx, who, refs)
	if err != nil {
		return err
	}
	for _, r := range refs {
		if !open[r] {
			return ErrFileUnreadable
		}
	}
	return nil
}

// fileRows are refs (checked: fileRef) as rows of event, put on by who at at.
func fileRows(event uuid.UUID, refs []FileRef, who uuid.UUID, at time.Time) []models.EventFile {
	out := make([]models.EventFile, 0, len(refs))
	for _, r := range refs {
		out = append(out, models.EventFile{EventID: event, Kind: r.Kind, Ref: uuid.MustParse(r.UUID), AddedBy: who, AddedAt: at})
	}
	return out
}

// filesTo is files (a series' rows) as rows of event: the same things, put
// on by the same people at the same times. Pure.
func filesTo(files []models.EventFile, event uuid.UUID) []models.EventFile {
	out := make([]models.EventFile, 0, len(files))
	for _, f := range files {
		f.EventID = event
		out = append(out, f)
	}
	return out
}

// carriedFiles is the live files of series as rows of event, for a batch
// that makes event from it ("this and following", or an instance back on its
// own). A failed read fails the batch: the files are never dropped quietly.
func (s *service) carriedFiles(ctx context.Context, series, event uuid.UUID) ([]models.EventFile, error) {
	got, err := s.store.filesOf(ctx, []uuid.UUID{series})
	if err != nil {
		return nil, err
	}
	return filesTo(got[series], event), nil
}

// filesOf is the files of each of rows' events that reader may open, by its
// owner's id (an instance's are its series'), in one read of the files and
// one of what reader may open (the permission model: nothing of a file they
// can't). A failed read gives none, and the events still answer.
func (s *service) filesOf(ctx context.Context, reader *model.UserInfo, rows []instance) map[uuid.UUID][]FileRef {
	ids := make([]uuid.UUID, 0, len(rows))
	for _, i := range rows {
		if id := i.owner().ID; !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	got, err := s.store.filesOf(ctx, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar: the files of %d events couldn't be read: %v", len(ids), err)
		return nil
	}
	var asked []FileRef
	for _, files := range got {
		for _, f := range files {
			if r := (FileRef{Kind: f.Kind, UUID: f.Ref.String()}); !slices.Contains(asked, r) {
				asked = append(asked, r)
			}
		}
	}
	if len(asked) == 0 {
		return nil
	}
	open, err := s.opens(ctx, reader, asked)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar: which of %d files its reader may open couldn't be read: %v", len(asked), err)
		return nil
	}
	out := make(map[uuid.UUID][]FileRef, len(got))
	for id, files := range got {
		for _, f := range files {
			if r := (FileRef{Kind: f.Kind, UUID: f.Ref.String()}); open[r] {
				out[id] = append(out[id], r)
			}
		}
	}
	return out
}

// AddFile puts a doc, a board or a task on the event id names (a series, from any of
// its instances), for whoever may change it and may open the thing: one
// already on it stays as it is, and one taken off comes back. It answers the
// occurrence asked for, as who reads it now.
func (s *service) AddFile(ctx context.Context, who *model.UserInfo, id string, ref FileRef) (*Event, error) {
	return s.changeFile(ctx, who, id, ref, true)
}

// RemoveFile takes a doc, a board or a task off the event id names, for whoever may
// change it; one that isn't on it leaves it as it is.
func (s *service) RemoveFile(ctx context.Context, who *model.UserInfo, id string, ref FileRef) (*Event, error) {
	return s.changeFile(ctx, who, id, ref, false)
}

// changeFile is both: the change rule, then the thing (and, to put it on,
// whether who may open it), then the write, announced when it changed the
// event's files. Never a title: the announcement names the thing's kind and
// id alone.
func (s *service) changeFile(ctx context.Context, who *model.UserInfo, id string, ref FileRef, add bool) (*Event, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	inst, guests, err := s.resolve(ctx, who, id)
	if err != nil {
		return nil, err
	}
	e, me := inst.owner(), personOf(who)
	if err := mayChange(roleOf(e, guests, me), e); err != nil {
		return nil, err
	}
	f, err := fileRef(ref)
	if err != nil {
		return nil, err
	}
	var changed bool
	what := "removed"
	if add {
		what = "added"
		if err := s.mayOpenAll(ctx, who, []FileRef{f}); err != nil {
			return nil, err
		}
		changed, err = s.store.addFile(ctx, fileRows(e.ID, []FileRef{f}, me, s.now())[0], maxEventFiles)
		if errors.Is(err, models.ErrFilesFull) {
			return nil, errFilesFull
		}
	} else {
		changed, err = s.store.removeFile(ctx, e.ID, f.Kind, uuid.MustParse(f.UUID), me, s.now())
	}
	if err != nil {
		return nil, err
	}
	if changed {
		s.announce(ctx, who, resourceEventBusiness.CalendarEventUpdated, e, guestIDs(guests),
			map[string]string{"changed": "files", "file": what, "file_kind": f.Kind, "file_uuid": f.UUID})
	}
	return s.view(ctx, who, inst, guests), nil
}
