package business

// Cross-surface "what needs me now" assistant — Wave 3, Requirement 1.
//
// One prioritized, read-only queue of everything that needs the member's
// action across every OneCamp surface, so they stop checking five places:
//
//   - pending write approvals (the durable Approve/Deny tray)
//   - their own overdue/assigned tasks (Dgraph "My Tasks", overdue filter)
//   - their own overdue commitments + open questions (workspace memory layer)
//   - upcoming calendar items (reuses the briefing's cross-connector "day")
//
// It is NOT a new aggregator: it fans out to the SAME permission-scoped
// sources the briefing/memory/pending-action surfaces already use, mirrors the
// briefing's parallel-with-timeout pattern, degrades gracefully (any source
// that errors/times out is omitted), and never surfaces anything the member
// could not already see. No LLM call: this is a cheap, bounded assembler — the
// "follow-up question" path (Req 1.4) rides the existing AskAI surface.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/AI"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	memoryModels "github.com/akashc777/OneCamp/models/postgres/WorkspaceMemory"
	"github.com/google/uuid"
)

const (
	attentionTimeout      = 1500 * time.Millisecond
	attentionMaxApprovals = 10
	attentionMaxTasks     = 15
	attentionMaxMemory    = 12
	attentionMaxCalendar  = 6
	attentionMaxTotal     = 40

	// Priority tiers (lower = more urgent). Within a tier, dated items sort
	// earliest-due first.
	prApproval = 0
	prOverdue  = 1
	prDueSoon  = 2
	prCalendar = 3
	prQuestion = 4

	// taskDueSoonWindow is how far ahead a not-yet-overdue assigned task counts
	// as "needs me now". Tz-independent (a rolling 24h from the request).
	taskDueSoonWindow = 24 * time.Hour
)

// GetWhatNeedsMe assembles the member's cross-surface attention queue. Never
// errors on partial failure — it returns whatever it could gather within the
// budget. Returns enabled=false (empty) when AI is off so the FE hides it.
func GetWhatNeedsMe(ctx context.Context, userInfo *userModels.UserInfo) (*adapter.AttentionResponse, error) {
	resp := &adapter.AttentionResponse{
		Enabled: false,
		Items:   []adapter.AttentionItem{},
		Counts:  map[string]int{},
	}

	settings, err := getAISettingsForMemory(ctx)
	if err != nil || !settings.enabled {
		return resp, nil
	}
	resp.Enabled = true

	bctx, cancel := context.WithTimeout(ctx, attentionTimeout)
	defer cancel()

	approvalsCh := make(chan []adapter.AttentionItem, 1)
	tasksCh := make(chan []adapter.AttentionItem, 1)
	memoryCh := make(chan []adapter.AttentionItem, 1)
	// Calendar runs on the parent ctx (external API) with its own deadline,
	// joined under a hard backstop below — same shape as the briefing.
	calCh := make(chan []adapter.AttentionItem, 1)

	helpers.GoSafeSend("attention.approvals", approvalsCh, func() []adapter.AttentionItem {
		return gatherApprovalItems(bctx, userInfo)
	})
	helpers.GoSafeSend("attention.tasks", tasksCh, func() []adapter.AttentionItem {
		return gatherOverdueTaskItems(bctx, userInfo)
	})
	helpers.GoSafeSend("attention.memory", memoryCh, func() []adapter.AttentionItem {
		if !settings.memoryEnabled {
			return nil
		}
		return gatherMemoryAttentionItems(bctx, userInfo)
	})
	helpers.GoSafeSend("attention.calendar", calCh, func() []adapter.AttentionItem {
		return gatherCalendarAttentionItems(ctx, userInfo)
	})

	// Collect the three bounded sources under the shared deadline.
	for i := 0; i < 3; i++ {
		select {
		case it := <-approvalsCh:
			resp.Items = append(resp.Items, it...)
		case it := <-tasksCh:
			resp.Items = append(resp.Items, it...)
		case it := <-memoryCh:
			resp.Items = append(resp.Items, it...)
		case <-bctx.Done():
			i = 3 // budget exhausted; ship what we have
		}
	}

	// Join the (external) calendar agenda, bounded so a slow API can't hold
	// the surface.
	select {
	case it := <-calCh:
		resp.Items = append(resp.Items, it...)
	case <-time.After(700 * time.Millisecond):
	}

	sortAttention(resp.Items)
	if len(resp.Items) > attentionMaxTotal {
		resp.Items = resp.Items[:attentionMaxTotal]
	}
	for _, it := range resp.Items {
		resp.Counts[it.Source]++
	}
	return resp, nil
}

