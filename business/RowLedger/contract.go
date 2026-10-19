// Package business (RowLedger) is the one writer of tables' rows
// (design/tables-vs-airtable/PROGRAM.md §4.2 #62; A2 builds it): every write a
// change set, with who made it and how, each cell's before and after, a version
// per row and a place that needs no renumbering; batches of up to 1,000 rows;
// undo, a trash and record history read from the change sets.
//
// WHERE IT SITS. Below business/DataTable, which keeps a table's rules (who may
// write, what a cell may hold, links, worked-out cells) and writes through the
// ledger. Rows is the way in for a package that writes rows without importing
// business/DataTable (forms, scripts, imports): business/DataTable registers it
// at init, so this package imports no business package and nothing cycles.
//
// DAY 0. Rows writes as today (business/DataTable CreateRow, PatchRow and
// DeleteRow, one row at a time, nothing recorded); Log answers ErrNotBuilt, as
// nothing is recorded to read; Observers hear nothing, as nothing writes
// through the ledger yet. Nothing calls any of them, so nothing changes by
// their existing. A2 fills them behind the same names.
package business

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"time"

	tableAccess "github.com/akashc777/OneCamp/business/TableAccess"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// ActorKind is how a change was made: by a person, or by something acting for
// one. Never taken from what a client sends.
type ActorKind string

const (
	ActorPerson     ActorKind = "person"
	ActorAgent      ActorKind = "agent"
	ActorAutomation ActorKind = "automation"
	ActorImport     ActorKind = "import"
	ActorAPI        ActorKind = "api"
	ActorForm       ActorKind = "form"
	// ActorSystem is the server on its own: a sweep, a repair, the demo seed.
	ActorSystem ActorKind = "system"
)

// ChangeKind is what kind of write a change set is.
type ChangeKind string

const (
	ChangeEdit    ChangeKind = "edit"
	ChangeCreate  ChangeKind = "create"
	ChangeDelete  ChangeKind = "delete"
	ChangeMove    ChangeKind = "move"
	ChangeLinks   ChangeKind = "links"
	ChangePaste   ChangeKind = "paste"
	ChangeFill    ChangeKind = "fill"
	ChangeImport  ChangeKind = "import"
	ChangeAI      ChangeKind = "ai"
	ChangeUndo    ChangeKind = "undo"
	ChangeRedo    ChangeKind = "redo"
	ChangeRestore ChangeKind = "restore"
	// ChangeField is a field's cells rewritten with the field: an option
	// renamed, a type changed.
	ChangeField ChangeKind = "field"
)

// By is who made a change, and how.
type By struct {
	tableAccess.Who
	Kind ActorKind
}

// The Field of a Change that isn't one cell's.
const (
	// WholeRow is a row made, deleted or brought back: Before and After are
	// the whole row (its cells, its place, and for a delete its links).
	WholeRow = ""
	// FieldPosition is a row moved: its place before and after.
	FieldPosition = "#position"
	// LinksPrefix and a relation field's id is that field's links changed: the
	// rows linked and unlinked.
	LinksPrefix = "#links:"
)

// Change is one cell's value before and after a write (JSON as stored; nil
// for none), or a whole row's, a move or a link change (the Field constants).
type Change struct {
	Row    uuid.UUID       `json:"row_id"`
	Field  string          `json:"field"`
	Before json.RawMessage `json:"before,omitempty"`
	After  json.RawMessage `json:"after,omitempty"`
}

// ChangeSet is one write, with every change it made.
type ChangeSet struct {
	ID      uuid.UUID  `json:"id"`
	Table   uuid.UUID  `json:"table_id"`
	By      By         `json:"-"`
	Kind    ChangeKind `json:"kind"`
	At      time.Time  `json:"at"`
	Rows    int        `json:"rows"`
	Fields  []string   `json:"fields,omitempty"`
	Undoes  *uuid.UUID `json:"undoes,omitempty"`
	Changes []Change   `json:"changes,omitempty"`
}

// Position is where new rows go, or moved ones: after one row, or before one;
// with neither, after the table's last row.
type Position struct {
	After  *uuid.UUID `json:"after,omitempty"`
	Before *uuid.UUID `json:"before,omitempty"`
}

// RowInput is a new row's cells, by field id.
type RowInput struct {
	Cells map[string]any `json:"values"`
}

// RowPatch sets the cells it names on one row (nil clears one), and only
// those. Version, when set, is the version the writer read: a row written
// since is refused (A2).
type RowPatch struct {
	Row     uuid.UUID      `json:"row"`
	Cells   map[string]any `json:"values"`
	Version int64          `json:"version,omitempty"`
}

// Writer writes a table's rows for someone, checking what they may do and
// what each cell may hold.
type Writer interface {
	Create(ctx context.Context, by By, table uuid.UUID, rows []RowInput, at Position) ([]*model.Row, error)
	Patch(ctx context.Context, by By, table uuid.UUID, patches []RowPatch) ([]*model.Row, error)
	Delete(ctx context.Context, by By, table uuid.UUID, rows []uuid.UUID) error
}

