package business

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	v1api "github.com/akashc777/OneCamp/business/PublicAPI"
	"github.com/akashc777/OneCamp/helpers/idempotency"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

// The JSON Schema pieces the fields and views ops share (static text, as
// Input must be; the numbers are the app's own limits).
var (
	fieldArg = `"field":{"type":"string","maxLength":64,"description":"The field's id."}`
	viewArg  = `"view":{"type":"string","maxLength":64,"description":"The view's id."}`
	keyArg   = fmt.Sprintf(`"key":{"type":"string","minLength":1,"maxLength":%d,"description":"Names this batch: the same batch sent again with the same key skips the items it did and answers them as before. A new batch takes a new key."}`, idempotency.MaxKey)
	// fieldProps are a field's settings, as a create sets them and an update
	// changes them (what an update leaves out keeps its value).
	fieldProps = fmt.Sprintf(`"name":{"type":"string","maxLength":%d,"description":"The field's name."},
		"type":{"type":"string","maxLength":32,"description":"Its kind (text, number, select, date, checkbox, formula, relation, rollup and the rest the reference lists); text when a create leaves it out."},
		"config":{"type":"object","additionalProperties":{},"description":"Its settings as its kind reads them: a select's options, a number's format, a formula's text, a link's table."},
		"description":{"type":"string","maxLength":%d,"description":"What the field is for."},
		"required":{"type":"boolean","description":"Whether every row made through the API, an AI tool or a form must give it."},
		"default_value":{"description":"What a new row's cell starts as; null is none."},
		"position":{"type":"number","description":"Its place among the columns: lower is further left."}`, maxNameLen, maxDescriptionLen)
	// viewProps are a view's settings (a create's; an update changes those it
	// names).
	viewProps = fmt.Sprintf(`"name":{"type":"string","maxLength":%d,"description":"The view's name."},
		"type":{"type":"string","maxLength":32,"description":"grid, board, calendar, chart, gallery, form, timeline or list; grid when a create leaves it out."},
		"config":{"type":"object","additionalProperties":{},"description":"Its settings: filter, sorts, groups, hidden fields and widths, as the app keeps them."},
		"position":{"type":"number","description":"Its place among the table's views."}`, maxNameLen)
	fieldUpdateProps = fieldArg + `,` + fieldProps + `,
		"primary":{"type":"boolean","description":"true makes it the field naming the table's rows; another field becoming primary is how one stops being it."},
		"decimal":{"type":"string","maxLength":1,"description":"How text in a column made numbers writes its decimal sign; a point when left out."},
		"group":{"type":"string","maxLength":1,"description":"How such text writes the sign between thousands; a comma when left out."}`
	viewCreateProps = viewProps + `,
		"personal":{"type":"boolean","description":"true makes it the caller's own view, which anyone who reads the table may make; a shared view is its managers' to make."}`
)

