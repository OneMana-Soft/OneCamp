//go:build integration

package business

// OneCamp's own table templates against Postgres 12 with every migration
// (gsheets-structure-05): each installs as the member who asks, as its linked
// tables, opening on the first; its links read from both sides, its boards
// group by their select, and its rollups and formulas show the values its
// rows add up to. An install that fails part way leaves nothing live; an
// unknown id makes nothing.
// Run: go test -tags=integration ./business/Marketplace/ -run TestBuiltinTableTemplates -v

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	marketplaceModel "github.com/akashc777/OneCamp/models/postgres/Marketplace"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func templatesWorld(t *testing.T) (context.Context, func(name string) *userModels.UserInfo) {
	t.Helper()
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("connect: %v", err)
	}
	return ctx, func(name string) *userModels.UserInfo {
		t.Helper()
		id := uuid.New()
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `
			INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, name+"@example.com", name, name); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		u := &userModels.UserInfo{}
		u.UserPostgresInfo.Id = id
		return u
	}
}

func countOf(t *testing.T, ctx context.Context, q string, args ...any) int {
	t.Helper()
	var n int
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// readTable is a table as its bundle gives it to the member: fields by name,
// and each row's cells by field name, by the row's name.
type readTable struct {
	bundle *dataTableBusiness.TableBundle
	field  map[string]*model.Field
	rows   map[string]map[string]any
	byID   map[string]map[string]any
}

func readAs(t *testing.T, ctx context.Context, id uuid.UUID, who *userModels.UserInfo) readTable {
	t.Helper()
	b, err := dataTableBusiness.GetBundle(ctx, id, dataTableBusiness.Actor{UserID: who.UserPostgresInfo.Id})
	if err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	out := readTable{bundle: b, field: map[string]*model.Field{}, rows: map[string]map[string]any{}, byID: map[string]map[string]any{}}
	for _, f := range b.Fields {
		out.field[f.Name] = f
	}
	for _, r := range b.Rows {
		raw := map[string]any{}
		if err := json.Unmarshal([]byte(r.Values), &raw); err != nil {
			t.Fatalf("row %s: %v", r.Id, err)
		}
		cells := map[string]any{}
		for _, f := range b.Fields {
			cells[f.Name] = raw[f.Id.String()]
		}
		out.rows[fmt.Sprint(cells[b.Fields[0].Name])] = cells
		out.byID[r.Id.String()] = raw
	}
	return out
}

func configOf(t *testing.T, raw string) map[string]any {
	t.Helper()
	c := map[string]any{}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			t.Fatalf("config %q: %v", raw, err)
		}
	}
	return c
}

// refs is a relation cell's links: their row ids and their names.
func refs(cell any) (ids, names []string) {
	list, _ := cell.([]any)
	for _, r := range list {
		m, _ := r.(map[string]any)
		ids = append(ids, fmt.Sprint(m["id"]))
		names = append(names, fmt.Sprint(m["label"]))
	}
	return ids, names
}

type tplValue struct {
	table, row, field string
	want              any
}

// The values each template's rows add up to, through its formulas over
// select labels, its rollups (a count of names, sums, an average) and
// formulas over those.
var tplValues = map[string][]tplValue{
	"CRM": {{"Companies", "Northwind", "Open deal value", 12000.0}, {"Companies", "Lakeside Health", "Open deal value", 5000.0},
		{"Companies", "Bluebird Finance", "Open deal value", 15000.0}, {"Deals", "Annual support", "Open value", 0.0}, {"Deals", "Website redesign", "Open value", 12000.0}},
	"Hiring": {{"Roles", "Backend engineer", "Applicants", 2.0}, {"Roles", "Backend engineer", "Hired", 0.0}, {"Roles", "Backend engineer", "Still to hire", 2.0},
		{"Roles", "Product designer", "Applicants", 2.0}, {"Roles", "Product designer", "Hired", 1.0}, {"Roles", "Product designer", "Still to hire", 0.0},
		{"Roles", "Account executive", "Still to hire", 1.0}, {"Candidates", "Ben Carter", "Score", 4.5}, {"Candidates", "Chloe Kim", "Hired", 1.0}},
	"Content calendar": {{"Campaigns", "Autumn launch", "Pieces", 3.0}, {"Campaigns", "Autumn launch", "Out", 1.0},
		{"Campaigns", "Remote work series", "Pieces", 2.0}, {"Campaigns", "Remote work series", "Out", 0.0}},
	"Stock": {{"Products", "USB-C hub", "In stock", 8.0}, {"Products", "USB-C hub", "Status", "Reorder"},
		{"Products", "27-inch monitor", "In stock", 6.0}, {"Products", "27-inch monitor", "Status", "OK"},
		{"Products", "HDMI cable, 2 m", "In stock", 49.0}, {"Products", "HDMI cable, 2 m", "Status", "OK"}},
	"Budget": {{"Categories", "Software", "Spent", 1380.0}, {"Categories", "Software", "Left", -180.0}, {"Categories", "Software", "Status", "Over budget"},
		{"Categories", "Travel", "Spent", 1790.0}, {"Categories", "Travel", "Left", 1210.0}, {"Categories", "Travel", "Status", "Within budget"}},
}

func TestBuiltinTableTemplatesInstallWhole(t *testing.T) {
	ctx, member := templatesWorld(t)
	today := time.Now()
	for _, b := range BuiltinTableTemplates {
		b := b
		t.Run(b.Name, func(t *testing.T) {
			// The source brings everything it says: no cell or field left out.
			made, err := dataTableBusiness.ImportBaseAs(ctx, dataTableBusiness.Actor{UserID: member("notes-" + b.Glyph).UserPostgresInfo.Id}, b.base(today))
			if err != nil {
				t.Fatalf("import %s: %v", b.Name, err)
			}
			if len(made.Notes) != 0 {
				t.Errorf("%s left things out: %+v", b.Name, made.Notes)
			}

			who := member("member-" + b.Glyph)
			res, err := Install(ctx, b.ID, who, InstallOptions{})
			if err != nil {
				t.Fatalf("install %s: %v", b.Name, err)
			}
			if res.Kind != marketplaceModel.KindTable || res.Name != b.Name {
				t.Errorf("install %s answered %+v", b.Name, res)
			}
			if n := countOf(t, ctx, `SELECT count(*) FROM data_tables WHERE created_by = $1 AND deleted_at IS NULL`, who.UserPostgresInfo.Id); n != len(b.Tables) {
				t.Fatalf("%s made %d tables for the member, want %d", b.Name, n, len(b.Tables))
			}
			tables := map[string]readTable{}
			ids := map[string]uuid.UUID{}
			for _, tb := range b.Tables {
				var id uuid.UUID
				if err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT id FROM data_tables WHERE created_by = $1 AND name = $2 AND deleted_at IS NULL`,
					who.UserPostgresInfo.Id, tb.Name).Scan(&id); err != nil {
					t.Fatalf("%s's table %s: %v", b.Name, tb.Name, err)
				}
				ids[tb.Name] = id
				tables[tb.Name] = readAs(t, ctx, id, who)
			}
			if res.EntityID != ids[b.Tables[0].Name].String() {
				t.Errorf("%s opens %s, not its first table %s", b.Name, res.EntityID, b.Tables[0].Name)
			}

			for _, tb := range b.Tables {
				got := tables[tb.Name]
				if len(got.bundle.Fields) != len(tb.Fields) || len(got.bundle.Rows) != len(tb.Rows) {
					t.Errorf("%s/%s: %d fields %d rows, want %d %d", b.Name, tb.Name, len(got.bundle.Fields), len(got.bundle.Rows), len(tb.Fields), len(tb.Rows))
				}
				// Views by name and kind; a board groups by its select.
				views := map[string]*model.View{}
				for _, v := range got.bundle.Views {
					views[v.Name] = v
				}
				for _, v := range tb.Views {
					mv := views[v.Name]
					if mv == nil || mv.Type != v.Type {
						t.Errorf("%s/%s: view %q (%s) is %+v", b.Name, tb.Name, v.Name, v.Type, mv)
						continue
					}
					if v.GroupBy == "" {
						continue
					}
					display, _ := configOf(t, mv.Config)["display"].(map[string]any)
					group := nameOfKey(tb, v.GroupBy)
					if f := got.field[group]; f == nil || f.Type != model.FieldSelect || display["boardGroup"] != f.Id.String() {
						t.Errorf("%s/%s: board %q groups by %v, want %s", b.Name, tb.Name, v.Name, display["boardGroup"], group)
					}
				}
				for _, f := range tb.Fields {
					mf := got.field[f.Name]
					if mf == nil || mf.Type != f.Type {
						t.Errorf("%s/%s: field %q (%s) is %+v", b.Name, tb.Name, f.Name, f.Type, mf)
						continue
					}
					cfg := configOf(t, mf.Config)
					// A shown format, a rating's top and a select's labels stay.
					if want, ok := f.Config["format"]; ok {
						if fmt.Sprint(cfg["format"]) != fmt.Sprint(want) {
							t.Errorf("%s/%s.%s: format %v, want %v", b.Name, tb.Name, f.Name, cfg["format"], want)
						}
					}
					if want, ok := f.Config["max"]; ok && cfg["max"] != want {
						t.Errorf("%s/%s.%s: max %v, want %v", b.Name, tb.Name, f.Name, cfg["max"], want)
					}
					if f.Type == model.FieldRelation {
						checkLinksBothWays(t, b.Name, tb, f, got, cfg, tables, ids)
					}
				}
				// Every cell as the template wrote it: text, numbers, a select's
				// label, a date as the day so many days from today.
				for _, r := range tb.Rows {
					name := fmt.Sprint(r.Cells[tb.Fields[0].Key])
					cells := got.rows[name]
					if cells == nil {
						t.Errorf("%s/%s: no row %q", b.Name, tb.Name, name)
						continue
					}
					for key, v := range r.Cells {
						f := nameOfKey(tb, key)
						want := v
						if d, ok := v.(relDay); ok {
							want = today.AddDate(0, 0, int(d)).Format("2006-01-02")
						}
						if got := cellText(cells[f], got.field[f]); got != fmt.Sprint(want) {
							t.Errorf("%s/%s %q.%s = %v, want %v", b.Name, tb.Name, name, f, cells[f], want)
						}
					}
				}
			}
			for _, v := range tplValues[b.Name] {
				cells := tables[v.table].rows[v.row]
				if cells == nil || fmt.Sprint(cells[v.field]) != fmt.Sprint(v.want) {
					t.Errorf("%s: %s %q.%s = %v, want %v", b.Name, v.table, v.row, v.field, cells[v.field], v.want)
				}
			}
		})
	}
}

