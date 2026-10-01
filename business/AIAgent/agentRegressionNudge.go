package business

// Telling the owner when their agent got worse.
//
// The eval watcher already reruns a suite after an agent is edited and already
// notices when the pass rate drops. Until now it wrote a WARN and stopped
// there, which means the one person who could act on it, the agent's owner,
// found out only if they happened to read the server log.
//
// This is the first half of a governed improvement loop, and deliberately the
// half with a human in it: the system detects and explains, a person decides.
// A loop that also applied its own fix would need an eval gate and an audit
// trail before it could be trusted, and neither is worth building before the
// detection has proved it says something useful.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	nudgeModels "github.com/akashc777/OneCamp/models/postgres/WorkspaceNudge"
	"github.com/google/uuid"
)

// regressionNudgeCooldown stops the same drop being raised again on every pass.
//
// The watcher runs on a timer, and an agent that regressed stays regressed
// until somebody fixes it. Without this the owner would be told the same thing
// every cycle, which is how a signal becomes noise and then gets dismissed
// permanently.
const regressionNudgeCooldown = 24 * time.Hour

// notifyRegression raises a nudge for the agent's owner.
//
// Best-effort throughout: the watcher's job is to measure, and a failure to
// deliver the news must not stop it measuring the next agent.
func notifyRegression(ctx context.Context, a *model.AiAgent, before float64, passed, scored int) {
	if a == nil || a.CreatedBy == uuid.Nil {
		return
	}
	dedupKey := "agent_regression:" + a.Id.String()

	// Do not resurrect something this person has already dismissed today. They
	// have seen it and decided; saying it again is arguing.
	if recent, err := nudgeModels.HasRecentTerminal(ctx, dedupKey, regressionNudgeCooldown); err == nil && recent {
		return
	}

	body := fmt.Sprintf(
		"%s now passes %d of %d of its tests, down from %.0f%%. Something changed since it was last measured: the agent itself, or a skill it uses.",
		a.Name, passed, scored, before*100)
	if causes := recentSkillChanges(ctx, a); causes != "" {
		body += " " + causes
	}

	if _, _, err := nudgeModels.Upsert(ctx, nudgeModels.UpsertInput{
		UserID:     a.CreatedBy,
		Kind:       nudgeModels.KindAgentRegression,
		Title:      "An agent started failing its tests",
		Body:       body,
		CTAText:    "Review the agent",
		CTAURL:     "/app/settings/agents",
		SourceType: "agent",
		SourceID:   a.Id.String(),
		// Below overdue work: an agent doing worse matters, and it matters less
		// than a commitment somebody made to a colleague.
		Priority: 3,
		DedupKey: dedupKey,
	}); err != nil {
		helpers.LogErrorWithContext(ctx, "agent eval watcher: could not raise a regression nudge for %q: %+v", a.Name, err)
	}
}

// recentSkillChanges names skills this agent uses that were edited recently, so
// the owner is pointed at the likeliest cause instead of being told to go
// looking.
//
// This is the payoff of skill revisions. A shared skill is the one thing that
// can change an agent's behaviour without anyone touching the agent, and before
// there was a history there was no way to see that it had.
func recentSkillChanges(ctx context.Context, a *model.AiAgent) string {
	ids := skillIDsOf(a)
	if len(ids) == 0 {
		return ""
	}
	names := []string{}
	for _, id := range ids {
		revs, err := model.ListSkillRevisions(ctx, id, 1)
		if err != nil || len(revs) == 0 {
			continue
		}
		if time.Since(revs[0].CreatedAt) <= regressionNudgeCooldown {
			names = append(names, revs[0].Name)
		}
	}
	switch len(names) {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf("The skill %q was edited in the last day.", names[0])
	default:
		return fmt.Sprintf("These skills were edited in the last day: %s.", strings.Join(names, ", "))
	}
}