// schemaOps are the tables ops on a table's fields and views (tables H2b):
// each listed, made, changed and deleted one at a time, and several at once
// in a batch that is checked whole, then applied item by item (apiBatch.go).
// Each Serve calls the app's own function as the token's member (condition
// c); a delete is soft: a field or a view waits in the table's trash (a).
func schemaOps() []v1api.Op {
	return []v1api.Op{{
		Name:    "tables.fields.list",
		Summary: "A table's fields: each one's id, name, kind, settings and place",
		Method:  http.MethodGet, Path: "/tables/{id}/fields", Scope: apiTokenBusiness.ScopeTablesRead,
		Behaviour: v1api.Behaviour{ReadOnly: true, Idempotent: true},
		Input:     json.RawMessage(`{"type":"object","properties":{` + tableArg + `},"required":["id"]}`),
		Reach:     tableReach,
		Serve:     serveFieldsList,
	}, {
		Name:    "tables.field.create",
		Summary: "Add a field to a table, for those who manage it; the answer is the field",
		Method:  http.MethodPost, Path: "/tables/{id}/fields", Scope: apiTokenBusiness.ScopeTablesWrite,
		Behaviour: v1api.Behaviour{Recoverable: true},
		UndoneBy:  []string{"tables.field.delete"},
		Input:     json.RawMessage(`{"type":"object","properties":{` + tableArg + `,` + fieldProps + `},"required":["id","name"]}`),
		Reach:     tableReach,
		Serve:     serveFieldCreate,
	}, {
		Name:    "tables.field.update",
		Summary: "Change a field's settings, and its cells with its kind or options; what's left out stays as it is",
		Method:  http.MethodPatch, Path: "/tables/{id}/fields/{field}", Scope: apiTokenBusiness.ScopeTablesWrite,
		Behaviour: v1api.Behaviour{Idempotent: true, Recoverable: true},
		// The settings are set back by another update, the cells it rewrote
		// from each row's history (one change set).
		UndoneBy: []string{"tables.field.update", "tables.row.history", "tables.row.update"},
		Input:    json.RawMessage(`{"type":"object","properties":{` + tableArg + `,` + fieldUpdateProps + `},"required":["id","field"]}`),
		Reach:    tableReach,
		Serve:    serveFieldUpdate,
	}, {
		Name:    "tables.field.delete",
		Summary: "Move a field to its table's trash, with its cells, from where it can be brought back",
		Method:  http.MethodDelete, Path: "/tables/{id}/fields/{field}", Scope: apiTokenBusiness.ScopeTablesWrite,
		Behaviour: v1api.Behaviour{Destructive: true, Idempotent: true, Recoverable: true},
		UndoneBy:  []string{"tables.trash.restore"},
		Input:     json.RawMessage(`{"type":"object","properties":{` + tableArg + `,` + fieldArg + `},"required":["id","field"]}`),
		Reach:     tableReach,
		Serve:     serveFieldDelete,
	}, {
		Name: "tables.fields.batch",
		Summary: "Add, change and delete a table's fields in one call: every item is checked first and one refused refuses the call; " +
			"then they are applied one by one in order, so a failure part way names the items done, and the same call sent again with its key does the rest",
		Method: http.MethodPost, Path: "/tables/{id}/fields/batch", Scope: apiTokenBusiness.ScopeTablesWrite,
		// Its key makes it idempotent; deleted fields come back from the trash.
		Behaviour: v1api.Behaviour{Destructive: true, Idempotent: true, Recoverable: true},
		UndoneBy:  []string{"tables.field.delete", "tables.field.update", "tables.row.history", "tables.row.update", "tables.trash.restore"},
		Input: json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{`+tableArg+`,`+keyArg+`,
			"create":{"type":"array","maxItems":%[1]d,"description":"Fields to add, applied first.","items":{"type":"object","properties":{`+fieldProps+`},"required":["name"]}},
			"update":{"type":"array","maxItems":%[1]d,"description":"Fields to change, applied next.","items":{"type":"object","properties":{`+fieldUpdateProps+`},"required":["field"]}},
			"delete":{"type":"array","maxItems":%[1]d,"items":{"type":"string","maxLength":64},"description":"Fields to move to the trash, applied last."}},
			"required":["id","key"]}`, maxSchemaBatch)),
		Reach: tableReach,
		Cost:  countOf("create", "update", "delete"),
		Serve: serveFieldsBatch,
	}, {
		Name:    "tables.views.list",
		Summary: "A table's views the caller sees: the shared ones and their own, each with its settings",
		Method:  http.MethodGet, Path: "/tables/{id}/views", Scope: apiTokenBusiness.ScopeTablesRead,
		Behaviour: v1api.Behaviour{ReadOnly: true, Idempotent: true},
		Input:     json.RawMessage(`{"type":"object","properties":{` + tableArg + `},"required":["id"]}`),
		Reach:     tableReach,
		Serve:     serveViewsList,
	}, {
		Name:    "tables.view.create",
		Summary: "Add a view to a table: one of the caller's own, or a shared one for those who manage it; the answer is the view",
		Method:  http.MethodPost, Path: "/tables/{id}/views", Scope: apiTokenBusiness.ScopeTablesWrite,
		Behaviour: v1api.Behaviour{Recoverable: true},
		UndoneBy:  []string{"tables.view.delete"},
		Input:     json.RawMessage(`{"type":"object","properties":{` + tableArg + `,` + viewCreateProps + `},"required":["id"]}`),
		Reach:     tableReach,
		Serve:     serveViewCreate,
	}, {
		Name:    "tables.view.update",
		Summary: "Change a view's name, kind, settings or place; what's left out stays as it is, and a locked view is refused",
		Method:  http.MethodPatch, Path: "/tables/{id}/views/{view}", Scope: apiTokenBusiness.ScopeTablesWrite,
		Behaviour: v1api.Behaviour{Idempotent: true, Recoverable: true},
		UndoneBy:  []string{"tables.view.update"},
		Input:     json.RawMessage(`{"type":"object","properties":{` + tableArg + `,` + viewArg + `,` + viewProps + `},"required":["id","view"]}`),
		Reach:     tableReach,
		Serve:     serveViewUpdate,
	}, {
		Name:    "tables.view.delete",
		Summary: "Move a view to its table's trash, from where it can be brought back; a locked view is refused",
		Method:  http.MethodDelete, Path: "/tables/{id}/views/{view}", Scope: apiTokenBusiness.ScopeTablesWrite,
		Behaviour: v1api.Behaviour{Destructive: true, Idempotent: true, Recoverable: true},
		UndoneBy:  []string{"tables.trash.restore"},
		Input:     json.RawMessage(`{"type":"object","properties":{` + tableArg + `,` + viewArg + `},"required":["id","view"]}`),
		Reach:     tableReach,
		Serve:     serveViewDelete,
	}, {
		Name: "tables.views.batch",
		Summary: "Add, change and delete a table's views in one call: every item is checked first and one refused refuses the call; " +
			"then they are applied one by one in order, so a failure part way names the items done, and the same call sent again with its key does the rest",
		Method: http.MethodPost, Path: "/tables/{id}/views/batch", Scope: apiTokenBusiness.ScopeTablesWrite,
		Behaviour: v1api.Behaviour{Destructive: true, Idempotent: true, Recoverable: true},
		UndoneBy:  []string{"tables.trash.restore", "tables.view.delete", "tables.view.update"},
		Input: json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{`+tableArg+`,`+keyArg+`,
			"create":{"type":"array","maxItems":%[1]d,"description":"Views to add, applied first.","items":{"type":"object","properties":{`+viewCreateProps+`}}},
			"update":{"type":"array","maxItems":%[1]d,"description":"Views to change, applied next.","items":{"type":"object","properties":{`+viewArg+`,`+viewProps+`},"required":["view"]}},
			"delete":{"type":"array","maxItems":%[1]d,"items":{"type":"string","maxLength":64},"description":"Views to move to the trash, applied last."}},
			"required":["id","key"]}`, maxSchemaBatch)),
		Reach: tableReach,
		Cost:  countOf("create", "update", "delete"),
		Serve: serveViewsBatch,
	}}
}

