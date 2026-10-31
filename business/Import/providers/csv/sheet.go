package csv

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	"github.com/akashc777/OneCamp/helpers"
)

// sheet is what a table says, read with the admin's choices: the project,
// its tasks and sub-tasks, the people they name, and the plan that shows it.
type sheet struct {
	project importProvider.SourceProject
	// tasks are the top-level tasks in the file's order, then a "__sub__"
	// marker for each task with sub-tasks, which schedules them.
	tasks    []importProvider.SourceTask
	children map[string][]importProvider.SourceTask // by their parent's source id
	people   []importProvider.SourceUser
	plan     *importProvider.Plan
}

// fieldColumn is a column that becomes a field.
type fieldColumn struct {
	col   *column
	field importProvider.SourceField
}

// choices are the kinds the admin gave field columns (options.columns, by
// header); a kind no field has is left out.
func choices(opts importProvider.JobOptions) map[string]string {
	out := map[string]string{}
	m, _ := opts["columns"].(map[string]any)
	for header, v := range m {
		if kind, _ := v.(string); kinds[kind] {
			out[headerKey(header)] = kind
		}
	}
	return out
}

// read is a table as an import brings it into a project called name. Pure.
func read(t *table, name string, opts importProvider.JobOptions) *sheet {
	zone, _ := opts["time_zone"].(string)
	loc, _ := helpers.FirstLocation(zone)
	by := map[property]*column{}
	for _, c := range t.cols {
		if c.prop != "" {
			by[c.prop] = c
		}
	}
	r := &reading{t: t, by: by, loc: loc, people: map[string]*person{}, emailsOf: map[string]map[string]bool{}}
	r.fieldColumns(choices(opts))
	r.pairNamesWithAddresses()
	r.rows()

	s := &sheet{children: map[string][]importProvider.SourceTask{}}
	s.project = importProvider.SourceProject{SourceID: t.id, Name: name}
	for _, f := range r.fields {
		s.project.Fields = append(s.project.Fields, f.field)
	}
	prefixes := map[string]bool{}
	for _, st := range r.tasks {
		st.ProjectSourceID = t.id
		if prefix, n := importProvider.SplitKey(st.SourceKey); n > 0 {
			prefixes[prefix] = true
			s.project.HighestNumber = max(s.project.HighestNumber, n)
		}
		if parent := r.parentOf[st.SourceID]; parent != "" {
			st.ParentTaskID = parent
			s.children[parent] = append(s.children[parent], st)
			continue
		}
		s.tasks = append(s.tasks, st)
	}
	if len(prefixes) == 1 {
		for p := range prefixes {
			s.project.SourceKey = p
		}
	}
	top := len(s.tasks)
	for _, st := range r.tasks {
		if len(s.children[st.SourceID]) > 0 {
			s.tasks = append(s.tasks, importProvider.SourceTask{SourceID: "__sub__:" + st.SourceID, ParentTaskID: st.SourceID})
		}
	}
	for _, id := range r.order {
		p := r.people[id]
		s.people = append(s.people, importProvider.SourceUser{SourceID: id, Email: p.email, DisplayName: p.shownName()})
	}
	s.plan = r.plan(top, len(r.tasks)-top)
	return s
}

// reading is read's work: the columns, then the rows.
type reading struct {
	t      *table
	by     map[property]*column
	loc    *time.Location
	fields []fieldColumn
	// emailCol is each column of people's address column, when it has one.
	emailCol map[*column]*column
	people   map[string]*person
	order    []string
	// emailsOf is the addresses the file gives beside each name (by
	// PersonNameKey), so a row that names the person alone finds them too.
	emailsOf map[string]map[string]bool
	tasks    []importProvider.SourceTask
	parentOf map[string]string
	planCols []importProvider.PlanColumn
	statuses []string
	priority []string
	warn     counts
}

// person is someone the file names.
type person struct {
	name, email string
	rows        int
	lastLine    int
}

func (p *person) shownName() string {
	if p.name != "" {
		return p.name
	}
	return helpers.PersonDisplayName("", "", p.email)
}

// counts are what the plan warns of, each with an example.
type counts struct {
	noTitle, sameKey, cycles, unknownParent, badDates, badEstimates int
	unknownParentEx, badDateEx, badEstimateEx                       string
}

