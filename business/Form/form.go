// Package business holds intake forms: Asana's forms. A project's admins
// publish a form at /f/<token>; anyone can fill it in, and each submission
// becomes a task in the project. One answer names the task, every answer goes
// into its description, and answers can fill its priority, due date, tags and
// the project's own fields (fill.go). Sections split a form into pages, and a
// question or a section can show only on an earlier answer (branch.go). The
// task is made as the form's owner, through the Task Writer, so the project
// sees who set the form up; a project rule can run when the form is sent. A
// question can take files, which go on the task (files.go), and a form can
// add a row to a data table instead of making a task (row.go).
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
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	formula "github.com/akashc777/OneCamp/business/Formula"
	principalBusiness "github.com/akashc777/OneCamp/business/Principal"
	taskPriority "github.com/akashc777/OneCamp/business/TaskPriority"
	taskWriter "github.com/akashc777/OneCamp/business/TaskWriter"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	formModel "github.com/akashc777/OneCamp/models/postgres/Form"
	taskFieldModel "github.com/akashc777/OneCamp/models/postgres/TaskField"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// FormError is a request the person can fix; its text is written for them.
type FormError struct{ msg string }

func (e *FormError) Error() string { return e.msg }

// Field types a form can ask, and the section, which asks nothing: it starts
// a page, with a title and a line of help.
const (
	ShortText   = "short_text"
	LongText    = "long_text"
	Email       = "email"
	Number      = "number"
	Date        = "date"
	Select      = "select"
	Checkbox    = "checkbox"
	MultiSelect = "multi_select"
	URL         = "url"
	Phone       = "phone"
	// File takes files: the ids of the visitor's uploads (files.go).
	File    = "file"
	Section = "section"
)

// Field is one item of a form: a question, or a section.
type Field struct {
	Id       string   `json:"id"`
	Label    string   `json:"label"`
	Type     string   `json:"type"`
	Required bool     `json:"required"`
	Options  []string `json:"options,omitempty"`
	// Help is a line under the question, or under a section's title.
	Help string `json:"help,omitempty"`
	// ShowIf shows the item only on an earlier answer (branch.go).
	ShowIf *ShowIf `json:"show_if,omitempty"`
	// MapsTo is what the answer fills besides the description (fill.go), and
	// Map, for the priority, which priority each choice sets.
	MapsTo string            `json:"maps_to,omitempty"`
	Map    map[string]string `json:"map,omitempty"`
	// Hidden is a question the form never shows: only its link fills it
	// (?prefill_<id>=…, Airtable's hide_), so it is never required.
	Hidden bool `json:"hidden,omitempty"`
	// LinkHides lets a link that fills this shown question hide it for that
	// visit (?hide_<id>=true, tables B5c). The form's managers turn it on
	// question by question, and it starts off, so a link never takes out of
	// sight something a person must see, such as what they agree to (main,
	// 28 Oct).
	LinkHides bool `json:"link_hides,omitempty"`
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
	Confirmation string  `json:"confirmation"`
	// Redirect is the page a visitor goes to once the form is sent, in place
	// of its message: an http(s) address, or "".
	Redirect string `json:"redirect"`
	// TargetTable is a data table's id: the form adds a row there, each
	// question filling a column, instead of making a task (row.go).
	TargetTable string `json:"target_table"`
	// Cover and Logo are the attachments it wears as its cover and its logo
	// (look.go): "" for none.
	Cover string `json:"cover_id"`
	Logo  string `json:"logo_id"`
}

const (
	maxFields         = 20
	maxSections       = 10
	maxChoices        = 50
	maxHelp           = 300
	maxConfirmation   = 500
	maxRedirect       = 500
	maxForms          = 20 // a project's, or a table's own
	maxPerFormPerHour = 100
)

var validTypes = map[string]bool{ShortText: true, LongText: true, Email: true, Number: true, Date: true, Select: true,
	Checkbox: true, MultiSelect: true, URL: true, Phone: true, File: true, Section: true}

