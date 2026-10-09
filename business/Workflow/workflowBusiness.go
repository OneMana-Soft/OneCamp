// Package business (Workflow) is the Workflow Builder engine — event-triggered
// "when X happens, do Y" automation.
//
// Design: a workflow is a saved rule, NOT a new runtime. Triggers ride the
// existing workspace event bus (webhook.RegisterEventListener → "post.created"),
// and actions reuse the proven AI executors (send_message, create_task), which
// already re-check the acting user's permissions. So a workflow can never do
// something its owner couldn't do by hand, and we add no parallel
// delivery/queue machinery.
//
// Loop safety: actions run under helpers.WithWorkflowGenerated(ctx); the event
// dispatcher skips in-process listeners for such writes, so a "reply" action
// can't trigger another workflow forever.
package business

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	botpost "github.com/akashc777/OneCamp/business/BotPost"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// Action types supported by a workflow.
const (
	ActionReply          = "reply"           // public post back into the triggering channel
	ActionReplyEphemeral = "reply_ephemeral" // private card visible only to the target user
	ActionCreateTask     = "create_task"     // create a task in a project
	ActionDeleteMessage  = "delete_message"  // moderation: remove the triggering message
	ActionWarnUser       = "warn_user"       // moderation: private ephemeral warning to the author
	ActionFlagToChannel  = "flag_to_channel" // moderation: copy + link the message to a review channel
)

// MaxActionsPerWorkflow bounds how much one workflow can do per trigger, so a
// misconfigured rule can't fan out unbounded work.
const MaxActionsPerWorkflow = 5

// WorkflowAction is one step in a workflow's action list (parsed from JSON).
type WorkflowAction struct {
	Type string `json:"type"`
	// reply / reply_ephemeral / warn_user
	Text string `json:"text,omitempty"`
	// create_task
	ProjectID   string `json:"project_id,omitempty"`
	TaskName    string `json:"task_name,omitempty"` // optional; defaults to the message text
	Priority    string `json:"priority,omitempty"`
	Description string `json:"description,omitempty"`
	// flag_to_channel: the review channel the flagged message is routed to.
	TargetChannelID string `json:"target_channel_id,omitempty"`
}

// compiledWorkflow is the cached, ready-to-match form of a workflow row.
type compiledWorkflow struct {
	id          uuid.UUID
	name        string
	createdBy   string
	triggerType string
	botName     string // per-workflow bot label override; "" → fall back to name
	channelID   string // "" = any channel
	// task_status_changed only (see taskStatusTrigger.go): which moves count.
	taskMove  taskStatusBusiness.MoveFilter
	matchType string
	keywords  []string
	regexes   []*regexp.Regexp
	actions   []WorkflowAction
}

// triggerEvent is the normalized context an event delivers to a workflow's
// actions. Not every field is set for every trigger; actions use what's
// relevant (e.g. reply uses ChannelID + the substitution vars; reply_ephemeral
// also needs targetUserUUID).
type triggerEvent struct {
	channelID      string            // channel the action should act in ("" if none)
	projectID      string            // project the event came from (task_status_changed)
	text           string            // triggering message text (message_posted)
	postUUID       string            // triggering post id (message_posted) for moderation
	targetUserUUID string            // the user an ephemeral reply targets
	vars           map[string]string // template substitutions for reply text
}

// registry caches active workflows per trigger type for the hot match path,
// refreshed on every write. Keyed by trigger_type so an event only fans out to
// the workflows that asked for it.
var (
	regMu     sync.RWMutex
	byTrigger map[string][]*compiledWorkflow
	started   bool
)

// Start wires the engine into the event bus and loads the initial rule set.
// Idempotent; call once at startup after the DB is ready.
func Start(ctx context.Context) {
	regMu.Lock()
	if started {
		regMu.Unlock()
		return
	}
	started = true
	regMu.Unlock()

	if err := reload(ctx); err != nil {
		helpers.LogErrorWithContext(ctx, "Workflow/Start initial load err: %+v", err)
	}
	// Every write path hot-reloads the compiled rules IN THIS PROCESS. Another
	// replica learns of the change from the store, here; see the reconciler's
	// own header for why that is the only reliable source.
	if err := helpers.StartConfigReconciler(ctx, helpers.ConfigReconciler{
		Name:     "workflows",
		Interval: helpers.DefaultConfigReconcileInterval,
		Fingerprint: func(ctx context.Context) (string, error) {
			return postgresInit.RowsFingerprint(ctx, "workflows")
		},
		Apply: reload,
	}); err != nil {
		helpers.LogErrorWithContext(ctx, "Workflow/Start reconciler: %v", err)
	}
	webhookBusiness.RegisterEventListener(handleEvent)
	helpers.MessageLogs.InfoLog.Println("Workflow engine started")
}

