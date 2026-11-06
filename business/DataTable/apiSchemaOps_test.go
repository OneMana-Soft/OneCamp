package business

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	v1api "github.com/akashc777/OneCamp/business/PublicAPI"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

// Tables H2b: a table's fields and views are read, made, changed and deleted
// through the public API one at a time, and in batches that take a key, as
// declared once here (REST in both editions, MCP tools in the AI edition).
func TestTheFieldsAndViewsOpsArePublished(t *testing.T) {
	byName := map[string]v1api.Op{}
	for _, op := range APIOps() {
		byName[op.Name] = op
	}
	read, write := apiTokenBusiness.ScopeTablesRead, apiTokenBusiness.ScopeTablesWrite
	for _, c := range []struct{ name, method, path, scope string }{
		{"tables.fields.list", http.MethodGet, "/tables/{id}/fields", read},
		{"tables.field.create", http.MethodPost, "/tables/{id}/fields", write},
		{"tables.field.update", http.MethodPatch, "/tables/{id}/fields/{field}", write},
		{"tables.field.delete", http.MethodDelete, "/tables/{id}/fields/{field}", write},
		{"tables.fields.batch", http.MethodPost, "/tables/{id}/fields/batch", write},
		{"tables.views.list", http.MethodGet, "/tables/{id}/views", read},
		{"tables.view.create", http.MethodPost, "/tables/{id}/views", write},
		{"tables.view.update", http.MethodPatch, "/tables/{id}/views/{view}", write},
		{"tables.view.delete", http.MethodDelete, "/tables/{id}/views/{view}", write},
		{"tables.views.batch", http.MethodPost, "/tables/{id}/views/batch", write},
	} {
		op, ok := byName[c.name]
		if !ok {
			t.Errorf("%s isn't published", c.name)
			continue
		}
		if op.Method != c.method || op.Path != c.path || op.Scope != c.scope || op.Behaviour.ReadOnly != (c.scope == read) || op.Reach == nil {
			t.Errorf("%s is %s %s (%s, read-only %v), want %s %s (%s)", c.name, op.Method, op.Path, op.Scope, op.Behaviour.ReadOnly, c.method, c.path, c.scope)
		}
		var in struct {
			Required   []string                   `json:"required"`
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(op.Input, &in); err != nil {
			t.Errorf("%s's Input isn't JSON: %v", c.name, err)
		}
		// A batch takes a key, and is not all-or-nothing: its Summary says the
		// check is whole and the apply is item by item (ruling (i), 4).
		if batch := op.Path[len(op.Path)-6:] == "/batch"; batch != slices.Contains(in.Required, "key") || (batch && (!op.Behaviour.Idempotent ||
			op.Cost == nil || op.Cost(map[string]any{"create": []any{1}, "update": []any{1, 2}, "delete": []any{1}}) != 4)) {
			t.Errorf("%s: batch %v, key required %v, idempotent %v; a batch takes a key and spends one a item", c.name, batch,
				slices.Contains(in.Required, "key"), op.Behaviour.Idempotent)
		}
	}
	for _, name := range []string{"tables.fields.batch", "tables.views.batch"} {
		if s := byName[name].Summary; !strings.Contains(s, "checked first") || !strings.Contains(s, "one by one") {
			t.Errorf("%s's Summary %q doesn't say the check is whole and the apply item by item", name, s)
		}
	}
	// The trash brings views back too, and charges what it restores.
	restore := byName["tables.trash.restore"]
	if !strings.Contains(string(restore.Input), `"views"`) || restore.Cost(map[string]any{"rows": []any{1}, "fields": []any{1, 2}, "views": []any{1, 2, 3}}) != 6 {
		t.Errorf("tables.trash.restore takes no views, or doesn't charge them: %s", restore.Input)
	}
}

// A field or a view named twice in a batch's updates and deletes is refused
// at its second naming, in the batch's check; the first is checked as ever.
func TestAFieldNamedTwiceInABatchIsRefused(t *testing.T) {
	id := uuid.New()
	checked := 0
	step := func(list string) batchStep {
		return batchStep{item: batchItem{List: list, ID: id.String()}, check: func(context.Context) error { checked++; return nil }}
	}
	seen := map[uuid.UUID]bool{}
	first, second := namedOnce(step("update"), seen, "field"), namedOnce(step("delete"), seen, "field")
	var bad *inputError
	if err := first.check(context.Background()); err != nil || checked != 1 {
		t.Errorf("the first naming: %v (checked %d), want it checked as ever", err, checked)
	}
	if err := second.check(context.Background()); !errors.As(err, &bad) || checked != 1 {
		t.Errorf("the second naming: %v (checked %d), want it refused unchecked", err, checked)
	}
}

// A fields or views op's refusal is the API's: the app's own refusal of what
// was sent is the caller's to fix in its words (the app's controller answers
// it 400), a store failure the server's (503), a field or view it hasn't got
// a 404, and a locked view a 409.
func TestAFieldsOpSaysTheAppsRefusalInItsWords(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
		code   string
		words  string
	}{
		{errors.New("field name is too long"), http.StatusBadRequest, "invalid_argument", "field name is too long"},
		{errPrimaryDeleted, http.StatusBadRequest, "invalid_argument", errPrimaryDeleted.Error()},
		{storeFailed("failed to update field"), http.StatusServiceUnavailable, "unavailable", "the table couldn't be read or changed just now"},
		{errFieldNotFound, http.StatusNotFound, "not_found", "there is no such field"},
		{errViewNotFound, http.StatusNotFound, "not_found", "there is no such view"},
		{errViewLocked, http.StatusConflict, "locked", errViewLocked.Error()},
	} {
		e := asAPIError(t, schemaErr(context.Background(), uuid.Nil, Actor{}, c.err))
		if e.Status != c.status || e.Code != c.code || e.Message != c.words {
			t.Errorf("%v answered %d %s %q, want %d %s %q", c.err, e.Status, e.Code, e.Message, c.status, c.code, c.words)
		}
	}
}

