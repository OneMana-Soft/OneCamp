package business

// A table's own life over the public API (tables H2c): made, renamed, moved to
// the tables' trash and brought back, as the app does each (CreateTable,
// UpdateTable, DeleteTable, RestoreTable), as the token's member and never
// with their admin powers (apiActor). Anyone may make a table, as in the app;
// its lifecycle after that is its managers' (canManage: its maker, or an admin
// acting in the app, which a token never is). Each change is applied with its
// audit row in one transaction (applyItem: the write, the row, the commit), so
// a change the log can't record isn't made.

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

// AppsMakeNoTables is an app's refusal of the tables ops that aren't about one
// table it was added to.
const AppsMakeNoTables = "An app works in the tables it was added to: it can't make tables or list the deleted ones."

// tableHeaderProps are a table's header, as a create sets it and an update
// changes it (the app's own limits).
var tableHeaderProps = fmt.Sprintf(`"name":{"type":"string","minLength":1,"maxLength":%d,"description":"The table's name."},
	"description":{"type":"string","maxLength":%d,"description":"What the table is for; empty clears it."},
	"icon":{"type":"string","maxLength":%d,"description":"An emoji for it; empty clears it."}`, maxNameLen, maxDescriptionLen, maxIconLen)

func tableOps() []v1api.Op {
	return []v1api.Op{{
		Name:    "tables.table.create",
		Summary: "Make a table as the token's member, with a Name field and a grid view to start: shown to the workspace, or private to its maker",
		Method:  http.MethodPost, Path: "/tables", Scope: apiTokenBusiness.ScopeTablesWrite,
		// Each one sent is one more table: an Idempotency-Key makes a retry
		// safe. It deletes nothing, and tables.table.delete takes it back.
		Behaviour: v1api.Behaviour{Recoverable: true},
		UndoneBy:  []string{"tables.table.delete"},
		Input: json.RawMessage(`{"type":"object","properties":{` + tableHeaderProps + `,
			"visibility":{"type":"string","enum":["workspace","private"],"description":"Who can see it: the workspace (when left out), or only its maker and whoever it's shared with."}},
			"required":["name"]}`),
		AppsRefused: AppsMakeNoTables,
		Unplaced:    "self",
		Serve:       serveTableCreate,
	}, {
		Name:    "tables.table.update",
		Summary: "Rename a table, or change its description or icon: for its managers",
		Method:  http.MethodPatch, Path: "/tables/{id}", Scope: apiTokenBusiness.ScopeTablesWrite,
		Behaviour: v1api.Behaviour{Idempotent: true, Recoverable: true},
		UndoneBy:  []string{"tables.table.update"},
		Input:     json.RawMessage(`{"type":"object","properties":{` + tableArg + `,` + tableHeaderProps + `},"required":["id"]}`),
		Reach:     tableReach,
		Serve:     serveTableUpdate,
	}, {
		Name:    "tables.table.delete",
		Summary: "Move a table to the tables' trash, with its rows, fields and views: for its managers; tables.table.restore brings it back",
		Method:  http.MethodDelete, Path: "/tables/{id}", Scope: apiTokenBusiness.ScopeTablesWrite,
		Behaviour: v1api.Behaviour{Destructive: true, Idempotent: true, Recoverable: true},
		UndoneBy:  []string{"tables.table.restore"},
		Input:     json.RawMessage(`{"type":"object","properties":{` + tableArg + `},"required":["id"]}`),
		Reach:     tableReach,
		Serve:     serveTableDelete,
	}, {
		Name:    "tables.table.restore",
		Summary: "Bring a table back from the tables' trash, with its rows, fields and views: for its managers",
		Method:  http.MethodPost, Path: "/tables/{id}/restore", Scope: apiTokenBusiness.ScopeTablesWrite,
		Behaviour: v1api.Behaviour{Idempotent: true, Recoverable: true},
		UndoneBy:  []string{"tables.table.delete"},
		Input:     json.RawMessage(`{"type":"object","properties":{` + tableArg + `},"required":["id"]}`),
		Reach:     tableReach,
		Serve:     serveTableRestore,
	}, {
		Name:    "tables.deleted.list",
		Summary: "The tables in the tables' trash that the token's member may bring back: the ones they made, newest first",
		Method:  http.MethodGet, Path: "/tables/trash", Scope: apiTokenBusiness.ScopeTablesRead,
		Behaviour:   v1api.Behaviour{ReadOnly: true, Idempotent: true},
		Input:       json.RawMessage(`{"type":"object","properties":{}}`),
		AppsRefused: AppsMakeNoTables,
		Unplaced:    "workspace",
		Cost:        func(map[string]any) int { return trashPage },
		Serve:       serveTablesDeleted,
	}}
}

// tableChange applies a change to a table with its audit row as action, for
// op (applyItem), and answers it in the API's one error body: a table the
// member can open but not manage is forbidden, one they can't open isn't
// there (schemaErr).
func tableChange(ctx context.Context, actor Actor, table uuid.UUID, op, action string, apply func(ctx context.Context) (itemDone, error)) (any, error) {
	s := batchStep{item: batchItem{List: "tables", ID: table.String()}, action: action, apply: apply}
	v, err := applyItem(ctx, s, func(ctx context.Context, s batchStep, done itemDone, action string) error {
		return auditItem(ctx, actor, op, "", table, s, done, action)
	})
	if err != nil {
		return nil, schemaErr(ctx, table, actor, err)
	}
	return v, nil
}

