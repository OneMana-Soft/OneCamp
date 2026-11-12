package business

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	estimateScale "github.com/akashc777/OneCamp/business/EstimateScale"
	domain "github.com/akashc777/OneCamp/domain/Project"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	cycleModel "github.com/akashc777/OneCamp/models/postgres/Cycle"
	taskFieldModel "github.com/akashc777/OneCamp/models/postgres/TaskField"
	"github.com/google/uuid"
)

// sliceRow is a slice's name, counts and effort, for comparing. Pure.
func sliceRow(s InsightSlice) string {
	return fmt.Sprintf("%s open %d overdue %d done %d effort %v/%v", s.Name, s.Open, s.Overdue, s.Done, s.Effort.Open, s.Effort.Done)
}

// slicesOf is the slices one way, by key. Pure.
func slicesOf(in Insights, by string) map[string]string {
	out := map[string]string{}
	for _, s := range in.Slices {
		if s.By == by {
			out[s.Key] = sliceRow(s)
		}
	}
	return out
}

// Work slices by status (a project's own by name, as the board names it),
// labels (each tag, case folded), cycle and estimate (points on a project's
// scale, else hours in bands), and every slice counts its open, overdue and
// done tasks and their effort beside its spreads (L32). Each done task's
// point carries its key in every way.
func TestInsightsByEveryDim(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 9, d, 12, 0, 0, 0, time.UTC) }
	at := func(d int) *time.Time { x := day(d); return &x }
	str := func(s string) *string { return &s }
	pts := func(p float64) *float64 { return &p }
	mins := func(m int) *int { return &m }
	done := func(d int) []*dgraphStruct.DgraphTaskActivity {
		return []*dgraphStruct.DgraphTaskActivity{{Type: dgraphStruct.ACTIVITY_TYPE_STATUS, LogTime: at(d), PrevState: dgraphStruct.TASK_STATUS_TODO, NextState: dgraphStruct.TASK_STATUS_DONE}}
	}
	p1, p2 := uuid.New(), uuid.New()
	sprint := cycleModel.Cycle{Id: uuid.New(), ProjectUUID: p1, Number: 1, Name: "Sprint 1"}
	projects := []domain.ReportProject{
		{UUID: p1.String(), Name: "Launch",
			Open: []*dgraphStruct.DgraphTask{
				{Uuid: "A", Name: "A", CreatedAt: at(10), Status: dgraphStruct.TASK_STATUS_INREVIEW, CustomStatus: str("s-qa"), Label: str("Bug, ui , bug"), EstimatePoints: pts(3), DueDate: at(20)},
				// A status taken away since reads as its category, as on the board.
				{Uuid: "B", Name: "B", CreatedAt: at(12), Status: dgraphStruct.TASK_STATUS_TODO, CustomStatus: str("s-gone")},
			},
			Closed: []*dgraphStruct.DgraphTask{
				{Uuid: "C", Name: "C", CreatedAt: at(8), Status: dgraphStruct.TASK_STATUS_DONE, Label: str("UI"), EstimatePoints: pts(5), EstimateMinutes: mins(120), Activity: done(20)},
			}},
		{UUID: p2.String(), Name: "Ops",
			Open: []*dgraphStruct.DgraphTask{
				{Uuid: "D", Name: "D", CreatedAt: at(9), Status: dgraphStruct.TASK_STATUS_TODO, Label: str("ops"), EstimateMinutes: mins(90), DueDate: at(30)},
			},
			Closed: []*dgraphStruct.DgraphTask{
				// In another project's cycle: in none of this one's.
				{Uuid: "E", Name: "E", CreatedAt: at(15), Status: dgraphStruct.TASK_STATUS_DONE, EstimateMinutes: mins(30), Activity: done(22)},
				// No creation date: done, with no point.
				{Uuid: "F", Name: "F", Status: dgraphStruct.TASK_STATUS_DONE, EstimateMinutes: mins(600), Activity: done(21)},
			}},
	}
	in := buildInsights(projects, insightIn{first: day(7), now: day(25), today: time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC),
		statuses: map[string]string{"s-qa": "QA"},
		cycles:   map[string]cycleModel.Cycle{"A": sprint, "C": sprint, "E": sprint},
		scales:   map[string]*estimateScale.Scale{p1.String(): {Kind: "tshirt"}},
	})
	none := Effort{}
	for by, want := range map[string]map[string]string{
		"status": {
			"s-qa": sliceRow(InsightSlice{Name: "QA", Open: 1, Overdue: 1, Effort: InsightEffort{Open: Effort{Points: 3}}}),
			"todo": sliceRow(InsightSlice{Name: "To do", Open: 2, Effort: InsightEffort{Open: Effort{Hours: 1.5}}}),
			"done": sliceRow(InsightSlice{Name: "Done", Done: 3, Effort: InsightEffort{Done: Effort{Points: 5, Hours: 12.5}}}),
		},
		"labels": {
			"bug": sliceRow(InsightSlice{Name: "Bug", Open: 1, Overdue: 1, Effort: InsightEffort{Open: Effort{Points: 3}}}),
			"ui":  sliceRow(InsightSlice{Name: "ui", Open: 1, Overdue: 1, Done: 1, Effort: InsightEffort{Open: Effort{Points: 3}, Done: Effort{Points: 5, Hours: 2}}}),
			"ops": sliceRow(InsightSlice{Name: "ops", Open: 1, Effort: InsightEffort{Open: Effort{Hours: 1.5}}}),
			"":    sliceRow(InsightSlice{Open: 1, Done: 2, Effort: InsightEffort{Open: none, Done: Effort{Hours: 10.5}}}),
		},
		"cycle": {
			sprint.Id.String(): sliceRow(InsightSlice{Name: "Sprint 1", Open: 1, Overdue: 1, Done: 1, Effort: InsightEffort{Open: Effort{Points: 3}, Done: Effort{Points: 5, Hours: 2}}}),
			"":                 sliceRow(InsightSlice{Open: 2, Done: 2, Effort: InsightEffort{Open: Effort{Hours: 1.5}, Done: Effort{Hours: 10.5}}}),
		},
		"estimate": {
			"M":     sliceRow(InsightSlice{Name: "M", Open: 1, Overdue: 1, Effort: InsightEffort{Open: Effort{Points: 3}}}),
			"L":     sliceRow(InsightSlice{Name: "L", Done: 1, Effort: InsightEffort{Done: Effort{Points: 5, Hours: 2}}}),
			"":      sliceRow(InsightSlice{Open: 1}),
			"1-4h":  sliceRow(InsightSlice{Name: "1–4 h", Open: 1, Effort: InsightEffort{Open: Effort{Hours: 1.5}}}),
			"0-1h":  sliceRow(InsightSlice{Name: "Under 1 h", Done: 1, Effort: InsightEffort{Done: Effort{Hours: 0.5}}}),
			"8-16h": sliceRow(InsightSlice{Name: "8–16 h", Done: 1, Effort: InsightEffort{Done: Effort{Hours: 10}}}),
		},
	} {
		if got := slicesOf(in, by); !reflect.DeepEqual(got, want) {
			t.Errorf("by %s\n got %v\nwant %v", by, got, want)
		}
	}
	if want := (InsightEffort{Open: Effort{Points: 3, Hours: 1.5}, Done: Effort{Points: 5, Hours: 12.5}}); in.Effort != want {
		t.Errorf("the whole effort %+v, want %+v", in.Effort, want)
	}
	// The project's slices count too, as the old slices' spreads do.
	if got := slicesOf(in, "project")[p2.String()]; got != sliceRow(InsightSlice{Name: "Ops", Open: 1, Done: 2, Effort: InsightEffort{Open: Effort{Hours: 1.5}, Done: Effort{Hours: 10.5}}}) {
		t.Errorf("Ops: %s", got)
	}
	if len(in.Points) != 2 || in.Points[1].Task != "C" {
		t.Fatalf("points %+v, want E then C", in.Points)
	}
	c := in.Points[1]
	want := map[string][]string{"project": {p1.String()}, "person": {""}, "priority": {""}, "type": {""}, "status": {"done"},
		"labels": {"ui"}, "cycle": {sprint.Id.String()}, "estimate": {"L"}}
	if !reflect.DeepEqual(c.In, want) || c.Person != "" || c.Project != p1.String() {
		t.Errorf("C's point carries its key in every way: %+v", c.In)
	}
	// Due earlier today is open and not yet overdue: a slice's overdue
	// takes the report's rule (due before today's start), never "before now".
	early := time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC)
	todays := buildInsights([]domain.ReportProject{{UUID: p1.String(), Name: "Launch",
		Open: []*dgraphStruct.DgraphTask{{Uuid: "G", Name: "G", CreatedAt: at(24), Status: dgraphStruct.TASK_STATUS_TODO, DueDate: &early}}}},
		insightIn{first: day(7), now: day(25), today: time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)})
	if got := slicesOf(todays, "status")["todo"]; got != sliceRow(InsightSlice{Name: "To do", Open: 1}) {
		t.Errorf("a task due earlier today: %s, want open and not overdue", got)
	}
	// The ways, in order: the built-in ones a breakdown may name, and a
	// field's by its id.
	var bys []string
	for _, d := range builtInDims(insightIn{}) {
		bys = append(bys, d.by)
	}
	if !reflect.DeepEqual(bys, InsightBys) {
		t.Errorf("dims %v, InsightBys %v", bys, InsightBys)
	}
	id := uuid.New()
	for by, want := range map[string]string{"labels": "labels", "project": "project", "field:" + strings.ToUpper(id.String()): "field:" + id.String(), "field:x": "", "mood": "", "": ""} {
		if got, ok := InsightBy(by); got != want || ok != (want != "") {
			t.Errorf("InsightBy(%q) = %q, %v", by, got, ok)
		}
	}
}

