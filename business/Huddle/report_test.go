package business

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	callQABusiness "github.com/akashc777/OneCamp/business/CallQA"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	callThreadBusiness "github.com/akashc777/OneCamp/business/CallThread"
	pollBusiness "github.com/akashc777/OneCamp/business/Poll"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// A report lists each poll's counts, says where results were kept back and
// where votes were anonymous, and lists the questions by upvotes, answered or
// not, with the asker's name only where they gave it. People's words are
// escaped. A call with neither polls nor questions gets no report.
func TestAReportSaysWhatTheCallAsked(t *testing.T) {
	if got := reportHTML(nil, nil, nil); got != "" {
		t.Errorf("a call with nothing: %q", got)
	}
	polls := []*pollBusiness.View{
		{Question: "Ship on <Tuesday>?", Voters: 3, Options: []pollBusiness.OptionView{{Text: "Yes", Votes: 2}, {Text: "No", Votes: 1}}},
		{Question: "Rate the demo", Voters: 4, Anonymous: true, Options: []pollBusiness.OptionView{{Text: "Good", Votes: 4}}},
		{Question: "Who leads?", Voters: 5, ResultsHidden: true, Options: []pollBusiness.OptionView{{Text: "Kai", Votes: 5}}},
	}
	questions := []callQABusiness.View{
		{Text: "Is it <b>free</b>?", Votes: 3, State: callQABusiness.StateAnswered, Asker: "Guest: Gia"},
		{Text: "Will it scale?", Votes: 1, State: callQABusiness.StateOpen, Anonymous: true},
	}
	got := reportHTML(polls, questions, nil)
	for _, want := range []string{
		"Ship on &lt;Tuesday&gt;?", "<li>Yes: 2 votes</li>", "<li>No: 1 vote</li>", "3 people voted.",
		"4 people voted, anonymously.",
		"Who leads?</p><p>The results stayed with the people running the call.</p>",
		"Is it &lt;b&gt;free&lt;/b&gt;? (3 upvotes, answered, asked by Guest: Gia)",
		"Will it scale? (1 upvote)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the report lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Kai: 5") || strings.Contains(got, "<b>free</b>") {
		t.Errorf("held-back counts or unescaped words:\n%s", got)
	}
}

// A report lists the 25 most upvoted questions and counts the rest.
func TestAReportListsTheTopQuestions(t *testing.T) {
	var questions []callQABusiness.View
	for i := 0; i < reportQuestions+5; i++ {
		questions = append(questions, callQABusiness.View{Text: fmt.Sprintf("Question %d", i), State: callQABusiness.StateOpen})
	}
	got := reportHTML(nil, questions, nil)
	if strings.Count(got, "<li>") != reportQuestions || !strings.Contains(got, "And 5 more questions.") {
		t.Errorf("the top questions:\n%s", got)
	}
}

// The boards a call showed on its stage are linked from its report, by link
// alone: opening one follows the board's own rules, and its title stays with
// whoever may read it. A call that showed only a board gets a report too.
func TestAReportLinksTheBoardsTheCallShowed(t *testing.T) {
	first, second := uuid.New(), uuid.New()
	got := reportHTML(nil, nil, []uuid.UUID{first})
	if !strings.Contains(got, `<a href="/app/board/`+first.String()+`">Board from this call</a>`) {
		t.Errorf("one board:\n%s", got)
	}
	got = reportHTML(nil, nil, []uuid.UUID{first, second})
	if !strings.Contains(got, "Boards from this call") || strings.Count(got, `<a href="/app/board/`) != 2 {
		t.Errorf("two boards:\n%s", got)
	}

	writes, _, _ := fakeReport(t, true)
	reportSeams.polls = func(context.Context, uuid.UUID) ([]*pollBusiness.View, error) { return nil, nil }
	reportSeams.boards = func(context.Context, uuid.UUID) ([]uuid.UUID, error) { return []uuid.UUID{first}, nil }
	if err := report(context.Background(), endedCall()); err != nil || *writes != 1 {
		t.Fatalf("a call that showed only a board: writes %d, err %v", *writes, err)
	}
}

