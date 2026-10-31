package business

// A project's task list as a CSV file (asana-16, jira-16): the list its reader
// sees, filtered, searched and sorted as they chose, a row a task with every
// custom field. It is written a page at a time as the list is read, so a long
// list never sits whole in memory, and the rows read back into a project
// through the import's CSV source.

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"strings"
	"time"

	taskFieldBusiness "github.com/akashc777/OneCamp/business/TaskField"
	taskKeys "github.com/akashc777/OneCamp/business/TaskKeys"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	domain "github.com/akashc777/OneCamp/domain/Project"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	taskFieldModel "github.com/akashc777/OneCamp/models/postgres/TaskField"
	authService "github.com/akashc777/OneCamp/services/Auth"
	"github.com/google/uuid"
)

// ExportMax is the most tasks a list's CSV holds: as many as the list reads
// in one go. A longer list is cut there, and its reader is told how many it
// holds.
const ExportMax = dgraphquery.ReadAllMax

// TaskListExport is a project's task list as one reader sees it.
type TaskListExport struct {
	ProjectUUID string
	// ReaderUID is the reader's graph uid.
	ReaderUID string
	// Filter and Sort are the list's own (taskDomain.TaskListFilter,
	// dgraphquery.TaskListOrder).
	Filter, Sort string
	// Location is where the file's dates are written.
	Location *time.Location
}

