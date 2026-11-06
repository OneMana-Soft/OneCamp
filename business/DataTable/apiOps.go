package business

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"

	"github.com/google/uuid"

	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	v1api "github.com/akashc777/OneCamp/business/PublicAPI"
	query "github.com/akashc777/OneCamp/business/QuerySpec"
	rowLedger "github.com/akashc777/OneCamp/business/RowLedger"
	rowQuery "github.com/akashc777/OneCamp/business/RowQuery"
	tableAccess "github.com/akashc777/OneCamp/business/TableAccess"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// APIOps are the tables operations of the public API (tables H1), each one
// declared once: a /v1 route in both editions and, in the AI edition, an MCP
// tool with the same scope (business/MCPServer publishOps). Serve calls the
// app's own functions as the token's member, so a call sees and changes
// exactly what that member can in the app (condition c).
func APIOps() []v1api.Op {
	return append([]v1api.Op{{
		Name:    "tables.rows.query",
		Summary: "A table's rows, filtered, sorted and searched as a view does, a page at a time",
		Method:  http.MethodPost, Path: "/tables/{id}/rows/query", Scope: apiTokenBusiness.ScopeTablesRead,
		Behaviour: v1api.Behaviour{ReadOnly: true, Idempotent: true},
		Input: json.RawMessage(`{"type":"object","properties":{
			"id":{"type":"string","maxLength":64,"description":"The table's id."},
			"filter":{"description":"Which rows: a view's filter, conditions in groups. Leave it out for every row."},
			"sorts":{"type":"array","maxItems":10,"items":{"type":"object","properties":{
				"field":{"type":"string","maxLength":64},"desc":{"type":"boolean"}},"required":["field"]}},
			"search":{"type":"string","maxLength":200,"description":"Words a row's cells hold."},
			"cursor":{"type":"string","maxLength":4096,"description":"The next_cursor of the page before."},
			"limit":{"type":"integer","minimum":1,"maximum":200,"description":"Rows a page, 50 when left out."},
			"tz":{"type":"string","maxLength":64,"description":"An IANA time zone, where a formula's TODAY() is."}},
			"required":["id"]}`),
		Reach: tableReach,
		// A read spends the page it asks for (PLATFORM-LEAD-81, change 5).
		Cost:  func(args map[string]any) int { _, n := v1api.PageArgs(args); return n },
		Serve: serveRowsQuery,
	}, {
		Name:    "tables.row.update",
		Summary: "Change some cells of a row and leave the rest as they are; the answer names the change set it stored (null if it changed nothing), as the row's history lists it",
		Method:  http.MethodPatch, Path: "/tables/{id}/rows/{row}", Scope: apiTokenBusiness.ScopeTablesWrite,
		// As update_table_row is bridged: the same cells set twice are set once.
		Behaviour: v1api.Behaviour{Idempotent: true, Recoverable: true},
		// An overwrite is set back from the row's history.
		UndoneBy: []string{"tables.row.history", "tables.row.update"},
		Input: json.RawMessage(`{"type":"object","properties":{
			"id":{"type":"string","maxLength":64,"description":"The table's id."},
			"row":{"type":"string","maxLength":64,"description":"The row's id."},
			"values":{"type":"object","additionalProperties":{},"description":"Each field's id and its new value; null clears it."},
			"version":{"type":"integer","minimum":1,"description":"The row's version as it was read (If-Match): a row changed since is refused."}},
			"required":["id","row","values"]}`),
		Versioned: true,
		Reach:     tableReach,
		Serve:     serveRowUpdate,
	}}, slices.Concat(rowOps(), schemaOps(), commentOps(), tableOps())...)
}

// tableReach is the table a call names: an app's token is held to the tables
// it was added to, on REST and MCP alike.
func tableReach(args map[string]any) (string, string) {
	id, _ := args["id"].(string)
	return "table", id
}

// apiActor is who a call acts as: the token's member, as the token's
// middleware (and MCP's principal) put them on the context, and never with
// their admin powers, which a token doesn't carry (helpers.ActsThroughToken).
func apiActor(ctx context.Context) (Actor, error) {
	info, ok := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	if !ok || info.UserPostgresInfo.Id == uuid.Nil {
		return Actor{}, v1api.Errorf(http.StatusUnauthorized, "unauthenticated", "send a token", "this call acts for nobody")
	}
	return Actor{UserID: info.UserPostgresInfo.Id, IsAdmin: info.UserPostgresInfo.IsAdmin && !helpers.ActsThroughToken(ctx)}, nil
}

