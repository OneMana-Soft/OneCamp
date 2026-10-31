package business

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	importModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	slackapi "github.com/akashc777/OneCamp/services/Slack"
	"github.com/google/uuid"
)

// A member's read reads only what is theirs: the kinds of conversation they
// bring (private channels, group DMs, DMs, never the public channels the
// team's import brought), and of those only the ones they are in. One Slack
// lists that they aren't in is left out unread and unnamed: its history is
// never asked for, and no note names it to them.
func TestAMembersReadReadsOnlyTheirOwnConversations(t *testing.T) {
	slack := acmeAPI() // its token's person, U1, isn't in the private channel G1 ("leads")
	var out bytes.Buffer
	report, err := ReadAPIExport(context.Background(), slack, APIReadOptions{Kinds: memberKinds, MembersOnly: true, MaxFileBytes: 1 << 20}, &out)
	if err != nil {
		t.Fatalf("the read: %v (calls %q)", err, slack.calls)
	}
	entries, _ := readZip(t, out.Bytes())
	for name, body := range entries {
		if strings.HasPrefix(name, "leads/") || strings.HasPrefix(name, "general/") || strings.Contains(body, `"leads"`) {
			t.Errorf("the export holds %s, which isn't theirs: %s", name, body)
		}
	}
	if strings.TrimSpace(entries["channels.json"]) != "[]" || strings.TrimSpace(entries["groups.json"]) != "[]" {
		t.Errorf("channels %s, groups %s, want none", entries["channels.json"], entries["groups.json"])
	}
	if !strings.Contains(entries["dms.json"], `"D1"`) || !strings.Contains(entries["mpims.json"], `"M1"`) {
		t.Errorf("their DM and group DM: dms %s, mpims %s", entries["dms.json"], entries["mpims.json"])
	}
	for _, call := range slack.calls {
		if strings.Contains(call, "types=public_channel") || (strings.HasPrefix(call, "conversations.history") && strings.Contains(call, "channel=G1")) ||
			strings.HasPrefix(call, "pins.list") && strings.Contains(call, "channel=G1") {
			t.Errorf("Slack was asked %q", call)
		}
	}
	for _, n := range report.Notes {
		if n.ID == "G1" || n.Name == "leads" {
			t.Errorf("a note names the conversation that isn't theirs: %+v", n)
		}
	}
	if report.Groups != 0 || report.DMs != 1 || report.MPIMs != 1 || report.Conversations != 2 || report.Of != 2 {
		t.Errorf("the read's report: %+v, want their DM and group DM, 2 of 2", report)
	}
}

// Whose token a member's read holds is settled before anything is read: an
// app's token is refused by its prefix, or by Slack's word that it is a
// bot's; anything that isn't a person's token is refused; and Slack refusing
// the token is said as Slack's refusal, in a sentence.
func TestAMembersReadRefusesATokenThatIsntAPersons(t *testing.T) {
	was := newAPIClient
	t.Cleanup(func() { newAPIClient = was })
	member := &userModels.UserInfo{UserPostgresInfo: userModels.User{Id: uuid.New(), EmailID: "bo@acme.test"}}
	start := func(token string, slack *fakeSlack) error {
		newAPIClient = func(string) apiClient { return slack }
		_, err := StartMemberRead(context.Background(), member, token, APIReadOptions{})
		return err
	}

	for token, want := range map[string]error{
		"xoxb-123-456-abc": ErrBotToken, " xoxb-1 ": ErrBotToken,
		"ghp_123": ErrNotAPersonsToken, "": ErrNotAPersonsToken, "xoxe-1": ErrNotAPersonsToken,
	} {
		slack := acmeAPI()
		if err := start(token, slack); !errors.Is(err, want) || len(slack.calls) != 0 {
			t.Errorf("%q: %v (Slack asked %q), want %v and Slack never asked", token, err, slack.calls, want)
		}
	}

	bot := acmeAPI()
	bot.team, bot.bot = "T1", "B1"
	if err := start("xoxp-looks-like-a-person", bot); !errors.Is(err, ErrBotToken) {
		t.Errorf("a bot's token Slack says is a bot's: %v", err)
	}

	refused := acmeAPI()
	refused.refuse = map[string]*slackapi.Error{"auth.test ": {Method: "auth.test", Code: "invalid_auth"}}
	err := start("xoxp-revoked", refused)
	code, msg, ok := MemberReadRefusal(err)
	if !ok || code != "slack_refused" || msg != "Slack didn't accept the token (invalid_auth). Check it, and start the read again." {
		t.Errorf("Slack refusing the token: %v → %q %q %v", err, code, msg, ok)
	}
}

