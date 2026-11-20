package business

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	transcriptDomain "github.com/akashc777/OneCamp/domain/LiveKit"
	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// The recap the model writes is split once: its words, without its action
// items section, and each item with who and by when when it names them.
func TestARecapSplitsIntoItsWordsAndItsActionItems(t *testing.T) {
	recap := "**📝 Summary**\n- We ship the beta on Friday.\n\n**📋 Action Items**\n" +
		"- @Maya Rao: send the deck to legal (due 2026-11-14)\n" +
		"- **Jonas**: book the launch room by 2026-11-13\n" +
		"* [ ] Draft the FAQ\n" +
		"- Rollback: rehearse it on staging (due 2026-02-30)\n\n" +
		"**✅ Decisions**\n- The Business tier is on the pricing page."
	got := splitRecap(recap)
	if strings.Contains(got.Text, "Action Items") || strings.Contains(got.Text, "send the deck") {
		t.Errorf("the recap's words kept its action items: %q", got.Text)
	}
	for _, want := range []string{"We ship the beta on Friday.", "**✅ Decisions**", "The Business tier is on the pricing page."} {
		if !strings.Contains(got.Text, want) {
			t.Errorf("the recap's words lost %q: %q", want, got.Text)
		}
	}
	want := []recordingDomain.ActionItem{
		{Text: "send the deck to legal", Owner: "Maya Rao", Due: "2026-11-14"},
		{Text: "book the launch room by 2026-11-13", Owner: "Jonas", Due: "2026-11-13"},
		{Text: "Draft the FAQ"},
		// No name is read from "Word:", and no day that isn't one.
		{Text: "Rollback: rehearse it on staging (due 2026-02-30)"},
	}
	if fmt.Sprint(got.Items) != fmt.Sprint(want) {
		t.Errorf("items =\n %+v\nwant\n %+v", got.Items, want)
	}

	for _, none := range []string{"**📋 Action Items**\n- None", "### Action items:\n- None.\n", "Summary only."} {
		if got := splitRecap(none); len(got.Items) != 0 {
			t.Errorf("%q made items %+v", none, got.Items)
		}
	}
	if got := splitRecap("Summary only."); got.Text != "Summary only." {
		t.Errorf("a recap with no items section kept %q", got.Text)
	}
	var many strings.Builder
	many.WriteString("Action items:\n")
	for i := 0; i < recordingDomain.MaxActionItems+3; i++ {
		fmt.Fprintf(&many, "%d. Task %d\n", i+1, i)
	}
	if got := splitRecap(many.String()); len(got.Items) != recordingDomain.MaxActionItems || got.Items[1].Text != "Task 1" {
		t.Errorf("numbered items = %d %+v, want the first %d", len(got.Items), got.Items, recordingDomain.MaxActionItems)
	}
}

// The transcript goes to the model with the day the call ended and the week
// after it, so "by Friday" is written as the Friday's date.
func TestARecapsDeadlinesAreLookedUpInTheWeekAfterTheCall(t *testing.T) {
	got := recapCalendar(time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC))
	for _, want := range []string{"Thursday 2026-11-12", "Fri 2026-11-13", "Thu 2026-11-19", "Transcript:"} {
		if !strings.Contains(got, want) {
			t.Errorf("recapCalendar = %q, missing %q", got, want)
		}
	}
	if recapCalendar(time.Time{}) != "" {
		t.Error("a call with no end was given a calendar")
	}
	if !strings.Contains(recapSystemPrompt, "(due YYYY-MM-DD)") || !strings.Contains(recapSystemPrompt, "@Name:") {
		t.Error("the recap prompt no longer asks for items as they are read")
	}
}

// recapSpies stand in for everything deliverAndKeepRecap writes to.
type recapSpies struct {
	kept     []recordingDomain.CallRecap
	keptKeys []string
	notesDoc string
	told     []calendarBusiness.Recap
	toldTo   []uuid.UUID
}

