//go:build integration
// +build integration

package integration_test

// Renaming, moving and removing a select field's options, against Postgres 12
// with every migration (TAB-BUG-3).
//
// A select cell holds its option's label, and renaming an option changed only
// the field's config: every cell that had it kept the old label, lost its
// colour, and went to "No value" on the board. Now options keep ids, and
// saving a field rewrites its cells in the same transaction, over every row
// (here 700, past the page the web holds).
//
// Run: go test -tags=integration ./tests/integration/ -run TestTableSelectOptions -v

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	dataTableController "github.com/akashc777/OneCamp/controllers/DataTable"
	"github.com/akashc777/OneCamp/helpers"
	dataTableModel "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/akashc777/OneCamp/tests/integration"
)

// uuidOf is an id written as text, as a uuid.
func uuidOf(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// listen records what the server publishes, for tableActions.
func (w *tableWorld) listen() *noticeBroker {
	b := &noticeBroker{}
	integration.UseMqttClient(w.t, b)
	return b
}

// tableActions is the actions published on a table's topic, in order.
func tableActions(b *noticeBroker, tb *dataTableModel.DataTable) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, m := range b.got {
		var msg struct {
			Data struct {
				Action string `json:"action"`
			} `json:"data"`
		}
		if m.topic == helpers.GetMqttTopicForTable(tb.Id.String()) && json.Unmarshal(m.payload, &msg) == nil {
			out = append(out, msg.Data.Action)
		}
	}
	return out
}