// sortAttention orders by urgency tier, then earliest due date within a tier,
// so the single list reads top-to-bottom as "do this first".
func sortAttention(items []adapter.AttentionItem) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Priority != items[j].Priority {
			return items[i].Priority < items[j].Priority
		}
		// Both dated: earlier first. A dated item sorts before an undated one.
		if items[i].DueAt != "" && items[j].DueAt != "" {
			return items[i].DueAt < items[j].DueAt
		}
		return items[i].DueAt != "" && items[j].DueAt == ""
	})
}

// gatherApprovalItems pulls the member's open write approvals (the highest-
// urgency tier — something is waiting on an explicit yes/no).
func gatherApprovalItems(ctx context.Context, userInfo *userModels.UserInfo) []adapter.AttentionItem {
	uid, perr := uuid.Parse(userInfo.UserDgraphInfo.Uuid)
	if perr != nil {
		return nil
	}
	actions, err := ListOpenPendingActions(ctx, uid)
	if err != nil {
		helpers.LogInfoWithContext(ctx, "attention: approvals failed: %v", err)
		return nil
	}
	out := make([]adapter.AttentionItem, 0, len(actions))
	for _, a := range actions {
		if len(out) >= attentionMaxApprovals {
			break
		}
		title := strings.TrimSpace(a.Description)
		if title == "" {
			title = a.ToolName
		}
		out = append(out, adapter.AttentionItem{
			Source:   "approval",
			Kind:     "Needs your approval",
			Title:    trimOneLine(title, 200),
			Subtitle: "Waiting for you to approve or deny",
			Priority: prApproval,
			RefID:    a.Id.String(),
		})
	}
	return out
}

// gatherOverdueTaskItems pulls the member's OWN assigned tasks that need them
// now — anything overdue, plus anything due within the next 24h — via the same
// Dgraph user-task list the AI task agent uses (so it can never surface a task
// the user couldn't already see in "My Tasks").
func gatherOverdueTaskItems(ctx context.Context, userInfo *userModels.UserInfo) []adapter.AttentionItem {
	now := time.Now()
	// Single query: not-done tasks with a real due date up to 24h from now.
	// This captures both long-overdue tasks (due in the past) and due-soon
	// tasks; each is classified below by comparing its due date to now.
	upper := now.Add(taskDueSoonWindow).Format(time.RFC3339Nano)
	filter := fmt.Sprintf(dgraphStruct.TASK_OPEN_FILTER+` AND lt(task_due_date, "%s") AND gt(task_due_date, "1970-01-01T00:00:00Z")`, upper)

	dgraphUser, err := userBusiness.GetDgraphUserTaskList(
		ctx,
		userInfo.UserDgraphInfo.Uuid,
		userInfo.UserDgraphInfo.Uid,
		filter,
		"orderasc: task_due_date",
		attentionMaxTasks,
		0,
		false,
	)
	if err != nil {
		helpers.LogInfoWithContext(ctx, "attention: tasks failed: %v", err)
		return nil
	}
	if dgraphUser == nil {
		return nil
	}
	out := make([]adapter.AttentionItem, 0, len(dgraphUser.Tasks))
	for _, t := range dgraphUser.Tasks {
		out = append(out, taskToAttentionItem(t, now))
	}
	return out
}

