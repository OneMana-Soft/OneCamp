package business

// On-demand "turn discussion into tasks" — extract action items from a
// conversation (channel / DM / group) or pasted text, then let the user
// batch-approve creating them as real tasks.
//
// This is the discussion->action loop a collaboration tool needs: a thread
// says "Alice will draft the spec by Friday, and we should follow up on
// pricing" and the user one-clicks those into the tracker instead of
// copy-pasting. Read-then-approve: extraction is read-only and proposes
// tasks; nothing is created until the user confirms with a target project.
//
// Properties:
//   - Provider-agnostic via the AI service chokepoint (per-user model, rate
//     limit, circuit breaker, JSON mode).
//   - Permission-correct: the source transcript is pulled through the same
//     access-scoped helper the assistant uses, and creation goes through the
//     normal task path (project-admin enforced), so it can never do more than
//     the user could by hand.
//   - Grounded + bounded: the model uses only the supplied text and returns a
//     capped, strict-JSON array; the parser is pure and defensively validated.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	taskAdapter "github.com/akashc777/OneCamp/adapter/Task"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	transcriptDomain "github.com/akashc777/OneCamp/domain/LiveKit"
	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

const (
	extractMaxTasks       = 20
	extractMaxTitleLen    = 160
	extractMaxDescLen     = 400
	extractMinSourceChars = 40
	extractMaxSourceChars = 16000
	extractTranscriptMsgs = 80
)

// ProposedTask is one candidate task the model extracted (+ resolved assignee).
type ProposedTask struct {
	Title        string `json:"title"`
	Description  string `json:"description,omitempty"`
	AssigneeName string `json:"assignee_name,omitempty"` // display name (resolved or as-stated)
	AssigneeUUID string `json:"assignee_uuid,omitempty"` // resolved user UUID, else empty
	Due          string `json:"due,omitempty"`           // RFC3339, else empty
	Priority     string `json:"priority,omitempty"`      // low|medium|high
}

// ExtractTasksResult is the proposal returned to the batch-approve UI.
type ExtractTasksResult struct {
	Enabled bool           `json:"enabled"`
	Tasks   []ProposedTask `json:"tasks"`
	Note    string         `json:"note,omitempty"`
	// ScannedCount is how many recent messages (or transcript lines) the
	// assistant looked at to produce this proposal. It powers a transparent
	// "scanned N recent messages" caption so the user understands the scope
	// (the recent window, not the whole history) without exposing any internal
	// cap. 0 for pasted-text sources (the user supplied the content directly).
	ScannedCount int `json:"scanned_count,omitempty"`
}

// taskExtractionPrompt asks for a strict JSON array of action items.
const taskExtractionPrompt = `You extract ACTION ITEMS from workspace text and turn them into tasks.
Return ONLY a JSON object (no prose, no markdown fences) of this shape:
{"tasks": [
  {
    "title": "<short imperative task title>",
    "assignee": "<person name if a specific owner is stated, else empty>",
    "due": "<YYYY-MM-DD if a deadline is stated, else empty>",
    "priority": "low" | "medium" | "high",
    "details": "<one short line of context, optional>"
  }
]}

Rules:
- Extract only CONCRETE things someone should DO (commitments, follow-ups, requests). Ignore decisions, opinions, and questions that only ask for information.
- In chat, most requests are phrased as questions: "can we get the rollback steps in before Thursday?" or "could you send the numbers?" asks for work, so it IS an action item. Title it as the work ("Add the rollback steps before Thursday").
- Use ONLY what the text says. Never invent owners, dates, or work.
- Title is a short imperative ("Draft the pricing doc"), not a sentence copied verbatim.
- Resolve relative dates against today's date if the text states one; otherwise leave "due" empty.
- Return at most 20 items. If there are no clear action items, return exactly: {"tasks": []}
- Output MUST be valid JSON. No trailing commas, no comments.`

// rawProposedTask is the model's per-item JSON shape.
type rawProposedTask struct {
	Title    string `json:"title"`
	Assignee string `json:"assignee"`
	Due      string `json:"due"`
	Priority string `json:"priority"`
	Details  string `json:"details"`
}