// schemaErr is a fields or views op's refusal in the API's one error body:
// apiErr's, a table the member can open but not change said as forbidden
// (one they can't open is one that isn't there), and the app's own refusal of
// what was sent (a plain error: a name too long, a kind it hasn't got) the
// caller's to fix in the app's words, as its controller answers it (400).
func schemaErr(ctx context.Context, table uuid.UUID, actor Actor, err error) error {
	if errors.Is(err, errForbidden) {
		if open, _ := ViewableBy(ctx, actor, table); open {
			return apiErr(err)
		}
	}
	err = apiErr(unseen(err))
	if errors.As(err, new(*v1api.Error)) {
		return err
	}
	return v1api.Errorf(http.StatusBadRequest, "invalid_argument", "", "%s", err.Error())
}

// schemaCall is the member a fields or views call acts as (with the API's or
// the agent's kind, as the row ledger records it), and the table it names.
func schemaCall(ctx context.Context, call v1api.Call) (Actor, uuid.UUID, error) {
	member, table, err := tableCall(ctx, call)
	return actorBy(apiBy(ctx, member)), table, err
}

func serveFieldsList(ctx context.Context, call v1api.Call) (any, error) {
	actor, table, err := schemaCall(ctx, call)
	if err != nil {
		return nil, err
	}
	fields, err := ListTableFields(ctx, table, actor)
	if err != nil {
		return nil, schemaErr(ctx, table, actor, err)
	}
	return v1api.Page[*model.Field]{Items: fields}, nil
}

// serveOne serves a single fields or views write: the step its batch would
// run, applied alone with its audit row (applyItem; the app's own function
// checks it).
func serveOne(ctx context.Context, call v1api.Call, op string, step func(Actor, uuid.UUID) (batchStep, error)) (any, error) {
	actor, table, err := schemaCall(ctx, call)
	if err != nil {
		return nil, err
	}
	s, err := step(actor, table)
	if err != nil {
		return nil, schemaErr(ctx, table, actor, err)
	}
	v, err := applyItem(ctx, s, func(ctx context.Context, s batchStep, done itemDone, action string) error {
		return auditItem(ctx, actor, op, "", table, s, done, action)
	})
	if err != nil {
		return nil, schemaErr(ctx, table, actor, err)
	}
	return v, nil
}

func serveFieldCreate(ctx context.Context, call v1api.Call) (any, error) {
	return serveOne(ctx, call, "tables.field.create", func(actor Actor, table uuid.UUID) (batchStep, error) {
		return fieldCreate(actor, table, nil, call.Args, 0, uuid.Nil)
	})
}

