//go:build integration
// +build integration

package integration_test

// A person's correction to an AI column's cell, against Postgres 12 with every
// migration (TAB-BUG-4).
//
// An "auto" AI column wrote its answer over every cell of the row after each
// write, so a person who corrected the AI's answer saw it replaced seconds
// later. Now a person's write that changes an AI cell marks it theirs, in the
// same statement, and a fill's answer for a marked cell is dropped, also in
// one statement, unless someone asks to fill it again.
//
// Run: go test -tags=integration ./tests/integration/ -run TestTableAICells -v

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	dataTableController "github.com/akashc777/OneCamp/controllers/DataTable"
	dataTableModel "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

func TestTableAICells(t *testing.T) {
	w := newTableWorld(t, "amara")
	me := w.people["amara"]
	tb := w.table("amara", "Accounts")
	notes := w.field("amara", tb, "Notes", dataTableModel.FieldText, nil)
	summary := w.field("amara", tb, "Summary", dataTableModel.FieldText,
		map[string]interface{}{"ai": map[string]interface{}{"prompt": "Summarise {Notes}", "auto": true}})
	row, err := dataTableBusiness.CreateRow(w.ctx, tb.Id, dataTableBusiness.RowInput{Values: map[string]interface{}{notes: "Renewal due in March"}}, me)
	if err != nil {
		t.Fatal(err)
	}
	// cellNow is the summary cell as a fill reads it before asking the model.
	cellNow := func() json.RawMessage {
		r, err := dataTableModel.GetRowByID(w.ctx, tb.Id, row.Id)
		if err != nil || r == nil {
			t.Fatal(err)
		}
		var cells map[string]json.RawMessage
		_ = json.Unmarshal([]byte(r.Values), &cells)
		return cells[summary]
	}
	// readNow is the notes cell as a fill reads it, the cell its prompt
	// reads, as readCells gives it.
	readNow := func() string {
		r, err := dataTableModel.GetRowByID(w.ctx, tb.Id, row.Id)
		if err != nil || r == nil {
			t.Fatal(err)
		}
		var cells map[string]json.RawMessage
		_ = json.Unmarshal([]byte(r.Values), &cells)
		b, _ := json.Marshal(map[string]json.RawMessage{notes: cells[notes]})
		return string(b)
	}
	// aiFills is a fill's answer landing, as fillOneCell writes it, for the
	// cell and the notes as the fill read them.
	aiFillsFrom := func(read json.RawMessage, inputs, answer string, again bool) error {
		b, _ := json.Marshal(map[string]string{summary: answer})
		_, err := dataTableModel.WriteAICell(w.ctx, tb.Id, row.Id, summary, string(b), read, inputs, again, 100*1024)
		return err
	}
	aiFills := func(read json.RawMessage, answer string, again bool) error {
		return aiFillsFrom(read, readNow(), answer, again)
	}
	aiWrites := func(answer string, again bool) error { return aiFills(cellNow(), answer, again) }
	kept := func(t *testing.T) []string {
		t.Helper()
		r, err := dataTableModel.GetRowByID(w.ctx, tb.Id, row.Id)
		if err != nil || r == nil {
			t.Fatal(err)
		}
		if strings.Contains(r.Values, dataTableModel.AIKeptKey) {
			t.Errorf("the row's values carry its marks: %s", r.Values)
		}
		return r.AIKept
	}

	if len(row.AIKept) != 0 || len(kept(t)) != 0 {
		t.Fatalf("a new row is marked %v", row.AIKept)
	}
	if err := aiWrites("Renews in March", false); err != nil {
		t.Fatalf("the first fill: %v", err)
	}

	t.Run("a write to another cell marks nothing", func(t *testing.T) {
		if _, err := dataTableBusiness.PatchRow(w.ctx, tb.Id, row.Id, map[string]interface{}{notes: "Renewal due in April"}, me); err != nil {
			t.Fatal(err)
		}
		if got := kept(t); len(got) != 0 {
			t.Errorf("editing the notes marked %v", got)
		}
	})

	t.Run("a person's correction is marked, and a fill landing after it is dropped", func(t *testing.T) {
		patched, err := dataTableBusiness.PatchRow(w.ctx, tb.Id, row.Id, map[string]interface{}{summary: "Renews in April; ask Bo"}, me)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(patched.AIKept, []string{summary}) || !reflect.DeepEqual(kept(t), []string{summary}) {
			t.Errorf("the correction is marked %v / %v", patched.AIKept, kept(t))
		}
		if err := aiWrites("Renews in April", false); !errors.Is(err, dataTableModel.ErrAIKept) {
			t.Errorf("a fill over the correction answered %v, want ErrAIKept", err)
		}
		if v := w.stored(tb, row.Id); v[summary] != "Renews in April; ask Bo" {
			t.Errorf("after the fill the summary is %v", v[summary])
		}
	})

	t.Run("the bundle says which cells a person wrote", func(t *testing.T) {
		b, err := dataTableBusiness.GetBundle(w.ctx, tb.Id, me)
		if err != nil || len(b.Rows) != 1 {
			t.Fatal(err)
		}
		text, _ := json.Marshal(b.Rows[0])
		if !strings.Contains(string(text), `"ai_kept":["`+summary+`"]`) || strings.Contains(string(text), dataTableModel.AIKeptKey) {
			t.Errorf("the bundle's row is %s", text)
		}
	})

	t.Run("a whole-row write keeps the marks, and marks only what it changes", func(t *testing.T) {
		values := map[string]interface{}{notes: "Renewal due in May", summary: "Renews in April; ask Bo", dataTableModel.AIKeptKey: []string{}}
		if _, err := dataTableBusiness.UpdateRow(w.ctx, tb.Id, row.Id, dataTableBusiness.RowInput{Values: values}, me); err != nil {
			t.Fatal(err)
		}
		if got := kept(t); !reflect.DeepEqual(got, []string{summary}) {
			t.Errorf("after a whole-row write the marks are %v", got)
		}
	})

	// A correction typed while the model answered was overwritten, and its
	// mark taken off, with no sign: even "Fill again" writes only over the
	// cell it read.
	t.Run("a fill whose cell changed while the model answered is dropped, even filling again", func(t *testing.T) {
		read := cellNow()
		if _, err := dataTableBusiness.PatchRow(w.ctx, tb.Id, row.Id, map[string]interface{}{summary: "Renews in April; ask Cy"}, me); err != nil {
			t.Fatal(err)
		}
		if err := aiFills(read, "Stale answer", false); !errors.Is(err, dataTableModel.ErrAIKept) {
			t.Errorf("a stale fill answered %v, want ErrAIKept", err)
		}
		if err := aiFills(read, "Stale answer", true); !errors.Is(err, dataTableModel.ErrCellChanged) {
			t.Errorf("filling again over a cell changed meanwhile answered %v, want ErrCellChanged", err)
		}
		if v := w.stored(tb, row.Id); v[summary] != "Renews in April; ask Cy" {
			t.Errorf("after the stale fills the summary is %v", v[summary])
		}
	})

	t.Run("fill again writes over the correction and takes the mark off", func(t *testing.T) {
		if err := aiWrites("Renews in May", true); err != nil {
			t.Fatal(err)
		}
		if got := kept(t); len(got) != 0 {
			t.Errorf("after filling again the marks are %v", got)
		}
		if v := w.stored(tb, row.Id); v[summary] != "Renews in May" || v[notes] != "Renewal due in May" {
			t.Errorf("after filling again the row is %v", v)
		}
		if err := aiWrites("Renews in June", false); err != nil {
			t.Errorf("a later fill: %v", err)
		}
	})

	// Two edits a moment apart each ask the model, and the first to answer
	// read the older notes: it was written, and the answer for the newer
	// notes was dropped, as the cell had changed.
	t.Run("of two fills for two quick edits, the answer for the newer notes is written", func(t *testing.T) {
		firstCell, firstNotes := cellNow(), readNow()
		if _, err := dataTableBusiness.PatchRow(w.ctx, tb.Id, row.Id, map[string]interface{}{notes: "Renewal due in July"}, me); err != nil {
			t.Fatal(err)
		}
		secondCell, secondNotes := cellNow(), readNow()
		if err := aiFillsFrom(firstCell, firstNotes, "Renews in June", false); !errors.Is(err, dataTableModel.ErrCellChanged) {
			t.Errorf("the fill for the older notes answered %v, want ErrCellChanged", err)
		}
		if err := aiFillsFrom(secondCell, secondNotes, "Renews in July", false); err != nil {
			t.Errorf("the fill for the newer notes answered %v", err)
		}
		if v := w.stored(tb, row.Id); v[summary] != "Renews in July" {
			t.Errorf("the summary is %v, want the newer notes' answer", v[summary])
		}
	})

	// The web released before patches saves a cell edit as its copy of the
	// whole row, with the AI cells as it last showed them.
	t.Run("a whole row from a copy read before an answer landed keeps the answer, and its edit", func(t *testing.T) {
		copied := w.stored(tb, row.Id)
		if err := aiWrites("Renews in July; call Bo", false); err != nil {
			t.Fatal(err)
		}
		copied[notes] = "Renewal due in August"
		if _, err := dataTableBusiness.UpdateRow(w.ctx, tb.Id, row.Id, dataTableBusiness.RowInput{Values: copied}, me); err != nil {
			t.Fatal(err)
		}
		if v := w.stored(tb, row.Id); v[summary] != "Renews in July; call Bo" || v[notes] != "Renewal due in August" {
			t.Errorf("after the copy's save the row is %v", v)
		}
		if got := kept(t); len(got) != 0 {
			t.Errorf("the copy's save marked %v as a person's", got)
		}
	})

	t.Run("a whole row changing only its AI cell is a correction, written and marked", func(t *testing.T) {
		values := w.stored(tb, row.Id)
		values[summary] = "Renews in August; ask Cy"
		if _, err := dataTableBusiness.UpdateRow(w.ctx, tb.Id, row.Id, dataTableBusiness.RowInput{Values: values}, me); err != nil {
			t.Fatal(err)
		}
		if v := w.stored(tb, row.Id); v[summary] != "Renews in August; ask Cy" {
			t.Errorf("after the correction the summary is %v", v[summary])
		}
		if got := kept(t); !reflect.DeepEqual(got, []string{summary}) {
			t.Errorf("after the correction the marks are %v", got)
		}
	})
}