func spyRecap(t *testing.T) *recapSpies {
	t.Helper()
	s := &recapSpies{}
	oldRecord, oldTell, oldNotes, oldDeliver, oldGap := recordCallArtefacts, tellEventRecap, makeNotesDoc, deliverRecapTo, gapNoteOf
	t.Cleanup(func() {
		recordCallArtefacts, tellEventRecap, makeNotesDoc, deliverRecapTo, gapNoteOf = oldRecord, oldTell, oldNotes, oldDeliver, oldGap
	})
	recordCallArtefacts = func(_ context.Context, key, notesDoc, _ string, r recordingDomain.CallRecap) error {
		s.keptKeys, s.notesDoc = append(s.keptKeys, key), notesDoc
		s.kept = append(s.kept, r)
		return nil
	}
	tellEventRecap = func(_ context.Context, event uuid.UUID, r calendarBusiness.Recap) error {
		s.toldTo, s.told = append(s.toldTo, event), append(s.told, r)
		return nil
	}
	makeNotesDoc = func(context.Context, string, liveKitBusiness.FinishedCall, recapHome, []string, string, string, []transcriptDomain.TranscriptLine, map[string]string) (string, error) {
		return "doc-1", nil
	}
	deliverRecapTo = func(context.Context, recapHome, []string, string, string, string, string) (string, error) {
		return "uid-maya", nil
	}
	gapNoteOf = func(context.Context, string) string { return "" }
	return s
}

// An event's call keeps its recap on the call whatever the notes setting and
// whether or not a conversation holds it, and tells the event's people of
// the recap's words and items: never a line of the transcript.
func TestAnEventsCallKeepsItsRecapAndTellsItsPeopleNeverTheTranscript(t *testing.T) {
	event := uuid.New()
	call := liveKitBusiness.FinishedCall{Room: "meet-1", SessionKey: "call:s1", Ended: time.Now()}
	secret := "the board must never hear the acquisition price"
	lines := []transcriptDomain.TranscriptLine{{ParticipantIdentity: "uid-maya", Text: secret}}
	recap := "**📝 Summary**\n- We ship Friday.\n\n**📋 Action Items**\n- @Maya: send the deck (due 2026-11-14)"
	for _, c := range []struct {
		name    string
		notesOn bool
		conv    conversationBusiness.Ref
	}{
		{"notes off, no conversation", false, conversationBusiness.Ref{}},
		{"notes on, the people's group", true, conversationBusiness.Ref{Kind: conversationBusiness.KindGroup, ID: "grp-1"}},
	} {
		s := spyRecap(t)
		home := recapHome{home: callStateBusiness.Home{Ref: callStateBusiness.Ref{Kind: callStateBusiness.KindMeeting, ID: "1"}, Event: event},
			conv: c.conv, event: &calendarBusiness.CallPeople{Title: "Launch sync"}}
		if _, err := deliverAndKeepRecap(context.Background(), call, home, c.notesOn, []string{"uid-maya"}, recap, "", lines, nil); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(s.kept) != 1 || s.keptKeys[0] != "call:s1" || s.kept[0].Text != "**📝 Summary**\n- We ship Friday." ||
			len(s.kept[0].Items) != 1 || s.kept[0].Items[0].Owner != "Maya" || s.kept[0].Items[0].Due != "2026-11-14" {
			t.Errorf("%s: kept %+v on %v, want the recap's words and its one item on the call", c.name, s.kept, s.keptKeys)
		}
		if (s.notesDoc == "doc-1") != c.notesOn {
			t.Errorf("%s: notes doc %q", c.name, s.notesDoc)
		}
		if len(s.told) != 1 || s.toldTo[0] != event || s.told[0].Key != "call:s1" || s.told[0].Text != s.kept[0].Text ||
			len(s.told[0].Items) != 1 || s.told[0].Items[0].Text != "send the deck" {
			t.Fatalf("%s: told %+v to %v, want the event told the recap", c.name, s.told, s.toldTo)
		}
		if strings.Contains(fmt.Sprint(s.told), secret) || strings.Contains(fmt.Sprint(s.kept), secret) {
			t.Errorf("%s: the transcript reached the recap: %+v", c.name, s.told)
		}
	}
}

