package business

// Agent triggers: the runtime that fires agents automatically. An agent is a
// saved definition (agentBusiness) + a tool-loop (agentRunner); this file wires
// the three non-manual trigger kinds to live workspace activity, reusing the
// SAME primitives as the Workflow engine so there is no parallel runtime:
//
//   - event    — an in-process listener on the workspace event bus
//                (webhook.RegisterEventListener). When an event whose type
//                matches the agent's trigger_config.event fires, the agent runs.
//   - mention  — also on post.created: when an agent's name/handle is @typed in
//                a channel message, that agent runs with the message as input.
//   - schedule — a once-a-minute ticker that runs each active scheduled agent
//                whose trigger_config.interval_minutes has elapsed since its
//                last run.
//
// Loop safety: an agent's own writes run under helpers.WithWorkflowGenerated
// (set inside RunAgent), and DispatchEvent skips in-process listeners for such
// writes — so an agent (or workflow) action can never re-trigger an agent.
//
// Cost/abuse safety: every launch passes through launchAgent, which drops a
// trigger if that agent is already running (one run at a time per agent), and
// the runner itself is bounded by max_steps + the AI circuit breaker / rate
// limiter. Agents only fire while AI is enabled; an inert config never spends a
// model call or writes a run row.

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	aiBusiness "github.com/akashc777/OneCamp/business/AI"
	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	botpost "github.com/akashc777/OneCamp/business/BotPost"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	schedulerBusiness "github.com/akashc777/OneCamp/business/Scheduler"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	postDomain "github.com/akashc777/OneCamp/domain/Post"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

const (
	// scheduleTickInterval is how often the scheduler wakes to look for due
	// agents. One minute is the finest schedule granularity we expose.
	scheduleTickInterval = time.Minute
	// cacheRefreshInterval is a safety net that re-syncs the mention/event
	// caches even if a CRUD invalidation is ever missed (e.g. a write on
	// another replica).
	cacheRefreshInterval = time.Minute
	// minScheduleMinutes is the smallest accepted schedule interval; a config
	// below this (or unset) means the agent never auto-fires.
	minScheduleMinutes = 1
)

// triggerConfig is the parsed shape of an agent's trigger_config JSON. Only the
// fields relevant to the agent's trigger_type are populated/used.
type triggerConfig struct {
	IntervalMinutes int    `json:"interval_minutes"` // schedule (fixed-interval mode)
	Recurrence      string `json:"recurrence"`       // schedule (cron mode): RRULE-lite, e.g. FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR
	AtMinuteUTC     int    `json:"at_minute_utc"`    // schedule (cron mode): fire time, minutes past UTC midnight
	Event           string `json:"event"`            // event: workspace event type
	Handle          string `json:"handle"`           // mention: explicit @handle (defaults to name)
	// event task.status_changed: which moves (project_id, to_status). See agentMoveFilter.go.
	taskStatusBusiness.MoveFilter
}

var (
	// trigMu guards the cached trigger sets and the started flag.
	trigMu       sync.RWMutex
	mentionCache []*model.AiAgent            // active mention-trigger agents
	eventCache   map[string][]*model.AiAgent // active event-trigger agents, keyed by event type
	dmAbleCache  []*model.AiAgent            // active DM-able agents (Req 10.1), any trigger type
	ambientCache []*model.AiAgent            // active ambient agents (any trigger type)
	trigStarted  bool

	// agentRunLock serializes agent runs per intent (see agentLaunchSerialKey):
	//   - mention  -> per (agent, channel): a channel's @mentions are answered
	//                 one at a time in order, different channels run concurrently,
	//                 and only a flood beyond the cap is shed — a distinct mention
	//                 is never silently dropped (Slack-class per-channel queue).
	//   - schedule -> per agent, cap 1: an overlapping tick is dropped, not
	//                 stacked (don't queue standups).
	//   - event    -> per agent, cap 1: one event run at a time (the bus can
	//                 burst).
	// Hard cost ceilings stay in the runner (token budget + rate limit +
	// circuit breaker); this only shapes concurrency/ordering.
	agentRunLock helpers.KeyedLock
)

// maxMentionTurnsPerChannel bounds how many @mention replies for one agent in
// one channel may be in-flight-or-queued at once (1 running + the rest queued).
// Beyond this a genuine @mention flood is shed; ordinary back-to-back mentions
// are all answered in order.
const maxMentionTurnsPerChannel = 4

// agentLaunchSerialKey returns the per-intent serialization key and its
// in-flight cap for a launch. Mentions serialize per (agent, channel) and queue
// up to the cap so distinct mentions are never dropped; schedule/event runs
// serialize per agent with cap 1 so an overlapping run is dropped rather than
// stacked.
func agentLaunchSerialKey(agentID uuid.UUID, triggerSource, replyChannelID string) (string, int) {
	if triggerSource == model.TriggerMention {
		return "mention:" + agentID.String() + ":" + replyChannelID, maxMentionTurnsPerChannel
	}
	return triggerSource + ":" + agentID.String(), 1
}

// StartTriggers wires the event listener and starts the schedule + cache-refresh
// loops. Idempotent; call once at startup after the DB is ready. Inert until an
// agent with a non-manual trigger is created.
func StartTriggers(ctx context.Context) {
	trigMu.Lock()
	if trigStarted {
		trigMu.Unlock()
		return
	}
	trigStarted = true
	trigMu.Unlock()

	loadTriggerCache(ctx)
	webhookBusiness.RegisterEventListener(handleAgentEvent)
	go scheduleLoop(ctx)
	go routineLoop(ctx)
	go cacheRefreshLoop(ctx)
	helpers.MessageLogs.InfoLog.Println("AI agent triggers started")
}

// ReloadTriggerCache refreshes the in-memory mention/event sets after an agent
// mutation so changes take effect without waiting for the periodic refresh.
// No-op until the trigger workers have started.
func ReloadTriggerCache(ctx context.Context) {
	trigMu.RLock()
	started := trigStarted
	trigMu.RUnlock()
	if !started {
		return
	}
	loadTriggerCache(ctx)
}

// loadTriggerCache rebuilds the mention + event caches from the DB.
func loadTriggerCache(ctx context.Context) {
	mentions, err := model.ListActiveByTrigger(ctx, model.TriggerMention)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "agentTriggers: load mention agents: %v", err)
		mentions = nil
	}
	events, err := model.ListActiveByTrigger(ctx, model.TriggerEvent)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "agentTriggers: load event agents: %v", err)
		events = nil
	}
	dmAble, err := model.ListDMable(ctx)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "agentTriggers: load dm-able agents: %v", err)
		dmAble = nil
	}
	ambient, err := model.ListAmbient(ctx)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "agentTriggers: load ambient agents: %v", err)
		ambient = nil
	}
	byEvent := make(map[string][]*model.AiAgent)
	for _, a := range events {
		ev := strings.TrimSpace(parseTriggerConfig(a).Event)
		if ev == "" {
			continue // misconfigured: no event to bind to
		}
		if isInternalEventType(ev) {
			// Refused, not honoured. See isInternalEventType: binding a plain
			// event trigger to an internal event would run the agent through the
			// generic launch loop, which performs none of the delegation checks
			// that event is meant to be gated by.
			helpers.MessageLogs.InfoLog.Printf(
				"agentTriggers: agent %s is bound to internal event %q; ignoring "+
					"(internal events are not bindable as agent triggers)", a.Id, ev)
			continue
		}
		byEvent[ev] = append(byEvent[ev], a)
	}
	trigMu.Lock()
	mentionCache = mentions
	eventCache = byEvent
	dmAbleCache = dmAble
	ambientCache = ambient
	trigMu.Unlock()
}

