// Package business (TaskStatus) is a project's task statuses: the six built-in
// ones every project has, and the custom ones its admins add.
//
// A custom status ("QA", "Blocked", "Ready to ship") belongs to one built-in
// status, its category. A task in a custom status keeps task_status set to the
// category and carries the custom status beside it (task_custom_status, with
// its name in task_custom_status_name). So everything that asks whether a task
// is done, overdue or open (counts, briefings, reminders, AI tools, GitHub
// issue state, imports, webhooks) reads the category and needs no change, and
// only what shows or picks a status needs to know about custom ones.
//
// Every write of a task's status goes through Resolve, which accepts what a
// person or a tool would say: a built-in key ("inReview"), a built-in label
// ("In review"), a custom status's id, or its name ("QA").
package business

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/TaskStatus"
	"github.com/google/uuid"
)

// BuiltIn is one of the six statuses every project has.
type BuiltIn struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// BuiltIns in board order.
var BuiltIns = []BuiltIn{
	{dgraphStruct.TASK_STATUS_BACKLOG, "Backlog"},
	{dgraphStruct.TASK_STATUS_TODO, "Todo"},
	{dgraphStruct.TASK_STATUS_INPROGRESS, "In Progress"},
	{dgraphStruct.TASK_STATUS_INREVIEW, "In Review"},
	{dgraphStruct.TASK_STATUS_DONE, "Done"},
	{dgraphStruct.TASK_STATUS_CANCELED, "Canceled"},
}

// Colors a custom status may take; the app maps each to its own tokens.
var Colors = []string{"slate", "red", "orange", "amber", "yellow", "lime", "green", "emerald", "teal", "cyan", "sky", "blue", "indigo", "violet", "purple", "pink", "rose"}

const (
	MaxPerProject = 20
	MaxNameLength = 40
)

var (
	ErrInvalid       = errors.New("invalid task status")
	ErrUnknownStatus = errors.New("unknown task status")
	ErrTooMany       = fmt.Errorf("a project can have at most %d custom statuses", MaxPerProject)
	ErrNameTaken     = model.ErrNameTaken
	ErrNotFound      = model.ErrNotFound
)

// IsBuiltIn reports whether key is one of the six built-in statuses.
func IsBuiltIn(key string) bool {
	return builtInByKey(key) != nil
}

// IsClosed reports whether a category is finished: done or canceled. A
// project's own status is closed when the status it counts as is, so pass a
// task's task_status (its category), not its custom status.
func IsClosed(category string) bool {
	return category == dgraphStruct.TASK_STATUS_DONE || category == dgraphStruct.TASK_STATUS_CANCELED
}

func builtInByKey(key string) *BuiltIn {
	for i := range BuiltIns {
		if BuiltIns[i].Key == key {
			return &BuiltIns[i]
		}
	}
	return nil
}

func builtInByName(name string) *BuiltIn {
	norm := normalize(name)
	for i := range BuiltIns {
		if normalize(BuiltIns[i].Key) == norm || normalize(BuiltIns[i].Label) == norm {
			return &BuiltIns[i]
		}
	}
	return nil
}

// normalize folds case, spaces, dashes and underscores, so "in progress",
// "In-Progress" and "inProgress" all name the same status.
func normalize(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '-', '_':
			return -1
		}
		return r
	}, strings.ToLower(strings.TrimSpace(s)))
}

func categoryOrder(key string) int {
	for i, b := range BuiltIns {
		if b.Key == key {
			return i
		}
	}
	return len(BuiltIns)
}

// Resolved is where a status value leads: the built-in category the task's
// task_status takes, and the custom status, if any.
type Resolved struct {
	Category   string
	CustomID   string
	CustomName string
}

// Display is how a resolved status reads: the custom name, or the built-in label.
func (r Resolved) Display() string {
	if r.CustomName != "" {
		return r.CustomName
	}
	if b := builtInByKey(r.Category); b != nil {
		return b.Label
	}
	return r.Category
}