// hasChoices is a question answered from a list.
func hasChoices(t string) bool { return t == Select || t == MultiSelect }

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
	table := strings.TrimSpace(in.TargetTable) != ""
	if table {
		id, err := uuid.Parse(strings.TrimSpace(in.TargetTable))
		if err != nil {
			return f, &FormError{"Pick the table its answers go to."}
		}
		f.TargetTable = &id
	}
	questions, sections, files := 0, 0, 0
	for _, fl := range in.Fields {
		switch fl.Type {
		case Section:
			sections++
		case File:
			files++
			questions++
		default:
			questions++
		}
	}
	if files > maxFileQuestions {
		return f, &FormError{fmt.Sprintf("A form asks for files at most %d times.", maxFileQuestions)}
	}
	if questions == 0 || questions > maxFields {
		return f, &FormError{fmt.Sprintf("A form has 1 to %d questions.", maxFields)}
	}
	if sections > maxSections {
		return f, &FormError{fmt.Sprintf("A form has at most %d sections.", maxSections)}
	}
	seen := map[string]bool{}
	fields := make([]Field, 0, len(in.Fields))
	sets := map[string]int{}
	for i, fl := range in.Fields {
		n := i + 1
		fl.Label = tidy(fl.Label)
		limit := 200
		if fl.Type == Section {
			limit = 120
		}
		if fl.Label == "" || utf8.RuneCountInString(fl.Label) > limit {
			return f, &FormError{fmt.Sprintf("Item %d needs a label of up to %d characters.", n, limit)}
		}
		if !validTypes[fl.Type] {
			return f, &FormError{fmt.Sprintf("Question %d has a type this form can't ask.", n)}
		}
		if fl.Id == "" {
			fl.Id = fmt.Sprintf("q%d", n)
		}
		if seen[fl.Id] || len(fl.Id) > 40 {
			return f, &FormError{"Each question needs its own id."}
		}
		seen[fl.Id] = true
		fl.Help = strings.TrimSpace(fl.Help)
		if utf8.RuneCountInString(fl.Help) > maxHelp {
			return f, &FormError{fmt.Sprintf("Keep item %d's help under %d characters.", n, maxHelp)}
		}
		if hasChoices(fl.Type) {
			var opts []string
			for _, o := range fl.Options {
				if o = tidy(o); o != "" && !slices.Contains(opts, o) {
					if utf8.RuneCountInString(o) > 100 {
						return f, &FormError{fmt.Sprintf("Keep the choices in question %d under 100 characters.", n)}
					}
					opts = append(opts, o)
				}
			}
			if len(opts) < 2 || len(opts) > maxChoices {
				return f, &FormError{fmt.Sprintf("Question %d needs 2 to %d choices.", n, maxChoices)}
			}
			fl.Options = opts
		} else {
			fl.Options = nil
		}
		if fl.ShowIf != nil {
			if msg := checkShowIf(fl.ShowIf, fields, n); msg != "" {
				return f, &FormError{msg}
			}
		}
		if fl.Hidden {
			switch {
			case fl.Type == Section || fl.Type == File:
				return f, &FormError{fmt.Sprintf("Item %d can't be hidden: only a question its link can answer can.", n)}
			case fl.Required:
				return f, &FormError{fmt.Sprintf("Question %d is hidden, so only its link fills it: it can't be required.", n)}
			}
		}
		if fl.LinkHides {
			switch {
			case fl.Type == Section || fl.Type == File:
				return f, &FormError{fmt.Sprintf("Item %d can't be hidden by a link: only a question a link can answer can.", n)}
			case fl.Hidden:
				fl.LinkHides = false // hidden on every visit already
			}
		}
		switch {
		case fl.Type == Section:
			// A section asks nothing: it is never required and fills nothing.
			if fl.MapsTo != "" {
				return f, &FormError{fmt.Sprintf("Item %d is a section, which fills nothing.", n)}
			}
			fl.Required, fl.Map = false, nil
		case fl.Type == File && table:
			return f, &FormError{fmt.Sprintf("Question %d asks for files, which a table row can't keep yet.", n)}
		case fl.Type == File:
			// Its files go on the task; it fills nothing else.
			if fl.MapsTo != "" && fl.MapsTo != "description" {
				return f, &FormError{fmt.Sprintf("Question %d takes files, which go on the task: it fills nothing else.", n)}
			}
			fl.MapsTo, fl.Map = "", nil
		case table:
			if msg := checkColumn(&fl, n); msg != "" {
				return f, &FormError{msg}
			}
		default:
			if msg := checkMapsTo(&fl, n); msg != "" {
				return f, &FormError{msg}
			}
		}
		// The priority and the due date are set by one question each, and a
		// field or a column is filled by one question.
		if t := fl.MapsTo; t == MapPriority || t == MapDue || strings.HasPrefix(t, MapField) || strings.HasPrefix(t, MapColumn) {
			if sets[t]++; sets[t] > 1 {
				return f, &FormError{fmt.Sprintf("Question %d fills what another question already fills.", n)}
			}
		}
		fields = append(fields, fl)
	}
	// Every section has a question in it before the next section or the end.
	for i, fl := range fields {
		if fl.Type == Section && (i == len(fields)-1 || fields[i+1].Type == Section) {
			return f, &FormError{fmt.Sprintf("The section “%s” needs a question in it.", fl.Label)}
		}
	}
	// The task is named by an answer in words: never a section or files.
	names := func(fl Field) bool { return fl.Type != Section && fl.Type != File }
	f.TitleField = in.TitleField
	if !slices.ContainsFunc(fields, func(fl Field) bool { return fl.Id == f.TitleField && names(fl) }) {
		f.TitleField = ""
		for _, fl := range fields {
			if names(fl) {
				f.TitleField = fl.Id
				break
			}
		}
	}
	f.Confirmation = strings.TrimSpace(in.Confirmation)
	if utf8.RuneCountInString(f.Confirmation) > maxConfirmation {
		return f, &FormError{fmt.Sprintf("Keep the message after sending under %d characters.", maxConfirmation)}
	}
	if f.RedirectURL = strings.TrimSpace(in.Redirect); f.RedirectURL != "" && !redirectable(f.RedirectURL) {
		return f, &FormError{"The page after sending needs a full web address, like https://example.com/thanks."}
	}
	cover, err := parseLook(in.Cover, "cover")
	if err != nil {
		return f, err
	}
	logo, err := parseLook(in.Logo, "logo")
	if err != nil {
		return f, err
	}
	f.CoverID, f.LogoID = cover, logo
	switch {
	case in.Priority == "":
		f.Priority = dgraphStruct.TASK_PRIORITY_MEDIUM
	case taskPriority.Is(in.Priority):
		f.Priority = in.Priority
	default:
		return f, &FormError{"Pick urgent, high, medium or low priority."}
	}
	if table {
		// A row has no priority or assignee.
		f.Priority, in.AssigneeUUID = dgraphStruct.TASK_PRIORITY_MEDIUM, ""
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

// createForm stores a new form while its home has room; a seam for tests.
var createForm = formModel.CreateWithin

// newForm makes f, with a new link, unless its home (a project, or a table
// for the table's own form, named in the refusal) has maxForms forms
// already. The count and the insert are one step (formModel.CreateWithin), so
// two saves at once can't both take the last place.
func newForm(ctx context.Context, f formModel.Form, home string) (*formModel.Form, error) {
	var err error
	if f.Token, err = newToken(); err != nil {
		return nil, err
	}
	made, n, err := createForm(ctx, f, maxForms)
	if err == nil && made == nil {
		return nil, &FormError{fmt.Sprintf("This %s has %d forms. Delete one to make another.", home, n)}
	}
	return made, err
}

// Save creates or updates a project's form. A question that fills one of the
// project's fields is checked against it and takes its options, and the
// assignee its tasks get must be in the project. A form that adds rows is
// checked against its table, as whoever saves it (checkTable).
func Save(ctx context.Context, project uuid.UUID, in Input, by uuid.UUID) (*formModel.Form, error) {
	f, err := CheckForm(in)
	if err != nil {
		return nil, err
	}
	var fields []Field
	if err := json.Unmarshal(f.Fields, &fields); err != nil {
		return nil, err
	}
	if f.TargetTable != nil {
		saver, err := whoIs(ctx, by)
		if err != nil {
			return nil, err
		}
		if err := checkTable(ctx, *f.TargetTable, fields, saver); err != nil {
			return nil, err
		}
	} else if err := checkFields(ctx, project, fields); err != nil {
		return nil, err
	}
	f.Fields, _ = json.Marshal(fields)
	if f.AssigneeUUID != nil {
		ok, err := memberOf(ctx, f.AssigneeUUID.String(), project.String())
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, &FormError{"The person its tasks go to needs to be in this project."}
		}
	}
	var was *formModel.Form
	if f.Id != uuid.Nil && (f.CoverID != nil || f.LogoID != nil) {
		if was, err = formModel.Get(ctx, project, f.Id); err != nil {
			return nil, err
		}
	}
	if err := checkLook(ctx, f, was, by); err != nil {
		return nil, err
	}
	f.ProjectUUID = project
	if f.Id == uuid.Nil {
		f.CreatedBy = by
		return newForm(ctx, f, "project")
	}
	// Whoever saves a form files its tasks from then on: a project admin now,
	// which is what brings back a form whose maker has left the project.
	f.CreatedBy = by
	saved, err := formModel.Update(ctx, f)
	if err == nil && saved == nil {
		return nil, &FormError{"That isn't one of this project's forms."}
	}
	return saved, err
}