// ExtractActionItems pulls the source text (permission-scoped) and asks the
// model for candidate tasks. Read-only. sourceType is one of channel | dm |
// group | meeting | text; for text, rawText carries the content, and for
// meeting, sourceID is the call room name (its transcript is used).
func ExtractActionItems(ctx context.Context, userInfo *userModels.UserInfo, sourceType, sourceID, rawText string) (*ExtractTasksResult, error) {
	resp := &ExtractTasksResult{Enabled: false, Tasks: []ProposedTask{}}

	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return resp, nil // AI off → caller hides the surface
	}
	resp.Enabled = true

	text, scanned := gatherExtractionSource(ctx, userInfo, sourceType, sourceID, rawText)
	resp.ScannedCount = scanned
	text = strings.TrimSpace(text)
	if len([]rune(text)) > extractMaxSourceChars {
		text = string([]rune(text)[:extractMaxSourceChars])
	}
	if len(text) < extractMinSourceChars {
		resp.Note = "There isn't enough here to pull action items from."
		return resp, nil
	}

	userUUID := userInfo.UserDgraphInfo.Uuid
	if err := svc.Resiliency.CheckRateLimit(ctx, userUUID); err != nil {
		return nil, err
	}
	llm, cb := svc.ResolveUserModel(ctx, userUUID)
	if llm == nil {
		return nil, fmt.Errorf("AI is not enabled for this workspace")
	}
	if err := cb.Allow(); err != nil {
		return nil, err
	}

	user := extractionCalendar(time.Now().UTC()) + "\n\nText:\n" + text
	out, err := ai.ChatJSONWithRetry(ctx, llm, cb, taskExtractionPrompt, user,
		ai.ChatOptions{Temperature: 0.2, MaxTokens: 1200},
		func(s string) bool { return isValidTaskJSON(s) })
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/AI/ExtractActionItems model call failed err: %v", err)
		resp.Note = "Couldn't read the action items right now. Try again in a moment."
		return resp, nil
	}

	raws := parseProposedTasks(out)
	if len(raws) == 0 {
		resp.Note = "No clear action items found."
		return resp, nil
	}

	// Resolve assignees (name -> user) and normalize due/priority. Resolution
	// is best-effort: an unmatched name is kept as a label so the user sees it.
	for _, rt := range raws {
		pt := ProposedTask{
			Title:       rt.Title,
			Description: rt.Details,
			Priority:    normalizeTaskPriority(rt.Priority),
		}
		if name := strings.TrimSpace(rt.Assignee); name != "" {
			pt.AssigneeName = name
			if u, rerr := resolveAssigneeRef(ctx, userUUID, name); rerr == nil && u != nil {
				pt.AssigneeUUID = u.Uuid
				if u.UserName != "" {
					pt.AssigneeName = u.UserName
				}
			}
		}
		if due := normalizeDueDate(rt.Due); due != "" {
			pt.Due = due
		}
		resp.Tasks = append(resp.Tasks, pt)
	}
	return resp, nil
}

// gatherExtractionSource returns the source text for extraction (permission
// scoped) plus the number of messages/lines it scanned. For conversations it
// reuses the same access-checked transcript helper the assistant uses; for
// "text" it returns the caller-supplied body with a scanned count of 0 (the
// user supplied the content directly, so there's nothing to disclose).
func gatherExtractionSource(ctx context.Context, userInfo *userModels.UserInfo, sourceType, sourceID, rawText string) (string, int) {
	switch strings.ToLower(strings.TrimSpace(sourceType)) {
	case "channel":
		return GetRecentConversationTranscriptWithCount(ctx, userInfo, "channel_uuid", sourceID, extractTranscriptMsgs)
	case "dm", "group":
		return GetRecentConversationTranscriptWithCount(ctx, userInfo, "chat_grp_id", sourceID, extractTranscriptMsgs)
	case "meeting", "recap":
		return gatherMeetingTranscript(ctx, userInfo, sourceID)
	default: // "text"
		return rawText, 0
	}
}