// Every kind of custom field slices (L32): a checkbox Yes or No; text and
// links by their value, trimmed and case folded, the 12 with most tasks and
// then Other; numbers each value up to 12, else in 6 equal ranges; dates by
// month; money as numbers, only for an admin of its project, and never
// counted against the cap otherwise. A widget's own field is sliced past the
// cap.
func TestInsightsByEveryFieldKind(t *testing.T) {
	p := uuid.New()
	field := func(name, typ string) *taskFieldModel.Field {
		return &taskFieldModel.Field{ID: uuid.New(), ProjectID: p, Name: name, Type: typ, Currency: map[bool]string{true: "USD"}[typ == taskFieldModel.TypeMoney]}
	}
	check, text, link := field("Signed off", taskFieldModel.TypeCheckbox), field("Customer", taskFieldModel.TypeText), field("Spec", taskFieldModel.TypeURL)
	num, date, money := field("Score", taskFieldModel.TypeNumber), field("Ship by", taskFieldModel.TypeDate), field("Budget", taskFieldModel.TypeMoney)
	made := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	var open []*dgraphStruct.DgraphTask
	for i := 0; i < 17; i++ {
		v := map[string]json.RawMessage{}
		put := func(f *taskFieldModel.Field, x any) { b, _ := json.Marshal(x); v[f.ID.String()] = b }
		switch {
		case i < 3:
			put(text, "Alpha")
		case i == 3:
			put(text, " alpha ")
		default:
			put(text, fmt.Sprintf("v%02d", i))
		}
		if i%2 == 0 {
			put(check, true)
		}
		if i < 13 {
			put(num, i*6)
		}
		dates := []string{"2026-09-03", "2026-09-28", "2026-10-01"}
		if i < len(dates) {
			put(date, dates[i])
			put(money, []int{1050, 2500, 1050}[i])
		}
		if i == 0 {
			put(link, "https://Example.com/a")
		}
		open = append(open, &dgraphStruct.DgraphTask{Uuid: fmt.Sprint(i), Name: fmt.Sprint(i), CreatedAt: &made, Status: dgraphStruct.TASK_STATUS_TODO, Fields: v})
	}
	projects := []domain.ReportProject{{UUID: p.String(), Name: "Launch", Open: open}}
	fields := []*taskFieldModel.Field{check, text, link, num, date, money}
	window := insightIn{first: time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC), now: time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)}

	// Not an admin of the project: no money.
	dims, capped := fieldDims(fieldAsk{fields: fields}, projects)
	window.fields = dims
	in := buildInsights(projects, window)
	if len(dims) != 5 || capped || len(in.Fields) != 5 || len(slicesOf(in, "field:"+money.ID.String())) != 0 {
		t.Fatalf("a reader who isn't the project's admin: %d dims, fields %+v", len(dims), in.Fields)
	}
	counts := func(by string) map[string]string {
		out := map[string]string{}
		for _, s := range in.Slices {
			if s.By == "field:"+by {
				out[s.Key] = fmt.Sprintf("%s %d", s.Name, s.Open)
			}
		}
		return out
	}
	wantText := map[string]string{"alpha": "Alpha 4", "Other": "Other 2"}
	for i := 4; i <= 14; i++ {
		wantText[fmt.Sprintf("v%02d", i)] = fmt.Sprintf("v%02d 1", i)
	}
	for by, want := range map[string]map[string]string{
		check.ID.String(): {"yes": "Yes 9", "no": "No 8"},
		text.ID.String():  wantText,
		link.ID.String():  {"https://example.com/a": "https://Example.com/a 1", "": " 16"},
		num.ID.String(): {"0..12": "0–12 2", "12..24": "12–24 2", "24..36": "24–36 2", "36..48": "36–48 2", "48..60": "48–60 2",
			"60..72": "60–72 3", "": " 4"},
		date.ID.String(): {"2026-09": "Sep 2026 2", "2026-10": "Oct 2026 1", "": " 14"},
	} {
		if got := counts(by); !reflect.DeepEqual(got, want) {
			t.Errorf("field %s\n got %v\nwant %v", by, got, want)
		}
	}

	// An admin of the project: money too, each value while there are few.
	dims, _ = fieldDims(fieldAsk{fields: fields, admin: map[string]bool{p.String(): true}}, projects)
	window.fields = dims
	in = buildInsights(projects, window)
	if got, want := counts(money.ID.String()), map[string]string{"1050": "10.50 USD 2", "2500": "25.00 USD 1", "": " 14"}; !reflect.DeepEqual(got, want) {
		t.Errorf("money for an admin\n got %v\nwant %v", got, want)
	}

	// The cap counts the fields a reader may slice by; a widget's own field
	// is sliced past it.
	many := []*taskFieldModel.Field{money}
	for i := 0; i < insightFieldsCap; i++ {
		many = append(many, field(fmt.Sprint("Select ", i), taskFieldModel.TypeSelect))
	}
	if dims, capped := fieldDims(fieldAsk{fields: many}, nil); len(dims) != insightFieldsCap || capped {
		t.Errorf("money unread is no field against the cap: %d dims, capped %v", len(dims), capped)
	}
	late := field("Late", taskFieldModel.TypeSelect)
	many = append(many, late, field("Later", taskFieldModel.TypeSelect))
	dims, capped = fieldDims(fieldAsk{fields: many, always: map[string]bool{"field:" + late.ID.String(): true}}, nil)
	if len(dims) != insightFieldsCap+1 || !capped || dims[insightFieldsCap].by != "field:"+late.ID.String() {
		t.Errorf("a widget's field past the cap: %d dims, capped %v", len(dims), capped)
	}
}
