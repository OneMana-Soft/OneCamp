package business

import (
	"errors"
	"strings"
	"testing"
	"time"

	pollModel "github.com/akashc777/OneCamp/models/postgres/Poll"
	"github.com/google/uuid"
)

func TestNormalizeKeepsOnlyRealOptions(t *testing.T) {
	n, err := NewPoll{Question: "  Lunch?  ", Options: []string{" Pizza ", "pizza", "", "Sushi"}}.Normalize()
	if err != nil || n.Question != "Lunch?" || len(n.Options) != 2 || n.Options[0] != "Pizza" {
		t.Fatalf("got %+v err=%v", n, err)
	}
	bad := []NewPoll{
		{Question: "", Options: []string{"a", "b"}},
		{Question: "q", Options: []string{"a", "A"}},
		{Question: "q", Options: strings.Split("a,b,c,d,e,f,g,h,i,j,k", ",")},
		{Question: strings.Repeat("x", MaxQuestion+1), Options: []string{"a", "b"}},
		{Question: "q", Options: []string{"a", "b"}, OpenHours: MaxOpenHours + 1},
	}
	for i, b := range bad {
		var ie InputError
		if _, err := b.Normalize(); !errors.As(err, &ie) {
			t.Errorf("case %d: want an input error, got %v", i, err)
		}
	}
}

func TestSplitOptionsKeepsCommas(t *testing.T) {
	got := SplitOptions("Tue, 10am | Wed, 2pm\nThu")
	if len(got) != 3 || got[0] != "Tue, 10am " {
		t.Fatalf("got %q", got)
	}
}

func TestVoteRules(t *testing.T) {
	now := time.Now()
	single := &pollModel.Poll{Options: []pollModel.Option{{ID: "1"}, {ID: "2"}}}
	if got, err := ValidateChoice(single, []string{"2", "2"}, now); err != nil || len(got) != 1 {
		t.Errorf("a repeated choice counts once: %v %v", got, err)
	}
	if _, err := ValidateChoice(single, []string{"1", "2"}, now); err == nil {
		t.Error("a single-choice poll took two")
	}
	if _, err := ValidateChoice(single, []string{"9"}, now); err == nil {
		t.Error("an option outside the poll was accepted")
	}
	if got, err := ValidateChoice(single, nil, now); err != nil || len(got) != 0 {
		t.Error("an empty choice must retract the vote")
	}
	multi := &pollModel.Poll{Multiple: true, Options: single.Options}
	if got, _ := ValidateChoice(multi, []string{"1", "2"}, now); len(got) != 2 {
		t.Error("a multiple-choice poll refused two")
	}
	past := now.Add(-time.Minute)
	if _, err := ValidateChoice(&pollModel.Poll{Options: single.Options, ClosesAt: &past}, []string{"1"}, now); err == nil {
		t.Error("a poll past its closing time took a vote")
	}
	if _, err := ValidateChoice(&pollModel.Poll{Options: single.Options, ClosedAt: &past}, []string{"1"}, now); err == nil {
		t.Error("a closed poll took a vote")
	}
}

func TestPollHTMLEscapesTheQuestion(t *testing.T) {
	id := uuid.New()
	h := PollHTML(id, `<img src=x onerror=alert(1)>`)
	if strings.Contains(h, "<img") || !strings.Contains(h, `data-type="poll" data-id="`+id.String()+`"`) {
		t.Fatalf("got %s", h)
	}
}