// gatherMeetingTranscript returns the formatted transcript of a call room for
// task extraction, but ONLY after verifying the requester has access to the
// surface the room maps to. Room-name shape mirrors the recap agent's delivery
// routing (deliverRecap):
//   - contains a space → DM (space-joined sorted user UUIDs)
//   - 32-char hex      → group chat (grouping id)
//   - UUID             → channel
//
// Transcripts live only as edges on a recording node, so this returns the
// full transcript of the room's most-recent recording plus the line count it
// scanned. Returns ("", 0) when the room is empty/unknown, the caller lacks
// access, or there's no transcript — the caller treats an empty/short result
// as "nothing to extract".
func gatherMeetingTranscript(ctx context.Context, userInfo *userModels.UserInfo, roomName string) (string, int) {
	roomName = strings.TrimSpace(roomName)
	if roomName == "" || !meetingRoomAccessible(userInfo, roomName) {
		return "", 0
	}
	lines, _, err := transcriptDomain.GetTranscriptLinesByRoom(ctx, roomName, maxRecapLines)
	if err != nil || len(lines) == 0 {
		return "", 0
	}
	nameByUID := resolveParticipantNames(ctx, lines)
	transcript, _ := formatTranscriptForRecap(lines, nameByUID)
	return transcript, len(lines)
}

// meetingRoomAccessible reports whether the user may read the transcript of a
// call room, using only their in-memory access graph (no extra queries). This
// is the same permission boundary the rest of AI enforces: a member of the
// channel/group, or a participant of the DM.
func meetingRoomAccessible(userInfo *userModels.UserInfo, roomName string) bool {
	switch {
	case strings.Contains(roomName, " "):
		// DM room: the requester must be one of the two participants (the
		// room name is the space-joined sorted pair of user UUIDs).
		me := userInfo.UserDgraphInfo.Uuid
		mePg := userInfo.UserPostgresInfo.Id.String()
		for _, p := range strings.Fields(roomName) {
			if p != "" && (p == me || p == mePg) {
				return true
			}
		}
		return false
	case !strings.Contains(roomName, "-") && len(roomName) == 32:
		return containsStr(accessibleGroupingIDs(userInfo), roomName)
	default:
		channels, _ := getAccessibleResourceUUIDs(userInfo)
		return containsStr(channels, roomName)
	}
}

// decodeTaskItems reads the model's answer in any of the shapes it comes in.
// The prompt asks for {"tasks": [...]}, the one shape every provider's JSON
// mode allows. JSON mode forbids a top-level array, and before the prompt
// asked for an object, small models answered a single action item with the
// bare item object, which an array-only parser dropped: every one-request
// message came back "No clear action items found". So it also accepts a bare
// array (providers without JSON mode) and a lone item. ok is false only when
// there is no JSON of a known shape at all. Pure.
func decodeTaskItems(out string) (items []rawProposedTask, ok bool) {
	s := strings.TrimSpace(out)
	s = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(s, "```json"), "```"), "```")
	s = strings.TrimSpace(s)
	if start, end := strings.Index(s, "{"), strings.LastIndex(s, "}"); start != -1 && end > start &&
		(strings.Index(s, "[") == -1 || start < strings.Index(s, "[")) {
		obj := s[start : end+1]
		var wrapped struct {
			Tasks *[]rawProposedTask `json:"tasks"`
		}
		if json.Unmarshal([]byte(obj), &wrapped) == nil && wrapped.Tasks != nil {
			return *wrapped.Tasks, true
		}
		var one rawProposedTask
		if json.Unmarshal([]byte(obj), &one) == nil && strings.TrimSpace(one.Title) != "" {
			return []rawProposedTask{one}, true
		}
	}
	if js := extractJSONArray(s); js != "" {
		if json.Unmarshal([]byte(js), &items) == nil {
			return items, true
		}
	}
	return nil, false
}

// extractionCalendar tells the model today's date and weekday and lists the
// next seven days. Given only "Today is 2026-09-29", a small model resolved
// "by Friday" to a Wednesday and "before Thursday" to the day before; with the
// days written out it looks the date up instead of counting. Pure.
func extractionCalendar(now time.Time) string {
	var b strings.Builder
	b.WriteString("Today is " + now.Format("Monday 2006-01-02") + " (UTC). The next seven days:")
	for i := 1; i <= 7; i++ {
		b.WriteString(" " + now.AddDate(0, 0, i).Format("Mon 2006-01-02") + ";")
	}
	b.WriteString(" A deadline \"by\" or \"before\" a weekday is that day in the coming week.")
	return b.String()
}

// isValidTaskJSON reports whether out holds task JSON of a known shape (an
// empty list is valid: it means "no action items"). Used as the retry-validity
// check. Pure.
func isValidTaskJSON(out string) bool {
	_, ok := decodeTaskItems(out)
	return ok
}

