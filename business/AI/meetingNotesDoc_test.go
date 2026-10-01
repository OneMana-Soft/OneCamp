package business

import (
	"context"
	"strings"
	"testing"
	"time"

	transcriptDomain "github.com/akashc777/OneCamp/domain/LiveKit"
)

func TestBuildMeetingNotesHTMLEscapesEverything(t *testing.T) {
	// A transcript is speech a model wrote down from audio anyone on the call
	// supplied, going into a document other people open. It is untrusted input.
	lines := []transcriptDomain.TranscriptLine{
		{ParticipantIdentity: "0x1", Text: `<img src=x onerror="alert(1)">`},
		{ParticipantIdentity: "0x2", Text: "plain words"},
	}
	names := map[string]string{"0x1": `<script>bad</script>`, "0x2": "Sam"}

	out := buildMeetingNotesHTML("<b>recap</b> & more", "", lines, names)

	// What matters is that no TAG survives. "onerror=" as escaped text is inert:
	// there is no element for it to be an attribute of. Asserting on the
	// substring instead of the tag opener would fail on correct output, which is
	// how a guard gets loosened until it checks nothing.
	for _, tag := range []string{"<img", "<script", "<b>"} {
		if strings.Contains(out, tag) {
			t.Errorf("unescaped tag %q reached the document body:\n%s", tag, out)
		}
	}
	if !strings.Contains(out, "Sam:") {
		t.Error("a normal speaker name should survive escaping")
	}
	// The recap is stripped of markup, not escaped into visible tag text.
	if strings.Contains(out, "&lt;b&gt;") {
		t.Error("recap markup should be removed, not shown as escaped text")
	}
}

func TestBuildMeetingNotesHTMLNamesAnUnknownSpeaker(t *testing.T) {
	// Somebody who left the workspace still spoke. A raw dgraph uid in a
	// document is meaningless to a reader and leaks an internal identifier.
	out := buildMeetingNotesHTML("r", "", []transcriptDomain.TranscriptLine{
		{ParticipantIdentity: "0xdeadbeef", Text: "I said something"},
	}, map[string]string{})
	if strings.Contains(out, "0xdeadbeef") {
		t.Errorf("a raw uid reached the document:\n%s", out)
	}
	if !strings.Contains(out, "Unknown speaker") {
		t.Errorf("an unresolved speaker should be named, got:\n%s", out)
	}
}

func TestBuildMeetingNotesHTMLBoundsTheTranscript(t *testing.T) {
	// The body is stored and re-embedded on every edit, so it cannot be
	// unbounded, and a truncated document must say that it is truncated.
	lines := make([]transcriptDomain.TranscriptLine, maxDocTranscriptLines+50)
	for i := range lines {
		lines[i] = transcriptDomain.TranscriptLine{ParticipantIdentity: "0x1", Text: "line"}
	}
	out := buildMeetingNotesHTML("r", "", lines, map[string]string{"0x1": "Sam"})
	if strings.Count(out, "Sam:") > maxDocTranscriptLines {
		t.Error("the transcript was not capped")
	}
	if !strings.Contains(out, "Showing the first") {
		t.Error("a truncated transcript must say so")
	}
}

func TestMeetingNotesTitle(t *testing.T) {
	at := time.Date(2026, 8, 31, 14, 5, 0, 0, time.UTC)
	if got := meetingNotesTitle("#general", at); !strings.Contains(got, "#general") || !strings.Contains(got, "2026") {
		t.Errorf("title = %q, want the surface and the date", got)
	}
	// A DM has no surface name, and the title still has to be usable.
	got := meetingNotesTitle("", at)
	if strings.Contains(got, ", ,") || !strings.HasPrefix(got, "Meeting notes: ") {
		t.Errorf("title with no surface = %q", got)
	}
}

// notesAudience decides who can open a meeting document, so its degradation
// behaviour matters as much as its happy path: with Redis unavailable,
// GetSetMembers returns an empty set and no error, and the audience must fall
// back to the speakers rather than to nobody.
func TestNotesAudienceFallsBackToSpeakers(t *testing.T) {
	// No Redis in a unit test, which is exactly the degraded case.
	got := notesAudience(context.Background(), "room-1", []string{"0x1", "0x2"})
	if len(got) != 2 || got[0] != "0x1" || got[1] != "0x2" {
		t.Errorf("audience = %v, want the speakers in order", got)
	}
}

func TestNotesAudienceKeepsSpeakersFirstAndDeduplicates(t *testing.T) {
	// The owner is picked from the head of this list, and a speaker is the
	// likeliest to still be a live user, so speakers must lead.
	got := notesAudience(context.Background(), "room-1", []string{"0x1", "", "0x1", "  0x2  "})
	if len(got) != 2 {
		t.Fatalf("audience = %v, want 2 after trimming and deduplication", got)
	}
	if got[0] != "0x1" || got[1] != "0x2" {
		t.Errorf("audience = %v, want [0x1 0x2]", got)
	}
}

func TestNotesAudienceIsEmptyWhenNobodySpokeAndNothingWasRecorded(t *testing.T) {
	// Empty in, empty out. createMeetingNotesDoc treats this as "do not
	// create", because a document with no editors is one nobody can open.
	if got := notesAudience(context.Background(), "room-1", nil); len(got) != 0 {
		t.Errorf("audience = %v, want none", got)
	}
}