func serveFieldUpdate(ctx context.Context, call v1api.Call) (any, error) {
	return serveOne(ctx, call, "tables.field.update", func(actor Actor, table uuid.UUID) (batchStep, error) {
		return fieldUpdate(actor, table, nil, call.Args, 0)
	})
}

func serveFieldDelete(ctx context.Context, call v1api.Call) (any, error) {
	return serveOne(ctx, call, "tables.field.delete", func(actor Actor, table uuid.UUID) (batchStep, error) {
		id, err := idArg(call.Args, "field")
		return fieldDelete(actor, table, nil, id, 0), err
	})
}

func serveViewsList(ctx context.Context, call v1api.Call) (any, error) {
	actor, table, err := schemaCall(ctx, call)
	if err != nil {
		return nil, err
	}
	if err := CanView(ctx, table, actor); err != nil {
		return nil, schemaErr(ctx, table, actor, err)
	}
	views, err := model.ListViews(ctx, table, actor.UserID)
	if err != nil {
		return nil, schemaErr(ctx, table, actor, errViewsUnavailable)
	}
	return v1api.Page[*model.View]{Items: views}, nil
}

func serveViewCreate(ctx context.Context, call v1api.Call) (any, error) {
	return serveOne(ctx, call, "tables.view.create", func(actor Actor, table uuid.UUID) (batchStep, error) {
		return viewCreate(actor, table, nil, call.Args, 0, uuid.Nil)
	})
}

func serveViewUpdate(ctx context.Context, call v1api.Call) (any, error) {
	return serveOne(ctx, call, "tables.view.update", func(actor Actor, table uuid.UUID) (batchStep, error) {
		return viewUpdate(actor, table, nil, call.Args, 0)
	})
}

func serveViewDelete(ctx context.Context, call v1api.Call) (any, error) {
	return serveOne(ctx, call, "tables.view.delete", func(actor Actor, table uuid.UUID) (batchStep, error) {
		id, err := idArg(call.Args, "view")
		return viewDelete(actor, table, nil, id, 0), err
	})
}

// fieldsRead is what a fields batch checks its items against, read before
// any is checked, each in one read: the table, its fields, the tables they
// and the items link to (linkedTables), and which of its creates' ids the
// table holds already. A single op has none: its write's own function
// checks it.
type fieldsRead struct {
	t      *model.DataTable
	fields []*model.Field
	linked *linkedTables
	held   map[uuid.UUID]bool
}

func serveFieldsBatch(ctx context.Context, call v1api.Call) (any, error) {
	return serveBatch(ctx, call, "tables.fields.batch", func(ctx context.Context, actor Actor, table uuid.UUID, keys batchKeys, creates, updates []map[string]any, deletes []uuid.UUID) ([]batchStep, error) {
		t, err := loadManageable(ctx, table, actor)
		if err != nil {
			return nil, err
		}
		r := &fieldsRead{t: t, linked: newLinked()}
		if r.fields, err = model.ListFields(ctx, table); err != nil {
			return nil, storeFailed("failed to load fields")
		}
		if err := r.linked.preload(ctx, batchLinks(r.fields, creates, updates)); err != nil {
			return nil, err
		}
		if r.held, err = model.HeldFields(ctx, table, keys.createIDs(len(creates))); err != nil {
			return nil, storeFailed("failed to read the table")
		}
		var steps []batchStep
		for i, c := range creates {
			s, err := fieldCreate(actor, table, r, c, i, keys.createID("create", i))
			if err != nil {
				return nil, err
			}
			steps = append(steps, s)
		}
		seen := map[uuid.UUID]bool{}
		for i, u := range updates {
			s, err := fieldUpdate(actor, table, r, u, i)
			if err != nil {
				return nil, err
			}
			steps = append(steps, namedOnce(s, seen, "field"))
		}
		for i, id := range deletes {
			steps = append(steps, namedOnce(fieldDelete(actor, table, r, id, i), seen, "field"))
		}
		return steps, nil
	})
}

// batchLinks is the tables a fields batch's checks can read, named before
// any is checked: those its relations link to, and, when an item is a
// formula or a rollup, those the table's relation fields link to. A rollup's
// chain past them is read as it's walked (linkedTables.tableAt).
func batchLinks(fields []*model.Field, creates, updates []map[string]any) []uuid.UUID {
	var ids []uuid.UUID
	reads := false
	for _, item := range append(append([]map[string]any{}, creates...), updates...) {
		typ, _ := item["type"].(string)
		var was *model.Field
		if id, err := uuid.Parse(fmt.Sprint(item["field"])); err == nil {
			was = fieldOf(fields, id)
		}
		if typ == "" && was != nil {
			typ = was.Type
		}
		switch typ {
		case model.FieldFormula, model.FieldRollup:
			reads = true
		case model.FieldRelation:
			cfg, _ := item["config"].(map[string]any)
			if id, err := uuid.Parse(fmt.Sprint(cfg["table_id"])); err == nil {
				ids = append(ids, id)
			}
			if l, ok := linkOf(was); was != nil && ok {
				ids = append(ids, l.table)
			}
		}
	}
	if reads {
		for _, f := range fields {
			if l, ok := linkOf(f); ok {
				ids = append(ids, l.table)
			}
		}
	}
	return ids
}

