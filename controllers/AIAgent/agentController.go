package controllers

// HTTP handlers for the Agent Builder. Mounted under a capability-gated group
// (agent.manage): admins always, members when an admin opens the capability.
// Ownership is enforced in the business layer (you manage agents you created;
// admins manage all). Every mutation is audit-logged.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	business "github.com/akashc777/OneCamp/business/AIAgent"
	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func actorFrom(r *http.Request) business.Actor {
	userInfo, _ := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	return business.Actor{
		UserID:  userInfo.UserPostgresInfo.Id,
		IsAdmin: userInfo.UserPostgresInfo.IsAdmin,
		// Carried so a read can be authorized against the SURFACE the work
		// happens on (channel membership, project membership) using each domain's
		// own visibility query, instead of inventing a second permission model.
		DgraphUID: userInfo.UserDgraphInfo.Uid,
	}
}

func writeManageErr(w http.ResponseWriter, err error) {
	switch {
	case business.IsForbidden(err):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "you can only manage agents you created"})
	case business.IsNotFound(err):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "agent not found"})
	default:
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
	}
}

// coworkerDMSuggestions are the curated starter prompts shown in an empty DM
// with the shared "OneCamp AI" coworker, so a new user isn't faced with a blank
// box (the adoption affordance Slack/Notion/Gemini all surface).
var coworkerDMSuggestions = []string{
	"Summarize what I missed across my channels today",
	"What are my open tasks, and which are overdue?",
	"Draft a quick status update for my team",
	"Catch me up on the latest in my projects",
}

// ListDMSuggestions GET /user/dm-ai-suggestions?peer=<uuid> — starter prompts
// for an empty DM with an AI peer (the shared coworker or a DM-able agent).
// Member-accessible; returns an empty list when AI is off, the peer is not an
// AI principal, or (for the shared coworker) the coworker policy is off — so
// there is never a dangling affordance.
func ListDMSuggestions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	empty := func() { helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": []string{}}) }

	peer := strings.TrimSpace(r.URL.Query().Get("peer"))
	if peer == "" {
		empty()
		return
	}
	settings, serr := aiModels.GetSettings(ctx)
	if serr != nil || settings == nil || !settings.Enabled {
		empty()
		return
	}

	// Shared coworker peer (gated on the coworker policy).
	if bot := userBusiness.GetAutomationBot(ctx); bot != nil && (peer == bot.UUID || peer == bot.DgraphUID) {
		if settings.CoworkerEnabled {
			helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": coworkerDMSuggestions})
			return
		}
		empty()
		return
	}

	// DM-able agent peer: generic, agent-aware starters.
	if agent, _, aerr := business.AgentForDMPrincipal(ctx, peer); aerr == nil && agent != nil {
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": agentDMSuggestions(agent.Name)})
		return
	}
	empty()
}

// agentDMSuggestions returns starter prompts for a DM-able agent. Kept generic
// (the agent's own instructions decide what it does) but addressed to it by
// name so the empty state feels personal.
func agentDMSuggestions(name string) []string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "this assistant"
	}
	return []string{
		"What can you help me with?",
		"Show me an example of something you can do",
		"What information do you have access to?",
	}
}

// ListDMTargets GET /user/dm-ai-targets — the DM-able AI teammates a member can
// start a 1:1 DM with (Req 10.1). Member-accessible (NOT agent.manage gated):
// any member can DM a DM-able agent the same way they DM a colleague. Returns
// an empty list when AI is disabled, so there is no dangling affordance for a
// turned-off feature. The shared coworker is surfaced separately (the global
// user list), so this returns only the per-agent DM-able principals.
func ListDMTargets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if s, serr := aiModels.GetSettings(ctx); serr != nil || s == nil || !s.Enabled {
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": []business.DMTarget{}})
		return
	}
	targets, err := business.ListDMableAgentPrincipals(ctx)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/ListDMTargets err: %+v", err)
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": []business.DMTarget{}})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": targets})
}

// ListAgents GET /agents — scoped to the actor (admins see all, members own).
func ListAgents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	items, err := business.ListAgents(ctx, actorFrom(r))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/ListAgents err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "failed to load agents"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": items})
}

// GetAgent GET /agents/{id}
func GetAgent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid agent id"})
		return
	}
	a, err := business.GetAgent(ctx, id, actorFrom(r))
	if err != nil {
		writeManageErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": a})
}