// cacheRefreshLoop periodically re-syncs the caches as a safety net.
func cacheRefreshLoop(ctx context.Context) {
	t := time.NewTicker(cacheRefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			loadTriggerCache(ctx)
		}
	}
}

// scheduleLoop runs due scheduled agents once per tick.
func scheduleLoop(ctx context.Context) {
	t := time.NewTicker(scheduleTickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			runDueScheduledAgents(ctx)
		}
	}
}

// runDueScheduledAgents launches every active scheduled agent whose interval has
// elapsed since its last run. Inert when AI is disabled.
func runDueScheduledAgents(ctx context.Context) {
	if svc := ai.GetService(); svc == nil || !svc.IsEnabled() {
		return
	}
	agents, err := model.ListActiveByTrigger(ctx, model.TriggerSchedule)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "agentTriggers: load schedule agents: %v", err)
		return
	}
	now := time.Now()
	for _, a := range agents {
		cfg := parseTriggerConfig(a)
		// Two schedule modes, both reusing shared primitives:
		//   - cron: an RRULE-lite recurrence + fire time (e.g. weekdays 9am),
		//     evaluated via the Scheduler's recurrence engine.
		//   - fixed interval: "every N minutes" since the last run (the
		//     original behavior, kept for backward compatibility).
		if strings.TrimSpace(cfg.Recurrence) != "" {
			if !schedulerBusiness.DueForSchedule(cfg.Recurrence, cfg.AtMinuteUTC, a.LastRunAt, now) {
				continue // not due this tick
			}
		} else {
			interval := cfg.IntervalMinutes
			if interval < minScheduleMinutes {
				continue // unset/invalid → never auto-fire
			}
			if a.LastRunAt != nil && now.Sub(*a.LastRunAt) < time.Duration(interval)*time.Minute {
				continue // not due yet
			}
		}
		// Multi-replica safety: atomically claim this due occurrence before
		// launching. On >1 replica, every replica's ticker sees the same due
		// agent; the CAS on last_run_at lets exactly one win, so a scheduled
		// agent never double-fires (the in-process launch lock can't guard this
		// — it doesn't span processes). The loser skips.
		if won, cerr := model.ClaimDueScheduledRun(ctx, a.Id, a.LastRunAt); cerr != nil || !won {
			continue
		}
		// A scheduled agent that has been added to channel(s) posts its update
		// there automatically (proactive check-in); one with no channel scope
		// runs silently (it may still act via its tools), preserving prior
		// behavior. Posting is opt-in by adding the agent to a channel.
		postChannels := parseScope(a).ChannelIDs
		prompt := ""
		if len(postChannels) > 0 {
			prompt = scheduledCheckinPrompt
		}
		launchAgent(ctx, a, model.TriggerSchedule, prompt, "", "", postChannels, "")
	}
}

// handleAgentEvent is the event-bus listener for event + mention triggers. It is
// only invoked for genuine (non-automation) writes, since DispatchEvent skips
// listeners for workflow-generated writes — that is the loop guard.
func handleAgentEvent(ctx context.Context, eventType string, data map[string]interface{}) {
	if svc := ai.GetService(); svc == nil || !svc.IsEnabled() {
		return
	}

	trigMu.RLock()
	evAgents := eventCache[eventType]
	mAgents := mentionCache
	amAgents := ambientCache
	trigMu.RUnlock()

	for _, a := range evAgents {
		if !eventWanted(a, eventType, data) {
			continue
		}
		launchAgent(ctx, a, model.TriggerEvent, synthEventPrompt(eventType, data), "", "", nil, "")
	}

	// An agent's own message reaches ONLY the mention dispatcher, and only when
	// delegation is on (EmitAgentMessage is a no-op otherwise). Deliberately not
	// the ambient path: an agent volunteering an opinion on another agent's
	// message is noise nobody asked for, and it is the shape that fills a channel
	// fastest. Delegation must be explicitly addressed.
	if eventType == EventTypeAgentMessage {
		dispatchMentionAgents(ctx, mAgents, data)
		return
	}

	// Mention triggers fire on channel messages only (the surface where an
	// agent can naturally @reply via its send_message tool).
	if eventType == "post.created" {
		dispatchMentionAgents(ctx, mAgents, data)
		// Ambient agents may also reply to a non-mention channel message when
		// they judge it useful — gated hard (opt-in, scoped, pre-filtered,
		// cooldown, budget, self-selecting). Runs only for agents NOT already
		// @mentioned in this post (the mention path handles those).
		dispatchAmbientAgents(ctx, amAgents, data)
	}

	// Thread continuation: a comment on a post the agent replied in (or a fresh
	// @mention inside the thread) lets a person continue the conversation right
	// where it happened — answer the agent's needs_human question, add an
	// instruction, or just say "retry" — and the agent picks the work back up,
	// replying in the SAME thread.
	if eventType == "post.comment.created" {
		dispatchThreadAgents(ctx, mAgents, data)
	}
}

// dispatchMentionAgents launches every mention agent whose name/handle appears
// (as @handle) in the posted message.
func dispatchMentionAgents(ctx context.Context, agents []*model.AiAgent, data map[string]interface{}) {
	if len(agents) == 0 {
		return
	}
	text, _ := data["text"].(string)
	if strings.TrimSpace(text) == "" {
		return
	}
	lower := strings.ToLower(text)
	channelID, _ := data["channel_id"].(string)
	channelName, _ := data["channel_name"].(string)
	authorName, _ := data["author_name"].(string)
	authorID, _ := data["author_id"].(string)
	postID, _ := data["post_id"].(string)
	mentionIDs := mentionIDsFromEvent(data["mention_ids"])

	// Delegation budget. For a human-authored message (every message that can
	// reach here today, since botpost writes emit no post.created) this always
	// allows and nothing changes. It is wired first, before agent posts are ever
	// emitted, so the budget can never be retro-fitted onto a live loop.
	lineage := DelegationContextFromEvent(data, AgentIDFromBotUser(authorID))
	delegationCfg := LoadDelegationConfig(agentCollabAllowedInSurface(channelID))

	for _, a := range agents {
		if !mentionMatchesAgent(ctx, a, mentionIDs, lower) {
			continue
		}
		if d := AuthorizeDelegation(ctx, delegationCfg, lineage, a.Id.String(),
			Surface{Kind: SurfaceChannelPost, ChannelID: channelID, PostID: postID}, postID); !d.Allow {
			// Logged rather than silent: "my agent didn't answer" is otherwise
			// indistinguishable from a bug, and the reason is the whole answer.
			helpers.MessageLogs.InfoLog.Printf(
				"agentTriggers: delegation refused (agent=%s channel=%s hop=%d chain=%d): %s",
				a.Id, channelID, lineage.Hop, len(lineage.Chain), d.Reason)
			continue
		}
		// "Invited or silent" governance: when an agent has an explicit channel
		// scope, it only responds in those channels (prevents drive-by pings
		// from channels it was never scoped to). An empty scope keeps the
		// backward-compatible behavior of responding anywhere it is mentioned.
		if !agentAllowedInChannel(a, channelID) {
			continue
		}
		// Carry the lineage this agent's own reply must stamp. Only the agent
		// reply path reads it, and only when delegation is enabled — so for the
		// human-mention case this is an unread context value.
		rooted := originLineage(lineage, authorID)
		runCtx := WithDelegationContext(ctx, NextDelegationContext(rooted, a.Id.String()))
		// A mention written by another agent arrives through exactly this path,
		// and the person's absence is the fact the audit log needs. The hop is
		// known here and nowhere later, so the answer is set before the run.
		runCtx = auditBusiness.WithInitiator(runCtx, initiatorForTrigger(model.TriggerMention, lineage.Hop))
		// Attribute the run to the PERSON accountable for it, not to the agent
		// that relayed it. For a human mention these are the same id. For a
		// delegated hop, authorID is the delegating agent's bot principal, and
		// recording that would leave an audit trail whose actor is a bot with
		// nobody behind it — while the delegator is already preserved in the
		// lineage chain, so nothing is lost by crediting the human.
		attributedTo := authorID
		if rooted.OriginUserID != "" {
			attributedTo = rooted.OriginUserID
		}

		// Etiquette guard: don't run the full agent loop (and spend tokens) on
		// social greetings or explicit stop requests. A greeting gets one friendly
		// reply; a dismissal is honoured silently.
		switch ClassifyMentionIntent(text, a) {
		case IntentGreeting:
			bot, _ := userBusiness.EnsureAgentBot(ctx, a.Id, a.Name, deref(a.AvatarKey))
			postAgentReply(ctx, a, bot, channelID, postID, agentGreetingReply(a.Name))
			continue
		case IntentDismiss:
			if by, perr := uuid.Parse(strings.TrimSpace(attributedTo)); perr == nil {
				cancelOpenAgentRunsForSource(ctx, string(SurfaceChannelPost), postID, by)
			}
			continue
		}

		launchAgent(runCtx, a, model.TriggerMention, synthMentionPrompt(channelID, channelName, authorName, text), channelID, postID, nil, attributedTo)
	}
}

