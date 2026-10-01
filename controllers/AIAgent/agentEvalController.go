package controllers

// HTTP handlers for the agent evaluation harness: saved test scenarios and
// scored runs. Same capability gate (agent.manage) + ownership model as the
// rest of the builder. Reads are cheap; runs execute the agent in DRY-RUN (no
// writes) and score the outcome.

import (
	"encoding/json"
	"net/http"

	business "github.com/akashc777/OneCamp/business/AIAgent"
	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// ListEvalScenarios GET /agents/{id}/eval/scenarios
func ListEvalScenarios(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid agent id"})
		return
	}
	list, err := business.ListScenarios(ctx, id, actorFrom(r))
	if err != nil {
		writeManageErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": list})
}

// CreateEvalScenario POST /agents/{id}/eval/scenarios
func CreateEvalScenario(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid agent id"})
		return
	}
	var in business.ScenarioInput
	if derr := json.NewDecoder(r.Body).Decode(&in); derr != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	v, err := business.CreateScenario(ctx, id, in, actorFrom(r))
	if err != nil {
		writeManageErr(w, err)
		return
	}
	auditBusiness.Record(r, "agent.eval.scenario.create", auditBusiness.CategoryIntegration,
		"Created agent eval scenario", map[string]interface{}{"agent_id": id.String(), "scenario_id": v.Id.String()})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": v})
}

// UpdateEvalScenario POST /agents/eval/scenarios/{sid}/update
func UpdateEvalScenario(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sid, err := uuid.Parse(chi.URLParam(r, "sid"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid scenario id"})
		return
	}
	var in business.ScenarioInput
	if derr := json.NewDecoder(r.Body).Decode(&in); derr != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	v, err := business.UpdateScenario(ctx, sid, in, actorFrom(r))
	if err != nil {
		writeManageErr(w, err)
		return
	}
	auditBusiness.Record(r, "agent.eval.scenario.update", auditBusiness.CategoryIntegration,
		"Updated agent eval scenario", map[string]interface{}{"scenario_id": sid.String()})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": v})
}

// DeleteEvalScenario POST /agents/eval/scenarios/{sid}/delete
func DeleteEvalScenario(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sid, err := uuid.Parse(chi.URLParam(r, "sid"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid scenario id"})
		return
	}
	if derr := business.DeleteScenario(ctx, sid, actorFrom(r)); derr != nil {
		writeManageErr(w, derr)
		return
	}
	auditBusiness.Record(r, "agent.eval.scenario.delete", auditBusiness.CategoryIntegration,
		"Deleted agent eval scenario", map[string]interface{}{"scenario_id": sid.String()})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "deleted"})
}

// RunEvalScenario POST /agents/eval/scenarios/{sid}/run — execute + score one.
func RunEvalScenario(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sid, err := uuid.Parse(chi.URLParam(r, "sid"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid scenario id"})
		return
	}
	res, err := business.RunScenario(ctx, sid, actorFrom(r))
	if err != nil {
		writeManageErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": res})
}

// RunEvalSuite POST /agents/{id}/eval/run — execute + score all active scenarios.
func RunEvalSuite(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid agent id"})
		return
	}
	res, err := business.RunSuite(ctx, id, actorFrom(r))
	if err != nil {
		writeManageErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": res})
}

// GetEvalSummary GET /agents/{id}/eval/summary — latest-suite rollup for the badge.
func GetEvalSummary(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid agent id"})
		return
	}
	sum, err := business.EvalSummary(ctx, id, actorFrom(r))
	if err != nil {
		writeManageErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": sum})
}

// GetEvalSummaryBatch GET /agents/eval/summary — per-agent eval rollup for the
// whole list (no N+1), keyed by agent id. Actor-scoped.
func GetEvalSummaryBatch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out, err := business.EvalSummaryBatch(ctx, actorFrom(r))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetEvalSummaryBatch err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "failed to load eval summaries"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": out})
}

// GetAgentOutcomes GET /agents/outcomes — per-agent acceptance record for the
// whole list (no N+1), keyed by agent id. Actor-scoped.
//
// Served beside the eval summary rather than inside it because they answer
// different questions and one must not be mistaken for the other: the eval
// rollup is the agent against its own test cases, this is the agent against the
// people it works for.
func GetAgentOutcomes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out, err := business.AgentOutcomeBatch(ctx, actorFrom(r))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetAgentOutcomes err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "failed to load agent outcomes"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": out})
}

// GetAgentOutcome GET /agents/{id}/outcome — one agent's acceptance record.
func GetAgentOutcome(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid agent id"})
		return
	}
	out, err := business.AgentOutcomeFor(ctx, actorFrom(r), id)
	if err != nil {
		writeManageErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": out})
}

// ReviewAgentLearning GET /agents/{id}/learning
//
// What this agent's own history suggests should be checked or changed. Nothing
// here has been applied: it is a review screen, and accepting a proposal goes
// through the ordinary scenario-creation endpoint so an accepted proposal is
// indistinguishable from a hand-written scenario afterwards.
func ReviewAgentLearning(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid agent id"})
		return
	}
	review, err := business.ReviewLearning(ctx, id, actorFrom(r))
	if err != nil {
		writeManageErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": review})
}
