package business

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	planBusiness "github.com/akashc777/OneCamp/business/ProjectPlan"
	model "github.com/akashc777/OneCamp/models/postgres/ProjectPlan"
	"github.com/google/uuid"
)

func planDay(t *testing.T, s string) importProvider.Day {
	t.Helper()
	d, ok := importProvider.ParseDay(s)
	if !ok {
		t.Fatalf("bad day %q", s)
	}
	return d
}

func planText(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	if raw == nil {
		return "<absent>"
	}
	return string(raw)
}

// A source's plan comes over whole: its lead, its dates at their resolutions,
// its priority through the import's map, its status by the workspace's own
// status of that name, and its labels, those the workspace has by name and
// the rest made with the palette colour nearest theirs.
func TestPlanChangeCarriesTheSourcesPlan(t *testing.T) {
	lead, onHold, design := uuid.New(), uuid.New(), uuid.New()
	p := &importProvider.SourcePlan{
		LeadID:   "src-lead",
		Start:    &importProvider.SourceWhen{Day: planDay(t, "2026-11-03"), Resolution: "month"},
		Target:   &importProvider.SourceWhen{Day: planDay(t, "2027-03-31"), Resolution: "quarter"},
		Priority: "High",
		Status:   importProvider.SourcePlanStatus{Category: "paused", Name: "on  hold", Color: "#f2994a"},
		Labels: []importProvider.SourceLabel{
			{Name: "Backend", Color: "#ef4444"}, {Name: "backend ", Color: ""}, {Name: "DESIGN", Color: "#22c55e"},
		},
	}
	in := planInputs{
		Lead: lead,
		Registry: planBusiness.Registry{
			Statuses: []model.Status{{ID: onHold, Name: "On hold", Category: "paused"}, {ID: uuid.New(), Name: "On hold", Category: "planned"}},
			Labels:   []model.Label{{ID: design, Name: "Design", Color: "green"}},
		},
		Priority: strings.ToLower,
	}
	c, left := planChange(p, in)
	for _, w := range []struct{ what, got, want string }{
		{"lead", planText(t, c.Lead), fmt.Sprintf("%q", lead.String())},
		{"start", planText(t, c.Start), `{"date":"2026-11-03","resolution":"month"}`},
		{"target", planText(t, c.Target), `{"date":"2027-03-31","resolution":"quarter"}`},
		{"priority", planText(t, c.Priority), `"high"`},
		{"status", planText(t, c.Status), fmt.Sprintf("%q", onHold.String())},
		{"labels", planText(t, c.Labels), fmt.Sprintf("[%q]", design.String())},
	} {
		if w.got != w.want {
			t.Errorf("%s: got %s, want %s", w.what, w.got, w.want)
		}
	}
	if len(c.NewLabels) != 1 || c.NewLabels[0].Name != "Backend" || c.NewLabels[0].Color != "red" {
		t.Errorf("new labels: got %+v, want one Backend in red (the duplicate name made once)", c.NewLabels)
	}
	if len(left) != 0 {
		t.Errorf("nothing is left out, got %q", left)
	}
}