// fakeReport answers a report's reads with one poll, counts its claims, writes
// and marks, and grants claims from claims in turn.
func fakeReport(t *testing.T, claims ...bool) (writes, marks *int, writeErr *error) {
	t.Helper()
	old := reportSeams
	t.Cleanup(func() { reportSeams = old })
	fake(t, nil)
	var w, m int
	var werr error
	reportSeams.claim = func(context.Context, uuid.UUID, time.Duration) (bool, error) {
		if len(claims) == 0 {
			return false, nil
		}
		c := claims[0]
		claims = claims[1:]
		return c, nil
	}
	reportSeams.done = func(context.Context, uuid.UUID) error { m++; return nil }
	reportSeams.polls = func(context.Context, uuid.UUID) ([]*pollBusiness.View, error) {
		return []*pollBusiness.View{{Question: "Which day?", Options: []pollBusiness.OptionView{{Text: "Tue", Votes: 1}}, Voters: 1}}, nil
	}
	reportSeams.questions = func(context.Context, uuid.UUID, string) ([]callQABusiness.View, error) { return nil, nil }
	reportSeams.boards = func(context.Context, uuid.UUID) ([]uuid.UUID, error) { return nil, nil }
	reportSeams.write = func(context.Context, *userModels.UserInfo, *callStateBusiness.Session, string) error {
		if werr != nil {
			return werr
		}
		w++
		return nil
	}
	return &w, &m, &werr
}

func endedCall() *callStateBusiness.Session {
	ended := time.Now()
	return &callStateBusiness.Session{ID: uuid.NewString(), StartedBy: uuid.NewString(), PostUUID: uuid.NewString(), EndedAt: &ended, Resource: callStateBusiness.Ref{Kind: "channel"}}
}

// A call ended twice (the end's sink, then the sweep, or a host's double end)
// gets one report: the second finds the claim taken or the report written.
func TestAReportIsWrittenOnce(t *testing.T) {
	writes, marks, _ := fakeReport(t, true, false)
	s := endedCall()
	for i := 0; i < 2; i++ {
		if err := report(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}
	if *writes != 1 || *marks != 1 {
		t.Errorf("writes %d, marks %d", *writes, *marks)
	}
}

// A write that failed isn't marked written, so the claim lapses and the sweep
// writes it; a starter who may no longer reply there leaves it nowhere to go,
// which is said and marked, never tried for ever.
func TestAReportThatFailedIsTriedAgain(t *testing.T) {
	writes, marks, writeErr := fakeReport(t, true, true, true)
	s := endedCall()
	*writeErr = errors.New("dgraph is down")
	if err := report(context.Background(), s); err == nil || *marks != 0 {
		t.Errorf("a failed write: %v, marks %d", err, *marks)
	}
	*writeErr = nil
	if err := report(context.Background(), s); err != nil || *writes != 1 || *marks != 1 {
		t.Errorf("the sweep's try: %v, writes %d, marks %d", err, *writes, *marks)
	}
	*writeErr = callThreadBusiness.ErrReadOnly
	if err := report(context.Background(), s); err != nil || *marks != 2 {
		t.Errorf("nowhere to go: %v, marks %d", err, *marks)
	}
}

// A live call, or one with no post, has no report to write yet or ever.
func TestOnlyAnEndedCallWithAPostIsReported(t *testing.T) {
	writes, marks, _ := fakeReport(t, true, true)
	live := endedCall()
	live.Live, live.EndedAt = true, nil
	postless := endedCall()
	postless.PostUUID = ""
	for _, s := range []*callStateBusiness.Session{live, postless} {
		if err := report(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}
	if *writes != 0 || *marks != 0 {
		t.Errorf("writes %d, marks %d", *writes, *marks)
	}
}
