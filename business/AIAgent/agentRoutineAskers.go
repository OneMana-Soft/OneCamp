package business

// Routines from before the person who asked for one was recorded.
//
// A routine runs for whoever set it up (routineRunFor): one someone other than
// the agent's sponsor asked for reaches only what they and the sponsor both
// can. Until this release every routine was recorded as created by the sponsor,
// whoever asked for it, so each one set up before then runs with the sponsor's
// whole reach, a teammate's "every morning, post what leadership decided here"
// included.
//
// Who asked for those is recorded nowhere. So once per install, before any
// routine fires, the ones that post where someone besides the sponsor can see
// (a public channel, a channel with anyone else in it, a group chat) are
// paused and marked as asked for by nobody known (created_by is the nil uuid).
// Each sponsor is told once, by a direct message from the agent, and only the
// sponsor can turn one back on, which records them as the person it runs for
// (SetAgentRoutineEnabled). A routine in a channel only its sponsor is in could
// only have been theirs, and keeps running. No migration: the nil uuid is the
// marker, and system_configs records that it was done.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html"
	"strings"
	"time"

	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
	"github.com/google/uuid"
)

// routineAskersKey is the system_configs key recording the pass: "since <time>"
// while under way (routines created from then on had their asker recorded),
// "done" once finished.
const routineAskersKey = "agent_routines_askers_v1"

// errRoutineNeedsSponsor is the refusal to turn on a routine nobody known asked
// for, for anyone but the agent's sponsor.
var errRoutineNeedsSponsor = errors.New("only the person who set up this agent can turn this routine back on: " +
	"nobody knows who asked for it, and it would run for them")

// lookupRoutineChannel reads where a channel routine posts; a seam for tests.
var lookupRoutineChannel = channelBusiness.GetDgraphChannelInfoByUUID

// awaitRoutineAskers runs settleRoutineAskers until it has succeeded, waiting
// longer after each failure. False when ctx ends first.
func awaitRoutineAskers(ctx context.Context) bool {
	wait := 5 * time.Second
	for {
		err := settleRoutineAskers(ctx)
		if err == nil {
			return true
		}
		helpers.LogErrorWithContext(ctx, "routines: pausing the ones nobody known asked for failed (will retry): %v", err)
		select {
		case <-ctx.Done():
			return false
		case <-time.After(wait):
		}
		if wait < 5*time.Minute {
			wait *= 2
		}
	}
}

// settleRoutineAskers pauses the routines from before askers were recorded
// that post where others can see, and tells their sponsors, unless that has
// been done.
func settleRoutineAskers(ctx context.Context) error {
	before, done, err := routineAskersCutoff()
	if err != nil || done {
		return err
	}
	routines, err := model.ListRoutinesRecordedAsSponsorBefore(ctx, before)
	if err != nil {
		return err
	}
	agents := map[uuid.UUID]*model.AiAgent{}
	paused := map[uuid.UUID][]*model.AgentRoutine{}
	for _, r := range routines {
		agent, seen := agents[r.AgentId]
		if !seen {
			if agent, err = model.GetAgentByID(ctx, r.AgentId); err != nil {
				return err
			}
			agents[r.AgentId] = agent
		}
		if agent != nil && !routineShared(ctx, agent, r) {
			continue
		}
		changed, err := model.ForgetRoutineAsker(ctx, r.Id)
		if err != nil {
			return err
		}
		if changed && r.Enabled && agent != nil {
			paused[agent.Id] = append(paused[agent.Id], r)
		}
	}
	for id, rs := range paused {
		tellSponsorRoutinesPaused(ctx, agents[id], rs)
	}
	return configModel.UpsertConfig(routineAskersKey, "done")
}

// routineAskersCutoff returns when the pass began, recording now when it hasn't
// yet: the routines created before then are the ones whose askers weren't
// recorded. done when the pass has finished.
func routineAskersCutoff() (before time.Time, done bool, err error) {
	cfg, err := configModel.GetConfigByKey(routineAskersKey)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, err
	}
	if err == nil && cfg != nil {
		if cfg.Value == "done" {
			return time.Time{}, true, nil
		}
		if t, perr := time.Parse(time.RFC3339Nano, strings.TrimPrefix(cfg.Value, "since ")); perr == nil {
			return t, false, nil
		}
	}
	before = time.Now().UTC()
	if err := configModel.UpsertConfig(routineAskersKey, "since "+before.Format(time.RFC3339Nano)); err != nil {
		return time.Time{}, false, err
	}
	return before, false, nil
}

// routineShared reports whether anyone besides the agent's sponsor can see
// where a routine posts: a group chat, a public channel, or a channel with
// another person in it. Can't tell is yes.
func routineShared(ctx context.Context, agent *model.AiAgent, r *model.AgentRoutine) bool {
	if r.ChannelId == nil {
		return true
	}
	ch, err := lookupRoutineChannel(ctx, *r.ChannelId, "")
	if err != nil || ch == nil || ch.Uuid == "" {
		return true
	}
	if ch.IsPrivate == nil || !*ch.IsPrivate {
		return true
	}
	for _, m := range ch.Members {
		if m != nil && !m.IsBot && !strings.EqualFold(strings.TrimSpace(m.Uuid), agent.CreatedBy.String()) {
			return true
		}
	}
	return false
}

// tellSponsorRoutinesPaused tells an agent's sponsor, in a direct message from
// the agent, which of its routines were paused and why. Best-effort.
func tellSponsorRoutinesPaused(ctx context.Context, agent *model.AiAgent, routines []*model.AgentRoutine) {
	bot, err := userBusiness.EnsureAgentBot(ctx, agent.Id, agent.Name, deref(agent.AvatarKey))
	if err == nil {
		err = sendAgentDM(ctx, bot, agent.CreatedBy, routinesPausedNote(routines))
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "routines: telling the sponsor of agent %s about paused routines failed: %v", agent.Id, err)
	}
}

// routinesPausedNote is that message. Pure.
func routinesPausedNote(routines []*model.AgentRoutine) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<p>I've paused %s that %s where other people can see. %s set up before I kept track of who asked for each one, "+
		"so I can't tell whose access %s should run with.</p><ul>",
		plural(len(routines), "a routine", fmt.Sprintf("%d routines", len(routines))),
		plural(len(routines), "posts", "post"), plural(len(routines), "It was", "They were"), plural(len(routines), "it", "they"))
	for _, r := range routines {
		fmt.Fprintf(&b, "<li>%s (%s)</li>", html.EscapeString(strings.TrimSpace(r.Name)), html.EscapeString(describeCadence(r.Recurrence, r.AtMinuteUTC)))
	}
	b.WriteString("</ul><p>If one should run for you, turn it back on in my settings. Anyone else can ask me to set theirs up again.</p>")
	return b.String()
}

// plural picks one or many by n.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
