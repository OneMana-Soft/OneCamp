package jira

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	"github.com/akashc777/OneCamp/helpers"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
)

// Jira's sprints (jira-16) come over as the project's cycles. Jira keeps
// sprints on boards, and a project can have several; an issue's Sprint field
// lists every sprint it has been in, closed ones too. A project gets the
// sprints that hold its issues here, each issue goes into its own
// (sprintOf), and sprints of several boards that share days fold into one
// cycle (business/Import placeCycles).

// jiraSprint is one sprint as the Sprint field gives it.
type jiraSprint struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	State        string `json:"state"` // closed, active or future
	StartDate    string `json:"startDate"`
	EndDate      string `json:"endDate"`
	CompleteDate string `json:"completeDate"`
}

// sprintDays is how long a sprint Jira gives no dates runs here when no
// sprint of the project has dates: two weeks, Jira's own first length.
const sprintDays = 14

func (s *jiraSprint) closed() bool { return strings.EqualFold(s.State, "closed") }

// closedAt is when the sprint was closed: its completeDate, else its end.
func (s *jiraSprint) closedAt() time.Time {
	if t := parseJiraTime(s.CompleteDate); !t.IsZero() {
		return t
	}
	return parseJiraTime(s.EndDate)
}

// sourceID is the sprint's id among the import's cycles.
func (s *jiraSprint) sourceID() string { return strconv.FormatInt(s.ID, 10) }

// name is the sprint's name, else "Sprint" and its id.
func (s *jiraSprint) name() string {
	if n := strings.TrimSpace(s.Name); n != "" {
		return n
	}
	return "Sprint " + s.sourceID()
}

// done says whether the issue is finished: its status's category is done,
// whatever the status is called. A status without its category goes by its
// name.
func (i *jiraIssue) done() bool {
	if k := i.Fields.Status.Category.Key; k != "" {
		return strings.EqualFold(k, "done")
	}
	return isDoneStatus(i.Fields.Status.Name)
}

// sprintOf is the sprint an issue is in here: the one it is in now (active,
// or future when it's planned for one), else, for a finished issue, the last
// one it was in, which finished it. An unfinished issue whose sprints have
// all closed is back in the backlog in Jira, and in no cycle here; so is a
// sub-task, which comes inside its task. nil for none. The field is read as
// Jira Cloud gives it, a list of objects; Server and Data Center give each
// sprint as a string ("…Sprint@…[id=…]"), which reads as none here.
func (i *jiraIssue) sprintOf(field string) *jiraSprint {
	if field == "" || i.Fields.IssueType.Subtask {
		return nil
	}
	var sprints []jiraSprint
	if json.Unmarshal(i.Custom[field], &sprints) != nil {
		return nil
	}
	var last *jiraSprint
	for k := range sprints {
		s := &sprints[k]
		if !s.closed() {
			return s
		}
		if last == nil || s.closedAt().After(last.closedAt()) {
			last = s
		}
	}
	if last == nil || !i.done() {
		return nil
	}
	return last
}

// sprintTally gathers the sprints that hold a project's issues, in the order
// they first come, and how many issues each closed one finished.
type sprintTally struct {
	order []int64
	by    map[int64]*talliedSprint
}

type talliedSprint struct {
	sprint jiraSprint
	done   int
}

// add counts an issue in its sprint, if it has one here.
func (t *sprintTally) add(iss *jiraIssue, field string) {
	s := iss.sprintOf(field)
	if s == nil {
		return
	}
	if t.by == nil {
		t.by = map[int64]*talliedSprint{}
	}
	e, ok := t.by[s.ID]
	if !ok {
		e = &talliedSprint{sprint: *s}
		t.by[s.ID] = e
		t.order = append(t.order, s.ID)
	}
	if s.closed() {
		e.done++
	}
}