// engineTriggers are the trigger kinds the runtime engine actively evaluates.
// (The DB CHECK allows a forward-compatible superset; only these are wired to
// live events today.) Adding a trigger here + a case in handleEvent activates
// it.
var engineTriggers = []string{
	workflowModel.TriggerMessagePosted,
	workflowModel.TriggerUserJoinedChannel,
	// Handled below since meeting_ended shipped, but never listed here, so the
	// editor offered it and saving one failed ("not available yet").
	workflowModel.TriggerMeetingEnded,
	workflowModel.TriggerTaskStatusChanged,
}

// IsEngineTrigger reports whether a trigger type is actively evaluated by the
// runtime today. The validator uses this to reject workflows whose trigger
// would silently never fire (the DB CHECK is a forward-compatible superset).
func IsEngineTrigger(t string) bool {
	for _, e := range engineTriggers {
		if e == t {
			return true
		}
	}
	return false
}

// reload rebuilds the in-memory rule set from the DB. Called at startup and
// after any create/update/delete/toggle so the engine reflects changes without
// a restart. Loads every engine-supported trigger, grouped by trigger type.
func reload(ctx context.Context) error {
	next := make(map[string][]*compiledWorkflow, len(engineTriggers))
	for _, trig := range engineTriggers {
		rows, err := workflowModel.ListActiveByTrigger(ctx, trig)
		if err != nil {
			return err
		}
		compiled := make([]*compiledWorkflow, 0, len(rows))
		for _, w := range rows {
			cw, cerr := compile(w)
			if cerr != nil {
				helpers.LogErrorWithContext(ctx, "Workflow/reload skipping invalid workflow %s: %v", w.Id, cerr)
				continue
			}
			compiled = append(compiled, cw)
		}
		next[trig] = compiled
	}
	regMu.Lock()
	byTrigger = next
	regMu.Unlock()
	return nil
}

// candidatesFor returns the cached workflows for a trigger type (read-locked).
func candidatesFor(trigger string) []*compiledWorkflow {
	regMu.RLock()
	defer regMu.RUnlock()
	return byTrigger[trigger]
}

// compile parses and pre-compiles a workflow row for matching.
func compile(w *workflowModel.Workflow) (*compiledWorkflow, error) {
	var keywords []string
	if strings.TrimSpace(w.Keywords) != "" {
		if err := json.Unmarshal([]byte(w.Keywords), &keywords); err != nil {
			return nil, fmt.Errorf("bad keywords json: %w", err)
		}
	}
	var actions []WorkflowAction
	if strings.TrimSpace(w.Actions) != "" {
		if err := json.Unmarshal([]byte(w.Actions), &actions); err != nil {
			return nil, fmt.Errorf("bad actions json: %w", err)
		}
	}
	if len(actions) == 0 {
		return nil, fmt.Errorf("workflow has no actions")
	}

	regexes := make([]*regexp.Regexp, 0, len(keywords))
	for _, kw := range keywords {
		kw = strings.TrimSpace(kw)
		if kw == "" {
			continue
		}
		// Word-boundary, case-insensitive — same convention as outgoing webhook
		// trigger words.
		re, err := regexp.Compile(`(?i)\b` + regexp.QuoteMeta(kw) + `\b`)
		if err == nil {
			regexes = append(regexes, re)
		}
	}

	channelID := ""
	if w.ChannelId != nil {
		channelID = w.ChannelId.String()
	}
	matchType := w.MatchType
	if matchType != workflowModel.MatchAll {
		matchType = workflowModel.MatchAny
	}

	botName := ""
	if w.BotName != nil {
		botName = strings.TrimSpace(*w.BotName)
	}

	var move taskStatusBusiness.MoveFilter
	if w.TriggerType == workflowModel.TriggerTaskStatusChanged && strings.TrimSpace(w.TriggerConfig) != "" {
		if err := json.Unmarshal([]byte(w.TriggerConfig), &move); err != nil {
			return nil, fmt.Errorf("bad trigger config json: %w", err)
		}
	}

	return &compiledWorkflow{
		taskMove:    move,
		id:          w.Id,
		name:        w.Name,
		createdBy:   w.CreatedBy.String(),
		triggerType: w.TriggerType,
		botName:     botName,
		channelID:   channelID,
		matchType:   matchType,
		keywords:    keywords,
		regexes:     regexes,
		actions:     actions,
	}, nil
}

