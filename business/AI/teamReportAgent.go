package business

// Memory-grounded team report — an opt-in ambient agent.
//
// WHY
// ---
// The recap agent summarizes a single call; the digest nudges an
// individual. This agent closes the loop at the TEAM level: on a cadence it
// posts a "state of the channel" report INTO each active channel —
// decisions made, commitments outstanding (with owners), and open questions
// — grounded in the structured memory graph plus a short recent-activity
// window. It turns the invisible memory layer into a visible, recurring
// team ritual, the kind of workspace-aware automation a chat-only tool
// can't do because it owns neither the structured knowledge nor the
// destination surface.
//
// PRODUCTION PROPERTIES
//   - Opt-in: gated on ai_settings (AI + memory layer enabled) AND its own
//     ai_settings.team_report_enabled flag.
//   - Permission-correct: the report is authored by the channel's creator
//     (a real member), so it lands with genuine access — no synthetic bot
//     identity that could leak content.
//   - Grounded, not hallucinated: the body is built from the GraphRAG scope
//     read (real, owner-attributed items) + a bounded recent-activity
//     window; the LLM only narrates a short intro. If there's nothing
//     durable to report, it posts nothing.
//   - Cheap + bounded: discovers only channels with recent activity (one
//     OpenSearch aggregation), caps channels per run, one LLM call per
//     channel for the intro, circuit-breaker guarded.
//   - Idempotent: a per-(channel, period) Redis lock means a channel gets
//     at most one report per period across overlapping ticks/instances.

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	ai "github.com/akashc777/OneCamp/services/AI"
)

const (
	teamReportInitialDelay = 20 * time.Minute
	teamReportTickInterval = 1 * time.Hour

	// Default local hour + weekday to post (Monday 09:00). Tunable via
	// AI_TEAM_REPORT_HOUR. Weekly cadence keeps it high-signal, not noise.
	teamReportDefaultHour = 9

	// Bounds per run.
	teamReportMaxChannels   = 100
	teamReportMaxItems      = 40
	teamReportActivityHours = 7 * 24 * time.Hour // discovery window

	// Minimum open items to bother posting a report (below this it's noise).
	teamReportMinItems = 2
)

// teamReportSystemPrompt asks for a SHORT narrative intro only; the
// structured body is appended deterministically from real data.
const teamReportSystemPrompt = `You are OneCamp's team assistant writing a brief weekly channel update.
Given a list of the channel's open decisions, commitments, and questions, write ONLY a 1-2 sentence plain-text intro that frames the week's state (e.g. what's progressing, what needs attention).
Rules:
- Use ONLY the provided items. Never invent decisions, owners, or dates.
- No markdown, no headings, no bullet points — just 1-2 sentences.
- If the items don't warrant an intro, reply with exactly: SKIP_REPORT`

// StartTeamReportLoop launches the team-report agent. Self-gates on
// settings every tick; inert until enabled. ctx drives shutdown.
func StartTeamReportLoop(ctx context.Context) {
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(teamReportInitialDelay):
		}
		ticker := time.NewTicker(teamReportTickInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				helpers.MessageLogs.InfoLog.Println("Team report worker shutting down")
				return
			case <-ticker.C:
				maybeRunTeamReports(ctx)
			}
		}
	}()
}

// maybeRunTeamReports fires weekly on the configured local hour/weekday.
func maybeRunTeamReports(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			helpers.MessageLogs.ErrorLog.Printf("panic in team report run: %v", r)
		}
	}()

	now := time.Now()
	if now.Weekday() != time.Monday || now.Hour() != teamReportHour() {
		return
	}

	settings, err := getTeamReportSettings(ctx)
	if err != nil || !settings.enabled || !settings.memoryEnabled || !settings.teamReportEnabled {
		return
	}
	if svc := ai.GetService(); svc == nil || !svc.IsEnabled() {
		return
	}

	posted, processed := runTeamReports(ctx, false)
	helpers.LogInfoWithContext(ctx, "team report: posted %d report(s) across %d active channel(s)", posted, processed)
}