// cycles are the tallied sprints as the project's cycles. A sprint Jira
// gives no dates (one that hasn't started) follows the dated one that ends
// last, or now when that has ended, and runs as long as the dated one that
// started last (two weeks when none has dates), in the order Jira made them.
// undated is how many there were. Pure.
func (t *sprintTally) cycles(now time.Time) (out []importProvider.SourceCycle, undated int) {
	var later []jiraSprint
	var lastEnd, lastStart time.Time
	length := sprintDays * 24 * time.Hour
	for _, id := range t.order {
		e := t.by[id]
		start, end := parseJiraTime(e.sprint.StartDate), parseJiraTime(e.sprint.EndDate)
		if start.IsZero() || !end.After(start) {
			later = append(later, e.sprint)
			continue
		}
		c := importProvider.SourceCycle{SourceID: e.sprint.sourceID(), Name: e.sprint.name(), StartsAt: start, EndsAt: end}
		if e.sprint.closed() {
			at := e.sprint.closedAt()
			c.CompletedAt, c.Done = &at, e.done
		}
		out = append(out, c)
		if end.After(lastEnd) {
			lastEnd = end
		}
		if start.After(lastStart) {
			lastStart, length = start, end.Sub(start)
		}
	}
	slices.SortFunc(later, func(a, b jiraSprint) int { return cmp.Compare(a.ID, b.ID) })
	from := lastEnd
	if from.Before(now) {
		from = now
	}
	for _, s := range later {
		out = append(out, importProvider.SourceCycle{SourceID: s.sourceID(), Name: s.name(), StartsAt: from, EndsAt: from.Add(length)})
		from = from.Add(length)
	}
	return out, len(later)
}

// projectSprints are the sprints that hold a project's issues, as its
// cycles, read before its tasks come, since a project makes its cycles first
// (business/Import makeCycles). A site without the Sprint field has none.
// Each project's are kept for the job: the projects' stage is tried again
// from its first project, and one read before is neither searched nor
// warned about again.
func (p *Provider) projectSprints(ctx context.Context, j *importModels.Job, tok, site, projectKey, field string) ([]importProvider.SourceCycle, error) {
	if field == "" {
		return nil, nil
	}
	p.mu.Lock()
	cycles, read := p.sprintsCache[j.Id][projectKey]
	p.mu.Unlock()
	if read {
		return cycles, nil
	}
	cycles, err := p.searchSprints(ctx, j, tok, site, projectKey, field)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	if p.sprintsCache[j.Id] == nil {
		p.sprintsCache[j.Id] = map[string][]importProvider.SourceCycle{}
	}
	p.sprintsCache[j.Id][projectKey] = cycles
	p.mu.Unlock()
	return cycles, nil
}

// searchSprints searches the project's issues that are in a sprint, asking
// only their sprints, status and type. A search Jira refuses (a token that
// may not read it, or a site that can't search the field) is said once, and
// the project comes without cycles; Jira asking to slow down, a server error
// or a timeout is the chunk's to try again.
func (p *Provider) searchSprints(ctx context.Context, j *importModels.Job, tok, site, projectKey, field string) ([]importProvider.SourceCycle, error) {
	var tally sprintTally
	jql := fmt.Sprintf(`project = "%s" AND cf[%s] is not EMPTY`, projectKey, strings.TrimPrefix(field, "customfield_"))
	err := p.searchIssues(ctx, tok, site, jql, []string{field, "status", "issuetype"}, false, func(iss *jiraIssue) bool {
		tally.add(iss, field)
		return true
	})
	if err != nil {
		var status *importProvider.StatusError
		if !errors.As(err, &status) || (status.Code != 400 && status.Code != 401 && status.Code != 403 && status.Code != 404) {
			return nil, err
		}
		importModels.LogImportError(ctx, j.Id, nil, importModels.EntityCycle, projectKey, importModels.SeverityWarning, "SPRINTS_UNREADABLE",
			"Jira wouldn't list this project's sprints, so its issues came without them: "+err.Error(), nil)
		return nil, nil
	}
	cycles, undated := tally.cycles(time.Now())
	if undated > 0 {
		importModels.LogImportError(ctx, j.Id, nil, importModels.EntityCycle, projectKey, importModels.SeverityWarning, "SPRINTS_DATED",
			helpers.Count(undated, "sprint that hadn't started has", "sprints that hadn't started have")+" no dates in Jira. Each follows the one before it here, with its issues; change the dates to suit.", nil)
	}
	return cycles, nil
}
