package business

import (
	"testing"

	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

func TestAPlaceholderAddressIsOneAnImportMadeUp(t *testing.T) {
	for email, made := range map[string]bool{
		"priya@acme.com":                              false,
		"Priya.Shah@Acme.COM":                         false,
		"asana-import-acme-123@no-reply.local":        true,
		"slack-import-acme-u024@no-reply.local":       true,
		"github+octocat@external.onecamp.local":       true,
		"priya+x-12345@acme.com":                      true,
		"priya+slack-u024@acme.com":                   true,
		"priya+design@acme.com":                       false,
		"":                                            true,
		"no-at-sign":                                  true,
		"@acme.com":                                   true,
		"priya@":                                      true,
		"  priya@acme.com  ":                          false,
		"trello-import-board-5f2@NO-REPLY.LOCAL":      true,
		"someone@external.onecamp.example.internal":   true,
		"someone@not-external.onecamp.local.acme.com": false,
	} {
		if got := IsPlaceholderEmail(email); got != made {
			t.Errorf("IsPlaceholderEmail(%q) = %v, want %v", email, got, made)
		}
	}
}

func TestTheOfferSaysWhoCanBeInvitedAndWhyTheRestCannot(t *testing.T) {
	placeholder := func(name, email string) importModels.ImportedPerson {
		return importModels.ImportedPerson{UserID: uuid.New(), Name: name, Email: email, IsExternal: true}
	}
	member := placeholder("Sam", "sam@acme.com")
	member.IsExternal = false
	invited := placeholder("Lee", "lee@acme.com")
	invited.Invited = true
	bot := placeholder("Deploy bot", "deploy@acme.com")
	bot.SourceBot = true
	botHere := placeholder("Notifier", "notifier@acme.com")
	botHere.IsBot = true
	leftSource := placeholder("Old timer", "old@acme.com")
	leftSource.SourceLeft = true
	deactivated := placeholder("Gone", "gone@acme.com")
	deactivated.Deactivated = true
	// The same mailbox under two source accounts is one person.
	twin := placeholder("Priya S.", "PRIYA@acme.com")

	got := buildImportPeople([]importModels.ImportedPerson{
		placeholder("Priya", "priya@acme.com"),
		twin,
		placeholder("", "jo@acme.com"),
		placeholder("Nomail", "trello-import-x-1@no-reply.local"),
		member, invited, bot, botHere, leftSource, deactivated,
	}, 20, 25)

	if len(got.People) != 2 {
		t.Fatalf("offered %d people, want Priya and jo: %+v", len(got.People), got.People)
	}
	if got.People[0].Email != "priya@acme.com" || got.People[0].Name != "Priya" {
		t.Errorf("first offered: %+v", got.People[0])
	}
	if got.People[1].Name != "jo" {
		t.Errorf("someone without a name is offered under their address's name, got %q", got.People[1].Name)
	}
	if got.AlreadyMembers != 1 || got.AlreadyInvited != 1 || got.NoEmail != 1 || got.Left != 2 {
		t.Errorf("counts: members %d invited %d no email %d left %d", got.AlreadyMembers, got.AlreadyInvited, got.NoEmail, got.Left)
	}
	if got.Seats.Used != 20 || got.Seats.Limit != 25 || got.Seats.Left == nil || *got.Seats.Left != 5 {
		t.Errorf("seats: %+v", got.Seats)
	}
}

func TestSeatRoomOnEveryPlan(t *testing.T) {
	if r := seatRoom(40, 0); r.Left != nil {
		t.Errorf("no limit leaves no count of seats left, got %d", *r.Left)
	}
	if r := seatRoom(25, 25); r.Left == nil || *r.Left != 0 {
		t.Errorf("a full plan has 0 left, got %+v", r)
	}
	// More people than the plan covers (a licence lowered under them) is
	// full, not negative.
	if r := seatRoom(30, 25); r.Left == nil || *r.Left != 0 {
		t.Errorf("over the limit has 0 left, got %+v", r)
	}
}

func TestAnEmptyImportOffersNobodyButStillAnswers(t *testing.T) {
	got := buildImportPeople(nil, 3, 25)
	if got.People == nil || len(got.People) != 0 {
		t.Errorf("people is an empty list, not null: %#v", got.People)
	}
}
