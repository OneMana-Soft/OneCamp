package business

import (
	"testing"

	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
	"github.com/google/uuid"
)

// newCompiled builds a compiledWorkflow via the production compile() path so
// the test exercises real keyword-regex compilation.
func newCompiled(t *testing.T, keywords []string, matchType string, actions string) *compiledWorkflow {
	t.Helper()
	kwJSON := "[]"
	if len(keywords) > 0 {
		kwJSON = `["` + join(keywords, `","`) + `"]`
	}
	cw, err := compile(&workflowModel.Workflow{
		Id:        uuid.New(),
		CreatedBy: uuid.New(),
		MatchType: matchType,
		Keywords:  kwJSON,
		Actions:   actions,
	})
	if err != nil {
		t.Fatalf("compile failed: %v", err)
	}
	return cw
}

func join(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}

const replyAction = `[{"type":"reply","text":"hi"}]`

func TestMatch_NoKeywords_AlwaysMatches(t *testing.T) {
	cw := newCompiled(t, nil, workflowModel.MatchAny, replyAction)
	if !cw.matches("literally anything") {
		t.Fatal("empty keyword set should match every message")
	}
}

func TestMatch_Any(t *testing.T) {
	cw := newCompiled(t, []string{"bug", "error"}, workflowModel.MatchAny, replyAction)
	cases := map[string]bool{
		"there is a bug here":   true,
		"got an ERROR in prod":  true, // case-insensitive
		"all good, shipping it": false,
		"debugger":              false, // word boundary: 'bug' inside 'debugger' must NOT match
	}
	for text, want := range cases {
		if got := cw.matches(lower(text)); got != want {
			t.Errorf("any-match %q = %v, want %v", text, got, want)
		}
	}
}

func TestMatch_All(t *testing.T) {
	cw := newCompiled(t, []string{"deploy", "prod"}, workflowModel.MatchAll, replyAction)
	cases := map[string]bool{
		"deploy to prod now": true,
		"deploy to staging":  false, // missing 'prod'
		"prod incident":      false, // missing 'deploy'
	}
	for text, want := range cases {
		if got := cw.matches(lower(text)); got != want {
			t.Errorf("all-match %q = %v, want %v", text, got, want)
		}
	}
}

func TestCompile_RejectsNoActions(t *testing.T) {
	_, err := compile(&workflowModel.Workflow{
		Id:        uuid.New(),
		CreatedBy: uuid.New(),
		MatchType: workflowModel.MatchAny,
		Keywords:  "[]",
		Actions:   "[]",
	})
	if err == nil {
		t.Fatal("expected error for workflow with no actions")
	}
}

func TestCompile_InvalidMatchTypeDefaultsToAny(t *testing.T) {
	cw := newCompiled(t, []string{"x"}, "garbage", replyAction)
	if cw.matchType != workflowModel.MatchAny {
		t.Fatalf("invalid match type should default to any, got %q", cw.matchType)
	}
}

// lower mirrors the engine's pre-lowercasing of message text before matches().
func lower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

func TestApplyVars(t *testing.T) {
	vars := map[string]string{"user": "alice", "channel": "general"}
	cases := map[string]string{
		"Welcome {user} to #{channel}!": "Welcome alice to #general!",
		"no tokens here":                "no tokens here",
		"{user} {user}":                 "alice alice",
		"{unknown} stays":               "{unknown} stays",
		"":                              "",
	}
	for in, want := range cases {
		if got := applyVars(in, vars); got != want {
			t.Errorf("applyVars(%q) = %q, want %q", in, got, want)
		}
	}
	// Nil/empty vars is a no-op.
	if got := applyVars("{user}", nil); got != "{user}" {
		t.Errorf("applyVars with nil vars should be a no-op, got %q", got)
	}
}

func TestIsEngineTrigger(t *testing.T) {
	if !IsEngineTrigger(workflowModel.TriggerMessagePosted) {
		t.Error("message_posted must be an engine trigger")
	}
	if !IsEngineTrigger(workflowModel.TriggerUserJoinedChannel) {
		t.Error("user_joined_channel must be an engine trigger")
	}
	// Both have handlers; meeting_ended was offered by the editor and refused on save.
	for _, trig := range []string{workflowModel.TriggerMeetingEnded, workflowModel.TriggerTaskStatusChanged} {
		if !IsEngineTrigger(trig) {
			t.Errorf("%s must be an engine trigger", trig)
		}
	}
	// A DB-valid but not-yet-wired trigger must be rejected by the engine gate.
	if IsEngineTrigger(workflowModel.TriggerTaskCreated) {
		t.Error("task_created is not wired yet and must not be an engine trigger")
	}
	if IsEngineTrigger("garbage") {
		t.Error("unknown trigger must not be an engine trigger")
	}
}

func TestValidate_RejectsUnwiredTrigger(t *testing.T) {
	in := &WorkflowInput{
		Name:        "x",
		TriggerType: workflowModel.TriggerTaskCreated,
		Actions:     []WorkflowAction{{Type: ActionReply, Text: "hi"}},
	}
	if _, err := validate(in); err == nil {
		t.Fatal("expected validate to reject a not-yet-wired trigger type")
	}
}

