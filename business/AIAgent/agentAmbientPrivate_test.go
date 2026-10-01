package business

import (
	"context"
	"strings"
	"testing"

	aiBusiness "github.com/akashc777/OneCamp/business/AI"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/google/uuid"
)

func TestAmbientDecision(t *testing.T) {
	cases := []struct {
		result, public, private string
	}{
		{"The deploy freeze starts Friday, per the pinned post.", "The deploy freeze starts Friday, per the pinned post.", ""},
		{"PRIVATE: #finance is renegotiating this vendor; worth checking before anyone signs.", "", "#finance is renegotiating this vendor; worth checking before anyone signs."},
		{"private:  lower-case prefix still counts", "", "lower-case prefix still counts"},
		{scheduledNothingSentinel, "", ""},
		{"", "", ""},
		{"Not private: a public reply that mentions the word later", "Not private: a public reply that mentions the word later", ""},
	}
	for _, c := range cases {
		pub, priv := ambientDecision(&RunOutcome{Result: c.result})
		if pub != c.public || priv != c.private {
			t.Errorf("%q: got (%q, %q) want (%q, %q)", c.result, pub, priv, c.public, c.private)
		}
	}
}

func TestAmbientPrivateNoteHTML(t *testing.T) {
	long := strings.Repeat("word ", 100)
	h := ambientPrivateNoteHTML("ch-1", "po-2", "<b>"+long, "watch <this>")
	for _, want := range []string{"watch &lt;this&gt;", "&lt;b&gt;", `href="/app/channel/ch-1/po-2"`, "…"} {
		if !strings.Contains(h, want) {
			t.Errorf("note lacks %q: %s", want, h)
		}
	}
	if strings.Contains(h, "<b>") || strings.Contains(h, "<this>") {
		t.Fatalf("message text must be escaped: %s", h)
	}
}

func TestPrivateNoteGoesToTheSponsorOnly(t *testing.T) {
	old := sendAgentDM
	t.Cleanup(func() { sendAgentDM = old })
	var to uuid.UUID
	var sent string
	sendAgentDM = func(_ context.Context, _ *userBusiness.BotIdentity, id uuid.UUID, h string) error {
		to, sent = id, h
		return nil
	}
	sponsor := uuid.New()
	a := testAgent()
	a.CreatedBy = sponsor
	deliverAmbientOutcome(context.Background(), a, &userBusiness.BotIdentity{}, "ch", "po", "the message", &RunOutcome{Result: "PRIVATE: only for you"})
	if to != sponsor || !strings.Contains(sent, "only for you") {
		t.Fatalf("private note went to %s: %q", to, sent)
	}
}

func TestRelatedElsewhere(t *testing.T) {
	hits := []aiBusiness.UnifiedHit{
		{ChannelUUID: "here", ChannelName: "eng", Snippet: "same channel, never shown"},
		{ChatGrpID: "dm-1", ChannelUUID: "x", Snippet: "a DM, never shown"},
		{ChannelUUID: "", Snippet: "a doc, not a conversation"},
		{ChannelUUID: "fin", ChannelName: "finance", Snippet: "We are  renegotiating the vendor contract"},
		{ChannelUUID: "fin", ChannelName: "finance", Snippet: "We are renegotiating the vendor contract"},
		{ChannelUUID: "ops", ChannelName: "#ops", Snippet: "Vendor outage postmortem"},
		{ChannelUUID: "sales", ChannelName: "", Snippet: "Vendor asked about pricing"},
		{ChannelUUID: "legal", ChannelName: "legal", Snippet: "one too many"},
	}
	got := relatedElsewhere(hits, "here", 3)
	want := []string{
		"#finance: We are renegotiating the vendor contract",
		"#ops: Vendor outage postmortem",
		"another channel: Vendor asked about pricing",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q", got)
	}
}

func TestAmbientPromptOffersOtherChannelsForPrivateUseOnly(t *testing.T) {
	plain := ambientRunPrompt("Should we sign the vendor renewal?", nil)
	if strings.Contains(plain, "other channels") {
		t.Fatal("no related discussions, no section")
	}
	withRelated := ambientRunPrompt("Should we sign the vendor renewal?", []string{"#finance: renegotiating the vendor"})
	for _, want := range []string{"#finance: renegotiating the vendor", ambientPrivatePrefix, "never in this channel"} {
		if !strings.Contains(withRelated, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
}
