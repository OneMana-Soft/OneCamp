package business

// Routine dispatcher — the tick that fires due agent routines (agent_routines,
// created via create_routine) as scheduled agent runs.
//
// It mirrors the scheduled-agent tick (runDueScheduledAgents) but with two
// differences that matter for correctness:
//   - Serialization is PER ROUTINE, not per agent, so an agent with several
//     routines fires all of them (the schedule tick caps per agent at 1).
//   - Each due routine is atomically claimed (ClaimDueRoutineRun, a
//     compare-and-set on last_run_at) BEFORE its goroutine starts, so a slow run
//     can't double-fire on the next tick AND, on a multi-replica deployment,
//     exactly one replica fires each occurrence (at-most-once).
//
// A routine runs the user's prompt on the owner-identity, permission-checked
// path and posts a concise result to its channel — staying silent when there is
// nothing to report (the same NOTHING_TO_REPORT sentinel the scheduled check-in
// uses), so a recurring routine never spams a channel.

import (
	"context"
	"fmt"
	"time"

	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	schedulerBusiness "github.com/akashc777/OneCamp/business/Scheduler"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// maxRoutinesPerTick bounds how many due routines one tick launches, so a large
// backlog can't spawn an unbounded burst of runs.
const maxRoutinesPerTick = 100

// routineLoop fires due routines once per tick (same cadence as the schedule
// tick — one minute is the finest granularity a routine's fire time exposes).
func routineLoop(ctx context.Context) {
	t := time.NewTicker(scheduleTickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			runDueRoutines(ctx)
		}
	}
}

// runDueRoutines scans enabled routines and launches every one whose scheduled
// occurrence has passed since its last run. Inert when AI is disabled.
func runDueRoutines(ctx context.Context) {
	if svc := ai.GetService(); svc == nil || !svc.IsEnabled() {
		return
	}
	routines, err := model.ListEnabledRoutines(ctx, 500)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "routineDispatch: list enabled routines: %v", err)
		return
	}
	now := time.Now()
	launched := 0
	for _, r := range routines {
		if launched >= maxRoutinesPerTick {
			return
		}
		if r.ChannelId == nil {
			continue // only channel routines post today (create restricts to channels)
		}
		if !routineDue(r, now) {
			continue
		}
		// Atomically claim the occurrence up-front (compare-and-set on
		// last_run_at) so a slow run can't re-fire next tick AND, on a
		// multi-replica deployment, exactly one replica fires it — the loser's
		// CAS matches no row and it skips. Replaces the unconditional stamp,
		// which two replicas could both apply and both launch.
		if won, merr := model.ClaimDueRoutineRun(ctx, r.Id, r.LastRunAt, now); merr != nil {
			helpers.LogErrorWithContext(ctx, "routineDispatch: claim run (routine=%s): %v", r.Id, merr)
			continue
		} else if !won {
			continue
		}
		launchRoutine(ctx, r)
		launched++
	}
}

// routineDue reports whether a routine should fire at `now`. An interval
// cadence (FREQ=HOURLY[;INTERVAL=N]) fires relative to the last run; a
// fixed-time cadence (DAILY/WEEKLY) fires when its scheduled occurrence has
// passed since the last run. Pure dispatch on the Scheduler's recurrence
// primitives.
func routineDue(r *model.AgentRoutine, now time.Time) bool {
	if mins, ok := schedulerBusiness.IntervalMinutesForRule(r.Recurrence); ok {
		return schedulerBusiness.DueForInterval(mins, r.LastRunAt, now)
	}
	return schedulerBusiness.DueForSchedule(r.Recurrence, r.AtMinuteUTC, r.LastRunAt, now)
}

// launchRoutine runs one routine to completion in its own goroutine and posts
// the result to its channel as the agent's own principal. Serialized per
// routine (cap 1) so an overlapping tick never stacks the same routine.
func launchRoutine(ctx context.Context, r *model.AgentRoutine) {
	runCtx := context.WithoutCancel(ctx)
	serialKey := fmt.Sprintf("routine:%s", r.Id)
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				helpers.MessageLogs.ErrorLog.Printf("routineDispatch: recovered panic (routine=%s): %v", r.Id, rec)
			}
		}()
		if !agentRunLock.Acquire(serialKey, 1) {
			return // an instance of this routine is already running
		}
		defer agentRunLock.Release(serialKey)

		agent, aerr := model.GetAgentByID(runCtx, r.AgentId)
		if aerr != nil || agent == nil || !agent.IsActive {
			return // agent gone/disabled — routine stays but does nothing
		}
		channelID := r.ChannelId.String()

		// Author posts as the agent's own principal (named, badged teammate).
		var agentBot *userBusiness.BotIdentity
		if b, berr := userBusiness.EnsureAgentBot(runCtx, agent.Id, agent.Name, deref(agent.AvatarKey)); berr == nil {
			agentBot = b
			if agent.BotUserId == nil || *agent.BotUserId != b.UserID {
				_ = model.SetAgentBotUser(runCtx, agent.Id, b.UserID)
			}
		}

		// Per-channel daily AI cost control + memory scope, exactly like a
		// channel mention: the routine is bounded by the channel's cap and can
		// honor the channel's standing instructions.
		if chUUID, perr := uuid.Parse(channelID); perr == nil {
			if cap, cerr := channelBusiness.GetChannelAITokenCap(runCtx, chUUID); cerr == nil {
				runCtx = ai.WithChannelBudget(runCtx, channelID, cap)
			}
		}
		runCtx = WithAgentRunScope(runCtx, channelID, "")

		outcome := RunAgent(runCtx, agent, model.TriggerSchedule, routineRunPrompt(r.Prompt), false)

		// Post the result to the channel; a "nothing to report" run stays quiet.
		if text, ok := scheduledCheckinText(outcome); ok {
			postAgentMessage(runCtx, agent, agentBot, channelID, text)
		}
	}()
}

// routineRunPrompt frames a routine's standing instruction as a scheduled run:
// do the task now, post a concise result, or stay silent via the sentinel.
func routineRunPrompt(prompt string) string {
	return "This is a scheduled routine you were asked to run. Task:\n\"\"\"\n" + prompt + "\n\"\"\"\n\n" +
		"Do it now using your tools, then write a concise, skimmable result to post in the channel. " +
		"If there is nothing noteworthy to report, reply with exactly " + scheduledNothingSentinel + " and nothing else."
}
