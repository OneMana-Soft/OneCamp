package business

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	taskFieldBusiness "github.com/akashc777/OneCamp/business/TaskField"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	taskFieldModel "github.com/akashc777/OneCamp/models/postgres/TaskField"
	"github.com/google/uuid"
)

// A task list's CSV as a spreadsheet reads it: the list's columns, the
// project's fields in their order, then the description and the link. What
// people typed never runs as a formula, and numbers stay numbers.

func exportFields() []*taskFieldModel.Field {
	return []*taskFieldModel.Field{
		{ID: uuid.New(), Name: "status", Type: taskFieldBusiness.TypeText},
		{ID: uuid.New(), Name: "Budget", Type: taskFieldBusiness.TypeMoney, Currency: "EUR"},
		{ID: uuid.New(), Name: "Score", Type: taskFieldBusiness.TypeNumber},
		{ID: uuid.New(), Name: "=SUM(A1:A9)", Type: taskFieldBusiness.TypeText},
		{ID: uuid.New(), Name: "Reviewer", Type: taskFieldBusiness.TypePerson},
	}
}

func TestTheListsCSVNamesItsColumnsThenTheFields(t *testing.T) {
	got := (&taskCSV{fields: exportFields()}).header()
	want := []string{"Key", "Title", "Type", "Status", "Priority", "Assignee", "Start date", "Due date", "Estimate (hours)", "Tags", "Parent", "Created", "Created by",
		"status (field)", "Budget (EUR)", "Score", "'=SUM(A1:A9)", "Reviewer", "Description", "Link"}
	if !slices.Equal(got, want) {
		t.Errorf("header\n got %q\nwant %q", got, want)
	}
}

func TestATasksRowKeepsWhatPeopleTypedFromRunningAsAFormula(t *testing.T) {
	fields := exportFields()
	kolkata, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	// 23:00 UTC on 1 Nov is 2 Nov in Kolkata: a date is the reader's day.
	start := time.Date(2026, 11, 1, 23, 0, 0, 0, time.UTC)
	var unset time.Time
	created := time.Date(2026, 10, 20, 9, 15, 0, 0, time.UTC)
	minutes := 90
	text := func(s string) *string { return &s }
	value := func(v any) json.RawMessage {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	task := &dgraphStruct.DgraphTask{
		Uuid: "6b0f", Key: "SF-12", Name: `=HYPERLINK("http://x","open")`, WorkType: &dgraphStruct.TaskWorkType{Name: "Bug"},
		Status: dgraphStruct.TASK_STATUS_INPROGRESS, Priority: "high", Assignee: &dgraphStruct.DgraphUser{Uuid: "maya"},
		StartDate: &start, DueDate: &unset, EstimateMinutes: &minutes, Label: text("launch, web"),
		ParentTask: &dgraphStruct.DgraphTask{Key: "SF-3"}, CreatedAt: &created, CreatedBy: &dgraphStruct.DgraphUser{Uuid: "gone"},
		Description: text("<p>@channel ship it</p><ul><li><p>copy</p></li><li><p>ads</p></li></ul>"),
		Fields: map[string]json.RawMessage{
			fields[0].ID.String(): value("-1 day"),
			fields[1].ID.String(): value(-250),
			fields[2].ID.String(): value(-3.5),
			fields[3].ID.String(): value("+44 20 7946 0000"),
			fields[4].ID.String(): value("maya"),
		},
	}
	c := &taskCSV{fields: fields, loc: kolkata, link: "https://app.example/app/task/", names: map[string]string{"maya": "Maya Chen"}}
	got := c.row(task)
	want := []string{"SF-12", `'=HYPERLINK("http://x","open")`, "Bug", "In progress", "High", "Maya Chen", "2026-11-02", "", "1.50", "launch, web", "SF-3",
		"2026-10-20 14:45", "Former member", "'-1 day", "-2.50", "-3.5", "'+44 20 7946 0000", "Maya Chen", "'@channel ship it\n\n- copy\n- ads", "https://app.example/app/task/6b0f"}
	if !slices.Equal(got, want) {
		t.Errorf("row\n got %q\nwant %q", got, want)
	}
	if len(got) != len(c.header()) {
		t.Errorf("a row has %d cells and the header %d", len(got), len(c.header()))
	}
}

// A task with nothing set but its title writes empty cells, never "Former
// member" for no one or a date for the graph's zero.
func TestAnEmptyTasksRowIsEmptyButItsTitleAndLink(t *testing.T) {
	var unset time.Time
	c := &taskCSV{fields: exportFields(), loc: time.UTC, link: "https://app.example/app/task/", names: map[string]string{}}
	got := c.row(&dgraphStruct.DgraphTask{Uuid: "u1", Name: "Plan", Status: dgraphStruct.TASK_STATUS_TODO, StartDate: &unset, CreatedAt: &unset})
	want := []string{"", "Plan", "", "To do", "", "", "", "", "", "", "", "", "", "", "", "", "", "", "", "https://app.example/app/task/u1"}
	if !slices.Equal(got, want) {
		t.Errorf("row\n got %q\nwant %q", got, want)
	}
}
