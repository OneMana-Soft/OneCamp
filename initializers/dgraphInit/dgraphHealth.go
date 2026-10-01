package dgraphInit

// Is the graph answering, and does it know the schema?
//
// WHAT LIVES HERE. Tasks, projects, docs, boards and every membership edge
// between them. A Dgraph that is reachable but has no schema answers queries
// perfectly happily with nothing in them, so the product looks empty rather than
// broken: no projects, no tasks, and no error anywhere.
//
// Asking for a predicate the product cannot work without separates the two. A
// fresh install that has genuinely never stored anything still has the schema,
// because the schema is created at startup, not on first write.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/akashc777/OneCamp/helpers"
)

func init() {
	helpers.RegisterSystemCheck(helpers.SystemCheck{
		Name: "graph",
		Kind: helpers.CheckKindDependency,
		Describe: "Dgraph answers and the schema this build needs is present. It does not prove any " +
			"particular task or project is intact, only that the store behind them is working.",
		Probe: func(ctx context.Context) error {
			if DgraphClient == nil {
				return fmt.Errorf("no Dgraph client: the graph store was never initialised, so tasks, projects, docs and boards cannot load")
			}

			res, err := DgraphClient.NewReadOnlyTxn().Query(ctx, `schema(pred: [user_uuid, task_uuid]) { type }`)
			if err != nil {
				return fmt.Errorf("Dgraph did not answer: %w", err)
			}

			var parsed struct {
				Schema []struct {
					Predicate string `json:"predicate"`
				} `json:"schema"`
			}
			if uerr := json.Unmarshal(res.Json, &parsed); uerr != nil {
				return fmt.Errorf("Dgraph answered with something unreadable: %w", uerr)
			}
			if len(parsed.Schema) == 0 {
				return fmt.Errorf("Dgraph is reachable but has no schema for the predicates this build " +
					"needs, so every project, task, doc and board will load as empty rather than as an error")
			}
			return nil
		},
	})
}
