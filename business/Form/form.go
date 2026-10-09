// Package business holds intake forms: Asana's forms. A project's admins
// publish a form at /f/<token>; anyone can fill it in, and each submission
// becomes a task in the project. One answer names the task; the rest form its
// description. The task is created as the form's owner, so the project sees
// who set the form up.
package business

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/mail"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	adapter "github.com/akashc777/OneCamp/adapter/Task"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	formModel "github.com/akashc777/OneCamp/models/postgres/Form"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// FormError is a request the person can fix; its text is written for them.
type FormError struct{ msg string }

func (e *FormError) Error() string { return e.msg }

// Field types a form can ask.
const (
	ShortText = "short_text"
	LongText  = "long_text"
	Email     = "email"
	Number    = "number"
	Date      = "date"
	Select    = "select"
	Checkbox  = "checkbox"
)

// Field is one question.
type Field struct {
	Id       string   `json:"id"`
	Label    string   `json:"label"`
	Type     string   `json:"type"`
	Required bool     `json:"required"`
	Options  []string `json:"options,omitempty"`
}

// Input is a form as its admins edit it.
type Input struct {
	Id           string  `json:"id,omitempty"`
	Title        string  `json:"title"`
	Description  string  `json:"description"`
	Fields       []Field `json:"fields"`
	TitleField   string  `json:"title_field"`
	Priority     string  `json:"priority"`
	AssigneeUUID string  `json:"assignee_uuid"`
	Active       bool    `json:"active"`
}

const (
	maxFields         = 20
	maxFormsPerProj   = 20
	maxPerFormPerHour = 100
)

var validTypes = map[string]bool{ShortText: true, LongText: true, Email: true, Number: true, Date: true, Select: true, Checkbox: true}

func tidy(s string) string { return strings.Join(strings.Fields(s), " ") }

// CheckForm validates a form and returns it ready to store. Pure.
func CheckForm(in Input) (formModel.Form, error) {
	var f formModel.Form
	f.Title = tidy(in.Title)
	if f.Title == "" || utf8.RuneCountInString(f.Title) > 120 {
		return f, &FormError{"Give the form a title of up to 120 characters."}
	}
	f.Description = strings.TrimSpace(in.Description)
	if utf8.RuneCountInString(f.Description) > 2000 {
		return f, &FormError{"Keep the description under 2,000 characters."}
	}
	if len(in.Fields) == 0 || len(in.Fields) > maxFields {
		return f, &FormError{fmt.Sprintf("A form has 1 to %d questions.", maxFields)}
	}
	seen := map[string]bool{}
	fields := make([]Field, 0, len(in.Fields))
	for i, fl := range in.Fields {
		fl.Label = tidy(fl.Label)
		if fl.Label == "" || utf8.RuneCountInString(fl.Label) > 200 {
			return f, &FormError{fmt.Sprintf("Question %d needs a label of up to 200 characters.", i+1)}
		}
		if !validTypes[fl.Type] {
			return f, &FormError{fmt.Sprintf("Question %d has a type this form can't ask.", i+1)}
		}
		if fl.Id == "" {
			fl.Id = fmt.Sprintf("q%d", i+1)
		}
		if seen[fl.Id] || len(fl.Id) > 40 {
			return f, &FormError{"Each question needs its own id."}
		}
		seen[fl.Id] = true
		if fl.Type == Select {
			var opts []string
			for _, o := range fl.Options {
				if o = tidy(o); o != "" {
					if utf8.RuneCountInString(o) > 100 {
						return f, &FormError{fmt.Sprintf("Keep the choices in question %d under 100 characters.", i+1)}
					}
					opts = append(opts, o)
				}
			}
			if len(opts) < 2 || len(opts) > 30 {
				return f, &FormError{fmt.Sprintf("Question %d needs 2 to 30 choices.", i+1)}
			}
			fl.Options = opts
		} else {
			fl.Options = nil
		}
		fields = append(fields, fl)
	}
	f.TitleField = in.TitleField
	if f.TitleField == "" || !seen[f.TitleField] {
		f.TitleField = fields[0].Id
	}
	switch in.Priority {
	case dgraphStruct.TASK_PRIORITY_LOW, dgraphStruct.TASK_PRIORITY_MEDIUM, dgraphStruct.TASK_PRIORITY_HIGH:
		f.Priority = in.Priority
	case "":
		f.Priority = dgraphStruct.TASK_PRIORITY_MEDIUM
	default:
		return f, &FormError{"Pick low, medium or high priority."}
	}
	if in.AssigneeUUID != "" {
		id, err := uuid.Parse(in.AssigneeUUID)
		if err != nil {
			return f, &FormError{"That assignee isn't in this workspace."}
		}
		f.AssigneeUUID = &id
	}
	if in.Id != "" {
		id, err := uuid.Parse(in.Id)
		if err != nil {
			return f, &FormError{"That isn't one of this project's forms."}
		}
		f.Id = id
	}
	f.Fields, _ = json.Marshal(fields)
	f.Active = in.Active
	return f, nil
}

