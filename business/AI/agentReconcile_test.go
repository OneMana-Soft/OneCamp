package business

import (
	"strings"
	"testing"

	adapter "github.com/akashc777/OneCamp/adapter/AI"
	ai "github.com/akashc777/OneCamp/services/AI"
)

const realTaskUUID = "44444444-4444-4444-4444-444444444444"
const realProjUUID = "77777777-7777-7777-7777-777777777777"

func TestHarvestEntityRefs_Tasks(t *testing.T) {
	result := "Your tasks (1 shown):\n- login bug [status: todo, priority: medium] (task_uuid: " + realTaskUUID + ")"
	known := map[string][]agentEntityRef{}
	harvestEntityRefs(result, known)

	ents := known["task_uuid"]
	if len(ents) != 1 {
		t.Fatalf("expected 1 task_uuid, got %d", len(ents))
	}
	if ents[0].uuid != realTaskUUID {
		t.Errorf("uuid: got %s want %s", ents[0].uuid, realTaskUUID)
	}
	if ents[0].name != "login bug" {
		t.Errorf("name: got %q want %q", ents[0].name, "login bug")
	}
}

// The core demo bug: the model mangles the UUID it copied from the read result.
// Reconciliation must repair it from the single authoritative candidate.
func TestReconcile_RepairsMangledSingleCandidate(t *testing.T) {
	known := map[string][]agentEntityRef{
		"task_uuid": {{name: "login bug", uuid: realTaskUUID}},
	}
	writes := []ai.ProposedAction{
		{
			ToolName:    "update_task_status",
			Description: "Mark the login bug task as done",
			// Model dropped a character -> fails uuid.Parse at execution.
			Params: map[string]string{"task_uuid": "4444444-4444-4444-4444-444444444444", "status": "done"},
		},
	}
	n := reconcileWriteUUIDs(writes, known)
	if n != 1 {
		t.Fatalf("expected 1 repair, got %d", n)
	}
	if got := writes[0].Params["task_uuid"]; got != realTaskUUID {
		t.Errorf("task_uuid not repaired: got %s want %s", got, realTaskUUID)
	}
}

func TestReconcile_KeepsCorrectUUID(t *testing.T) {
	known := map[string][]agentEntityRef{
		"task_uuid": {{name: "login bug", uuid: realTaskUUID}},
	}
	writes := []ai.ProposedAction{
		{ToolName: "update_task_status", Params: map[string]string{"task_uuid": realTaskUUID, "status": "done"}},
	}
	if n := reconcileWriteUUIDs(writes, known); n != 0 {
		t.Fatalf("expected 0 repairs for a correct UUID, got %d", n)
	}
	if got := writes[0].Params["task_uuid"]; got != realTaskUUID {
		t.Errorf("task_uuid changed unexpectedly: %s", got)
	}
}

// Multiple tasks surfaced: pick by name match against the action description.
func TestReconcile_MultiCandidateNameMatch(t *testing.T) {
	other := "11111111-1111-1111-1111-111111111111"
	known := map[string][]agentEntityRef{
		"task_uuid": {
			{name: "deploy pipeline", uuid: other},
			{name: "login bug", uuid: realTaskUUID},
		},
	}
	writes := []ai.ProposedAction{
		{
			ToolName:    "update_task_status",
			Description: "Mark the login bug task as done",
			Params:      map[string]string{"task_uuid": "garbage", "status": "done"},
		},
	}
	if n := reconcileWriteUUIDs(writes, known); n != 1 {
		t.Fatalf("expected 1 repair, got %d", n)
	}
	if got := writes[0].Params["task_uuid"]; got != realTaskUUID {
		t.Errorf("expected name match to login bug (%s), got %s", realTaskUUID, got)
	}
}

// Multiple candidates and no name match: leave untouched (don't guess).
func TestReconcile_MultiCandidateNoMatchLeavesAlone(t *testing.T) {
	a := "11111111-1111-1111-1111-111111111111"
	b := "22222222-2222-2222-2222-222222222222"
	known := map[string][]agentEntityRef{
		"task_uuid": {{name: "deploy", uuid: a}, {name: "infra", uuid: b}},
	}
	writes := []ai.ProposedAction{
		{ToolName: "update_task_status", Description: "mark it done", Params: map[string]string{"task_uuid": "garbage", "status": "done"}},
	}
	if n := reconcileWriteUUIDs(writes, known); n != 0 {
		t.Fatalf("expected 0 repairs (ambiguous), got %d", n)
	}
	if got := writes[0].Params["task_uuid"]; got != "garbage" {
		t.Errorf("should not have guessed; got %s", got)
	}
}

// create_task uses project_uuid; reconciliation works across labels.
func TestReconcile_ProjectUUID(t *testing.T) {
	known := map[string][]agentEntityRef{
		"project_uuid": {{name: "roadmap", uuid: realProjUUID}},
	}
	writes := []ai.ProposedAction{
		{ToolName: "create_task", Description: "Create task in Roadmap", Params: map[string]string{"project_uuid": "bad", "task_name": "x"}},
	}
	if n := reconcileWriteUUIDs(writes, known); n != 1 {
		t.Fatalf("expected 1 repair, got %d", n)
	}
	if got := writes[0].Params["project_uuid"]; got != realProjUUID {
		t.Errorf("project_uuid not repaired: %s", got)
	}
}