// viewsRead is what a views batch checks its items against, read before any
// is checked, each in one read: the table, if its caller opens it, the live
// views its updates and deletes name, and which of its creates' ids the
// table holds already. A single op has none: its write's own function
// checks it.
type viewsRead struct {
	t     *model.DataTable
	views map[uuid.UUID]*model.View
	held  map[uuid.UUID]bool
}

func serveViewsBatch(ctx context.Context, call v1api.Call) (any, error) {
	return serveBatch(ctx, call, "tables.views.batch", func(ctx context.Context, actor Actor, table uuid.UUID, keys batchKeys, creates, updates []map[string]any, deletes []uuid.UUID) ([]batchStep, error) {
		// A view is its maker's own, or shared by the table's managers: each
		// item asks its own rule of a table its caller opens.
		t, err := loadViewable(ctx, table, actor)
		if err != nil {
			return nil, err
		}
		r := &viewsRead{t: t, views: map[uuid.UUID]*model.View{}}
		named := append([]uuid.UUID{}, deletes...)
		for _, u := range updates {
			if id, err := uuid.Parse(fmt.Sprint(u["view"])); err == nil {
				named = append(named, id)
			}
		}
		views, err := model.ViewsByIDs(ctx, table, named)
		if err != nil {
			return nil, errViewsUnavailable
		}
		for _, v := range views {
			r.views[v.Id] = v
		}
		if r.held, err = model.HeldViews(ctx, table, keys.createIDs(len(creates))); err != nil {
			return nil, storeFailed("failed to read the table")
		}
		var steps []batchStep
		for i, c := range creates {
			s, err := viewCreate(actor, table, r, c, i, keys.createID("create", i))
			if err != nil {
				return nil, err
			}
			steps = append(steps, s)
		}
		seen := map[uuid.UUID]bool{}
		for i, u := range updates {
			s, err := viewUpdate(actor, table, r, u, i)
			if err != nil {
				return nil, err
			}
			steps = append(steps, namedOnce(s, seen, "view"))
		}
		for i, id := range deletes {
			steps = append(steps, namedOnce(viewDelete(actor, table, r, id, i), seen, "view"))
		}
		return steps, nil
	})
}

// namedOnce is s refused, in its batch's check, when its field or view is
// named again: an update and a delete of one, or the same one twice.
func namedOnce(s batchStep, seen map[uuid.UUID]bool, what string) batchStep {
	id, _ := uuid.Parse(s.item.ID)
	again := twice(seen, id, what)
	check := s.check
	s.check = func(ctx context.Context) error {
		if again != nil {
			return again
		}
		return check(ctx)
	}
	return s
}

// fieldOf is the field fields hold by id; nil when it isn't live there.
func fieldOf(fields []*model.Field, id uuid.UUID) *model.Field {
	for _, f := range fields {
		if f.Id == id {
			return f
		}
	}
	return nil
}

// madeBefore is a create's made: whether the batch's read found its id
// held, by a send before (none for a single op, which takes a new id).
func madeBefore(held map[uuid.UUID]bool, id uuid.UUID) func(context.Context) (bool, error) {
	made := held[id]
	return func(context.Context) (bool, error) { return made, nil }
}

// fieldWrite is a field item's write, run at its turn by applyItem, in the
// item's transaction with its audit row. A batch's checks only read.
type fieldWrite struct {
	actor Actor
	table uuid.UUID
	// id is the field's: a create's batch id (uuid.Nil: a new one), or the
	// one changed or deleted.
	id   uuid.UUID
	item map[string]any
}

// fieldInput is a create's item as CreateField takes it, with its id.
func (w fieldWrite) fieldInput() (FieldInput, error) {
	var in FieldInput
	err := decodeItem(w.item, &in)
	in.id = w.id
	return in, err
}

// fieldPatch is an update's item as UpdateField takes it.
func (w fieldWrite) fieldPatch() (FieldPatch, error) {
	var p FieldPatch
	return p, decodeItem(w.item, &p)
}

