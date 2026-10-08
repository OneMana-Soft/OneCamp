package business

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	taskFieldBusiness "github.com/akashc777/OneCamp/business/TaskField"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	statusModel "github.com/akashc777/OneCamp/models/postgres/TaskStatus"
)

func TestBuiltInsAreUsable(t *testing.T) {
	seen := map[string]bool{}
	for _, b := range builtins {
		// Links name a built-in by its id (?new=client-project, the template
		// pages on onemana.dev), and the app takes one only in this shape.
		if !regexp.MustCompile(`^[a-z][a-z0-9-]{0,59}$`).MatchString(b.ID) || seen[b.ID] {
			t.Fatalf("built-in %q needs a unique lowercase id that starts with a letter: %q", b.Name, b.ID)
		}
		seen[b.ID] = true
		checked, err := Check(b)
		if err != nil {
			t.Fatalf("%s: %v", b.ID, err)
		}
		if len(checked.Tasks) < 5 || len(b.Description) == 0 || len(b.Description) > MaxAboutLength {
			t.Errorf("%s: %d tasks, description %q", b.ID, len(checked.Tasks), b.Description)
		}
		for i, task := range checked.Tasks {
			if task.DueDay == nil {
				t.Errorf("%s: %q has no date, so it would sit apart from the plan", b.ID, task.Name)
			}
			if task.Priority == "" {
				t.Errorf("%s: %q has no priority", b.ID, task.Name)
			}
			// The template pages on onemana.dev promise every task says what
			// done looks like.
			if task.Description == "" {
				t.Errorf("%s: %q doesn't say what done looks like", b.ID, task.Name)
			}
			// A plan reads in order: nothing is due before the task above it.
			if i > 0 && checked.Tasks[i-1].DueDay != nil && task.DueDay != nil && *task.DueDay < *checked.Tasks[i-1].DueDay {
				t.Errorf("%s: %q is due before %q above it", b.ID, task.Name, checked.Tasks[i-1].Name)
			}
			if strings.Contains(task.Description, "<script") {
				t.Errorf("%s: description isn't escaped", b.ID)
			}
		}
	}
	if builtIn("client-project") == nil || builtIn("nope") != nil {
		t.Fatal("builtIn finds by id")
	}
}

func templateError(t *testing.T, err error, want string) {
	t.Helper()
	var te *TemplateError
	if !errors.As(err, &te) {
		t.Fatalf("want a TemplateError containing %q, got %v", want, err)
	}
	if !strings.Contains(te.Error(), want) {
		t.Fatalf("got %q, want it to say %q", te.Error(), want)
	}
}

