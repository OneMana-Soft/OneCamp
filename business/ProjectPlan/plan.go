// Package business is a project's plan (linear-15): who leads it, when it
// starts and should land, each at the resolution its people chose (Period),
// how much it matters, its labels, and where it stands among the five status
// categories every project status falls in, with the workspace's own
// statuses beside the built-in ones. On every plan.
//
// Who may do what: whoever may see a project reads its plan; its admins and
// the members who post its updates (workAccess.NeedPostUpdate) change its
// status, priority, dates and labels, and make new labels as they give them;
// choosing its lead is its admins' (workAccess.NeedManage), and the lead is
// one of its admins or members. An archived project's plan stays as it was.
// The workspace's statuses and labels are its admins' to add, rename,
// recolour, reorder and delete (the routes say so); everyone reads them.
package business

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	model "github.com/akashc777/OneCamp/models/postgres/ProjectPlan"
	softDelete "github.com/akashc777/OneCamp/models/postgres/SoftDelete"
	"github.com/google/uuid"
)

// Category is one of the five status categories, with its built-in status.
type Category struct {
	Key   string `json:"key"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

// Categories is the five, in the order a project goes through them.
var Categories = []Category{
	{Key: "planned", Name: "Planned", Color: "slate"},
	{Key: "in_progress", Name: "In progress", Color: "amber"},
	{Key: "paused", Name: "Paused", Color: "orange"},
	{Key: "done", Name: "Done", Color: "green"},
	{Key: "cancelled", Name: "Cancelled", Color: "red"},
}

// Priorities is a project's priorities, most pressing first: a task's.
var Priorities = []string{"urgent", "high", "medium", "low"}

const (
	// MostLabelsOnAProject is how many labels one project carries.
	MostLabelsOnAProject = 20
	// MostLabels is how many live labels the workspace holds.
	MostLabels = 300
	// MostStatuses is how many statuses of its own the workspace holds.
	MostStatuses = 50
	// MostNameRunes is how many characters a status's or label's name takes.
	MostNameRunes = 40
)

// Refusal is a request refused in words its maker reads (a 400).
type Refusal string

func (r Refusal) Error() string { return string(r) }

func categoryOf(key string) *Category {
	for i := range Categories {
		if Categories[i].Key == key {
			return &Categories[i]
		}
	}
	return nil
}

func isOneOf(s string, of []string) bool {
	for _, x := range of {
		if x == s {
			return true
		}
	}
	return false
}

// When is a plan's date as it's read and written: a day and its resolution.
type When struct {
	Date       string `json:"date"`
	Resolution string `json:"resolution"`
}

// Lead is the person who leads a project, named as every member is: by the
// one rule (helpers.PersonDisplayName) over the graph's names, as mentions,
// assignees and a project update's author are, never by their handle.
type Lead struct {
	UUID string `json:"user_uuid"`
	Name string `json:"name"`
}

// Status is where a project stands: its category, and the workspace status
// it's in (none: the category's built-in).
type Status struct {
	Category string `json:"category"`
	ID       string `json:"id,omitempty"`
	Name     string `json:"name"`
	Color    string `json:"color"`
}

// View is a project's plan as its readers see it.
type View struct {
	Lead      *Lead         `json:"lead"`
	Start     *When         `json:"start"`
	Target    *When         `json:"target"`
	Priority  string        `json:"priority"`
	Status    Status        `json:"status"`
	Labels    []model.Label `json:"labels"`
	UpdatedAt *time.Time    `json:"updated_at,omitempty"`
}

func whenOf(d *time.Time, r string) *When {
	if d == nil {
		return nil
	}
	return &When{Date: d.Format("2006-01-02"), Resolution: r}
}

// Views is each of projects' plans as its readers see it, in four reads
// however many projects there are (the leads' names are one graph read).
func Views(ctx context.Context, projects []uuid.UUID) (map[uuid.UUID]View, error) {
	plans, err := model.Plans(ctx, projects)
	if err != nil {
		return nil, err
	}
	statuses, err := model.Statuses(ctx)
	if err != nil {
		return nil, err
	}
	labels, err := model.Labels(ctx)
	if err != nil {
		return nil, err
	}
	leads := make([]string, 0)
	for _, p := range plans {
		if p.LeadID != nil {
			leads = append(leads, p.LeadID.String())
		}
	}
	// Read as an inbox item reads its people (one graph read, those who have
	// since left too: a plan still says who led it). A person's own node comes
	// first; an import's later copies may hold no name.
	people, err := userDomain.GetPeopleByUUIDs(ctx, leads)
	if err != nil {
		return nil, err
	}
	names := make(map[string]string, len(people))
	for _, u := range people {
		if u != nil && names[u.Uuid] == "" {
			names[u.Uuid] = u.DisplayName()
		}
	}
	byStatus := make(map[uuid.UUID]model.Status, len(statuses))
	for _, s := range statuses {
		byStatus[s.ID] = s
	}
	byLabel := make(map[uuid.UUID]model.Label, len(labels))
	for _, l := range labels {
		byLabel[l.ID] = l
	}
	out := make(map[uuid.UUID]View, len(plans))
	for id, p := range plans {
		v := View{Start: whenOf(p.StartOn, p.StartResolution), Target: whenOf(p.TargetOn, p.TargetResolution), Priority: p.Priority,
			Labels: []model.Label{}, UpdatedAt: p.UpdatedAt}
		if p.UpdatedAt == nil || p.UpdatedAt.IsZero() {
			v.UpdatedAt = nil
		}
		if c := categoryOf(p.StatusCategory); c != nil {
			v.Status = Status{Category: c.Key, Name: c.Name, Color: c.Color}
		}
		if p.StatusID != nil {
			if s, ok := byStatus[*p.StatusID]; ok {
				v.Status = Status{Category: s.Category, ID: s.ID.String(), Name: s.Name, Color: s.Color}
			}
		}
		if p.LeadID != nil {
			v.Lead = &Lead{UUID: p.LeadID.String(), Name: names[p.LeadID.String()]}
		}
		for _, l := range p.Labels {
			if x, ok := byLabel[l]; ok {
				v.Labels = append(v.Labels, x)
			}
		}
		out[id] = v
	}
	return out, nil
}

// NewLabel is a label a change makes as it gives it to the project.
type NewLabel struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

// Change is a change to a project's plan: only the parts it carries change,
// and a part sent as null is cleared (the status goes back to in progress).
// status is a category's key or a workspace status's id; labels is the ids
// the project carries after it, with new_labels made and added beside them.
type Change struct {
	Lead      json.RawMessage `json:"lead_uuid"`
	Start     json.RawMessage `json:"start"`
	Target    json.RawMessage `json:"target"`
	Priority  json.RawMessage `json:"priority"`
	Status    json.RawMessage `json:"status"`
	Labels    json.RawMessage `json:"labels"`
	NewLabels []NewLabel      `json:"new_labels"`
}

// Need is what the change asks of its maker: choosing the lead is the
// project's admins'; the rest is for whoever posts its updates.
func (c Change) Need() workAccess.Need {
	if c.Lead != nil {
		return workAccess.NeedManage
	}
	return workAccess.NeedPostUpdate
}

func isNull(raw json.RawMessage) bool { return strings.TrimSpace(string(raw)) == "null" }

func parseWhen(raw json.RawMessage, target bool) (*time.Time, string, error) {
	if isNull(raw) {
		return nil, "", nil
	}
	var w When
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, "", Refusal("A date is a day and how finely it's chosen.")
	}
	if !isResolution(w.Resolution) {
		return nil, "", Refusal("A date is chosen as a day, a month, a quarter, a half or a year.")
	}
	d, err := time.Parse("2006-01-02", w.Date)
	if err != nil {
		return nil, "", Refusal("A date is a day, written 2027-03-31.")
	}
	first, last := Period(d, w.Resolution)
	if target {
		return &last, w.Resolution, nil
	}
	return &first, w.Resolution, nil
}

// name is a status's or label's name as kept: trimmed, 1 to 40 characters.
func name(s string) (string, error) {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" || utf8.RuneCountInString(s) > MostNameRunes {
		return "", Refusal(fmt.Sprintf("A name takes 1 to %d characters.", MostNameRunes))
	}
	return s, nil
}

func color(c string) (string, error) {
	if c == "" {
		return "slate", nil
	}
	if !isOneOf(c, taskStatusBusiness.Colors) {
		return "", Refusal("That isn't one of the colours.")
	}
	return c, nil
}

// MayLead answers whether a person may lead the project: one of its admins or
// members.
type MayLead func(ctx context.Context, person uuid.UUID) (bool, error)

// Save makes change c to the project's plan, by by, as one save, and answers
// the plan as it now is. A Refusal says what was wrong with c.
func Save(ctx context.Context, project uuid.UUID, c Change, by uuid.UUID, mayLead MayLead) (View, error) {
	next, err := model.Get(ctx, project)
	if err != nil {
		return View{}, err
	}
	var patch model.Patch
	if c.Lead != nil {
		patch.Lead, next.LeadID = true, nil
		if !isNull(c.Lead) {
			var s string
			if json.Unmarshal(c.Lead, &s) != nil {
				return View{}, Refusal("A lead is a person's id.")
			}
			id, err := uuid.Parse(s)
			if err != nil {
				return View{}, Refusal("A lead is a person's id.")
			}
			ok, err := mayLead(ctx, id)
			if err != nil {
				return View{}, err
			}
			if !ok {
				return View{}, Refusal("A project's lead is one of its admins or members.")
			}
			next.LeadID = &id
		}
	}
	if c.Start != nil {
		patch.Start = true
		if next.StartOn, next.StartResolution, err = parseWhen(c.Start, false); err != nil {
			return View{}, err
		}
	}
	if c.Target != nil {
		patch.Target = true
		if next.TargetOn, next.TargetResolution, err = parseWhen(c.Target, true); err != nil {
			return View{}, err
		}
	}
	if next.StartOn != nil && next.TargetOn != nil && next.StartOn.After(*next.TargetOn) {
		return View{}, Refusal("A project can't start after its target.")
	}
	if c.Priority != nil {
		patch.Priority, next.Priority = true, ""
		if !isNull(c.Priority) {
			if json.Unmarshal(c.Priority, &next.Priority) != nil || (next.Priority != "" && !isOneOf(next.Priority, Priorities)) {
				return View{}, Refusal("A priority is urgent, high, medium or low.")
			}
		}
	}
	if c.Status != nil {
		patch.Status, next.StatusCategory, next.StatusID = true, "in_progress", nil
		var s string
		if !isNull(c.Status) {
			if json.Unmarshal(c.Status, &s) != nil {
				return View{}, Refusal("There's no such status.")
			}
			if cat := categoryOf(s); cat != nil {
				next.StatusCategory = cat.Key
			} else if id, err := uuid.Parse(s); err != nil {
				return View{}, Refusal("There's no such status.")
			} else if st, err := model.GetStatus(ctx, id); err != nil {
				return View{}, err
			} else if st == nil {
				return View{}, Refusal("That status was deleted. Pick another.")
			} else {
				next.StatusCategory, next.StatusID = st.Category, &st.ID
			}
		}
	}
	var fresh []model.NewLabel
	if c.Labels != nil || len(c.NewLabels) > 0 {
		patch.Labels = true
		if c.Labels != nil {
			var ids []uuid.UUID
			if !isNull(c.Labels) && json.Unmarshal(c.Labels, &ids) != nil {
				return View{}, Refusal("Labels are a list of label ids.")
			}
			live, err := model.Labels(ctx)
			if err != nil {
				return View{}, err
			}
			known := make(map[uuid.UUID]bool, len(live))
			for _, l := range live {
				known[l.ID] = true
			}
			next.Labels = next.Labels[:0]
			seen := map[uuid.UUID]bool{}
			for _, id := range ids {
				if !known[id] {
					return View{}, Refusal("A label was deleted. Pick another.")
				}
				if !seen[id] {
					seen[id] = true
					next.Labels = append(next.Labels, id)
				}
			}
		}
		for _, l := range c.NewLabels {
			n, err := name(l.Name)
			if err != nil {
				return View{}, err
			}
			col, err := color(l.Color)
			if err != nil {
				return View{}, err
			}
			fresh = append(fresh, model.NewLabel{Name: n, Color: col})
		}
		if len(next.Labels)+len(fresh) > MostLabelsOnAProject {
			return View{}, Refusal(fmt.Sprintf("A project carries at most %d labels.", MostLabelsOnAProject))
		}
	}
	if patch == (model.Patch{}) {
		return View{}, Refusal("There's nothing to change.")
	}
	if _, err := model.Save(ctx, next, patch, fresh, MostLabels, by); err != nil {
		if errors.Is(err, model.ErrDates) {
			return View{}, Refusal("A project can't start after its target.")
		}
		return View{}, duplicate(err, "label", "labels", MostLabels)
	}
	views, err := Views(ctx, []uuid.UUID{project})
	if err != nil {
		return View{}, err
	}
	return views[project], nil
}

// Registry is the workspace's statuses and labels, beside the built-in
// statuses.
type Registry struct {
	Categories []Category     `json:"categories"`
	Statuses   []model.Status `json:"statuses"`
	Labels     []model.Label  `json:"labels"`
	Colors     []string       `json:"colors"`
}

// ReadRegistry is the workspace's statuses and labels.
func ReadRegistry(ctx context.Context) (Registry, error) {
	statuses, err := model.Statuses(ctx)
	if err != nil {
		return Registry{}, err
	}
	labels, err := model.Labels(ctx)
	if err != nil {
		return Registry{}, err
	}
	return Registry{Categories: Categories, Statuses: statuses, Labels: labels, Colors: taskStatusBusiness.Colors}, nil
}

func builtInNamed(n string) bool {
	for _, c := range Categories {
		if strings.EqualFold(c.Name, n) {
			return true
		}
	}
	return false
}

// duplicate words a registry write's refusals: a name a live one has, or the
// workspace's most (many is the word for what it holds, "statuses").
func duplicate(err error, what, many string, most int) error {
	switch {
	case errors.Is(err, model.ErrDuplicate):
		return Refusal("There's a " + what + " with that name already.")
	case errors.Is(err, model.ErrFull):
		return Refusal(fmt.Sprintf("The workspace has %d %s, as many as it may. Delete one first.", most, many))
	}
	return err
}

// AddStatus adds a status of the workspace's own in category, after the
// others.
func AddStatus(ctx context.Context, rawName, category, rawColor string, by uuid.UUID) (*model.Status, error) {
	n, err := name(rawName)
	if err != nil {
		return nil, err
	}
	if builtInNamed(n) {
		return nil, Refusal("There's a status with that name already.")
	}
	if categoryOf(category) == nil {
		return nil, Refusal("A status is planned, in progress, paused, done or cancelled.")
	}
	col, err := color(rawColor)
	if err != nil {
		return nil, err
	}
	s, err := model.CreateStatus(ctx, model.Status{Name: n, Category: category, Color: col}, MostStatuses, by)
	return s, duplicate(err, "status", "statuses", MostStatuses)
}

// EditStatus renames, recolours or moves a status of the workspace's own;
// nil parts keep what they hold. Its category stays: a project in it stays
// where it stands. It answers whether the status is live.
func EditStatus(ctx context.Context, id uuid.UUID, rawName, rawColor *string, position *int) (bool, error) {
	if rawName != nil {
		n, err := name(*rawName)
		if err != nil {
			return false, err
		}
		if builtInNamed(n) {
			return false, Refusal("There's a status with that name already.")
		}
		rawName = &n
	}
	if rawColor != nil {
		if _, err := color(*rawColor); err != nil || *rawColor == "" {
			return false, Refusal("That isn't one of the colours.")
		}
	}
	ok, err := model.UpdateStatus(ctx, id, rawName, rawColor, position)
	return ok, duplicate(err, "status", "statuses", MostStatuses)
}

// EditLabel renames or recolours a label; nil parts keep what they hold. It
// answers whether the label is live.
func EditLabel(ctx context.Context, id uuid.UUID, rawName, rawColor *string) (bool, error) {
	if rawName != nil {
		n, err := name(*rawName)
		if err != nil {
			return false, err
		}
		rawName = &n
	}
	if rawColor != nil {
		if _, err := color(*rawColor); err != nil || *rawColor == "" {
			return false, Refusal("That isn't one of the colours.")
		}
	}
	ok, err := model.UpdateLabel(ctx, id, rawName, rawColor)
	return ok, duplicate(err, "label", "labels", MostLabels)
}

// AddLabel adds a label to the workspace's, from its settings; a plan change
// makes one as it gives it to a project (Change.NewLabels).
func AddLabel(ctx context.Context, rawName, rawColor string, by uuid.UUID) (*model.Label, error) {
	n, err := name(rawName)
	if err != nil {
		return nil, err
	}
	col, err := color(rawColor)
	if err != nil {
		return nil, err
	}
	l, err := model.CreateLabel(ctx, model.NewLabel{Name: n, Color: col}, MostLabels, by)
	return l, duplicate(err, "label", "labels", MostLabels)
}

// RestoreLabel brings a deleted label back to the workspace, while it has
// fewer than MostLabels (refused in a new label's words), and to the projects
// that carried it, except a project that has taken its most labels
// (MostLabelsOnAProject) since: that one stays as it is. ErrTaken
// (softDelete): a live label has its name now.
func RestoreLabel(ctx context.Context, id uuid.UUID) error {
	_, err := model.RestoreLabel(ctx, id, MostLabels, MostLabelsOnAProject)
	return duplicate(err, "label", "labels", MostLabels)
}

// RestoreStatus brings a deleted status of the workspace's own back, while
// it has fewer than MostStatuses (refused in a new status's words). ErrTaken
// (softDelete): a live status has its name now.
func RestoreStatus(ctx context.Context, id uuid.UUID) error {
	err := softDelete.RestoreAs(ctx, softDelete.ProjectStatus, id, nil, model.StatusBound(MostStatuses))
	return duplicate(err, "status", "statuses", MostStatuses)
}