// memberOf says whether someone may be given the project's tasks: one of its
// people or an AI teammate (the Task Writer's rule); a seam for tests.
var memberOf = taskWriter.Assignable

// Answer is one question's answer: what the task shows (Value), each choice
// of a multi-select (Values), and a number as it was read (Num). Stored with
// its question's id; a submission from before ids reads by label.
type Answer struct {
	Id     string   `json:"id,omitempty"`
	Label  string   `json:"label"`
	Value  string   `json:"value"`
	Values []string `json:"values,omitempty"`
	Num    *float64 `json:"num,omitempty"`
	// Files are a file question's uploads: their ids as sent, their names
	// once the form is sent with them (files.go).
	Files []FileRef `json:"files,omitempty"`
}

// CheckAnswers validates a submission against the form's questions and
// returns the answers in question order, written out. Only what the visitor
// was shown counts: a section or a question whose condition doesn't hold for
// the answers before it is skipped, never required, and an answer sent for it
// is dropped (branch.go). A number answer is read in the signs its sender
// writes numbers with (the zero NumberSigns are en-US's), which the form sends
// with the answers: "1,5" from Berlin was refused, as a comma groups digits in
// en-US's. Pure.
func CheckAnswers(fields []Field, raw map[string]any, signs formula.NumberSigns) ([]Answer, error) {
	signs, ok := signs.Checked()
	if !ok {
		return nil, &FormError{"Those number signs can't be read."}
	}
	out := make([]Answer, 0, len(fields))
	got := map[string]Answer{}
	page := true
	for _, f := range fields {
		if f.Type == Section {
			page = holds(f.ShowIf, got)
			continue
		}
		if !page || !holds(f.ShowIf, got) {
			continue
		}
		v, present := raw[f.Id]
		a := Answer{Id: f.Id, Label: f.Label}
		switch f.Type {
		case Checkbox:
			b, _ := v.(bool)
			if f.Required && !b {
				return nil, &FormError{fmt.Sprintf("“%s” needs to be ticked.", f.Label)}
			}
			a.Value = "No"
			if b {
				a.Value = "Yes"
			}
		case MultiSelect:
			var picked []string
			if present && v != nil {
				list, ok := v.([]any)
				if !ok {
					return nil, &FormError{fmt.Sprintf("“%s” isn't answered the right way.", f.Label)}
				}
				for _, x := range list {
					c, _ := x.(string)
					if !slices.Contains(f.Options, c) {
						return nil, &FormError{fmt.Sprintf("Pick from the choices for “%s”.", f.Label)}
					}
					if !slices.Contains(picked, c) {
						picked = append(picked, c)
					}
				}
			}
			if len(picked) == 0 {
				if f.Required {
					return nil, &FormError{fmt.Sprintf("“%s” needs an answer.", f.Label)}
				}
				continue
			}
			a.Value, a.Values = strings.Join(picked, ", "), picked
		case File:
			var ids []string
			if present && v != nil {
				list, ok := v.([]any)
				if !ok {
					return nil, &FormError{fmt.Sprintf("“%s” isn't answered the right way.", f.Label)}
				}
				for _, x := range list {
					s, _ := x.(string)
					id, err := uuid.Parse(s)
					if err != nil {
						return nil, &FormError{fmt.Sprintf("“%s” isn't answered the right way.", f.Label)}
					}
					if !slices.Contains(ids, id.String()) {
						ids = append(ids, id.String())
					}
				}
			}
			if len(ids) == 0 {
				if f.Required {
					return nil, &FormError{fmt.Sprintf("“%s” needs a file.", f.Label)}
				}
				continue
			}
			if len(ids) > maxFilesPerQuestion {
				return nil, &FormError{fmt.Sprintf("Add at most %d files to “%s”.", maxFilesPerQuestion, f.Label)}
			}
			for _, id := range ids {
				a.Files = append(a.Files, FileRef{ID: id})
			}
			// The files' names once they're claimed; their ids until then.
			a.Value = strings.Join(ids, ", ")
		default:
			var s string
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
			switch f.Type {
			case LongText:
				limit = 5000
			case URL:
				limit = 2000
			}
			if utf8.RuneCountInString(s) > limit {
				return nil, &FormError{fmt.Sprintf("Keep “%s” under %d characters.", f.Label, limit)}
			}
			switch f.Type {
			case Email:
				addr, err := mail.ParseAddress(s)
				if err != nil || !strings.Contains(addr.Address[strings.LastIndex(addr.Address, "@")+1:], ".") {
					return nil, &FormError{fmt.Sprintf("“%s” needs an email address like name@example.com.", f.Label)}
				}
				s = addr.Address
			case Number:
				n, ok := formula.ParseNumber(s, signs)
				if !ok {
					return nil, &FormError{fmt.Sprintf("“%s” needs a number.", f.Label)}
				}
				a.Num = &n
			case Date:
				if _, err := time.Parse(time.DateOnly, s); err != nil {
					return nil, &FormError{fmt.Sprintf("“%s” needs a date.", f.Label)}
				}
			case Select:
				if !slices.Contains(f.Options, s) {
					return nil, &FormError{fmt.Sprintf("Pick one of the choices for “%s”.", f.Label)}
				}
			case URL:
				if _, ok := helpers.WebLink(s); !ok {
					return nil, &FormError{fmt.Sprintf("“%s” needs a link that starts with https://.", f.Label)}
				}
			case Phone:
				if !isPhone(s) {
					return nil, &FormError{fmt.Sprintf("“%s” needs a phone number.", f.Label)}
				}
			}
			a.Value = s
		}
		out = append(out, a)
		got[f.Id] = a
	}
	return out, nil
}

