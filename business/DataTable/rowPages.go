package business

// Rows as pages (tables T2; docs D-18, amended): a row can be opened as a
// page, a doc of its own under its fields, which anyone who may edit the row
// writes in and anyone who may open the row reads. The doc is hosted by the
// row: its level is the row's (business/Access asks the "table_row" kind,
// rowAccess.go), capped at edit, so it is shared, moved and trashed only with
// the row; it is in no docs list and no docs index.
//
// It is made the first time someone writes in it (EnsureRowPageFor), one a
// row however many ask at once: the row is held while the doc is made, and
// the doc's make is one a host in the graph too. The row's side of the link is
// data_table_rows.doc_uuid (migration 307).
//
// Who may open a table's pages changes with the table and its rows: a table
// made private or put in the trash, a row put in the trash. Then the pages'
// live sockets are closed, so each reconnects and is authorised afresh by its
// row (docBusiness.HostsChanged, I10).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	rowLedger "github.com/akashc777/OneCamp/business/RowLedger"
	tableAccess "github.com/akashc777/OneCamp/business/TableAccess"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

func init() {
	docBusiness.RegisterHostKind(tableAccess.KindTableRow, docBusiness.HostKind{Place: rowPagePlace})
	rowLedger.Observers.Register(rowLedger.Observer{Name: "tables.row_pages", Committed: rowPagesLeft})
}

// Seams for tests: the reads and writes a row's page makes.
var (
	pagesOfRows     = model.PagesOfRows
	pagesOfTable    = model.PagesOfTable
	ensureRowPage   = model.EnsureRowPage
	ensureHostedDoc = docBusiness.EnsureHostedDoc
	pageDocOf       = docBusiness.HostedDocOf
	hostsChanged    = docBusiness.HostsChanged
	rowTitlesOf     = RowTitles
	publishPage     = mqttBusiness.PublishTableRow
	decideRow       = func(ctx context.Context, who accessBusiness.Subject, row uuid.UUID) (accessBusiness.Decision, error) {
		return accessBusiness.Rules.Resolve(ctx, who, accessBusiness.Resource{Kind: tableAccess.KindTableRow, UUID: row.String()})
	}
)

// errPageReadOnly is someone who may open a row but not write in it, asking
// to make its page.
var errPageReadOnly = errors.New("You can read this row's page, but not write in it.")

// IsPageReadOnly is errPageReadOnly, for the controller.
func IsPageReadOnly(err error) bool { return errors.Is(err, errPageReadOnly) }

// rowPath is the address of a row's record in its table.
func rowPath(table, row uuid.UUID) string {
	return "/app/tables/" + table.String() + "?row=" + row.String()
}

// rowPagePlace is where a row's page is, as a docs path that refuses the doc
// says it to someone who may open the row (docBusiness.HostPlace).
func rowPagePlace(ctx context.Context, hostUUID string) (docBusiness.HostPlace, error) {
	row, err := uuid.Parse(hostUUID)
	if err != nil {
		return docBusiness.HostPlace{}, docBusiness.ErrNoHost
	}
	titles, err := rowTitlesOf(ctx, []uuid.UUID{row})
	if err != nil {
		return docBusiness.HostPlace{}, err
	}
	t, ok := titles[row]
	if !ok {
		return docBusiness.HostPlace{}, docBusiness.ErrNoHost
	}
	return docBusiness.HostPlace{
		Belongs: fmt.Sprintf("This page belongs to a row of %s. Change it there.", t.TableName),
		Path:    rowPath(t.Table, row),
	}, nil
}

// RowPage is a row's page as its record shows it: the doc's id and its saved
// body, which the editor shows until its live copy syncs (none when the row
// has no page yet), and what the person may do with it, "view" or "edit"
// (write in it, and make it).
type RowPage struct {
	Doc   string `json:"doc_uuid,omitempty"`
	Body  string `json:"body,omitempty"`
	Level string `json:"level"`
}

// rowFor decides a row in a table for who: the row's own table decides, and a
// row that isn't in the table named (or is in the trash, or not theirs to
// open) is errRowNotFound, as a row that isn't there.
func rowFor(ctx context.Context, who accessBusiness.Subject, table, row uuid.UUID) (accessBusiness.Decision, error) {
	d, err := decideRow(ctx, who, row)
	if errors.Is(err, accessBusiness.ErrNotFound) {
		return d, errRowNotFound
	}
	if err != nil {
		return d, err
	}
	if d.Deleted || d.Level < accessBusiness.LevelView {
		return d, errRowNotFound
	}
	homes, err := rowHomes(ctx, []uuid.UUID{row})
	if err != nil {
		return d, err
	}
	if h, ok := homes[row]; !ok || h.Table != table {
		return d, errRowNotFound
	}
	return d, nil
}