// Value is how to store a reference to it: the custom status's id, or the
// built-in key. Resolve reads it back.
func (r Resolved) Value() string {
	if r.CustomID != "" {
		return r.CustomID
	}
	return r.Category
}

// Of is the resolved status a task is in now.
func Of(t *dgraphStruct.DgraphTask) Resolved {
	r := Resolved{Category: t.Status}
	if t.CustomStatus != nil && *t.CustomStatus != "" {
		r.CustomID = *t.CustomStatus
		if t.CustomStatusName != nil {
			r.CustomName = *t.CustomStatusName
		}
	}
	return r
}

// Resolve turns what a person or tool gave as a status into a Resolved one for
// a task in projectID. An empty value is Todo, the status a new task starts in.
func Resolve(ctx context.Context, projectID string, value string) (Resolved, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return Resolved{Category: dgraphStruct.TASK_STATUS_TODO}, nil
	}
	if b := builtInByKey(value); b != nil {
		return Resolved{Category: b.Key}, nil
	}
	pid, err := uuid.Parse(projectID)
	if err != nil {
		if b := builtInByName(value); b != nil {
			return Resolved{Category: b.Key}, nil
		}
		return Resolved{}, ErrUnknownStatus
	}
	if id, err := uuid.Parse(value); err == nil {
		s, err := model.Get(ctx, pid, id)
		if errors.Is(err, model.ErrNotFound) {
			return Resolved{}, ErrUnknownStatus
		}
		if err != nil {
			return Resolved{}, err
		}
		return Resolved{Category: s.Category, CustomID: s.ID.String(), CustomName: s.Name}, nil
	}
	// A custom status by name comes before a built-in label, so a project that
	// named one "Review" gets its own when it asks for "Review".
	if s, err := model.FindByName(ctx, pid, value); err == nil {
		return Resolved{Category: s.Category, CustomID: s.ID.String(), CustomName: s.Name}, nil
	} else if !errors.Is(err, model.ErrNotFound) {
		return Resolved{}, err
	}
	if b := builtInByName(value); b != nil {
		return Resolved{Category: b.Key}, nil
	}
	return Resolved{}, ErrUnknownStatus
}

// Describe lists the statuses a project accepts, for error messages and for
// the AI's tool descriptions.
func Describe(ctx context.Context, projectID string) string {
	names := make([]string, 0, len(BuiltIns))
	for _, b := range BuiltIns {
		names = append(names, b.Key)
	}
	if pid, err := uuid.Parse(projectID); err == nil {
		if customs, err := model.List(ctx, pid); err == nil {
			for _, c := range customs {
				names = append(names, fmt.Sprintf("%q", c.Name))
			}
		}
	}
	return strings.Join(names, ", ")
}

// ProjectStatuses is everything a project's status pickers and board need.
type ProjectStatuses struct {
	BuiltIn []BuiltIn           `json:"built_in"`
	Custom  []*model.TaskStatus `json:"custom"`
	Colors  []string            `json:"colors"`
	Max     int                 `json:"max"`
}

// List returns a project's statuses; custom ones grouped by category in board
// order, then in the order the admins set.
func List(ctx context.Context, projectID uuid.UUID) (*ProjectStatuses, error) {
	customs, err := model.List(ctx, projectID)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(customs, func(i, j int) bool {
		return categoryOrder(customs[i].Category) < categoryOrder(customs[j].Category)
	})
	return &ProjectStatuses{BuiltIn: BuiltIns, Custom: customs, Colors: Colors, Max: MaxPerProject}, nil
}

// Input is what an admin sends to create or change a custom status.
type Input struct {
	Name     string `json:"name"`
	Category string `json:"category"`
	Color    string `json:"color"`
}

var controlChars = regexp.MustCompile(`[\x00-\x1f\x7f]`)