// originLineage fills in the originating person for a chain that is starting now.
// A human-authored message has no lineage on the event, so the author IS the
// origin; an agent-authored message already carries the origin from further up
// and must not have it overwritten, or hop 2 would credit itself as the root and
// every chain would look like it was authorised by a bot.
func originLineage(dc DelegationContext, authorID string) DelegationContext {
	if strings.TrimSpace(dc.OriginUserID) == "" && strings.TrimSpace(dc.AuthorAgentID) == "" {
		dc.OriginUserID = strings.TrimSpace(authorID)
	}
	return dc
}

// dispatchThreadAgents continues an agent's work when a human comments in a
// post's thread. An agent runs if it is explicitly @mentioned in the comment OR
// it already participated in this thread (so a plain follow-up like "retry" or
// an answer to its needs_human question resumes it — no re-@mention needed).
// The reply lands as another comment in the same thread (replyPostID = the
// parent post). The full thread transcript is fed in as context so the run
// continues coherently instead of starting cold. One dgraph read per relevant
// comment, only when an agent could actually run.
func dispatchThreadAgents(ctx context.Context, agents []*model.AiAgent, data map[string]interface{}) {
	if len(agents) == 0 {
		return
	}
	text, _ := data["text"].(string)
	if strings.TrimSpace(text) == "" {
		return
	}
	postID, _ := data["post_id"].(string)
	channelID, _ := data["channel_id"].(string)
	channelName, _ := data["channel_name"].(string)
	authorName, _ := data["author_name"].(string)
	authorID, _ := data["author_id"].(string)
	if strings.TrimSpace(postID) == "" {
		return
	}
	lower := strings.ToLower(text)
	mentionIDs := mentionIDsFromEvent(data["mention_ids"])

	// Durable resume: a human reply in this post's thread resumes any paused
	// (awaiting_input) durable run for this post in place — the non-task analog
	// of answering a blocked task. Agents resumed this way are excluded from a
	// fresh (synchronous or durable) launch below so a follow-up never both
	// resumes AND restarts the same agent.
	resumedDurable := ResumeDurableRunsForSurface(ctx, SurfaceChannelPost, postID, authorID, text)

	// Cheap pre-pass: who is @mentioned, and which agents are even eligible to
	// be thread participants (already provisioned a bot principal). If nothing
	// could run, skip the dgraph read entirely.
	toRun := make(map[uuid.UUID]bool)
	ordered := make([]*model.AiAgent, 0, len(agents))
	hasProvisioned := false
	for _, a := range agents {
		if !agentAllowedInChannel(a, channelID) {
			continue
		}
		if mentionMatchesAgent(ctx, a, mentionIDs, lower) {
			if !toRun[a.Id] {
				toRun[a.Id] = true
				ordered = append(ordered, a)
			}
		}
		if a.BotUserId != nil {
			hasProvisioned = true
		}
	}
	if len(toRun) == 0 && !hasProvisioned {
		return
	}

	// One read: the parent post + its comments (text + authors). Used both to
	// build the thread transcript (context) and to detect which provisioned
	// agents already participated (so a no-@mention follow-up resumes them).
	transcript, authorSet := threadContext(ctx, postID)

	for _, a := range agents {
		if toRun[a.Id] {
			continue // already going to run (explicit @mention)
		}
		if a.BotUserId == nil || !agentAllowedInChannel(a, channelID) {
			continue
		}
		bot, berr := userBusiness.EnsureAgentBot(ctx, a.Id, a.Name, deref(a.AvatarKey))
		if berr != nil || bot == nil {
			continue
		}
		// Loop guard: never let an agent's own comment drive its work.
		if authorID != "" && (authorID == bot.UUID || authorID == bot.DgraphUID) {
			continue
		}
		if authorSet[bot.UUID] || (bot.DgraphUID != "" && authorSet[bot.DgraphUID]) {
			toRun[a.Id] = true
			ordered = append(ordered, a)
		}
	}

	for _, a := range ordered {
		// A durable run already resumed for this agent on this post handles the
		// follow-up itself; don't also launch a fresh run for it.
		if resumedDurable[a.Id] {
			continue
		}

		// Etiquette guard in thread continuations: honour stop/close/dismiss and
		// do not restart the agent for a bare greeting. A real follow-up task
		// continues normally.
		switch ClassifyMentionIntent(text, a) {
		case IntentDismiss:
			if by, perr := uuid.Parse(strings.TrimSpace(authorID)); perr == nil {
				cancelOpenAgentRunsForSource(ctx, string(SurfaceChannelPost), postID, by)
			}
			continue
		case IntentGreeting:
			continue
		}

		launchAgent(ctx, a, model.TriggerMention, synthThreadPrompt(channelName, authorName, text, transcript), channelID, postID, nil, authorID)
	}
}

// threadContext reads a post + its comments once and returns (a) a compact,
// oldest-first transcript of the thread for run context and (b) the set of
// comment author ids (uuid) for participant detection. Best-effort: on any read
// error it returns ("", empty) so a continuation still runs (just without the
// transcript) rather than being blocked.
func threadContext(ctx context.Context, postID string) (string, map[string]bool) {
	authorSet := make(map[string]bool)
	dgp, err := postDomain.GetDgraphPostByUUIDWithAllComments(ctx, postID, "")
	if err != nil || dgp == nil {
		return "", authorSet
	}

	comments := append([]*dgraphStruct.DgraphComment(nil), dgp.Comments...)
	sort.SliceStable(comments, func(i, j int) bool {
		ci, cj := comments[i], comments[j]
		if ci == nil || ci.CreatedAt == nil {
			return true
		}
		if cj == nil || cj.CreatedAt == nil {
			return false
		}
		return ci.CreatedAt.Before(*cj.CreatedAt)
	})

	var b strings.Builder
	if postText := promptText(dgp.Text); postText != "" {
		author := "Someone"
		if dgp.PostBy != nil && strings.TrimSpace(dgp.PostBy.UserName) != "" {
			author = dgp.PostBy.UserName
		}
		b.WriteString(author + ": " + postText + "\n")
	}
	for _, c := range comments {
		if c == nil {
			continue
		}
		body := promptText(c.Text)
		if body == "" {
			continue
		}
		author := "Someone"
		if c.CommentBy != nil {
			if strings.TrimSpace(c.CommentBy.UserName) != "" {
				author = c.CommentBy.UserName
			}
			if c.CommentBy.Uuid != "" {
				authorSet[c.CommentBy.Uuid] = true
			}
		}
		b.WriteString(author + ": " + body + "\n")
	}
	return strings.TrimSpace(b.String()), authorSet
}