func serveRowsQuery(ctx context.Context, call v1api.Call) (any, error) {
	actor, err := apiActor(ctx)
	if err != nil {
		return nil, err
	}
	table, err := idArg(call.Args, "id")
	if err != nil {
		return nil, err
	}
	// The spec as the app's grid sends it: the query checks it as it does the
	// app's (an unknown field or operator is refused there).
	raw, _ := json.Marshal(map[string]any{"filter": call.Args["filter"], "sorts": call.Args["sorts"], "search": call.Args["search"]})
	var spec query.Spec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return nil, v1api.Errorf(http.StatusBadRequest, "invalid_argument", "a filter is a view's, as the reference shows", "the filter can't be read: %v", err)
	}
	cursor, limit := v1api.PageArgs(call.Args)
	if tz, _ := call.Args["tz"].(string); tz != "" {
		ctx = WithZone(ctx, tz)
	}
	w, err := QueryRows(ctx, table, actor, spec, query.Page{Size: limit, After: query.Cursor(cursor)}, false)
	if err != nil {
		return nil, apiErr(err)
	}
	return v1api.Page[*model.Row]{Items: w.Rows, NextCursor: string(w.Next)}, nil
}

func serveRowUpdate(ctx context.Context, call v1api.Call) (any, error) {
	actor, err := apiActor(ctx)
	if err != nil {
		return nil, err
	}
	table, err := idArg(call.Args, "id")
	if err != nil {
		return nil, err
	}
	row, err := idArg(call.Args, "row")
	if err != nil {
		return nil, err
	}
	values, _ := call.Args["values"].(map[string]any)
	// The row ledger's one write path: no version is PatchRow, a version is a
	// batch of one that refuses a row changed since. A table the member can't
	// open is one that isn't there, as the query answers it (unseen): a 403
	// would tell the token it exists.
	done, set, err := ledgerWriter{}.patch(ctx, apiBy(ctx, actor), table, []rowLedger.RowPatch{{Row: row, Cells: values, Version: int64Of(call.Args["version"])}})
	if err != nil {
		return nil, apiErr(unseen(err))
	}
	if len(done) != 1 {
		return nil, apiErr(errRowNotFound)
	}
	return apiWritten{(&Written{Row: done[0], ChangeSet: set}).AsRead(ctx, table, actor)}, nil
}

// apiBy is who a token's write is by, as the row ledger records it
// (condition e): the token's member, and the agent the token is bound to, of
// the agent's kind, or the API's for a plain token. From the context the
// token's check filled, never from what the call sends.
func apiBy(ctx context.Context, actor Actor) rowLedger.By {
	by := rowLedger.By{Who: tableAccess.Who{User: actor.UserID, Admin: actor.IsAdmin}, Kind: rowLedger.ActorAPI}
	if agent, err := uuid.Parse(helpers.GetApiAgentID(ctx)); err == nil {
		by.Who.Agent, by.Kind = agent, rowLedger.ActorAgent
	}
	return by
}

// apiRow is a row as the API answers it: its version is its ETag.
type apiRow struct{ *model.Row }

func (r apiRow) APIVersion() int64 { return r.Version }

// apiWritten is a row as the API answers a write of it: the row, its version
// as its ETag, and the change set the write stored (Written).
type apiWritten struct{ *Written }

func (r apiWritten) APIVersion() int64 { return r.Version }

// idArg is the id an argument names; one that isn't an id names nothing there.
func idArg(args map[string]any, name string) (uuid.UUID, error) {
	s, _ := args[name].(string)
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, v1api.Errorf(http.StatusNotFound, "not_found", "", "there is no %s %q", name, s)
	}
	return id, nil
}

// int64Of is a whole number as a body (json.Number) or MCP (float64) gives it.
func int64Of(v any) int64 {
	switch n := v.(type) {
	case json.Number:
		i, _ := n.Int64()
		return i
	case float64:
		return int64(n)
	}
	return 0
}

// retryRows is how many seconds a caller waits before asking again for rows
// still being worked out, or that couldn't be read just now.
const retryRows = 5