// handleEvent is the event-bus listener. It maps a workspace event to the
// trigger kind it satisfies, then runs every matching workflow. Runs in its own
// goroutine (the dispatcher detaches + panic-guards it).
func handleEvent(ctx context.Context, eventType string, data map[string]interface{}) {
	switch eventType {
	case "post.created":
		handleMessagePosted(ctx, data)
	case "user.joined":
		handleUserJoinedChannel(ctx, data)
	case "meeting.ended":
		handleMeetingEnded(ctx, data)
	case "task.status_changed":
		handleTaskStatusChanged(ctx, data)
	}
}

// handleMessagePosted runs message_posted workflows whose channel scope +
// keyword filter match the posted message.
func handleMessagePosted(ctx context.Context, data map[string]interface{}) {
	channelID, _ := data["channel_id"].(string)
	text, _ := data["text"].(string)
	if strings.TrimSpace(text) == "" {
		return
	}
	lowerText := strings.ToLower(text)
	authorID, _ := data["author_id"].(string)
	postID, _ := data["post_id"].(string)

	for _, cw := range candidatesFor(workflowModel.TriggerMessagePosted) {
		if cw.channelID != "" && cw.channelID != channelID {
			continue
		}
		if !cw.matches(lowerText) {
			continue
		}
		runWorkflow(ctx, cw, triggerEvent{
			channelID:      channelID,
			text:           text,
			postUUID:       postID,
			targetUserUUID: authorID,
			vars: map[string]string{
				"text":    text,
				"channel": channelID,
			},
		})
	}
}

// handleUserJoinedChannel runs user_joined_channel workflows (welcome bots).
// The reply posts into the joined channel; {user}/{channel} substitutions are
// available in the reply text.
func handleUserJoinedChannel(ctx context.Context, data map[string]interface{}) {
	channelID, _ := data["channel_id"].(string)
	userID, _ := data["user_id"].(string)
	if strings.TrimSpace(channelID) == "" {
		return
	}

	// Resolve a friendly @mention/name for the joiner (best-effort).
	userName := ""
	if userID != "" {
		if u, err := userDomain.GetActiveDgraphUserInfoByUUID(ctx, userID); err == nil && u != nil {
			userName = u.UserName
		}
	}
	userMention := userName
	if userMention == "" {
		userMention = "there"
	}

	for _, cw := range candidatesFor(workflowModel.TriggerUserJoinedChannel) {
		// Channel scope: a welcome workflow may target one channel or any.
		if cw.channelID != "" && cw.channelID != channelID {
			continue
		}
		runWorkflow(ctx, cw, triggerEvent{
			channelID:      channelID,
			targetUserUUID: userID,
			vars: map[string]string{
				"user":    userMention,
				"channel": channelID,
			},
		})
	}
}

// matches applies the keyword/match-type filter to message text (already
// lower-cased). No keywords = always match.
func (cw *compiledWorkflow) matches(lowerText string) bool {
	if len(cw.regexes) == 0 {
		return true
	}
	if cw.matchType == workflowModel.MatchAll {
		for _, re := range cw.regexes {
			if !re.MatchString(lowerText) {
				return false
			}
		}
		return true
	}
	// any
	for _, re := range cw.regexes {
		if re.MatchString(lowerText) {
			return true
		}
	}
	return false
}