func TestCheck(t *testing.T) {
	ok := func() Template {
		return Template{Name: "  Client   work ", Statuses: []Status{{Name: "Client review", Category: "inReview", Color: "amber"}},
			Tasks: []Task{{Name: " Kickoff ", Status: "client REVIEW", Tags: "b, a, b"}, {Name: "Draft"}}}
	}

	got, err := Check(ok())
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Client work" || got.Tasks[0].Name != "Kickoff" {
		t.Errorf("names are tidied: %q %q", got.Name, got.Tasks[0].Name)
	}
	if got.Tasks[0].Status != "Client review" || got.Tasks[1].Status != dgraphStruct.TASK_STATUS_TODO {
		t.Errorf("statuses: %q, %q", got.Tasks[0].Status, got.Tasks[1].Status)
	}
	if got.Tasks[0].Tags != "b, a" && got.Tasks[0].Tags != "b,a" {
		t.Errorf("tags are normalised once each: %q", got.Tasks[0].Tags)
	}

	for name, c := range map[string]struct {
		edit func(*Template)
		want string
	}{
		"no name":          {func(t *Template) { t.Name = "  " }, "Give the template a name"},
		"long name":        {func(t *Template) { t.Name = strings.Repeat("x", MaxNameLength+1) }, "under 60"},
		"long description": {func(t *Template) { t.Description = strings.Repeat("x", MaxAboutLength+1) }, "description under"},
		"no tasks":         {func(t *Template) { t.Tasks = nil }, "at least one task"},
		"unnamed task":     {func(t *Template) { t.Tasks[1].Name = "" }, "Give task 2 a name"},
		"unknown status":   {func(t *Template) { t.Tasks[1].Status = "Shipping" }, `"Draft" is in a status the template doesn't have: "Shipping"`},
		"priority":         {func(t *Template) { t.Tasks[1].Priority = "urgent" }, "Use low, medium or high"},
		"negative day":     {func(t *Template) { t.Tasks[1].DueDay = on(-1) }, "a template counts from 0"},
		"far day":          {func(t *Template) { t.Tasks[1].StartDay = on(MaxDay + 1) }, "a template counts from 0"},
		"starts late":      {func(t *Template) { t.Tasks[1].StartDay, t.Tasks[1].DueDay = on(5), on(2) }, "starts after it's due"},
		"unnamed subtask":  {func(t *Template) { t.Tasks[1].Subtasks = []Subtask{{Name: " "}} }, `subtask 1 of "Draft"`},
		"same status":      {func(t *Template) { t.Statuses = append(t.Statuses, Status{Name: "client review", Category: "todo"}) }, "two statuses named"},
		"built-in status":  {func(t *Template) { t.Statuses = append(t.Statuses, Status{Name: "Done", Category: "done"}) }, `The status "Done"`},
		"descriptions too long": {func(t *Template) {
			t.Tasks = nil
			for i := 0; i < 50; i++ {
				t.Tasks = append(t.Tasks, Task{Name: "step", Description: strings.Repeat("x", MaxTaskDescription-5000)})
			}
		}, "add up to more than 2 MB"},
		"too big": {func(t *Template) {
			t.Tasks[0].Subtasks = make([]Subtask, MaxTasks)
			for i := range t.Tasks[0].Subtasks {
				t.Tasks[0].Subtasks[i].Name = "step"
			}
		}, "up to 200 tasks and subtasks; this one has 202"},
	} {
		t.Run(name, func(t *testing.T) {
			tpl := ok()
			c.edit(&tpl)
			_, err := Check(tpl)
			templateError(t, err, c.want)
		})
	}
}

func TestSizeAndPreview(t *testing.T) {
	tpl := Template{Tasks: []Task{{Name: "a", Subtasks: steps("x", "y")}, {Name: "b"}, {Name: "c"}, {Name: "d"}, {Name: "e"}}}
	if tpl.Size() != 7 {
		t.Errorf("size %d", tpl.Size())
	}
	if p := tpl.Preview(); strings.Join(p, ",") != "a,b,c,d" {
		t.Errorf("preview %v", p)
	}
	if p := (Template{}).Preview(); p == nil || len(p) != 0 {
		t.Errorf("an empty preview is an empty list, not null: %#v", p)
	}
}

func at(s string) *time.Time {
	d, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return &d
}

func ptr(s string) *string { return &s }