// synthThreadPrompt frames a thread continuation: the conversation so far plus
// the latest message, with an instruction to continue the work and reply as the
// final answer (which is posted as a comment in the same thread).
func synthThreadPrompt(channelName, authorName, text, transcript string) string {
	var b strings.Builder
	b.WriteString("You are being addressed in a message thread")
	if strings.TrimSpace(channelName) != "" {
		b.WriteString(" in the channel \"" + channelName + "\"")
	}
	b.WriteString(".\n\n")
	if strings.TrimSpace(transcript) != "" {
		b.WriteString("The thread so far (oldest first):\n\"\"\"\n" + transcript + "\n\"\"\"\n\n")
	}
	b.WriteString("Latest message")
	if strings.TrimSpace(authorName) != "" {
		b.WriteString(" from " + authorName)
	}
	b.WriteString(":\n\"\"\"\n" + strings.TrimSpace(text) + "\n\"\"\"\n\n")
	b.WriteString("Continue the work using your tools as needed, then reply concisely. Your reply is posted as a comment in THIS thread, so do NOT use a send-message tool — just write the reply as your final answer.")
	return b.String()
}

// mentionMatchesAgent reports whether a message mentions an agent. It prefers a
// robust PRINCIPAL (chip) match: when the agent's bot principal has been
// provisioned (it is added to a channel), a selected @mention chip carries the
// principal's Dgraph uid in mention_ids, which we match exactly. It falls back
// to plaintext @handle matching (one release) so agents that were never added
// to a channel — and thus have no chip in the typeahead — still respond to a
// literally typed handle.
//
// The principal lookup is skipped for agents without a provisioned principal so
// that evaluating an ordinary message never provisions a bot user for every
// agent in the workspace.
func mentionMatchesAgent(ctx context.Context, a *model.AiAgent, mentionIDs []string, lowerText string) bool {
	if a.BotUserId != nil && len(mentionIDs) > 0 {
		if bot, err := userBusiness.EnsureAgentBot(ctx, a.Id, a.Name, deref(a.AvatarKey)); err == nil && bot != nil {
			if idInList(mentionIDs, bot.DgraphUID) || idInList(mentionIDs, bot.UUID) {
				return true
			}
		}
	}
	return mentionMatches(a, lowerText)
}

// idInList reports whether target (non-empty) appears in ids.
func idInList(ids []string, target string) bool {
	if target == "" {
		return false
	}
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}

// mentionIDsFromEvent coerces the event's mention_ids field (which may be
// []string or []interface{} through the generic event map) into a []string.
func mentionIDsFromEvent(raw interface{}) []string {
	switch v := raw.(type) {
	case []string:
		return v
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// parseScope reads an agent's scope JSON (the channels / projects it is limited to).
// Empty lists mean "the owner's full accessible scope".
//
// The parser moved to the model, because the MCP authorizer needs the same answer and
// must not import this package. Kept as the name the trigger checks already read well
// with.
func parseScope(a *model.AiAgent) model.AgentScope {
	return a.ScopeConfig()
}

// agentAllowedInChannel reports whether a mention agent may respond in a given
// channel. An empty channel scope means "anywhere it is @mentioned" (backward
// compatible); a non-empty scope restricts the agent to exactly those channels
// — the "invited or silent" rule that keeps an agent from being pulled into
// channels it was never scoped to.
func agentAllowedInChannel(a *model.AiAgent, channelID string) bool {
	ids := parseScope(a).ChannelIDs
	if len(ids) == 0 {
		return true
	}
	for _, id := range ids {
		if strings.TrimSpace(id) == channelID {
			return true
		}
	}
	return false
}

// mentionMatches reports whether any of the agent's handles appears as a
// standalone @handle in the (already lower-cased) message text. The match is
// boundary-aware so an agent named "bot" fires on "@bot" but not "@bottom".
func mentionMatches(a *model.AiAgent, lowerText string) bool {
	for _, h := range mentionHandles(a) {
		if containsHandle(lowerText, h) {
			return true
		}
	}
	return false
}

// containsHandle reports whether "@handle" occurs in text not immediately
// followed by another word character (letter/digit/underscore), so it matches a
// whole mention rather than a prefix of a longer word. handle is expected
// lower-cased; text is scanned case-insensitively by the caller's lower-casing.
func containsHandle(text, handle string) bool {
	if handle == "" {
		return false
	}
	needle := "@" + handle
	from := 0
	for {
		i := strings.Index(text[from:], needle)
		if i < 0 {
			return false
		}
		end := from + i + len(needle)
		if end >= len(text) || !isWordByte(text[end]) {
			return true
		}
		from = from + i + 1
	}
}

// isWordByte reports whether b is an ASCII word character. Adequate for the
// boundary check; non-ASCII bytes are treated as boundaries (a name ending in a
// multi-byte rune still matches).
func isWordByte(b byte) bool {
	return b == '_' ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
}

// mentionHandles returns the lower-cased handles an agent answers to: its
// explicit trigger_config.handle (if any) and its name, each also in a
// space-stripped form so "@Standup Bot" and "@standupbot" both match.
func mentionHandles(a *model.AiAgent) []string {
	cfg := parseTriggerConfig(a)
	out := make([]string, 0, 4)
	add := func(s string) {
		s = strings.ToLower(strings.TrimSpace(s))
		if s != "" {
			out = append(out, s)
		}
	}
	if cfg.Handle != "" {
		add(cfg.Handle)
		add(strings.ReplaceAll(cfg.Handle, " ", ""))
	}
	add(a.Name)
	add(strings.ReplaceAll(a.Name, " ", ""))
	return out
}

// EnqueueDurableAgentRun enqueues a durable job for an opted-in agent's mention
// on a reply surface, replacing the synchronous run: the durable worker drives
// it to completion, shows an evolving in-thread status comment, survives
// restarts, and pauses/resumes on needs_human/budget. Returns true when a job
// was enqueued (or already open — idempotent), false on any problem so the
// caller can fall back to the synchronous path (a mention is never dropped).
// Exported so the coworker (DM/group) can reuse it. Generic across surfaces.
// triggeredBy is the app user uuid of the person who asked (the @mention/message
// author); it is recorded on the job so the worker can notify the right human on
// block/finish. Empty when there is no single human trigger.
func EnqueueDurableAgentRun(ctx context.Context, agent *model.AiAgent, surface Surface, prompt, triggeredBy string) bool {
	if agent == nil {
		return false
	}
	// The idempotency key is (source_type, source_id, agent_id); use the
	// triggering post/message id as source_id so a duplicate event or mention
	// burst can't stack duplicate jobs.
	sourceID := strings.TrimSpace(surface.PostID)
	if sourceID == "" {
		sourceID = strings.TrimSpace(surface.MessageID)
	}
	if sourceID == "" {
		return false
	}
	enc, err := EncodeSurface(surface)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "agentTriggers: encode surface failed (agent=%s): %v", agent.Id, err)
		return false
	}
	owner := agent.CreatedBy
	task := &model.AgentTask{
		AgentId:     agent.Id,
		SourceType:  string(surface.Kind),
		SourceId:    sourceID,
		Prompt:      prompt,
		RunAsUserId: &owner,
		Surface:     enc,
	}
	if tb, perr := uuid.Parse(strings.TrimSpace(triggeredBy)); perr == nil {
		task.TriggeredBy = &tb
	}
	id, _, eerr := model.EnqueueAgentTask(ctx, task)
	if eerr != nil {
		helpers.LogErrorWithContext(ctx, "agentTriggers: enqueue durable run failed (agent=%s surface=%s): %v", agent.Id, surface.Kind, eerr)
		return false
	}
	// Somebody is waiting in a thread for this: start it now instead of on the
	// next worker tick, and tell the thread it exists so the live strip can show
	// it (and offer Stop) even in the moment before a worker claims it — a job
	// held back by the per-agent concurrency cap would otherwise be invisible.
	WakeAgentTaskWorker()
	publishAgentWorkChanged(ctx, id)
	return true
}