// GetAgentCard GET /agent-card/{botUserId}
//
// The card for the agent behind a bot user. 404 when the bot is not an agent,
// which the client takes as "show the ordinary profile".
func GetAgentCard(w http.ResponseWriter, r *http.Request) {
	botID, err := uuid.Parse(chi.URLParam(r, "botUserId"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid user id"})
		return
	}
	card, err := business.GetAgentCard(r.Context(), botID, actorFrom(r))
	if business.IsNotFound(err) {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "not an agent"})
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/GetAgentCard err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "could not load this agent"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": card})
}

// GetChannelAgents GET /agent-card/channel/{channelId}
//
// The active agents in a channel, for its members.
func GetChannelAgents(w http.ResponseWriter, r *http.Request) {
	chID, err := uuid.Parse(chi.URLParam(r, "channelId"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid channel id"})
		return
	}
	agents, err := business.ChannelAgents(r.Context(), chID, actorFrom(r))
	if business.IsForbidden(err) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "you are not in this channel"})
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/GetChannelAgents err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "could not load this channel's agents"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": agents})
}

// CreateAgent POST /agents
func CreateAgent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var in business.AgentInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	a, err := business.CreateAgent(ctx, in, userInfo.UserPostgresInfo.Id)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	auditBusiness.Record(r, "agent.create", auditBusiness.CategorySettings,
		"Created agent: "+a.Name, map[string]interface{}{"agent_id": a.Id.String()})
	helpers.WriteJSON(w, http.StatusCreated, helpers.Envolope{"data": a})
}

// CheckRemoteBrain POST /agents/agui/check
//
// Tries one run against an AG-UI endpoint and reports what came back, so an
// admin learns that a URL is wrong or a secret is stale while they are typing
// it rather than from a failed run afterwards. Body:
// { "agent_id"?: string, "endpoint": string, "auth_header"?: string, "auth_secret"?: string }.
//
// A transport failure is a 200 carrying ok:false, because "the remote answered
// 401" is the answer to the question, not a failure to answer it. Only a
// request this workspace refuses to make at all is a 400.
func CheckRemoteBrain(w http.ResponseWriter, r *http.Request) {
	var in business.RemoteBrainCheck
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	res, err := business.CheckRemoteBrain(r.Context(), in, actorFrom(r))
	if err != nil {
		writeManageErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": res})
}

// UpdateAgent PUT /agents/{id}
func UpdateAgent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid agent id"})
		return
	}
	var in business.AgentInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	a, err := business.UpdateAgent(ctx, id, in, actorFrom(r))
	if err != nil {
		writeManageErr(w, err)
		return
	}
	auditBusiness.Record(r, "agent.update", auditBusiness.CategorySettings,
		"Updated agent: "+a.Name, map[string]interface{}{"agent_id": a.Id.String()})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": a})
}

