package business

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	v1api "github.com/akashc777/OneCamp/business/PublicAPI"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

// The JSON Schema pieces the rows ops share (STATIC text, as Input must be).
const (
	tableArg = `"id":{"type":"string","maxLength":64,"description":"The table's id."}`
	rowArg   = `"row":{"type":"string","maxLength":64,"description":"The row's id."}`
	pageArg  = `"cursor":{"type":"string","maxLength":64,"description":"The next_cursor of the page before."}`
)

// rowOps are the tables ops on rows beside the query and the update (tables
// H2a): a row and rows read by id, a batch written as one change, a delete to
// the table's trash, the trash and its restore, and a row's history. Each
// Serve calls the app's own function as the token's member (condition c), and
// a delete is soft: the row waits in the trash, as the app's does (a).
func rowOps() []v1api.Op {
	return []v1api.Op{{
		Name:    "tables.row.get",
		Summary: "A row by its id, as it is now; its version is the ETag a change's If-Match names",
		Method:  http.MethodGet, Path: "/tables/{id}/rows/{row}", Scope: apiTokenBusiness.ScopeTablesRead,
		Behaviour: v1api.Behaviour{ReadOnly: true, Idempotent: true},
		Input:     json.RawMessage(`{"type":"object","properties":{` + tableArg + `,` + rowArg + `},"required":["id","row"]}`),
		Versioned: true,
		Reach:     tableReach,
		Serve:     serveRowGet,
	}, {
		Name:    "tables.rows.get",
		Summary: "Rows of a table by their ids, in the order named; a row deleted or not the table's is left out",
		Method:  http.MethodPost, Path: "/tables/{id}/rows/get", Scope: apiTokenBusiness.ScopeTablesRead,
		Behaviour: v1api.Behaviour{ReadOnly: true, Idempotent: true},
		Input: json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{`+tableArg+`,
			"ids":{"type":"array","minItems":1,"maxItems":%d,"items":{"type":"string","maxLength":64},"description":"The rows' ids."}},
			"required":["id","ids"]}`, MaxRowsByID)),
		Reach: tableReach,
		Cost:  countOf("ids"),
		Serve: serveRowsGet,
	}, {
		Name:    "tables.rows.batch",
		Summary: "Make, change and delete rows of a table in one change: all of it is written, or none and the rows refused are named",
		Method:  http.MethodPost, Path: "/tables/{id}/rows/batch", Scope: apiTokenBusiness.ScopeTablesWrite,
		// It may delete rows (to the trash), and the same rows made twice are two.
		Behaviour: v1api.Behaviour{Destructive: true, Recoverable: true},
		// Rows it made go to the trash, cells it changed are set back from
		// the history, and rows it deleted come back from the trash.
		UndoneBy: []string{"tables.row.delete", "tables.row.history", "tables.row.update", "tables.trash.restore"},
		Input: json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{`+tableArg+`,
			"create":{"type":"array","maxItems":%[1]d,"description":"Rows to make.","items":{"type":"object","properties":{
				"values":{"type":"object","additionalProperties":{},"description":"Each field's id and its value."}},"required":["values"]}},
			"update":{"type":"array","maxItems":%[1]d,"description":"Rows to change: the cells named, the rest as they are.","items":{"type":"object","properties":{
				`+rowArg+`,
				"values":{"type":"object","additionalProperties":{},"description":"Each field's id and its new value; null clears it."},
				"version":{"type":"integer","minimum":1,"description":"The row's version as it was read: a row changed since refuses the batch."}},
				"required":["row","values"]}},
			"delete":{"type":"array","maxItems":%[1]d,"items":{"type":"string","maxLength":64},"description":"Rows to move to the trash."},
			"after":{"type":"string","maxLength":64,"description":"New rows go after this row; with neither after nor before, after the last."},
			"before":{"type":"string","maxLength":64,"description":"New rows go before this row."}},
			"required":["id"]}`, maxBatch)),
		Reach: tableReach,
		Cost:  countOf("create", "update", "delete"),
		Serve: serveRowsBatch,
	}, {
		Name:    "tables.row.delete",
		Summary: "Move a row to its table's trash, from where it can be brought back",
		Method:  http.MethodDelete, Path: "/tables/{id}/rows/{row}", Scope: apiTokenBusiness.ScopeTablesWrite,
		Behaviour: v1api.Behaviour{Destructive: true, Idempotent: true, Recoverable: true},
		UndoneBy:  []string{"tables.trash.restore"},
		Input:     json.RawMessage(`{"type":"object","properties":{` + tableArg + `,` + rowArg + `},"required":["id","row"]}`),
		Reach:     tableReach,
		Serve:     serveRowDelete,
	}, {
		Name:    "tables.trash.list",
		Summary: "A table's trash: its deleted rows, newest first, a page at a time, its deleted fields for those who manage it, and its deleted views the caller may bring back",
		Method:  http.MethodGet, Path: "/tables/{id}/trash", Scope: apiTokenBusiness.ScopeTablesRead,
		Behaviour: v1api.Behaviour{ReadOnly: true, Idempotent: true},
		Input:     json.RawMessage(`{"type":"object","properties":{` + tableArg + `,` + pageArg + `},"required":["id"]}`),
		Reach:     tableReach,
		Cost:      func(map[string]any) int { return trashPage },
		Serve:     serveTrashList,
	}, {
		Name:    "tables.trash.restore",
		Summary: "Bring rows, fields and views back from a table's trash, rows and fields with their cells and links",
		Method:  http.MethodPost, Path: "/tables/{id}/trash/restore", Scope: apiTokenBusiness.ScopeTablesWrite,
		Behaviour: v1api.Behaviour{Idempotent: true, Recoverable: true},
		UndoneBy:  []string{"tables.field.delete", "tables.row.delete", "tables.view.delete"},
		Input: json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{`+tableArg+`,
			"rows":{"type":"array","maxItems":%[1]d,"items":{"type":"string","maxLength":64},"description":"Rows to bring back."},
			"fields":{"type":"array","maxItems":%[1]d,"items":{"type":"string","maxLength":64},"description":"Fields to bring back: for those who manage the table."},
			"views":{"type":"array","maxItems":%[1]d,"items":{"type":"string","maxLength":64},"description":"Views to bring back: one's own, and shared ones for those who manage the table."}},
			"required":["id"]}`, maxBatch)),
		Reach: tableReach,
		Cost:  countOf("rows", "fields", "views"),
		Serve: serveTrashRestore,
	}, {
		Name:    "tables.row.history",
		Summary: "A row's history, newest first, a page at a time: each change, who made it and how, and its cells before and after",
		Method:  http.MethodGet, Path: "/tables/{id}/rows/{row}/history", Scope: apiTokenBusiness.ScopeTablesRead,
		Behaviour: v1api.Behaviour{ReadOnly: true, Idempotent: true},
		Input:     json.RawMessage(`{"type":"object","properties":{` + tableArg + `,` + rowArg + `,` + pageArg + `},"required":["id","row"]}`),
		Reach:     tableReach,
		Cost:      func(map[string]any) int { return historyPage },
		Serve:     serveRowHistory,
	}}
}

func serveRowGet(ctx context.Context, call v1api.Call) (any, error) {
	actor, table, row, err := rowCall(ctx, call)
	if err != nil {
		return nil, err
	}
	rows, err := RowsByID(ctx, table, actor, []uuid.UUID{row})
	if err != nil {
		return nil, apiErr(unseen(err))
	}
	if len(rows) != 1 {
		return nil, apiErr(errRowNotFound)
	}
	return apiRow{rows[0]}, nil
}

func serveRowsGet(ctx context.Context, call v1api.Call) (any, error) {
	actor, table, err := tableCall(ctx, call)
	if err != nil {
		return nil, err
	}
	ids, err := idsArg(call.Args, "ids")
	if err != nil {
		return nil, err
	}
	rows, err := RowsByID(ctx, table, actor, ids)
	if err != nil {
		return nil, apiErr(unseen(err))
	}
	return v1api.Page[*model.Row]{Items: rows}, nil
}

func serveRowsBatch(ctx context.Context, call v1api.Call) (any, error) {
	actor, table, err := tableCall(ctx, call)
	if err != nil {
		return nil, err
	}
	var in BatchInput
	for _, c := range listArg(call.Args, "create") {
		values, _ := c["values"].(map[string]any)
		in.Create = append(in.Create, RowInput{Values: values})
	}
	for i, u := range listArg(call.Args, "update") {
		row, err := idAt(u["row"], fmt.Sprintf("update[%d].row", i))
		if err != nil {
			return nil, err
		}
		values, _ := u["values"].(map[string]any)
		in.Patch = append(in.Patch, PatchInput{Row: row, Values: values, Version: int64Of(u["version"])})
	}
	if in.Delete, err = idsArg(call.Args, "delete"); err != nil {
		return nil, err
	}
	var at PlaceInput
	if at.After, err = optionalID(call.Args, "after"); err != nil {
		return nil, err
	}
	if at.Before, err = optionalID(call.Args, "before"); err != nil {
		return nil, err
	}
	if at.After != nil || at.Before != nil {
		in.At = &at
	}
	done, err := Batch(ctx, table, in, actorBy(apiBy(ctx, actor)))
	if err != nil {
		return nil, apiErr(unseen(err))
	}
	return done, nil
}

// apiDeleted is a delete's answer: the row, now in its table's trash.
type apiDeleted struct {
	ID      uuid.UUID `json:"id"`
	Deleted bool      `json:"deleted"`
}

func serveRowDelete(ctx context.Context, call v1api.Call) (any, error) {
	actor, table, row, err := rowCall(ctx, call)
	if err != nil {
		return nil, err
	}
	if err := (ledgerWriter{}).Delete(ctx, apiBy(ctx, actor), table, []uuid.UUID{row}); err != nil {
		return nil, apiErr(unseen(err))
	}
	return apiDeleted{ID: row, Deleted: true}, nil
}

// apiTrash is a page of a table's trash as the API answers it: the rows in
// the one page shape, and the deleted fields and views beside them.
type apiTrash struct {
	Items      []TrashRow    `json:"items"`
	Fields     []TrashField  `json:"fields"`
	Views      []*model.View `json:"views"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

