package provider

import "time"

// SourcePlan is a project's plan as its source keeps it (linear-15): who
// leads it, when it starts and should land, each at the resolution the
// source chose, how much it matters, where it stands, and its labels. Every
// part is optional. The projects stage saves it through business/ProjectPlan
// (business/Import projectPlan.go), and its report names any part that
// couldn't come.
type SourcePlan struct {
	// LeadID is the source's id of the person who leads it.
	LeadID string
	Start  *SourceWhen
	Target *SourceWhen
	// Priority is raw, read through the import's priority map as a task's is.
	Priority string
	Status   SourcePlanStatus
	Labels   []SourceLabel
	// Unread is what of the plan its source couldn't give, each in the
	// report's words ("its labels after the first 120 (Linear didn't give
	// them: …)"): the report names it beside what couldn't come here.
	Unread []string
}

// SourceWhen is a plan's date: a day, and how finely the source chose it
// (day, month, quarter, half or year; "" is a day).
type SourceWhen struct {
	Day        Day
	Resolution string
}

// SourcePlanStatus is where a project stands in its source. Category is one
// of business/ProjectPlan's five (planned, in_progress, paused, done,
// cancelled; "" keeps in progress), and Name and Color are the source's own
// status, when it names one (a source colour: PaletteColor reads it).
type SourcePlanStatus struct {
	Category string
	Name     string
	Color    string
}

// SourceLabel is a project's label in its source: its name and colour (a
// source colour: PaletteColor reads it).
type SourceLabel struct {
	Name  string
	Color string
}

// OnDay is a plan date its source keeps as a day ("2026-10-01"), or nil when
// it keeps none or something else (Asana's start_on and due_on). Pure.
func OnDay(s string) *SourceWhen {
	d, ok := ParseDay(s)
	if !ok {
		return nil
	}
	return &SourceWhen{Day: d, Resolution: "day"}
}

// GivenTime is a source's time as a task's StatusSince takes it: nil when the
// source gives none (the zero time). Pure.
func GivenTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
