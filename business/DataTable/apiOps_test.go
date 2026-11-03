package business

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"

	v1api "github.com/akashc777/OneCamp/business/PublicAPI"
	query "github.com/akashc777/OneCamp/business/QuerySpec"
	rowLedger "github.com/akashc777/OneCamp/business/RowLedger"
	rowQuery "github.com/akashc777/OneCamp/business/RowQuery"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// A query the table won't answer is the caller's to fix: a 400 in the API's
// one error body, in the query's own words, never a 500 (main 12:4x, (a)).
// Rows still being worked out, or rows that couldn't be read just now, are
// the server's: a 503 that says when to ask again.
func TestAQueryTheTableRefusesIsTheCallersToFix(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
		code   string
		words  string
	}{
		{fmt.Errorf("%w: %s", query.ErrBadSpec, `"resembles" isn't an operator`), http.StatusBadRequest, "invalid_argument", `"resembles" isn't an operator`},
		{fmt.Errorf("filtering: %w", fmt.Errorf("%w: there's no field %q in this table", query.ErrBadSpec, "5f0c")), http.StatusBadRequest, "invalid_argument", `There's no field "5f0c" in this table`},
		{fmt.Errorf("%w: %s", rowQuery.ErrCantQuery, "a number can't be compared with contains"), http.StatusBadRequest, "invalid_argument", "A number can't be compared with contains"},
		{rowQuery.ErrBadCursor, http.StatusBadRequest, "invalid_argument", "That page marker isn't this query's"},
		{rowQuery.ErrTooManyToWorkOut, http.StatusBadRequest, "invalid_argument", rowQuery.Refusal(rowQuery.ErrTooManyToWorkOut)},
		{fmt.Errorf("sorting: %w", &rowQuery.StillWorkingOut{Field: "Double", Done: 42000, Total: 100000}), http.StatusServiceUnavailable, "working_out", "Double is being worked out over 100,000 rows: 42% done"},
		{errRowsUnavailable, http.StatusServiceUnavailable, "unavailable", "the rows couldn't be read just now"},
	} {
		var e *v1api.Error
		if !errors.As(apiErr(c.err), &e) {
			t.Errorf("%v: answered as %v, which the API says as a 500", c.err, apiErr(c.err))
			continue
		}
		if e.Status != c.status || e.Code != c.code || e.Message != c.words {
			t.Errorf("%v: answered %d %s %q, want %d %s %q", c.err, e.Status, e.Code, e.Message, c.status, c.code, c.words)
		}
		if retry := e.RetryAfter > 0; retry != (c.status == http.StatusServiceUnavailable) {
			t.Errorf("%v: Retry-After %d: only the server's own refusal says when to ask again", c.err, e.RetryAfter)
		}
	}
}

// A token's call acts as its member, never with the member's admin powers
// (helpers.ActsThroughToken): an admin's token opens what a member opens.
func TestATokensCallActsAsAMemberEvenForAnAdmin(t *testing.T) {
	admin := userModels.UserInfo{}
	admin.UserPostgresInfo.Id = uuid.New()
	admin.UserPostgresInfo.IsAdmin = true
	ctx := context.WithValue(context.Background(), helpers.UserInfoContextKey, admin)
	ctx = context.WithValue(ctx, helpers.ApiTokenIDContextKey, uuid.NewString())
	a, err := apiActor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if a.UserID != admin.UserPostgresInfo.Id || a.IsAdmin {
		t.Errorf("an admin's token acts as %+v, want them as a member (IsAdmin false)", a)
	}
	if _, err := apiActor(context.Background()); err == nil {
		t.Error("a call with nobody on its context acts for someone")
	}
}

// Every write through the API says who made it and how, in the row ledger
// (main 12:4x, (b); condition e): the token's member, and the agent the token
// is bound to with the agent's kind, or the API's kind for a plain token.
func TestATokensWriteSaysWhoMadeItAndHow(t *testing.T) {
	member := Actor{UserID: uuid.New()}
	plain := context.WithValue(context.Background(), helpers.ApiTokenIDContextKey, uuid.NewString())
	by := apiBy(plain, member)
	if by.Who.User != member.UserID || by.Who.Agent != uuid.Nil || by.Kind != rowLedger.ActorAPI {
		t.Errorf("a plain token's write is by %+v, want %s, no agent, kind %q", by, member.UserID, rowLedger.ActorAPI)
	}
	agent := uuid.New()
	bound := context.WithValue(plain, helpers.ApiAgentIDContextKey, agent.String())
	by = apiBy(bound, member)
	if by.Who.User != member.UserID || by.Who.Agent != agent || by.Kind != rowLedger.ActorAgent {
		t.Errorf("an agent's token's write is by %+v, want %s for agent %s, kind %q", by, member.UserID, agent, rowLedger.ActorAgent)
	}
}