// A conversation's call keeps its recap on the call too, and tells no event.
func TestAConversationsCallKeepsItsRecapAndTellsNoEvent(t *testing.T) {
	s := spyRecap(t)
	home := recapHome{home: callStateBusiness.Home{Ref: callStateBusiness.Ref{Kind: conversationBusiness.KindChannel, ID: "ch-1"}},
		conv: conversationBusiness.Ref{Kind: conversationBusiness.KindChannel, ID: "ch-1"}}
	author, err := deliverAndKeepRecap(context.Background(), liveKitBusiness.FinishedCall{Room: "ch-1", SessionKey: "call:s2"}, home, false,
		[]string{"uid-maya"}, "- We met.", "", nil, nil)
	if err != nil || author != "uid-maya" {
		t.Fatalf("deliverAndKeepRecap = %q, %v", author, err)
	}
	if len(s.kept) != 1 || s.kept[0].Text != "- We met." || len(s.told) != 0 {
		t.Errorf("kept %+v, told %+v; want it kept and no event told", s.kept, s.told)
	}
}

// A recap posted to the event's conversation (its people's DM or group)
// tells the calendar who had it there, the members that conversation is of,
// so the calendar sends them no second notice. A recap that went nowhere (no
// conversation, or its post failed) leaves every member to the calendar.
func TestARecapPostedToTheEventsGroupNamesWhoHadItThere(t *testing.T) {
	event, maya, lee := uuid.New(), uuid.New(), uuid.New()
	call := liveKitBusiness.FinishedCall{Room: "meet-1", SessionKey: "call:s1", Ended: time.Now()}
	group := conversationBusiness.Ref{Kind: conversationBusiness.KindGroup, ID: "grp-1"}
	for _, c := range []struct {
		name      string
		conv      conversationBusiness.Ref
		delivered bool
		want      []uuid.UUID
	}{
		{"posted to the group", group, true, []uuid.UUID{maya, lee}},
		{"the group's post failed", group, false, nil},
		{"no conversation", conversationBusiness.Ref{}, false, nil},
	} {
		s := spyRecap(t)
		if !c.delivered {
			deliverRecapTo = func(context.Context, recapHome, []string, string, string, string, string) (string, error) {
				return "", fmt.Errorf("no author may post there")
			}
		}
		home := recapHome{home: callStateBusiness.Home{Ref: callStateBusiness.Ref{Kind: callStateBusiness.KindMeeting, ID: "1"}, Event: event},
			conv: c.conv, event: &calendarBusiness.CallPeople{Title: "Launch sync"},
			members: []*dgraphStruct.DgraphUser{{Uuid: maya.String()}, {Uuid: lee.String()}}}
		_, _ = deliverAndKeepRecap(context.Background(), call, home, false, []string{"uid-maya"}, "- We met.", "", nil, nil)
		if len(s.told) != 1 {
			t.Fatalf("%s: told %+v, want the event told once", c.name, s.told)
		}
		if got := s.told[0].InChat; fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%s: InChat = %v, want %v", c.name, got, c.want)
		}
	}
}

// The chat's recap names each action item's owner in bold, never as a plain
// "@Name": in a channel's post a plain "@Name" summons an agent of that name
// (business/AIAgent mentionMatches). It mentions nobody (no chip), and the
// owners it names are read back as the same items.
func TestARecapsActionItemsNameTheirOwnersWithoutAMention(t *testing.T) {
	recap := "**📝 Summary**\n- We ship Friday.\n\n**📋 Action Items**\n- @Maya Rao: send the deck (due 2026-11-14)\n* @Scout: draft the brief\n- Book the room"
	html := recapToHTML(recap, "", "", "")
	for _, at := range []string{"@Maya", "@Scout"} {
		if strings.Contains(html, at) {
			t.Errorf("the chat's recap says %q: %s", at, html)
		}
	}
	for _, want := range []string{"<strong>Maya Rao:</strong> send the deck (due 2026-11-14)", "<strong>Scout:</strong> draft the brief", "Book the room"} {
		if !strings.Contains(html, want) {
			t.Errorf("the chat's recap lacks %q: %s", want, html)
		}
	}
	if ids, _ := helpers.GetMentions(html); len(ids) != 0 {
		t.Errorf("the chat's recap mentions %v", ids)
	}
	if got, want := splitRecap(recapForChat(recap)).Items, splitRecap(recap).Items; fmt.Sprint(got) != fmt.Sprint(want) || len(got) != 3 || got[1].Owner != "Scout" {
		t.Errorf("read back %+v, want %+v", got, want)
	}
	// Only an action item's owner loses its "@": the rest is the model's.
	if got := recapForChat("- We met @noon: lunch"); got != "- We met @noon: lunch" {
		t.Errorf("recapForChat outside the action items = %q", got)
	}
}