// Every refusal of a member's read has a code and the member's own words,
// however it is wrapped; a failure of the server's own is not a refusal.
func TestAMembersReadSaysWhyItIsRefused(t *testing.T) {
	for _, c := range []struct {
		err  error
		code string
	}{
		{ErrNotAPersonsToken, "bad_token"}, {ErrBotToken, "bad_token"}, {ErrNotYourToken, "not_your_token"},
		{ErrNoEmail, "no_email"}, {ErrNoTeamImport, "no_team_import"}, {ErrOtherSlack, "other_slack"},
		{ErrMemberReadUnderWay, "under_way"}, {ErrNotYourRead, "not_yours"},
	} {
		code, msg, ok := MemberReadRefusal(fmt.Errorf("starting: %w", c.err))
		if !ok || code != c.code || msg != c.err.Error() {
			t.Errorf("%v: %q %q %v, want %q", c.err, code, msg, ok, c.code)
		}
		if !strings.HasSuffix(msg, ".") || strings.Contains(msg, "error") {
			t.Errorf("%q isn't a sentence a member can act on", msg)
		}
	}
	missing := fmt.Errorf("Slack wouldn't say whose token it is: %w", &slackapi.Error{Method: "users.info", Code: "missing_scope", Needed: "users:read.email"})
	if code, msg, ok := MemberReadRefusal(missing); !ok || code != "slack_refused" || !strings.Contains(msg, "users:read.email") {
		t.Errorf("a missing permission: %q %q %v", code, msg, ok)
	}
	if _, _, ok := MemberReadRefusal(errors.New("dial tcp: connection refused")); ok {
		t.Error("the server's own failure was taken for a refusal")
	}
}

// The team's import a member's read goes into is the one of their token's
// Slack workspace; before imports noted their workspace, the only one there
// is. Anything else is said, not guessed.
func TestAMembersReadFindsTheirTeamsImport(t *testing.T) {
	ws := func(pairs ...string) []importModels.SlackWorkspace {
		var out []importModels.SlackWorkspace
		for i := 0; i < len(pairs); i += 2 {
			out = append(out, importModels.SlackWorkspace{Label: pairs[i], Team: pairs[i+1]})
		}
		return out
	}
	for _, c := range []struct {
		name     string
		imported []importModels.SlackWorkspace
		team     string
		label    string
		err      error
	}{
		{"none imported", nil, "T1", "", ErrNoTeamImport},
		{"theirs among others", ws("Other", "T9", "Acme", "T1"), "T1", "Acme", nil},
		{"theirs imported again, newest first", ws("Acme", "T1", "Acme", ""), "T1", "Acme", nil},
		{"the only one, from before imports noted their team", ws("Acme", "", "Acme", ""), "T1", "Acme", nil},
		{"the only one is another workspace's", ws("Other", "T9"), "T1", "", ErrOtherSlack},
		{"two from before, which one can't be told", ws("Acme", "", "Other", ""), "T1", "", ErrOtherSlack},
	} {
		label, err := pickWorkspace(c.imported, c.team)
		if label != c.label || !errors.Is(err, c.err) {
			t.Errorf("%s: %q %v, want %q %v", c.name, label, err, c.label, c.err)
		}
	}
	for _, c := range []struct {
		a, b string
		same bool
	}{{"Bo@Acme.test", " bo@acme.test", true}, {"bo@acme.test", "ada@acme.test", false}, {"", "", false}, {" ", "bo@acme.test", false}} {
		if sameEmail(c.a, c.b) != c.same {
			t.Errorf("sameEmail(%q, %q) = %v", c.a, c.b, !c.same)
		}
	}
}
