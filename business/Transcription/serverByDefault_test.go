package business

import (
	"errors"
	"testing"
)

// zoom-09 C2: no call audio leaves the server by default. A server where
// nobody chose anything transcribes on itself; whatever an install's env or an
// admin chose is kept; and audio goes anywhere else only after an admin said
// they know it does.

// unsetEnv clears every variable the mode and provider read, for one test.
func unsetEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"TRANSCRIPTION_MODE", "STT_PROVIDER", "STT_BASE_URL"} {
		t.Setenv(k, "")
	}
	invalidate()
}

func TestAnUnsetModeKeepsCallAudioOnTheServer(t *testing.T) {
	unsetEnv(t)
	if got := Mode(); got != ModeBackend {
		t.Errorf("mode with nothing set: %q, want %q", got, ModeBackend)
	}
	if got := STTProvider(); got != STTLocal {
		t.Errorf("provider with nothing set: %q, want %q", got, STTLocal)
	}
	if got := Where(); got != WhereServer {
		t.Errorf("where with nothing set: %q, want %q", got, WhereServer)
	}
	if LeavesServer(Where()) {
		t.Error("a fresh install sends call audio off the server")
	}
}

// Existing installs keep what their env says: the old installer wrote the
// browser mode and Deepgram explicitly, and nothing flips without an admin.
func TestAnExplicitEnvModeIsKept(t *testing.T) {
	for _, c := range []struct{ mode, provider, base, want string }{
		{"frontend", "deepgram", "", WhereBrowser},
		{"frontend", "", "", WhereBrowser},
		{"backend", "deepgram", "", STTDeepgram},
		{"backend", "google", "", STTGoogle},
		{"backend", "openai", "", STTOpenAI},
		{"backend", "openai", "https://stt.example.com/v1", WhereCustom},
		{"backend", "local", "", WhereServer},
		{"off", "deepgram", "", ""},
	} {
		unsetEnv(t)
		t.Setenv("TRANSCRIPTION_MODE", c.mode)
		t.Setenv("STT_PROVIDER", c.provider)
		t.Setenv("STT_BASE_URL", c.base)
		if got := Where(); got != c.want {
			t.Errorf("env %s/%s: where %q, want %q", c.mode, c.provider, got, c.want)
		}
		if got := Mode(); got != c.mode {
			t.Errorf("env mode %s became %q", c.mode, got)
		}
	}
}

func TestOnlyTheServerKeepsTheAudio(t *testing.T) {
	for where, leaves := range map[string]bool{
		WhereServer: false, "": false,
		WhereBrowser: true, WhereCustom: true, STTDeepgram: true, STTGoogle: true, STTOpenAI: true,
	} {
		if LeavesServer(where) != leaves {
			t.Errorf("LeavesServer(%q) = %v, want %v", where, !leaves, leaves)
		}
	}
}

// Captions are off, with a code the clients put in words, when they can't
// run; never a quiet fall-through to another path.
func TestCaptionsSayWhyTheyAreOff(t *testing.T) {
	up := AgentStatus{Running: true, LiveKit: true}
	for _, c := range []struct {
		mode  string
		agent AgentStatus
		want  string
	}{
		{ModeOff, up, ReasonTranscriptionDisabled},
		{ModeBackend, AgentStatus{}, ReasonTranscriberNotRunning},
		{ModeBackend, AgentStatus{Running: true}, ReasonTranscriberNotRunning},
		{ModeBackend, up, ""},
		{ModeFrontend, AgentStatus{}, ""},
	} {
		if got := captionsOffReason(c.mode, c.agent); got != c.want {
			t.Errorf("%s with agent %+v: %q, want %q", c.mode, c.agent, got, c.want)
		}
	}
}

// An admin acknowledges exactly where the audio will go, once.
func TestAnAcknowledgementNamesWhereTheAudioGoes(t *testing.T) {
	for _, c := range []struct {
		next, stored, given string
		record              bool
		refused             bool
	}{
		{WhereServer, "", "", false, false},
		{"", "", "", false, false},
		{WhereServer, "", WhereBrowser, false, false},
		{WhereBrowser, "", "", false, true},
		{WhereBrowser, "", STTDeepgram, false, true},
		{WhereBrowser, "", WhereBrowser, true, false},
		{WhereBrowser, WhereBrowser, "", false, false},
		{WhereBrowser, WhereBrowser, WhereBrowser, false, false},
		{STTDeepgram, WhereBrowser, "", false, true},
		{STTDeepgram, WhereBrowser, STTDeepgram, true, false},
	} {
		record, err := checkAcknowledgement(c.next, c.stored, c.given)
		var need *AcknowledgementRequired
		if refused := errors.As(err, &need); refused != c.refused || record != c.record {
			t.Errorf("next %q stored %q given %q: record %v err %v", c.next, c.stored, c.given, record, err)
		} else if refused && need.Where != c.next {
			t.Errorf("the refusal names %q, want %q", need.Where, c.next)
		}
		if got := acknowledgementNeeded(c.next, c.stored); got != (LeavesServer(c.next) && c.stored != c.next) {
			t.Errorf("needed(%q, %q) = %v", c.next, c.stored, got)
		}
	}
}

// Where a save would send the audio, from what it changes over what is set.
func TestWhereASaveSendsTheAudio(t *testing.T) {
	unsetEnv(t)
	s := func(v string) *string { return &v }
	for _, c := range []struct {
		in   SaveInput
		want string
	}{
		{SaveInput{}, WhereServer},
		{SaveInput{Mode: s("frontend")}, WhereBrowser},
		{SaveInput{STTProvider: s("deepgram")}, STTDeepgram},
		{SaveInput{Mode: s("backend"), STTProvider: s("openai"), STTBaseURL: s("https://stt.example.com/v1")}, WhereCustom},
		{SaveInput{Mode: s("off")}, ""},
		{SaveInput{STTLanguage: s("en")}, WhereServer},
	} {
		if got := whereFor(configAfter(c.in)); got != c.want {
			t.Errorf("%+v: %q, want %q", c.in, got, c.want)
		}
	}
	// A save that empties the endpoint falls back to the env's, the way
	// STTBaseURL reads it: the audio goes to the env's host, not to OpenAI.
	t.Setenv("STT_BASE_URL", "https://stt.example.com/v1")
	emptied := SaveInput{Mode: s("backend"), STTProvider: s("openai"), STTBaseURL: s("")}
	if got := whereFor(configAfter(emptied)); got != WhereCustom {
		t.Errorf("an emptied endpoint with the env's set: %q, want %q", got, WhereCustom)
	}
}
