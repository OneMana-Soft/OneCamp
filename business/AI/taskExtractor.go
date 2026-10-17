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
	"unicode/utf8"

	taskAdapter "github.com/akashc777/OneCamp/adapter/Task"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	transcriptDomain "github.com/akashc777/OneCamp/domain/LiveKit"
	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

const (
	extractMaxTasks       = 20
	extractMaxTitleLen    = 160
	extractMaxDescLen     = 400
	extractMaxNameLen     = 120
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

// rawProposedTask is the model's per-item JSON shape. Block is the line a
// doc's item came from, as the model wrote it (3, "3" or "[3]"; docTasks.go).
type rawProposedTask struct {
	Title    string          `json:"title"`
	Assignee string          `json:"assignee"`
	Due      string          `json:"due"`
	Priority string          `json:"priority"`
	Details  string          `json:"details"`
	Block    json.RawMessage `json:"block,omitempty"`
}

// ExtractActionItems proposes the action items in a conversation, a call or
// pasted text (or a doc), read-only. sourceType is one of channel | dm |
// group | meeting | text | doc; for text, rawText carries the content, and
// for meeting, sourceID is the call room name (its transcript is used).
func ExtractActionItems(ctx context.Context, userInfo *userModels.UserInfo, sourceType, sourceID, rawText string) (*ExtractTasksResult, error) {
	src := talkSource(userInfo, sourceType, sourceID, rawText)
	if strings.EqualFold(strings.TrimSpace(sourceType), "doc") {
		src = docTaskSource(userInfo, sourceID, nil)
	}
	x, err := extractTasks(ctx, userInfo, src)
	if err != nil {
		return nil, err
	}
	resp := &ExtractTasksResult{Enabled: x.enabled, Note: x.note, ScannedCount: x.scanned, Tasks: make([]ProposedTask, 0, len(x.tasks))}
	for _, t := range x.tasks {
		resp.Tasks = append(resp.Tasks, t.ProposedTask)
	}
	return resp, nil
}

// talkSource is a conversation, a call's transcript or pasted text, read as
// the person may read it, at most extractMaxSourceChars of it.
func talkSource(userInfo *userModels.UserInfo, sourceType, sourceID, rawText string) taskSource {
	return taskSource{
		read: func(ctx context.Context) (string, int, error) {
			text, scanned := gatherExtractionSource(ctx, userInfo, sourceType, sourceID, rawText)
			return helpers.TruncateRunes(strings.TrimSpace(text), extractMaxSourceChars), scanned, nil
		},
		prompt:    taskExtractionPrompt,
		noun:      "Text",
		maxTokens: 1200,
		tooShort:  "There isn't enough here to pull action items from.",
		noneFound: "No clear action items found.",
	}
}

// ---------------------------------------------------------------------------
// One flow for every source
// ---------------------------------------------------------------------------

// taskSource is where action items are read from (a conversation, a call's
// transcript, pasted text, a doc) and how the model is asked about it: all
// that differs between the sources. extractTasks is the rest.
type taskSource struct {
	// read is the text the model reads, already bounded, and how many
	// messages or lines it came from. It is called only when AI is on, and
	// reads only what the person may: an error ends the extraction.
	read func(ctx context.Context) (text string, scanned int, err error)
	// prompt is the model's instructions; noun names the text in the
	// message ("Text", "Document").
	prompt, noun string
	// maxTokens bounds the model's answer.
	maxTokens int
	// tooShort and noneFound are the notes for too little to read and for
	// nothing found.
	tooShort, noneFound string
}

// extractedTask is a proposed task, with the model's item it was made from
// (a doc reads the line it names).
type extractedTask struct {
	ProposedTask
	raw rawProposedTask
}

// extraction is what extractTasks found: enabled is false when AI is off (the
// surface is hidden); note says why there are no tasks.
type extraction struct {
	enabled bool
	tasks   []extractedTask
	note    string
	scanned int
}

// extractionOn reports whether AI is on, so the surface shows: a seam for
// tests.
var extractionOn = func() bool {
	svc := ai.GetService()
	return svc != nil && svc.IsEnabled()
}

// extractionModel is the person's model, asked for only once there is enough
// to read: it spends one of their AI requests. nil when AI is off. A seam for
// tests.
var extractionModel = func(ctx context.Context, userUUID string) (ai.LLMProvider, *ai.CircuitBreaker, error) {
	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return nil, nil, nil
	}
	if err := svc.Resiliency.CheckRateLimit(ctx, userUUID); err != nil {
		return nil, nil, err
	}
	llm, cb := svc.ResolveUserModel(ctx, userUUID)
	return llm, cb, nil
}

