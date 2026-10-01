package controllers

// HTTP handlers for the reusable-skills library. Mounted under the same
// agent.manage capability group as the rest of the builder. Mutations are
// audit-logged; edit/delete are creator-or-admin (enforced in the business
// layer).

import (
	"encoding/json"
	"net/http"
	"strings"

	business "github.com/akashc777/OneCamp/business/AIAgent"
	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// ListSkills GET /agent-skills
func ListSkills(w http.ResponseWriter, r *http.Request) {
	skills, err := business.ListSkills(r.Context())
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "failed to load skills"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": skills})
}

// CreateSkill POST /agent-skills
func CreateSkill(w http.ResponseWriter, r *http.Request) {
	var in business.SkillInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	sk, err := business.CreateSkill(r.Context(), in, actorFrom(r))
	if err != nil {
		writeManageErr(w, err)
		return
	}
	auditBusiness.Record(r, "agent.skill.create", auditBusiness.CategoryIntegration,
		"Created agent skill", map[string]interface{}{"skill_id": sk.Id.String()})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": sk})
}

// UpdateSkill POST /agent-skills/{id}/update
func UpdateSkill(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid skill id"})
		return
	}
	var in business.SkillInput
	if derr := json.NewDecoder(r.Body).Decode(&in); derr != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	sk, uerr := business.UpdateSkill(r.Context(), id, in, actorFrom(r))
	if uerr != nil {
		writeManageErr(w, uerr)
		return
	}
	auditBusiness.Record(r, "agent.skill.update", auditBusiness.CategoryIntegration,
		"Updated agent skill", map[string]interface{}{"skill_id": id.String()})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": sk})
}

// DeleteSkill POST /agent-skills/{id}/delete
func DeleteSkill(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid skill id"})
		return
	}
	if derr := business.DeleteSkill(r.Context(), id, actorFrom(r)); derr != nil {
		writeManageErr(w, derr)
		return
	}
	auditBusiness.Record(r, "agent.skill.delete", auditBusiness.CategoryIntegration,
		"Deleted agent skill", map[string]interface{}{"skill_id": id.String()})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "deleted"})
}

// GetSkillUsage GET /agent-skills/{id}/usage — which agents this skill is
// attached to.
//
// The blast radius of an edit, meant to be seen BEFORE one. A skill is a single
// text shared by many agents, and until now the person rewriting it could not
// see how many they were about to change.
func GetSkillUsage(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid skill id"})
		return
	}
	agents, err := business.SkillUsage(r.Context(), id)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "failed to load skill usage"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]interface{}{
		"agent_count": len(agents),
		"agents":      agents,
	}})
}

// ListSkillRevisions GET /agent-skills/{id}/revisions — the skill's history.
func ListSkillRevisions(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid skill id"})
		return
	}
	revs, err := business.SkillRevisions(r.Context(), id, 50)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "failed to load revisions"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": revs})
}

// RevertSkill POST /agent-skills/{id}/revert — restore a previous version.
func RevertSkill(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid skill id"})
		return
	}
	var in struct {
		RevisionID string `json:"revision_id"`
	}
	if derr := json.NewDecoder(r.Body).Decode(&in); derr != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	revID, perr := uuid.Parse(strings.TrimSpace(in.RevisionID))
	if perr != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid revision id"})
		return
	}
	skill, err := business.RevertSkill(r.Context(), id, revID, actorFrom(r))
	if err != nil {
		writeManageErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": skill})
}