func (w fieldWrite) applyFieldCreate(ctx context.Context) (itemDone, error) {
	in, _ := w.fieldInput()
	f, err := CreateField(ctx, w.table, in, w.actor)
	if err != nil {
		return itemDone{}, err
	}
	return itemDone{answer: f, summary: fmt.Sprintf("Added the field “%s” to a table through the API", f.Name), facts: fieldFacts(f)}, nil
}

func (w fieldWrite) applyFieldUpdate(ctx context.Context) (itemDone, error) {
	p, _ := w.fieldPatch()
	stored, err := updateFieldStored(ctx, w.table, w.id, p, w.actor)
	if err != nil {
		return itemDone{}, err
	}
	done := itemDone{answer: apiFieldChanged{ID: w.id, CellsChanged: stored.Cells}}
	changed := fieldChanges(stored.Was, stored.Now)
	if len(changed) == 0 && stored.Cells == 0 {
		return done, nil // it changed nothing: no row
	}
	done.summary = fmt.Sprintf("Changed the field “%s” of a table through the API", stored.Now.Name)
	done.facts = fieldFacts(stored.Now)
	done.facts["changed"], done.facts["cells_changed"] = changed, stored.Cells
	return done, nil
}

func (w fieldWrite) applyFieldDelete(ctx context.Context) (itemDone, error) {
	f, err := deleteFieldStored(ctx, w.table, w.id, w.actor)
	if err != nil {
		return itemDone{}, err
	}
	return itemDone{answer: apiDeleted{ID: w.id, Deleted: true},
		summary: fmt.Sprintf("Moved the field “%s” to its table's trash through the API", f.Name), facts: fieldFacts(f)}, nil
}

// fieldCreate is a field's create: checked as CreateField checks it (on a
// copy: the check sets the config it reads) against the batch's read r,
// applied through CreateField with the id given (a batch's; uuid.Nil is a
// new one).
func fieldCreate(actor Actor, table uuid.UUID, r *fieldsRead, item map[string]any, i int, id uuid.UUID) (batchStep, error) {
	w := fieldWrite{actor: actor, table: table, id: id, item: item}
	if _, err := w.fieldInput(); err != nil {
		return batchStep{}, err
	}
	s := batchStep{item: batchItem{List: "create", Index: i}, action: "table.field.created", apply: w.applyFieldCreate}
	if id != uuid.Nil {
		s.item.ID = id.String()
		if r != nil {
			s.made = madeBefore(r.held, id)
		}
	}
	s.check = func(ctx context.Context) error {
		in, _ := w.fieldInput()
		if _, err := computedFieldConfig(ctx, actor, r.t, r.fields, uuid.Nil, &in, r.linked); err != nil {
			return err
		}
		_, _, err := buildField(table, in, nil, false)
		return err
	}
	return s, nil
}

// apiFieldChanged answers a field's update: its id, and how many rows'
// cells changed with it (a new kind, options taken away).
type apiFieldChanged struct {
	ID           uuid.UUID `json:"id"`
	CellsChanged int       `json:"cells_changed"`
}

// fieldUpdate is a field's update: checked as UpdateField checks it against
// the batch's read r, applied through UpdateField.
func fieldUpdate(actor Actor, table uuid.UUID, r *fieldsRead, item map[string]any, i int) (batchStep, error) {
	id, err := idAt(item["field"], fmt.Sprintf("update[%d].field", i))
	if err != nil {
		return batchStep{}, err
	}
	w := fieldWrite{actor: actor, table: table, id: id, item: item}
	if _, err := w.fieldPatch(); err != nil {
		return batchStep{}, err
	}
	s := batchStep{item: batchItem{List: "update", Index: i, ID: id.String()}, action: "table.field.updated", apply: w.applyFieldUpdate}
	s.check = func(ctx context.Context) error {
		p, _ := w.fieldPatch()
		was := fieldOf(r.fields, id)
		if was == nil {
			return errFieldNotFound
		}
		if _, ok := p.NumberSigns.Checked(); !ok {
			return errNumberSigns
		}
		if p.Primary != nil {
			if err := primaryProblem(was, *p.Primary, r.fields); err != nil {
				return err
			}
		}
		in := p.onto(was)
		if _, err := computedFieldConfig(ctx, actor, r.t, r.fields, id, &in, r.linked); err != nil {
			return err
		}
		_, _, err := buildField(table, in, was, p.whole())
		return err
	}
	return s, nil
}