// handOffMentionToDurable converts a bounded synchronous channel-mention run
// into a durable continuation: it enqueues a channel_post durable job and seeds
// it with the synchronous run's captured conversation, so the durable worker
// resumes MID-flight (the runner rebuilds its dedupe set from the seeded tool
// calls, so a write the sync run already performed is never repeated). Returns
// true when the continuation was enqueued (the caller then skips the truncated
// synchronous reply — the durable worker posts an evolving status comment and
// the final result on the same post). Returns false on any failure so the
// caller falls back to posting the synchronous answer (a mention is never
// dropped). Seeding is best-effort: if it fails, the durable run still proceeds
// from the enriched prompt.
func handOffMentionToDurable(ctx context.Context, a *model.AiAgent, channelID, postID, prompt string, msgs []ai.ChatMessage, triggeredBy string) bool {
	if a == nil || strings.TrimSpace(postID) == "" {
		return false
	}
	surface := Surface{Kind: SurfaceChannelPost, ChannelID: channelID, PostID: postID}
	enc, err := EncodeSurface(surface)
	if err != nil {
		return false
	}
	owner := a.CreatedBy
	task := &model.AgentTask{
		AgentId:     a.Id,
		SourceType:  string(surface.Kind),
		SourceId:    postID,
		Prompt:      agentRunPrompt(ctx, a, channelID, postID, prompt),
		RunAsUserId: &owner,
		Surface:     enc,
	}
	if tb, perr := uuid.Parse(strings.TrimSpace(triggeredBy)); perr == nil {
		task.TriggeredBy = &tb
	}
	id, _, eerr := model.EnqueueAgentTask(ctx, task)
	if eerr != nil {
		helpers.LogErrorWithContext(ctx, "agentTriggers: hand-off enqueue failed (agent=%s): %v", a.Id, eerr)
		return false
	}
	if len(msgs) > 0 {
		if b, merr := json.Marshal(msgs); merr == nil {
			if _, serr := model.SeedAgentTaskMessages(ctx, id, string(b)); serr != nil {
				helpers.LogErrorWithContext(ctx, "agentTriggers: hand-off seed messages failed (agent=%s): %v", a.Id, serr)
			}
		}
	}
	// Seed FIRST, then wake: a worker that claimed the job before the captured
	// conversation landed would restart the work from the prompt instead of
	// continuing mid-flight.
	WakeAgentTaskWorker()
	return true
}

