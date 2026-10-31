package business

// The work graph's rows and tables (DG14, with DGRAPH-LEAD-3): a row that
// links work (a relation's links, a people field's people, its page's
// references) is a thin node of the graph, kept by the graph's projector. The
// tables tell it which rows and tables changed, in the change's own
// transaction, through the existing outbox, so a row write never loses its
// graph event. The projector reads each row and keeps a node only while it
// links something; the reconciler repairs what an event missed
// (RowsLinkingAfter).
//
// One row-ledger observer hears every write the ledger makes (the grid, the
// record, the API, MCP, AI tools, forms, undo and redo, imports, links). The
// writes outside it (a relation field taken away, a table's trash and
// restore, a row's page saved, T-O3's purge) send the same entry once the
// graph's side (DG12) is in; until then the reconciler finds them.

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"time"

	rowLedger "github.com/akashc777/OneCamp/business/RowLedger"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	outboxModels "github.com/akashc777/OneCamp/models/postgres/Outbox"
	"github.com/google/uuid"
)

// GraphRow is a row, as the work graph's entries name its kind.
const GraphRow = "row"

// GraphNodes is how the tables tell the work graph what changed: the work
// graph's NodesInTx and NodesCommitted (business/Refs), set from its init,
// as RowQuery's WorkedOut is set from here. Unset, the tables tell nothing.
//
// InTx adds held entries inside tx for kind's ids (a row's table as scope,
// or none), as many as its entries' size takes (200 ids an entry), in one
// statement, and answers their ids; Committed releases and delivers them,
// once tx has committed.
var GraphNodes struct {
	InTx      func(ctx context.Context, tx outboxModels.Execer, kind, scope string, ids []string) ([]int64, error)
	Committed func(ids []int64)
}

func init() {
	rowLedger.Observers.Register(rowLedger.Observer{Name: "tables.graph", Committing: graphInLedger, Committed: graphLedgerCommitted})
}

// linksWork is whether a field's cells link work: a relation's links (rows,
// docs, tasks, boards, projects, people) or a people field's people.
func linksWork(f *model.Field) bool {
	return f != nil && f.DeletedAt == nil && (f.Type == model.FieldRelation || f.Type == model.FieldPeople)
}

// graphRowsOf is the rows a change set tells the graph about: rows made,
// deleted or brought back, rows whose links changed, and rows whose cells in
// a field that links work changed; each once, in the order they came. Pure.
func graphRowsOf(cs *rowLedger.ChangeSet, links func(field string) bool) []uuid.UUID {
	seen := map[uuid.UUID]bool{}
	var out []uuid.UUID
	for _, ch := range cs.Changes {
		told := ch.Field == rowLedger.WholeRow || strings.HasPrefix(ch.Field, rowLedger.LinksPrefix) ||
			(ch.Field != rowLedger.FieldPosition && !strings.HasPrefix(ch.Field, "#") && links(ch.Field))
		if told && !seen[ch.Row] {
			seen[ch.Row] = true
			out = append(out, ch.Row)
		}
	}
	return out
}

// heldGraph is the entries a change set added, by its id, until it commits:
// the observer's Committing can't hand them to its Committed. A change set
// whose write rolled back took its entries with it, and is let go here after
// a minute.
var heldGraph = struct {
	sync.Mutex
	by map[uuid.UUID]heldEntries
}{by: map[uuid.UUID]heldEntries{}}

type heldEntries struct {
	ids []int64
	at  time.Time
}

const heldGraphFor = time.Minute

func holdGraph(cs uuid.UUID, ids []int64, now time.Time) {
	heldGraph.Lock()
	defer heldGraph.Unlock()
	for id, h := range heldGraph.by {
		if now.Sub(h.at) > heldGraphFor {
			delete(heldGraph.by, id)
		}
	}
	heldGraph.by[cs] = heldEntries{ids: ids, at: now}
}

func releaseGraph(cs uuid.UUID) []int64 {
	heldGraph.Lock()
	defer heldGraph.Unlock()
	h := heldGraph.by[cs]
	delete(heldGraph.by, cs)
	return h.ids
}

// graphInLedger is the observer's Committing: the change set's rows, told
// inside its transaction.
func graphInLedger(ctx context.Context, tx *sql.Tx, cs *rowLedger.ChangeSet) error {
	if GraphNodes.InTx == nil || len(cs.Changes) == 0 {
		return nil
	}
	fields, err := model.LiveFields(pgexec.WithTx(ctx, tx), cs.Table)
	if err != nil {
		return err
	}
	byID := make(map[string]*model.Field, len(fields))
	for _, f := range fields {
		byID[f.Id.String()] = f
	}
	rows := graphRowsOf(cs, func(field string) bool { return linksWork(byID[field]) })
	if len(rows) == 0 {
		return nil
	}
	ids, err := GraphNodes.InTx(ctx, tx, GraphRow, cs.Table.String(), helpers.UUIDStrings(rows))
	if err != nil {
		return err
	}
	holdGraph(cs.ID, ids, time.Now())
	return nil
}

// graphLedgerCommitted is the observer's Committed: what the change set
// told, delivered.
func graphLedgerCommitted(_ context.Context, cs *rowLedger.ChangeSet) {
	if ids := releaseGraph(cs.ID); len(ids) > 0 {
		GraphNodes.Committed(ids)
	}
}