func TestTableSelectOptions(t *testing.T) {
	w := newTableWorld(t, "owner")
	me := w.people["owner"]
	tb := w.table("owner", "Tasks")
	options := func(labels ...string) map[string]interface{} {
		list := make([]interface{}, len(labels))
		for i, l := range labels {
			list[i] = map[string]interface{}{"label": l}
		}
		return map[string]interface{}{"options": list}
	}
	status := w.field("owner", tb, "Status", dataTableModel.FieldSelect, options("Todo", "Doing", "Done"))
	tags := w.field("owner", tb, "Tags", dataTableModel.FieldMultiSelect, options("Design", "Launch", "Ops"))
	// 700 rows: Todo, Doing, Done in turn; tags in four patterns.
	if _, err := w.env.PG.Exec(`INSERT INTO data_table_rows (id, table_id, values, position, created_by)
		SELECT uuid_generate_v4(), $1, jsonb_build_object(
			$2::text, (ARRAY['Todo','Doing','Done'])[n % 3 + 1],
			$3::text, (ARRAY['["Design","Launch"]','["Launch"]','["Ops","Design"]','[]'])[n % 4 + 1]::jsonb), n, $4
		FROM generate_series(0, 699) n`, tb.Id, status, tags, me.UserID); err != nil {
		t.Fatalf("seed rows: %v", err)
	}
	count := func(t *testing.T, where string, args ...interface{}) int {
		t.Helper()
		var n int
		if err := w.env.PG.QueryRow(`SELECT COUNT(*) FROM data_table_rows WHERE table_id = $1 AND deleted_at IS NULL AND `+where,
			append([]interface{}{tb.Id}, args...)...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	statusIs := func(t *testing.T, label string) int {
		return count(t, `values ->> $2 = $3`, status, label)
	}
	tagged := func(t *testing.T, label string) int {
		return count(t, `values -> $2 ? $3`, tags, label)
	}
	// update saves a field's options as given, the ids in removed taken out,
	// as the field's owner.
	update := func(t *testing.T, field, name, typ, list string, removed ...string) int {
		t.Helper()
		var cfg map[string]interface{}
		if err := json.Unmarshal([]byte(`{"options":`+list+`}`), &cfg); err != nil {
			t.Fatal(err)
		}
		if len(removed) > 0 {
			cfg["removed_options"] = removed
		}
		changed, err := dataTableBusiness.UpdateField(w.ctx, tb.Id, uuidOf(t, field), dataTableBusiness.FieldPatch{Name: &name, Type: &typ, Config: cfg}, me)
		if err != nil {
			t.Fatalf("update %s: %v", name, err)
		}
		return changed
	}
	stored := func(t *testing.T, field string) string {
		t.Helper()
		var cfg string
		if err := w.env.PG.QueryRow(`SELECT config::text FROM data_table_fields WHERE id = $1`, field).Scan(&cfg); err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	todo, doing, done := statusIs(t, "Todo"), statusIs(t, "Doing"), statusIs(t, "Done")
	if todo+doing+done != 700 {
		t.Fatalf("seeded %d, %d and %d", todo, doing, done)
	}

	t.Run("a new field's options are stored with ids", func(t *testing.T) {
		if cfg := stored(t, status); cfg != `{"options": [{"id": "label:Todo", "label": "Todo"}, {"id": "label:Doing", "label": "Doing"}, {"id": "label:Done", "label": "Done"}]}` {
			t.Errorf("the status field is stored as %s", cfg)
		}
	})

	t.Run("renaming an option renames it in every row, and the table's readers hear of it", func(t *testing.T) {
		broker := w.listen()
		changed := update(t, status, "Status", dataTableModel.FieldSelect,
			`[{"id":"label:Todo","label":"To do","color":"red"},{"id":"label:Doing","label":"Doing"},{"id":"label:Done","label":"Done"}]`)
		if changed != todo || statusIs(t, "To do") != todo || statusIs(t, "Todo") != 0 {
			t.Errorf("renamed %d rows; %d say To do and %d Todo; want all %d", changed, statusIs(t, "To do"), statusIs(t, "Todo"), todo)
		}
		if got := tableActions(broker, tb); len(got) != 1 || got[0] != "fields" {
			t.Errorf("the table's topic was told %v, want [fields]", got)
		}
	})

	// That web saves a field whole: its name, type, options and place, the
	// options with the ids it read.
	t.Run("the web before patches renames a multi-select option by the id it read", func(t *testing.T) {
		launch := tagged(t, "Launch")
		var cfg map[string]interface{}
		if err := json.Unmarshal([]byte(`{"options":[{"id":"label:Design","label":"Design"},{"id":"label:Launch","label":"Go-live"},{"id":"label:Ops","label":"Ops"}]}`), &cfg); err != nil {
			t.Fatal(err)
		}
		var at float64
		if err := w.env.PG.QueryRow(`SELECT position FROM data_table_fields WHERE id = $1`, tags).Scan(&at); err != nil {
			t.Fatal(err)
		}
		changed, err := dataTableBusiness.UpdateField(w.ctx, tb.Id, uuidOf(t, tags),
			dataTableBusiness.FieldPatch{Name: ptr("Tags"), Type: ptr(dataTableModel.FieldMultiSelect), Config: cfg, Position: &at}, me)
		if err != nil {
			t.Fatalf("update Tags: %v", err)
		}
		if changed != launch || tagged(t, "Go-live") != launch || tagged(t, "Launch") != 0 {
			t.Errorf("renamed %d rows; %d have Go-live and %d Launch; want %d", changed, tagged(t, "Go-live"), tagged(t, "Launch"), launch)
		}
		if n := count(t, `values -> $2 = '["Design","Go-live"]'::jsonb`, tags); n != 175 {
			t.Errorf("%d rows hold [Design, Go-live] in order, want 175", n)
		}
	})

	t.Run("two options swapping names swap in their cells", func(t *testing.T) {
		changed := update(t, status, "Status", dataTableModel.FieldSelect,
			`[{"id":"label:Todo","label":"To do"},{"id":"label:Doing","label":"Done"},{"id":"label:Done","label":"Doing"}]`)
		if changed != doing+done || statusIs(t, "Done") != doing || statusIs(t, "Doing") != done {
			t.Errorf("swapped %d rows; now %d Done and %d Doing", changed, statusIs(t, "Done"), statusIs(t, "Doing"))
		}
	})

	t.Run("moving options changes no cell", func(t *testing.T) {
		if changed := update(t, status, "Status", dataTableModel.FieldSelect,
			`[{"id":"label:Done","label":"Doing"},{"id":"label:Todo","label":"To do"},{"id":"label:Doing","label":"Done"}]`); changed != 0 {
			t.Errorf("moving options changed %d rows", changed)
		}
	})

	t.Run("a removed option is taken out of its cells", func(t *testing.T) {
		ops := tagged(t, "Ops")
		changed := update(t, tags, "Tags", dataTableModel.FieldMultiSelect, `[{"label":"Design"},{"label":"Go-live"}]`, "label:Ops")
		if changed != ops || tagged(t, "Ops") != 0 || count(t, `values -> $2 = '["Design"]'::jsonb`, tags) != 175 {
			t.Errorf("took Ops out of %d rows (had %d); %d still have it", changed, ops, tagged(t, "Ops"))
		}
		before := statusIs(t, "To do")
		if changed := update(t, status, "Status", dataTableModel.FieldSelect,
			`[{"id":"label:Done","label":"Doing"},{"id":"label:Doing","label":"Done"}]`, "label:Todo"); changed != before || statusIs(t, "") != before {
			t.Errorf("removing To do cleared %d rows, %d now empty; want %d", changed, statusIs(t, ""), before)
		}
	})

	// A save from a copy read before an option was added, or from a menu
	// that hasn't read the options again, lists only some of them: the rest
	// are kept, and so are their cells (the audit's B1). So is the type, when
	// the save names none.
	t.Run("a save that leaves options out keeps them and their cells", func(t *testing.T) {
		doing := statusIs(t, "Doing")
		if changed := update(t, status, "Status", dataTableModel.FieldSelect, `[{"id":"o_blocked01","label":"Blocked"}]`); changed != 0 ||
			statusIs(t, "Doing") != doing || !strings.Contains(stored(t, status), `"Blocked"`) || !strings.Contains(stored(t, status), `"Doing"`) {
			t.Errorf("a save of Blocked alone changed %d rows; Doing in %d, was %d; config %s", changed, statusIs(t, "Doing"), doing, stored(t, status))
		}
		design := tagged(t, "Design")
		if changed := update(t, tags, "Tags", "", `null`); changed != 0 || tagged(t, "Design") != design {
			t.Errorf("a save with no type or options changed %d rows; Design in %d, was %d", changed, tagged(t, "Design"), design)
		}
		var typ string
		if err := w.env.PG.QueryRow(`SELECT type FROM data_table_fields WHERE id = $1`, tags).Scan(&typ); err != nil || typ != dataTableModel.FieldMultiSelect {
			t.Errorf("a save naming no type made the field %q (%v)", typ, err)
		}
	})

	// The audit's repro of B1: a column made a select, and then an option
	// added from a menu that read the field before it became one. The menu
	// sends its list, without the options the cells became.
	t.Run("an option added to a column just made a select keeps its cells", func(t *testing.T) {
		stage := w.field("owner", tb, "Stage", dataTableModel.FieldText, nil)
		if _, err := w.env.PG.Exec(`UPDATE data_table_rows SET values = values || jsonb_build_object($2::text, (ARRAY['Open','Won','Lost'])[position::int % 3 + 1])
			WHERE table_id = $1`, tb.Id, stage); err != nil {
			t.Fatal(err)
		}
		if changed := update(t, stage, "Stage", dataTableModel.FieldSelect, `[]`); changed != 0 {
			t.Fatalf("making it a select changed %d rows", changed)
		}
		if changed := update(t, stage, "Stage", "", `[{"id":"o_blocked02","label":"Blocked"}]`); changed != 0 {
			t.Errorf("adding Blocked changed %d rows", changed)
		}
		if n := count(t, `values ->> $2 IN ('Open','Won','Lost')`, stage); n != 700 {
			t.Errorf("%d of 700 rows kept their stage", n)
		}
		for _, label := range []string{"Blocked", "Open", "Won", "Lost"} {
			if !strings.Contains(stored(t, stage), `"label": "`+label+`"`) {
				t.Errorf("the field no longer offers %s: %s", label, stored(t, stage))
			}
		}
	})

	// Two saves of one field at once each plan the change to its cells from
	// the field as the other left it: the later waits for the field, then
	// reads it again. meanwhile holds the field as a save that got there
	// first does, runs save, and once save waits, makes first's change and
	// lets it go: save was planned from a copy read before first was made.
	meanwhile := func(t *testing.T, field string, first func(tx *sql.Tx), save func() error) error {
		t.Helper()
		tx, err := w.env.PG.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(`SELECT 1 FROM data_table_fields WHERE id = $1 FOR UPDATE`, field); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- save() }()
		waiting := false
		for i := 0; i < 400 && !waiting; i++ {
			if err := w.env.PG.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_stat_activity
				WHERE wait_event_type = 'Lock' AND query LIKE '%data_table_fields%')`).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if !waiting {
				time.Sleep(25 * time.Millisecond)
			}
		}
		if !waiting {
			t.Fatal("the save never waited for the field")
		}
		first(tx)
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			return err
		case <-time.After(time.Minute):
			t.Fatal("the save never finished")
			return nil
		}
	}
	phase := w.field("owner", tb, "Phase", dataTableModel.FieldSelect,
		map[string]interface{}{"options": []interface{}{map[string]interface{}{"id": "p1", "label": "Alpha"}}})
	if _, err := w.env.PG.Exec(`UPDATE data_table_rows SET values = values || jsonb_build_object($2::text, 'Alpha') WHERE table_id = $1`, tb.Id, phase); err != nil {
		t.Fatal(err)
	}
	phaseIs := func(t *testing.T, label string) int { return count(t, `values ->> $2 = $3`, phase, label) }
	phaseID := uuidOf(t, phase)

	t.Run("an option renamed twice at once is renamed in every cell", func(t *testing.T) {
		err := meanwhile(t, phase, func(tx *sql.Tx) {
			// The first save: Alpha renamed Beta, in the field and its cells.
			if _, err := tx.Exec(`UPDATE data_table_fields SET config = '{"options":[{"id":"p1","label":"Beta"}]}' WHERE id = $1`, phase); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(`UPDATE data_table_rows SET values = jsonb_set(values, ARRAY[$2::text], '"Beta"')
				WHERE table_id = $1 AND values ->> $2 = 'Alpha'`, tb.Id, phase); err != nil {
				t.Fatal(err)
			}
		}, func() error {
			_, err := dataTableBusiness.UpdateField(w.ctx, tb.Id, phaseID, dataTableBusiness.FieldPatch{Name: ptr("Phase"),
				Config: map[string]interface{}{"options": []interface{}{map[string]interface{}{"id": "p1", "label": "Gamma"}}}}, me)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if phaseIs(t, "Gamma") != 700 || phaseIs(t, "Beta") != 0 {
			t.Errorf("%d rows say Gamma and %d Beta; want all 700 Gamma", phaseIs(t, "Gamma"), phaseIs(t, "Beta"))
		}
	})

	t.Run("a save naming no type while the type changes changes nothing", func(t *testing.T) {
		err := meanwhile(t, phase, func(tx *sql.Tx) {
			if _, err := tx.Exec(`UPDATE data_table_fields SET type = 'multi_select' WHERE id = $1`, phase); err != nil {
				t.Fatal(err)
			}
		}, func() error {
			_, err := dataTableBusiness.UpdateField(w.ctx, tb.Id, phaseID, dataTableBusiness.FieldPatch{Name: ptr("Phase renamed"),
				Config: map[string]interface{}{"options": []interface{}{map[string]interface{}{"id": "p1", "label": "Gamma"}}}}, me)
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "changed while you were saving") {
			t.Errorf("the save answered %v, want that the field changed", err)
		}
		var typ, name string
		if err := w.env.PG.QueryRow(`SELECT type, name FROM data_table_fields WHERE id = $1`, phase).Scan(&typ, &name); err != nil || typ != dataTableModel.FieldMultiSelect || name != "Phase" {
			t.Errorf("the field is now %q %q (%v); want the multi-select its type change made it", name, typ, err)
		}
	})

	// A save names what it changes, and the rest keeps its stored value: a
	// rename sent its page's copy whole, so it put back the options and the
	// place the copy held, and one sending no place moved the column to the
	// front.
	t.Run("a move or a rename alone keeps everything else", func(t *testing.T) {
		twice := w.field("owner", tb, "Twice", dataTableModel.FieldFormula, map[string]interface{}{"formula": "{Status} & {Status}"})
		send := func(t *testing.T, field, body string) {
			t.Helper()
			rec := httptest.NewRecorder()
			w.asMember("owner", http.MethodPost, "/tables/{id}/fields/{fieldId}/update", dataTableController.UpdateField).
				ServeHTTP(rec, httptest.NewRequest(http.MethodPost, fmt.Sprintf("/tables/%s/fields/%s/update", tb.Id, field), bytes.NewBufferString(body)))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s answered %d %s", body, rec.Code, rec.Body)
			}
		}
		read := func(t *testing.T, field string) (name string, position float64, config string) {
			t.Helper()
			if err := w.env.PG.QueryRow(`SELECT name, position, config::text FROM data_table_fields WHERE id = $1`, field).Scan(&name, &position, &config); err != nil {
				t.Fatal(err)
			}
			return name, position, config
		}
		_, _, options := read(t, status)
		send(t, status, `{"position": 3}`)
		if name, at, cfg := read(t, status); name != "Status" || at != 3 || cfg != options {
			t.Errorf("a move alone left %q at %v with %s", name, at, cfg)
		}
		send(t, status, `{"name": "State"}`)
		if name, at, cfg := read(t, status); name != "State" || at != 3 || cfg != options {
			t.Errorf("a rename alone left %q at %v with %s; want the place 3 and %s", name, at, cfg, options)
		}
		_, _, formula := read(t, twice)
		send(t, twice, `{"name": "Double"}`)
		if name, _, cfg := read(t, twice); name != "Double" || cfg != formula {
			t.Errorf("a formula renamed alone is %q with %s, was %s", name, cfg, formula)
		}
		send(t, status, `{"name": "Status"}`)
	})

	t.Run("over HTTP the update says how many rows changed", func(t *testing.T) {
		h := w.asMember("owner", http.MethodPost, "/tables/{id}/fields/{fieldId}/update", dataTableController.UpdateField)
		body := `{"name":"Status","type":"select","config":{"options":[{"id":"label:Done","label":"Doing"},{"id":"label:Doing","label":"Finished"}]}}`
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, fmt.Sprintf("/tables/%s/fields/%s/update", tb.Id, status), bytes.NewBufferString(body)))
		var out map[string]interface{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code != http.StatusOK || out["cells_changed"] != float64(doing) {
			t.Errorf("the update answered %d %v, want cells_changed %d", rec.Code, out, doing)
		}
	})
}

// The web released before ids (v2.72.2's) saves a field whole with each
// change: its name, type, config and place, with the options as its menu
// holds them (components/table/ColumnMenu.tsx's OptionsEditor and type
// picker, saved by DataTableGrid's saveColumn), one rename, removal, addition
// or move at a time, and the ids it read sent back. Open tabs keep that web
// through a deploy, and a server updated before its web is reached by it for
// as long as that takes: each such save is read as v2.72.2 read it, and
// changes no cell it didn't, but a rename's by its id. The web since names
// only what it changes, and keeps an option it leaves out unless it says it
// took it out.
func TestTableOptionsFromTheReleasedWeb(t *testing.T) {
	w := newTableWorld(t, "owner")
	me := w.people["owner"]
	tb := w.table("owner", "Deals")
	stage := w.field("owner", tb, "Stage", dataTableModel.FieldSelect, nil)
	phase := w.field("owner", tb, "Phase", dataTableModel.FieldSelect, nil)
	amount := w.field("owner", tb, "Amount", dataTableModel.FieldText, nil)
	cost := w.field("owner", tb, "Cost", dataTableModel.FieldText, nil)
	// The options as v2.72.2 stored them, without ids; 30 rows.
	for field, options := range map[string]string{stage: `[{"label":"Open","color":"blue"},{"label":"Won"},{"label":"Lost"}]`, phase: `[{"label":"Alpha"},{"label":"Beta"}]`} {
		if _, err := w.env.PG.Exec(`UPDATE data_table_fields SET config = jsonb_build_object('options', $2::jsonb) WHERE id = $1`, field, options); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.env.PG.Exec(`INSERT INTO data_table_rows (id, table_id, values, position, created_by)
		SELECT uuid_generate_v4(), $1, jsonb_build_object($2::text, (ARRAY['Open','Won','Lost'])[n % 3 + 1], $3::text, (ARRAY['Alpha','Beta'])[n % 2 + 1],
			$4::text, (ARRAY['12','1.234','TBD'])[n % 3 + 1], $5::text, (ARRAY['12','1.234','TBD'])[n % 3 + 1]), n, $6
		FROM generate_series(0, 29) n`, tb.Id, stage, phase, amount, cost, me.UserID); err != nil {
		t.Fatalf("seed rows: %v", err)
	}
	h := w.asMember("owner", http.MethodPost, "/tables/{id}/fields/{fieldId}/update", dataTableController.UpdateField)
	// save posts a field's save, and answers how many rows' cells changed.
	save := func(t *testing.T, field, body string) int {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, fmt.Sprintf("/tables/%s/fields/%s/update", tb.Id, field), bytes.NewBufferString(body)))
		var out struct {
			CellsChanged int `json:"cells_changed"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("%s answered %d %s", body, rec.Code, rec.Body)
		}
		return out.CellsChanged
	}
	holding := func(t *testing.T, field, label string) int {
		t.Helper()
		var n int
		if err := w.env.PG.QueryRow(`SELECT COUNT(*) FROM data_table_rows WHERE table_id = $1 AND deleted_at IS NULL AND values ->> $2 = $3`,
			tb.Id, field, label).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// offered is the field's options as stored, "label=id" in order.
	offered := func(t *testing.T, field string) string {
		t.Helper()
		var list string
		if err := w.env.PG.QueryRow(`SELECT COALESCE(string_agg((o ->> 'label') || '=' || (o ->> 'id'), ', ' ORDER BY n), '')
			FROM data_table_fields, jsonb_array_elements(config -> 'options') WITH ORDINALITY AS e(o, n) WHERE id = $1`, field).Scan(&list); err != nil {
			t.Fatal(err)
		}
		return list
	}

	t.Run("the first save stores the options' ids and changes no cell", func(t *testing.T) {
		if n := save(t, stage, `{"name":"Stage","type":"select","config":{"options":[{"label":"Open","color":"green"},{"label":"Won"},{"label":"Lost"}]},"position":0}`); n != 0 {
			t.Errorf("a colour changed %d rows", n)
		}
		if got := offered(t, stage); got != "Open=label:Open, Won=label:Won, Lost=label:Lost" {
			t.Errorf("the options are %s", got)
		}
	})

	t.Run("a rename by the id it read renames the option in its cells", func(t *testing.T) {
		if n := save(t, stage, `{"name":"Stage","type":"select","config":{"options":[{"id":"label:Open","label":"Open","color":"green"},{"id":"label:Won","label":"Closed won"},{"id":"label:Lost","label":"Lost"}]},"position":0}`); n != 10 ||
			holding(t, stage, "Closed won") != 10 || holding(t, stage, "Won") != 0 {
			t.Errorf("renamed %d rows; %d say Closed won, %d Won", n, holding(t, stage, "Closed won"), holding(t, stage, "Won"))
		}
	})

	t.Run("a removal takes the option out, and its cells keep it", func(t *testing.T) {
		if n := save(t, stage, `{"name":"Stage","type":"select","config":{"options":[{"id":"label:Open","label":"Open","color":"green"},{"id":"label:Won","label":"Closed won"}]},"position":0}`); n != 0 ||
			holding(t, stage, "Lost") != 10 {
			t.Errorf("removing Lost changed %d rows; %d still say Lost, want 10", n, holding(t, stage, "Lost"))
		}
		if got := offered(t, stage); got != "Open=label:Open, Closed won=label:Won" {
			t.Errorf("the options are %s", got)
		}
	})

	t.Run("an addition and a move change no cell", func(t *testing.T) {
		if n := save(t, stage, `{"name":"Stage","type":"select","config":{"options":[{"id":"label:Open","label":"Open","color":"green"},{"id":"label:Won","label":"Closed won"},{"label":"Paused"}]},"position":0}`); n != 0 {
			t.Errorf("adding Paused changed %d rows", n)
		}
		if n := save(t, stage, `{"name":"Stage","type":"select","config":{"options":[{"id":"label:Won","label":"Closed won"},{"id":"label:Open","label":"Open","color":"green"},{"id":"label:Paused","label":"Paused"}]},"position":0}`); n != 0 {
			t.Errorf("moving Closed won changed %d rows", n)
		}
		if got := offered(t, stage); got != "Closed won=label:Won, Open=label:Open, Paused=label:Paused" {
			t.Errorf("the options are %s", got)
		}
	})

	// A tab read before Paused was added adds Cancelled where Paused now is,
	// last: as v2.72.2 did, its list takes Paused out, and Paused's cells
	// keep it. Read as a rename by place, they became Cancelled.
	t.Run("a list read before an option was added gives that option's cells no other label", func(t *testing.T) {
		if _, err := w.env.PG.Exec(`UPDATE data_table_rows SET values = jsonb_set(values, ARRAY[$2::text], '"Paused"') WHERE table_id = $1 AND values ->> $2 = 'Lost'`, tb.Id, stage); err != nil {
			t.Fatal(err)
		}
		if n := save(t, stage, `{"name":"Stage","type":"select","config":{"options":[{"id":"label:Open","label":"Open","color":"green"},{"id":"label:Won","label":"Closed won"},{"label":"Cancelled"}]},"position":0}`); n != 0 ||
			holding(t, stage, "Paused") != 10 || holding(t, stage, "Cancelled") != 0 {
			t.Errorf("the stale list changed %d rows; %d say Paused, %d Cancelled", n, holding(t, stage, "Paused"), holding(t, stage, "Cancelled"))
		}
	})

	t.Run("the first rename of a field saved before ids leaves its cells as they are, as v2.72.2 did", func(t *testing.T) {
		if n := save(t, phase, `{"name":"Phase","type":"select","config":{"options":[{"label":"Alpha"},{"label":"Gamma"}]},"position":0}`); n != 0 ||
			holding(t, phase, "Beta") != 15 || holding(t, phase, "Gamma") != 0 {
			t.Errorf("the rename changed %d rows; %d say Beta, %d Gamma", n, holding(t, phase, "Beta"), holding(t, phase, "Gamma"))
		}
		if got := offered(t, phase); got != "Alpha=label:Alpha, Gamma=label:Gamma" {
			t.Errorf("the options are %s", got)
		}
	})

	t.Run("the web since keeps an option it leaves out, unless it took it out", func(t *testing.T) {
		if n := save(t, stage, `{"config":{"options":[{"id":"label:Open","label":"Open","color":"green"}]}}`); n != 0 {
			t.Errorf("a save leaving Closed won and Cancelled out changed %d rows", n)
		}
		if got := offered(t, stage); got != "Open=label:Open, Closed won=label:Won, Cancelled=label:Cancelled" {
			t.Errorf("after a save leaving options out they are %s", got)
		}
		if n := save(t, stage, `{"config":{"options":[{"id":"label:Open","label":"Open","color":"green"},{"id":"label:Cancelled","label":"Cancelled"}],"removed_options":["label:Won"]}}`); n != 10 ||
			holding(t, stage, "Closed won") != 0 || holding(t, stage, "") != 10 {
			t.Errorf("taking Closed won out changed %d rows; %d still say it", n, holding(t, stage, "Closed won"))
		}
	})

	// That web's type picker sends the field whole with the new type. v2.72.2
	// kept the cells as they were, and so does this: read with that web's
	// signs, "1.234" written in de-DE would be 1.234, not 1234.
	t.Run("a type change keeps the cells, and the web since converts them", func(t *testing.T) {
		if n := save(t, amount, `{"name":"Amount","type":"number","config":{},"position":0}`); n != 0 ||
			holding(t, amount, "12") != 10 || holding(t, amount, "1.234") != 10 || holding(t, amount, "TBD") != 10 {
			t.Errorf("that web's type change changed %d rows; the cells are %d 12, %d 1.234, %d TBD",
				n, holding(t, amount, "12"), holding(t, amount, "1.234"), holding(t, amount, "TBD"))
		}
		var typ string
		if err := w.env.PG.QueryRow(`SELECT type FROM data_table_fields WHERE id = $1`, amount).Scan(&typ); err != nil || typ != dataTableModel.FieldNumber {
			t.Errorf("the field is %q (%v), want a number", typ, err)
		}
		if n := save(t, cost, `{"type":"number","config":{},"decimal":",","group":"."}`); n != 30 {
			t.Errorf("the web since's type change changed %d rows, want 30", n)
		}
		var thousands, twelves, cleared int
		if err := w.env.PG.QueryRow(`SELECT COUNT(*) FILTER (WHERE values -> $2 = '1234'::jsonb), COUNT(*) FILTER (WHERE values -> $2 = '12'::jsonb),
			COUNT(*) FILTER (WHERE values -> $2 = 'null'::jsonb) FROM data_table_rows WHERE table_id = $1`, tb.Id, cost).Scan(&thousands, &twelves, &cleared); err != nil {
			t.Fatal(err)
		}
		if thousands != 10 || twelves != 10 || cleared != 10 {
			t.Errorf("read in de-DE's signs, the cells are %d 1234, %d 12 and %d cleared; want 10 of each", thousands, twelves, cleared)
		}
	})
}