// runWorkflow executes a matched workflow's actions as its owner, then records
// the run outcome. Actions run under a workflow-generated context so they don't
// re-trigger workflows. An event from a channel or project its owner can't see
// is skipped before any of them (see access.go), and isn't a run.
func runWorkflow(ctx context.Context, cw *compiledWorkflow, ev triggerEvent) {
	if !access.allows(ctx, cw.createdBy, ev) {
		return
	}
	actionCtx := helpers.WithWorkflowGenerated(ctx)

	n := len(cw.actions)
	if n > MaxActionsPerWorkflow {
		n = MaxActionsPerWorkflow
	}

	var firstErr error
	for i := 0; i < n; i++ {
		act := cw.actions[i]
		if err := cw.runAction(actionCtx, act, ev); err != nil {
			helpers.LogErrorWithContext(ctx, "Workflow %s action %q failed: %v", cw.id, act.Type, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	errMsg := ""
	if firstErr != nil {
		errMsg = firstErr.Error()
	}
	if err := workflowModel.RecordRun(ctx, cw.id, time.Now(), errMsg); err != nil {
		helpers.LogErrorWithContext(ctx, "Workflow %s RecordRun err: %v", cw.id, err)
	}
}

// runAction dispatches a single action. Channel-targeted actions resolve their
// channel from the trigger event.
func (cw *compiledWorkflow) runAction(ctx context.Context, act WorkflowAction, ev triggerEvent) error {
	switch act.Type {
	case ActionReply:
		text := applyVars(strings.TrimSpace(act.Text), ev.vars)
		if text == "" {
			return fmt.Errorf("reply action has empty text")
		}
		if ev.channelID == "" {
			return fmt.Errorf("reply action: no channel in trigger context")
		}
		// Post AS the shared automation bot, badged with this workflow's label
		// (its bot_name override, falling back to the workflow name) so members
		// see a recognizable automation identity rather than the admin who
		// configured it. The workflow-generated context (set by runWorkflow)
		// suppresses in-process listener fan-out, so this reply can't re-trigger
		// workflows.
		label := cw.botName
		if label == "" {
			label = cw.name
		}
		chUUID, perr := uuid.Parse(ev.channelID)
		if perr != nil {
			return fmt.Errorf("reply action: invalid channel id %q: %w", ev.channelID, perr)
		}
		_, err := botpost.PostToChannel(ctx, chUUID, text, label)
		return err

	case ActionReplyEphemeral:
		text := applyVars(strings.TrimSpace(act.Text), ev.vars)
		if text == "" {
			return fmt.Errorf("reply_ephemeral action has empty text")
		}
		if ev.targetUserUUID == "" {
			// No identifiable recipient (e.g. an anonymous/system event) — an
			// ephemeral message has no audience, so skip rather than error.
			return fmt.Errorf("reply_ephemeral action: no target user in trigger context")
		}
		label := cw.botName
		if label == "" {
			label = cw.name
		}
		return botpost.PostEphemeralToUser(ctx, ev.targetUserUUID, text, label)

	case ActionCreateTask:
		if strings.TrimSpace(act.ProjectID) == "" {
			return fmt.Errorf("create_task action requires project_id")
		}
		taskName := strings.TrimSpace(act.TaskName)
		if taskName == "" {
			// Default the task name to the triggering message (trimmed).
			taskName = ev.text
		}
		taskName = applyVars(taskName, ev.vars)
		taskName = truncate(taskName, 200)
		if strings.TrimSpace(taskName) == "" {
			taskName = "Workflow task"
		}
		priority := act.Priority
		if priority == "" {
			priority = "medium"
		}
		exec, ok := ai.Executors["create_task"]
		if !ok {
			return fmt.Errorf("create_task executor not registered")
		}
		_, _, err := exec(ctx, ai.ProposedAction{
			ToolName: "create_task",
			Params: map[string]string{
				"task_name":    taskName,
				"project_uuid": act.ProjectID,
				"description":  applyVars(act.Description, ev.vars),
				"priority":     priority,
			},
		}, cw.createdBy)
		return err

	case ActionWarnUser:
		// Private moderation notice to the message author. Ephemeral: it never
		// touches channel history. No-op (not an error) when there's no author.
		text := applyVars(strings.TrimSpace(act.Text), ev.vars)
		if text == "" {
			return fmt.Errorf("warn_user action has empty text")
		}
		if ev.targetUserUUID == "" {
			return fmt.Errorf("warn_user action: no target user in trigger context")
		}
		label := cw.botName
		if label == "" {
			label = cw.name
		}
		return botpost.PostEphemeralToUser(ctx, ev.targetUserUUID, text, label)

	case ActionDeleteMessage:
		// Moderation: remove the triggering message. Only meaningful for the
		// message_posted trigger (the only one carrying a post to delete), and
		// gated on the workflow OWNER actually being a moderator/admin of that
		// channel at execution time — owning a workflow never grants deletion
		// rights a person doesn't already have.
		if ev.postUUID == "" {
			return fmt.Errorf("delete_message action: no message in trigger context")
		}
		if ev.channelID == "" {
			return fmt.Errorf("delete_message action: no channel in trigger context")
		}
		return cw.moderateDeleteMessage(ctx, ev.postUUID, ev.channelID)

	case ActionFlagToChannel:
		// Moderation: route a copy + link of the flagged message to a review
		// channel, posted as the bot. The workflow owner must have access to
		// the target review channel (checked in the helper).
		if strings.TrimSpace(act.TargetChannelID) == "" {
			return fmt.Errorf("flag_to_channel action requires target_channel_id")
		}
		return cw.moderateFlagToChannel(ctx, act.TargetChannelID, ev)

	default:
		return fmt.Errorf("unknown action type %q", act.Type)
	}
}

// applyVars substitutes {key} tokens in s with values from vars. Unknown tokens
// are left intact. Cheap, allocation-light for the common no-token case.
func applyVars(s string, vars map[string]string) string {
	if s == "" || len(vars) == 0 || !strings.Contains(s, "{") {
		return s
	}
	for k, v := range vars {
		s = strings.ReplaceAll(s, "{"+k+"}", v)
	}
	return s
}

// truncate clips s to at most max BYTES without splitting a multi-byte UTF-8
// rune (task names can contain emoji/unicode). Trailing partial runes are
// dropped rather than corrupted.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	// Back up to a rune boundary at/under max.
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// handleMeetingEnded runs meeting_ended workflows for the surface a finished
// call belonged to.
//
// WHY THIS TRIGGER. A meeting is the moment most likely to produce work: a
// decision worth recording, a task somebody agreed to, a status update the rest
// of the team is waiting on. It was the only workspace event with nothing able
// to react to it, so that work stayed in whoever's memory until they got round
// to it.
//
// It fires from room_finished, which LiveKit sends only after the transcription
// agent has left, so by this point the recap has run and a workflow acts on a
// meeting that has been summarised rather than one still being written down.
//
// No keyword matching. The other triggers filter on message text; a meeting has
// no text to match, and pretending otherwise would mean matching against a
// transcript, which is a different and much noisier feature. A meeting_ended
// workflow is scoped by channel and nothing else.
func handleMeetingEnded(ctx context.Context, data map[string]interface{}) {
	roomName, _ := data["room_name"].(string)

	// A channel call only. A workflow posts into a channel, and a DM has no
	// channel to post into; an instant meeting has no surface at all. The shared
	// classifier is what keeps this agreeing with the recap agent about what a
	// room is.
	channelID, _ := helpers.ClassifyRoom(roomName)
	if channelID == "" {
		return
	}

	for _, cw := range candidatesFor(workflowModel.TriggerMeetingEnded) {
		if cw.channelID != "" && cw.channelID != channelID {
			continue
		}
		runWorkflow(ctx, cw, triggerEvent{
			channelID: channelID,
			vars: map[string]string{
				"channel": channelID,
				"room":    roomName,
			},
		})
	}
}

// NotifyMeetingEnded is the entry point the LiveKit room_finished webhook calls.
//
// Exported so the controller has ONE line that is identical on both editions:
// the AI-free build has no recap agent, and this must fire there too. Detached
// and panic-guarded like every other listener, because a workflow must never be
// able to hold up or crash call teardown.
func NotifyMeetingEnded(roomName string) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				helpers.MessageLogs.ErrorLog.Printf("workflow: meeting_ended panic: %v", r)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		handleMeetingEnded(ctx, map[string]interface{}{"room_name": roomName})
	}()
}