func serveTrashList(ctx context.Context, call v1api.Call) (any, error) {
	actor, table, err := tableCall(ctx, call)
	if err != nil {
		return nil, err
	}
	before, err := cursorID(call.Args)
	if err != nil {
		return nil, err
	}
	trash, err := TableTrash(ctx, table, before, actor)
	if err != nil {
		return nil, apiErr(unseen(err))
	}
	out := apiTrash{Items: trash.Rows, Fields: trash.Fields, Views: trash.Views}
	if trash.More && len(trash.Rows) > 0 {
		out.NextCursor = trash.Rows[len(trash.Rows)-1].ID.String()
	}
	return out, nil
}

func serveTrashRestore(ctx context.Context, call v1api.Call) (any, error) {
	actor, table, err := tableCall(ctx, call)
	if err != nil {
		return nil, err
	}
	var in RestoreInput
	if in.Rows, err = idsArg(call.Args, "rows"); err != nil {
		return nil, err
	}
	if in.Fields, err = idsArg(call.Args, "fields"); err != nil {
		return nil, err
	}
	if in.Views, err = idsArg(call.Args, "views"); err != nil {
		return nil, err
	}
	done, err := RestoreFromTrash(ctx, table, in, actorBy(apiBy(ctx, actor)))
	if err != nil {
		return nil, apiErr(unseen(err))
	}
	return done, nil
}

