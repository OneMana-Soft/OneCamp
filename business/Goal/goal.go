// Package business (Goal): goals and their check-ins. A goal is an outcome
// the team is after by a date, with one owner. Its progress is read rather
// than typed: from the projects that serve it (the average of how much of each
// one's work is done), from its sub-goals (their average), or from a number
// its owner moves from where it started towards its target. Check-ins say
// where it stands, drafted from those projects and sub-goals, and closing one
// keeps how it ended. Asana's goals and Linear's initiatives, without the plan
// they sit behind.
package business

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/akashc777/OneCamp/helpers"
	goalModel "github.com/akashc777/OneCamp/models/postgres/Goal"
	updateModel "github.com/akashc777/OneCamp/models/postgres/ProjectUpdate"
	"github.com/google/uuid"
)

// GoalError is a request the person can fix; its text is written for them.
type GoalError struct{ msg string }

func (e *GoalError) Error() string { return e.msg }

func refuse(format string, args ...any) error { return &GoalError{fmt.Sprintf(format, args...)} }

var (
	// ErrNotFound: no such live goal.
	ErrNotFound = errors.New("goal not found")
	// ErrNotYours: only the goal's owner, its creator or a workspace admin may change it.
	ErrNotYours = errors.New("only the goal's owner, its creator or a workspace admin can change it")
	// ErrCheckInNotFound: no such check-in on this goal.
	ErrCheckInNotFound = errors.New("check-in not found")
	// ErrCheckInNotYours: only a check-in's author may edit it.
	ErrCheckInNotYours = errors.New("only the check-in's author can change it")
)

// Limits on a goal, as the person is told them.
const (
	MaxTitle       = 200
	MaxDescription = 4000
	MaxBody        = 8000
	MaxUnit        = 24
	// MaxProjects is how many projects can serve one goal.
	MaxProjects = 20
	// MaxDepth is how many levels goals nest: a company's, a department's, a
	// team's, a person's.
	MaxDepth = 4
	// maxAbs keeps a number goal's values to ones a person means.
	maxAbs = 1e15
)

// CheckInDueDays is how old a goal's last check-in is when the next is due.
const CheckInDueDays = 14

// Healths names where a goal can stand, as the app does: the four a check-in
// gives an open goal, and the three endings that close it.
var Healths = map[string]string{
	updateModel.OnTrack:      "On track",
	updateModel.AtRisk:       "At risk",
	updateModel.OffTrack:     "Off track",
	updateModel.OnHold:       "On hold",
	goalModel.StatusAchieved: "Achieved",
	goalModel.StatusMissed:   "Missed",
	goalModel.StatusDropped:  "Dropped",
}

// Closing reports whether a check-in's health closes the goal.
func Closing(health string) bool {
	return health == goalModel.StatusAchieved || health == goalModel.StatusMissed || health == goalModel.StatusDropped
}

// Input is a goal as written in the editor.
type Input struct {
	Title        string   `json:"title"`
	Description  string   `json:"description"`
	OwnerUUID    string   `json:"owner_uuid"`
	ParentID     string   `json:"parent_id"`
	StartDate    string   `json:"start_date"`
	DueDate      string   `json:"due_date"`
	Measure      string   `json:"measure"`
	StartValue   *float64 `json:"start_value"`
	TargetValue  *float64 `json:"target_value"`
	CurrentValue *float64 `json:"current_value"`
	Unit         string   `json:"unit"`
	// ProjectUUIDs are the projects that serve a new goal; editing ignores them.
	ProjectUUIDs []string `json:"project_uuids,omitempty"`
}

func parseDay(s string) (time.Time, bool) {
	t, err := time.Parse(goalModel.DateLayout, strings.TrimSpace(s))
	if err != nil || t.Year() < 2000 || t.Year() > 2100 {
		return time.Time{}, false
	}
	return t, true
}

func finite(v *float64) bool {
	return v != nil && !math.IsNaN(*v) && !math.IsInf(*v, 0) && math.Abs(*v) <= maxAbs
}

