package business

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	reviewModel "github.com/akashc777/OneCamp/models/postgres/ImportReview"
)

// The admins' card groups an import's reviews by project: a project's two
// gaps add up, it and its import stand where their least settled review does,
// an archived project is listed as one, and only counts and task ids leave
// the server: never a pass's cursor, tries or who re-checked, and the tasks a
// pass failed on only once they keep failing.
func TestTheAdminsCardGroupsAnImportsReviewsAndShowsOnlyCounts(t *testing.T) {
	older, newer := uuid.New(), uuid.New()
	launch, archive, gone, failing := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	decided := time.Date(2026, 10, 25, 20, 0, 0, 0, time.UTC)
	row := func(imp, project uuid.UUID, name string, archived bool, gap, state, summary string) reviewModel.AccessImportRow {
		return reviewModel.AccessImportRow{
			AccessReview: reviewModel.AccessReview{ID: uuid.New(), ImportID: imp, Provider: "jira", ProjectUUID: project,
				Gap: gap, State: state, Summary: json.RawMessage(summary)},
			Site: "acme.atlassian.net", RanBy: "Ada", ProjectName: name, Archived: archived,
		}
	}
	kept := row(older, uuid.New(), "Old", false, reviewModel.GapJiraSecurity, reviewModel.StateKept, `{"restricted": 1}`)
	kept.DecidedAt, kept.DecidedByName = &decided, "Ana"
	rows := []reviewModel.AccessImportRow{
		row(newer, launch, "Launch", false, reviewModel.GapJiraComments, reviewModel.StateRechecked, `{"comments_hidden": 2}`),
		row(newer, launch, "Launch", false, reviewModel.GapJiraSecurity, reviewModel.StateNeedsAdmin,
			`{"restricted": 4, "gone_or_hidden": 1, "gone_or_hidden_tasks": ["`+gone.String()+`"], "failed": ["`+failing.String()+`"],
			  "reason": "source_failing", "retries": 3, "failures": 5, "checked_by": "`+uuid.NewString()+`",
			  "cursor": {"entity": "task", "source": "ACME-7"}}`),
		row(newer, archive, "Archive", true, reviewModel.GapJiraSecurity, reviewModel.StateRechecked,
			`{"restricted": 2, "failed": ["`+failing.String()+`"], "reason": "tasks_failing"}`),
		kept,
	}
	got := importsOf(rows)
	if len(got) != 2 || got[0].ImportID != newer || got[1].ImportID != older {
		t.Fatalf("imports %+v, want the newer then the older", got)
	}
	n := got[0]
	if n.State != reviewModel.StateNeedsAdmin || n.Reason != "source_failing" || n.Site != "acme.atlassian.net" || n.RanBy != "Ada" {
		t.Errorf("the newer import: %+v", n)
	}
	if len(n.Projects) != 2 {
		t.Fatalf("its projects: %+v", n.Projects)
	}
	l, a := n.Projects[0], n.Projects[1]
	if l.ProjectUUID != launch || l.State != reviewModel.StateNeedsAdmin || l.Restricted != 4 || l.CommentsHidden != 2 ||
		len(l.GoneOrHiddenTasks) != 1 || l.GoneOrHiddenTasks[0] != gone || len(l.Failed) != 0 {
		t.Errorf("Launch, its two gaps together: %+v", l)
	}
	if a.ProjectUUID != archive || !a.Archived || a.State != reviewModel.StateRechecked || a.Restricted != 2 ||
		len(a.Failed) != 1 || a.Failed[0] != failing {
		t.Errorf("the archived project, whose tasks keep failing: %+v", a)
	}
	if o := got[1]; o.State != reviewModel.StateKept || o.DecidedBy != "Ana" || o.DecidedAt == nil || !o.DecidedAt.Equal(decided) {
		t.Errorf("the kept import: %+v", o)
	}
	out, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, internal := range []string{"cursor", "ACME-7", "retries", "failures", "checked_by", `"summary"`} {
		if strings.Contains(string(out), internal) {
			t.Errorf("the card carries %s: %s", internal, out)
		}
	}
	if none, err := json.Marshal(importsOf(nil)); err != nil || string(none) != "[]" {
		t.Errorf("no import reads as %s to the web, want []", none)
	}
}