// parseProposedTasks decodes + sanitizes the model's answer. Pure + unit
// tested. Drops items with no title and caps the count.
func parseProposedTasks(rawOut string) []rawProposedTask {
	items, ok := decodeTaskItems(rawOut)
	if !ok {
		return nil
	}
	out := make([]rawProposedTask, 0, len(items))
	for _, it := range items {
		title := sanitizeLabel(it.Title)
		if title == "" {
			continue
		}
		if len([]rune(title)) > extractMaxTitleLen {
			title = string([]rune(title)[:extractMaxTitleLen])
		}
		details := sanitizeLabel(it.Details)
		if len([]rune(details)) > extractMaxDescLen {
			details = string([]rune(details)[:extractMaxDescLen])
		}
		out = append(out, rawProposedTask{
			Title:    title,
			Assignee: sanitizeLabel(it.Assignee),
			Due:      strings.TrimSpace(it.Due),
			Priority: strings.TrimSpace(it.Priority),
			Details:  details,
		})
		if len(out) >= extractMaxTasks {
			break
		}
	}
	return out
}

// normalizeDueDate parses a YYYY-MM-DD (or RFC3339) date and returns it as an
// RFC3339 timestamp at 17:00 UTC (end-of-workday), or "" when unparseable.
// Pure.
func normalizeDueDate(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC().Format(time.RFC3339)
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return time.Date(t.Year(), t.Month(), t.Day(), 17, 0, 0, 0, time.UTC).Format(time.RFC3339)
	}
	return ""
}

// CreateTasksFromProposals creates the chosen tasks in the target project AS
// the user, through the normal task path (project-admin enforced once). Each
// task's assignee is resolved (uuid or name) and its due date set when
// present. Returns the count created and any per-task failures (best-effort;
// one bad task never aborts the batch). Generic: works for any source.
func CreateTasksFromProposals(ctx context.Context, userInfo *userModels.UserInfo, projectUUID string, tasks []ProposedTask) (created int, failed int, err error) {
	if len(tasks) == 0 {
		return 0, 0, fmt.Errorf("no tasks to create")
	}
	projectParsed, perr := uuid.Parse(strings.TrimSpace(projectUUID))
	if perr != nil {
		return 0, 0, fmt.Errorf("invalid project id")
	}
	dgraphProject, derr := projectDomain.GetBasicDgraphProjectInfo(ctx, projectUUID, userInfo.UserDgraphInfo.Uid)
	if derr != nil || dgraphProject == nil || dgraphProject.Uuid == "" {
		return 0, 0, fmt.Errorf("project not found or you don't have access")
	}
	if dgraphProject.IsProjectAdmin == 0 {
		return 0, 0, fmt.Errorf("you must be an admin of project '%s' to create tasks", dgraphProject.Name)
	}

	for _, t := range tasks {
		title := strings.TrimSpace(t.Title)
		if title == "" {
			failed++
			continue
		}
		// Prefer the resolved uuid; fall back to the name so "assign to John"
		// still works. Empty → unassigned.
		ref := strings.TrimSpace(t.AssigneeUUID)
		if ref == "" {
			ref = strings.TrimSpace(t.AssigneeName)
		}
		assigneeDgraph, rerr := resolveAssigneeRef(ctx, userInfo.UserDgraphInfo.Uuid, ref)
		if rerr != nil {
			assigneeDgraph = nil // unresolved assignee → create unassigned rather than fail
		}

		input := taskAdapter.CreateOrUpdateTaskInput{
			TaskName:        title,
			TaskDescription: strings.TrimSpace(t.Description),
			Priority:        normalizeTaskPriority(t.Priority),
			Status:          "todo",
		}
		taskUUID, cerr := taskBusiness.CreateTask(ctx, projectParsed, userInfo, dgraphProject, assigneeDgraph, input, nil)
		if cerr != nil {
			failed++
			continue
		}
		created++

		// Set the due date when one was extracted (separate call: CreateTask
		// does not persist a due date).
		if due := strings.TrimSpace(t.Due); due != "" {
			if dt, terr := time.Parse(time.RFC3339, due); terr == nil {
				if _, dgTask, lerr := loadTaskForUpdateAsAdmin(ctx, taskUUID.String(), userInfo.UserDgraphInfo.Uuid); lerr == nil {
					_ = taskBusiness.UpdateTaskDueDateByTaskUUID(ctx, taskUUID, &dt, dgTask, &userInfo.UserDgraphInfo)
				}
			}
		}
	}
	return created, failed, nil
}