func serveRowHistory(ctx context.Context, call v1api.Call) (any, error) {
	actor, table, row, err := rowCall(ctx, call)
	if err != nil {
		return nil, err
	}
	before, err := cursorID(call.Args)
	if err != nil {
		return nil, err
	}
	h, err := RowHistory(ctx, table, row, before, actor)
	if err != nil {
		return nil, apiErr(unseen(err))
	}
	page := v1api.Page[HistoryItem]{Items: h.Items}
	if h.More && len(h.Items) > 0 {
		page.NextCursor = h.Items[len(h.Items)-1].ChangeSet.String()
	}
	return page, nil
}

// tableCall is the member a call acts as and the table it names.
func tableCall(ctx context.Context, call v1api.Call) (Actor, uuid.UUID, error) {
	actor, err := apiActor(ctx)
	if err != nil {
		return Actor{}, uuid.Nil, err
	}
	table, err := idArg(call.Args, "id")
	return actor, table, err
}

// rowCall is tableCall and the row the call names.
func rowCall(ctx context.Context, call v1api.Call) (Actor, uuid.UUID, uuid.UUID, error) {
	actor, table, err := tableCall(ctx, call)
	if err != nil {
		return Actor{}, uuid.Nil, uuid.Nil, err
	}
	row, err := idArg(call.Args, "row")
	return actor, table, row, err
}

// listArg is the objects a list argument holds (the validator has checked
// each is one).
func listArg(args map[string]any, name string) []map[string]any {
	list, _ := args[name].([]any)
	out := make([]map[string]any, 0, len(list))
	for _, v := range list {
		m, _ := v.(map[string]any)
		out = append(out, m)
	}
	return out
}

// idAt is the id an argument at a place in the call holds; one that isn't an
// id is the caller's to fix.
func idAt(v any, at string) (uuid.UUID, error) {
	s, _ := v.(string)
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, v1api.Errorf(http.StatusBadRequest, "invalid_argument", "", "%s isn't an id: %q", at, s)
	}
	return id, nil
}

// idsArg is the ids a list argument names, in its order.
func idsArg(args map[string]any, name string) ([]uuid.UUID, error) {
	list, _ := args[name].([]any)
	out := make([]uuid.UUID, 0, len(list))
	for i, v := range list {
		id, err := idAt(v, fmt.Sprintf("%s[%d]", name, i))
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, nil
}

// optionalID is the id an argument names, or nil when the call leaves it out.
func optionalID(args map[string]any, name string) (*uuid.UUID, error) {
	if s, _ := args[name].(string); s == "" {
		return nil, nil
	}
	id, err := idAt(args[name], name)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

// cursorID is the page marker a list handed back (the last id of the page
// before), or nil for the first page.
func cursorID(args map[string]any) (*uuid.UUID, error) {
	if s, _ := args["cursor"].(string); s == "" {
		return nil, nil
	}
	if id, err := uuid.Parse(args["cursor"].(string)); err == nil {
		return &id, nil
	}
	return nil, v1api.Errorf(http.StatusBadRequest, "invalid_argument", "send the next_cursor a page handed back", "that page marker isn't this list's")
}

// countOf is what a call spends of the token's row budget: the rows its list
// arguments name (condition b: a batch charges per row).
func countOf(names ...string) func(map[string]any) int {
	return func(args map[string]any) int {
		n := 0
		for _, name := range names {
			list, _ := args[name].([]any)
			n += len(list)
		}
		return n
	}
}