// pageLevel is a row decision as its page's level: the row's, never above edit.
func pageLevel(d accessBusiness.Decision) accessBusiness.Level {
	return min(d.Level, accessBusiness.LevelEdit)
}

// RowPageFor is a row's page for someone who may open the row.
func RowPageFor(ctx context.Context, who accessBusiness.Subject, table, row uuid.UUID) (*RowPage, error) {
	d, err := rowFor(ctx, who, table, row)
	if err != nil {
		return nil, err
	}
	out := &RowPage{Level: pageLevel(d).String()}
	pages, err := pagesOfRows(ctx, []uuid.UUID{row})
	if err != nil {
		return nil, err
	}
	page, ok := pages[row]
	if !ok {
		return out, nil
	}
	// Only a doc this row hosts is read: a link to any other is never followed.
	doc, err := pageDocOf(ctx, docBusiness.Host{Kind: tableAccess.KindTableRow, UUID: row.String()}, page.String())
	if errors.Is(err, docBusiness.ErrNotFound) {
		// Its doc isn't the row's, or went from the graph (a restore of an
		// older one): the row shows no page.
		helpers.LogErrorWithContext(ctx, "business/RowPageFor %s: %s isn't its page in the graph", row, page)
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	out.Doc, out.Body = strings.ToLower(doc.Uuid), doc.Body
	return out, nil
}

// EnsureRowPageFor is a row's page, made now when it has none, for someone
// who may edit the row; made says whether this call made it. The row's
// open views hear of a page made (a live "page" message), so its mark shows.
func EnsureRowPageFor(ctx context.Context, who accessBusiness.Subject, table, row uuid.UUID) (doc string, made bool, err error) {
	d, err := rowFor(ctx, who, table, row)
	if err != nil {
		return "", false, err
	}
	if pageLevel(d) < accessBusiness.LevelEdit {
		return "", false, errPageReadOnly
	}
	title := "Untitled"
	if titles, err := rowTitlesOf(ctx, []uuid.UUID{row}); err == nil {
		if t, ok := titles[row]; ok && t.Title != "" {
			title = t.Title
		}
	}
	page, made, err := ensureRowPage(ctx, table, row, func(ctx context.Context) (uuid.UUID, error) {
		id, _, err := ensureHostedDoc(ctx, docBusiness.Host{Kind: tableAccess.KindTableRow, UUID: row.String()}, title)
		if err != nil {
			return uuid.Nil, err
		}
		return uuid.Parse(id)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, errRowNotFound
	}
	if err != nil {
		return "", false, err
	}
	if made {
		publishPage(table.String(), map[string]interface{}{"action": "page", "row_id": row.String(), "page": page.String()})
	}
	return page.String(), made, nil
}

// WithPages sets each row's page, for the member's grid and record: one read
// for the lot. A failed read is logged, and the rows go out without their
// pages, as before migration 307.
func WithPages(ctx context.Context, rows []*model.Row) {
	if len(rows) == 0 {
		return
	}
	ids := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		if r != nil {
			ids = append(ids, r.Id)
		}
	}
	pages, err := pagesOfRows(ctx, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/WithPages: rows go out without their pages: %+v", err)
		return
	}
	for _, r := range rows {
		if r == nil {
			continue
		}
		if p, ok := pages[r.Id]; ok {
			page := p
			r.Page = &page
		}
	}
}

// tablePagesChanged tells docs that who may open a table's pages changed (the
// table made private, or put in the trash).
func tablePagesChanged(ctx context.Context, table uuid.UUID) {
	pages, err := pagesOfTable(ctx, table)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/tablePagesChanged %s: its pages' sockets stay until they reconnect: %+v", table, err)
		return
	}
	hostsChanged(uuidStrings(pages))
}

// rowPagesLeft is the ledger's observer: a change set that put rows in the
// trash closes their pages' live sockets. Rows brought back need nothing:
// whoever opens them is authorised then.
func rowPagesLeft(ctx context.Context, cs *rowLedger.ChangeSet) {
	if cs == nil {
		return
	}
	var gone []uuid.UUID
	for _, c := range cs.Changes {
		if (model.Change{Row: c.Row, Field: c.Field, Before: c.Before, After: c.After}).RowEvent() == model.RowDeleted {
			gone = append(gone, c.Row)
		}
	}
	if len(gone) == 0 {
		return
	}
	pages, err := pagesOfRows(ctx, gone)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/rowPagesLeft: deleted rows' pages keep their sockets until they reconnect: %+v", err)
		return
	}
	ids := make([]uuid.UUID, 0, len(pages))
	for _, p := range pages {
		ids = append(ids, p)
	}
	hostsChanged(uuidStrings(ids))
}

func uuidStrings(ids []uuid.UUID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	return out
}
