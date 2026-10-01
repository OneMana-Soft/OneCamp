package controllers

// The versioned public API surface (/v1). These handlers run behind the
// VerifyApiToken middleware (so a token's owner is the acting user) and a
// per-route RequireScope gate. They reuse the same business layer as the app,
// so the API can never do something the token owner couldn't, and behavior
// stays consistent with the UI.

import (
	"encoding/json"
	"net/http"
	"strconv"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func actor(r *http.Request) dataTableBusiness.Actor {
	userInfo, _ := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	return dataTableBusiness.Actor{
		UserID:  userInfo.UserPostgresInfo.Id,
		IsAdmin: userInfo.UserPostgresInfo.IsAdmin,
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

// ListTables GET /v1/tables — scope tables:read.
func ListTables(w http.ResponseWriter, r *http.Request) {
	items, err := dataTableBusiness.ListTables(r.Context(), actor(r))
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "failed to load tables"})
		return
	}
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
	var body struct {
		Values   map[string]interface{} `json:"values"`
		Position float64                `json:"position"`
	}
	if derr := json.NewDecoder(r.Body).Decode(&body); derr != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	row, rerr := dataTableBusiness.CreateRow(r.Context(), id, dataTableBusiness.RowInput{Values: body.Values, Position: body.Position}, actor(r))
	if rerr != nil {
		tableErr(w, rerr)
		return
	}
	auditBusiness.Record(r, "api.create_table_row", auditBusiness.CategoryIntegration,
		"Public API call: create_table_row", map[string]interface{}{"token_id": helpers.GetApiTokenID(r.Context()), "table_id": id.String()})
	helpers.WriteJSON(w, http.StatusCreated, helpers.Envolope{"data": row})
}

// ───────────────────────── tasks / projects / messages ─────────────────────────

// runTool dispatches a public AI tool by name as the token owner, reusing the
// exact permission-checked executors the in-app assistant uses. The scope gate
// is applied by the route's RequireScope middleware. The response carries the
// human-readable result plus any structured action metadata the executor
// emitted (ids, names) so SDK callers can chain operations.
func runTool(w http.ResponseWriter, r *http.Request, toolName string, params map[string]string) {
	helpers.WriteJSON(w, http.StatusNotImplemented, helpers.Envolope{"msg": "AI features are not supported on this build"})
}

// decodeParams reads a JSON object body into a string-keyed param map, coercing
// scalar values the way the tool executors expect.
func decodeParams(r *http.Request) (map[string]string, error) {
	raw := map[string]interface{}{}
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil && err.Error() != "EOF" {
			return nil, err
		}
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

// ListTasksV1 GET /v1/tasks — scope tasks:read. Lists the token owner's tasks.
// Optional query params: status, filter (overdue), search.
func ListTasksV1(w http.ResponseWriter, r *http.Request) {
	params := map[string]string{}
	if v := r.URL.Query().Get("status"); v != "" {
		params["status"] = v
	}
	if v := r.URL.Query().Get("filter"); v != "" {
		params["filter"] = v
	}
	if v := r.URL.Query().Get("search"); v != "" {
		params["search"] = v
	}
	runTool(w, r, "list_tasks", params)
}

// CreateTaskV1 POST /v1/tasks — scope tasks:write. Body: task_name, project_uuid,
// description, priority, assignee_uuid.
func CreateTaskV1(w http.ResponseWriter, r *http.Request) {
	params, err := decodeParams(r)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	runTool(w, r, "create_task", params)
}

// UpdateTaskStatusV1 POST /v1/tasks/{id}/status — scope tasks:write. Body: status.
func UpdateTaskStatusV1(w http.ResponseWriter, r *http.Request) {
	params, err := decodeParams(r)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	params["task_uuid"] = chi.URLParam(r, "id")
	runTool(w, r, "update_task_status", params)
}

// ListProjectsV1 GET /v1/projects — scope projects:read.
func ListProjectsV1(w http.ResponseWriter, r *http.Request) {
	runTool(w, r, "list_projects", map[string]string{})
}

// SendMessageV1 POST /v1/messages/channel — scope messages:write. Body:
// channel_uuid, text.
func SendMessageV1(w http.ResponseWriter, r *http.Request) {
	params, err := decodeParams(r)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	runTool(w, r, "send_message", params)
}

// SearchV1 GET/POST /v1/search — scope search:read. Cross-source recall across
// the token owner's workspace, Memory, and connected apps. Query via ?q= or a
// JSON body {"query": "..."}.
func SearchV1(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if query == "" {
		params, err := decodeParams(r)
		if err != nil {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
			return
		}
		query = params["query"]
	}
	runTool(w, r, "search_workspace", map[string]string{"query": query})
}

// SendDMV1 POST /v1/messages/dm — scope messages:write. Body: to_uuid, text.
func SendDMV1(w http.ResponseWriter, r *http.Request) {
	params, err := decodeParams(r)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	runTool(w, r, "send_dm", params)
}