// apiErr is a table's refusal in the API's one error body. A query the table
// can't answer (a filter or sort that isn't one, a field it hasn't got, a page
// marker from another query) is the caller's to fix: a 400 in the query's own
// words (business/RowQuery Refusal), as the app's view says it. Rows still
// being worked out, or that couldn't be read just now, are the server's: a
// 503 that says when to ask again. Anything else stays the API's 500.
func apiErr(err error) error {
	var re model.RowErrors
	if errors.As(err, &re) && len(re) == 1 {
		err = re[0].Err
	}
	var conflict *RowConflict
	var working *rowQuery.StillWorkingOut
	var rows BatchErrors
	var bad *inputError
	switch {
	// First: a fill still running is also ErrTooManyToWorkOut (its Is), and
	// it's the server's to finish, not the caller's to fix.
	case errors.As(err, &working):
		e := v1api.Errorf(http.StatusServiceUnavailable, "working_out", "send it again in a few seconds", "%s", rowQuery.Refusal(working))
		e.RetryAfter = retryRows
		return e
	case errors.Is(err, query.ErrBadSpec), errors.Is(err, rowQuery.ErrCantQuery), errors.Is(err, rowQuery.ErrBadCursor),
		errors.Is(err, rowQuery.ErrTooManyToWorkOut):
		return v1api.Errorf(http.StatusBadRequest, "invalid_argument", "a query's filter and sorts are a view's, as the reference shows",
			"%s", rowQuery.Refusal(err))
	// The store's failure is the server's, the app's words aside (storeFailed).
	case errors.As(err, new(storeFailed)), errors.Is(err, errViewsUnavailable):
		e := v1api.Errorf(http.StatusServiceUnavailable, "unavailable", "send it again in a few seconds", "the table couldn't be read or changed just now")
		e.RetryAfter = retryRows
		return e
	case IsUnavailable(err):
		e := v1api.Errorf(http.StatusServiceUnavailable, "unavailable", "send it again in a few seconds", "the rows couldn't be read just now")
		e.RetryAfter = retryRows
		return e
	// A batch refused writes nothing; its details name each row and why, a
	// row changed since it was read among them: a batch's details always
	// survive. Only a single row's write is refused for its version.
	case errors.As(err, &rows):
		return batchRefused(rows)
	case errors.As(err, &conflict) && len(conflict.Rows) > 0:
		return batchRefused(conflict.Rows)
	case errors.As(err, &conflict):
		return v1api.ErrVersion
	case errors.As(err, &bad):
		return v1api.Errorf(http.StatusBadRequest, "invalid_argument", "", "%s", bad.msg)
	case IsForbidden(err):
		return v1api.Errorf(http.StatusForbidden, "forbidden", "", tableAccess.AnswerForbidden)
	case IsNotFound(err):
		return v1api.Errorf(http.StatusNotFound, "not_found", "", tableAccess.AnswerUnseen)
	case IsRowNotFound(err):
		return v1api.Errorf(http.StatusNotFound, "not_found", "", "there is no such row")
	case errors.Is(err, errFieldNotFound):
		return v1api.Errorf(http.StatusNotFound, "not_found", "", "there is no such field")
	case IsViewNotFound(err):
		return v1api.Errorf(http.StatusNotFound, "not_found", "", "there is no such view")
	case errors.Is(err, errViewLocked):
		return v1api.Errorf(http.StatusConflict, "locked", "a manager of the table unlocks it", "%s", errViewLocked.Error())
	}
	return err
}

// apiRowError is one row's part of a refused batch, as the API says it: the
// create's place in create (index), or the row, the field, and why.
type apiRowError struct {
	Index   *int   `json:"index,omitempty"`
	Row     string `json:"row,omitempty"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

// batchRefused is a refused batch as the API's one error body: the caller's
// to fix (400), with every row it refused in the details.
func batchRefused(rows BatchErrors) error {
	e := v1api.Errorf(http.StatusBadRequest, "invalid_argument", "fix the rows the details name, then send the batch again", "%s", rows.Error())
	e.Details = rowErrors(rows)
	return e
}

// rowErrors is a refused batch's rows as the error body's details: plain
// values, which the answer writes as they are.
func rowErrors(rows BatchErrors) []apiRowError {
	out := make([]apiRowError, len(rows))
	for i, r := range rows {
		out[i] = apiRowError{Index: r.Index, Field: r.Field, Message: r.Msg}
		if r.Row != uuid.Nil {
			out[i].Row = r.Row.String()
		}
	}
	return out
}

// storeFailed is a read or write the store didn't take, in the app's words
// ("failed to load fields"): the server's to answer (the API's 503), where a
// plain error is the app's refusal of what was sent. The app's controller
// words both as before (writeErr).
type storeFailed string

func (e storeFailed) Error() string { return string(e) }