// fieldDelete is a field's delete, to the trash: checked as DeleteField
// checks it against the batch's read r, applied through DeleteField.
func fieldDelete(actor Actor, table uuid.UUID, r *fieldsRead, id uuid.UUID, i int) batchStep {
	w := fieldWrite{actor: actor, table: table, id: id}
	return batchStep{
		item: batchItem{List: "delete", Index: i, ID: id.String()}, action: "table.field.deleted",
		check: func(context.Context) error {
			f := fieldOf(r.fields, id)
			switch {
			case f == nil:
				return errFieldNotFound
			case f.Primary:
				return errPrimaryDeleted
			}
			return nil
		},
		apply: w.applyFieldDelete,
	}
}

// viewWrite is a view item's write, run at its turn by applyItem, in the
// item's transaction with its audit row. A batch's checks only read.
type viewWrite struct {
	actor Actor
	table uuid.UUID
	// id is the view's: a create's batch id (uuid.Nil: a new one), or the
	// one changed or deleted.
	id   uuid.UUID
	item map[string]any
}

// viewInput is a create's item as CreateView takes it, with its id.
func (w viewWrite) viewInput() (ViewInput, error) {
	var in ViewInput
	err := decodeItem(w.item, &in)
	in.id = w.id
	return in, err
}

// changedOnto is view v as the update leaves it (viewPatch.onto, from a
// fresh copy of the item: onto keeps its config map); a locked view is
// refused.
func (w viewWrite) changedOnto(v *model.View) (ViewInput, error) {
	if v.Locked {
		return ViewInput{}, errViewLocked
	}
	var p viewPatch
	_ = decodeItem(w.item, &p)
	return p.onto(v), nil
}

func (w viewWrite) applyViewCreate(ctx context.Context) (itemDone, error) {
	in, _ := w.viewInput()
	v, err := CreateView(ctx, w.table, in, w.actor)
	if err != nil {
		return itemDone{}, err
	}
	return itemDone{answer: v, summary: fmt.Sprintf("Added the view “%s” to a table through the API", v.Name), facts: viewFacts(v)}, nil
}

// applyViewUpdate applies the update onto the view as it is at its turn.
func (w viewWrite) applyViewUpdate(ctx context.Context) (itemDone, error) {
	v, err := viewToChange(ctx, w.table, w.id, w.actor)
	if err != nil {
		return itemDone{}, err
	}
	in, err := w.changedOnto(v)
	if err != nil {
		return itemDone{}, err
	}
	out, err := buildView(w.table, in)
	if err != nil {
		return itemDone{}, err
	}
	out.Id, out.Owner, out.Locked, out.CreatedAt = w.id, v.Owner, v.Locked, v.CreatedAt
	what := viewChanges(v, out)
	if len(what) == 0 {
		return itemDone{answer: out}, nil // it changed nothing: no write, no row
	}
	if err := UpdateView(ctx, w.table, w.id, in, w.actor); err != nil {
		return itemDone{}, err
	}
	done := itemDone{answer: out, summary: fmt.Sprintf("Changed the view “%s” of a table through the API", out.Name), facts: viewFacts(out)}
	done.facts["changed"] = what
	return done, nil
}

func (w viewWrite) applyViewDelete(ctx context.Context) (itemDone, error) {
	v, err := viewToChange(ctx, w.table, w.id, w.actor)
	if err != nil {
		return itemDone{}, err
	}
	if err := DeleteView(ctx, w.table, w.id, w.actor); err != nil {
		return itemDone{}, err
	}
	return itemDone{answer: apiDeleted{ID: w.id, Deleted: true},
		summary: fmt.Sprintf("Moved the view “%s” to its table's trash through the API", v.Name), facts: viewFacts(v)}, nil
}

// viewCreate is a view's create: checked as CreateView checks it against the
// batch's read r, applied through CreateView with the id given (a batch's;
// uuid.Nil is a new one).
func viewCreate(actor Actor, table uuid.UUID, r *viewsRead, item map[string]any, i int, id uuid.UUID) (batchStep, error) {
	w := viewWrite{actor: actor, table: table, id: id, item: item}
	if _, err := w.viewInput(); err != nil {
		return batchStep{}, err
	}
	s := batchStep{item: batchItem{List: "create", Index: i}, action: "table.view.created", apply: w.applyViewCreate}
	if id != uuid.Nil {
		s.item.ID = id.String()
		if r != nil {
			s.made = madeBefore(r.held, id)
		}
	}
	s.check = func(context.Context) error {
		in, _ := w.viewInput()
		if !in.Personal && !canManage(r.t, actor) {
			return errForbidden
		}
		_, err := buildView(table, in)
		return err
	}
	return s, nil
}