// fieldColumns gives each column that isn't a task's own property its
// field's kind: the admin's choice, else what its values suggest. An
// address column beside a column of people is those people's addresses.
func (r *reading) fieldColumns(chosen map[string]string) {
	kindOf := map[*column]string{}
	guessOf := map[*column]string{}
	for _, c := range r.t.cols {
		if c.prop != "" {
			continue
		}
		_, currency := fieldName(c.name)
		guessOf[c] = guessKind(r.values(c), currency)
		kindOf[c] = guessOf[c]
		if k := chosen[headerKey(c.name)]; k != "" {
			kindOf[c] = k
		}
	}
	people := func(c *column) bool {
		return c.prop == propAssignee || c.prop == propCreatedBy || kindOf[c] == importProvider.FieldPerson
	}
	r.emailCol = map[*column]*column{}
	for _, c := range r.t.cols {
		pc := importProvider.PlanColumn{Name: c.name, Property: string(c.prop), Values: len(r.values(c))}
		switch {
		case c.prop != "":
		case c.emailOf != nil && people(c.emailOf):
			r.emailCol[c.emailOf] = c
			pc.Property = string(propEmail)
		default:
			name, currency := fieldName(c.name)
			pc.Kind, pc.Guess, pc.Currency = kindOf[c], guessOf[c], currency
			if kindOf[c] != importProvider.KindSkip {
				f := importProvider.SourceField{SourceID: headerKey(c.name), Name: name, Type: kindOf[c], Currency: currency}
				f.Options = r.options(c, f.Type)
				r.fields = append(r.fields, fieldColumn{col: c, field: f})
			}
		}
		r.planCols = append(r.planCols, pc)
	}
}

