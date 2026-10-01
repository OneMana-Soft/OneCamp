package domain

// Coding-agent run scorecard reads — domain layer.
//
// Follows the repo convention: the QUERY is built here (domain) and EXECUTED by
// the model (models/postgres/AI). This is the admin reliability view's signal
// scan; the optional since-window is the only dynamic bit, so it belongs here.

import (
	"context"

	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
)

// ListCodePRRunSignals returns the scorecard signals for runs created within the
// last sinceDays days (0 => all time), newest first, capped at limit
// (<=0 => 500). Read-only + bounded so the admin reliability view is a cheap
// scan regardless of ledger size.
func ListCodePRRunSignals(ctx context.Context, sinceDays, limit int) ([]aiModels.CodePRRunSignal, error) {
	if limit <= 0 || limit > 5000 {
		limit = 500
	}

	query := `SELECT status, pr_url, all_passed, had_tests, in_scope, draft, outcome
	        FROM code_pr_runs`
	args := []interface{}{}
	if sinceDays > 0 {
		query += ` WHERE created_at >= NOW() - ($1 || ' days')::interval`
		args = append(args, sinceDays)
		query += ` ORDER BY created_at DESC LIMIT $2`
		args = append(args, limit)
	} else {
		query += ` ORDER BY created_at DESC LIMIT $1`
		args = append(args, limit)
	}
	return aiModels.ExecCodePRRunSignals(ctx, query, args)
}
