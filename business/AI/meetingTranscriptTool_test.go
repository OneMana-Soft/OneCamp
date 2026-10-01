package business

import (
	"strings"
	"testing"

	ai "github.com/akashc777/OneCamp/services/AI"
)

// The tool is registered and read-only. A call transcript tool that the registry
// thinks might write would be skipped by dry runs and approval-gated for nothing.
func TestReadMeetingTranscriptIsRegisteredReadOnly(t *testing.T) {
	if _, ok := ai.Executors["read_meeting_transcript"]; !ok {
		t.Fatal("read_meeting_transcript has no executor, so the model can propose it and nothing runs")
	}
	if !ai.ToolIsReadOnly("read_meeting_transcript") {
		t.Error("reading a transcript is being treated as a write")
	}
}

// The refusal must not distinguish "no call here" from "not allowed to see it".
// Telling somebody a call happened somewhere they cannot see is a disclosure in
// itself, and it is the kind of message that gets softened later by someone
// trying to be helpful.
func TestInaccessibleChannelIsIndistinguishableFromNoCall(t *testing.T) {
	src := readSource(t, "meetingRecapAgent.go")
	i := strings.Index(src, "func executeReadMeetingTranscript")
	if i < 0 {
		t.Fatal("executor not found")
	}
	body := src[i:]
	if end := strings.Index(body, "\n}\n"); end > 0 {
		body = body[:end]
	}
	if !strings.Contains(body, "no call found for that channel, or you do not have access to it") {
		t.Error("the permission refusal no longer reads the same as an empty channel")
	}
}

// Access is checked BEFORE the transcript store is touched. Reading first and
// filtering after would mean the sensitive fetch happens for people who may not
// see it, which is the bug this ordering exists to prevent.
func TestAccessIsCheckedBeforeReading(t *testing.T) {
	src := readSource(t, "meetingRecapAgent.go")
	i := strings.Index(src, "func executeReadMeetingTranscript")
	body := src[i:]
	gate := strings.Index(body, "getAccessibleResourceUUIDs")
	fetch := strings.Index(body, "GetTranscriptLinesByRoom")
	if gate < 0 || fetch < 0 {
		t.Fatal("expected both the gate and the fetch in the executor")
	}
	if gate > fetch {
		t.Error("the transcript is fetched before access is checked")
	}
}

// Output is bounded, or one long call eats the context budget and the cost
// ceiling in a single tool call.
func TestTranscriptOutputIsBounded(t *testing.T) {
	if transcriptMaxChars <= 0 || transcriptMaxLines <= 0 {
		t.Fatal("transcript limits must be positive")
	}
	if transcriptMaxChars > 40000 {
		t.Errorf("transcriptMaxChars = %d, large enough to blow a context window", transcriptMaxChars)
	}
}
