package business

// Conversational routine tools (create_routine / list_routines / cancel_routine).
//
// These let a person hand an agent standing work from chat — "every weekday at
// 9am summarize this channel" — the way Claude Tag's routines work, but generic:
// many routines per agent per surface. They are registry-free control tools
// handled in the runner loop (like remember/forget): scope-bound to the run's
// channel/DM, always run (creating standing work is self-configuration, not an
// external workspace write), and only advertised when the run has a surface.
//
// A routine re-runs the agent's prompt on its schedule via the routine
// dispatcher (a later slice), on the same owner-identity, permission-checked
// path as an interactive mention. Persistence + the pure spec core live in
// agentRoutineModel.go / routineSpec.go.

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	schedulerBusiness "github.com/akashc777/OneCamp/business/Scheduler"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

const (
	createRoutineToolName = "create_routine"
	listRoutinesToolName  = "list_routines"
	cancelRoutineToolName = "cancel_routine"
	// defaultRoutineMinuteUTC is 09:00 UTC — a sane fire time when the user
	// names a cadence but no time.
	defaultRoutineMinuteUTC = 9 * 60
)

// isRoutineTool reports whether a tool name is one of the routine control tools.
func isRoutineTool(name string) bool {
	return name == createRoutineToolName || name == listRoutinesToolName || name == cancelRoutineToolName
}

// handleRoutineTool executes a routine control tool, recording the outcome on
// rec. Scope-bound: a no-op (skipped) when the run has no channel/DM surface to
// attach the routine to. Every routine is attributed to and run as the agent's
// owner.
func handleRoutineTool(ctx context.Context, agent *model.AiAgent, a ai.ProposedAction, rec *toolCallRecord) {
	sc := agentRunScopeFromCtx(ctx)
	if !sc.hasScope() {
		rec.Skipped = "routines can only be set up in a channel or conversation"
		return
	}
	switch a.ToolName {
	case createRoutineToolName:
		handleCreateRoutine(ctx, agent, sc, a, rec)
	case listRoutinesToolName:
		handleListRoutines(ctx, agent, sc, rec)
	case cancelRoutineToolName:
		handleCancelRoutine(ctx, agent, sc, a, rec)
	}
}

// handleCreateRoutine validates + persists a new routine for the current
// surface. It normalizes the request (routineSpec) so a bad cadence or blank
// prompt yields an actionable error the model relays, never a broken row.
func handleCreateRoutine(ctx context.Context, agent *model.AiAgent, sc agentRunScope, a ai.ProposedAction, rec *toolCallRecord) {
	// Routines post their result to a channel, so they can only be set up in a
	// channel (not a 1:1/group DM) for now. Fail clearly rather than create a
	// routine that would have nowhere to post.
	if sc.ChannelID == "" {
		rec.Error = "I can set up routines in a channel (add me to one and ask there) — not in a direct message yet"
		return
	}

	norm, err := NormalizeRoutineInput(RoutineInput{
		Name:        a.Params["name"],
		Prompt:      a.Params["prompt"],
		Recurrence:  a.Params["recurrence"],
		AtMinuteUTC: parseRoutineFireMinute(a.Params),
	})
	if err != nil {
		rec.Error = err.Error()
		return
	}

	routine := &model.AgentRoutine{
		AgentId:     agent.Id,
		CreatedBy:   agent.CreatedBy,
		GroupId:     sc.GroupID,
		Name:        norm.Name,
		Prompt:      norm.Prompt,
		Recurrence:  norm.Recurrence,
		AtMinuteUTC: norm.AtMinuteUTC,
		Enabled:     true,
	}
	if sc.ChannelID != "" {
		id, perr := uuid.Parse(sc.ChannelID)
		if perr != nil {
			rec.Error = "could not resolve this channel for the routine"
			return
		}
		routine.ChannelId = &id
	}

	id, cerr := model.CreateRoutine(ctx, routine)
	if cerr != nil {
		rec.Error = "could not create the routine: " + cerr.Error()
		return
	}
	rec.Result = fmt.Sprintf("Routine %q created (id %s) — runs %s. Say \"list routines\" to see it or \"cancel routine %s\" to stop it.",
		norm.Name, id, describeCadence(norm.Recurrence, norm.AtMinuteUTC), id)
}

// handleListRoutines lists the active routines for the current surface, so a
// person can see (and then cancel) the standing work in this channel/DM.
func handleListRoutines(ctx context.Context, agent *model.AiAgent, sc agentRunScope, rec *toolCallRecord) {
	routines, err := model.ListRoutinesForAgent(ctx, agent.Id)
	if err != nil {
		rec.Error = "could not list routines: " + err.Error()
		return
	}
	var lines []string
	for _, r := range routines {
		if !routineInScope(r, sc) {
			continue
		}
		status := ""
		if !r.Enabled {
			status = " (paused)"
		}
		lines = append(lines, fmt.Sprintf("• %s — %s%s [id %s]", r.Name, describeCadence(r.Recurrence, r.AtMinuteUTC), status, r.Id))
	}
	if len(lines) == 0 {
		rec.Result = "No routines set up here yet."
		return
	}
	rec.Result = "Routines here:\n" + strings.Join(lines, "\n")
}

