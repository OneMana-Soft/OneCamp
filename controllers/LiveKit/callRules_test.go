package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	adapter "github.com/akashc777/OneCamp/adapter/LiveKit"
	business "github.com/akashc777/OneCamp/business/LiveKit"
)

// Each refusal is a normal answer with the code the client acts on (the
// CALLS-FE ↔ CALLS-BE contract), never a 500.
func TestTranscriptRefusalsCarryTheirCode(t *testing.T) {
	for err, want := range map[error]struct {
		status int
		code   string
	}{
		business.ErrNotInCall:             {http.StatusForbidden, "not_in_call"},
		business.ErrNotASpeaker:           {http.StatusForbidden, "not_in_call"},
		business.ErrTranscriptionDisabled: {http.StatusConflict, "transcription_disabled"},
		business.ErrTranscriptionOff:      {http.StatusConflict, "transcription_off"},
		business.ErrTranscriptNotKept:     {http.StatusConflict, "transcript_not_kept"},
		business.ErrNotBrowserMode:        {http.StatusConflict, "not_browser_mode"},
	} {
		w := httptest.NewRecorder()
		if !transcriptRefusal(w, err) {
			t.Fatalf("%v was not answered", err)
		}
		var got map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if w.Code != want.status || got["code"] != want.code {
			t.Errorf("%v: %d %v, want %d %s", err, w.Code, got, want.status, want.code)
		}
	}
	if transcriptRefusal(httptest.NewRecorder(), errors.New("dgraph is down")) {
		t.Error("a store that failed is not a refusal")
	}
}

// CALL-BUG-5. The agent used to relay any participant's "lk.transcription"
// packet, with the speaker named inside it, so anyone in a call could put
// words in someone else's mouth. Only lines the agent transcribed itself are
// accepted; a line without that (what a relaying agent sends) is refused
// before anything is looked up.
func TestTheServerTranscriberCannotRelayOtherPeoplesPackets(t *testing.T) {
	body := `{"room_name":"4f0c2a1e-3b7d-4c55-9a51-0d8b1f6a2c33","participant_identity":"0x1","text":"I approve the budget","egress_id":"call-RM_x"}`
	w := httptest.NewRecorder()
	SaveTranscript(w, httptest.NewRequest(http.MethodPost, "/livekit/transcript", strings.NewReader(body)))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "not_transcribed_here") {
		t.Fatalf("a relayed line was not refused: %d %s", w.Code, w.Body.String())
	}
}

// CALL-BUG-16. egress_ended without its egress is ignored, never
// dereferenced.
func TestARecordingEndWithoutItsEgressIsIgnored(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("egress_ended without an egress panicked: %v", r)
		}
	}()
	recordingEnded(context.Background(), nil)
}

// The server decides which call a line belongs to; the agent says only that it
// transcribed the line itself. The marker lives in two languages, so the two
// are pinned together here, and the agent must not relay packets any more.
func TestTheAgentSendsOnlyWhatItTranscribed(t *testing.T) {
	agent, err := os.ReadFile(filepath.Join("..", "..", "livekit-agent", "agent.py"))
	if err != nil {
		t.Fatalf("the transcription agent ships in the archive and must be here: %v", err)
	}
	src := string(agent)
	if want := `"source": "` + adapter.TranscriptSourceSTT + `"`; !strings.Contains(src, want) {
		t.Errorf("the agent's posts lack %s, so the server refuses every line it transcribes", want)
	}
	if strings.Contains(src, `@self.room.on("data_received")`) {
		t.Error("the agent relays data packets again: anyone in a call could put words in someone else's mouth")
	}
	if strings.Contains(src, `"egress_id"`) {
		t.Error("the agent names the call its lines belong to; the server decides that")
	}
}