// launchAgent runs an agent in the background under a detached context, with a
// per-agent in-flight guard (drop if already running) and a panic guard. Writes
// performed by the agent are loop-safe (RunAgent tags the context).
//
// replyChannelID, when set (mention triggers), makes the agent reply IN that
// channel authored as a badged AI teammate (the agent's name): the run's result
// is posted via the shared bot principal with the agent name as the label, so
// the reply reads as "[Agent Name] …" rather than being posted as the human
// owner. Posting goes through botpost, which does not emit post.created, so it
// can't re-trigger this or any other agent (loop-safe).
//
// replyPostID, when set alongside replyChannelID (mention triggers on a channel
// message), makes the reply land as an in-thread COMMENT on the triggering post
// (Slack-style threaded reply) rather than a new top-level channel message. It
// falls back to a top-level post if the comment can't be written, so an
// @mention is never dropped.
func launchAgent(ctx context.Context, a *model.AiAgent, triggerSource, prompt, replyChannelID, replyPostID string, postChannelIDs []string, triggeredBy string) {
	runCtx := context.WithoutCancel(ctx)
	serialKey, maxInFlight := agentLaunchSerialKey(a.Id, triggerSource, replyChannelID)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				helpers.MessageLogs.ErrorLog.Printf("agentTriggers: recovered panic (agent=%s): %v", a.Id, r)
			}
		}()

		// Durable async opt-in: an opted-in agent's channel/thread @mention runs
		// as a durable job (evolving in-thread progress, survives restarts,
		// pauses/resumes on needs_human/budget) instead of one synchronous pass.
		// Only for a mention reply on a real post (channel_post surface); a
		// scheduled check-in stays synchronous. The prompt is enriched here (as
		// the sync path would) so the durable run has the same context. On any
		// enqueue failure we fall through to the synchronous path below, so a
		// mention is never dropped.
		if shouldRunMentionDurably(a.RunInBackground, agentHasTools(a), durableMentionsEnabled()) &&
			triggerSource == model.TriggerMention && replyChannelID != "" && replyPostID != "" {
			enriched := agentRunPrompt(runCtx, a, replyChannelID, replyPostID, prompt)
			if EnqueueDurableAgentRun(runCtx, a, Surface{Kind: SurfaceChannelPost, ChannelID: replyChannelID, PostID: replyPostID}, enriched, triggeredBy) {
				return
			}
		}

		// Serialize per intent. A mention waits its turn in the channel's queue
		// (answered in order, never dropped) until the cap is reached; a
		// schedule/event run is dropped if one is already in progress. Acquire
		// blocks for a queued mention, so we run it inside the goroutine to keep
		// the caller (the event bus) non-blocking.
		if !agentRunLock.Acquire(serialKey, maxInFlight) {
			return
		}
		defer agentRunLock.Release(serialKey)

		// Resolve the agent's OWN bot principal (provisioning on first use) so
		// anything it posts (a mention reply or a scheduled check-in) and its
		// typing indicator are authored as itself — its own name, avatar and
		// uuid — rather than the shared automation bot. Best-effort: on failure
		// we fall back to a shared-bot post under the agent's display name.
		needsPrincipal := replyChannelID != "" || len(postChannelIDs) > 0
		var agentBot *userBusiness.BotIdentity
		if needsPrincipal {
			if b, berr := userBusiness.EnsureAgentBot(runCtx, a.Id, a.Name, deref(a.AvatarKey)); berr == nil && b != nil {
				agentBot = b
				// Persist the denormalized link once (cheap no-op when unchanged).
				if a.BotUserId == nil || *a.BotUserId != b.UserID {
					if serr := model.SetAgentBotUser(runCtx, a.Id, b.UserID); serr != nil {
						helpers.LogErrorWithContext(runCtx, "agentTriggers: persist bot_user_id failed (agent=%s): %v", a.Id, serr)
					}
				}
			} else if berr != nil {
				helpers.LogErrorWithContext(runCtx, "agentTriggers: resolve agent principal failed (agent=%s): %v", a.Id, berr)
			}
		}

		// For a mention reply, show a live "typing…" indicator as the agent
		// (its own principal) while the run executes, so a multi-step / slow-model
		// run reads as the teammate working rather than dead air. Re-published on
		// a ticker so it doesn't expire on clients before the reply lands. A
		// scheduled check-in has no one waiting, so it shows no typing.
		var stopTyping chan struct{}
		if replyChannelID != "" {
			stopTyping = make(chan struct{})
			go runAgentTyping(runCtx, a.Name, agentBot, replyChannelID, stopTyping)
		}
		// Per-channel daily AI cost control: a channel reply also meters against
		// (and is bounded by) the channel's own daily cap, on top of the agent's
		// per-agent cap. Best-effort read; a 0 cap meters without capping.
		if replyChannelID != "" {
			if chUUID, perr := uuid.Parse(replyChannelID); perr == nil {
				if cap, cerr := channelBusiness.GetChannelAITokenCap(runCtx, chUUID); cerr == nil {
					runCtx = ai.WithChannelBudget(runCtx, replyChannelID, cap)
				}
			}
		}
		// Sync->durable hand-off (opt-in via AI_AGENT_ASYNC_HANDOFF, default
		// OFF): for a channel mention reply, capture the run's evolving
		// conversation so that IF the synchronous run hits a hard bound
		// (step/token/time) WITH real tool progress, it can continue as a durable
		// job MID-conversation rather than returning a truncated answer. The
		// resume state is attached ONLY when the gate is on, so the default fast
		// path is byte-identical to before.
		handoffEligible := replyChannelID != "" && replyPostID != "" && triggerSource == model.TriggerMention && asyncHandoffEnabled()
		var (
			handoffMu   sync.Mutex
			handoffMsgs []ai.ChatMessage
		)
		if handoffEligible {
			runCtx = WithAgentResumeState(runCtx, nil, func(msgs []ai.ChatMessage) {
				handoffMu.Lock()
				handoffMsgs = msgs
				handoffMu.Unlock()
			})
		}

		// Conversation scope for the memory tools: a channel mention is scoped
		// to that channel so "remember for this channel" works.
		if replyChannelID != "" {
			runCtx = WithAgentRunScope(runCtx, replyChannelID, "")
			// Full reply surface (channel + parent post) so a code_pr tool call
			// can enqueue a durable job that posts the PR back in THIS thread.
			if replyPostID != "" {
				runCtx = WithAgentRunSurface(runCtx, Surface{Kind: SurfaceChannelPost, ChannelID: replyChannelID, PostID: replyPostID})
			}
		}
		outcome := RunAgent(runCtx, a, triggerSource, agentRunPrompt(runCtx, a, replyChannelID, replyPostID, prompt), false)
		if stopTyping != nil {
			close(stopTyping)
		}

		// If the synchronous run hit a bounded stop with real progress, continue
		// it durably instead of posting a truncated reply. On any hand-off
		// failure we fall through to posting the synchronous answer, so a mention
		// is never dropped.
		if handoffEligible && outcome != nil && shouldRunAsync(AsyncSignal{
			HandoffEnabled: true,
			StopReason:     outcome.StopReason,
			ToolsSucceeded: len(outcome.ToolsSucceeded),
		}) {
			handoffMu.Lock()
			captured := handoffMsgs
			handoffMu.Unlock()
			if handOffMentionToDurable(runCtx, a, replyChannelID, replyPostID, prompt, captured, triggeredBy) {
				return
			}
		}

		// Mention reply: post the agent's answer to the originating channel as
		// its own principal (named, badged teammate). Best-effort and loop-safe
		// (botpost emits no post.created). If the run produced no answer (a
		// non-throttle failure or empty result), post a brief honest note so a
		// member who @mentioned the agent is never met with silence (Req 6.2);
		// transient throttle/breaker stops stay silent to avoid a reply storm.
		if replyChannelID != "" {
			if reply, ok := agentMentionReply(outcome); ok {
				postAgentReply(runCtx, a, agentBot, replyChannelID, replyPostID, reply)
			}
		}

		// Scheduled check-in: post the agent's update to each channel it has
		// been added to (its scope). Result-only — a scheduled run never posts a
		// failure note (that would be recurring noise), and a "nothing to report"
		// run stays quiet (sentinel), so a check-in only speaks when it has
		// something to say.
		if len(postChannelIDs) > 0 {
			if text, ok := scheduledCheckinText(outcome); ok {
				for _, chID := range postChannelIDs {
					postAgentMessage(runCtx, a, agentBot, chID, text)
				}
			}
		}
	}()
}

// postAgentReply posts a mention-triggered agent's answer to the originating
// channel. When a triggering post id is known and the agent has its own
// principal, it replies as an in-thread COMMENT on that post (Slack-style
// threaded reply) so the exchange stays grouped under the original message. It
// falls back to a top-level channel post if the post id is absent, the agent
// has no resolved principal, or the comment write fails — so an @mention is
// never met with silence. Loop-safe (botpost emits no post.created).
func postAgentReply(ctx context.Context, a *model.AiAgent, agentBot *userBusiness.BotIdentity, channelID, replyPostID, text string) {
	if dc, ok := delegationFromContext(ctx); ok && a != nil {
		text = withHandoff(ctx, a.Id, dc.Chain, dc.OriginUserID, text)
	}
	if replyPostID != "" && agentBot != nil {
		if postUUID, perr := uuid.Parse(replyPostID); perr == nil {
			if res, cerr := botpost.PostCommentToPostAsBot(ctx, postUUID, text, agentBot); cerr == nil {
				// Make this reply audible to any agent it @mentions. No-op unless
				// delegation is enabled for this channel. The Result's HTMLText is
				// the final sanitized body that was actually written, so mention
				// extraction matches what readers see rather than the raw input.
				chName, html := "", text
				if res != nil {
					chName, html = res.ChannelName, res.HTMLText
				}
				EmitAgentMessage(ctx, Surface{Kind: SurfaceChannelPost, ChannelID: channelID, PostID: replyPostID},
					replyPostID, chName, replyPostID, html, agentBot)
				return
			} else {
				helpers.LogErrorWithContext(ctx, "agentTriggers: in-thread reply failed for agent %s on post %s, falling back to channel post: %v", a.Id, replyPostID, cerr)
			}
		}
	}
	postAgentMessage(ctx, a, agentBot, channelID, text)
}

// postAgentMessage posts text to a channel as the agent's own principal,
// falling back to the shared bot under the agent's display name if the
// principal couldn't be resolved. Best-effort and loop-safe (botpost emits no
// post.created).
func postAgentMessage(ctx context.Context, a *model.AiAgent, agentBot *userBusiness.BotIdentity, channelID, text string) {
	chUUID, perr := uuid.Parse(channelID)
	if perr != nil {
		return
	}
	var berr error
	var res *botpost.Result
	if agentBot != nil {
		res, berr = botpost.PostToChannelAsBot(ctx, chUUID, text, agentBot)
	} else {
		res, berr = botpost.PostToChannelAs(ctx, chUUID, text, a.Name)
	}
	if berr != nil {
		helpers.LogErrorWithContext(ctx, "agentTriggers: failed to post for agent %s in channel %s: %v", a.Id, channelID, berr)
		return
	}
	// Only the agent's OWN principal is emitted as a delegating author: a reply
	// written through the shared automation bot cannot be attributed to one agent,
	// so resolving it back would name the wrong delegator.
	if agentBot != nil {
		postID, chName, html := "", "", text
		if res != nil {
			postID, chName, html = res.PostUUID, res.ChannelName, res.HTMLText
		}
		EmitAgentMessage(ctx, Surface{Kind: SurfaceChannelPost, ChannelID: channelID, PostID: postID},
			postID, chName, postID, html, agentBot)
	}
}