// isPhone is a phone number as people write one: digits, spaces and + - ( )
// . /, with 5 to 20 digits. Pure.
func isPhone(s string) bool {
	if utf8.RuneCountInString(s) > 30 {
		return false
	}
	digits := 0
	for _, r := range s {
		switch {
		case unicode.IsDigit(r):
			digits++
		case strings.ContainsRune(" +-().//", r):
		default:
			return false
		}
	}
	return digits >= 5 && digits <= 20
}

// TaskFrom names a task and writes its description from the answers. The
// name is the answer to titleField (by its id); the description is HTML for
// the task editor, every answer escaped. Pure.
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
		if name == "" && (a.Id == titleField || (a.Id == "" && a.Label == titleLabel)) {
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

// Public is what a visitor sees of a form: never the project or its people,
// nor what its answers fill. Files says how many files a file question
// takes and how large, when the form has one.
type Public struct {
	Title        string  `json:"title"`
	Description  string  `json:"description"`
	Fields       []Field `json:"fields"`
	Confirmation string  `json:"confirmation,omitempty"`
	// Redirect is where the visitor goes once it's sent (Input.Redirect).
	Redirect string      `json:"redirect,omitempty"`
	Files    *FileLimits `json:"files,omitempty"`
	// Cover and Logo are where its page asks this server for the form's
	// pictures (look.go); none when it has none.
	Cover string `json:"cover,omitempty"`
	Logo  string `json:"logo,omitempty"`
}

// redirectable says whether s is an address a sent form may send its visitor
// to: a web link (helpers.WebLink) with no name or password in it, at most
// maxRedirect characters. Pure.
func redirectable(s string) bool {
	if utf8.RuneCountInString(s) > maxRedirect {
		return false
	}
	u, ok := helpers.WebLink(s)
	return ok && u.User == nil
}

// ErrNoForm is a form that doesn't exist or is switched off; both answer the
// same, so a closed form reveals nothing.
var ErrNoForm = errors.New("no such form")

// live is a form people may fill in, with its owner (as the business layer
// takes an actor) and project as stored.
type live struct {
	form    *formModel.Form
	fields  []Field
	owner   *model.UserInfo
	project *dgraphStruct.DgraphProject
}

// byLink is the form a link's token names, open or not: nil when none does,
// as for a token of another shape.
func byLink(ctx context.Context, token string) (*formModel.Form, error) {
	if len(token) != 24 {
		return nil, nil
	}
	return formByToken(ctx, token)
}

// load finds an active form whose project is still there. A form of an
// archived project, or one whose owner has gone (from the workspace or from
// the project) or may no longer add tasks to it, answers as no form at all:
// nobody should be filing tasks into a project nobody sees, and its tasks are
// filed as the owner, whose notifications would carry the answers to someone
// no longer in the project.
func load(ctx context.Context, token string) (*live, error) {
	f, err := byLink(ctx, token)
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
	owner, err := actorOf(sys, f.CreatedBy)
	if err != nil || owner == nil || owner.UserDgraphInfo.Uid == "" || helpers.IsSoftDeleted(owner.UserDgraphInfo.DeletedAt) {
		return nil, ErrNoForm
	}
	if f.ProjectUUID == uuid.Nil {
		// A table's own form (tableForms.go): its home is its table, and its
		// owner must still be able to manage it.
		l := &live{form: f, fields: fields, owner: owner}
		if f.TargetTable == nil || !tableFormOpen(sys, l) {
			return nil, ErrNoForm
		}
		return l, nil
	}
	project, err := projectDomain.GetBasicDgraphProjectInfo(sys, f.ProjectUUID.String(), owner.UserDgraphInfo.Uid)
	if err != nil || project == nil || project.Uid == "" || project.Team == nil || helpers.IsSoftDeleted(project.DeletedAt) ||
		!project.ReaderIsIn() {
		return nil, ErrNoForm
	}
	l := &live{form: f, fields: fields, owner: owner, project: project}
	if f.TargetTable != nil {
		// Its rows are added as the owner, who must still be able to.
		if !rowsOpen(sys, l) {
			return nil, ErrNoForm
		}
		return l, nil
	}
	// Its tasks are filed as the owner, so the owner must still be able to
	// add tasks here (Work Access): an owner made a viewer, or a member once
	// the project keeps its tasks with its admins, closes their forms.
	standing, err := workAccess.Of(sys, owner.UserDgraphInfo.Uuid, project)
	if err != nil {
		return nil, err
	}
	if !standing.Can(workAccess.NeedCreateTask) {
		return nil, ErrNoForm
	}
	return l, nil
}

// actorOf loads a form's owner as an actor (the Postgres row with the admin
// flag, and the graph node); a seam for tests.
var actorOf = userDomain.ActorByUUID

// GetPublic is a form for a visitor.
func GetPublic(ctx context.Context, token string) (*Public, error) {
	l, err := loadLive(ctx, token)
	if err != nil {
		return nil, err
	}
	shown := make([]Field, len(l.fields))
	for i, f := range l.fields {
		f.MapsTo, f.Map = "", nil
		shown[i] = f
	}
	p := &Public{Title: l.form.Title, Description: l.form.Description, Fields: shown, Confirmation: l.form.Confirmation, Redirect: l.form.RedirectURL}
	if slices.ContainsFunc(l.fields, func(f Field) bool { return f.Type == File }) {
		p.Files = &FileLimits{Max: maxFilesPerQuestion, MB: FileLimitMB()}
	}
	p.Cover, p.Logo = lookPaths(l.form, token)
	return p, nil
}

// Seams for tests: the form a link opens, the form its token names (open or
// not), the Task Writer, the event a sent form is, and its submissions.
var (
	loadLive    = load
	formByToken = formModel.ByToken
	makeTask    = taskWriter.MakeTask
	tellRules   = func(ctx context.Context, data map[string]interface{}) {
		webhookBusiness.DispatchEvent(ctx, "form.submitted", data)
	}
	countSince       = formModel.CountSince
	recordSubmission = func(ctx context.Context, form uuid.UUID, task, row *uuid.UUID, answers []byte) (uuid.UUID, error) {
		return formModel.Record(ctx, form, task, row, answers)
	}
	zoneOfForm = func(tz string) *time.Location {
		if tz = strings.TrimSpace(tz); tz != "" && len(tz) <= 64 {
			if l, err := time.LoadLocation(tz); err == nil {
				return l
			}
		}
		return time.UTC
	}
)

// Submit turns a visitor's answers into a task in the form's project, or a
// row of its table. signs are how the visitor writes numbers (CheckAnswers),
// tz their zone, where a date answer that sets the due date is a day, and
// claim the token their page holds for the files they added (files.go).
func Submit(ctx context.Context, token string, raw map[string]any, signs formula.NumberSigns, tz, claim string, now time.Time) error {
	// The task is filed as the form's owner, but the words are the visitor's,
	// and the visitor is nobody the workspace knows. Marked, so an AI teammate
	// the task reaches does not take them as the owner's request.
	ctx = helpers.WithPublicSubmission(ctx)
	l, err := loadLive(ctx, token)
	if err != nil {
		return err
	}
	f := l.form
	answers, err := CheckAnswers(l.fields, raw, signs)
	if err != nil {
		return err
	}
	if n, err := countSince(ctx, f.Id, now.Add(-time.Hour)); err != nil {
		return err
	} else if n >= maxPerFormPerHour {
		return &FormError{"This form is taking a lot of answers right now. Try again in a while."}
	}
	if f.TargetTable != nil {
		return submitRow(ctx, l, answers)
	}
	files, err := claimFiles(ctx, l, claim, answers)
	if err != nil {
		return err
	}
	sys := helpers.WithSystemRead(ctx)
	var project []*taskFieldModel.Field
	if fillsFields(l.fields) {
		if project, err = projectFields(sys, f.ProjectUUID); err != nil {
			files.release(ctx)
			return err
		}
	}
	values := Fill(l.fields, answers, zoneOfForm(tz), project)
	// The form's assignee, while they can be given work. One whose account was
	// deactivated since leaves the task unassigned: the visitor can do nothing
	// about whom the form gives it to, so their answers are never refused for it.
	var assignee *dgraphStruct.DgraphUser
	if f.AssigneeUUID != nil {
		if a, err := userBusiness.GetDgraphUserInfoByUUID(sys, f.AssigneeUUID.String()); err == nil && principalBusiness.CanBeAssigned(a).Allowed {
			assignee = a
		}
	}
	name, desc := TaskFrom(f.Title, l.fields, f.TitleField, answers)
	priority := values.Priority
	if priority == "" {
		priority = f.Priority
	}
	taskID, err := makeTask(ctx, l.owner, f.ProjectUUID, taskWriter.NewTask{
		Name: name, Description: desc, Priority: priority, Status: dgraphStruct.TASK_STATUS_TODO,
		Label: strings.Join(values.Tags, ", "), Due: values.Due, Assignee: assignee, Fields: values.Fields,
		Attachments: files.attachments,
	})
	if err != nil {
		files.release(ctx)
		return err
	}
	stored, _ := json.Marshal(answers)
	sent, err := recordSubmission(ctx, f.Id, &taskID, nil, stored)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Form/Submit record err: %+v", err)
		sent = uuid.New()
	}
	told, event := context.WithoutCancel(ctx), submitted(l, sent, taskID, name, answers)
	helpers.GoSafeNamed("business/Form.tellRules", func() { tellRules(told, event) })
	return nil
}

// submitted is the event a sent form is: a project rule can run on it
// (business/Workflow form_submitted), about the task it made, saying its
// project's name ({project}) and its answers by question. Pure.
func submitted(l *live, sent, taskID uuid.UUID, name string, answers []Answer) map[string]interface{} {
	byID := make(map[string]interface{}, len(answers))
	for _, a := range answers {
		byID[a.Id] = a.Value
	}
	return map[string]interface{}{
		"event_id":     sent.String(),
		"form_id":      l.form.Id.String(),
		"form_title":   l.form.Title,
		"task_id":      taskID.String(),
		"project_id":   l.form.ProjectUUID.String(),
		"project_name": l.project.Name,
		"name":         name,
		"created_by":   l.owner.UserDgraphInfo.Uuid,
		"answers":      byID,
	}
}
