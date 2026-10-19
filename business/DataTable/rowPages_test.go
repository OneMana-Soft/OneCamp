package business

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	rowLedger "github.com/akashc777/OneCamp/business/RowLedger"
	tableAccess "github.com/akashc777/OneCamp/business/TableAccess"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// pageWorld is one table with one row, Acme, and the seams a row's page
// reads and writes through.
type pageWorld struct {
	table, row, other uuid.UUID
	level             accessBusiness.Level
	deleted           bool
	pages             map[uuid.UUID]uuid.UUID
	docs              map[string]*dgraphStruct.DgraphDoc
	made              int
	told              []map[string]interface{}
	changed           [][]string
}

func newPageWorld(t *testing.T) *pageWorld {
	t.Helper()
	w := &pageWorld{table: uuid.New(), row: uuid.New(), other: uuid.New(), level: accessBusiness.LevelEdit,
		pages: map[uuid.UUID]uuid.UUID{}, docs: map[string]*dgraphStruct.DgraphDoc{}}
	oldDecide, oldHomes, oldTitles := decideRow, rowHomes, rowTitlesOf
	oldPages, oldTablePages, oldEnsure, oldHosted := pagesOfRows, pagesOfTable, ensureRowPage, ensureHostedDoc
	oldDoc, oldChanged, oldPublish := pageDocOf, hostsChanged, publishPage
	t.Cleanup(func() {
		decideRow, rowHomes, rowTitlesOf = oldDecide, oldHomes, oldTitles
		pagesOfRows, pagesOfTable, ensureRowPage, ensureHostedDoc = oldPages, oldTablePages, oldEnsure, oldHosted
		pageDocOf, hostsChanged, publishPage = oldDoc, oldChanged, oldPublish
	})
	decideRow = func(_ context.Context, _ accessBusiness.Subject, row uuid.UUID) (accessBusiness.Decision, error) {
		if row != w.row {
			return accessBusiness.Decision{}, accessBusiness.ErrNotFound
		}
		d := accessBusiness.Decision{Level: w.level, Was: w.level}
		if w.deleted {
			d.Level, d.Deleted = accessBusiness.LevelNone, true
		}
		return d, nil
	}
	rowHomes = func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]model.RowHome, error) {
		return map[uuid.UUID]model.RowHome{w.row: {Table: w.table, Deleted: w.deleted}}, nil
	}
	rowTitlesOf = func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]RowTitle, error) {
		return map[uuid.UUID]RowTitle{w.row: {Table: w.table, TableName: "Deals", Title: "Acme"}}, nil
	}
	pagesOfRows = func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]uuid.UUID, error) {
		out := map[uuid.UUID]uuid.UUID{}
		for _, id := range ids {
			if p, ok := w.pages[id]; ok {
				out[id] = p
			}
		}
		return out, nil
	}
	ensureRowPage = func(ctx context.Context, table, row uuid.UUID, makePage func(context.Context) (uuid.UUID, error)) (uuid.UUID, bool, error) {
		if table != w.table || row != w.row || w.deleted {
			return uuid.Nil, false, sql.ErrNoRows
		}
		if p, ok := w.pages[row]; ok {
			return p, false, nil
		}
		p, err := makePage(ctx)
		if err != nil {
			return uuid.Nil, false, err
		}
		w.pages[row] = p
		return p, true, nil
	}
	ensureHostedDoc = func(_ context.Context, h docBusiness.Host, title string) (string, bool, error) {
		if h.Kind != tableAccess.KindTableRow || h.UUID != w.row.String() || title != "Acme" {
			t.Errorf("the row's page was made for %+v titled %q", h, title)
		}
		w.made++
		id := uuid.NewString()
		w.docs[id] = &dgraphStruct.DgraphDoc{Uuid: id, HostKind: h.Kind, HostUUID: h.UUID, Body: "<p>Call on Friday.</p>"}
		return id, true, nil
	}
	pageDocOf = func(_ context.Context, h docBusiness.Host, id string) (*dgraphStruct.DgraphDoc, error) {
		// As docBusiness.HostedDocOf: only a doc the host hosts.
		if d, ok := w.docs[id]; ok && d.HostKind == h.Kind && d.HostUUID == h.UUID {
			return d, nil
		}
		return nil, docBusiness.ErrNotFound
	}
	publishPage = func(_ string, msg interface{}) { w.told = append(w.told, msg.(map[string]interface{})) }
	hostsChanged = func(ids []string) { w.changed = append(w.changed, ids) }
	return w
}

