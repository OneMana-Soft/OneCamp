package linear

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// A re-check reads each issue as Linear has it now, by the import's own rules:
// an issue of a private team is kept to the team's members, one of a public
// team to nobody; an issue in no project sits in its team's inbox, as the
// import put it; an issue Linear doesn't return is gone; and Linear refusing
// the token is a refused token.
func TestARecheckReadsEachIssueAsLinearHasItNow(t *testing.T) {
	refuse := false
	withFakeLinear(t, func(w http.ResponseWriter, r *http.Request) {
		if refuse {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(req.Query, "teams("):
			_, _ = w.Write([]byte(`{"data":{"teams":{"nodes":[
				{"id":"t-pub","key":"PUB","name":"Public","visibility":"public","members":{"nodes":[{"id":"u-ann"}]}},
				{"id":"t-priv","key":"PRV","name":"Payroll","visibility":"private","members":{"nodes":[{"id":"u-rae"},{"id":"u-pip"}]}}
			],"pageInfo":{"hasNextPage":false}}}}`))
		case strings.Contains(req.Query, "issues("):
			ids, _ := req.Variables["ids"].([]any)
			if len(ids) != 3 {
				t.Errorf("asked for %v; want the three issues by id", req.Variables["ids"])
			}
			_, _ = w.Write([]byte(`{"data":{"issues":{"nodes":[
				{"id":"i-1","identifier":"PRV-1","team":{"id":"t-priv"},"project":{"id":"p-1"},"assignee":{"id":"u-pip"},"creator":{"id":"u-cy"}},
				{"id":"i-2","identifier":"PUB-2","team":{"id":"t-pub"},"creator":{"id":"u-cy"}}
			],"pageInfo":{"hasNextPage":false}}}}`))
		default:
			t.Errorf("an unexpected query: %s", req.Query)
		}
	})
	got, err := New().recheck(context.Background(), "tok", []string{"i-1", "i-2", "i-9"})
	if err != nil {
		t.Fatal(err)
	}
	i1 := got["i-1"].Task
	if i1.Restricted == "" || !slices.Equal(i1.Audience, []string{"u-rae", "u-pip"}) || i1.ProjectSourceID != "p-1" ||
		i1.CreatedBy != "u-cy" || !reflect.DeepEqual(i1.AssigneeIds, []string{"u-pip"}) {
		t.Errorf("i-1, of a private team, reads as %+v", i1)
	}
	if i2 := got["i-2"]; i2.Gone || i2.Task.Restricted != "" || i2.Task.ProjectSourceID != inboxProjectIDPrefix+"t-pub" {
		t.Errorf("i-2, of a public team in no project, reads as %+v", i2)
	}
	if !got["i-9"].Gone {
		t.Errorf("i-9, which Linear doesn't return, reads as %+v; want gone", got["i-9"])
	}
	refuse = true
	if _, err := New().recheck(context.Background(), "tok", []string{"i-1"}); !errors.Is(err, importProvider.ErrTokenRejected) {
		t.Errorf("Linear refusing the token: %v; want a refused token", err)
	}
}
