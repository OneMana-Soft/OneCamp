package controllers

// The versioned public API surface (/v1). These handlers run behind the
// VerifyApiToken middleware (so a token's owner is the acting user) and a
// per-route RequireScope gate. They reuse the same business layer as the app,
// so the API can never do something the token owner couldn't, and behavior
// stays consistent with the UI.

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	appBusiness "github.com/akashc777/OneCamp/business/App"
	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	rowLedger "github.com/akashc777/OneCamp/business/RowLedger"
	"github.com/akashc777/OneCamp/helpers"
	dataTableModel "github.com/akashc777/OneCamp/models/postgres/DataTable"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// actor is the token's owner as a table's rules see them: as a member, never as
// an admin, since every /v1 route is a token's and a token carries none of its
// owner's admin powers (helpers.ActsThroughToken). So a table is theirs, open to
// the workspace, or refused, as for any member.
func actor(r *http.Request) dataTableBusiness.Actor {
	userInfo, _ := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	return dataTableBusiness.Actor{
		UserID:  userInfo.UserPostgresInfo.Id,
		IsAdmin: userInfo.UserPostgresInfo.IsAdmin && !helpers.ActsThroughToken(r.Context()),
		Kind:    rowLedger.ActorAPI,
	}
}

func tableErr(w http.ResponseWriter, err error) {
	switch {
	case dataTableBusiness.IsForbidden(err):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "no access to this table"})
	case dataTableBusiness.IsNotFound(err):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "table not found"})
	default:
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
	}
}

// Me GET /v1/me — the authenticated identity behind the token.
func Me(w http.ResponseWriter, r *http.Request) {
	userInfo, _ := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]interface{}{
		"id":       userInfo.UserPostgresInfo.Id.String(),
		"is_admin": userInfo.UserPostgresInfo.IsAdmin,
		"scopes":   helpers.GetApiScopes(r.Context()),
	}})
}

// ListTables GET /v1/tables — scope tables:read. An app's, only the tables it
// was added to.
func ListTables(w http.ResponseWriter, r *http.Request) {
	items, err := dataTableBusiness.ListTables(r.Context(), actor(r))
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "failed to load tables"})
		return
	}
	items, _ = narrowed(r, items, func(t *dataTableModel.DataTable) []appBusiness.Place {
		return []appBusiness.Place{{Kind: "table", ID: t.Id.String()}}
	})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": items})
}

// GetTable GET /v1/tables/{id} — scope tables:read. Returns the full bundle.
func GetTable(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid table id"})
		return
	}
	bundle, berr := dataTableBusiness.GetBundle(r.Context(), id, actor(r))
	if berr != nil {
		tableErr(w, berr)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": bundle})
}

// AggregateTable POST /v1/tables/{id}/aggregate — scope tables:read. Computes a
// grouped aggregation (count/sum/avg/min/max, optional group-by + filters) over
// the table's rows and returns the buckets, so an API/SDK client can pull a
// totals/breakdown/trend without downloading every row.
func AggregateTable(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid table id"})
		return
	}
	var spec dataTableBusiness.QuerySpec
	if derr := json.NewDecoder(r.Body).Decode(&spec); derr != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	res, _, aerr := dataTableBusiness.AggregateTable(r.Context(), id, actor(r), spec)
	if aerr != nil {
		tableErr(w, aerr)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": res})
}

// CreateRow POST /v1/tables/{id}/rows — scope tables:write.
func CreateRow(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid table id"})
		return
	}
	var body dataTableBusiness.RowInput
	if derr := json.NewDecoder(r.Body).Decode(&body); derr != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	row, rerr := dataTableBusiness.CreateRow(r.Context(), id, body, actor(r))
	if rerr != nil {
		tableErr(w, rerr)
		return
	}
	auditBusiness.Record(r, "api.create_table_row", auditBusiness.CategoryIntegration,
		"Public API call: create_table_row", map[string]interface{}{"token_id": helpers.GetApiTokenID(r.Context()), "table_id": id.String()})
	helpers.WriteJSON(w, http.StatusCreated, helpers.Envolope{"data": row})
}

// ───────────────────────── tasks / projects / messages ─────────────────────────
// (direct.go)

// decodeParams reads a JSON object body into a string-keyed param map, coercing
// scalar values the way the tool executors expect.
func decodeParams(r *http.Request) (map[string]string, error) {
	if r.Body == nil {
		return map[string]string{}, nil
	}
	return paramsOf(r.Body)
}

// paramsOf is decodeParams over a body: also what a route's Reach reads its
// place with (param), so both read one id.
func paramsOf(body io.Reader) (map[string]string, error) {
	raw := map[string]interface{}{}
	if err := json.NewDecoder(body).Decode(&raw); err != nil && err.Error() != "EOF" {
		return nil, err
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		switch val := v.(type) {
		case string:
			out[k] = val
		case bool:
			if val {
				out[k] = "true"
			} else {
				out[k] = "false"
			}
		case float64:
			if val == float64(int64(val)) {
				out[k] = strconv.FormatInt(int64(val), 10)
			} else {
				out[k] = strconv.FormatFloat(val, 'g', -1, 64)
			}
		case nil:
			out[k] = ""
		default:
			b, _ := json.Marshal(val)
			out[k] = string(b)
		}
	}
	return out, nil
}
