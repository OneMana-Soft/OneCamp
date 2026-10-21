package domain

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	userDomain "github.com/akashc777/OneCamp/domain/User"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

var t0 = time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)

func person(id, name string) *dgraphStruct.DgraphUser {
	return &dgraphStruct.DgraphUser{Uuid: id, UserName: name, UserFullName: name, EmailID: name + "@acme.test"}
}

var (
	maya   = person("u-maya", "Maya Chen")
	jonas  = person("u-jonas", "Jonas Weber")
	sam    = person("u-sam", "Sam Rivera")
	ada    = person("u-ada", "Ada Lovelace")
	guests = &dgraphStruct.DgraphUser{Uuid: "u-guests", UserName: "Guests", IsBot: true, EmailID: userDomain.ChannelGuestBotEmail}
	slack  = &dgraphStruct.DgraphUser{Uuid: "u-slack", UserName: "Slack", IsBot: true, EmailID: userDomain.SlackBridgeBotEmail}
	agent  = &dgraphStruct.DgraphUser{Uuid: "u-agent", UserName: "Scout", IsBot: true, EmailID: userDomain.AgentBotPrefix + "scout" + userDomain.BotEmailDomain}
)

// reply is a reply by who, at minute m past 10:00.
func reply(id string, who *dgraphStruct.DgraphUser, m int) *dgraphStruct.DgraphComment {
	at := t0.Add(time.Duration(m) * time.Minute)
	return &dgraphStruct.DgraphComment{Uuid: id, CreatedAt: &at, CommentBy: who}
}

func names(cs []*dgraphStruct.DgraphComment) []string {
	var out []string
	for _, c := range cs {
		n := c.CommentBy.UserName
		if c.Text != "" {
			n += " " + c.Text
		}
		out = append(out, n)
	}
	return out
}

func withTexts(t *testing.T, texts map[string]string, err error) *[]string {
	t.Helper()
	var asked []string
	old := relayTexts
	t.Cleanup(func() { relayTexts = old })
	relayTexts = func(_ context.Context, ids []string) (map[string]string, error) {
		asked = append(asked, ids...)
		return texts, err
	}
	return &asked
}

// A thread's faces are up to three people, each once, in the order they last
// replied: the last is the thread's latest reply, which "Last reply" reads.
func TestAThreadsFacesAreThreePeopleEachOnce(t *testing.T) {
	asked := withTexts(t, nil, nil)
	replies := []*dgraphStruct.DgraphComment{
		reply("c5", maya, 5), reply("c4", jonas, 4), reply("c3", maya, 3), reply("c2", sam, 2), reply("c1", ada, 1),
	}
	KeepRepliers(context.Background(), &replies)
	if got := names(replies); !slices.Equal(got, []string{"Sam Rivera", "Jonas Weber", "Maya Chen"}) {
		t.Fatalf("faces %v: want Sam, Jonas and Maya, Maya once, Ada past three", got)
	}
	if last := replies[len(replies)-1]; last.Uuid != "c5" || !last.CreatedAt.Equal(t0.Add(5*time.Minute)) {
		t.Errorf("last face %s at %v: want Maya's latest reply, the thread's", last.Uuid, last.CreatedAt)
	}
	for _, c := range replies {
		if c.CommentBy.EmailID != "" || c.CommentBy.UserFullName == "" {
			t.Errorf("%s's face: %+v (an email leaves the server, or the name doesn't)", c.CommentBy.UserName, c.CommentBy)
		}
	}
	if len(*asked) != 0 {
		t.Errorf("read the words of %v: nobody relays here", *asked)
	}
	// One person, three replies: one face.
	one := []*dgraphStruct.DgraphComment{reply("a3", jonas, 3), reply("a2", jonas, 2), reply("a1", jonas, 1)}
	KeepRepliers(context.Background(), &one)
	if got := names(one); !slices.Equal(got, []string{"Jonas Weber"}) || one[0].Uuid != "a3" {
		t.Errorf("one person's three replies: %v", got)
	}
	// No replies: none.
	var none []*dgraphStruct.DgraphComment
	KeepRepliers(context.Background(), &none)
	if len(none) != 0 {
		t.Errorf("faces for no replies: %v", none)
	}
}

