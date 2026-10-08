package business

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	goalModel "github.com/akashc777/OneCamp/models/postgres/Goal"
	updateModel "github.com/akashc777/OneCamp/models/postgres/ProjectUpdate"
	"github.com/google/uuid"
)

// Facts are what a goal's next check-in is drafted from: where it is, where
// it was at the last check-in, how much of its time has passed, and how each
// project and sub-goal serving it is doing.
type Facts struct {
	Goal     string
	Measure  string
	Status   string
	Progress *float64
	// Previous is the progress at the last check-in, posted at PreviousAt.
	Previous   *float64
	PreviousAt *time.Time
	Expected   *float64
	Due        time.Time
	// A number goal's values.
	Start, Target, Current *float64
	Unit                   string
	Projects               []ProjectLine
	HiddenProjects         int
	Subgoals               []Summary
}

// Draft is a check-in written from the facts, to edit and post.
type Draft struct {
	Text   string     `json:"text"`
	Health string     `json:"health"`
	Since  *time.Time `json:"since,omitempty"`
	Facts  Facts      `json:"-"`
}

// maxListed is how many projects or sub-goals a draft names before "and N more".
const maxListed = 8

// FactsOf reads a goal's facts off its page.
func FactsOf(d *Detail) Facts {
	f := Facts{
		Goal: d.Title, Measure: d.Measure, Status: d.Status, Progress: d.Progress, Expected: d.Expected,
		Start: d.StartValue, Target: d.TargetValue, Current: d.CurrentValue, Unit: d.Unit,
		Projects: d.ProjectList, HiddenProjects: d.HiddenProjects, Subgoals: d.SubgoalList,
	}
	f.Due, _ = time.Parse(goalModel.DateLayout, d.DueDate)
	if len(d.CheckIns) > 0 {
		last := d.CheckIns[0]
		f.Previous, f.PreviousAt = last.Progress, &last.CreatedAt
	}
	return f
}

// MakeDraft drafts a goal's next check-in, read as the person asking. now is
// in their zone.
func MakeDraft(ctx context.Context, r Reader, id uuid.UUID, now time.Time) (*Draft, error) {
	d, err := Get(ctx, r, id, now)
	if err != nil {
		return nil, err
	}
	f := FactsOf(d)
	return &Draft{Text: DraftText(f, now), Health: SuggestHealth(f), Since: f.PreviousAt, Facts: f}, nil
}

