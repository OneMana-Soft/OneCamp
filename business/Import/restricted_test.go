package business

import (
	"slices"
	"strings"
	"testing"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

// Of a restricted task's people in its source, only those matched by email
// take part here: its assignee stays its assignee and its reporter is
// granted. One matched by name alone, or who didn't come across, isn't let
// in, and its creator is one of those who take part, or else whoever ran the
// import. Each one left out is named with what they were, but whoever ran
// the import, who sees it anyway.
func TestOnlyPeopleMatchedByEmailTakePartInARestrictedTask(t *testing.T) {
	ada, rae, al := uuid.New(), uuid.New(), uuid.New()
	matched := map[string]importModels.PersonMatch{
		"ada": {ID: ada, Name: "Ada", ByEmail: true},
		"rae": {ID: rae, Name: "Rae", ByEmail: true},
		"al":  {ID: al, Name: "Al", ByEmail: true},
		"cy":  {ID: uuid.New(), Name: "Cy", ByEmail: true},
		"nia": {ID: uuid.New(), Name: "Nia"},
		"ned": {ID: uuid.New(), Name: "Ned"},
	}
	for _, c := range []struct {
		name              string
		src               sourcePeople
		assignee, creator bool
		grants            []uuid.UUID
		left              string // as:source, in order
	}{
		{"its creator, for having made it", sourcePeople{Creator: "cy", Assignee: "al", Audience: []string{"rae"}}, true, false, []uuid.UUID{rae}, "creator:cy"},
		{"its creator as its reporter", sourcePeople{Creator: "rae", Assignee: "al", Audience: []string{"rae"}}, true, true, []uuid.UUID{rae}, ""},
		{"its creator as its assignee", sourcePeople{Creator: "al", Assignee: "al"}, true, true, nil, ""},
		{"people matched by name alone", sourcePeople{Creator: "nia", Assignee: "ned", Audience: []string{"nia"}}, false, false, nil, "assignee:ned audience:nia creator:nia"},
		{"people who didn't come across", sourcePeople{Creator: "zed", Assignee: "zoe", Audience: []string{"zia"}}, false, false, nil, "assignee:zoe audience:zia creator:zed"},
		{"whoever ran the import", sourcePeople{Creator: "ada"}, false, false, nil, ""},
		{"no one", sourcePeople{}, false, false, nil, ""},
	} {
		s := settle(c.src, matched, ada)
		var left []string
		for _, l := range s.left {
			left = append(left, l.As+":"+l.Source)
		}
		if s.assignee != c.assignee || s.creator != c.creator || !slices.Equal(s.grants, c.grants) || strings.Join(left, " ") != c.left {
			t.Errorf("%s: assignee %v, creator %v, grants %v, left %q", c.name, s.assignee, s.creator, s.grants, strings.Join(left, " "))
		}
	}
}

// The report's line for a restricted task says why and who isn't let in, by
// their names here, so the admin can share it on purpose.
func TestARestrictedTasksReportLineNamesWhoIsntLetIn(t *testing.T) {
	job := &importModels.Job{Provider: importModels.ProviderJira}
	why := restrictedWhy(job, &importProvider.SourceTask{Restricted: "an issue security level"})
	n := restrictedNote(job, nil, importModels.EntityTask, "HR-4", why, []leftOut{{As: "creator", Source: "cy", Name: "Cy"}, {As: "audience", Source: "zia"}})
	for _, want := range []string{"Jira keeps it to some people (an issue security level)", "Cy (who made it)", "someone who didn't come across (one Jira lets see it)"} {
		if !strings.Contains(n.Message, want) {
			t.Errorf("the line doesn't say %q: %s", want, n.Message)
		}
	}
	if n.SourceID != "HR-4" || n.Code != "TASK_RESTRICTED" {
		t.Errorf("the line: %+v", n)
	}
}

// The report counts a task's comments that stayed in their source, and why,
// on one line for the task, without their text; a task with none has none.
func TestTheReportCountsTheCommentsThatStayedInTheSource(t *testing.T) {
	job := &importModels.Job{Provider: importModels.ProviderJira}
	if n := commentsKeptNote(job, nil, "HR-5", nil); n != nil {
		t.Errorf("a line for no comments: %+v", n)
	}
	n := commentsKeptNote(job, nil, "HR-5", []string{"a project role or group", "an internal note", "a project role or group"})
	if len(n) != 1 || n[0].SourceID != "HR-5" || n[0].Code != "COMMENTS_RESTRICTED" ||
		!strings.Contains(n[0].Message, "(a project role or group; an internal note)") || !strings.Contains(n[0].Message, "Left there: 3.") {
		t.Errorf("the line: %+v", n)
	}
}