func newToken() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Save creates or updates a project's form.
func Save(project uuid.UUID, in Input, by uuid.UUID) (*formModel.Form, error) {
	f, err := CheckForm(in)
	if err != nil {
		return nil, err
	}
	f.ProjectUUID = project
	if f.Id == uuid.Nil {
		if n, err := formModel.Count(project); err != nil {
			return nil, err
		} else if n >= maxFormsPerProj {
			return nil, &FormError{fmt.Sprintf("This project has %d forms. Delete one to make another.", n)}
		}
		if f.Token, err = newToken(); err != nil {
			return nil, err
		}
		f.CreatedBy = by
		return formModel.Create(f)
	}
	// Whoever saves a form files its tasks from then on: a project admin now,
	// which is what brings back a form whose maker has left the project.
	f.CreatedBy = by
	saved, err := formModel.Update(f)
	if err == nil && saved == nil {
		return nil, &FormError{"That isn't one of this project's forms."}
	}
	return saved, err
}

// Answer is one question's answer as the task shows it.
type Answer struct {
	Label string
	Value string
}

// CheckAnswers validates a submission against the form's questions and
// returns the answers in question order, written out. Pure.
func CheckAnswers(fields []Field, raw map[string]any) ([]Answer, error) {
	out := make([]Answer, 0, len(fields))
	for _, f := range fields {
		v, present := raw[f.Id]
		var s string
		switch f.Type {
		case Checkbox:
			b, _ := v.(bool)
			if f.Required && !b {
				return nil, &FormError{fmt.Sprintf("“%s” needs to be ticked.", f.Label)}
			}
			s = "No"
			if b {
				s = "Yes"
			}
		default:
			if present && v != nil {
				switch t := v.(type) {
				case string:
					s = strings.TrimSpace(t)
				case float64:
					s = strconv.FormatFloat(t, 'f', -1, 64)
				default:
					return nil, &FormError{fmt.Sprintf("“%s” isn't answered the right way.", f.Label)}
				}
			}
			if s == "" {
				if f.Required {
					return nil, &FormError{fmt.Sprintf("“%s” needs an answer.", f.Label)}
				}
				continue
			}
			limit := 300
			if f.Type == LongText {
				limit = 5000
			}
			if utf8.RuneCountInString(s) > limit {
				return nil, &FormError{fmt.Sprintf("Keep “%s” under %d characters.", f.Label, limit)}
			}
			switch f.Type {
			case Email:
				a, err := mail.ParseAddress(s)
				if err != nil || !strings.Contains(a.Address[strings.LastIndex(a.Address, "@")+1:], ".") {
					return nil, &FormError{fmt.Sprintf("“%s” needs an email address like name@example.com.", f.Label)}
				}
				s = a.Address
			case Number:
				if _, err := strconv.ParseFloat(s, 64); err != nil {
					return nil, &FormError{fmt.Sprintf("“%s” needs a number.", f.Label)}
				}
			case Date:
				if _, err := time.Parse("2006-01-02", s); err != nil {
					return nil, &FormError{fmt.Sprintf("“%s” needs a date.", f.Label)}
				}
			case Select:
				ok := false
				for _, o := range f.Options {
					if o == s {
						ok = true
						break
					}
				}
				if !ok {
					return nil, &FormError{fmt.Sprintf("Pick one of the choices for “%s”.", f.Label)}
				}
			}
		}
		out = append(out, Answer{Label: f.Label, Value: s})
	}
	return out, nil
}

