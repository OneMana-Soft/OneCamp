package business_test

// The browser's view rules mean what the server's do (tables T5c), so a
// table shows the same rows, in the same order, on either side of the rows
// the browser sorts itself. testdata/rowquery/meanings.json holds one table
// in the API's shapes, cases of a view's rules as the browser keeps them, the
// spec each maps to (the web's lib/tables/query.ts rulesToSpec) and the rows
// each shows in order. The web's meanings.test.ts holds rulesToSpec and
// applyViewRules to it; this holds the evaluator, which the SQL is held to
// (TestRowQueryMeansWhatItsEvaluatorMeans, and meanings_integration_test.go
// for this table), to the same ids. Both repos hold the same bytes.
//
// WRITE_MEANINGS=<file> writes the evaluator's ids for each case to <file>,
// for the fixture's generator to take in.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	query "github.com/akashc777/OneCamp/business/QuerySpec"
	rowQuery "github.com/akashc777/OneCamp/business/RowQuery"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// meaningCase is one of the fixture's cases: its spec and the rows it shows.
type meaningCase struct {
	Name string     `json:"name"`
	Spec query.Spec `json:"spec"`
	IDs  []string   `json:"ids"`
}

// meanings is the fixture.
type meanings struct {
	Now    time.Time      `json:"now"`
	Zone   string         `json:"zone"`
	Me     uuid.UUID      `json:"me"`
	Table  uuid.UUID      `json:"table"`
	Fields []*model.Field `json:"fields"`
	Rows   []*model.Row   `json:"rows"`
	Cases  []meaningCase  `json:"cases"`
}

func readMeanings(t *testing.T) (meanings, *time.Location) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "rowquery", "meanings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m meanings
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	zone, err := time.LoadLocation(m.Zone)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Cases) == 0 || len(m.Rows) == 0 {
		t.Fatal("the fixture holds no cases or no rows")
	}
	return m, zone
}

func TestTheBrowsersRulesMeanWhatTheServersDo(t *testing.T) {
	m, zone := readMeanings(t)
	write := os.Getenv("WRITE_MEANINGS")
	var got []meaningCase
	for _, c := range m.Cases {
		ids, _, _, err := rowQuery.Oracle(c.Spec, m.Fields, m.Rows, m.Me, m.Now, zone)
		if err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		if ids == nil {
			ids = []string{}
		}
		got = append(got, meaningCase{Name: c.Name, IDs: ids})
		if write == "" && !slices.Equal(ids, c.IDs) {
			t.Errorf("%s: the evaluator shows\n%v\nthe fixture says\n%v", c.Name, ids, c.IDs)
		}
	}
	if write != "" {
		b, _ := json.Marshal(map[string]interface{}{"cases": got})
		if err := os.WriteFile(write, b, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %d cases' ids to %s", len(got), write)
	}
}
