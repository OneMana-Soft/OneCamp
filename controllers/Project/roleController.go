package controllers

// What people may do in a project (Work Access, A1): someone's role, and the
// project's own "Who can change tasks".

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	business "github.com/akashc777/OneCamp/business/Project"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectaccess "github.com/akashc777/OneCamp/controllers/ProjectAccess"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// SetMemberRole gives someone in the project a role: admin, member,
// commenter or viewer. POST /project/{project_uuid}/members/{user_uuid}/role
// {role}. For the project's admins and its team's admins; its creator stays
// one of its admins.
func SetMemberRole(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me, _ := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	var in struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That request couldn't be read."})
		return
	}
	to, ok := workAccess.ParseRole(strings.TrimSpace(in.Role))
	if !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "A role is admin, member, commenter or viewer."})
		return
	}
	projectID, err := uuid.Parse(chi.URLParam(r, "project_uuid"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a project."})
		return
	}
	member, err := uuid.Parse(chi.URLParam(r, "user_uuid"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a person."})
		return
	}
	p, err := business.GetBasicDgraphProjectInfoWithGivenMemberUUID(ctx, projectID.String(), me.UserDgraphInfo.Uid, member.String())
	if err != nil {
		helpers.WriteReadFailure(w, r, err, "project", "controllers/SetMemberRole")
		return
	}
	manages, ok := managesPeople(w, r, p)
	if !ok {
		return
	}
	change := changeRole
	if to == workAccess.RoleAdmin {
		change = makeAdmin
	}
	if status, msg := memberChangeRefusal(p, manages, change, me.UserDgraphInfo.Uuid, member.String()); status != 0 {
		helpers.WriteJSON(w, status, helpers.Envolope{"msg": msg})
		return
	}
	target := p.Members[0]
	if target.IsExternal {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "A guest can't have a role here. Share the project with them instead."})
		return
	}
	if err := business.SetRole(ctx, me.UserPostgresInfo.Id, projectID, p.Uid, target, len(p.Admins) > 0, to); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/SetMemberRole err: %+v", err)
		helpers.WriteRetryLater(w, "Couldn't change their role.")
		return
	}
	business.RecordMemberChange(r, resourceEventBusiness.ProjectChanged, "project.member.role", "Changed what someone can do in a project", p.Uuid, target.Uuid, to)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": doneWords[changeRole], "data": map[string]string{"user_uuid": target.Uuid, "role": string(to)}})
}

// ProjectPolicies is the policies a project can set for itself, as they stand
// for it: today "Who can change tasks". GET /project/{project_uuid}/policies.
// For the people in the project.
func ProjectPolicies(w http.ResponseWriter, r *http.Request) {
	id, _, _, ok := projectaccess.RequireStanding(w, r, workAccess.NeedView, "")
	if !ok {
		return
	}
	in, err := policyBusiness.Admin.InfoIn(r.Context(), policyBusiness.ProjectsEditWho.Key, projectScope(id))
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/ProjectPolicies err: %+v", err)
		helpers.WriteRetryLater(w, "Couldn't load the project's settings.")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": []policyBusiness.Info{in}})
}

// SetProjectPolicy sets one of the project's own policies, or with a null
// value gives it back to the workspace's. PUT /project/{project_uuid}/policies/{key}
// {value}. For the project's admins; recorded in the audit log.
func SetProjectPolicy(w http.ResponseWriter, r *http.Request) {
	id, _, ok := projectaccess.Require(w, r, workAccess.NeedManage, "Only the project's admins choose who can change its tasks.")
	if !ok {
		return
	}
	var in struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That request couldn't be read."})
		return
	}
	me, _ := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	by := policyBusiness.Changer{UserID: me.UserPostgresInfo.Id, Email: me.UserPostgresInfo.EmailID}
	key, scope := chi.URLParam(r, "key"), projectScope(id)
	var info policyBusiness.Info
	var err error
	if len(in.Value) == 0 || strings.TrimSpace(string(in.Value)) == "null" {
		info, err = policyBusiness.Admin.ClearIn(r.Context(), key, scope, by)
	} else {
		info, err = policyBusiness.Admin.SetIn(r.Context(), key, scope, in.Value, by)
	}
	switch {
	case err == nil:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": info})
	case errors.Is(err, policyBusiness.ErrUnknownPolicy), errors.Is(err, policyBusiness.ErrNotScoped):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "A project can't set that."})
	case errors.Is(err, policyBusiness.ErrBadValue):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Choose members, members on the tasks assigned to them, or admins only."})
	case errors.Is(err, policyBusiness.ErrNotRecorded):
		helpers.WriteRetryLater(w, "The change couldn't be recorded, so it wasn't made.")
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/SetProjectPolicy err: %+v", err)
		helpers.WriteRetryLater(w, "Couldn't save the project's setting.")
	}
}

// projectScope is a project's own policy values.
func projectScope(id uuid.UUID) policyBusiness.Scope {
	return policyBusiness.Scope{Kind: policyBusiness.ScopeProject, ID: id.String()}
}