// chatExtraction asks the model for the items as JSON: a seam for tests.
var chatExtraction = ai.ChatJSONWithRetry

// extractionNow is the time the calendar is written from: a seam for tests.
var extractionNow = time.Now

// assigneeOf resolves an owner the text names to a member: a seam for tests.
var assigneeOf = resolveAssigneeRef

// extractTasks finds the action items in a source and proposes them as tasks,
// read-only: the person's model, its rate limit and breaker, the calendar,
// the JSON answer retried until it reads, and each item made a proposed task
// (proposedTaskFrom), at most extractMaxTasks. A model that fails gives a
// note, never an error.
func extractTasks(ctx context.Context, u *userModels.UserInfo, src taskSource) (extraction, error) {
	if !extractionOn() {
		return extraction{}, nil
	}
	text, scanned, err := src.read(ctx)
	if err != nil {
		return extraction{}, err
	}
	x := extraction{enabled: true, scanned: scanned}
	if utf8.RuneCountInString(text) < extractMinSourceChars {
		x.note = src.tooShort
		return x, nil
	}
	asker := u.UserDgraphInfo.Uuid
	llm, cb, err := extractionModel(ctx, asker)
	if err != nil {
		return extraction{}, err
	}
	if llm == nil || cb == nil {
		return extraction{}, nil // AI went off: the surface is hidden
	}
	if err := cb.Allow(); err != nil {
		return extraction{}, err
	}
	user := extractionCalendar(extractionNow().UTC()) + "\n\n" + src.noun + ":\n" + text
	out, err := chatExtraction(ctx, llm, cb, src.prompt, user, ai.ChatOptions{Temperature: 0.2, MaxTokens: src.maxTokens}, isValidTaskJSON)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/AI/extractTasks model call failed: %v", err)
		x.note = "Couldn't read the action items just now. Try again in a moment."
		return x, nil
	}
	items, _ := decodeTaskItems(out)
	for _, it := range items {
		pt, ok := proposedTaskFrom(ctx, asker, it)
		if !ok {
			continue
		}
		x.tasks = append(x.tasks, extractedTask{ProposedTask: pt, raw: it})
		if len(x.tasks) >= extractMaxTasks {
			break
		}
	}
	if len(x.tasks) == 0 {
		x.note = src.noneFound
	}
	return x, nil
}

// proposedTaskFrom is one item of the model's answer as a proposed task: its
// title, details and owner's name cleaned and bounded (cleanLabel), its
// priority and due date normalised, and its owner resolved to a member where
// one matches; a name that matches nobody is kept as written, for the person
// to see. ok is false for an item with no title.
func proposedTaskFrom(ctx context.Context, askerUUID string, rt rawProposedTask) (ProposedTask, bool) {
	title := cleanLabel(rt.Title, extractMaxTitleLen)
	if title == "" {
		return ProposedTask{}, false
	}
	pt := ProposedTask{
		Title:       title,
		Description: cleanLabel(rt.Details, extractMaxDescLen),
		Priority:    normalizeTaskPriority(rt.Priority),
		Due:         normalizeDueDate(rt.Due),
	}
	if name := cleanLabel(rt.Assignee, extractMaxNameLen); name != "" {
		pt.AssigneeName = name
		if du, err := assigneeOf(ctx, askerUUID, name, false); err == nil && du != nil {
			pt.AssigneeUUID = du.Uuid
			if du.UserName != "" {
				pt.AssigneeName = du.UserName
			}
		}
	}
	return pt, true
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
	return "Today is " + now.Format("Monday 2006-01-02") + " (UTC). " + UpcomingDays(now, 7) +
		" A deadline \"by\" or \"before\" a weekday is that day in the coming week."
}