// runTeamReports executes one pass: discover active channels and post a
// report to each. When force is true it bypasses the per-(channel,period)
// idempotency lock — used by the admin "run now" verify action so a report is
// posted even if one already went out this period. Returns (posted, processed).
func runTeamReports(ctx context.Context, force bool) (int, int) {
	now := time.Now()
	// Discover channels with recent activity (reuses the worker's generic
	// aggregation; we only act on the channel dimension here).
	since := now.Add(-teamReportActivityHours).Unix()
	scopes, err := ai.DiscoverActiveScopes(ctx, since, teamReportMaxChannels*2)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "team report: discover scopes failed: %v", err)
		return 0, 0
	}

	// Which channels opted in. Read ONCE per run and used as a filter, rather
	// than asked per channel: discovery returns up to 100 channels and the
	// opted-in set is normally much smaller. An empty set means nothing posts,
	// which is the correct behaviour on the day the org switch is first turned
	// on and no channel has chosen yet.
	enabledChannels, err := aiModels.TeamReportEnabledChannels(ctx)
	if err != nil {
		// Fail CLOSED. Posting into channels that never opted in is exactly the
		// behaviour this gate exists to prevent, so a lookup failure must post
		// nothing rather than fall back to "everywhere".
		helpers.LogErrorWithContext(ctx, "team report: cannot read per-channel opt-ins, skipping run: %v", err)
		return 0, 0
	}
	if len(enabledChannels) == 0 {
		return 0, 0
	}

	period := now.Format("2006-01-02")
	posted, processed := 0, 0
	for _, sc := range scopes {
		if sc.FilterType != "channel_uuid" {
			continue
		}
		if !enabledChannels[sc.FilterValue] {
			continue
		}
		if processed >= teamReportMaxChannels {
			break
		}
		processed++
		if ctx.Err() != nil {
			return posted, processed
		}
		if postChannelReport(ctx, sc.FilterValue, period, force) {
			posted++
		}
	}
	return posted, processed
}

// RunTeamReportNowForVerify runs the team report immediately (bypassing the
// weekly schedule and the idempotency lock) so an admin can verify it end to
// end. Requires Workspace AI + Workspace Memory to be enabled (it reads the
// memory graph and writes a short LLM intro). Returns how many channels were
// processed and how many reports were actually posted.
func RunTeamReportNowForVerify(ctx context.Context) (posted int, processed int, err error) {
	settings, gerr := getTeamReportSettings(ctx)
	if gerr != nil {
		return 0, 0, gerr
	}
	if !settings.enabled {
		return 0, 0, fmt.Errorf("Workspace AI is disabled — enable it first")
	}
	if !settings.memoryEnabled {
		return 0, 0, fmt.Errorf("Workspace Memory is disabled — the team report is built from it")
	}
	if svc := ai.GetService(); svc == nil || !svc.IsEnabled() {
		return 0, 0, fmt.Errorf("the AI service is not available")
	}
	posted, processed = runTeamReports(ctx, true)
	return posted, processed, nil
}

// postChannelReport builds and posts one channel's report. Returns true
// when a report was actually posted. Best-effort; logs and returns on any
// failure so one channel never stalls the run.
func postChannelReport(ctx context.Context, channelUUID, period string, force bool) bool {
	// Idempotency: one report per (channel, period). Bypassed for a forced
	// admin verify run so a test always posts.
	if !force {
		lockKey := channelUUID + ":" + period
		if res := redisStore.AllowFixedWindow(ctx, registry.AITeamReportLock, []string{lockKey}, 1); !res.Allowed {
			return false
		}
	}

	// Pull the channel's open items via GraphRAG (owner-attributed).
	items := fetchGraphScopeItems(ctx, "channel", channelUUID, teamReportMaxItems)
	if len(items) < teamReportMinItems {
		return false
	}

	// Resolve the channel + its creator (the permission-correct author).
	authorInfo, channelName, err := resolveChannelReportAuthor(ctx, channelUUID)
	if err != nil || authorInfo == nil {
		helpers.LogInfoWithContext(ctx, "team report: no eligible author for channel %q (%v)", channelUUID, err)
		return false
	}

	// Build the structured body deterministically from real items.
	body := formatGraphScopeItemsForLLM("#"+channelName, items)
	if strings.TrimSpace(body) == "" {
		return false
	}

	// Ask the LLM for a short intro only (circuit-breaker guarded). The
	// body is real data; the intro is the only generated text, so a model
	// failure degrades gracefully to "no intro".
	intro := buildReportIntro(ctx, body)

	html := teamReportToHTML(intro, items, channelName)
	if html == "" {
		return false
	}

	if err := deliverRecapToChannel(ctx, channelUUID, authorInfo, html); err != nil {
		helpers.LogInfoWithContext(ctx, "team report: deliver to channel %q failed: %v", channelUUID, err)
		return false
	}
	return true
}

// buildReportIntro asks the active LLM for a 1-2 sentence intro; returns ""
// on any failure or SKIP. Best-effort and bounded.
func buildReportIntro(ctx context.Context, structuredBody string) string {
	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return ""
	}
	if err := svc.Resiliency.CB.Allow(); err != nil {
		return ""
	}
	intro, err := svc.Summarize(ctx, structuredBody, teamReportSystemPrompt)
	if err != nil {
		svc.Resiliency.CB.RecordResult(err)
		return ""
	}
	svc.Resiliency.CB.RecordSuccess()
	intro = strings.TrimSpace(SanitizeResponse(intro))
	if intro == "" || strings.Contains(intro, "SKIP_REPORT") {
		return ""
	}
	return intro
}