// Page is a page of a list read newest first: Before is the last one of the
// page before (nil for the first page).
type Page struct {
	Before *uuid.UUID
	Limit  int
}

// HistoryPage is a page of one row's change sets, newest first.
type HistoryPage struct {
	Sets []ChangeSet `json:"items"`
	More bool        `json:"more"`
}

// TrashItem is something in a table's trash: deleted rows, or a deleted field.
type TrashItem struct {
	Kind string    `json:"kind"` // "row" or "field"
	ID   uuid.UUID `json:"id"`
}

// TrashPage is a page of a table's trash, newest first.
type TrashPage struct {
	Items []TrashItem `json:"items"`
	More  bool        `json:"more"`
}

// Ledger reads and reverses what was written.
type Ledger interface {
	History(ctx context.Context, who tableAccess.Who, table, row uuid.UUID, page Page) (*HistoryPage, error)
	// Undo reverses one of who's own change sets: the one named, or their
	// latest on the table.
	Undo(ctx context.Context, by By, table uuid.UUID, changeSet *uuid.UUID) (*ChangeSet, error)
	Redo(ctx context.Context, by By, table uuid.UUID) (*ChangeSet, error)
	Trash(ctx context.Context, who tableAccess.Who, table uuid.UUID, page Page) (*TrashPage, error)
	Restore(ctx context.Context, by By, table uuid.UUID, items []TrashItem) (*ChangeSet, error)
}

var (
	// ErrNotBuilt is something the ledger can't do yet.
	ErrNotBuilt = errors.New("row ledger: that isn't available yet")
	// ErrNotRegistered is a write asked for before business/DataTable
	// registered the writer.
	ErrNotRegistered = errors.New("row ledger: no writer is registered")
)

// Rows is the writer every package outside business/DataTable writes rows
// through. business/DataTable registers it at init.
var Rows Writer = noWriter{}

// Log reads and reverses change sets.
var Log Ledger = notBuilt{}

// noWriter writes nothing: a binary without business/DataTable has no tables.
type noWriter struct{}

func (noWriter) Create(context.Context, By, uuid.UUID, []RowInput, Position) ([]*model.Row, error) {
	return nil, ErrNotRegistered
}

func (noWriter) Patch(context.Context, By, uuid.UUID, []RowPatch) ([]*model.Row, error) {
	return nil, ErrNotRegistered
}

func (noWriter) Delete(context.Context, By, uuid.UUID, []uuid.UUID) error { return ErrNotRegistered }

// notBuilt is today: nothing is recorded, so there is nothing to read or undo.
type notBuilt struct{}

func (notBuilt) History(context.Context, tableAccess.Who, uuid.UUID, uuid.UUID, Page) (*HistoryPage, error) {
	return nil, ErrNotBuilt
}

func (notBuilt) Undo(context.Context, By, uuid.UUID, *uuid.UUID) (*ChangeSet, error) {
	return nil, ErrNotBuilt
}

func (notBuilt) Redo(context.Context, By, uuid.UUID) (*ChangeSet, error) { return nil, ErrNotBuilt }

func (notBuilt) Trash(context.Context, tableAccess.Who, uuid.UUID, Page) (*TrashPage, error) {
	return nil, ErrNotBuilt
}

func (notBuilt) Restore(context.Context, By, uuid.UUID, []TrashItem) (*ChangeSet, error) {
	return nil, ErrNotBuilt
}

// Observer hears every change set the ledger writes: Committing inside its
// transaction (an error undoes the write: A3's cell index, A4's dirty marks),
// Committed once it's stored (A7's live diffs). Either may be nil.
type Observer struct {
	Name       string
	Committing func(ctx context.Context, tx *sql.Tx, cs *ChangeSet) error
	Committed  func(ctx context.Context, cs *ChangeSet)
}

// ObserverRegistry holds the observers.
type ObserverRegistry struct {
	mu   sync.RWMutex
	list []Observer
}

// Observers is the ledger's one registry. A package registers from its init.
var Observers = &ObserverRegistry{}

// Register adds an observer. One with no name, or a name taken, panics: it
// happens in init, where that fails the build's tests.
func (r *ObserverRegistry) Register(o Observer) {
	if o.Name == "" {
		panic("row ledger: an observer needs a name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, have := range r.list {
		if have.Name == o.Name {
			panic("row ledger: the observer " + o.Name + " is registered twice")
		}
	}
	r.list = append(r.list, o)
}

// All is the observers, in the order they registered.
func (r *ObserverRegistry) All() []Observer {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Observer(nil), r.list...)
}

// The ledger's sweeps, which A2 registers with business/Sweep when it has
// work for them: removing what has been in the trash past
// tables.trash.retention_days, and changes past tables.history.retention_days
// (both 0, keep, until the owner decides: PROGRAM.md §13 O3).
const (
	SweepTrashPurge   = "tables.trash.purge"
	SweepHistoryPurge = "tables.history.purge"
)