// UpcomingDays writes out the next n days with their weekdays, so a model looks
// a date up instead of counting. Every small model benchmarked for OneCamp
// Cloud (llama3.2:3b, qwen3:4b-instruct, 3 Oct 2026) turned "next Friday",
// asked on a Friday, into a Wednesday when given only today's date.
//
// n is the caller's: task extraction lists 7 so "by Friday" can only mean the
// coming one; an agent lists 14 so "next Friday" is there too. Pure.
func UpcomingDays(now time.Time, n int) string {
	var b strings.Builder
	b.WriteString("The coming days:")
	for i := 1; i <= n; i++ {
		b.WriteString(" " + now.AddDate(0, 0, i).Format("Mon 2006-01-02") + ";")
	}
	return b.String()
}

// isValidTaskJSON reports whether out holds task JSON of a known shape (an
// empty list is valid: it means "no action items"). Used as the retry-validity
// check. Pure.
func isValidTaskJSON(out string) bool {
	_, ok := decodeTaskItems(out)
	return ok
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
		if _, cerr := createProposedTask(ctx, userInfo, projectParsed, dgraphProject, t, strings.TrimSpace(t.Description)); cerr != nil {
			failed++
			continue
		}
		created++
	}
	return created, failed, nil
}

// createProposedTask makes one proposed task in a project the person is an
// admin of, AS them, through the normal task path: its assignee resolved (uuid
// or name; unresolved leaves it unassigned), its due date set when there is
// one. description is what the task's description holds.
func createProposedTask(ctx context.Context, userInfo *userModels.UserInfo, projectParsed uuid.UUID, dgraphProject *dgraphStruct.DgraphProject, t ProposedTask, description string) (uuid.UUID, error) {
	title := strings.TrimSpace(t.Title)
	if title == "" {
		return uuid.Nil, fmt.Errorf("a task needs a title")
	}
	// Prefer the resolved uuid; fall back to the name so "assign to John"
	// still works. Empty is unassigned.
	ref := strings.TrimSpace(t.AssigneeUUID)
	if ref == "" {
		ref = strings.TrimSpace(t.AssigneeName)
	}
	assigneeDgraph, rerr := resolveAssigneeRef(ctx, userInfo.UserDgraphInfo.Uuid, ref, false)
	if rerr != nil {
		assigneeDgraph = nil // unresolved assignee: created unassigned rather than failed
	}
	input := taskAdapter.CreateOrUpdateTaskInput{
		TaskName:        title,
		TaskDescription: description,
		Priority:        normalizeTaskPriority(t.Priority),
		Status:          "todo",
	}
	taskUUID, err := taskBusiness.CreateTask(ctx, projectParsed, userInfo, dgraphProject, assigneeDgraph, input, nil)
	if err != nil {
		return uuid.Nil, err
	}
	// The due date is a second call: CreateTask does not keep one.
	if due := strings.TrimSpace(t.Due); due != "" {
		if dt, terr := time.Parse(time.RFC3339, due); terr == nil {
			if _, dgTask, lerr := loadTaskForUpdateAsAdmin(ctx, taskUUID.String(), userInfo.UserDgraphInfo.Uuid); lerr == nil {
				_ = taskBusiness.UpdateTaskDueDateByTaskUUID(ctx, taskUUID, &dt, dgTask, &userInfo.UserDgraphInfo)
			}
		}
	}
	return taskUUID, nil
}