func nameOfKey(tb TemplateTable, key string) string {
	for _, f := range tb.Fields {
		if f.Key == key {
			return f.Name
		}
	}
	return key
}

// cellText is a stored cell as the template wrote it: a select by its label,
// a date by its day.
func cellText(cell any, f *model.Field) string {
	if f != nil && f.Type == model.FieldSelect {
		var c struct {
			Options []struct{ ID, Label string } `json:"options"`
		}
		_ = json.Unmarshal([]byte(f.Config), &c)
		for _, o := range c.Options {
			if o.ID == fmt.Sprint(cell) {
				return o.Label
			}
		}
	}
	s := fmt.Sprint(cell)
	if f != nil && f.Type == model.FieldDate && len(s) > 10 {
		s = s[:10]
	}
	return s
}

// checkLinksBothWays: each link the template wrote is there, and the linked
// row shows it back on the relation's other side.
func checkLinksBothWays(t *testing.T, tpl string, tb TemplateTable, f dataTableBusiness.ImportField, got readTable, cfg map[string]any,
	tables map[string]readTable, ids map[string]uuid.UUID) {
	t.Helper()
	var other readTable
	for name, id := range ids {
		if id.String() == fmt.Sprint(cfg["table_id"]) {
			other = tables[name]
		}
	}
	if other.bundle == nil {
		t.Errorf("%s/%s.%s links to %v, not a table of the template", tpl, tb.Name, f.Name, cfg["table_id"])
		return
	}
	back := ""
	for _, of := range other.bundle.Fields {
		oc := configOf(t, of.Config)
		mine := got.field[f.Name].Id.String()
		if of.Type == model.FieldRelation && (oc["inverse_of"] == mine || oc["inverse"] == mine) {
			back = of.Id.String()
		}
	}
	if back == "" {
		t.Errorf("%s/%s.%s: the linked table has no other side", tpl, tb.Name, f.Name)
		return
	}
	for _, r := range tb.Rows {
		name := fmt.Sprint(r.Cells[tb.Fields[0].Key])
		var rowID string
		for _, br := range got.bundle.Rows {
			if got.byID[br.Id.String()][got.bundle.Fields[0].Id.String()] == name {
				rowID = br.Id.String()
			}
		}
		linked, names := refs(got.rows[name][f.Name])
		if want := len(r.Links[f.Key]); want > 0 && len(linked) != want {
			t.Errorf("%s/%s %q.%s links %v, want %d", tpl, tb.Name, name, f.Name, names, want)
		}
		for i, id := range linked {
			backIDs, _ := refs(other.byID[id][back])
			if !strings.Contains(strings.Join(backIDs, ","), rowID) {
				t.Errorf("%s/%s %q.%s -> %q doesn't link back", tpl, tb.Name, name, f.Name, names[i])
			}
		}
	}
}