// worse is the worse of two healths, off track being the worst. Pure.
func worse(a, b string) string {
	rank := map[string]int{updateModel.OnTrack: 1, updateModel.OnHold: 1, updateModel.AtRisk: 2, updateModel.OffTrack: 3}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// SuggestHealth is where the facts say the goal stands; the person picks the
// one they post. Against its time when there is progress to measure; else the
// worst its projects' updates and sub-goals report; on track when nothing
// says otherwise. Pure.
func SuggestHealth(f Facts) string {
	if f.Expected != nil {
		if h := PaceHealth(f.Progress, *f.Expected); h != "" {
			return h
		}
	}
	h := updateModel.OnTrack
	for _, p := range f.Projects {
		if !p.Archived {
			h = worse(h, p.Health)
		}
	}
	for _, s := range f.Subgoals {
		if s.Status == goalModel.StatusOpen {
			h = worse(h, s.Health)
		}
	}
	return h
}

// ago is how long ago t was, in days as people say it. Pure.
func ago(t, now time.Time) string {
	// Rounded: a day that loses an hour to daylight saving is still a day.
	switch d := int(math.Round(today(now).Sub(today(t.In(now.Location()))).Hours() / 24)); {
	case d <= 0:
		return "today"
	case d == 1:
		return "yesterday"
	default:
		return fmt.Sprintf("%d days ago", d)
	}
}

// change is how progress moved since the last check-in, as a clause. Pure.
func change(f Facts, now time.Time) string {
	if f.Previous == nil || f.PreviousAt == nil || f.Progress == nil {
		return ""
	}
	when := " at the last check-in, " + ago(*f.PreviousAt, now)
	switch was, is := Percent(*f.Previous), Percent(*f.Progress); {
	case was == is:
		return ", the same as" + when
	case *f.Progress > *f.Previous:
		return ", up from " + was + when
	default:
		return ", down from " + was + when
	}
}

func list(sb *strings.Builder, title string, lines []string) {
	if len(lines) == 0 {
		return
	}
	sb.WriteString("\n\n" + title + ":")
	for i, l := range lines {
		if i == maxListed {
			sb.WriteString(fmt.Sprintf("\n- and %d more", len(lines)-maxListed))
			break
		}
		sb.WriteString("\n- " + l)
	}
}

// projectsLine sums up the projects serving the goal without naming any: a
// check-in is read by the whole workspace, while a project's name and how it
// is going belong to its people. The goal's page lists them for whoever may
// see them. Pure.
func projectsLine(f Facts) string {
	var live, archived, done, total, overdue int
	said := map[string]int{}
	for _, p := range f.Projects {
		if p.Archived {
			archived++
			continue
		}
		live++
		done, total, overdue = done+p.Done, total+p.Open+p.Done, overdue+p.Overdue
		said[p.Health]++
	}
	var parts []string
	switch {
	case live > 0 && total == 0:
		parts = append(parts, helpers.Count(live, "project", "projects")+", with no tasks yet.")
	case live > 0:
		line := fmt.Sprintf("%d of %s done across %s", done, helpers.Count(total, "task", "tasks"), helpers.Count(live, "project", "projects"))
		if overdue > 0 {
			line += fmt.Sprintf(", %d overdue", overdue)
		}
		parts = append(parts, line+".")
	}
	var healths []string
	for _, h := range []string{updateModel.OnTrack, updateModel.AtRisk, updateModel.OffTrack, updateModel.OnHold} {
		if n := said[h]; n > 0 {
			healths = append(healths, fmt.Sprintf("%d %s", n, strings.ToLower(Healths[h])))
		}
	}
	if n := said[""]; n > 0 && len(healths) > 0 {
		healths = append(healths, fmt.Sprintf("%d with no update yet", n))
	}
	if len(healths) > 0 {
		parts = append(parts, "Their last updates: "+strings.Join(healths, ", ")+".")
	}
	if archived > 0 {
		parts = append(parts, helpers.Count(archived, "is archived and no longer counts.", "are archived and no longer count."))
	}
	if f.HiddenProjects > 0 {
		parts = append(parts, helpers.Count(f.HiddenProjects, "more you're not in counts too.", "more you're not in count too."))
	}
	return strings.Join(parts, " ")
}

// DraftText is the facts as a check-in's text, ready to edit. now is in the
// reader's zone. Pure.
func DraftText(f Facts, now time.Time) string {
	var sb strings.Builder
	switch {
	case f.Progress == nil && f.Measure == goalModel.MeasureProjects && len(f.Projects)+f.HiddenProjects == 0:
		sb.WriteString("No project serves this goal yet.")
	case f.Progress == nil && f.Measure == goalModel.MeasureProjects:
		sb.WriteString("None of the projects serving it has tasks yet.")
	case f.Progress == nil && f.Measure == goalModel.MeasureSubgoals && len(f.Subgoals) == 0:
		sb.WriteString("It has no sub-goals yet.")
	case f.Progress == nil:
		sb.WriteString("None of its sub-goals has progress to measure yet.")
	case f.Measure == goalModel.MeasureNumber && f.Start != nil && f.Target != nil && f.Current != nil:
		sb.WriteString(fmt.Sprintf("Now at %s: %s of the way from %s to %s%s.",
			Amount(*f.Current, f.Unit), Percent(*f.Progress), Amount(*f.Start, f.Unit), Amount(*f.Target, f.Unit), change(f, now)))
	default:
		sb.WriteString(fmt.Sprintf("Progress is %s%s.", Percent(*f.Progress), change(f, now)))
	}
	if f.Status == goalModel.StatusOpen && !f.Due.IsZero() {
		due := f.Due.Format("Mon 2 Jan")
		if end := dayIn(f.Due, now.Location()).AddDate(0, 0, 1); !now.Before(end) {
			sb.WriteString(" It was due on " + due + ".")
		} else if f.Expected != nil {
			sb.WriteString(fmt.Sprintf(" %s of its time to %s has passed.", Percent(*f.Expected), due))
		}
	}

	if line := projectsLine(f); line != "" {
		sb.WriteString("\n\nProjects: " + line)
	}

	var subgoals []string
	for _, s := range f.Subgoals {
		line := s.Title + ": "
		switch {
		case s.Status != goalModel.StatusOpen:
			line += strings.ToLower(Healths[s.Status])
			if s.Progress != nil {
				line += " at " + Percent(*s.Progress)
			}
		case s.Progress == nil:
			line += "nothing to measure yet"
		default:
			line += Percent(*s.Progress)
		}
		if s.Status == goalModel.StatusOpen && s.Health != "" {
			line += ", " + strings.ToLower(Healths[s.Health])
		}
		subgoals = append(subgoals, line+".")
	}
	list(&sb, "Sub-goals", subgoals)
	return sb.String()
}
