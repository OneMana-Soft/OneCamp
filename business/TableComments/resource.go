// Package business (TableComments) is comments on a table's rows (tables
// programme A8, design/tables-vs-airtable/PROGRAM.md §6; rows CO-1, CO-2):
// anchored threads (business/Thread, docs A8) on a row as a whole or on one of
// its cells, their comments kept in Postgres (Thread's StoredComments).
//
// Who may read, comment, resolve and moderate is the row's decision, the
// Access Resolver's "table_row" (business/DataTable rowAccess.go): its table's
// rule today, Table Access (#66) once A5 lands. A row in the trash takes its
// threads with it, and a restore brings them back; nothing is purged (owner
// questions O1 and O3).
//
// Around the threads: open tabs hear of every change on the table's live topic
// (live.go), the people mentioned and the thread's people are told (notify.go),
// and the inbox names each notice for its reader (notices.go). Threads' own
// routes serve it all (router/docsThreads.go); this package registers itself
// from its init, and router/tablesComments.go imports it.
package business

import (
	"context"
	"errors"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	tableAccess "github.com/akashc777/OneCamp/business/TableAccess"
	threadBusiness "github.com/akashc777/OneCamp/business/Thread"
	"github.com/google/uuid"
)

// Kind is a row as threads, the Access Resolver and notices name it; a row's
// threads have its id as their resource's uuid.
const Kind = tableAccess.KindTableRow

func init() {
	threadBusiness.Resources.Register(Kind, rowThreads{threadBusiness.StoredComments{Kind: Kind}})
}

// rowThreads is threads on a table's rows.
type rowThreads struct {
	threadBusiness.StoredComments
}

// Anchors: the row as a whole, or one of its cells ({"field": "<field id>"}).
func (rowThreads) Anchors() []threadBusiness.AnchorKind {
	return []threadBusiness.AnchorKind{threadBusiness.AnchorNone, threadBusiness.AnchorCell}
}

// Access is who's decision on the row. A row id not written as the server
// writes it (lower case, hyphenated) names no row: a thread is kept under the
// id it was made with, so two spellings of one id would split its threads.
func (rowThreads) Access(ctx context.Context, who accessBusiness.Subject, res accessBusiness.Resource) (accessBusiness.Decision, error) {
	if !canonical(res.UUID) {
		return accessBusiness.Decision{}, threadBusiness.ErrNotFound
	}
	d, err := accessBusiness.Rules.Resolve(ctx, who, res)
	if errors.Is(err, accessBusiness.ErrNotFound) {
		return d, threadBusiness.ErrNotFound
	}
	return d, err
}

// Post stores a comment once the threads service has decided who may write
// it here.
func (r rowThreads) Post(ctx context.Context, who accessBusiness.Subject, res accessBusiness.Resource, threadID string, body threadBusiness.Body) (threadBusiness.Posted, error) {
	return r.Write(ctx, who, res, threadID, body)
}

// canonical is whether id is a uuid as the server writes one. Pure.
func canonical(id string) bool {
	u, err := uuid.Parse(id)
	return err == nil && u.String() == id
}