// tableFacts are what a table's audit row says of it: never its rows.
func tableFacts(t *model.DataTable) map[string]any {
	return map[string]any{"table_id": t.Id.String(), "id": t.Id.String(), "name": t.Name, "visibility": t.Visibility}
}

// stringArg is a string argument the call names, nil when it names none.
func stringArg(args map[string]any, name string) *string {
	s, ok := args[name].(string)
	if !ok {
		return nil
	}
	return &s
}

func serveTableCreate(ctx context.Context, call v1api.Call) (any, error) {
	actor, err := apiActor(ctx)
	if err != nil {
		return nil, err
	}
	in := TableInput{}
	for name, into := range map[string]*string{"name": &in.Name, "description": &in.Description, "icon": &in.Icon, "visibility": &in.Visibility} {
		if s := stringArg(call.Args, name); s != nil {
			*into = *s
		}
	}
	// The table's id is the one it is made with: the audit row's facts name
	// it (they override the step's empty id).
	return tableChange(ctx, actor, uuid.Nil, "tables.table.create", "table.created", func(ctx context.Context) (itemDone, error) {
		t, err := CreateTable(ctx, in, actor)
		if err != nil {
			return itemDone{}, err
		}
		if t == nil {
			return itemDone{}, storeFailed("failed to read the new table")
		}
		return itemDone{answer: t, summary: fmt.Sprintf("Made the table “%s” through the API", t.Name), facts: tableFacts(t)}, nil
	})
}

func serveTableUpdate(ctx context.Context, call v1api.Call) (any, error) {
	actor, table, err := tableCall(ctx, call)
	if err != nil {
		return nil, err
	}
	in := TablePatch{Name: stringArg(call.Args, "name"), Description: stringArg(call.Args, "description"), Icon: stringArg(call.Args, "icon")}
	if in.Name == nil && in.Description == nil && in.Icon == nil {
		return nil, v1api.Errorf(http.StatusBadRequest, "invalid_argument", "", "name what to change: name, description or icon")
	}
	return tableChange(ctx, actor, table, "tables.table.update", "table.updated", func(ctx context.Context) (itemDone, error) {
		was, err := model.GetTableByID(ctx, table)
		if err != nil {
			return itemDone{}, storeFailed("failed to load table")
		}
		now, _, err := UpdateTable(ctx, table, in, actor)
		if err != nil {
			return itemDone{}, err
		}
		done := itemDone{answer: now}
		changed := tableChanges(was, now)
		if len(changed) == 0 {
			return done, nil // it changed nothing: no row
		}
		done.summary = fmt.Sprintf("Changed the table “%s” through the API", now.Name)
		done.facts = tableFacts(now)
		done.facts["changed"] = changed
		if was.Name != now.Name {
			done.facts["was_name"] = was.Name
		}
		return done, nil
	})
}

// tableChanges names the header fields a change moved.
func tableChanges(was, now *model.DataTable) []string {
	var out []string
	if was.Name != now.Name {
		out = append(out, "name")
	}
	if textOf(was.Description) != textOf(now.Description) {
		out = append(out, "description")
	}
	if textOf(was.Icon) != textOf(now.Icon) {
		out = append(out, "icon")
	}
	return out
}

func serveTableDelete(ctx context.Context, call v1api.Call) (any, error) {
	actor, table, err := tableCall(ctx, call)
	if err != nil {
		return nil, err
	}
	return tableChange(ctx, actor, table, "tables.table.delete", "table.deleted", func(ctx context.Context) (itemDone, error) {
		t, err := loadManageable(ctx, table, actor)
		if err != nil {
			return itemDone{}, err
		}
		if err := DeleteTable(ctx, table, actor); err != nil {
			return itemDone{}, err
		}
		return itemDone{answer: apiDeleted{ID: table, Deleted: true},
			summary: fmt.Sprintf("Moved the table “%s” to the tables' trash through the API", t.Name), facts: tableFacts(t)}, nil
	})
}

func serveTableRestore(ctx context.Context, call v1api.Call) (any, error) {
	actor, table, err := tableCall(ctx, call)
	if err != nil {
		return nil, err
	}
	return tableChange(ctx, actor, table, "tables.table.restore", "table.restored", func(ctx context.Context) (itemDone, error) {
		t, err := RestoreTable(ctx, table, actor)
		if err != nil {
			return itemDone{}, err
		}
		return itemDone{answer: t, summary: fmt.Sprintf("Brought the table “%s” back from the tables' trash through the API", t.Name), facts: tableFacts(t)}, nil
	})
}

func serveTablesDeleted(ctx context.Context, _ v1api.Call) (any, error) {
	actor, err := apiActor(ctx)
	if err != nil {
		return nil, err
	}
	list, err := TablesTrash(ctx, actor)
	if err != nil {
		return nil, apiErr(err)
	}
	return v1api.Page[TrashTable]{Items: list}, nil
}
