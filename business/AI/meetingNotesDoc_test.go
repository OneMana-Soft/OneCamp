package business

import (
	"context"
	"strings"
	"testing"
	"time"

	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
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

// mergeAudience puts speakers first (the owner is picked from the head of the
// list, and a speaker is the likeliest to still be a live user), then the
// call's other attendees, without blanks or repeats.
func TestMergeAudienceKeepsSpeakersFirstAndDeduplicates(t *testing.T) {
	got := mergeAudience([]string{"0x1", "", "0x1", "  0x2  "}, []string{"0x3", "0x2", " "})
	if want := []string{"0x1", "0x2", "0x3"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("audience = %v, want %v", got, want)
	}
	if got := mergeAudience(nil, nil); len(got) != 0 {
		t.Errorf("audience = %v, want none", got)
	}
}

// CALL-BUG-7. The notes reach only people who can still open the
// conversation when the document is written: someone removed from a private
// channel after the call is not given its notes, whether they spoke or only
// attended. Redis is absent here, which is also the degraded case: the
// audience falls back to the speakers, still checked.
func TestNotesAudienceLeavesOutWhoCanNoLongerOpenTheConversation(t *testing.T) {
	keep := mayOpenConversation
	t.Cleanup(func() { mayOpenConversation = keep })
	var asked []string
	mayOpenConversation = func(_ context.Context, room, uid string) bool {
		asked = append(asked, room+"/"+uid)
		return uid != "0x2" // 0x2 was removed from the channel after the call
	}

	call := liveKitBusiness.FinishedCall{Room: "4f0c2a1e-3b7d-4c55-9a51-0d8b1f6a2c33", SessionKey: "call-RM_abc"}
	got := notesAudience(context.Background(), call, []string{"0x1", "0x2", "0x3"})
	if want := "0x1,0x3"; strings.Join(got, ",") != want {
		t.Errorf("audience = %v, want %s", got, want)
	}
	if len(asked) != 3 || !strings.HasPrefix(asked[0], call.Room+"/") {
		t.Errorf("every candidate must be checked against the call's conversation, asked %v", asked)
	}
}