func TestFromProject(t *testing.T) {
	kolkata, _ := time.LoadLocation("Asia/Kolkata")
	zero := time.Time{}
	statuses := []*statusModel.TaskStatus{{Name: "Triage", Category: "backlog", Color: "slate"}, {Name: "QA", Category: "inReview", Color: "violet"}}
	tasks := []*dgraphStruct.DgraphTask{
		// In the order they were made, as the query reads them.
		{Name: "Undated idea", Status: "backlog", DueDate: &zero, StartDate: &zero},
		{Name: "Ship it", Status: "done", Priority: "high", DueDate: at("2026-03-10T11:00:00Z"), Label: ptr("launch"), Description: ptr("<p>Go</p>")},
		{Name: "Cancelled", Status: "canceled", DueDate: at("2026-01-01T00:00:00Z")},
		{Name: "In QA", Status: "inReview", CustomStatusName: ptr("QA"), Priority: "urgent", DueDate: at("2026-03-05T20:00:00Z"),
			// 20:00 UTC is 01:30 the next day in Kolkata.
			StartDate: at("2026-03-06T10:00:00Z")},
		{Name: "To triage", Status: "backlog", CustomStatusName: ptr("Triage"), DueDate: at("2026-03-02T05:00:00Z"),
			SubTasks: []*dgraphStruct.DgraphTask{{Name: "early step", Status: "todo", DueDate: at("2026-03-01T04:00:00Z")}, {Name: "dropped", Status: "canceled", DueDate: at("2026-02-01T04:00:00Z")}}},
		{Name: "  ", Status: "todo"},
	}
	got := FromProject("Launch", "", statuses, tasks, kolkata)

	names := []string{}
	for _, task := range got.Tasks {
		names = append(names, task.Name)
	}
	if strings.Join(names, "|") != "To triage|In QA|Ship it|Undated idea|Untitled task" {
		t.Fatalf("dated tasks first by due date, then the rest as they came, cancelled ones left out: %v", names)
	}
	byName := map[string]Task{}
	for _, task := range got.Tasks {
		byName[task.Name] = task
	}

	if s := byName["Ship it"].Status; s != "todo" {
		t.Errorf("a done task starts again in To do: %q", s)
	}
	if s := byName["In QA"].Status; s != "todo" {
		t.Errorf("a task under way starts again in To do, whatever its own status: %q", s)
	}
	if s := byName["To triage"].Status; s != "Triage" {
		t.Errorf("a task not yet started keeps its own status: %q", s)
	}
	if s := byName["Undated idea"].Status; s != "backlog" {
		t.Errorf("backlog stays backlog: %q", s)
	}

	// Day 0 is the earliest date anywhere, a subtask's included, but not a
	// cancelled one's (1 February): 1 March in Kolkata.
	day := func(d *int) int {
		if d == nil {
			return -1
		}
		return *d
	}
	if d := day(byName["To triage"].Subtasks[0].DueDay); d != 0 {
		t.Errorf("the earliest date is day 0: %d", d)
	}
	if len(byName["To triage"].Subtasks) != 1 {
		t.Errorf("cancelled subtasks are left out: %+v", byName["To triage"].Subtasks)
	}
	if d := day(byName["To triage"].DueDay); d != 1 {
		t.Errorf("2 March is day 1: %d", d)
	}
	if d := day(byName["In QA"].DueDay); d != 5 {
		t.Errorf("20:00 UTC on 5 March is 6 March in Kolkata, day 5: %d", d)
	}
	if byName["In QA"].StartDay == nil || *byName["In QA"].StartDay != 5 {
		t.Errorf("a start on the due day is kept: %v", byName["In QA"].StartDay)
	}
	if byName["In QA"].Priority != "" {
		t.Errorf("a priority OneCamp doesn't know is left out: %q", byName["In QA"].Priority)
	}
	if byName["Undated idea"].DueDay != nil || byName["Undated idea"].StartDay != nil {
		t.Errorf("unset dates stay unset")
	}
	if ship := byName["Ship it"]; ship.Tags != "launch" || ship.Description != "<p>Go</p>" || ship.Priority != "high" {
		t.Errorf("tags, description and priority are kept: %+v", ship)
	}
	if len(got.Statuses) != 2 || got.Statuses[1].Name != "QA" {
		t.Errorf("every status of the project's own is kept: %+v", got.Statuses)
	}
	if _, err := Check(got); err != nil {
		t.Errorf("a snapshot is a usable template: %v", err)
	}

	// A start after the due date is dropped rather than refused.
	late := FromProject("x", "", nil, []*dgraphStruct.DgraphTask{{Name: "odd", Status: "todo", StartDate: at("2026-03-09T09:00:00Z"), DueDate: at("2026-03-02T09:00:00Z")}}, time.UTC)
	if late.Tasks[0].StartDay != nil || late.Tasks[0].DueDay == nil {
		t.Errorf("start after due: %+v", late.Tasks[0])
	}
	long := FromProject("x", "", nil, []*dgraphStruct.DgraphTask{{Name: strings.Repeat("y", MaxTaskName+50), Status: "todo"}}, time.UTC)
	if n := len([]rune(long.Tasks[0].Name)); n != MaxTaskName {
		t.Errorf("a long name is cut to %d: %d", MaxTaskName, n)
	}
}