// An install that fails at its last table (its rows refused) takes back every
// table it made: none of them, nor a field, row or view of theirs, is live.
func TestBuiltinTableTemplatesFailingInstallLeavesNothing(t *testing.T) {
	ctx, member := templatesWorld(t)
	crm := BuiltinTableTemplates[0]
	last := crm.Tables[len(crm.Tables)-1].Name
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `
		CREATE FUNCTION tpl_refuse_last() RETURNS trigger AS $$ BEGIN
			IF EXISTS (SELECT 1 FROM data_tables WHERE id = NEW.table_id AND name = '`+last+`') THEN
				RAISE EXCEPTION 'the last table refuses its rows';
			END IF;
			RETURN NEW;
		END $$ LANGUAGE plpgsql;
		CREATE TRIGGER tpl_refuse_last BEFORE INSERT ON data_table_rows FOR EACH ROW EXECUTE PROCEDURE tpl_refuse_last();`); err != nil {
		t.Fatalf("trigger: %v", err)
	}
	who := member("unlucky")
	if _, err := Install(ctx, crm.ID, who, InstallOptions{}); err == nil {
		t.Fatal("an install whose last table refuses its rows succeeded")
	}
	id := who.UserPostgresInfo.Id
	if n := countOf(t, ctx, `SELECT count(*) FROM data_tables WHERE created_by = $1`, id); n != len(crm.Tables) {
		t.Errorf("the failed install made %d tables before failing, want %d (the failure fires at the last)", n, len(crm.Tables))
	}
	if n := countOf(t, ctx, `SELECT count(*) FROM data_tables WHERE created_by = $1 AND deleted_at IS NULL`, id); n != 0 {
		t.Errorf("a failed install left %d tables live", n)
	}
	for _, kind := range []string{"data_table_fields", "data_table_rows", "data_table_views"} {
		if n := countOf(t, ctx, `SELECT count(*) FROM `+kind+` x JOIN data_tables t ON t.id = x.table_id
			WHERE t.created_by = $1 AND x.deleted_at IS NULL AND t.deleted_at IS NULL`, id); n != 0 {
			t.Errorf("a failed install left %d %s live", n, kind)
		}
	}
}

