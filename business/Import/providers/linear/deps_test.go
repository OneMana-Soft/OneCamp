package linear

import (
	"context"
	"net/http"
	"testing"
)

// I1: a Linear issue another issue blocks waits on it after the import; the
// links were never asked for. A "related" or "duplicate" link isn't a wait.
func TestAnIssueKeepsWhatBlocksIt(t *testing.T) {
	withFakeLinear(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"team":{"issues":{"nodes":[
			{"id":"b","identifier":"ENG-2","title":"Ship","state":{"name":"Todo","type":"unstarted"},
			 "inverseRelations":{"nodes":[
				{"type":"blocks","issue":{"id":"a"}},
				{"type":"related","issue":{"id":"c"}},
				{"type":"duplicate","issue":{"id":"d"}}]}}
		],"pageInfo":{"hasNextPage":false}}}}}`))
	})
	got, err := New().fetchTeamIssues(context.Background(), "tok", "team")
	if err != nil || len(got) != 1 {
		t.Fatalf("issues %+v (%v)", got, err)
	}
	deps := New().issueToSourceTask(got[0], "p").Dependencies
	if len(deps) != 1 || deps[0].BlockerSourceID != "a" {
		t.Fatalf("ENG-2 waits on %+v, want only a", deps)
	}
}

// I1: a Linear issue's estimate is in the team's points, not time, so it
// comes across as a number field.
func TestAnIssueKeepsItsPoints(t *testing.T) {
	five := 5.0
	st := New().issueToSourceTask(linearIssue{ID: "a", Title: "Ship", Estimate: &five}, "p")
	if len(st.Fields) != 1 || st.Fields[0].Field.Name != "Points" || st.Fields[0].Value != 5.0 || st.EstimateMinutes != 0 {
		t.Fatalf("fields %+v, estimate %d", st.Fields, st.EstimateMinutes)
	}
}