// viewPatch is a change to a view through the API: what it names changes,
// and the rest keeps the view's stored value (onto), since UpdateView saves a
// view whole.
type viewPatch struct {
	Name     *string        `json:"name"`
	Type     *string        `json:"type"`
	Config   map[string]any `json:"config"`
	Position *float64       `json:"position"`
}

// onto is view v as p leaves it.
func (p viewPatch) onto(v *model.View) ViewInput {
	in := ViewInput{Name: v.Name, Type: v.Type, Config: p.Config, Position: v.Position}
	if p.Name != nil {
		in.Name = *p.Name
	}
	if p.Type != nil {
		in.Type = *p.Type
	}
	if p.Position != nil {
		in.Position = *p.Position
	}
	if in.Config == nil {
		in.Config = map[string]any{}
		_ = json.Unmarshal([]byte(v.Config), &in.Config)
	}
	return in
}

// viewUpdate is a view's update: checked as UpdateView checks it against the
// batch's read r, applied onto the view as it is at its turn
// (applyViewUpdate) through UpdateView.
func viewUpdate(actor Actor, table uuid.UUID, r *viewsRead, item map[string]any, i int) (batchStep, error) {
	id, err := idAt(item["view"], fmt.Sprintf("update[%d].view", i))
	if err != nil {
		return batchStep{}, err
	}
	var p viewPatch
	if err := decodeItem(item, &p); err != nil {
		return batchStep{}, err
	}
	w := viewWrite{actor: actor, table: table, id: id, item: item}
	s := batchStep{item: batchItem{List: "update", Index: i, ID: id.String()}, action: "table.view.updated", apply: w.applyViewUpdate}
	s.check = func(context.Context) error {
		v, err := changeable(r.t, r.views[id], actor)
		if err != nil {
			return err
		}
		in, err := w.changedOnto(v)
		if err == nil {
			_, err = buildView(table, in)
		}
		return err
	}
	return s, nil
}

// viewDelete is a view's delete, to the trash: checked as DeleteView checks
// it against the batch's read r, applied through DeleteView.
func viewDelete(actor Actor, table uuid.UUID, r *viewsRead, id uuid.UUID, i int) batchStep {
	w := viewWrite{actor: actor, table: table, id: id}
	return batchStep{
		item: batchItem{List: "delete", Index: i, ID: id.String()}, action: "table.view.deleted",
		check: func(context.Context) error {
			v, err := changeable(r.t, r.views[id], actor)
			if err == nil && v.Locked {
				err = errViewLocked
			}
			return err
		},
		apply: w.applyViewDelete,
	}
}

// fieldFacts are an audit row's facts of a field as stored.
func fieldFacts(f *model.Field) map[string]any {
	return map[string]any{"id": f.Id.String(), "name": f.Name, "type": f.Type}
}

// viewFacts are an audit row's facts of a view as stored.
func viewFacts(v *model.View) map[string]any {
	return map[string]any{"id": v.Id.String(), "name": v.Name, "type": v.Type}
}

// fieldChanges are what a field's write changed, as it found the field and
// as it left it: none for a write that left it as it was.
func fieldChanges(was, now *model.Field) []string {
	if was == nil || now == nil {
		return []string{"field"}
	}
	var out []string
	add := func(name string, differs bool) {
		if differs {
			out = append(out, name)
		}
	}
	add("name", was.Name != now.Name)
	add("type", was.Type != now.Type)
	add("config", !sameConfig(was.Config, now.Config))
	add("position", was.Position != now.Position)
	add("description", textOf(was.Description) != textOf(now.Description))
	add("required", was.Required != now.Required)
	add("default_value", !sameJSON(was.Default, now.Default))
	add("primary", was.Primary != now.Primary)
	return out
}

// viewChanges are what a view's write changes: none for one that leaves it
// as it is.
func viewChanges(was, now *model.View) []string {
	var out []string
	add := func(name string, differs bool) {
		if differs {
			out = append(out, name)
		}
	}
	add("name", was.Name != now.Name)
	add("type", was.Type != now.Type)
	add("config", !sameConfig(was.Config, now.Config))
	add("position", was.Position != now.Position)
	return out
}

// sameConfig is whether two configs say the same thing (sameJSON): a stored
// one comes back from jsonb in its own spacing and key order. Empty is {}.
func sameConfig(a, b string) bool {
	if strings.TrimSpace(a) == "" {
		a = "{}"
	}
	if strings.TrimSpace(b) == "" {
		b = "{}"
	}
	return sameJSON(json.RawMessage(a), json.RawMessage(b))
}

// textOf is an optional text's value ("" for none).
func textOf(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