// taskToAttentionItem renders one assigned task as an attention row, classified
// as overdue (due in the past) or due-soon (due within the window).
func taskToAttentionItem(t *dgraphStruct.DgraphTask, now time.Time) adapter.AttentionItem {
	name := strings.TrimSpace(t.Name)
	if name == "" {
		name = "(untitled task)"
	}
	overdue := t.DueDate != nil && t.DueDate.Before(now)
	kind := "Due soon"
	priority := prDueSoon
	sub := "Due soon"
	if overdue {
		kind = "Overdue task"
		priority = prOverdue
		sub = "Overdue task"
	}
	due, dueTime := "", ""
	if t.DueDate != nil && t.DueDate.Year() > 1970 {
		due = t.DueDate.Format("2006-01-02")
		dueTime = t.DueDate.UTC().Format(time.RFC3339)
		if overdue {
			sub = "Was due " + t.DueDate.Format("Jan 2")
		} else {
			sub = "Due " + t.DueDate.Format("Jan 2, 3:04 PM")
		}
	}
	project := ""
	if t.Project != nil {
		project = strings.TrimSpace(t.Project.Name)
	}
	if project != "" {
		sub += " · " + project
	}
	url := ""
	if strings.TrimSpace(t.Uuid) != "" {
		url = "/app/task/" + t.Uuid
	}
	return adapter.AttentionItem{
		Source:   "task",
		Kind:     kind,
		Title:    trimOneLine(name, 200),
		Subtitle: sub,
		URL:      url,
		DueAt:    due,
		DueTime:  dueTime,
		Context:  project,
		Priority: priority,
	}
}

// gatherMemoryAttentionItems pulls the member's OWN open commitments (overdue
// ones are urgent) and open questions from the workspace memory layer. Reuses
// the same owner-scoped query + scope enrichment as the briefing.
func gatherMemoryAttentionItems(ctx context.Context, userInfo *userModels.UserInfo) []adapter.AttentionItem {
	ownerID := userInfo.UserPostgresInfo.Id
	items, err := memoryModels.List(ctx, memoryModels.QueryFilter{
		Statuses: []string{memoryModels.StatusOpen},
		Kinds:    []string{memoryModels.KindCommitment, memoryModels.KindQuestion},
		OwnerID:  &ownerID,
		Limit:    attentionMaxMemory,
	})
	if err != nil {
		helpers.LogInfoWithContext(ctx, "attention: memory items failed: %v", err)
		return nil
	}
	today := time.Now().Format("2006-01-02")
	resolver := newScopeResolver(userInfo)
	out := make([]adapter.AttentionItem, 0, len(items))
	for _, m := range items {
		v := toMemoryView(m)
		resolver.enrich(&v)
		item := adapter.AttentionItem{
			Source: "commitment",
			Title:  trimOneLine(v.Content, 200),
			URL:    "/app/ai/memory?item=" + v.ID,
			DueAt:  v.DueAt,
		}
		if v.ScopeLabel != "" {
			item.Subtitle = v.ScopeLabel
		}
		switch v.Kind {
		case memoryModels.KindCommitment:
			// Only OVERDUE commitments belong in "needs me now"; upcoming ones
			// live in the briefing. Undated commitments are treated as pending.
			if v.DueAt != "" && v.DueAt >= today {
				continue
			}
			item.Kind = "Overdue commitment"
			item.Priority = prOverdue
			if v.DueAt != "" {
				pre := "Was due " + v.DueAt
				if item.Subtitle != "" {
					item.Subtitle = pre + " · " + item.Subtitle
				} else {
					item.Subtitle = pre
				}
			}
		case memoryModels.KindQuestion:
			item.Source = "question"
			item.Kind = "Open question"
			item.Priority = prQuestion
			if item.Subtitle == "" {
				item.Subtitle = "Still unresolved"
			}
		default:
			continue
		}
		out = append(out, item)
	}
	return out
}

// gatherCalendarAttentionItems reuses the briefing's cross-connector "day"
// assembler and keeps only the calendar entries (upcoming events the member
// should be ready for). Returns nothing when no calendar connector is linked.
func gatherCalendarAttentionItems(ctx context.Context, userInfo *userModels.UserInfo) []adapter.AttentionItem {
	day := gatherConnectorDay(ctx, userInfo)
	out := make([]adapter.AttentionItem, 0, attentionMaxCalendar)
	for _, d := range day {
		if d.Source != "calendar" {
			continue
		}
		if len(out) >= attentionMaxCalendar {
			break
		}
		out = append(out, adapter.AttentionItem{
			Source:   "calendar",
			Kind:     "Upcoming event",
			Title:    d.Title,
			Subtitle: d.Subtitle,
			URL:      d.URL,
			Priority: prCalendar,
		})
	}
	return out
}
