package business

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	templateBusiness "github.com/akashc777/OneCamp/business/ProjectTemplate"
	ai "github.com/akashc777/OneCamp/services/AI"
)

func TestPlanFromModel(t *testing.T) {
	good := "```json\n" + `{"name":"Mobile app launch","description":"Ship v1 to both stores.",
	 "statuses":[{"name":"QA","category":"inReview"},{"name":"Done","category":"done"},{"name":"qa","category":"todo"},{"name":"Beta","category":"inProgress"},{"name":"Extra","category":"todo"}],
	 "tasks":[{"name":"Write the store listing","details":"Title, screenshots.\n- Icon\n- Video","priority":"HIGH","start_day":2,"due_day":1,"steps":["Screenshots"," ","Copy"]},
	          {"name":"  ","details":"no name"},
	          {"name":"Submit for review","priority":"urgent","due_day":99999}]}` + "\n```"
	tpl, ok := planFromModel(good, "fallback")
	if !ok {
		t.Fatal("a fenced, slightly wrong plan is mended, not refused")
	}
	if tpl.Name != "Mobile app launch" || tpl.Description != "Ship v1 to both stores." {
		t.Errorf("name %q, description %q", tpl.Name, tpl.Description)
	}
	if len(tpl.Statuses) != 2 || tpl.Statuses[0].Name != "QA" || tpl.Statuses[1].Name != "Beta" {
		t.Errorf("at most two statuses, none clashing with a built-in or each other: %+v", tpl.Statuses)
	}
	if len(tpl.Tasks) != 2 {
		t.Fatalf("unnamed tasks are dropped: %+v", tpl.Tasks)
	}
	first, second := tpl.Tasks[0], tpl.Tasks[1]
	if first.Priority != "high" || first.StartDay != nil || first.DueDay == nil || *first.DueDay != 1 {
		t.Errorf("priority read in any case; a start after the due day dropped: %+v", first)
	}
	if !strings.Contains(first.Description, "<li>Icon</li>") || len(first.Subtasks) != 2 {
		t.Errorf("details become the editor's HTML, blank steps are dropped: %q %+v", first.Description, first.Subtasks)
	}
	if second.Priority != "" || *second.DueDay != 3650 {
		t.Errorf("an unknown priority is left out and a far day kept in range: %+v", second)
	}

	if tpl, ok := planFromModel(`{"tasks":[{"name":"One"}]}`, "Launch the app"); !ok || tpl.Name != "Launch the app" {
		t.Errorf("a plan without a name takes the person's words: %q %v", tpl.Name, ok)
	}
	for _, bad := range []string{"", "no json here", `{"name":"x"}`, `{"tasks":[]}`, `{"tasks":[{"name":""}]}`, `[{"name":"a"}]`} {
		if _, ok := planFromModel(bad, "x"); ok {
			t.Errorf("%q was taken for a plan", bad)
		}
	}
}

// The answer is JSON, not prose for a person: words that the chat cleaner
// removes from answers (UUID, /send, tags) are part of a plan here.
func TestPlanFromModelKeepsWhatAChatAnswerWouldLose(t *testing.T) {
	out := `{"name":"Device fleet","tasks":[{"name":"Give each device a UUID on first launch","details":"Build the /send endpoint and the <code> guide."},{"name":"Ship it"}]}`
	tpl, ok := planFromModel(out, "x")
	if !ok || len(tpl.Tasks) != 2 || tpl.Tasks[0].Name != "Give each device a UUID on first launch" ||
		!strings.Contains(tpl.Tasks[0].Description, "/send endpoint") || !strings.Contains(tpl.Tasks[0].Description, "&lt;code&gt;") {
		t.Fatalf("%v %+v", ok, tpl.Tasks)
	}
	if _, ok := planFromModel("<think>plan it</think>"+out, "x"); !ok {
		t.Error("a model's thinking before the answer is set aside")
	}
}

func TestTimeframeDays(t *testing.T) {
	for in, want := range map[string]int{
		"Launch our mobile app in six weeks, with a beta for 50 users first": 42,
		"a 3-month migration":                          0 + 90,
		"over 10 days":                                 10,
		"Plan the offsite in a fortnight":              14,
		"two weeks of testing, then launch in 6 weeks": 42,
		"Migrate the database to Postgres":             0,
		"a beta for 50 users":                          0,
		"ship in 999 months":                           templateBusiness.MaxDay,
	} {
		if got := timeframeDays(in); got != want {
			t.Errorf("%q: %d, want %d", in, got, want)
		}
	}
}

func TestFitTimeframe(t *testing.T) {
	d := func(n int) *int { return &n }
	plan := func() templateBusiness.Template {
		return templateBusiness.Template{Tasks: []templateBusiness.Task{
			{Name: "a", StartDay: d(0), DueDay: d(7)},
			{Name: "b", DueDay: d(16), Subtasks: []templateBusiness.Subtask{{Name: "s", DueDay: d(8)}, {Name: "u"}}},
			{Name: "c"},
		}}
	}
	six := plan()
	fitTimeframe(&six, 42)
	if *six.Tasks[0].StartDay != 0 || *six.Tasks[0].DueDay != 18 || *six.Tasks[1].DueDay != 42 || *six.Tasks[1].Subtasks[0].DueDay != 21 {
		t.Errorf("a plan ending on day 16 for six weeks is stretched to day 42: %+v", six.Tasks)
	}
	if six.Tasks[1].Subtasks[1].DueDay != nil || six.Tasks[2].DueDay != nil {
		t.Error("an undated task or step stays undated")
	}
	for _, days := range []int{0, 1, 20, 12} {
		p := plan()
		fitTimeframe(&p, days)
		if *p.Tasks[1].DueDay != 16 {
			t.Errorf("no timeframe, or one the plan already fits (%d days), changes nothing: %d", days, *p.Tasks[1].DueDay)
		}
	}
	short := plan()
	fitTimeframe(&short, 5)
	if *short.Tasks[1].DueDay != 5 || *short.Tasks[0].DueDay != 2 {
		t.Errorf("a plan far longer than the timeframe is squeezed: %+v", short.Tasks)
	}
}

func TestPlanFailure(t *testing.T) {
	for err, want := range map[error]string{
		ai.ErrRateLimited:                          "a lot of AI requests",
		fmt.Errorf("x: %w", ai.ErrCircuitOpen):     "isn't answering",
		context.DeadlineExceeded:                   "took too long",
		errors.New("the model said something odd"): "couldn't be drafted",
	} {
		if got := planFailure(err); !strings.Contains(got, want) {
			t.Errorf("%v: %q", err, got)
		}
	}
}