func TestStart(t *testing.T) {
	kolkata, _ := time.LoadLocation("Asia/Kolkata")
	// Friday 9 October 2026.
	s := StartOn("2026-10-09", "Asia/Kolkata", true)
	if !s.Day.Equal(time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)) || s.Loc.String() != kolkata.String() {
		t.Fatalf("day: %v in %v", s.Day, s.Loc)
	}
	for day, want := range map[int]string{
		0: "2026-10-09T17:00:00+05:30", // Friday
		1: "2026-10-12T17:00:00+05:30", // Saturday, moved to Monday
		2: "2026-10-12T17:00:00+05:30", // Sunday, moved to Monday
		3: "2026-10-12T17:00:00+05:30",
		7: "2026-10-16T17:00:00+05:30",
	} {
		if got := s.At(day, 17); got != want {
			t.Errorf("day %d: %s, want %s", day, got, want)
		}
	}
	s.SkipWeekends = false
	if got := s.At(1, 9); got != "2026-10-10T09:00:00+05:30" {
		t.Errorf("without skipping, Saturday stays: %s", got)
	}

	// Across a change of the clocks the hour holds.
	ny := StartOn("2026-10-30", "America/New_York", false)
	if got := ny.At(3, 17); got != "2026-11-02T17:00:00-05:00" {
		t.Errorf("after the clocks change: %s", got)
	}

	// Where the clocks skip midnight (Santiago, 6 September 2026), the plan
	// still starts on the day chosen and counts calendar days from it.
	scl := StartOn("2026-09-06", "America/Santiago", false)
	for day, want := range map[int]string{0: "2026-09-06T09:00:00-03:00", 2: "2026-09-08T09:00:00-03:00"} {
		if got := scl.At(day, 9); got != want {
			t.Errorf("Santiago day %d: %s, want %s", day, got, want)
		}
	}
	before := StartOn("2026-09-01", "America/Santiago", false)
	if got := before.At(5, 17); got != "2026-09-06T17:00:00-03:00" {
		t.Errorf("counting across the change: %s", got)
	}

	// An unreadable date is today, and an unknown zone is UTC.
	today := StartOn("next week", "Mars/Olympus", false)
	y, m, d := time.Now().UTC().Date()
	if !today.Day.Equal(time.Date(y, m, d, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("fallback: %v", today.Day)
	}
}

func TestReadingOrder(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	got := readingOrder([]string{"a", "a1", "b"}, now)
	if len(got) != 3 || got[0].Uuid != "a" || !got[0].CreatedAt.Equal(now) {
		t.Fatalf("%+v", got)
	}
	if !got[1].CreatedAt.Before(*got[0].CreatedAt) || !got[2].CreatedAt.Before(*got[1].CreatedAt) {
		t.Errorf("each is older than the one before it, so newest-first reads a, a1, b")
	}
}

func TestCheckKeepsAProjectsFields(t *testing.T) {
	tasks := []Task{{Name: "Plan"}}
	got, err := Check(Template{Name: "Launch", Tasks: tasks, Fields: []Field{
		{Name: "  Channel ", Type: "select", Options: []taskFieldBusiness.OptionInput{{ID: "aaaa1111", Label: "Blog", Color: "violet"}, {Label: "Email"}}, OnCard: true},
		{Name: "Budget", Type: "money"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Fields) != 2 || got.Fields[0].Name != "Channel" || !got.Fields[0].OnCard || got.Fields[0].Options[0].ID != "aaaa1111" || got.Fields[0].Options[1].ID == "" {
		t.Errorf("the fields as kept: %+v", got.Fields)
	}
	if got.Fields[1].Currency != taskFieldBusiness.DefaultCurrency {
		t.Errorf("money with no currency takes the default: %+v", got.Fields[1])
	}
	for _, bad := range [][]Field{
		{{Name: "Size", Type: "slider"}},
		{{Name: "Size", Type: "text"}, {Name: "size", Type: "number"}},
	} {
		var te *TemplateError
		if _, err := Check(Template{Name: "x", Tasks: tasks, Fields: bad}); !errors.As(err, &te) {
			t.Errorf("%+v: %v, want a message for the person", bad, err)
		}
	}
}