// scheduledNothingSentinel lets a scheduled check-in agent stay silent when it
// has nothing noteworthy to report, so a recurring run never spams a channel.
const scheduledNothingSentinel = "NOTHING_TO_REPORT"

// scheduledCheckinPrompt instructs a scheduled agent that posts to a channel to
// compose a concise team update, or opt out of posting via the sentinel.
const scheduledCheckinPrompt = `It is your scheduled run. Use your tools as needed, then write a concise, skimmable update to post in the channel for the team. ` +
	`If there is nothing noteworthy to report, reply with exactly ` + scheduledNothingSentinel + ` and nothing else.`

// scheduledCheckinText decides what (if anything) a scheduled run posts to its
// channels: the agent's update when there is one, nothing on an empty/failed
// run (no recurring failure noise) or when the agent reported nothing to say.
func scheduledCheckinText(outcome *RunOutcome) (string, bool) {
	if outcome == nil {
		return "", false
	}
	res := strings.TrimSpace(outcome.Result)
	if res == "" {
		return "", false
	}
	if strings.Contains(strings.ToUpper(res), scheduledNothingSentinel) {
		return "", false
	}
	return res, true
}

// agentMentionReply decides what (if anything) to post to the channel for a
// mention-triggered run. A real answer is posted as-is. With no answer it
// returns a brief honest note so an @mention is never met with silence
// (Req 6.2) — except for transient throttle/circuit stops, where staying
// silent avoids a reply storm during an outage. A token-budget stop gets a
// short, explainable pause message.
func agentMentionReply(outcome *RunOutcome) (string, bool) {
	if outcome == nil {
		return "", false
	}
	footer := proposedApprovalFooter(outcome.Proposed)
	if body := strings.TrimSpace(outcome.Result); body != "" {
		if footer != "" {
			body += "\n\n" + footer
		}
		if tf := toolsUsedFooter(outcome.ToolsSucceeded); tf != "" {
			body += "\n\n" + tf
		}
		if nf := failedToolsNote(outcome.FailedTools); nf != "" {
			body += "\n\n" + nf
		}
		return body, true
	}
	// No answer body. If the run proposed changes for approval, disclose that
	// rather than a generic note (the run did real work, it just needs a human).
	if footer != "" {
		return footer, true
	}
	return mentionNoAnswerReply(outcome)
}

// Stable user-facing messages for a mention run that produced no answer body.
const (
	budgetPausedMentionMsg = "I've paused for now — today's AI usage limit has been reached. It resets tomorrow, or an admin can raise it in AI settings."
	genericMentionRetryMsg = "I couldn't put together a reply just now. Please mention me again in a moment."
)

// mentionNoAnswerReply decides what a mention-triggered run that produced NO
// answer body should say in the channel. Like the durable worker's classifyStop,
// it switches on the runner's STABLE StopReason code (so a message-wording
// change can't silently alter who gets a reply), falling back to the legacy text
// match only for a blank code. A transient infra stop (breaker/rate limit)
// stays silent to avoid a reply storm during an outage; a budget stop explains
// the pause; anything else gets a brief honest "mention me again" note so an
// @mention is never met with silence (Req 6.2). Pure + DB-free for unit tests.
func mentionNoAnswerReply(outcome *RunOutcome) (string, bool) {
	switch outcome.StopReason {
	case StopReasonCircuitOpen, StopReasonRateLimited:
		return "", false
	case StopReasonUserBudget, StopReasonAgentBudget, StopReasonChannelBudget, StopReasonWorkspaceBudget:
		return budgetPausedMentionMsg, true
	case StopReasonRunTimeout, StopReasonStepLimit, StopReasonRunTokenLimit:
		return genericMentionRetryMsg, true
	}
	// Fallback for a blank/unknown code (older state or a future stop path that
	// hasn't set a code): preserve the prior text-based decision.
	reason := strings.ToLower(outcome.Error)
	switch {
	case strings.Contains(reason, "circuit") || strings.Contains(reason, "rate limit"):
		return "", false
	case strings.Contains(reason, "token budget"):
		return budgetPausedMentionMsg, true
	default:
		return genericMentionRetryMsg, true
	}
}

// proposedApprovalFooter renders a deterministic, informational disclosure that
// an approval-mode agent queued one or more writes that need a human's approval
// before they run. It is intentionally NOT an actionable Approve/Deny control:
// the proposal executes AS THE AGENT'S OWNER with permissions re-checked, so it
// must be approved only from the owner's pending-actions tray, never by
// arbitrary channel members. This keeps the channel honest about what the agent
// is about to do without depending on the model to narrate it. Returns "" when
// nothing was proposed; the list is capped so the note stays compact.
func proposedApprovalFooter(proposed []string) string {
	clean := make([]string, 0, len(proposed))
	for _, d := range proposed {
		if d = strings.TrimSpace(d); d != "" {
			clean = append(clean, d)
		}
	}
	if len(clean) == 0 {
		return ""
	}

	var b strings.Builder
	if len(clean) == 1 {
		b.WriteString("Heads up: I've proposed 1 change that needs approval before it runs: ")
	} else {
		b.WriteString("Heads up: I've proposed ")
		b.WriteString(strconv.Itoa(len(clean)))
		b.WriteString(" changes that need approval before they run: ")
	}

	const maxShown = 4
	shown := clean
	if len(shown) > maxShown {
		shown = shown[:maxShown]
	}
	b.WriteString(strings.Join(shown, "; "))
	if rest := len(clean) - len(shown); rest > 0 {
		b.WriteString("; and ")
		b.WriteString(strconv.Itoa(rest))
		b.WriteString(" more")
	}
	b.WriteString(".")
	return b.String()
}

// deref returns the string a *string points to, or "" when nil.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// agentSteerMaxMessages bounds how many recent channel messages are fed to a
// mention-triggered agent as conversation context (steerability). Kept modest
// so the context stays focused and the prompt small; the transcript is also
// token-bounded by the workspace context budget.
const agentSteerMaxMessages = 20

// mentionPromptWithContext enriches a mention-trigger prompt with the channel's
// recent conversation (steerability), so a follow-up @mention continues the
// thread with awareness of the prior exchange — including the agent's own
// agentRunPrompt assembles the final run input for a channel-surface launch:
// the base prompt, enriched with the recent channel transcript (mention
// context) and — when the triggering/parent post carries image attachments and
// a vision model is configured — a vision-derived description of those images.
// Both enrichments are additive + best-effort, so a run is never blocked by a
// missing transcript or an absent/unavailable vision model.
func agentRunPrompt(ctx context.Context, a *model.AiAgent, replyChannelID, replyPostID, prompt string) string {
	enriched := mentionPromptWithContext(ctx, a, replyChannelID, prompt)
	if replyPostID != "" {
		if img := buildImageContext(ctx, replyPostID); img != "" {
			enriched += img
		}
	}
	return enriched
}