// Validate tidies and checks an input.
func (in *Input) Validate() error {
	in.Name = strings.Join(strings.Fields(controlChars.ReplaceAllString(in.Name, " ")), " ")
	if in.Name == "" || utf8.RuneCountInString(in.Name) > MaxNameLength {
		return fmt.Errorf("%w: give it a name of up to %d characters", ErrInvalid, MaxNameLength)
	}
	// It would read as the built-in one everywhere a status is shown.
	if builtInByName(in.Name) != nil {
		return fmt.Errorf("%w: %q is already a built-in status", ErrInvalid, in.Name)
	}
	if !IsBuiltIn(in.Category) {
		return fmt.Errorf("%w: choose which built-in status it counts as", ErrInvalid)
	}
	if in.Color == "" {
		in.Color = "slate"
	}
	for _, c := range Colors {
		if c == in.Color {
			return nil
		}
	}
	return fmt.Errorf("%w: unknown colour %q", ErrInvalid, in.Color)
}

// Create adds a custom status to a project.
func Create(ctx context.Context, projectID, userID uuid.UUID, in Input) (*model.TaskStatus, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}
	n, err := model.Count(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if n >= MaxPerProject {
		return nil, ErrTooMany
	}
	return model.Create(ctx, &model.TaskStatus{ProjectID: projectID, Name: in.Name, Category: in.Category, Color: in.Color}, userID)
}

// Update changes a custom status. Its tasks follow: a new name is shown on
// them, and a new category moves them with it, so a task's category never
// disagrees with its custom status.
func Update(ctx context.Context, projectID, id uuid.UUID, in Input) (*model.TaskStatus, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}
	before, err := model.Get(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	after, err := model.Update(ctx, &model.TaskStatus{ID: id, ProjectID: projectID, Name: in.Name, Category: in.Category, Color: in.Color})
	if err != nil {
		return nil, err
	}
	if before.Name != after.Name || before.Category != after.Category {
		if err := taskDomain.RestatusTasksInCustomStatus(ctx, id.String(), after.Category, id.String(), after.Name); err != nil {
			return nil, fmt.Errorf("the status was saved but its tasks were not updated: %w", err)
		}
	}
	// Anything that named it by its old name now names it by id, which a
	// rename cannot break again.
	if !strings.EqualFold(before.Name, after.Name) {
		repoint(ctx, Repoint{ProjectID: projectID, Old: []string{before.Name}, New: id.String()})
	}
	return after, nil
}

// Delete removes a custom status. Its tasks move to moveTo, another status of
// the project, or when moveTo is empty back to the built-in status the deleted
// one counted as, so no task is left in a status that no longer exists.
func Delete(ctx context.Context, projectID, id uuid.UUID, moveTo string) error {
	s, err := model.Get(ctx, projectID, id)
	if err != nil {
		return err
	}
	target := Resolved{Category: s.Category}
	if strings.TrimSpace(moveTo) != "" {
		if target, err = Resolve(ctx, projectID.String(), moveTo); err != nil {
			return err
		}
		if target.CustomID == id.String() {
			return fmt.Errorf("%w: move its tasks to a different status", ErrInvalid)
		}
	}
	// Tasks first: if this fails the status still exists and nothing is lost.
	if err := taskDomain.RestatusTasksInCustomStatus(ctx, id.String(), target.Category, target.CustomID, target.CustomName); err != nil {
		return err
	}
	if err := model.Delete(ctx, projectID, id); err != nil {
		return err
	}
	// What pointed at it (a GitHub automation rule) now points where its tasks went.
	repoint(ctx, Repoint{ProjectID: projectID, Old: []string{id.String(), s.Name}, New: target.Value()})
	return nil
}

// Repoint says that a project's status was renamed or deleted: a stored
// reference to any of Old (compared as Resolve would, ignoring case) should
// now say New, a built-in key or a custom status's id.
type Repoint struct {
	ProjectID uuid.UUID
	Old       []string
	New       string
}

// Matches reports whether a stored status value is one of r.Old.
func (r Repoint) Matches(value string) bool {
	v := strings.TrimSpace(value)
	for _, o := range r.Old {
		if o != "" && strings.EqualFold(v, strings.TrimSpace(o)) {
			return true
		}
	}
	return false
}