// A reply the Guests or Slack bot posted is its person's: two guests are two
// faces and neither is the bot's, and what the page carries of their words is
// the label naming them, which the web's relay rule reads.
func TestARelayedReplyIsItsPersonsFace(t *testing.T) {
	asked := withTexts(t, map[string]string{
		"g3": "<p><strong>[Priya (Acme) (guest)]</strong></p><p>Looks good to us</p>",
		"g2": "<p><strong>[Ana &amp; Co (guest)]</strong></p><p>Same here</p>",
		"g1": "<p><strong>[Priya (Acme) (guest)]</strong></p><p>Earlier</p>",
		"s1": "<p><strong>[Ana Ruiz]</strong> On it</p>",
	}, nil)
	replies := []*dgraphStruct.DgraphComment{reply("g3", guests, 9), reply("g2", guests, 8), reply("g1", guests, 7), reply("m1", maya, 6), reply("x1", agent, 5)}
	slackThread := []*dgraphStruct.DgraphComment{reply("s1", slack, 4)}
	KeepRepliers(context.Background(), &replies, &slackThread)
	want := []string{
		"Maya Chen",
		"Guests <p><strong>[Ana &amp; Co (guest)]</strong></p>",
		"Guests <p><strong>[Priya (Acme) (guest)]</strong></p>",
	}
	if got := names(replies); !slices.Equal(got, want) {
		t.Fatalf("faces %q, want %q", got, want)
	}
	if got := names(slackThread); !slices.Equal(got, []string{"Slack <p><strong>[Ana Ruiz]</strong></p>"}) {
		t.Errorf("a Slack person's face %q", got)
	}
	for _, c := range append(replies, slackThread...) {
		if strings.Contains(c.Text, "Looks good") || strings.Contains(c.Text, "On it") {
			t.Errorf("a reply's words left the server: %q", c.Text)
		}
	}
	slices.Sort(*asked)
	if !slices.Equal(*asked, []string{"g1", "g2", "g3", "s1"}) {
		t.Errorf("read the words of %v: want the relayed replies only, never Maya's or the agent's", *asked)
	}
}

// When the relayed replies' words can't be read, they count as their bot's:
// the faces still show, and the page still answers.
func TestUnreadableRelaysCountAsTheirBot(t *testing.T) {
	withTexts(t, nil, errors.New("graph down"))
	replies := []*dgraphStruct.DgraphComment{reply("g2", guests, 2), reply("g1", guests, 1), reply("m1", maya, 0)}
	KeepRepliers(context.Background(), &replies)
	if got := names(replies); !slices.Equal(got, []string{"Maya Chen", "Guests"}) {
		t.Errorf("faces %v", got)
	}
}

// The label, as botpost.LabelledHTML writes it: a paragraph of its own, or
// inline before plain text; anything else has none.
func TestRelayLabel(t *testing.T) {
	for in, want := range map[string]string{
		"<p><strong>[Priya (Acme) (guest)]</strong></p><p>Hi</p>": "<p><strong>[Priya (Acme) (guest)]</strong></p>",
		"  <p> <strong>[Ana Ruiz]</strong> On it</p>":             "<p><strong>[Ana Ruiz]</strong></p>",
		"<P><STRONG>[A guest]</STRONG></P>":                       "<p><strong>[A guest]</strong></p>",
	} {
		if got, ok := RelayLabel(in); !ok || got != want {
			t.Errorf("RelayLabel(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "<p>Hi [there]</p>", "<p><strong>Bold</strong> [x]</p>", "<p>[Ana]</p>"} {
		if got, ok := RelayLabel(in); ok {
			t.Errorf("RelayLabel(%q) = %q: no label", in, got)
		}
	}
	if got := relayName("<p><strong>[Ana &amp;  Co (guest)]</strong></p>"); got != "Ana & Co (guest)" {
		t.Errorf("relayName = %q", got)
	}
}

// Every page query selects the same of each message's replies: the newest
// live ones, their ids, times and authors, never their words.
func TestRepliersSelection(t *testing.T) {
	s := RepliersSelection("chat_comments")
	for _, want := range []string{"chat_comments (orderdesc: comment_created_at, first: 10)", `not gt(comment_deleted_at, "1970-01-01T00:00:00Z")`, "comment_uuid", "comment_created_at", "comment_by", "user_email_id"} {
		if !strings.Contains(s, want) {
			t.Errorf("the selection lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "comment_text") {
		t.Errorf("the selection reads every reply's words:\n%s", s)
	}
}
