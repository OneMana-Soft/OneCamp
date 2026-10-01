package business

import (
	"context"
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

func TestActiveWorkState(t *testing.T) {
	cases := map[string]ActiveWorkState{
		model.TaskQueued:    ActiveWorkQueued,
		model.TaskRunning:   ActiveWorkWorking,
		model.TaskAwaiting:  ActiveWorkBlocked,
		model.TaskCancelled: ActiveWorkStopped,
		"something_else":    ActiveWorkQueued, // unknown open state reads as queued
		"":                  ActiveWorkQueued,
	}
	for in, want := range cases {
		if got := activeWorkState(in); got != want {
			t.Errorf("activeWorkState(%q) = %q, want %q", in, got, want)
		}
	}
}

// A stop that has been asked for but not yet settled must READ as stopping, on
// whatever state the job was in — otherwise the row keeps saying "working" after
// the person pressed Stop and the control looks broken.
func TestToActiveWorkItem_StopRequestedReadsAsStopping(t *testing.T) {
	for _, state := range []string{model.TaskQueued, model.TaskRunning, model.TaskAwaiting} {
		item := toActiveWorkItem(&model.AgentActiveTask{State: state, StopRequested: true})
		if item.State != ActiveWorkStopping {
			t.Errorf("state %q with a pending stop = %q, want %q", state, item.State, ActiveWorkStopping)
		}
	}
	// Without a request the state is untouched.
	if item := toActiveWorkItem(&model.AgentActiveTask{State: model.TaskRunning}); item.State != ActiveWorkWorking {
		t.Errorf("running job without a stop = %q, want %q", item.State, ActiveWorkWorking)
	}
}

// Who may stop an agent's work. The rule is deliberately not "can see it":
// a job can post into a channel or DM the viewer has no access to.
func TestCanStopAgentWork(t *testing.T) {
	owner := uuid.New()
	asker := uuid.New()
	runsAs := uuid.New()
	stranger := uuid.New()
	task := &model.AgentTask{TriggeredBy: &asker, RunAsUserId: &runsAs}

	allowed := map[string]Actor{
		"admin":         {UserID: stranger, IsAdmin: true},
		"agent owner":   {UserID: owner},
		"who asked":     {UserID: asker},
		"who it run as": {UserID: runsAs},
	}
	for name, actor := range allowed {
		if !canStopAgentWork(actor, taskPrincipals(owner, task)) {
			t.Errorf("%s should be allowed to stop the work", name)
		}
	}

	if canStopAgentWork(Actor{UserID: stranger}, taskPrincipals(owner, task)) {
		t.Error("an unrelated member must not be able to stop someone else's work")
	}
	if canStopAgentWork(Actor{}, taskPrincipals(owner, task)) {
		t.Error("an anonymous actor must never be able to stop work")
	}
	if canStopAgentWork(Actor{UserID: owner}, taskPrincipals(owner, nil)) {
		t.Error("a missing job must never authorize a stop")
	}
	// A job with no people attached (schedule/event) is still the owner's.
	bare := &model.AgentTask{}
	if !canStopAgentWork(Actor{UserID: owner}, taskPrincipals(owner, bare)) {
		t.Error("the agent owner should be able to stop an unattributed job")
	}
	if canStopAgentWork(Actor{UserID: asker}, taskPrincipals(owner, bare)) {
		t.Error("an unrelated member must not stop an unattributed job")
	}

	// The SAME rule must hold for a joined active-work row, which is the shape the
	// feeds carry — a second, drifting copy of the policy is the risk here.
	active := &model.AgentActiveTask{AgentCreatedBy: owner, TriggeredBy: &asker, RunAsUserId: &runsAs}
	for name, actor := range allowed {
		if !canStopAgentWork(actor, activeTaskPrincipals(active)) {
			t.Errorf("%s should be allowed to stop the work (active row)", name)
		}
	}
	if canStopAgentWork(Actor{UserID: stranger}, activeTaskPrincipals(active)) {
		t.Error("an unrelated member must not stop someone else's work (active row)")
	}
	if canStopAgentWork(Actor{UserID: stranger}, activeTaskPrincipals(nil)) {
		t.Error("a missing row must never authorize a stop")
	}
}

// The entity id is what lets the surface a job runs on find it. It must resolve
// from the surface descriptor first and cope with every routing convention,
// including the prefixed source ids a coding job carries.
func TestWorkEntityID(t *testing.T) {
	postID := uuid.NewString()
	msgID := uuid.NewString()
	taskID := uuid.NewString()

	cases := map[string]struct {
		in   *model.AgentActiveTask
		want string
	}{
		"channel post from the surface": {
			in:   &model.AgentActiveTask{Surface: `{"kind":"channel_post","channel_id":"c","post_id":"` + postID + `"}`},
			want: postID,
		},
		"chat message from the surface": {
			in:   &model.AgentActiveTask{Surface: `{"kind":"dm","group_id":"g","message_id":"` + msgID + `"}`},
			want: msgID,
		},
		"task assignment from source_id": {
			in:   &model.AgentActiveTask{SourceType: model.TaskSourceAssignment, SourceId: taskID},
			want: taskID,
		},
		"code PR on a task": {
			in:   &model.AgentActiveTask{SourceType: "code_pr", SourceId: "task:" + taskID},
			want: taskID,
		},
		"code PR in a thread": {
			in:   &model.AgentActiveTask{SourceType: "code_pr", SourceId: "post:" + postID},
			want: postID,
		},
		"nothing addressable": {in: &model.AgentActiveTask{}, want: ""},
		"nil row":             {in: nil, want: ""},
	}
	for name, c := range cases {
		if got := workEntityID(c.in); got != c.want {
			t.Errorf("%s: workEntityID = %q, want %q", name, got, c.want)
		}
	}
}

func TestSurfaceWhereLabel(t *testing.T) {
	cases := map[SurfaceKind]string{
		SurfaceChannelPost:   "in a channel thread",
		SurfaceGroupChat:     "in a group chat",
		SurfaceDM:            "in a direct message",
		SurfaceTask:          "on a task",
		SurfaceKind("weird"): "on a request", // unknown falls back generically
	}
	for kind, want := range cases {
		if got := surfaceWhereLabel(Surface{Kind: kind}); got != want {
			t.Errorf("surfaceWhereLabel(%q) = %q, want %q", kind, got, want)
		}
	}
}

func TestBlockerNote(t *testing.T) {
	if got, _ := blockerNote("blocked: I need the API key to continue"); got != "I need the API key to continue" {
		t.Errorf("blockerNote strip prefix = %q", got)
	}
	if got, _ := blockerNote("  multi\n line\t reason  "); got != "multi line reason" {
		t.Errorf("blockerNote collapse whitespace = %q", got)
	}
	long := ""
	for i := 0; i < 200; i++ {
		long += "x"
	}
	if got, _ := blockerNote(long); len([]rune(got)) > 161 || got[len(got)-3:] != "…" {
		t.Errorf("blockerNote should cap and ellipsize long text, got len=%d", len([]rune(got)))
	}
	if got, _ := blockerNote("   "); got != "" {
		t.Errorf("blockerNote blank = %q, want empty", got)
	}
}

// A question with choices must reach the card AS choices.
//
// Before this it did not: the note collapses whitespace so a person could read
// it at a glance, which flattened the rendered list into
// "Which environment? - staging - production" and left them reading an
// enumeration out of a sentence. The options now come out before the collapse.
func TestBlockerNoteSeparatesTheOptions(t *testing.T) {
	stored := "blocked: " + newElicitation("Which environment?", `["staging","production"]`).Render()

	note, options := blockerNote(stored)
	if note != "Which environment?" {
		t.Errorf("note = %q, want just the question", note)
	}
	if len(options) != 2 || options[0] != "staging" || options[1] != "production" {
		t.Fatalf("options = %v, want [staging production]", options)
	}

	// A pause that is not an enumerated question (a budget pause shares this
	// field) must come back as plain text with nothing invented.
	note, options = blockerNote("blocked: today's AI usage limit has been reached")
	if options != nil {
		t.Errorf("options = %v, want none for a non-enumerated pause", options)
	}
	if note != "today's AI usage limit has been reached" {
		t.Errorf("note = %q", note)
	}
}

// attachRequesters is index-sensitive, and every active-work feed FILTERS rows,
// so the failure mode to guard is attributing a row to the wrong person. These
// cover the shapes that produce that without needing a database: a length
// mismatch must be refused outright, and an empty requester must stay empty
// rather than shifting the names that follow it.
func TestAttachRequestersRefusesMisalignedInput(t *testing.T) {
	items := []ActiveWorkItem{{TaskID: "a"}, {TaskID: "b"}}

	// Fewer ids than items: attributing by position would name row b with row a's
	// requester. Refuse instead.
	attachRequesters(context.Background(), items, []string{"only-one"})
	for _, it := range items {
		if it.RequestedBy != "" {
			t.Fatalf("misaligned input must attribute nobody, got %q", it.RequestedBy)
		}
	}

	// More ids than items.
	attachRequesters(context.Background(), items, []string{"a", "b", "c"})
	for _, it := range items {
		if it.RequestedBy != "" {
			t.Fatalf("misaligned input must attribute nobody, got %q", it.RequestedBy)
		}
	}

	// All-empty requesters (every job is a scheduled routine) must short-circuit
	// before any resolution is attempted.
	attachRequesters(context.Background(), items, []string{"", ""})
	for _, it := range items {
		if it.RequestedBy != "" {
			t.Fatalf("no requesters must attribute nobody, got %q", it.RequestedBy)
		}
	}

	// Empty item list is safe.
	attachRequesters(context.Background(), nil, nil)
}

func TestRequesterUUID(t *testing.T) {
	if got := requesterUUID(nil); got != "" {
		t.Fatalf("nil task = %q, want empty", got)
	}
	// A scheduled routine has no requester and must read as absent rather than as
	// a zero uuid, which would render as a name lookup for 000...0.
	if got := requesterUUID(&model.AgentActiveTask{}); got != "" {
		t.Fatalf("task with no triggered_by = %q, want empty", got)
	}
	id := uuid.New()
	if got := requesterUUID(&model.AgentActiveTask{TriggeredBy: &id}); got != id.String() {
		t.Fatalf("got %q, want %q", got, id.String())
	}
}