// WriteTaskListCSV writes the list as a CSV. start is called once before the
// first byte, an empty list's too, with how many tasks the list holds and how
// many rows the file has (at most ExportMax), and gives where the file goes.
// An error after start means the file was cut short.
func WriteTaskListCSV(ctx context.Context, x TaskListExport, start func(total, rows int) io.Writer) error {
	projectID, err := uuid.Parse(x.ProjectUUID)
	if err != nil {
		return err
	}
	fields, err := taskFieldBusiness.List(ctx, projectID)
	if err != nil {
		return err
	}
	c := &taskCSV{fields: fields, loc: x.Location, link: authService.FrontendBaseURL() + "/app/task/", names: map[string]string{}}
	var cw *csv.Writer
	// begin starts the file once the list says how long it is; an empty list,
	// which hands no page, after its read.
	begin := func(total int) error {
		w, err := helpers.NewCSVWriter(start(total, min(total, ExportMax)))
		if err != nil {
			return err
		}
		cw = w
		return cw.Write(c.header())
	}
	err = eachListPage(ctx, x, func(total int, tasks []*dgraphStruct.DgraphTask) error {
		if cw == nil {
			if err := begin(total); err != nil {
				return err
			}
		}
		taskKeys.Lay(ctx, tasks...)
		if err := taskFieldBusiness.MergeFieldValues(ctx, tasks); err != nil {
			return err
		}
		if err := c.readPeople(ctx, tasks); err != nil {
			return err
		}
		for _, t := range tasks {
			if err := cw.Write(c.row(t)); err != nil {
				return err
			}
		}
		cw.Flush()
		return cw.Error()
	})
	if err != nil {
		return err
	}
	if cw == nil {
		if err := begin(0); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// eachListPage hands each a page of the list at a time, in its order, with how
// many tasks it holds, up to ExportMax: the graph's sorts in one snapshot,
// and the Manual order (L-1) from its places, read once and ordered here, a
// page of them read at a time by uid.
func eachListPage(ctx context.Context, x TaskListExport, each func(total int, tasks []*dgraphStruct.DgraphTask) error) error {
	if x.Sort != dgraphquery.ManualOrder {
		return domain.EachProjectTaskPage(ctx, x.ProjectUUID, x.ReaderUID, x.Filter, x.Sort, ExportMax, each)
	}
	places, err := domain.GetDgraphProjectTaskPlaces(ctx, x.ProjectUUID, x.ReaderUID, x.Filter)
	if err != nil {
		return err
	}
	order := manualPage(places, 0, 0, true)
	order = order[:min(len(order), ExportMax)]
	for from := 0; from < len(order); from += dgraphquery.ReadAllPage {
		project, err := manualRows(ctx, x.ProjectUUID, x.ReaderUID, x.Filter, order[from:min(from+dgraphquery.ReadAllPage, len(order))], false)
		if err != nil {
			return err
		}
		if project != nil && len(project.Tasks) > 0 {
			if err := each(len(places), project.Tasks); err != nil {
				return err
			}
		}
	}
	return nil
}

// taskCSV is how a list's tasks are written: its project's fields, where its
// dates are, and the names of the people its rows name so far.
type taskCSV struct {
	fields []*taskFieldModel.Field
	loc    *time.Location
	link   string
	names  map[string]string
}

// header names the columns. A field named like one of the list's own columns
// says it is a field; a money field says its currency.
func (c *taskCSV) header() []string {
	head := []string{"Key", "Title", "Type", "Status", "Priority", "Assignee", "Start date", "Due date", "Estimate (hours)", "Tags", "Parent", "Created", "Created by"}
	tail := []string{"Description", "Link"}
	taken := map[string]bool{}
	for _, h := range append(head, tail...) {
		taken[strings.ToLower(h)] = true
	}
	for _, f := range c.fields {
		name := f.Name
		if f.Type == taskFieldBusiness.TypeMoney && f.Currency != "" {
			name += " (" + f.Currency + ")"
		}
		if taken[strings.ToLower(name)] {
			name += " (field)"
		}
		head = append(head, helpers.CSVCell(name))
	}
	return append(head, tail...)
}

// row is a task as its columns read. What people typed is kept from being read
// as a formula; numbers are written as numbers.
func (c *taskCSV) row(t *dgraphStruct.DgraphTask) []string {
	cell := helpers.CSVCell
	var kind, assignee, parent, by, tags, estimate string
	if t.WorkType != nil {
		kind = t.WorkType.Name
	}
	if t.Assignee != nil {
		assignee = c.person(t.Assignee.Uuid)
	}
	if t.ParentTask != nil {
		parent = t.ParentTask.Key
	}
	if t.CreatedBy != nil {
		by = c.person(t.CreatedBy.Uuid)
	}
	if t.Label != nil {
		tags = strings.Join(helpers.Tags(*t.Label), ", ")
	}
	if m := estimateOf(t); m > 0 {
		estimate = helpers.Hours(int64(m) * 60)
	}
	created := ""
	if dated(t.CreatedAt) {
		created = t.CreatedAt.In(c.loc).Format("2006-01-02 15:04")
	}
	row := []string{
		cell(t.Key), cell(t.Name), cell(kind), cell(taskStatusBusiness.Of(t).Display()), priorityLabel(t.Priority),
		cell(assignee), c.day(t.StartDate), c.day(t.DueDate), estimate, cell(tags), cell(parent), created, cell(by),
	}
	for _, f := range c.fields {
		v := taskFieldBusiness.Text(f, t.Fields[f.ID.String()], c.person)
		if f.Type != taskFieldBusiness.TypeNumber && f.Type != taskFieldBusiness.TypeMoney {
			v = cell(v)
		}
		row = append(row, v)
	}
	// The description keeps its paragraphs, line breaks and lists, which the
	// CSV import reads back (helpers.PlainTextToHTML).
	description := ""
	if t.Description != nil {
		description = helpers.HTMLToText(*t.Description)
	}
	return append(row, cell(description), c.link+t.Uuid)
}

// day is a date as the reader's calendar has it; none for a task without one.
func (c *taskCSV) day(t *time.Time) string {
	if !dated(t) {
		return ""
	}
	return t.In(c.loc).Format(time.DateOnly)
}

// readPeople reads the names of the people a page names that no page before
// it did, in one read: assignees, creators and person fields.
func (c *taskCSV) readPeople(ctx context.Context, tasks []*dgraphStruct.DgraphTask) error {
	var ids []string
	want := func(id string) {
		if _, seen := c.names[id]; id != "" && !seen {
			c.names[id] = ""
			ids = append(ids, id)
		}
	}
	for _, t := range tasks {
		if t.Assignee != nil {
			want(t.Assignee.Uuid)
		}
		if t.CreatedBy != nil {
			want(t.CreatedBy.Uuid)
		}
		for _, f := range c.fields {
			var id string
			if f.Type == taskFieldBusiness.TypePerson && json.Unmarshal(t.Fields[f.ID.String()], &id) == nil {
				want(id)
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	people, err := userDomain.GetUserDisplayMapByUUIDs(ctx, ids)
	if err != nil {
		return err
	}
	for id, u := range people {
		if u != nil {
			c.names[id] = u.DisplayName()
		}
	}
	return nil
}

// person is someone's name; someone who has left is a former member.
func (c *taskCSV) person(id string) string {
	if id == "" {
		return ""
	}
	if name := c.names[id]; name != "" {
		return name
	}
	return "Former member"
}

// priorityLabel is a priority as the app names it: "high" is High.
func priorityLabel(p string) string {
	if p == "" {
		return ""
	}
	return strings.ToUpper(p[:1]) + p[1:]
}