// values are a column's cells with anything in them, a repeated header's
// joined as a list.
func (r *reading) values(c *column) []string {
	var out []string
	for _, row := range r.t.rows {
		if v := strings.Join(row.all(c), ", "); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// options are a select's or multi-select's choices: each label the column
// says, in the order the file first says it, its id the label lower-cased.
func (r *reading) options(c *column, kind string) []importProvider.SourceOption {
	if kind != importProvider.FieldSelect && kind != importProvider.FieldMultiSelect {
		return nil
	}
	var out []importProvider.SourceOption
	seen := map[string]bool{}
	for _, v := range r.values(c) {
		labels := []string{v}
		if kind == importProvider.FieldMultiSelect {
			labels = splitList(v)
		}
		for _, l := range labels {
			if id := strings.ToLower(l); !seen[id] {
				seen[id] = true
				out = append(out, importProvider.SourceOption{SourceID: id, Label: l})
			}
		}
	}
	return out
}

// pairNamesWithAddresses learns each name's address from the rows that give
// both, for the rows that give the name alone.
func (r *reading) pairNamesWithAddresses() {
	for col, emails := range r.emailCol {
		for _, row := range r.t.rows {
			name, email := row.cell(col), row.cell(emails)
			if a, ok := address(email); ok && name != "" {
				key := helpers.PersonNameKey(name)
				if r.emailsOf[key] == nil {
					r.emailsOf[key] = map[string]bool{}
				}
				r.emailsOf[key][a] = true
			}
		}
	}
}

// personID is the source id of the person a row names, by address when the
// file gives one (beside the name, as the cell itself, or beside the same
// name on another row), else by name; "" for no one.
func (r *reading) personID(line int, name, email string) string {
	if a, ok := address(name); ok {
		name, email = "", a
	} else if a, ok := address(email); ok {
		email = a
	} else {
		email = ""
		if known := r.emailsOf[helpers.PersonNameKey(name)]; len(known) == 1 {
			for a := range known {
				email = a
			}
		}
	}
	id := ""
	switch {
	case email != "":
		id = "email:" + email
	case helpers.PersonNameKey(name) != "":
		id = "name:" + helpers.PersonNameKey(name)
	default:
		return ""
	}
	p := r.people[id]
	if p == nil {
		p = &person{email: email}
		r.people[id] = p
		r.order = append(r.order, id)
	}
	if p.name == "" {
		p.name = strings.TrimSpace(name)
	}
	if p.lastLine != line {
		p.rows, p.lastLine = p.rows+1, line
	}
	return id
}

// keyedRow is a row with a title, and its source id.
type keyedRow struct {
	row
	id, title string
}

// rows reads each row with a title as a task: its source id (its key, else
// its line), then its parent among the rows, then the rest of it.
func (r *reading) rows() {
	var rows []keyedRow
	ids := map[string]bool{}
	byRef := map[string]string{}
	byTitle := map[string][]string{}
	for _, row := range r.t.rows {
		title := row.cell(r.by[propTitle])
		if title == "" {
			r.warn.noTitle++
			continue
		}
		key, id := row.cell(r.by[propKey]), row.cell(r.by[propID])
		local := strings.ToLower(helpers.FirstNonEmpty(key, id))
		if local == "" || ids[local] {
			if local != "" {
				r.warn.sameKey++
			}
			local = "line:" + strconv.Itoa(row.line)
		}
		ids[local] = true
		sid := r.t.id + ":" + local
		for _, ref := range []string{key, id} {
			if k := strings.ToLower(ref); k != "" && byRef[k] == "" {
				byRef[k] = sid
			}
		}
		byTitle[helpers.PersonNameKey(title)] = append(byTitle[helpers.PersonNameKey(title)], sid)
		rows = append(rows, keyedRow{row, sid, title})
	}

	r.parentOf = map[string]string{}
	for _, k := range rows {
		ref := k.cell(r.by[propParent])
		if ref == "" {
			continue
		}
		parent := byRef[strings.ToLower(ref)]
		if same := byTitle[helpers.PersonNameKey(ref)]; parent == "" && len(same) == 1 {
			parent = same[0]
		}
		if parent == "" || parent == k.id {
			r.warn.unknownParent++
			r.warn.unknownParentEx = helpers.FirstNonEmpty(r.warn.unknownParentEx, ref)
			continue
		}
		r.parentOf[k.id] = parent
	}
	r.cutCycles(rows)

	for _, k := range rows {
		r.tasks = append(r.tasks, r.task(k.row, k.id, k.title))
	}
}

// cutCycles makes a row whose parents lead back to it a task of its own,
// once a cycle, walking each row's parents once.
func (r *reading) cutCycles(rows []keyedRow) {
	const (
		walking = 1
		done    = 2
	)
	state := map[string]int{}
	for _, k := range rows {
		var path []string
		at := k.id
		for at != "" && state[at] == 0 {
			state[at] = walking
			path = append(path, at)
			at = r.parentOf[at]
		}
		if at != "" && state[at] == walking {
			delete(r.parentOf, path[len(path)-1])
			r.warn.cycles++
		}
		for _, id := range path {
			state[id] = done
		}
	}
}

// task is a row as a task. A date or estimate it can't read is kept in the
// task's description, as a value no field holds.
func (r *reading) task(row row, id, title string) importProvider.SourceTask {
	st := importProvider.SourceTask{
		SourceID:    id,
		Name:        title,
		Description: helpers.PlainTextToHTML(row.cell(r.by[propDescription])),
		Status:      row.cell(r.by[propStatus]),
		Priority:    row.cell(r.by[propPriority]),
		Metadata:    map[string]any{"line": row.line},
	}
	for _, ref := range []string{row.cell(r.by[propKey]), row.cell(r.by[propID])} {
		if _, n := importProvider.SplitKey(ref); n > 0 {
			st.SourceKey = ref
			break
		}
	}
	r.statuses = appendNew(r.statuses, st.Status)
	r.priority = appendNew(r.priority, strings.ToLower(st.Priority))
	for _, cell := range row.all(r.by[propTags]) {
		st.Labels = append(st.Labels, splitList(cell)...)
	}
	if a := r.by[propAssignee]; a != nil {
		if pid := r.personID(row.line, row.cell(a), row.cell(r.emailCol[a])); pid != "" {
			st.AssigneeIds = append(st.AssigneeIds, pid)
		}
	}
	for _, name := range splitList(row.cell(r.by[propAssignees])) {
		if pid := r.personID(row.line, name, ""); pid != "" && !contains(st.AssigneeIds, pid) {
			st.AssigneeIds = append(st.AssigneeIds, pid)
		}
	}
	if c := r.by[propCreatedBy]; c != nil {
		st.CreatedBy = r.personID(row.line, row.cell(c), row.cell(r.emailCol[c]))
	}
	for _, d := range []struct {
		prop property
		day  *importProvider.Day
		at   **time.Time
	}{{propStart, &st.StartDay, &st.StartDate}, {propDue, &st.DueDay, &st.DueDate}} {
		cell := row.cell(r.by[d.prop])
		if cell == "" {
			continue
		}
		day, at, ok := when(cell, r.loc)
		if !ok {
			st.Fields = append(st.Fields, kept(r.by[d.prop], cell))
			r.warn.badDates++
			r.warn.badDateEx = helpers.FirstNonEmpty(r.warn.badDateEx, cell)
			continue
		}
		*d.day, *d.at = day, at
	}
	st.Created, _ = instant(row.cell(r.by[propCreated]), r.loc)
	st.Updated, _ = instant(row.cell(r.by[propUpdated]), r.loc)
	st.Completed = row.cell(r.by[propCompleted]) != ""
	if cell := row.cell(r.by[propEstimate]); cell != "" {
		if hours, ok := number(cell); ok && hours >= 0 {
			st.EstimateMinutes = int(math.Round(hours * 60))
		} else {
			st.Fields = append(st.Fields, kept(r.by[propEstimate], cell))
			r.warn.badEstimates++
			r.warn.badEstimateEx = helpers.FirstNonEmpty(r.warn.badEstimateEx, cell)
		}
	}
	switch kind := row.cell(r.by[propType]); {
	case strings.EqualFold(kind, string(importProvider.TaskMilestone)):
		st.Type = importProvider.TaskMilestone
	case kind != "":
		st.Metadata["type"] = kind
	}
	if link := row.cell(r.by[propLink]); link != "" {
		st.Metadata["link"] = link
	}
	for _, f := range r.fields {
		cell := strings.Join(row.all(f.col), ", ")
		if cell == "" {
			continue
		}
		v := value(f.field, cell, func(name, email string) string { return r.personID(row.line, name, email) })
		switch f.field.Type {
		case importProvider.FieldSelect:
			v.Value = strings.ToLower(cell)
		case importProvider.FieldMultiSelect:
			ids := splitList(cell)
			for i := range ids {
				ids[i] = strings.ToLower(ids[i])
			}
			v.Value = ids
		}
		st.Fields = append(st.Fields, v)
	}
	return st
}

// kept is a cell no field holds: it goes into the task's description, under
// its column's name.
func kept(c *column, cell string) importProvider.SourceFieldValue {
	return importProvider.SourceFieldValue{Field: importProvider.SourceField{SourceID: headerKey(c.name), Name: c.name}, Text: cell}
}

// plan is what the import's confirm step shows of the file.
func (r *reading) plan(tasks, subtasks int) *importProvider.Plan {
	p := &importProvider.Plan{
		ProjectCount:   1,
		TaskCount:      tasks,
		SubtaskCount:   subtasks,
		UserCount:      len(r.order),
		StatusValues:   r.statuses,
		PriorityValues: r.priority,
		Columns:        r.planCols,
		Warnings:       []string{},
	}
	for _, id := range r.order {
		person := r.people[id]
		p.People = append(p.People, importProvider.PlanPerson{SourceID: id, Name: person.shownName(), Email: person.email, Rows: person.rows})
	}
	w := r.warn
	for _, line := range []struct {
		n   int
		msg string
	}{
		{w.noTitle, "%d rows have no title, so they're left out."},
		{w.sameKey, "%d rows repeat another row's key. Each comes in as a task of its own, and a parent naming that key means the first."},
		{w.unknownParent, "%d rows name a parent the file doesn't have (such as " + strconv.Quote(w.unknownParentEx) + "), so they come in as tasks of their own."},
		{w.cycles, "%d groups of rows are each other's parents, so one row of each comes in as a task of its own."},
		{w.badDates, "%d dates couldn't be read (such as " + strconv.Quote(w.badDateEx) + "). Each is kept in its task's description."},
		{w.badEstimates, "%d estimates aren't a number of hours (such as " + strconv.Quote(w.badEstimateEx) + "). Each is kept in its task's description."},
	} {
		if line.n > 0 {
			p.Warnings = append(p.Warnings, fmt.Sprintf(line.msg, line.n))
		}
	}
	return p
}

func appendNew(list []string, v string) []string {
	if v == "" || contains(list, v) {
		return list
	}
	return append(list, v)
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