// What can't come is left out and named, and the rest still comes: a lead who
// didn't come across, a start after the target, a priority or status the
// plan doesn't know, and labels past the most a project carries. A status
// the workspace doesn't name comes as its category's own.
func TestPlanChangeLeavesOutWhatCantCome(t *testing.T) {
	var labels []importProvider.SourceLabel
	for i := 0; i < planBusiness.MostLabelsOnAProject+5; i++ {
		labels = append(labels, importProvider.SourceLabel{Name: fmt.Sprintf("label %02d", i)})
	}
	labels[0].Name = strings.Repeat("x", 50)
	p := &importProvider.SourcePlan{
		LeadID:   "src-lead",
		Start:    &importProvider.SourceWhen{Day: planDay(t, "2027-05-02")},
		Target:   &importProvider.SourceWhen{Day: planDay(t, "2027-04-15"), Resolution: "month"},
		Priority: "someday",
		Status:   importProvider.SourcePlanStatus{Category: "done", Name: "Shipped"},
		Labels:   labels,
		Unread:   []string{"its labels after the first 120 (Linear didn't give them: a timeout)"},
	}
	c, left := planChange(p, planInputs{Priority: func(string) string { return "" }})
	if c.Lead != nil || c.Start != nil || c.Priority != nil {
		t.Errorf("lead, start and priority stay out: got %s, %s, %s", planText(t, c.Lead), planText(t, c.Start), planText(t, c.Priority))
	}
	if got := planText(t, c.Target); got != `{"date":"2027-04-15","resolution":"month"}` {
		t.Errorf("target still comes: got %s", got)
	}
	if got := planText(t, c.Status); got != `"done"` {
		t.Errorf("status comes as its category: got %s", got)
	}
	if len(c.NewLabels) != planBusiness.MostLabelsOnAProject {
		t.Errorf("labels: got %d, want %d", len(c.NewLabels), planBusiness.MostLabelsOnAProject)
	} else if first := c.NewLabels[0]; first.Name != strings.Repeat("x", 40) || first.Color != "" {
		t.Errorf("the first label: got %q (%q), want it cut to 40 characters, with no colour of its own", first.Name, first.Color)
	}
	said := strings.Join(left, " | ")
	// L2: the labels past the most are named, and so is what the source
	// couldn't give.
	for _, want := range []string{"lead", "start", `"Shipped" (it came as Done)`,
		`5 labels past the 20 a project carries ("label 20", "label 21", "label 22", "label 23", "label 24")`,
		"its labels after the first 120 (Linear didn't give them: a timeout)"} {
		if !strings.Contains(said, want) {
			t.Errorf("the report names %q: got %q", want, said)
		}
	}
	if c, left := planChange(nil, planInputs{}); c.Lead != nil || c.Status != nil || c.NewLabels != nil || left != nil {
		t.Errorf("no plan changes nothing: got %+v, %q", c, left)
	}
	if c, _ := planChange(&importProvider.SourcePlan{Status: importProvider.SourcePlanStatus{Category: "archived"}}, planInputs{Priority: strings.ToLower}); c.Status != nil {
		t.Errorf("a category the plan doesn't know stays out: got %s", c.Status)
	}
}

// An imported task enters its status when its source says it did; a finished
// one the source gives no time for, at its last change there; anything else,
// when it was made.
func TestStatusSince(t *testing.T) {
	made := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	changed := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	done := time.Date(2026, 10, 18, 17, 30, 0, 0, time.UTC)
	for _, c := range []struct {
		name string
		st   importProvider.SourceTask
		want time.Time
	}{
		{"the source's time", importProvider.SourceTask{Completed: true, Updated: changed, StatusSince: &done}, done},
		{"finished, no time", importProvider.SourceTask{Completed: true, Updated: changed}, changed},
		{"finished, nothing known", importProvider.SourceTask{Completed: true}, made},
		{"open", importProvider.SourceTask{Updated: changed}, made},
	} {
		if got := statusSince(&c.st, made); !got.Equal(c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// V-297 (CODE-REVIEWER-164 row 2285, mutant N7): when the import's priority
// map can't be read, the plan comes without its priority, and the report
// says why in the words the read failed with.
func TestPlanChangeSaysWhyItsPriorityDidntCome(t *testing.T) {
	why := "the import's priority map couldn't be read: connection reset"
	c, left := planChange(&importProvider.SourcePlan{Priority: "high"}, planInputs{PriorityWhy: why})
	if c.Priority != nil {
		t.Errorf("the priority stays out: got %s", planText(t, c.Priority))
	}
	if got, want := strings.Join(left, " | "), "its priority ("+why+")"; got != want {
		t.Errorf("the report %q, want %q", got, want)
	}
}