// handleCancelRoutine cancels a routine by id (preferred) or an unambiguous name
// match within the current surface. Refuses cross-surface / cross-agent cancels.
func handleCancelRoutine(ctx context.Context, agent *model.AiAgent, sc agentRunScope, a ai.ProposedAction, rec *toolCallRecord) {
	idParam := strings.TrimSpace(a.Params["routine_id"])
	nameParam := strings.TrimSpace(a.Params["name"])

	if idParam != "" {
		id, perr := uuid.Parse(idParam)
		if perr != nil {
			rec.Error = "that routine id isn't valid — say \"list routines\" to see the ids"
			return
		}
		r, gerr := model.GetRoutine(ctx, id)
		if gerr != nil || r == nil || r.AgentId != agent.Id || !routineInScope(r, sc) {
			rec.Error = "no such routine here"
			return
		}
		if derr := model.DeleteRoutine(ctx, id); derr != nil {
			rec.Error = "could not cancel that routine: " + derr.Error()
			return
		}
		rec.Result = fmt.Sprintf("Cancelled routine %q.", r.Name)
		return
	}

	if nameParam == "" {
		rec.Error = "tell me which routine to cancel (a routine id or its name) — say \"list routines\" to see them"
		return
	}

	// Name match within this surface; require exactly one to avoid a wrong cancel.
	routines, err := model.ListRoutinesForAgent(ctx, agent.Id)
	if err != nil {
		rec.Error = "could not look up routines: " + err.Error()
		return
	}
	var matches []*model.AgentRoutine
	for _, r := range routines {
		if routineInScope(r, sc) && strings.EqualFold(strings.TrimSpace(r.Name), nameParam) {
			matches = append(matches, r)
		}
	}
	switch len(matches) {
	case 0:
		rec.Error = fmt.Sprintf("no routine named %q here — say \"list routines\" to see them", nameParam)
	case 1:
		if derr := model.DeleteRoutine(ctx, matches[0].Id); derr != nil {
			rec.Error = "could not cancel that routine: " + derr.Error()
			return
		}
		rec.Result = fmt.Sprintf("Cancelled routine %q.", matches[0].Name)
	default:
		rec.Error = fmt.Sprintf("more than one routine is named %q — cancel it by id instead (say \"list routines\")", nameParam)
	}
}

// routineInScope reports whether a routine belongs to the current run surface
// (same channel, or same DM/group), so listing/cancelling never crosses into
// another conversation's standing work.
func routineInScope(r *model.AgentRoutine, sc agentRunScope) bool {
	if sc.ChannelID != "" {
		return r.ChannelId != nil && r.ChannelId.String() == sc.ChannelID
	}
	if sc.GroupID != "" {
		return r.GroupId == sc.GroupID
	}
	return false
}

// parseRoutineFireMinute resolves the UTC fire-minute from tool params: an
// explicit at_minute_utc, or a local time ("HH:MM") plus tz_offset_minutes, or
// the 09:00 UTC default. Robust to junk input (falls back to the default).
func parseRoutineFireMinute(params map[string]string) int {
	if v := strings.TrimSpace(params["at_minute_utc"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	if t := strings.TrimSpace(params["time"]); t != "" {
		if h, m, ok := parseHHMM(t); ok {
			off := 0
			if o := strings.TrimSpace(params["tz_offset_minutes"]); o != "" {
				if n, err := strconv.Atoi(o); err == nil {
					off = n
				}
			}
			return MinuteUTCFromLocal(h, m, off)
		}
	}
	return defaultRoutineMinuteUTC
}

// parseHHMM parses a 24-hour "HH:MM" (or "H:MM") clock string.
func parseHHMM(s string) (hour, min int, ok bool) {
	parts := strings.SplitN(strings.TrimSpace(s), ":", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	h, herr := strconv.Atoi(strings.TrimSpace(parts[0]))
	m, merr := strconv.Atoi(strings.TrimSpace(parts[1]))
	if herr != nil || merr != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

// describeCadence renders a recurrence + fire-minute as a short human phrase for
// tool results ("daily at 09:00 UTC", "weekly on Mon,Wed,Fri at 14:30 UTC").
func describeCadence(recurrence string, atMinuteUTC int) string {
	rule := strings.ToUpper(recurrence)
	if mins, ok := schedulerBusiness.IntervalMinutesForRule(recurrence); ok {
		if mins%60 == 0 {
			h := mins / 60
			if h == 1 {
				return "every hour"
			}
			return fmt.Sprintf("every %d hours", h)
		}
		return fmt.Sprintf("every %d minutes", mins)
	}
	hh := atMinuteUTC / 60
	mm := atMinuteUTC % 60
	at := fmt.Sprintf("%02d:%02d UTC", hh, mm)
	if strings.Contains(rule, "FREQ=WEEKLY") {
		if i := strings.Index(rule, "BYDAY="); i >= 0 {
			days := rule[i+len("BYDAY="):]
			if j := strings.Index(days, ";"); j >= 0 {
				days = days[:j]
			}
			return fmt.Sprintf("weekly on %s at %s", days, at)
		}
		return "weekly at " + at
	}
	return "daily at " + at
}
