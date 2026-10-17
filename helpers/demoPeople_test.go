package helpers

import (
	"slices"
	"testing"
)

type person struct {
	email string
	bot   bool
}

func personEmail(p person) string { return p.email }
func personBot(p person) bool     { return p.bot }

// On the live demo the visitor's @mention popup listed the demo's real
// members beside the seeded cast. Every list the visitor picks people from
// now holds only the demo's own people.
func TestTheDemoVisitorListsOnlyTheDemosOwnPeople(t *testing.T) {
	t.Setenv("DEMO_MODE", "true")
	t.Setenv("DEMO_USER_EMAIL", testVisitor)
	people := []person{
		{email: testOwner},                         // a real member: out
		{email: "maya.chen" + DemoCastEmailDomain}, // the cast: in
		{email: "Visitor@Demo.Example"},            // the visitor: in
		{email: "agent-bot-1" + BotEmailDomain},    // a bot by its address: in
		{email: "", bot: true},                     // a bot by its flag: in
		{email: ""},                                // unknown: out
		{email: "someone@cast.demo.onemana.dev.example.com"},
	}
	got := DemoPeopleOnly(testVisitor, people, personEmail, personBot)
	want := []person{people[1], people[2], people[3], people[4]}
	if !slices.Equal(got, want) {
		t.Errorf("the visitor's list: got %v, want %v", got, want)
	}
	if &got[0] == &people[1] {
		t.Error("the list was filtered in place: a cached list would lose people for everyone")
	}
	// The people who run the demo, signed in as themselves, see everyone.
	if got := DemoPeopleOnly(testOwner, people, personEmail, personBot); len(got) != len(people) {
		t.Errorf("a demo member's list lost people: %v", got)
	}
}

func TestEveryOtherServerListsEveryone(t *testing.T) {
	t.Setenv("DEMO_MODE", "")
	t.Setenv("DEMO_USER_EMAIL", testVisitor)
	people := []person{{email: testOwner}, {email: ""}}
	got := DemoPeopleOnly(testVisitor, people, personEmail, personBot)
	if len(got) != 2 || &got[0] != &people[0] {
		t.Errorf("a server that is not the demo changed a list: %v", got)
	}
	if !DemoListsPerson(testVisitor, testOwner, false) {
		t.Error("off the demo, everyone is listed")
	}
}