// A view's update through the API changes what it names and keeps the rest
// as stored, since the app saves a view whole.
func TestAViewsUpdateKeepsWhatItLeavesOut(t *testing.T) {
	stored := &model.View{Name: "Roadmap", Type: model.ViewBoard, Config: `{"group_by":"stage"}`, Position: 3}
	name := "Launch"
	in := viewPatch{Name: &name}.onto(stored)
	if in.Name != "Launch" || in.Type != model.ViewBoard || in.Position != 3 || in.Config["group_by"] != "stage" {
		t.Errorf("a rename leaves the view %+v; want its kind, place and settings kept", in)
	}
	in = viewPatch{Config: map[string]any{"hidden": []any{"f1"}}}.onto(stored)
	if in.Name != "Roadmap" || in.Config["hidden"] == nil || in.Config["group_by"] != nil {
		t.Errorf("new settings leave the view %+v; want the settings sent, whole", in)
	}
}

// An update's audit row names what its write changed, read from the field
// as the write found it and as it left it (main's audit rule, 3 Nov: from
// the stored result). A write that left it as it was changes nothing, in
// whatever spacing and key order jsonb gives its config back, so it writes
// no row.
func TestAFieldsChangesAreReadFromWhatWasStored(t *testing.T) {
	desc := "What it costs"
	was := &model.Field{Name: "Budget", Type: "number", Config: `{"precision": 2, "format": "currency"}`, Position: 1, Description: &desc}
	same := *was
	same.Config = `{"format":"currency","precision":2}`
	if got := fieldChanges(was, &same); len(got) != 0 {
		t.Errorf("a field written as it was changed %v; want nothing", got)
	}
	renamed := same
	renamed.Name, renamed.Primary = "Cost", true
	if got := fieldChanges(was, &renamed); !slices.Equal(got, []string{"name", "primary"}) {
		t.Errorf("a field renamed and made primary changed %v; want name and primary", got)
	}
	if got := fieldChanges(&model.Field{Name: "Notes", Type: "text"}, &model.Field{Name: "Notes", Type: "text", Config: "{}"}); len(got) != 0 {
		t.Errorf("an empty config written as {} changed %v; want nothing", got)
	}
	v := &model.View{Name: "Board", Type: "board", Config: `{"group_by": "status"}`}
	if got := viewChanges(v, &model.View{Name: "Board", Type: "board", Config: `{"group_by":"status"}`}); len(got) != 0 {
		t.Errorf("a view written as it was changed %v; want nothing", got)
	}
	if got := viewChanges(v, &model.View{Name: "Plan", Type: "board", Config: `{"group_by":"owner"}`}); !slices.Equal(got, []string{"name", "config"}) {
		t.Errorf("a view renamed and regrouped changed %v; want name and config", got)
	}
}