// The web released before patches (v2.72.2's) saves a cell edit as its copy
// of the whole row, the AI cells as it last showed them: lib/tables/
// useCellCommit.ts posts {values, position} to the row's update. A row's AI
// cells are kept as stored unless one of them is the only cell such a save
// changes, against the row as stored, so a copy read before an answer landed
// puts no answer back, and marks none a person's. The web since patches the
// cells it names.
func TestTableAICellsFromTheReleasedWeb(t *testing.T) {
	w := newTableWorld(t, "amara")
	me := w.people["amara"]
	tb := w.table("amara", "Accounts")
	notes := w.field("amara", tb, "Notes", dataTableModel.FieldText, nil)
	summary := w.field("amara", tb, "Summary", dataTableModel.FieldText,
		map[string]interface{}{"ai": map[string]interface{}{"prompt": "Summarise {Notes}", "auto": true}})
	next := w.field("amara", tb, "Next step", dataTableModel.FieldText,
		map[string]interface{}{"ai": map[string]interface{}{"prompt": "The next step for {Notes}", "auto": true}})
	update := w.asMember("amara", http.MethodPost, "/tables/{id}/rows/{rowId}/update", dataTableController.UpdateRow)
	patch := w.asMember("amara", http.MethodPost, "/tables/{id}/rows/{rowId}/patch", dataTableController.PatchRow)

	read := func(t *testing.T, row uuid.UUID) *dataTableModel.Row {
		t.Helper()
		r, err := dataTableModel.GetRowByID(w.ctx, tb.Id, row)
		if err != nil || r == nil {
			t.Fatalf("read row %s: %v", row, err)
		}
		return r
	}
	newRow := func(t *testing.T) uuid.UUID {
		t.Helper()
		r, err := dataTableBusiness.CreateRow(w.ctx, tb.Id, dataTableBusiness.RowInput{Values: map[string]interface{}{notes: "Renewal due in March"}}, me)
		if err != nil {
			t.Fatal(err)
		}
		return r.Id
	}
	// reads is what a fill of field reads before asking the model: its cell,
	// and the notes its prompt names (readCells).
	reads := func(t *testing.T, row uuid.UUID, field string) (json.RawMessage, string) {
		t.Helper()
		var cells map[string]json.RawMessage
		_ = json.Unmarshal([]byte(read(t, row).Values), &cells)
		inputs, _ := json.Marshal(map[string]json.RawMessage{notes: cells[notes]})
		return cells[field], string(inputs)
	}
	// lands is that fill's answer landing, as fillOneCell writes it.
	lands := func(row uuid.UUID, field string, cell json.RawMessage, inputs, answer string) error {
		b, _ := json.Marshal(map[string]string{field: answer})
		_, err := dataTableModel.WriteAICell(w.ctx, tb.Id, row, field, string(b), cell, inputs, false, 100*1024)
		return err
	}
	fills := func(t *testing.T, row uuid.UUID, field, answer string) {
		t.Helper()
		cell, inputs := reads(t, row, field)
		if err := lands(row, field, cell, inputs, answer); err != nil {
			t.Fatalf("the fill of %s: %v", field, err)
		}
	}
	post := func(t *testing.T, h http.Handler, path string, body interface{}) {
		t.Helper()
		b, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b)))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s answered %d %s", b, rec.Code, rec.Body)
		}
	}
	// saves is a cell edit as that web saves it: its copy of the row's
	// values with the edit, and the row's place.
	saves := func(t *testing.T, row uuid.UUID, copied map[string]interface{}, field string, value interface{}) {
		t.Helper()
		values := map[string]interface{}{field: value}
		for k, v := range copied {
			if k != field {
				values[k] = v
			}
		}
		post(t, update, fmt.Sprintf("/tables/%s/rows/%s/update", tb.Id, row),
			map[string]interface{}{"values": values, "position": read(t, row).Position})
	}
	marks := func(t *testing.T, row uuid.UUID) []string {
		t.Helper()
		return read(t, row).AIKept
	}

	t.Run("a copy read before an answer landed keeps the answer, and its edit", func(t *testing.T) {
		row := newRow(t)
		fills(t, row, summary, "Renews in March")
		copied := w.stored(tb, row)
		fills(t, row, summary, "Renews in March; call Bo")
		saves(t, row, copied, notes, "Renewal due in August")
		if v := w.stored(tb, row); v[summary] != "Renews in March; call Bo" || v[notes] != "Renewal due in August" {
			t.Errorf("after the copy's save the row is %v", v)
		}
		if got := marks(t, row); len(got) != 0 {
			t.Errorf("the copy's save marked %v as a person's", got)
		}
	})

	t.Run("a copy changing only an AI cell is a correction, written and marked", func(t *testing.T) {
		row := newRow(t)
		fills(t, row, summary, "Renews in March")
		saves(t, row, w.stored(tb, row), summary, "Renews in March; ask Cy")
		if v := w.stored(tb, row); v[summary] != "Renews in March; ask Cy" {
			t.Errorf("after the correction the summary is %v", v[summary])
		}
		if got := marks(t, row); !reflect.DeepEqual(got, []string{summary}) {
			t.Errorf("after the correction the marks are %v", got)
		}
	})

	// The other AI column's answer landed after the page read the row: the
	// copy holds the one before, or none. A save changing two AI cells can't
	// say which is the person's, so both stay as stored; the correction is
	// left out, and the page shows the summary as stored when it reads the
	// row again.
	t.Run("a correction from a copy read before another AI cell's answer landed keeps that answer", func(t *testing.T) {
		row := newRow(t)
		fills(t, row, summary, "Renews in March")
		fills(t, row, next, "Call in February")
		copied := w.stored(tb, row)
		fills(t, row, next, "Send the renewal quote")
		saves(t, row, copied, summary, "Renews in March; ask Cy")
		if v := w.stored(tb, row); v[next] != "Send the renewal quote" || v[summary] != "Renews in March" {
			t.Errorf("after the copy's save the row is %v", v)
		}
		if got := marks(t, row); len(got) != 0 {
			t.Errorf("the copy's save marked %v as a person's", got)
		}
	})

	t.Run("a correction from a copy read before another AI cell had an answer keeps that answer", func(t *testing.T) {
		row := newRow(t)
		fills(t, row, summary, "Renews in March")
		copied := w.stored(tb, row)
		fills(t, row, next, "Send the renewal quote")
		saves(t, row, copied, summary, "Renews in March; ask Cy")
		if v := w.stored(tb, row); v[next] != "Send the renewal quote" || v[summary] != "Renews in March" {
			t.Errorf("after the copy's save the row is %v", v)
		}
		if got := marks(t, row); len(got) != 0 {
			t.Errorf("the copy's save marked %v as a person's", got)
		}
	})

	t.Run("the web since writes the correction it names, and leaves the other answer", func(t *testing.T) {
		row := newRow(t)
		fills(t, row, summary, "Renews in March")
		fills(t, row, next, "Send the renewal quote")
		post(t, patch, fmt.Sprintf("/tables/%s/rows/%s/patch", tb.Id, row),
			map[string]interface{}{"values": map[string]interface{}{summary: "Renews in March; ask Cy"}})
		if v := w.stored(tb, row); v[summary] != "Renews in March; ask Cy" || v[next] != "Send the renewal quote" {
			t.Errorf("after the patch the row is %v", v)
		}
		if got := marks(t, row); !reflect.DeepEqual(got, []string{summary}) {
			t.Errorf("after the patch the marks are %v", got)
		}
	})

	// Two quick edits from that web, the second carrying the first, each ask
	// the model; the answer for the older notes lands first.
	t.Run("of two fills for two quick edits from that web, the answer for the newer notes is written", func(t *testing.T) {
		row := newRow(t)
		fills(t, row, summary, "Renews in March")
		copied := w.stored(tb, row)
		saves(t, row, copied, notes, "Renewal due in June")
		firstCell, firstNotes := reads(t, row, summary)
		copied[notes] = "Renewal due in June"
		saves(t, row, copied, notes, "Renewal due in July")
		secondCell, secondNotes := reads(t, row, summary)
		if err := lands(row, summary, firstCell, firstNotes, "Renews in June"); !errors.Is(err, dataTableModel.ErrCellChanged) {
			t.Errorf("the fill for the older notes answered %v, want ErrCellChanged", err)
		}
		if err := lands(row, summary, secondCell, secondNotes, "Renews in July"); err != nil {
			t.Errorf("the fill for the newer notes answered %v", err)
		}
		if v := w.stored(tb, row); v[summary] != "Renews in July" {
			t.Errorf("the summary is %v, want the newer notes' answer", v[summary])
		}
	})
}
