package models

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Option is one choice in a poll.
type Option struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// Poll is a poll as stored.
type Poll struct {
	ID        uuid.UUID
	ChannelID uuid.UUID
	PostID    *uuid.UUID
	Question  string
	Options   []Option
	Multiple  bool
	ClosesAt  *time.Time
	ClosedAt  *time.Time
	CreatedBy uuid.UUID
	CreatedAt time.Time
	// SessionID is the call a call's poll was asked in (calls package A); nil
	// for a channel's, a DM's or a group's.
	SessionID *uuid.UUID
	// Anonymous: nobody, the database included, can tell whose its votes are.
	Anonymous bool
	// ResultsHidden: only whoever runs the call sees the counts until shared.
	ResultsHidden bool
}

// ErrNotFound is a poll id that does not exist.
var ErrNotFound = errors.New("poll not found")

func db() *sql.DB { return postgresInit.DBConn.SqlDB }

func withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
}

// Create stores a new poll, in the transaction ctx carries if any.
func Create(ctx context.Context, p *Poll) error {
	opts, err := json.Marshal(p.Options)
	if err != nil {
		return err
	}
	_, err = pgexec.Exec(ctx, `
		INSERT INTO polls (id, channel_id, question, options, multiple, closes_at, created_by, session_id, anonymous, results_hidden)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		p.ID, p.ChannelID, p.Question, opts, p.Multiple, p.ClosesAt, p.CreatedBy, p.SessionID, p.Anonymous, p.ResultsHidden)
	return err
}

// CreateInSession stores a call's poll (p.SessionID) when the call has fewer
// than limit, and says whether it did.
func CreateInSession(ctx context.Context, p *Poll, limit int) (bool, error) {
	session := *p.SessionID
	return pgexec.Capped(ctx, "call_polls:"+session.String(), limit,
		func(ctx context.Context) (int, error) {
			return pgexec.Scalar[int](ctx, `SELECT count(*) FROM polls WHERE session_id = $1`, session)
		},
		func(ctx context.Context) error { return Create(ctx, p) })
}

// SetPost records the message a poll was posted as.
func SetPost(ctx context.Context, pollID, postID uuid.UUID) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := db().ExecContext(ctx, `UPDATE polls SET post_id = $2 WHERE id = $1`, pollID, postID)
	return err
}

// Delete removes a poll whose message could not be posted.
func Delete(ctx context.Context, pollID uuid.UUID) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := db().ExecContext(ctx, `DELETE FROM polls WHERE id = $1`, pollID)
	return err
}

const pollColumns = `id, channel_id, post_id, question, options, multiple, closes_at, closed_at, created_by, created_at,
	session_id, anonymous, results_hidden`

func scanPoll(row pgexec.Row) (*Poll, error) {
	var p Poll
	var opts []byte
	var post, session sql.NullString
	var closes, closed sql.NullTime
	err := row.Scan(&p.ID, &p.ChannelID, &post, &p.Question, &opts, &p.Multiple, &closes, &closed, &p.CreatedBy, &p.CreatedAt,
		&session, &p.Anonymous, &p.ResultsHidden)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(opts, &p.Options); err != nil {
		return nil, err
	}
	if post.Valid {
		if id, perr := uuid.Parse(post.String); perr == nil {
			p.PostID = &id
		}
	}
	if session.Valid {
		if id, perr := uuid.Parse(session.String); perr == nil {
			p.SessionID = &id
		}
	}
	if closes.Valid {
		p.ClosesAt = &closes.Time
	}
	if closed.Valid {
		p.ClosedAt = &closed.Time
	}
	return &p, nil
}

// Get loads a poll.
func Get(ctx context.Context, pollID uuid.UUID) (*Poll, error) {
	p, found, err := pgexec.QueryOne(ctx, `SELECT `+pollColumns+` FROM polls WHERE id = $1`, scanPoll, pollID)
	if err == nil && !found {
		err = ErrNotFound
	}
	return p, err
}

// OfSession lists a call's polls, oldest first.
func OfSession(ctx context.Context, session uuid.UUID) ([]*Poll, error) {
	return pgexec.Query(ctx, `SELECT `+pollColumns+` FROM polls WHERE session_id = $1 ORDER BY created_at, id`, scanPoll, session)
}

// SetResultsHidden holds a call's poll's counts back for whoever runs the
// call alone, or shares them.
func SetResultsHidden(ctx context.Context, pollID uuid.UUID, hidden bool) error {
	_, err := pgexec.Exec(ctx, `UPDATE polls SET results_hidden = $2 WHERE id = $1`, pollID, hidden)
	return err
}

// Tally is how many people chose each option, and what one person chose.
type Tally struct {
	Counts map[string]int
	Voters int
	Mine   []string
}

// Count tallies a poll's votes, with userID's own choices.
func Count(ctx context.Context, pollID, userID uuid.UUID) (*Tally, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	rows, err := db().QueryContext(ctx, `SELECT option_id, user_id FROM poll_votes WHERE poll_id = $1`, pollID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	t := &Tally{Counts: map[string]int{}}
	voters := map[uuid.UUID]bool{}
	for rows.Next() {
		var opt string
		var uid uuid.UUID
		if err := rows.Scan(&opt, &uid); err != nil {
			return nil, err
		}
		t.Counts[opt]++
		voters[uid] = true
		if uid == userID {
			t.Mine = append(t.Mine, opt)
		}
	}
	t.Voters = len(voters)
	return t, rows.Err()
}

// CountMany tallies several polls' votes in one read, each with the choices of
// the voter mine names for it (a call's polls key their voters per poll).
func CountMany(ctx context.Context, mine map[uuid.UUID]uuid.UUID) (map[uuid.UUID]*Tally, error) {
	out := make(map[uuid.UUID]*Tally, len(mine))
	ids := make([]string, 0, len(mine))
	for id := range mine {
		out[id] = &Tally{Counts: map[string]int{}}
		ids = append(ids, id.String())
	}
	if len(ids) == 0 {
		return out, nil
	}
	type cast struct {
		poll, voter uuid.UUID
		option      string
	}
	votes, err := pgexec.Query(ctx, `SELECT poll_id, option_id, user_id FROM poll_votes WHERE poll_id = ANY($1::uuid[])`,
		func(r pgexec.Row) (cast, error) {
			var c cast
			return c, r.Scan(&c.poll, &c.option, &c.voter)
		}, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	voters := map[uuid.UUID]map[uuid.UUID]bool{}
	for _, v := range votes {
		t := out[v.poll]
		t.Counts[v.option]++
		if voters[v.poll] == nil {
			voters[v.poll] = map[uuid.UUID]bool{}
		}
		voters[v.poll][v.voter] = true
		if v.voter == mine[v.poll] {
			t.Mine = append(t.Mine, v.option)
		}
	}
	for poll, v := range voters {
		out[poll].Voters = len(v)
	}
	return out, nil
}

// ReplaceVotes sets userID's choices to optionIDs (none retracts the vote).
func ReplaceVotes(ctx context.Context, pollID, userID uuid.UUID, optionIDs []string) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	tx, err := db().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM poll_votes WHERE poll_id = $1 AND user_id = $2`, pollID, userID); err != nil {
		return err
	}
	for _, o := range optionIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO poll_votes (poll_id, option_id, user_id) VALUES ($1, $2, $3)`, pollID, o, userID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Close ends voting now.
func Close(ctx context.Context, pollID uuid.UUID) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := db().ExecContext(ctx, `UPDATE polls SET closed_at = now() WHERE id = $1 AND closed_at IS NULL`, pollID)
	return err
}
