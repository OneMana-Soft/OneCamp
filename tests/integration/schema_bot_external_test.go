//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/tests/integration"
)

// A graph just dropped and given the schema, as a new install's is and as the
// shared harness leaves one between tests, answers filters on is_bot and
// is_external: both are declared with a bool index (F-24), so no filter waits
// on some node having written them first.
//
// WHY: the people, channel, call and principal reads filter on is_external.
// Before the declaration the predicate existed only once a node carried it (in
// production, the shared bot written at startup), and on a graph without one
// those reads failed with "Schema not defined for predicate: is_external". On
// v2.73.1's line, which predates the declaration, the shared harness's drop
// between tests failed 38 tests that way (17 Oct 2026).
func TestFiltersOnBotAndExternalAnswerOnANewGraph(t *testing.T) {
	integration.SetupDgraph(t)
	ctx := context.Background()

	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().Query(ctx, `schema(pred: [is_bot, is_external]) { type index tokenizer }`)
	if err != nil {
		t.Fatalf("schema query: %v", err)
	}
	var got struct {
		Schema []struct {
			Predicate string   `json:"predicate"`
			Type      string   `json:"type"`
			Index     bool     `json:"index"`
			Tokenizer []string `json:"tokenizer"`
		} `json:"schema"`
	}
	if err := json.Unmarshal(resp.Json, &got); err != nil {
		t.Fatalf("schema answer %s: %v", resp.Json, err)
	}
	declared := map[string]bool{}
	for _, p := range got.Schema {
		if p.Type == "bool" && p.Index && len(p.Tokenizer) == 1 && p.Tokenizer[0] == "bool" {
			declared[p.Predicate] = true
		}
	}
	for _, pred := range []string{"is_bot", "is_external"} {
		if !declared[pred] {
			t.Errorf("%s isn't declared bool @index(bool) on a new graph (schema answer: %s)", pred, resp.Json)
		}
	}

	// The filter the people and conversation reads use, on a graph no node has
	// been written to.
	q := `{ people(func: has(user_uuid)) @filter(NOT eq(is_external, true) OR eq(is_bot, true)) { uid } }`
	if _, err := dgraphInit.DgraphClient.NewReadOnlyTxn().Query(ctx, q); err != nil {
		t.Fatalf("a filter on is_external and is_bot failed on a new graph: %v", err)
	}
}
