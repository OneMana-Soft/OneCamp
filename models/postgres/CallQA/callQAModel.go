package models

// A call's questions and their upvotes (calls package A; migration
// *_call_activities). Every read and write is scoped by the call's session, so
// a question id from another call answers nothing.

import (
	"context"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
)

// Question is one call_questions row, with its upvotes as one reader reads them.
type Question struct {
	ID        uuid.UUID
	SessionID uuid.UUID
	// Asker is the asker's call identity, or for an anonymous question a key
	// no one can turn back into a person (business/CallQA).
	Asker     string
	AskerName string
	Anonymous bool
	Body      string
	State     string
	Pinned    bool
	CreatedAt time.Time
	// Votes is how many upvoted it; Voted whether the reader did, read by the
	// key their upvotes are kept under (Upvote).
	Votes int
	Voted bool
}

// ErrNotFound is a question that isn't the call's.
var ErrNotFound = errors.New("callqa: no such question")

// Insert stores a new question, and fills in its state and time, when its
// call has fewer than limit (dismissed ones included); it says whether it did.
func Insert(ctx context.Context, q *Question, limit int) (bool, error) {
	return pgexec.Capped(ctx, "call_questions:"+q.SessionID.String(), limit,
		func(ctx context.Context) (int, error) {
			return pgexec.Scalar[int](ctx, `SELECT count(*) FROM call_questions WHERE session_id = $1`, q.SessionID)
		},
		func(ctx context.Context) error {
			_, _, err := pgexec.QueryOne(ctx, `
				INSERT INTO call_questions (id, session_id, asker, asker_name, anonymous, body)
				VALUES ($1, $2, $3, $4, $5, $6) RETURNING state, created_at`,
				func(r pgexec.Row) (bool, error) { return true, r.Scan(&q.State, &q.CreatedAt) },
				q.ID, q.SessionID, q.Asker, q.AskerName, q.Anonymous, q.Body)
			return err
		})
}

const questionColumns = `q.id, q.session_id, q.asker, q.asker_name, q.anonymous, q.body, q.state, q.pinned, q.created_at,
	(SELECT count(*) FROM call_question_votes v WHERE v.question_id = q.id),
	EXISTS (SELECT 1 FROM call_question_votes v WHERE v.question_id = q.id AND v.voter = $2)`

func scanQuestion(r pgexec.Row) (*Question, error) {
	var q Question
	err := r.Scan(&q.ID, &q.SessionID, &q.Asker, &q.AskerName, &q.Anonymous, &q.Body, &q.State, &q.Pinned, &q.CreatedAt, &q.Votes, &q.Voted)
	return &q, err
}

// Get loads one of a call's questions, as reader (a voter key) reads its upvotes.
func Get(ctx context.Context, session, id uuid.UUID, reader string) (*Question, error) {
	q, found, err := pgexec.QueryOne(ctx, `SELECT `+questionColumns+` FROM call_questions q WHERE q.session_id = $1 AND q.id = $3`, scanQuestion, session, reader, id)
	if err == nil && !found {
		err = ErrNotFound
	}
	return q, err
}

// OfSession lists a call's questions, oldest first, as reader (a voter key)
// reads their upvotes.
func OfSession(ctx context.Context, session uuid.UUID, reader string) ([]*Question, error) {
	return pgexec.Query(ctx, `SELECT `+questionColumns+` FROM call_questions q WHERE q.session_id = $1 ORDER BY q.created_at, q.id`, scanQuestion, session, reader)
}

// SetState marks one of a call's questions open, answered or dismissed.
func SetState(ctx context.Context, session, id uuid.UUID, state string) error {
	return touched(pgexec.Exec(ctx, `UPDATE call_questions SET state = $3 WHERE session_id = $1 AND id = $2`, session, id, state))
}

// SetPinned pins one of a call's questions to the top, or unpins it.
func SetPinned(ctx context.Context, session, id uuid.UUID, pinned bool) error {
	return touched(pgexec.Exec(ctx, `UPDATE call_questions SET pinned = $3 WHERE session_id = $1 AND id = $2`, session, id, pinned))
}

// touched is ErrNotFound for a write that touched no question. Pure.
func touched(n int64, err error) error {
	if err == nil && n == 0 {
		err = ErrNotFound
	}
	return err
}

// Upvote records voter's upvote of a question, or with on false takes it back.
// Doing it twice changes nothing. voter is the key business/CallQA keeps
// someone's upvotes under, never who they are; the reads take the same key.
func Upvote(ctx context.Context, id uuid.UUID, voter string, on bool) error {
	q := `INSERT INTO call_question_votes (question_id, voter) VALUES ($1, $2) ON CONFLICT DO NOTHING`
	if !on {
		q = `DELETE FROM call_question_votes WHERE question_id = $1 AND voter = $2`
	}
	_, err := pgexec.Exec(ctx, q, id, voter)
	return err
}