// TaskFrom names a task and writes its description from the answers. The
// description is HTML for the task editor, every answer escaped. Pure.
func TaskFrom(formTitle string, fields []Field, titleField string, answers []Answer) (name, description string) {
	titleLabel := ""
	for _, f := range fields {
		if f.Id == titleField {
			titleLabel = f.Label
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<p>Sent through the form <strong>%s</strong>.</p><ul>", html.EscapeString(formTitle))
	for _, a := range answers {
		if a.Label == titleLabel && name == "" {
			name = a.Value
		}
		fmt.Fprintf(&b, "<li><p><strong>%s</strong>: %s</p></li>", html.EscapeString(a.Label), strings.ReplaceAll(html.EscapeString(a.Value), "\n", "<br>"))
	}
	b.WriteString("</ul>")
	name = tidy(name)
	if name == "" {
		name = formTitle + " response"
	}
	if utf8.RuneCountInString(name) > 200 {
		name = string([]rune(name)[:200])
	}
	return name, b.String()
}

// Public is what a visitor sees of a form: never the project or its people.
type Public struct {
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Fields      []Field `json:"fields"`
}

// ErrNoForm is a form that doesn't exist or is switched off; both answer the
// same, so a closed form reveals nothing.
var ErrNoForm = errors.New("no such form")

// live is a form people may fill in, with its owner and project as stored.
type live struct {
	form    *formModel.Form
	fields  []Field
	owner   *dgraphStruct.DgraphUser
	project *dgraphStruct.DgraphProject
}

// load finds an active form whose project is still there. A form of an
// archived project, or one whose owner has gone (from the workspace or from
// the project), answers as no form at all: nobody should be filing tasks into
// a project nobody sees, and its tasks are filed as the owner, whose
// notifications would carry the answers to someone no longer in the project.
func load(ctx context.Context, token string) (*live, error) {
	if len(token) != 24 {
		return nil, ErrNoForm
	}
	f, err := formModel.ByToken(token)
	if err != nil {
		return nil, err
	}
	if f == nil || !f.Active {
		return nil, ErrNoForm
	}
	var fields []Field
	if json.Unmarshal(f.Fields, &fields) != nil {
		return nil, ErrNoForm
	}
	sys := helpers.WithSystemRead(ctx)
	owner, err := userBusiness.GetDgraphUserInfoByUUID(sys, f.CreatedBy.String())
	if err != nil || owner == nil || helpers.IsSoftDeleted(owner.DeletedAt) {
		return nil, ErrNoForm
	}
	project, err := projectDomain.GetBasicDgraphProjectInfo(sys, f.ProjectUUID.String(), owner.Uid)
	if err != nil || project == nil || project.Uid == "" || project.Team == nil || helpers.IsSoftDeleted(project.DeletedAt) ||
		project.IsProjectMember == 0 {
		return nil, ErrNoForm
	}
	return &live{form: f, fields: fields, owner: owner, project: project}, nil
}

// GetPublic is a form for a visitor.
func GetPublic(ctx context.Context, token string) (*Public, error) {
	l, err := load(ctx, token)
	if err != nil {
		return nil, err
	}
	return &Public{Title: l.form.Title, Description: l.form.Description, Fields: l.fields}, nil
}

// Submit turns a visitor's answers into a task in the form's project.
func Submit(ctx context.Context, token string, raw map[string]any, now time.Time) error {
	l, err := load(ctx, token)
	if err != nil {
		return err
	}
	f := l.form
	answers, err := CheckAnswers(l.fields, raw)
	if err != nil {
		return err
	}
	if n, err := formModel.CountSince(f.Id, now.Add(-time.Hour)); err != nil {
		return err
	} else if n >= maxPerFormPerHour {
		return &FormError{"This form is taking a lot of answers right now. Try again in a while."}
	}
	sys := helpers.WithSystemRead(ctx)
	owner, project := l.owner, l.project
	var assignee *dgraphStruct.DgraphUser
	if f.AssigneeUUID != nil {
		if a, err := userBusiness.GetDgraphUserInfoByUUID(sys, f.AssigneeUUID.String()); err == nil && a != nil {
			assignee = a
		}
	}
	name, desc := TaskFrom(f.Title, l.fields, f.TitleField, answers)
	actor := &model.UserInfo{UserPostgresInfo: model.User{Id: f.CreatedBy}, UserDgraphInfo: *owner}
	taskID, err := taskBusiness.CreateTask(ctx, f.ProjectUUID, actor, project, assignee, adapter.CreateOrUpdateTaskInput{
		TaskName:        name,
		TaskDescription: desc,
		ProjectUuid:     f.ProjectUUID.String(),
		Priority:        f.Priority,
		Status:          dgraphStruct.TASK_STATUS_TODO,
	}, nil)
	if err != nil {
		return err
	}
	stored, _ := json.Marshal(answers)
	if err := formModel.Record(f.Id, &taskID, stored); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Form/Submit record err: %+v", err)
	}
	return nil
}
