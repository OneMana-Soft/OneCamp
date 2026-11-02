// Package controller serves a project's plan (linear-15) and the workspace's
// project statuses and labels. Who may do what is business/ProjectPlan's
// package comment.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	principalBusiness "github.com/akashc777/OneCamp/business/Principal"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	planBusiness "github.com/akashc777/OneCamp/business/ProjectPlan"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectaccess "github.com/akashc777/OneCamp/controllers/ProjectAccess"
	"github.com/akashc777/OneCamp/controllers/httpx"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/ProjectPlan"
	softDelete "github.com/akashc777/OneCamp/models/postgres/SoftDelete"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

const (
	denied     = "Only the project's admins and the members who post its updates can change its plan."
	deniedLead = "Only the project's admins choose its lead."
	archived   = "This project is archived, so its plan stays as it was."
	// asLead is what a lead is made, as principalBusiness.Unassignable says
	// it: "Sam can't be made the project's lead: their account is deactivated."
	asLead = "made the project's lead"
)

// answer writes err: a Refusal in its words (400), a project deleted a moment
// ago (404), a clash in its own words (409: a person who can't be given work,
// or a change made at the same moment), anything else as a retry.
func answer(w http.ResponseWriter, r *http.Request, where string, err error) {
	var refusal planBusiness.Refusal
	if errors.As(err, &refusal) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": string(refusal)})
		return
	}
	if errors.Is(err, model.ErrNoProject) {
		helpers.WriteNotFound(w, "That project was deleted a moment ago.")
		return
	}
	helpers.WriteWriteFailure(w, r, err, "Couldn't save the plan.", "controllers/ProjectPlan/"+where)
}

// GetPlan is GET /project/{project_uuid}/plan: whoever may see the project
// reads its plan, and what they may change.
func GetPlan(w http.ResponseWriter, r *http.Request) {
	id, _, s, ok := projectaccess.RequireStanding(w, r, workAccess.NeedView, denied)
	if !ok {
		return
	}
	views, err := planBusiness.Views(r.Context(), []uuid.UUID{id})
	if err != nil {
		answer(w, r, "GetPlan", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "ok", "data": map[string]any{
		"plan":     views[id],
		"can_edit": !s.Archived && s.Can(workAccess.NeedPostUpdate),
		"can_lead": !s.Archived && s.Can(workAccess.NeedManage),
	}})
}

// SetPlan is POST /project/{project_uuid}/plan: a change to the plan (its
// parts as business/ProjectPlan's Change), answered with the plan as it now
// is. Choosing the lead asks for the project's admins; the rest for whoever
// posts its updates.
func SetPlan(w http.ResponseWriter, r *http.Request) {
	var c planBusiness.Change
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&c); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a change to a plan."})
		return
	}
	deny := denied
	if c.Lead != nil {
		deny = deniedLead
	}
	id, _, s, ok := projectaccess.RequireStanding(w, r, c.Need(), deny)
	if !ok {
		return
	}
	if s.Archived {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": archived})
		return
	}
	u, ok := httpx.MustUser(w, r)
	if !ok {
		return
	}
	view, err := planBusiness.Save(r.Context(), id, c, u.UserPostgresInfo.Id, func(ctx context.Context, person uuid.UUID) (bool, error) {
		return mayLead(ctx, id, u.UserDgraphInfo.Uid, person)
	})
	if err != nil {
		answer(w, r, "SetPlan", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "ok", "data": map[string]any{"plan": view}})
}

// mayLead answers whether person may lead the project: one of its admins or
// members, never a guest. The project a request reads is read for its caller
// (its member and admin counts are the caller's, so workAccess.Of over it
// would give anyone outside the caller's standing), so it is read again for
// the person, as a member's role change reads it (controllers/Project's
// SetMemberRole), with the counts made theirs; their role row then lowers
// them as Of does (a viewer or commenter doesn't lead). A deactivated member
// keeps their place (reactivating gives it back) but takes no new work, so
// they're refused by principalBusiness's one rule, in its words (a 409).
func mayLead(ctx context.Context, project uuid.UUID, callerUID string, person uuid.UUID) (bool, error) {
	p, err := projectBusiness.GetBasicDgraphProjectInfoWithGivenMemberUUID(ctx, project.String(), callerUID, person.String())
	if err != nil || p == nil || len(p.Members) == 0 || !strings.EqualFold(p.Members[0].Uuid, person.String()) || p.Members[0].IsExternal {
		return false, err
	}
	if err := principalBusiness.Unassignable(p.Members[0], asLead); err != nil {
		return false, err
	}
	theirs := *p
	theirs.IsProjectMember, theirs.IsProjectAdmin, theirs.Team = 1, 0, nil
	if len(p.Admins) > 0 && strings.EqualFold(p.Admins[0].Uuid, person.String()) {
		theirs.IsProjectAdmin = 1
	}
	st, err := workAccess.Of(ctx, person.String(), &theirs)
	return st.Role == workAccess.RoleAdmin || st.Role == workAccess.RoleMember, err
}

// Registry is GET /project/plan/registry: the five categories, the
// workspace's own statuses and its labels, and the colours they take. Any
// signed-in person reads it.
func Registry(w http.ResponseWriter, r *http.Request) {
	if _, ok := httpx.MustUser(w, r); !ok {
		return
	}
	reg, err := planBusiness.ReadRegistry(r.Context())
	if err != nil {
		answer(w, r, "Registry", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "ok", "data": reg})
}