// An id that is no template makes nothing.
func TestBuiltinTableTemplatesUnknownIDMakesNothing(t *testing.T) {
	ctx, member := templatesWorld(t)
	who := member("curious")
	if _, err := Install(ctx, uuid.New(), who, InstallOptions{}); !IsNotFound(err) {
		t.Errorf("an unknown template: %v, want not found", err)
	}
	if n := countOf(t, ctx, `SELECT count(*) FROM data_tables WHERE created_by = $1`, who.UserPostgresInfo.Id); n != 0 {
		t.Errorf("an unknown template made %d tables", n)
	}
}

// OneCamp's table templates are in the tables list and the whole list, in
// their order, and never in the doc list; each opens by its id, as OneCamp's
// with the tables it makes; and no one, an admin either, can remove one.
func TestBuiltinTableTemplatesAreListedOpenedAndKept(t *testing.T) {
	ctx, member := templatesWorld(t)
	admin := member("keeper")
	admin.UserPostgresInfo.IsAdmin = true
	var want []string
	builtin := map[uuid.UUID]bool{}
	for _, b := range BuiltinTableTemplates {
		want = append(want, b.ID.String())
		builtin[b.ID] = true
	}
	if len(want) != 5 {
		t.Fatalf("%d built-in table templates, want 5", len(want))
	}
	listed := func(kind string) string {
		t.Helper()
		items, err := List(ctx, kind)
		if err != nil {
			t.Fatalf("list %q: %v", kind, err)
		}
		var ids []string
		for _, it := range items {
			if !builtin[it.Id] {
				continue
			}
			var p tableTemplatePayload
			if err := json.Unmarshal([]byte(it.Payload), &p); err != nil || !p.Builtin || it.Kind != marketplaceModel.KindTable {
				t.Errorf("list %q: %s reads kind %q, payload %s", kind, it.Name, it.Kind, it.Payload)
			}
			ids = append(ids, it.Id.String())
		}
		return strings.Join(ids, " ")
	}
	for _, kind := range []string{marketplaceModel.KindTable, ""} {
		if got := listed(kind); got != strings.Join(want, " ") {
			t.Errorf("list %q holds the built-ins [%s], want [%s]", kind, got, strings.Join(want, " "))
		}
	}
	if got := listed(KindDoc); got != "" {
		t.Errorf("the doc list holds table templates [%s]", got)
	}
	for _, b := range BuiltinTableTemplates {
		var wantTables []string
		for _, tb := range b.Tables {
			wantTables = append(wantTables, tb.Name)
		}
		got, err := Get(ctx, b.ID)
		if err != nil {
			t.Fatalf("get %s: %v", b.Name, err)
		}
		var p tableTemplatePayload
		if err := json.Unmarshal([]byte(got.Payload), &p); err != nil {
			t.Fatalf("get %s: payload %s: %v", b.Name, got.Payload, err)
		}
		var tables []string
		for _, l := range p.Tables {
			tables = append(tables, l.Name)
		}
		if got.Id != b.ID || got.Kind != marketplaceModel.KindTable || got.CreatedBy != uuid.Nil || !p.Builtin ||
			len(tables) == 0 || strings.Join(tables, ", ") != strings.Join(wantTables, ", ") {
			t.Errorf("get %s: id %s kind %q created_by %s builtin %v tables [%s], want %s, table, nil, true, [%s]",
				b.Name, got.Id, got.Kind, got.CreatedBy, p.Builtin, strings.Join(tables, ", "), b.ID, strings.Join(wantTables, ", "))
		}
		if err := Unpublish(ctx, b.ID, admin); !errors.Is(err, errBuiltinTemplate) {
			t.Errorf("an admin removing %s: %v, want %q", b.Name, err, errBuiltinTemplate)
		}
		if after, err := Get(ctx, b.ID); err != nil || after.Id != b.ID {
			t.Errorf("get %s after the removal was refused: %v, %v", b.Name, after, err)
		}
	}
	if got := listed(marketplaceModel.KindTable); got != strings.Join(want, " ") {
		t.Errorf("after the removals were refused, the tables list holds the built-ins [%s], want [%s]", got, strings.Join(want, " "))
	}
}
