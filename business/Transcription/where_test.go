package business

import (
	"strings"
	"testing"
)

// Where call audio goes, for the notice in every call and the admin card.
func TestWhereFor(t *testing.T) {
	for _, c := range []struct {
		mode, provider, baseURL, want string
	}{
		{ModeFrontend, STTDeepgram, "", WhereBrowser},
		{ModeBackend, STTLocal, "", WhereServer},
		{ModeBackend, STTDeepgram, "", STTDeepgram},
		{ModeBackend, STTGoogle, "", STTGoogle},
		{ModeBackend, STTOpenAI, "", STTOpenAI},
		{ModeBackend, STTOpenAI, "https://api.openai.com/v1", STTOpenAI},
		{ModeBackend, STTOpenAI, "https://api.groq.com/openai/v1", WhereCustom},
		{ModeOff, STTLocal, "", ""},
	} {
		if got := whereFor(c.mode, c.provider, c.baseURL); got != c.want {
			t.Errorf("whereFor(%s, %s, %q) = %q, want %q", c.mode, c.provider, c.baseURL, got, c.want)
		}
	}
}

// CALL-BUG-4. A speech model that answers is half of server mode; the agent
// that joins calls is the other half, and Test passed on the model alone while
// no agent ran on any customer install.
func TestTestFailsWithoutTheAgent(t *testing.T) {
	ok := &TestResult{OK: true, Provider: STTLocal, Message: "The bundled transcription server is running."}
	if got := withAgent(ok, AgentStatus{}); got.OK || !strings.Contains(got.Message, "make stt_up") {
		t.Errorf("no agent: %+v", got)
	}
	if got := withAgent(ok, AgentStatus{Running: true}); got.OK {
		t.Errorf("an agent that can't reach the call server can't join calls: %+v", got)
	}
	if got := withAgent(ok, AgentStatus{Running: true, LiveKit: true}); !got.OK {
		t.Errorf("model and agent both running: %+v", got)
	}
	failed := &TestResult{OK: false, Provider: STTLocal, Message: "Could not reach it."}
	if got := withAgent(failed, AgentStatus{Running: true, LiveKit: true}); got != failed {
		t.Errorf("a failed probe stays the answer: %+v", got)
	}
}