var repointers []func(context.Context, Repoint) error

// OnRepoint registers fn to update what it stores when a status is renamed or
// deleted. Packages that keep a status (GitHub automation rules) register in
// their init, since they sit above this one and it cannot call them.
func OnRepoint(fn func(context.Context, Repoint) error) {
	repointers = append(repointers, fn)
}

// repoint runs every listener. The status change has already happened, so a
// listener that fails is logged, not returned: the reference it kept fails at
// use with ErrUnknownStatus, which says what is wrong.
func repoint(ctx context.Context, r Repoint) {
	for _, fn := range repointers {
		if err := fn(ctx, r); err != nil {
			helpers.LogErrorWithContext(ctx, "business/TaskStatus repoint %v err: %+v", r.Old, err)
		}
	}
}

// Reorder sets the order of a project's custom statuses.
func Reorder(ctx context.Context, projectID uuid.UUID, ids []string) error {
	parsed := make([]uuid.UUID, 0, len(ids))
	for _, s := range ids {
		id, err := uuid.Parse(s)
		if err != nil {
			return fmt.Errorf("%w: %q is not a status id", ErrInvalid, s)
		}
		parsed = append(parsed, id)
	}
	return model.Reorder(ctx, projectID, parsed)
}

// QueryClause is the Dgraph filter for "tasks in the status someone named",
// as an AI tool or an API caller says it. A built-in status (key or label)
// matches its whole category, the project's own statuses that count as it
// included: "in review" finds the tasks in QA too. A custom status matches its
// own tasks: in projectID's statuses when there is a project, else in any
// project's status of that name, for lists that span projects. A name that is
// neither is ErrUnknownStatus.
func QueryClause(ctx context.Context, projectID, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if b := builtInByKey(value); b != nil {
		return fmt.Sprintf(`eq(task_status, %q)`, b.Key), nil
	}
	if projectID != "" {
		r, err := Resolve(ctx, projectID, value)
		if err != nil {
			return "", err
		}
		if r.CustomID != "" {
			return fmt.Sprintf(`eq(task_custom_status, %q)`, r.CustomID), nil
		}
		return fmt.Sprintf(`eq(task_status, %q)`, r.Category), nil
	}
	// As in Resolve, a custom status by name comes before a built-in label.
	ids, err := model.IDsByName(ctx, value)
	if err != nil {
		return "", err
	}
	if len(ids) == 0 {
		if b := builtInByName(value); b != nil {
			return fmt.Sprintf(`eq(task_status, %q)`, b.Key), nil
		}
		return "", fmt.Errorf("%w: %q", ErrUnknownStatus, value)
	}
	quoted := make([]string, len(ids))
	for i, id := range ids {
		quoted[i] = fmt.Sprintf("%q", id)
	}
	return fmt.Sprintf(`eq(task_custom_status, [%s])`, strings.Join(quoted, ", ")), nil
}

// FilterClause is the Dgraph filter for "tasks in any of these statuses", for
// the task lists' status filter. A built-in status matches tasks shown in it,
// which excludes those in one of its custom statuses; a custom status matches
// its own tasks. Values that are neither are ignored. "" when nothing is left.
func FilterClause(values []string) string {
	var builtIns, customs []string
	for _, v := range values {
		if IsBuiltIn(v) {
			builtIns = append(builtIns, v)
		} else if _, err := uuid.Parse(v); err == nil {
			customs = append(customs, fmt.Sprintf("%q", v))
		}
	}
	var parts []string
	if len(builtIns) > 0 {
		parts = append(parts, fmt.Sprintf(`(anyofterms(task_status, "%s") AND NOT has(task_custom_status))`, strings.Join(builtIns, " ")))
	}
	if len(customs) > 0 {
		parts = append(parts, fmt.Sprintf(`eq(task_custom_status, [%s])`, strings.Join(customs, ", ")))
	}
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}