// earlier replies. The transcript is fetched AS the agent's owner (so it
// respects that user's access); on any miss it returns the original prompt
// unchanged, so a run is never blocked by missing context. Only the mention
// reply path (replyChannelID set) is enriched.
func mentionPromptWithContext(ctx context.Context, a *model.AiAgent, replyChannelID, prompt string) string {
	if replyChannelID == "" {
		return prompt
	}
	ownerInfo, err := aiBusiness.BuildUserInfoByUserUUID(ctx, a.CreatedBy.String())
	if err != nil || ownerInfo == nil {
		return prompt
	}
	transcript := aiBusiness.GetRecentChannelTranscript(ctx, ownerInfo, replyChannelID, agentSteerMaxMessages)
	if strings.TrimSpace(transcript) == "" {
		return prompt
	}
	return "Recent conversation in this channel (oldest first), for context — you may have replied earlier in it:\n\"\"\"\n" +
		transcript + "\n\"\"\"\n\n" + prompt
}

// runAgentTyping publishes a channel typing indicator under the agent's name
// until stop is closed or the context is cancelled. It uses the agent's OWN
// principal (uuid + avatar) when resolved, falling back to the shared bot's
// identity so the indicator still renders if per-agent provisioning failed.
// Mirrors the AI coworker's typing behavior so an @mentioned agent shows it is
// working.
func runAgentTyping(ctx context.Context, agentName string, agentBot *userBusiness.BotIdentity, channelID string, stop <-chan struct{}) {
	var botUUID, profileKey string
	if agentBot != nil {
		botUUID = agentBot.UUID
		profileKey = agentBot.ProfileKey
	} else if bot := userBusiness.GetAutomationBot(ctx); bot != nil {
		botUUID = bot.UUID
		profileKey = bot.ProfileKey
	} else {
		return
	}
	publish := func() {
		mqttBusiness.PublishChannelTyping(&mqttStruct.MqttChannelTyping{
			UserName:    agentName,
			UserUUID:    botUUID,
			UserProfile: profileKey,
			ChannelUuid: channelID,
		}, channelID)
	}
	publish()
	t := time.NewTicker(4 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			publish()
		}
	}
}

// synthMentionPrompt builds the run input for a mention trigger: the message,
// plus the channel target so the agent can reply there if it chooses.
func synthMentionPrompt(channelID, channelName, authorName, text string) string {
	var b strings.Builder
	b.WriteString("You were mentioned")
	if strings.TrimSpace(channelName) != "" {
		b.WriteString(" in the channel \"" + channelName + "\"")
	}
	if strings.TrimSpace(channelID) != "" {
		b.WriteString(" (channel_uuid: " + channelID + ")")
	}
	if strings.TrimSpace(authorName) != "" {
		b.WriteString(" by " + authorName)
	}
	b.WriteString(".\n\nTheir message:\n\"\"\"\n")
	b.WriteString(strings.TrimSpace(text))
	b.WriteString("\n\"\"\"\n\nUse your tools as needed to take any actions, then compose a concise, helpful reply to their message. Your reply will be posted to the channel as you (a badged AI teammate), so do NOT use a send-message tool to reply here — just write the reply as your final answer.")
	return b.String()
}

// synthEventPrompt builds the run input for an event trigger from the event's
// salient fields.
func synthEventPrompt(eventType string, data map[string]interface{}) string {
	var b strings.Builder
	b.WriteString("The workspace event \"" + eventType + "\" just occurred")
	if lines := eventContextLines(data); lines != "" {
		b.WriteString(" with this context:\n")
		b.WriteString(lines)
	} else {
		b.WriteString(".")
	}
	b.WriteString("\n\nDecide whether this is relevant to your purpose. Use your tools as needed, then give a short summary of what you did. If nothing is needed, say so briefly.")
	return b.String()
}

// eventPromptKeys are the event payload fields safe and useful to surface to an
// agent. Anything else (internal ids, source markers) is omitted.
var eventPromptKeys = []string{
	"channel_id", "channel_name", "project_id", "task_id", "post_id",
	"author_name", "text", "old_status", "new_status", "status", "user_id", "title",
	// A project's own status by name ("QA"), beside the category above.
	"old_status_name", "new_status_name",
	// Which task moved, where, and who moved it; without these an agent told
	// "a task moved to QA" had only ids to go on.
	"task_name", "project_name", "updated_by_name",
	"table_id", "table_name", "row_id",
	// GitHub events (github.pr.opened / review_submitted / check_run.completed /
	// issue.opened) so a "PR-follow" event agent gets the repo + PR context.
	"owner", "repo", "pr_number", "pr_url", "review_state", "reviewer",
	"conclusion", "checks_total", "checks_passed", "checks_failed", "body",
}

// eventFieldMaxLen caps a single rendered event value so a large field (e.g. a
// PR review body) can't bloat the prompt.
const eventFieldMaxLen = 500

// eventContextLines renders the known event fields as "- key: value" lines.
// Scalar values (string / number / bool) are rendered; a long value is capped.
func eventContextLines(data map[string]interface{}) string {
	var b strings.Builder
	for _, k := range eventPromptKeys {
		v, ok := data[k]
		if !ok {
			continue
		}
		s := scalarToString(v)
		if strings.TrimSpace(s) == "" {
			continue
		}
		if len(s) > eventFieldMaxLen {
			s = strings.TrimSpace(s[:eventFieldMaxLen]) + "…"
		}
		b.WriteString("- " + k + ": " + s + "\n")
	}
	return strings.TrimSpace(b.String())
}

// scalarToString renders a scalar event value (string/int/float/bool) for the
// prompt, or "" for anything else (so a nested object/array is skipped rather
// than dumped as Go syntax).
func scalarToString(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		// Event ints often arrive as float64 through a generic map; render
		// whole numbers without a trailing ".0".
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return ""
	}
}

// internalEventPrefix marks event types that exist only to drive OneCamp's own
// dispatchers. They are not part of the workspace event vocabulary an agent may
// subscribe to.
const internalEventPrefix = "agent."

// isInternalEventType reports whether an event type is internal and therefore not
// bindable as an agent's event trigger.
//
// WHY THIS GUARD EXISTS. handleAgentEvent launches every event-trigger agent for
// the incoming type BEFORE it reaches the type-specific branches:
//
//	for _, a := range evAgents { launchAgent(ctx, a, model.TriggerEvent, …) }
//
// That loop is the generic path and performs no delegation checks — no hop budget,
// no cycle detection, no AuthorizeDelegation, and it passes an empty attributed-to
// user. agent.message is the event that carries delegation lineage, and the ONLY
// dispatcher that knows how to gate it is dispatchMentionAgents. So an agent bound
// to "agent.message" as a plain event trigger would fire on other agents' messages
// while skipping the check that the originating person could address it at all —
// exactly the privilege laundering AuthorizeDelegation exists to prevent — and
// would record a run with no human actor.
//
// The admin UI only offers a fixed list of real workspace events and does not
// include this one, so the gap is reachable only by writing trigger_config through
// the API directly. That is precisely why the guard lives HERE, at the point the
// cache is built, rather than in request validation: it holds however the row got
// written, including rows that predate this check.
//
// Matched by prefix rather than by exact name so a future internal event is
// covered the day it is added, instead of the day someone remembers this function.
func isInternalEventType(ev string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(ev)), internalEventPrefix)
}

// parseTriggerConfig decodes an agent's trigger_config JSON, returning a zero
// config on any error (treated as "no trigger configuration").
func parseTriggerConfig(a *model.AiAgent) triggerConfig {
	var cfg triggerConfig
	if strings.TrimSpace(a.TriggerConfig) != "" {
		_ = json.Unmarshal([]byte(a.TriggerConfig), &cfg)
	}
	return cfg
}