type edit struct {
	Name     *string `json:"name"`
	Category string  `json:"category"`
	Color    *string `json:"color"`
	Position *int    `json:"position"`
}

func decodeEdit(w http.ResponseWriter, r *http.Request) (edit, bool) {
	var e edit
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&e); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a status or a label."})
		return e, false
	}
	return e, true
}

func idParam(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteNotFound(w, "There's no such status or label.")
	}
	return id, err == nil
}

func done(w http.ResponseWriter, r *http.Request, where string, live bool, err error) {
	switch {
	case err != nil:
		answer(w, r, where, err)
	case !live:
		helpers.WriteNotFound(w, "It was deleted a moment ago.")
	default:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "ok"})
	}
}

// AddStatus is POST /project/plan/statuses {name, category, color}: a
// workspace admin adds a status of the workspace's own.
func AddStatus(w http.ResponseWriter, r *http.Request) {
	u, ok := httpx.RequireAdmin(w, r)
	if !ok {
		return
	}
	e, ok := decodeEdit(w, r)
	if !ok {
		return
	}
	n, c := "", ""
	if e.Name != nil {
		n = *e.Name
	}
	if e.Color != nil {
		c = *e.Color
	}
	s, err := planBusiness.AddStatus(r.Context(), n, e.Category, c, u.UserPostgresInfo.Id)
	if err != nil {
		answer(w, r, "AddStatus", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "ok", "data": s})
}

// EditStatus is POST /project/plan/statuses/{id}/edit {name?, color?, position?}.
func EditStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := httpx.RequireAdmin(w, r); !ok {
		return
	}
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	e, ok := decodeEdit(w, r)
	if !ok {
		return
	}
	live, err := planBusiness.EditStatus(r.Context(), id, e.Name, e.Color, e.Position)
	done(w, r, "EditStatus", live, err)
}

// DeleteStatus is POST /project/plan/statuses/{id}/delete: its projects read
// as its category's built-in until a restore (POST /restore/project_status/{id}).
func DeleteStatus(w http.ResponseWriter, r *http.Request) {
	u, ok := httpx.RequireAdmin(w, r)
	if !ok {
		return
	}
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	live, err := model.DeleteStatus(r.Context(), id, u.UserPostgresInfo.Id)
	done(w, r, "DeleteStatus", live, err)
}

// AddLabel is POST /project/plan/labels {name, color}: a workspace admin adds
// a label from the workspace's settings.
func AddLabel(w http.ResponseWriter, r *http.Request) {
	u, ok := httpx.RequireAdmin(w, r)
	if !ok {
		return
	}
	e, ok := decodeEdit(w, r)
	if !ok {
		return
	}
	n, c := "", ""
	if e.Name != nil {
		n = *e.Name
	}
	if e.Color != nil {
		c = *e.Color
	}
	l, err := planBusiness.AddLabel(r.Context(), n, c, u.UserPostgresInfo.Id)
	if err != nil {
		answer(w, r, "AddLabel", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "ok", "data": l})
}

// EditLabel is POST /project/plan/labels/{id}/edit {name?, color?}.
func EditLabel(w http.ResponseWriter, r *http.Request) {
	if _, ok := httpx.RequireAdmin(w, r); !ok {
		return
	}
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	e, ok := decodeEdit(w, r)
	if !ok {
		return
	}
	live, err := planBusiness.EditLabel(r.Context(), id, e.Name, e.Color)
	done(w, r, "EditLabel", live, err)
}

// DeleteLabel is POST /project/plan/labels/{id}/delete: it leaves every
// project until a restore (POST /restore/project_label/{id}).
func DeleteLabel(w http.ResponseWriter, r *http.Request) {
	u, ok := httpx.RequireAdmin(w, r)
	if !ok {
		return
	}
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	live, err := model.DeleteLabel(r.Context(), id, u.UserPostgresInfo.Id)
	done(w, r, "DeleteLabel", live, err)
}

// restore is the restore route's handler for what, which back brings back.
func restore(what string, back func(context.Context, uuid.UUID) error) func(http.ResponseWriter, *http.Request, uuid.UUID, softDelete.Owner) {
	return func(w http.ResponseWriter, r *http.Request, id uuid.UUID, _ softDelete.Owner) {
		if _, ok := httpx.RequireAdmin(w, r); !ok {
			return
		}
		// A taken name is a 400, as a task field's and a task status's are.
		err := back(r.Context(), id)
		if errors.Is(err, softDelete.ErrTaken) {
			err = planBusiness.Refusal("A live " + what + " has its name now. Rename that one first.")
		}
		done(w, r, "Restore "+what, true, err)
	}
}

// RestoreStatus and RestoreLabel are the restore route's (POST
// /restore/{kind}/{id}) for the workspace's statuses and labels: its admins',
// while the workspace has room, as for a new one. A label comes back to the
// projects that carried it, except one that has taken its most labels since
// (business/ProjectPlan's RestoreLabel).
var (
	RestoreStatus = restore("status", planBusiness.RestoreStatus)
	RestoreLabel  = restore("label", planBusiness.RestoreLabel)
)