// SetAgentActive POST /agents/{id}/active
func SetAgentActive(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid agent id"})
		return
	}
	var body struct {
		IsActive bool `json:"is_active"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := business.SetActive(ctx, id, body.IsActive, actorFrom(r)); err != nil {
		writeManageErr(w, err)
		return
	}
	auditBusiness.Record(r, "agent.toggle", auditBusiness.CategorySettings,
		"Toggled agent", map[string]interface{}{"agent_id": id.String(), "is_active": body.IsActive})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "updated"})
}

// DeleteAgent DELETE /agents/{id}
func DeleteAgent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid agent id"})
		return
	}
	if err := business.DeleteAgent(ctx, id, actorFrom(r)); err != nil {
		writeManageErr(w, err)
		return
	}
	auditBusiness.Record(r, "agent.delete", auditBusiness.CategorySettings,
		"Deleted agent", map[string]interface{}{"agent_id": id.String()})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "deleted"})
}

// ListAgentRuns GET /agents/{id}/runs
func ListAgentRuns(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid agent id"})
		return
	}
	limit := 25
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, perr := strconv.Atoi(l); perr == nil {
			limit = n
		}
	}
	runs, err := business.ListRuns(ctx, id, actorFrom(r), limit)
	if err != nil {
		writeManageErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": runs})
}

// GetAgentActivity GET /agents/activity?limit= — the cross-agent "show your
// work" feed: recent runs across every agent the caller may see (admins: whole
// workspace; members: agents they own), each with what it did + outcome.
// Read-only.
func GetAgentActivity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, perr := strconv.Atoi(l); perr == nil {
			limit = n
		}
	}
	items, err := business.RecentActivity(ctx, actorFrom(r), limit)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetAgentActivity err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "failed to load agent activity"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": items})
}

// GetAgentWork GET /agents/work?limit= — the live "what are my AI teammates
// doing right now, and where are they blocked on me" feed: the open durable jobs
// (queued / working / blocked) across every agent the caller may see (admins:
// whole workspace; members: agents they own). Read-only.
func GetAgentWork(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, perr := strconv.Atoi(l); perr == nil {
			limit = n
		}
	}
	items, err := business.ActiveWork(ctx, actorFrom(r), limit)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetAgentWork err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "failed to load active agent work"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": items})
}

// GetMyAgentWork GET /ai/agent-work?limit= — the MEMBER-facing "AI teammates"
// view: the open durable jobs the caller is personally involved in (ones they
// triggered or that run as them), across ANY agent — so anyone can see what
// their AI teammates are doing for them and where a teammate is blocked waiting
// on them, without the agent.manage capability. Read-only; strictly self-scoped
// in the business/model layer (never widens to other people's work).
func GetMyAgentWork(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, perr := strconv.Atoi(l); perr == nil {
			limit = n
		}
	}
	items, err := business.MyActiveWork(ctx, actorFrom(r), limit)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetMyAgentWork err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "failed to load your agent work"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": items})
}

// GetAgentWorkForEntity GET /ai/agent-work/for/{entityId} — the live agent work
// happening on ONE surface entity (a channel post/thread, a chat message, or a
// project task), so the place where the work is visible can show it and offer to
// stop it. entityId is the id that surface already knows (post / message / task).
//
// Authorization is two-layered in the business layer: a caller sees a job only if
// they are party to it OR can see the surface it runs on (channel/project
// membership, via that surface's own visibility query), and each item reports
// whether THIS caller may stop it. Read-only; an empty list is the normal case.
func GetAgentWorkForEntity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	entityID := strings.TrimSpace(chi.URLParam(r, "entityId"))
	if entityID == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "entity id is required"})
		return
	}
	items, err := business.ActiveWorkForEntity(ctx, actorFrom(r), entityID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetAgentWorkForEntity err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "failed to load agent work"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": items})
}

// StopAgentWork POST /ai/agent-work/{id}/stop — stop an AI teammate's in-flight
// work. Cooperative: a running job is asked to stop and its own worker wraps it
// up (posting what it managed to do); a job that hasn't started is ended
// immediately. Authorization is per job in the business layer (agent owner, the
// person who asked, the user it runs as, or an admin), so being able to call
// this route grants nothing on somebody else's work.
//
// Idempotent by design: stopping an already-stopped or finished job answers 200
// with what is true, because a person pressing Stop twice has done nothing wrong.
func StopAgentWork(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid work id"})
		return
	}
	res, err := business.CancelAgentWork(ctx, actorFrom(r), id)
	switch {
	case errors.Is(err, business.ErrAgentWorkNotFound):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "that agent work no longer exists"})
		return
	case errors.Is(err, business.ErrAgentWorkForbidden):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "you can't stop this agent work"})
		return
	case err != nil:
		helpers.LogErrorWithContext(ctx, "controllers/StopAgentWork err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "failed to stop this agent work"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": res})
}

// DraftAgent POST /agents/draft — turn a natural-language description into a
// starting agent configuration the builder prefills (the human reviews + saves
// via the normal create flow). Body: { "prompt": string }. Never persists.
func DraftAgent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body struct {
		Prompt string `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	draft, err := business.DraftAgent(ctx, actorFrom(r), body.Prompt)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": draft})
}

// workspace; members: their own agents). Read-only.
func GetWorkspaceAgentStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	stats, err := business.WorkspaceAgentStats(ctx, actorFrom(r))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetWorkspaceAgentStats err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "failed to load agent overview"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": stats})
}

// GetAgentHealthBatch GET /agents/health — compact per-agent health signal for
// every agent the actor may see (admins: whole workspace; members: their own),
// keyed by agent id. Powers the at-a-glance status dot per row in the agents
// list. Read-only.
func GetAgentHealthBatch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	health, err := business.AgentHealthBatch(ctx, actorFrom(r))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetAgentHealthBatch err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "failed to load agent health"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": health})
}