// A row's page is made the first time someone who may edit the row asks,
// once however often they ask, and the row's open views hear of it once.
// Someone who may only read the row can't make it; a row in another table,
// in the trash, or not theirs answers as a row that isn't there.
func TestARowsPageIsMadeOnceBySomeoneWhoMayEditIt(t *testing.T) {
	w := newPageWorld(t)
	bg := context.Background()
	doc, made, err := EnsureRowPageFor(bg, accessBusiness.Subject{UserUUID: "u"}, w.table, w.row)
	if err != nil || !made || doc == "" || w.made != 1 {
		t.Fatalf("the first ask: %q %v %v (%d made)", doc, made, err, w.made)
	}
	again, made, err := EnsureRowPageFor(bg, accessBusiness.Subject{UserUUID: "u"}, w.table, w.row)
	if err != nil || made || again != doc || w.made != 1 {
		t.Fatalf("the second ask: %q %v %v (%d made)", again, made, err, w.made)
	}
	if len(w.told) != 1 || w.told[0]["action"] != "page" || w.told[0]["row_id"] != w.row.String() || w.told[0]["page"] != doc {
		t.Errorf("the open views heard %v", w.told)
	}

	w.level = accessBusiness.LevelView
	if _, _, err := EnsureRowPageFor(bg, accessBusiness.Subject{UserUUID: "u"}, w.table, w.row); !IsPageReadOnly(err) {
		t.Errorf("a reader made the page: %v", err)
	}
	w.level = accessBusiness.LevelEdit
	if _, _, err := EnsureRowPageFor(bg, accessBusiness.Subject{UserUUID: "u"}, w.other, w.row); !IsRowNotFound(err) {
		t.Errorf("the row through another table's address: %v", err)
	}
	if _, _, err := EnsureRowPageFor(bg, accessBusiness.Subject{UserUUID: "u"}, w.table, w.other); !IsRowNotFound(err) {
		t.Errorf("a row that isn't there: %v", err)
	}
	w.deleted = true
	if _, _, err := EnsureRowPageFor(bg, accessBusiness.Subject{UserUUID: "u"}, w.table, w.row); !IsRowNotFound(err) {
		t.Errorf("a row in the trash: %v", err)
	}
	w.deleted = false
	w.level = accessBusiness.LevelNone
	if _, err := RowPageFor(bg, accessBusiness.Subject{UserUUID: "u"}, w.table, w.row); !IsRowNotFound(err) {
		t.Errorf("someone the row isn't open to read its page: %v", err)
	}
}

// A row's record reads its page with the row: none until it has one, then
// its doc and saved body, at the row's level never above edit. A link to a doc
// that isn't the row's own page is never followed.
func TestARowsPageIsReadWithTheRow(t *testing.T) {
	w := newPageWorld(t)
	bg := context.Background()
	who := accessBusiness.Subject{UserUUID: "u"}
	page, err := RowPageFor(bg, who, w.table, w.row)
	if err != nil || page.Doc != "" || page.Level != "edit" {
		t.Fatalf("a row with no page: %+v, %v", page, err)
	}
	doc, _, err := EnsureRowPageFor(bg, who, w.table, w.row)
	if err != nil {
		t.Fatal(err)
	}
	w.level = accessBusiness.LevelFull
	page, err = RowPageFor(bg, who, w.table, w.row)
	if err != nil || page.Doc != doc || page.Body != "<p>Call on Friday.</p>" || page.Level != "edit" {
		t.Fatalf("the row's page for its table's owner: %+v, %v", page, err)
	}
	w.level = accessBusiness.LevelView
	if page, _ = RowPageFor(bg, who, w.table, w.row); page.Level != "view" {
		t.Errorf("a reader's level: %+v", page)
	}
	// The link names a doc another row hosts.
	w.docs[doc].HostUUID = w.other.String()
	if page, err = RowPageFor(bg, who, w.table, w.row); err != nil || page.Doc != "" || page.Body != "" {
		t.Errorf("another row's page was served: %+v, %v", page, err)
	}
}

// Where a row's page is, for a docs path that refuses it to someone who may
// open the row: the table it belongs to, and the row's address.
func TestARowsPageSaysWhereItBelongs(t *testing.T) {
	w := newPageWorld(t)
	place, err := rowPagePlace(context.Background(), w.row.String())
	if err != nil {
		t.Fatal(err)
	}
	if place.Belongs != "This page belongs to a row of Deals. Change it there." ||
		place.Path != "/app/tables/"+w.table.String()+"?row="+w.row.String() {
		t.Errorf("the place: %+v", place)
	}
	if _, err := rowPagePlace(context.Background(), w.other.String()); !errors.Is(err, docBusiness.ErrNoHost) {
		t.Errorf("a row that isn't there was placed: %v", err)
	}
}

// Rows put in the trash close their pages' live sockets, so each reconnects
// and is decided afresh by its row; rows brought back, and cells changed,
// need nothing. The grid's rows carry their pages.
func TestRowsPutInTheTrashCloseTheirPages(t *testing.T) {
	w := newPageWorld(t)
	restored, edited := uuid.New(), uuid.New()
	page, back := uuid.New(), uuid.New()
	w.pages[w.row], w.pages[restored] = page, back
	deleted, _ := json.Marshal(map[string]any{"deleted": true})
	rowPagesLeft(context.Background(), &rowLedger.ChangeSet{Changes: []rowLedger.Change{
		{Row: w.row, Field: model.WholeRow, After: deleted},
		{Row: restored, Field: model.WholeRow, Before: deleted},
		{Row: edited, Field: uuid.NewString(), After: json.RawMessage(`"Won"`)},
	}})
	if len(w.changed) != 1 || len(w.changed[0]) != 1 || w.changed[0][0] != page.String() {
		t.Errorf("the pages told: %v, want only the deleted row's %s", w.changed, page)
	}
	rowPagesLeft(context.Background(), &rowLedger.ChangeSet{Changes: []rowLedger.Change{{Row: edited, Field: uuid.NewString()}}})
	if len(w.changed) != 1 {
		t.Errorf("a cell change told the pages: %v", w.changed)
	}

	rows := []*model.Row{{Id: w.row}, {Id: edited}, nil}
	WithPages(context.Background(), rows)
	if rows[0].Page == nil || *rows[0].Page != page || rows[1].Page != nil {
		t.Errorf("the grid's rows: %+v %+v", rows[0], rows[1])
	}
}