func TestValidate_CreateTaskNeedsNameForNonMessageTrigger(t *testing.T) {
	in := &WorkflowInput{
		Name:        "welcome-task",
		TriggerType: workflowModel.TriggerUserJoinedChannel,
		Actions:     []WorkflowAction{{Type: ActionCreateTask, ProjectID: uuid.New().String()}},
	}
	if _, err := validate(in); err == nil {
		t.Fatal("create_task on a non-message trigger must require an explicit task name")
	}
}

func TestValidate_MessageTriggerDefaultsOK(t *testing.T) {
	in := &WorkflowInput{
		Name:    "triage",
		Actions: []WorkflowAction{{Type: ActionReply, Text: "thanks"}},
	}
	v, err := validate(in)
	if err != nil {
		t.Fatalf("valid message workflow should pass: %v", err)
	}
	if v.triggerType != workflowModel.TriggerMessagePosted {
		t.Errorf("empty trigger should default to message_posted, got %q", v.triggerType)
	}
	if v.triggerConfigJSON != "{}" {
		t.Errorf("empty trigger config should default to {}, got %q", v.triggerConfigJSON)
	}
}

func TestValidate_EphemeralReplyRequiresText(t *testing.T) {
	in := &WorkflowInput{
		Name:        "welcome",
		TriggerType: workflowModel.TriggerUserJoinedChannel,
		Actions:     []WorkflowAction{{Type: ActionReplyEphemeral, Text: "  "}},
	}
	if _, err := validate(in); err == nil {
		t.Fatal("reply_ephemeral with blank text must be rejected")
	}
}

func TestValidate_EphemeralWelcomeOK(t *testing.T) {
	in := &WorkflowInput{
		Name:        "welcome",
		TriggerType: workflowModel.TriggerUserJoinedChannel,
		Actions:     []WorkflowAction{{Type: ActionReplyEphemeral, Text: "Welcome {user}!"}},
	}
	if _, err := validate(in); err != nil {
		t.Fatalf("valid ephemeral welcome should pass: %v", err)
	}
}

func TestCanManage(t *testing.T) {
	owner := uuid.New()
	other := uuid.New()
	wf := &workflowModel.Workflow{Id: uuid.New(), CreatedBy: owner}

	// Creator can manage their own.
	if !canManage(Actor{UserID: owner, IsAdmin: false}, wf) {
		t.Error("creator should manage their own workflow")
	}
	// A different non-admin cannot.
	if canManage(Actor{UserID: other, IsAdmin: false}, wf) {
		t.Error("a non-admin non-owner must NOT manage another's workflow")
	}
	// An admin can manage anyone's.
	if !canManage(Actor{UserID: other, IsAdmin: true}, wf) {
		t.Error("admin should manage any workflow")
	}
	// Nil workflow is never manageable.
	if canManage(Actor{UserID: owner, IsAdmin: true}, nil) {
		t.Error("nil workflow must not be manageable")
	}
}

func TestValidate_DeleteMessageRequiresMessageTrigger(t *testing.T) {
	// delete_message on a non-message trigger must be rejected.
	in := &WorkflowInput{
		Name:        "mod",
		TriggerType: workflowModel.TriggerUserJoinedChannel,
		Actions:     []WorkflowAction{{Type: ActionDeleteMessage}},
	}
	if _, err := validate(in); err == nil {
		t.Fatal("delete_message must require the message_posted trigger")
	}
	// On message_posted it's allowed.
	ok := &WorkflowInput{
		Name:    "mod2",
		Actions: []WorkflowAction{{Type: ActionDeleteMessage}},
	}
	if _, err := validate(ok); err != nil {
		t.Fatalf("delete_message on message_posted should pass: %v", err)
	}
}

func TestValidate_FlagToChannelNeedsValidTarget(t *testing.T) {
	// Missing target.
	in := &WorkflowInput{
		Name:    "flag",
		Actions: []WorkflowAction{{Type: ActionFlagToChannel}},
	}
	if _, err := validate(in); err == nil {
		t.Fatal("flag_to_channel must require a target channel")
	}
	// Invalid target uuid.
	bad := &WorkflowInput{
		Name:    "flag",
		Actions: []WorkflowAction{{Type: ActionFlagToChannel, TargetChannelID: "not-a-uuid"}},
	}
	if _, err := validate(bad); err == nil {
		t.Fatal("flag_to_channel must reject an invalid target channel")
	}
	// Valid.
	ok := &WorkflowInput{
		Name:    "flag",
		Actions: []WorkflowAction{{Type: ActionFlagToChannel, TargetChannelID: uuid.New().String()}},
	}
	if _, err := validate(ok); err != nil {
		t.Fatalf("valid flag_to_channel should pass: %v", err)
	}
}

func TestValidate_WarnUserRequiresText(t *testing.T) {
	in := &WorkflowInput{
		Name:    "warn",
		Actions: []WorkflowAction{{Type: ActionWarnUser, Text: ""}},
	}
	if _, err := validate(in); err == nil {
		t.Fatal("warn_user must require text")
	}
}