// Check is the goal as stored and the projects to link, or what to fix. The
// owner, the parent and the projects are only parsed here: whether they exist
// is for the caller to ask. Pure.
func Check(in Input) (*goalModel.Goal, []uuid.UUID, error) {
	g := &goalModel.Goal{Title: strings.Join(strings.Fields(in.Title), " "), Description: helpers.NormaliseText(in.Description)}
	switch n := len([]rune(g.Title)); {
	case n == 0:
		return nil, nil, refuse("Give the goal a title.")
	case n > MaxTitle:
		return nil, nil, refuse("Keep a goal's title under %d characters.", MaxTitle)
	}
	if len([]rune(g.Description)) > MaxDescription {
		return nil, nil, refuse("Keep the description under %s characters.", helpers.Thousands(MaxDescription))
	}
	owner, err := uuid.Parse(strings.TrimSpace(in.OwnerUUID))
	if err != nil {
		return nil, nil, refuse("Choose who owns the goal.")
	}
	g.OwnerUUID = owner
	if p := strings.TrimSpace(in.ParentID); p != "" {
		id, err := uuid.Parse(p)
		if err != nil {
			return nil, nil, refuse("That parent goal isn't one OneCamp knows.")
		}
		g.ParentId = &id
	}
	due, ok := parseDay(in.DueDate)
	if !ok {
		return nil, nil, refuse("Choose the date the goal is due, between 2000 and 2100.")
	}
	g.DueDate = due
	if s := strings.TrimSpace(in.StartDate); s != "" {
		start, ok := parseDay(s)
		if !ok {
			return nil, nil, refuse("That start date isn't a date between 2000 and 2100.")
		}
		if start.After(due) {
			return nil, nil, refuse("A goal can't start after it's due.")
		}
		g.StartDate = &start
	}
	switch g.Measure = strings.TrimSpace(in.Measure); g.Measure {
	case goalModel.MeasureProjects, goalModel.MeasureSubgoals:
	case goalModel.MeasureNumber:
		if !finite(in.StartValue) || !finite(in.TargetValue) || !finite(in.CurrentValue) {
			return nil, nil, refuse("Give the number where it starts, its target and where it is now.")
		}
		if *in.StartValue == *in.TargetValue {
			return nil, nil, refuse("The target has to differ from where the number starts.")
		}
		g.StartValue, g.TargetValue, g.CurrentValue = in.StartValue, in.TargetValue, in.CurrentValue
		g.Unit = strings.TrimSpace(in.Unit)
		if len([]rune(g.Unit)) > MaxUnit {
			return nil, nil, refuse("Keep the unit under %d characters.", MaxUnit)
		}
	default:
		return nil, nil, refuse("Choose how the goal measures progress: by its projects, its sub-goals or a number.")
	}
	var projects []uuid.UUID
	seen := map[uuid.UUID]bool{}
	for _, raw := range in.ProjectUUIDs {
		id, err := uuid.Parse(strings.TrimSpace(raw))
		if err != nil {
			return nil, nil, refuse("One of those projects isn't one OneCamp knows.")
		}
		if !seen[id] {
			seen[id] = true
			projects = append(projects, id)
		}
	}
	if len(projects) > MaxProjects {
		return nil, nil, refuse("A goal can have up to %d projects.", MaxProjects)
	}
	return g, projects, nil
}

// Reader is who is asking, as goals need to know them.
type Reader struct {
	UUID      uuid.UUID
	DgraphUID string
	IsAdmin   bool
}

// CanEdit reports whether the reader may change the goal: its owner, whoever
// made it, or a workspace admin.
func (r Reader) CanEdit(g *goalModel.Goal) bool {
	return r.IsAdmin || g.OwnerUUID == r.UUID || g.CreatedBy == r.UUID
}

func clamp01(x float64) float64 {
	if math.IsNaN(x) {
		return 0
	}
	return math.Max(0, math.Min(1, x))
}

// NumberProgress is how far a number has come from where it started towards
// its target, 0 to 1. A target below the start (fewer open bugs, a faster
// reply) counts downwards. Pure.
func NumberProgress(start, target, current float64) float64 {
	if start == target {
		return 0
	}
	return clamp01((current - start) / (target - start))
}

// TaskProgress is the share of a project's tasks that are done, or nil for a
// project with no tasks, which has nothing to say yet. Pure.
func TaskProgress(open, done int) *float64 {
	if open+done <= 0 {
		return nil
	}
	v := float64(done) / float64(open+done)
	return &v
}

// mean is the average of the values, or nil when there are none. Pure.
func mean(vs []float64) *float64 {
	if len(vs) == 0 {
		return nil
	}
	sum := 0.0
	for _, v := range vs {
		sum += v
	}
	m := sum / float64(len(vs))
	return &m
}

// dayIn is midnight of a goal's date in loc: goal dates are days, read in
// the reader's own calendar.
func dayIn(d time.Time, loc *time.Location) time.Time {
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc)
}

// Expected is where an open goal would be by now if it moved evenly over its
// time: the share of the days from its start (or the day it was made) to the
// end of its due date that have passed, 0 to 1, in now's zone. Pure.
func Expected(start, due, now time.Time) float64 {
	from := dayIn(start, now.Location())
	end := dayIn(due, now.Location()).AddDate(0, 0, 1)
	if !end.After(from) {
		return 1
	}
	return clamp01(float64(now.Sub(from)) / float64(end.Sub(from)))
}

// startOf is the day a goal's time began: its start date, or the day it was
// made in loc (the reader's calendar, not the database's UTC one).
func startOf(g *goalModel.Goal, loc *time.Location) time.Time {
	if g.StartDate != nil {
		return *g.StartDate
	}
	return g.CreatedAt.In(loc)
}

// PaceHealth is where an open goal stands against its time: on track within
// ten points of where it should be by now, at risk within twenty-five, off
// track beyond. Empty when there is no progress to measure. Pure.
func PaceHealth(progress *float64, expected float64) string {
	if progress == nil {
		return ""
	}
	switch gap := expected - *progress; {
	case gap <= 0.10:
		return updateModel.OnTrack
	case gap <= 0.25:
		return updateModel.AtRisk
	default:
		return updateModel.OffTrack
	}
}

// Percent is a progress as people read it: "55%". Pure.
func Percent(v float64) string { return strconv.Itoa(int(math.Round(clamp01(v)*100))) + "%" }

// Amount writes a number goal's value with its unit: "410 teams", "$250,000",
// "12.5%". A unit that is one currency sign goes first; any other follows. Up
// to two decimals, none when whole. Pure.
func Amount(v float64, unit string) string {
	r := math.Round(v*100) / 100
	whole, frac := math.Modf(math.Abs(r))
	s := helpers.Thousands(int64(whole))
	if frac > 0 {
		s += strings.TrimRight(strings.TrimPrefix(strconv.FormatFloat(frac, 'f', 2, 64), "0"), "0")
	}
	if r < 0 {
		s = "-" + s
	}
	switch rs := []rune(unit); {
	case unit == "":
		return s
	case unit == "%":
		return s + "%"
	case len(rs) == 1 && unicode.Is(unicode.Sc, rs[0]):
		return unit + s
	default:
		return s + " " + unit
	}
}
