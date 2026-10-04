package business

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

func TestCanReceiveDirectMessageAllowsAnOrdinaryMember(t *testing.T) {
	got := CanReceiveDirectMessage(liveUser())
	if !got.Allowed {
		t.Fatalf("an ordinary active member must be able to receive a DM, got: %s", got.Reason)
	}
	if strings.TrimSpace(got.Reason) == "" {
		t.Error("reason is empty; the refusal text and the audit record both read it")
	}
}

// THE CASE THAT MAKES THIS A SEPARATE FUNCTION FROM Assess. The shared automation bot is
// is_external by class and is intentionally messageable, because a DM to it is answered by
// the AI coworker. Assess refuses bots — correctly, since a bot cannot authorize work —
// so reusing Assess here would silently break a shipped feature.
func TestABotCanReceiveADirectMessageEvenThoughItCannotAuthorizeWork(t *testing.T) {
	bot := liveUser()
	bot.IsBot = true
	bot.IsExternal = true

	if got := CanReceiveDirectMessage(bot); !got.Allowed {
		t.Fatalf("the automation bot must be messageable — a DM to it is answered by the "+
			"AI coworker. Got: %s", got.Reason)
	}
	// And the two functions must genuinely disagree, or one of them is redundant.
	if got := Assess(bot); got.Allowed {
		t.Fatal("Assess now permits a bot to authorize work; these two functions exist " +
			"because they differ on exactly this case")
	}
}

func TestCanReceiveDirectMessageRefusesUnreachableIdentities(t *testing.T) {
	deleted := time.Now().UTC()
	zero := time.Time{}.UTC()

	cases := []struct {
		name string
		user *dgraphStruct.DgraphUser
		want string // substring the reason must contain
	}{
		{"nil", nil, "could not be resolved"},
		{"no uid", &dgraphStruct.DgraphUser{Uuid: "u"}, "could not be resolved"},
		{"blank uid", &dgraphStruct.DgraphUser{Uid: "   "}, "could not be resolved"},
		{"deactivated", &dgraphStruct.DgraphUser{Uid: "0x1", DeletedAt: &deleted}, "deactivated"},
		{"external ghost", &dgraphStruct.DgraphUser{Uid: "0x1", IsExternal: true}, "external"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CanReceiveDirectMessage(tc.user)
			if got.Allowed {
				t.Fatalf("%s must not be able to receive a DM", tc.name)
			}
			if !strings.Contains(got.Reason, tc.want) {
				t.Errorf("reason %q does not explain the refusal (wanted %q)", got.Reason, tc.want)
			}
		})
	}

	// A reactivated account writes the Dgraph zero time rather than clearing the field,
	// so a naive nil check would read it as deleted. Same trap Assess documents.
	reactivated := &dgraphStruct.DgraphUser{Uid: "0x1", DeletedAt: &zero}
	if got := CanReceiveDirectMessage(reactivated); !got.Allowed {
		t.Fatalf("a reactivated member must be messageable again; the zero timestamp is "+
			"not a deletion. Got: %s", got.Reason)
	}
}

// A DEACTIVATED BOT IS STILL REFUSED. The bot exception is about is_external, not a
// blanket pass — otherwise deactivating a bot would not stop messages reaching it.
func TestADeactivatedBotIsStillRefused(t *testing.T) {
	deleted := time.Now().UTC()
	bot := &dgraphStruct.DgraphUser{Uid: "0x1", IsBot: true, IsExternal: true, DeletedAt: &deleted}

	if got := CanReceiveDirectMessage(bot); got.Allowed {
		t.Fatal("a deactivated bot was allowed to receive messages; the external exception " +
			"must not override deactivation")
	}
}

// THE ANTI-DIVERGENCE RATCHET. Three paths can send a direct message, and before this
// function existed they did not agree: the HTTP controller refused an external
// attribution identity, and the AI executor — which is the same code the MCP surface
// reuses — checked only resolution and soft-deletion. So an agent could DM a ghost
// identity that a person using the app was refused.
//
// That divergence was invisible precisely because both paths looked like they were
// checking the recipient. A test that only exercised the function would not have caught
// it; what has to be asserted is that every path ASKS.
func TestEverySendPathAsksTheOneRecipientRule(t *testing.T) {
	for _, target := range []struct {
		file string
		why  string
	}{
		// The app's send path (and scheduled messages) since the rules moved out
		// of the controller; the next test checks the controller still uses it.
		{"../../business/Send/send.go",
			"a person using the app (now, or in a scheduled message) could DM an identity that cannot read it"},
		{"../../business/AI/aiExecutors.go",
			"the in-app AI (and the MCP surface, which reuses this executor) could DM an " +
				"identity the app refuses"},
		{"../../business/MCPServer/reach.go",
			"an external agent's send_dm would be authorised without checking the recipient"},
	} {
		raw, err := os.ReadFile(target.file)
		if err != nil {
			// In the AI-free edition these packages are removed; the rule is still
			// enforced for the paths that remain.
			t.Skipf("read %s: %v", target.file, err)
		}
		if !strings.Contains(string(raw), "CanReceiveDirectMessage(") {
			t.Errorf("%s does not call CanReceiveDirectMessage: %s", target.file, target.why)
		}
	}
}

// The HTTP handler must reach the recipient rule through business/Send, not
// around it: a direct call to the create function would skip every check.
func TestTheAppDMHandlerSendsThroughTheSharedRules(t *testing.T) {
	raw, err := os.ReadFile("../../controllers/Chat/chatController.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	if !strings.Contains(src, "sendBusiness.PrepareDirectMessage(") {
		t.Error("controllers/Chat no longer prepares DMs through business/Send, so the recipient rule may be skipped")
	}
	if strings.Contains(src, "business.CreateChat(") {
		t.Error("controllers/Chat calls CreateChat directly, around the recipient rule in business/Send")
	}
}

// The old inline rule must be GONE from the callers, not merely supplemented. Leaving a
// hand-written copy beside the shared call is how the two start disagreeing again — the
// copy gets edited and the shared one does not.
func TestTheInlineRecipientRuleIsNotDuplicated(t *testing.T) {
	// The distinctive shape of the old check: the external test with its bot exception.
	inline := regexp.MustCompile(`IsExternal\s*&&\s*!\w*\.?IsBot`)

	for _, file := range []string{
		"../../controllers/Chat/chatController.go",
		"../../business/AI/aiExecutors.go",
		"../../business/MCPServer/reach.go",
	} {
		raw, err := os.ReadFile(file)
		if err != nil {
			// In the AI-free edition these packages are removed; no inline rule can
			// survive in code that no longer ships.
			t.Skipf("read %s: %v", file, err)
		}
		// Comments are stripped: the reasoning legitimately describes the rule in prose.
		src := regexp.MustCompile(`//[^\n]*`).ReplaceAllString(string(raw), " ")
		if loc := inline.FindString(src); loc != "" {
			t.Errorf("%s still carries an inline copy of the recipient rule (%q). Delete it "+
				"and rely on CanReceiveDirectMessage, or the two will drift apart again",
				file, loc)
		}
	}
}