// The real-world failure: the model skipped list_tasks and emitted
// update_task_status directly with a mangled UUID it pulled from the RAG
// "Sources" (where the task's id lives in ContentUUID). Source reconciliation
// must repair it.
func TestReconcileWithSources_RepairsMalformedFromTaskSource(t *testing.T) {
	sources := []adapter.SourceRef{
		{ContentType: "task", ContentUUID: realTaskUUID, Snippet: "login bug"},
		{ContentType: "post", ContentUUID: "ignored", ChannelName: "general"},
	}
	writes := []ai.ProposedAction{
		{
			ToolName:    "update_task_status",
			Description: "Mark the login bug task as done",
			Params:      map[string]string{"task_uuid": "4444-bad-uuid", "status": "done"},
		},
	}
	n := ReconcileWriteActionsWithSources(writes, sources)
	if n != 1 {
		t.Fatalf("expected 1 repair, got %d", n)
	}
	if got := writes[0].Params["task_uuid"]; got != realTaskUUID {
		t.Errorf("task_uuid not repaired from source: got %s want %s", got, realTaskUUID)
	}
}

// Conservative policy: a well-formed UUID the model produced must NOT be
// overridden, because RAG sources are a partial set (the model may have a valid
// id we didn't retrieve).
func TestReconcileWithSources_KeepsWellFormedUUID(t *testing.T) {
	otherValid := "99999999-9999-9999-9999-999999999999"
	sources := []adapter.SourceRef{
		{ContentType: "task", ContentUUID: realTaskUUID},
	}
	writes := []ai.ProposedAction{
		{ToolName: "update_task_status", Params: map[string]string{"task_uuid": otherValid, "status": "done"}},
	}
	if n := ReconcileWriteActionsWithSources(writes, sources); n != 0 {
		t.Fatalf("expected 0 repairs (conservative), got %d", n)
	}
	if got := writes[0].Params["task_uuid"]; got != otherValid {
		t.Errorf("well-formed uuid should be left alone, got %s", got)
	}
}

// send_message channel repaired by name match when the user named the channel.
func TestReconcileWithSources_ChannelByName(t *testing.T) {
	chUUID := "88888888-8888-8888-8888-888888888888"
	sources := []adapter.SourceRef{
		{ContentType: "post", ContentUUID: "p1", ChannelUUID: "11111111-1111-1111-1111-111111111111", ChannelName: "random"},
		{ContentType: "post", ContentUUID: "p2", ChannelUUID: chUUID, ChannelName: "general"},
	}
	writes := []ai.ProposedAction{
		{ToolName: "send_message", Description: "Post the update in #general", Params: map[string]string{"channel_uuid": "bad", "text": "hi"}},
	}
	if n := ReconcileWriteActionsWithSources(writes, sources); n != 1 {
		t.Fatalf("expected 1 repair, got %d", n)
	}
	if got := writes[0].Params["channel_uuid"]; got != chUUID {
		t.Errorf("channel_uuid not matched by name: got %s want %s", got, chUUID)
	}
}

func TestNormalizeTaskPriority(t *testing.T) {
	cases := map[string]string{
		"high": "high", "urgent": "high", "critical": "high", "P1": "high", "highest": "high",
		"low": "low", "lowest": "low", "p5": "low", "minor": "low",
		"medium": "medium", "normal": "medium", "": "medium", "banana": "medium", "P2": "medium",
	}
	for in, want := range cases {
		if got := normalizeTaskPriority(in); got != want {
			t.Errorf("normalizeTaskPriority(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStripReasoning(t *testing.T) {
	cases := []struct{ in, want string }{
		{"<think>plan the thing</think>Hello", "Hello"},
		{"before<think>secret</think> after", "before after"},
		{"<thinking>x</thinking>Answer", "Answer"},
		{"no tags here", "no tags here"},
		{"<think>unclosed reasoning that runs to the end", ""},
	}
	for _, c := range cases {
		if got := StripReasoning(c.in); got != c.want {
			t.Errorf("StripReasoning(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestStreamThinkFilter(t *testing.T) {
	// Reasoning split across chunks must never surface; the answer must.
	f := &StreamThinkFilter{}
	var out string
	for _, chunk := range []string{"He", "llo <thi", "nk>secret rea", "soning</thi", "nk> world"} {
		out += f.Feed(chunk)
	}
	out += f.Flush()
	if strings.Contains(out, "secret") || strings.Contains(out, "<think") {
		t.Errorf("think content leaked: %q", out)
	}
	if !strings.Contains(out, "Hello") || !strings.Contains(out, "world") {
		t.Errorf("answer text dropped: %q", out)
	}
}

func TestFormatRecentForLLM_StripsHTML(t *testing.T) {
	out := formatRecentForLLM([]ai.SimilarResult{
		{AuthorName: "Akash", ContentText: `<p class="text-node">jkjkjs</p>`},
	})
	if strings.Contains(out, "<p") || strings.Contains(out, "text-node") || strings.Contains(out, "</p>") {
		t.Errorf("HTML leaked into LLM context: %q", out)
	}
	if !strings.Contains(out, "jkjkjs") {
		t.Errorf("content text dropped: %q", out)
	}
}

func TestSanitizeResponse_StripsLeakedHTML(t *testing.T) {
	in := "Your task about the login bug is:\n\n**login bug**\n\n<p class=\"text-node\">jkjkjs</p>\n\nTo mark it done, I will update its status."
	out := SanitizeResponse(in)
	if strings.Contains(out, "<p") || strings.Contains(out, "text-node") || strings.Contains(out, "</p>") {
		t.Errorf("HTML tags leaked into answer: %q", out)
	}
	// The visible text and markdown must survive.
	if !strings.Contains(out, "jkjkjs") || !strings.Contains(out, "**login bug**") {
		t.Errorf("legitimate content/markdown dropped: %q", out)
	}
	// A literal comparison in prose/code must NOT be stripped (whitelist guard).
	keep := SanitizeResponse("use a < b and b > c in the check")
	if !strings.Contains(keep, "a < b") || !strings.Contains(keep, "b > c") {
		t.Errorf("non-HTML angle brackets wrongly stripped: %q", keep)
	}
}