// A batch the table refuses writes nothing and is the caller's to fix (tables
// H2a): a 400 whose details name each row it refused and why, as the app's
// batch answer does. The batch's own rules (how many rows, each named once,
// something to do) are the caller's too, in their own words, never a 500.
func TestARefusedBatchNamesItsRows(t *testing.T) {
	row, at := uuid.New(), 1
	var e *v1api.Error
	if !errors.As(apiErr(BatchErrors{{Row: row, Field: "f1", Msg: "Points takes a number"}, {Index: &at, Msg: "Name is too long"}}), &e) {
		t.Fatal("a refused batch is answered as a 500")
	}
	details, _ := e.Details.([]apiRowError)
	if e.Status != http.StatusBadRequest || e.Code != "invalid_argument" || len(details) != 2 ||
		details[0] != (apiRowError{Row: row.String(), Field: "f1", Message: "Points takes a number"}) ||
		details[1].Index == nil || *details[1].Index != 1 || details[1].Row != "" || details[1].Message != "Name is too long" {
		t.Errorf("a refused batch answered %d %s %q with %+v; want 400 invalid_argument naming each row", e.Status, e.Code, e.Message, e.Details)
	}
	for _, c := range []struct {
		err   error
		words string
	}{
		{badInput("A batch writes at most %d rows: this one names %d", maxBatch, maxBatch+1), "A batch writes at most 1000 rows: this one names 1001"},
		{fmt.Errorf("restoring: %w", badInput("name the rows or fields to bring back")), "name the rows or fields to bring back"},
	} {
		if !errors.As(apiErr(c.err), &e) || e.Status != http.StatusBadRequest || e.Code != "invalid_argument" || e.Message != c.words {
			t.Errorf("%v: answered %v; want 400 invalid_argument %q", c.err, apiErr(c.err), c.words)
		}
	}
}

// A batch with a stale row is refused as a batch: over the API its details
// name every row it refused, the stale one and any refused beside it, while
// the app keeps its conflict (409, with the row as it is now). On 2 Nov a
// stale one-row tables_rows_batch over MCP answered version_mismatch with no
// details, and a row refused before a stale one went unnamed.
func TestAStaleBatchNamesEveryRowItRefuses(t *testing.T) {
	big, stale := uuid.New(), uuid.New()
	now := &model.Row{Id: stale}
	for _, c := range []struct {
		name string
		rows model.RowErrors
		want []apiRowError
	}{
		{"one stale row", model.RowErrors{{Row: stale, Err: model.ErrVersion, Current: now}},
			[]apiRowError{{Row: stale.String(), Message: errVersion.Error()}}},
		{"a row too large, then a stale one", model.RowErrors{{Row: big, Err: model.ErrRowTooLarge}, {Row: stale, Err: model.ErrVersion, Current: now}},
			[]apiRowError{{Row: big.String(), Message: "row is too large"}, {Row: stale.String(), Message: errVersion.Error()}}},
	} {
		refused := batchErr(c.rows)
		var conflict *RowConflict
		if !errors.As(refused, &conflict) || conflict.Row != now || !IsConflict(refused) {
			t.Errorf("%s: the app has %v; want its conflict with the row as it is now (409)", c.name, refused)
		}
		var e *v1api.Error
		if !errors.As(apiErr(refused), &e) {
			t.Fatalf("%s: answered as a 500", c.name)
		}
		details, _ := e.Details.([]apiRowError)
		if e.Status != http.StatusBadRequest || e.Code != "invalid_argument" || !slices.Equal(details, c.want) {
			t.Errorf("%s: the API answered %d %s with %+v; want 400 invalid_argument naming %+v", c.name, e.Status, e.Code, e.Details, c.want)
		}
	}
}

// A single row's write is a batch of one behind PATCH /rows/{id} with
// If-Match: stale, it is refused for its version (412 version_mismatch, the
// app's 409 with the row as it is now), never as a batch with details.
func TestAStaleSingleRowWriteIsRefusedForItsVersion(t *testing.T) {
	stale := uuid.New()
	now := &model.Row{Id: stale}
	refused := oneRow(batchErr(model.RowErrors{{Row: stale, Err: model.ErrVersion, Current: now}}))
	var conflict *RowConflict
	if !errors.As(refused, &conflict) || conflict.Row != now || len(conflict.Rows) != 0 || !IsConflict(refused) {
		t.Errorf("a stale single row's write is %#v; want the app's conflict with the row as it is now, and no batch rows", refused)
	}
	if got := apiErr(refused); !errors.Is(got, v1api.ErrVersion) {
		t.Errorf("a stale single row's write answered %v over the API; want 412 version_mismatch", got)
	}
	tooLarge := BatchErrors{{Row: stale, Msg: "row is too large"}}
	if got, ok := oneRow(tooLarge).(BatchErrors); !ok || !slices.Equal(got, tooLarge) {
		t.Errorf("oneRow changed a refusal that isn't a conflict: %v", got)
	}
}
