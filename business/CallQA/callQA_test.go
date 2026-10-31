package business

import (
	"errors"
	"strings"
	"testing"
	"time"

	model "github.com/akashc777/OneCamp/models/postgres/CallQA"
	"github.com/google/uuid"
)

// A question is plain text, trimmed, and at most 300 characters.
func TestAQuestionIsShortPlainText(t *testing.T) {
	if got, err := Text("  When does it ship?\n"); err != nil || got != "When does it ship?" {
		t.Errorf("trimmed: %q %v", got, err)
	}
	var in InputError
	if _, err := Text("   "); !errors.As(err, &in) {
		t.Errorf("an empty question: %v", err)
	}
	if _, err := Text(strings.Repeat("é", MaxQuestion)); err != nil {
		t.Errorf("300 characters, counted as characters: %v", err)
	}
	if _, err := Text(strings.Repeat("a", MaxQuestion+1)); !errors.As(err, &in) {
		t.Errorf("301 characters: %v", err)
	}
}

func question(asker, name string, anonymous bool, state string, votes int, at int) *model.Question {
	return &model.Question{
		ID: uuid.New(), Asker: asker, AskerName: name, Anonymous: anonymous, Body: name + "'s question",
		State: state, Votes: votes, CreatedAt: time.Unix(int64(at), 0),
	}
}

// Nobody sees a dismissed question; with only answered ones shown, someone
// who doesn't run the call sees those and their own, anonymous or not; whoever
// runs it sees every open and answered one.
func TestWhoSeesWhichQuestions(t *testing.T) {
	rows := []*model.Question{
		question("0xben", "Ben", false, StateOpen, 0, 1),
		question("key-of-ben", "", true, StateOpen, 0, 2),
		question("guest-gia-1", "Guest: Gia", false, StateAnswered, 0, 3),
		question("0xjo", "Jo", false, StateDismissed, 9, 4),
		question("0xjo", "Jo", false, StateOpen, 0, 5),
	}
	ids := func(vs []View) []string {
		var out []string
		for _, v := range vs {
			out = append(out, v.Text)
		}
		return out
	}
	everyone := viewsOf(rows, "0xmaya", "key-of-maya", true, ShownAnswered)
	if len(everyone) != 4 {
		t.Errorf("whoever runs the call: %v", ids(everyone))
	}
	ben := viewsOf(rows, "0xben", "key-of-ben", false, ShownAnswered)
	yours := 0
	for _, v := range ben {
		if v.Yours {
			yours++
		}
		if v.Text == "Jo's question" {
			t.Error("Ben sees Jo's open question while only answered ones are shown")
		}
	}
	if len(ben) != 3 || yours != 2 {
		t.Errorf("Ben, answered only: his two and Gia's answered one, got %v (%d his)", ids(ben), yours)
	}
	all := viewsOf(rows, "0xben", "key-of-ben", false, ShownAll)
	if len(all) != 4 {
		t.Errorf("Ben, every question: %v", ids(all))
	}
	for _, v := range all {
		if v.Text == "Jo's question" && v.State == StateDismissed {
			t.Error("a dismissed question is shown")
		}
	}
}

// An anonymous question never shows a name, to anyone; its asker still sees
// it's theirs, and nobody else does.
func TestAnAnonymousQuestionHasNoName(t *testing.T) {
	rows := []*model.Question{question("key-of-ben", "", true, StateOpen, 0, 1)}
	for _, reader := range []struct {
		identity, key string
		runs, yours   bool
	}{
		{"0xben", "key-of-ben", false, true},
		{"0xmaya", "key-of-maya", true, false},
		{"guest-gia-1", "key-of-gia", false, false},
		{"", "", true, false},
	} {
		v := viewsOf(rows, reader.identity, reader.key, reader.runs, ShownAll)
		if len(v) != 1 || v[0].Asker != "" || !v[0].Anonymous || v[0].Yours != reader.yours {
			t.Errorf("as %q: %+v", reader.identity, v)
		}
	}
}

// Pinned questions come first, then the most upvoted, then the oldest.
func TestQuestionsAreInTheOrderTheRoomWants(t *testing.T) {
	pinned := question("0xa", "A", false, StateOpen, 0, 5)
	pinned.Pinned = true
	rows := []*model.Question{
		question("0xb", "B", false, StateOpen, 1, 1),
		question("0xc", "C", false, StateOpen, 3, 3),
		pinned,
		question("0xd", "D", false, StateOpen, 3, 2),
	}
	var got []string
	for _, v := range viewsOf(rows, "0xz", "", false, ShownAll) {
		got = append(got, v.Asker)
	}
	if strings.Join(got, "") != "ADCB" {
		t.Errorf("order: %v", got)
	}
}

// Someone's upvotes and their anonymous questions are kept under two keys
// that never match and never carry who they are, the same each time in a
// call and another in the next call; no key is made without the secret.
func TestUpvotesAndAnonymousAsksAreKeptUnderTheirOwnKeys(t *testing.T) {
	t.Setenv("JWT_SECRET", "a server's secret")
	call, next := uuid.New(), uuid.New()
	voter, err := keyOf(voterScope, call, "0xben")
	if err != nil {
		t.Fatal(err)
	}
	asker, _ := keyOf(askerScope, call, "0xben")
	again, _ := keyOf(voterScope, call, "0xben")
	nextCall, _ := keyOf(voterScope, next, "0xben")
	if voter == asker || voter != again || voter == nextCall || strings.Contains(voter+asker, "0xben") {
		t.Errorf("keys: voter %s, asker %s, again %s, next call %s", voter, asker, again, nextCall)
	}
	t.Setenv("JWT_SECRET", "")
	if _, err := keyOf(voterScope, call, "0xben"); err == nil {
		t.Error("a key made with no secret")
	}
}