// resolveChannelReportAuthor returns the channel's creator as a full
// UserInfo (a real member with access) plus the channel name. The creator
// is the natural, permission-correct author for a channel-wide post.
func resolveChannelReportAuthor(ctx context.Context, channelUUID string) (*userModels.UserInfo, string, error) {
	// We only need ch_created_by + ch_name here; the userDgraphUUID arg
	// drives is_member/is_admin counts we don't use, so empty is fine.
	dgraphChannel, err := channelDomain.GetDgraphChannelInfoByUUID(ctx, channelUUID, "")
	if err != nil || dgraphChannel == nil {
		return nil, "", fmt.Errorf("channel lookup failed: %w", err)
	}
	if dgraphChannel.CreatedBy == nil || dgraphChannel.CreatedBy.Uuid == "" {
		return nil, dgraphChannel.Name, fmt.Errorf("channel has no resolvable creator")
	}
	authorInfo, err := getUserInfoForExecutor(ctx, dgraphChannel.CreatedBy.Uuid)
	if err != nil || authorInfo == nil {
		return nil, dgraphChannel.Name, fmt.Errorf("resolve creator info: %w", err)
	}
	return authorInfo, dgraphChannel.Name, nil
}

// teamReportToHTML composes the final post HTML: a header, the optional
// LLM intro, then the structured (already-rendered) items as paragraphs.
func teamReportToHTML(intro string, items []*dgraphStruct.DgraphMemoryItem, channelName string) string {
	if len(items) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("<p><strong>📊 Weekly Channel Report</strong></p>")
	if intro != "" {
		sb.WriteString("<p>")
		sb.WriteString(helpers.RemoveHTMLTags(intro))
		sb.WriteString("</p>")
	}

	var decisions, commitments, questions []*dgraphStruct.DgraphMemoryItem
	for _, it := range items {
		switch it.Kind {
		case "decision":
			decisions = append(decisions, it)
		case "commitment":
			commitments = append(commitments, it)
		default:
			questions = append(questions, it)
		}
	}
	writeReportSection(&sb, "✅ Decisions", decisions, false)
	writeReportSection(&sb, "📋 Commitments", commitments, true)
	writeReportSection(&sb, "❓ Open Questions", questions, true)
	return sb.String()
}

func writeReportSection(sb *strings.Builder, heading string, items []*dgraphStruct.DgraphMemoryItem, showOwner bool) {
	if len(items) == 0 {
		return
	}
	sb.WriteString("<p><strong>")
	sb.WriteString(heading)
	sb.WriteString("</strong></p>")
	for _, it := range items {
		line := strings.TrimSpace(helpers.RemoveHTMLTags(it.Content))
		if len(line) > 240 {
			line = line[:240] + "…"
		}
		owner := ""
		if showOwner && it.Owner != nil {
			name := it.Owner.UserName
			if name == "" {
				name = it.Owner.UserFullName
			}
			if name != "" {
				owner = " — @" + helpers.RemoveHTMLTags(name)
			}
		}
		due := ""
		if it.DueAt != nil {
			due = " (due " + it.DueAt.Format("2006-01-02") + ")"
		}
		sb.WriteString("<p>• ")
		sb.WriteString(line)
		sb.WriteString(owner)
		sb.WriteString(due)
		sb.WriteString("</p>")
	}
}

// teamReportHour reads AI_TEAM_REPORT_HOUR (0-23), default teamReportDefaultHour.
func teamReportHour() int {
	v := strings.TrimSpace(os.Getenv("AI_TEAM_REPORT_HOUR"))
	if v == "" {
		return teamReportDefaultHour
	}
	h, err := strconv.Atoi(v)
	if err != nil || h < 0 || h > 23 {
		return teamReportDefaultHour
	}
	return h
}

// teamReportSettings is the gate projection for this agent.
type teamReportSettings struct {
	enabled           bool
	memoryEnabled     bool
	teamReportEnabled bool
}

func getTeamReportSettings(ctx context.Context) (teamReportSettings, error) {
	s, err := aiModels.GetSettings(ctx)
	if err != nil {
		return teamReportSettings{}, err
	}
	return teamReportSettings{
		enabled:           s.Enabled,
		memoryEnabled:     s.MemoryLayerEnabled,
		teamReportEnabled: s.TeamReportEnabled,
	}, nil
}