// GetAgentStats GET /agents/{id}/stats — reliability + activity rollup for one
// agent (success/failure mix, tokens, avg duration, recent activity). Read-only;
// same ownership gate as the rest of the builder.
func GetAgentStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid agent id"})
		return
	}
	stats, err := business.AgentStats(ctx, id, actorFrom(r))
	if err != nil {
		writeManageErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": stats})
}

// GetAgentSignatures GET /agents/{id}/signatures: checks that the agent's
// recent recorded actions carry its signature and were not altered.
func GetAgentSignatures(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid agent id"})
		return
	}
	report, err := business.AgentSignatures(ctx, id, actorFrom(r))
	if err != nil {
		writeManageErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": report})
}

// ListAgentRoutines GET /agents/{id}/routines — the recurring routines set up
// for an agent (created conversationally), so an owner/admin can review them.
// Read-only; same ownership gate as the rest of the builder.
func ListAgentRoutines(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid agent id"})
		return
	}
	routines, err := business.ListAgentRoutines(ctx, id, actorFrom(r))
	if err != nil {
		writeManageErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": routines})
}

// SetAgentRoutineEnabled POST /agents/{id}/routines/{rid}/enabled — pause or
// resume a routine. Body: { "enabled": bool }.
func SetAgentRoutineEnabled(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid agent id"})
		return
	}
	rid, err := uuid.Parse(chi.URLParam(r, "rid"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid routine id"})
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := business.SetAgentRoutineEnabled(ctx, id, rid, actorFrom(r), body.Enabled); err != nil {
		writeManageErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success"})
}

// DeleteAgentRoutine DELETE /agents/{id}/routines/{rid} — cancel a routine.
func DeleteAgentRoutine(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid agent id"})
		return
	}
	rid, err := uuid.Parse(chi.URLParam(r, "rid"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid routine id"})
		return
	}
	if err := business.DeleteAgentRoutine(ctx, id, rid, actorFrom(r)); err != nil {
		writeManageErr(w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success"})
}

// RunAgent POST /agents/{id}/run — execute the agent now (builder "test").
// Body: { "prompt"?: string, "dry_run"?: bool }. Runs synchronously, bounded by
// the agent's step budget; dry-run previews without performing writes.
func RunAgent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid agent id"})
		return
	}
	var body struct {
		Prompt string `json:"prompt"`
		DryRun bool   `json:"dry_run"`
	}
	// Body is optional; ignore decode errors on an empty body.
	_ = json.NewDecoder(r.Body).Decode(&body)

	outcome, err := business.RunAgentManual(ctx, id, actorFrom(r), body.Prompt, body.DryRun)
	if err != nil {
		writeManageErr(w, err)
		return
	}
	if !body.DryRun {
		auditBusiness.Record(r, "agent.run", auditBusiness.CategorySettings,
			"Ran agent", map[string]interface{}{"agent_id": id.String(), "run_id": outcome.RunID.String(), "status": outcome.Status})
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": outcome})
}

// GetAgentInventory GET /agents/inventory
func GetAgentInventory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	inv, err := business.AgentInventory(ctx, actorFrom(r))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetAgentInventory err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "could not load the agent inventory"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": inv})
}

// RevokeInventoryCredential POST /agents/inventory/credentials/{id}/revoke
//
// An admin revoking someone else's credential is exactly the kind of change the
// audit log exists for, so it names the credential, its maker and its agent.
func RevokeInventoryCredential(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid credential id"})
		return
	}
	t, err := business.RevokeCredential(ctx, id, actorFrom(r))
	switch {
	case business.IsForbidden(err):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "only an admin can revoke someone else's credential"})
		return
	case err != nil:
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": err.Error()})
		return
	}
	meta := map[string]interface{}{
		"token_id":     t.Id.String(),
		"token_prefix": t.TokenPrefix,
		"created_by":   t.CreatedBy.String(),
	}
	if t.AgentId != nil {
		meta["agent_id"] = t.AgentId.String()
	}
	auditBusiness.Record(r, "api_token.revoke", auditBusiness.CategorySecurity,
		"Revoked credential: "+t.Name, meta)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "revoked"})
}
